// SPDX-License-Identifier: AGPL-3.0-only

package exporter

import (
	"context"
	"flag"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/grafana/mimir/pkg/util"
)

const (
	// ringKey is the key under which we store the overrides-exporter's ring in the KVStore.
	ringKey = "overrides-exporter"

	// ringNumTokens is how many tokens each overrides-exporter should have in the
	// ring. Overrides-exporter uses tokens to establish a ring leader, therefore
	// only one token is needed.
	ringNumTokens = 1

	// leaderToken is the special token that makes the owner the ring leader.
	leaderToken = 0
)

// ringOp is used as an instance state filter when obtaining instances from the
// ring. Instances in the LEAVING state are included to help minimise the number
// of leader changes during rollout and scaling operations. These instances will
// be forgotten after AutoForgetUnhealthyPeriods (see
// `KeepInstanceInTheRingOnShutdown`).
var ringOp = ring.NewOp([]ring.InstanceState{ring.ACTIVE, ring.LEAVING}, nil)

// RingConfig holds the configuration for the overrides-exporter ring.
type RingConfig struct {
	// Whether the ring is enabled for overrides-exporters.
	Enabled bool `yaml:"enabled"`

	// Use common config shared with other components' ring config.
	Common util.CommonRingConfig `yaml:",inline"`

	// Ring stability (used to decrease token reshuffling on scale-up).
	WaitStabilityMinDuration time.Duration `yaml:"wait_stability_min_duration" category:"advanced"`
	WaitStabilityMaxDuration time.Duration `yaml:"wait_stability_max_duration" category:"advanced"`

	AutoForgetUnhealthyPeriods int `yaml:"auto_forget_unhealthy_periods" category:"advanced"`
}

// RegisterFlags configures this RingConfig to the given flag set and sets defaults.
func (c *RingConfig) RegisterFlags(f *flag.FlagSet, logger log.Logger) {
	const flagNamePrefix = "overrides-exporter.ring."
	const kvStorePrefix = "collectors/"
	const componentPlural = "overrides-exporters"

	f.BoolVar(&c.Enabled, flagNamePrefix+"enabled", false, "Enable the ring used by override-exporters to deduplicate exported limit metrics.")

	c.Common.RegisterFlags(flagNamePrefix, kvStorePrefix, componentPlural, f, logger)

	// Ring stability flags.
	f.DurationVar(&c.WaitStabilityMinDuration, flagNamePrefix+"wait-stability-min-duration", 0, "Minimum time to wait for ring stability at startup, if set to positive value. Set to 0 to disable.")
	f.DurationVar(&c.WaitStabilityMaxDuration, flagNamePrefix+"wait-stability-max-duration", 5*time.Minute, "Maximum time to wait for ring stability at startup. If the overrides-exporter ring keeps changing after this period of time, it will start anyway.")

	// Auto-forget
	f.IntVar(&c.AutoForgetUnhealthyPeriods, flagNamePrefix+"auto-forget-unhealthy-periods", 4, "Number of consecutive timeout periods an unhealthy instance in the ring is automatically removed after. Set to 0 to disable auto-forget.")
}

// toBasicLifecyclerConfig transforms a RingConfig into configuration that can be used to create a BasicLifecycler.
func (c *RingConfig) toBasicLifecyclerConfig(logger log.Logger) (ring.BasicLifecyclerConfig, error) {
	instanceAddr, err := ring.GetInstanceAddr(c.Common.InstanceAddr, c.Common.InstanceInterfaceNames, logger, c.Common.EnableIPv6)
	if err != nil {
		return ring.BasicLifecyclerConfig{}, err
	}

	instancePort := ring.GetInstancePort(c.Common.InstancePort, c.Common.ListenPort)

	return ring.BasicLifecyclerConfig{
		ID:                              c.Common.InstanceID,
		Addr:                            net.JoinHostPort(instanceAddr, strconv.Itoa(instancePort)),
		HeartbeatPeriod:                 c.Common.HeartbeatPeriod,
		HeartbeatTimeout:                c.Common.HeartbeatTimeout,
		TokensObservePeriod:             0,
		NumTokens:                       ringNumTokens,
		KeepInstanceInTheRingOnShutdown: true,
	}, nil
}

func (c *RingConfig) toRingConfig() ring.Config {
	rc := c.Common.ToRingConfig()

	rc.ReplicationFactor = 1
	rc.SubringCacheDisabled = true

	return rc
}

// Validate the Config.
func (c *RingConfig) Validate() error {
	if c.WaitStabilityMinDuration > 0 {
		if c.WaitStabilityMinDuration > c.WaitStabilityMaxDuration {
			return errors.New("-overrides-exporter.ring.wait-stability-max-duration must be greater or equal " +
				"to -overrides-exporter.ring.wait-stability-min-duration")
		}
	}
	return nil
}

// overridesExporterRing is a ring client that overrides-exporters can use to
// establish a leader replica that is the unique exporter of per-tenant limit metrics.
type overridesExporterRing struct {
	services.Service

	config RingConfig

	client     *ring.Ring
	lifecycler *ring.BasicLifecycler

	subserviceManager *services.Manager
	subserviceWatcher *services.FailureWatcher
	logger            log.Logger
}

// newRing creates a new overridesExporterRing from the given configuration.
func newRing(config RingConfig, logger log.Logger, reg prometheus.Registerer) (*overridesExporterRing, error) {
	reg = prometheus.WrapRegistererWithPrefix("cortex_", reg)
	kvStore, err := kv.NewClient(
		config.Common.KVStore,
		ring.GetCodec(),
		kv.RegistererWithKVName(reg, "overrides-exporter-lifecycler"),
		logger,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize overrides-exporter's KV store")
	}

	delegate := ring.BasicLifecyclerDelegate(ring.NewInstanceRegisterDelegate(ring.ACTIVE, ringNumTokens))
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, logger)
	if config.AutoForgetUnhealthyPeriods > 0 {
		delegate = ring.NewAutoForgetDelegate(time.Duration(config.AutoForgetUnhealthyPeriods)*config.Common.HeartbeatTimeout, delegate, logger)
	}

	lifecyclerConfig, err := config.toBasicLifecyclerConfig(logger)
	if err != nil {
		return nil, err
	}

	const ringName = "overrides-exporter"
	lifecycler, err := ring.NewBasicLifecycler(lifecyclerConfig, ringName, ringKey, kvStore, delegate, logger, reg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize overrides-exporter's lifecycler")
	}

	ringClient, err := ring.New(config.toRingConfig(), ringName, ringKey, logger, reg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create a overrides-exporter ring client")
	}

	manager, err := services.NewManager(lifecycler, ringClient)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create service manager")
	}

	r := &overridesExporterRing{
		config:            config,
		client:            ringClient,
		lifecycler:        lifecycler,
		subserviceManager: manager,
		subserviceWatcher: services.NewFailureWatcher(),
		logger:            logger,
	}
	r.Service = services.NewBasicService(r.starting, r.running, r.stopping)
	return r, nil
}

// isLeader checks whether this instance is the leader replica that exports metrics for all tenants.
func (r *overridesExporterRing) isLeader() (bool, error) {
	// Get the leader from the ring and check whether it's this replica.
	rl, err := ringLeader(r.client)
	if err != nil {
		return false, err
	}

	return rl.Addr == r.lifecycler.GetInstanceAddr(), nil
}

// ringLeader returns the ring member that owns the special token.
func ringLeader(r ring.ReadRing) (*ring.InstanceDesc, error) {
	rs, err := r.Get(leaderToken, ringOp, nil, nil, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get a healthy instance for token %d", leaderToken)
	}
	if len(rs.Instances) != 1 {
		return nil, fmt.Errorf("got %d instances for token %d (but expected 1)", len(rs.Instances), leaderToken)
	}

	return &rs.Instances[0], nil
}

func (r *overridesExporterRing) starting(ctx context.Context) error {
	r.subserviceWatcher.WatchManager(r.subserviceManager)
	if err := services.StartManagerAndAwaitHealthy(ctx, r.subserviceManager); err != nil {
		return errors.Wrap(err, "unable to start overrides-exporter ring subservice manager")
	}

	level.Info(r.logger).Log("msg", "waiting until overrides-exporter is ACTIVE in the ring")
	if err := ring.WaitInstanceState(ctx, r.client, r.lifecycler.GetInstanceID(), ring.ACTIVE); err != nil {
		return errors.Wrap(err, "overrides-exporter failed to become ACTIVE in the ring")
	}
	level.Info(r.logger).Log("msg", "overrides-exporter is ACTIVE in the ring")

	// In the event of a cluster cold start or scale up of 2+ overrides-exporter
	// instances at the same time, the leader token may hop from one instance to
	// another, creating high series churn for the limit metrics. Waiting for a
	// stable ring helps to counteract that.
	if r.config.WaitStabilityMinDuration > 0 {
		minWaiting := r.config.WaitStabilityMinDuration
		maxWaiting := r.config.WaitStabilityMaxDuration

		level.Info(r.logger).Log("msg", "waiting until overrides-exporter ring topology is stable", "min_waiting", minWaiting.String(), "max_waiting", maxWaiting.String())
		if err := ring.WaitRingTokensStability(ctx, r.client, ringOp, minWaiting, maxWaiting); err != nil {
			level.Warn(r.logger).Log("msg", "overrides-exporter ring topology is not stable after the max waiting time, proceeding anyway")
		} else {
			level.Info(r.logger).Log("msg", "overrides-exporter ring topology is stable")
		}
	}
	return nil
}

func (r *overridesExporterRing) running(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-r.subserviceWatcher.Chan():
		return errors.Wrap(err, "a subservice of overrides-exporter ring has failed")
	}
}

func (r *overridesExporterRing) stopping(_ error) error {
	return errors.Wrap(
		services.StopManagerAndAwaitStopped(context.Background(), r.subserviceManager),
		"failed to stop overrides-exporter's ring subservice manager",
	)
}
