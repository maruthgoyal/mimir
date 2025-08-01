// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/compactor/compactor.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package compactor

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/thanos-io/objstore"
	"go.uber.org/atomic"

	"github.com/grafana/mimir/pkg/storage/bucket"
	"github.com/grafana/mimir/pkg/storage/indexheader"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
	"github.com/grafana/mimir/pkg/storage/tsdb/block"
	"github.com/grafana/mimir/pkg/util"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

const (
	// ringKey is the key under which we store the compactors ring in the KVStore.
	ringKey = "compactor"
)

const (
	blocksMarkedForDeletionName = "cortex_compactor_blocks_marked_for_deletion_total"
	blocksMarkedForDeletionHelp = "Total number of blocks marked for deletion in compactor."
)

var (
	errInvalidBlockRanges                         = "compactor block range periods should be divisible by the previous one, but %s is not divisible by %s"
	errInvalidCompactionOrder                     = fmt.Errorf("unsupported compaction order (supported values: %s)", strings.Join(CompactionOrders, ", "))
	errInvalidMaxOpeningBlocksConcurrency         = fmt.Errorf("invalid max-opening-blocks-concurrency value, must be positive")
	errInvalidMaxClosingBlocksConcurrency         = fmt.Errorf("invalid max-closing-blocks-concurrency value, must be positive")
	errInvalidSymbolFlushersConcurrency           = fmt.Errorf("invalid symbols-flushers-concurrency value, must be positive")
	errInvalidMaxBlockUploadValidationConcurrency = fmt.Errorf("invalid max-block-upload-validation-concurrency value, can't be negative")
	RingOp                                        = ring.NewOp([]ring.InstanceState{ring.ACTIVE}, nil)

	// compactionIgnoredLabels defines the external labels that compactor will
	// drop/ignore when planning jobs so that they don't keep blocks from
	// compacting together.
	compactionIgnoredLabels = []string{
		mimir_tsdb.DeprecatedIngesterIDExternalLabel,
		mimir_tsdb.DeprecatedTenantIDExternalLabel,
	}
)

// BlocksGrouperFactory builds and returns the grouper to use to compact a tenant's blocks.
type BlocksGrouperFactory func(
	ctx context.Context,
	cfg Config,
	cfgProvider ConfigProvider,
	userID string,
	logger log.Logger,
	reg prometheus.Registerer,
) Grouper

// BlocksCompactorFactory builds and returns the compactor and planner for compacting a tenant's blocks.
type BlocksCompactorFactory func(
	ctx context.Context,
	cfg Config,
	logger log.Logger,
	reg prometheus.Registerer,
) (Compactor, Planner, error)

// Config holds the MultitenantCompactor config.
type Config struct {
	BlockRanges                mimir_tsdb.DurationList `yaml:"block_ranges" category:"advanced"`
	BlockSyncConcurrency       int                     `yaml:"block_sync_concurrency" category:"advanced"`
	MetaSyncConcurrency        int                     `yaml:"meta_sync_concurrency" category:"advanced"`
	DataDir                    string                  `yaml:"data_dir"`
	CompactionInterval         time.Duration           `yaml:"compaction_interval" category:"advanced"`
	CompactionRetries          int                     `yaml:"compaction_retries" category:"advanced"`
	CompactionConcurrency      int                     `yaml:"compaction_concurrency" category:"advanced"`
	CompactionWaitPeriod       time.Duration           `yaml:"first_level_compaction_wait_period"`
	CleanupInterval            time.Duration           `yaml:"cleanup_interval" category:"advanced"`
	CleanupConcurrency         int                     `yaml:"cleanup_concurrency" category:"advanced"`
	DeletionDelay              time.Duration           `yaml:"deletion_delay" category:"advanced"`
	TenantCleanupDelay         time.Duration           `yaml:"tenant_cleanup_delay" category:"advanced"`
	MaxCompactionTime          time.Duration           `yaml:"max_compaction_time" category:"advanced"`
	NoBlocksFileCleanupEnabled bool                    `yaml:"no_blocks_file_cleanup_enabled" category:"experimental"`

	// Compactor concurrency options
	MaxOpeningBlocksConcurrency         int `yaml:"max_opening_blocks_concurrency" category:"advanced"`          // Number of goroutines opening blocks before compaction.
	MaxClosingBlocksConcurrency         int `yaml:"max_closing_blocks_concurrency" category:"advanced"`          // Max number of blocks that can be closed concurrently during split compaction. Note that closing of newly compacted block uses a lot of memory for writing index.
	SymbolsFlushersConcurrency          int `yaml:"symbols_flushers_concurrency" category:"advanced"`            // Number of symbols flushers used when doing split compaction.
	MaxBlockUploadValidationConcurrency int `yaml:"max_block_upload_validation_concurrency" category:"advanced"` // Max number of uploaded blocks that can be validated concurrently.
	UpdateBlocksConcurrency             int `yaml:"update_blocks_concurrency" category:"advanced"`               // Number of goroutines to use when updating blocks metadata during bucket index updates.

	EnabledTenants  flagext.StringSliceCSV `yaml:"enabled_tenants" category:"advanced"`
	DisabledTenants flagext.StringSliceCSV `yaml:"disabled_tenants" category:"advanced"`

	// Compactors sharding.
	ShardingRing RingConfig `yaml:"sharding_ring"`

	CompactionJobsOrder string `yaml:"compaction_jobs_order" category:"advanced"`

	// No need to add options to customize the retry backoff,
	// given the defaults should be fine, but allow to override
	// it in tests.
	retryMinBackoff time.Duration `yaml:"-"`
	retryMaxBackoff time.Duration `yaml:"-"`

	// Allow downstream projects to customise the blocks compactor.
	BlocksGrouperFactory   BlocksGrouperFactory   `yaml:"-"`
	BlocksCompactorFactory BlocksCompactorFactory `yaml:"-"`

	// Allow compactor to upload sparse-index-header files
	UploadSparseIndexHeaders       bool               `yaml:"upload_sparse_index_headers" category:"experimental"`
	SparseIndexHeadersSamplingRate int                `yaml:"-"`
	SparseIndexHeadersConfig       indexheader.Config `yaml:"-"`
}

// RegisterFlags registers the MultitenantCompactor flags.
func (cfg *Config) RegisterFlags(f *flag.FlagSet, logger log.Logger) {
	cfg.ShardingRing.RegisterFlags(f, logger)

	cfg.BlockRanges = mimir_tsdb.DurationList{2 * time.Hour, 12 * time.Hour, 24 * time.Hour}
	cfg.retryMinBackoff = 10 * time.Second
	cfg.retryMaxBackoff = time.Minute

	f.Var(&cfg.BlockRanges, "compactor.block-ranges", "List of compaction time ranges.")
	f.IntVar(&cfg.BlockSyncConcurrency, "compactor.block-sync-concurrency", 8, "Number of Go routines to use when downloading blocks for compaction and uploading resulting blocks.")
	f.IntVar(&cfg.MetaSyncConcurrency, "compactor.meta-sync-concurrency", 20, "Number of Go routines to use when syncing block meta files from the long term storage.")
	f.StringVar(&cfg.DataDir, "compactor.data-dir", "./data-compactor/", "Directory to temporarily store blocks during compaction. This directory is not required to be persisted between restarts.")
	f.DurationVar(&cfg.CompactionInterval, "compactor.compaction-interval", time.Hour, "The frequency at which the compaction runs")
	f.DurationVar(&cfg.MaxCompactionTime, "compactor.max-compaction-time", time.Hour, "Max time for starting compactions for a single tenant. After this time no new compactions for the tenant are started before next compaction cycle. This can help in multi-tenant environments to avoid single tenant using all compaction time, but also in single-tenant environments to force new discovery of blocks more often. 0 = disabled.")
	f.IntVar(&cfg.CompactionRetries, "compactor.compaction-retries", 3, "How many times to retry a failed compaction within a single compaction run.")
	f.IntVar(&cfg.CompactionConcurrency, "compactor.compaction-concurrency", 1, "Max number of concurrent compactions running.")
	f.DurationVar(&cfg.CompactionWaitPeriod, "compactor.first-level-compaction-wait-period", 25*time.Minute, "How long the compactor waits before compacting first-level blocks that are uploaded by the ingesters. This configuration option allows for the reduction of cases where the compactor begins to compact blocks before all ingesters have uploaded their blocks to the storage.")
	f.DurationVar(&cfg.CleanupInterval, "compactor.cleanup-interval", 15*time.Minute, "How frequently the compactor should run blocks cleanup and maintenance, as well as update the bucket index.")
	f.IntVar(&cfg.CleanupConcurrency, "compactor.cleanup-concurrency", 20, "Max number of tenants for which blocks cleanup and maintenance should run concurrently.")
	f.StringVar(&cfg.CompactionJobsOrder, "compactor.compaction-jobs-order", CompactionOrderOldestFirst, fmt.Sprintf("The sorting to use when deciding which compaction jobs should run first for a given tenant. Supported values are: %s.", strings.Join(CompactionOrders, ", ")))
	f.DurationVar(&cfg.DeletionDelay, "compactor.deletion-delay", 12*time.Hour, "Time before a block marked for deletion is deleted from bucket. "+
		"If not 0, blocks will be marked for deletion and the compactor component will permanently delete blocks marked for deletion from the bucket. "+
		"If 0, blocks will be deleted straight away. Note that deleting blocks immediately can cause query failures.")
	f.DurationVar(&cfg.TenantCleanupDelay, "compactor.tenant-cleanup-delay", 6*time.Hour, "For tenants marked for deletion, this is the time between deletion of the last block, and doing final cleanup (marker files, debug files) of the tenant.")
	f.BoolVar(&cfg.NoBlocksFileCleanupEnabled, "compactor.no-blocks-file-cleanup-enabled", false, "If enabled, will delete the bucket-index, markers and debug files in the tenant bucket when there are no blocks left in the index.")
	f.BoolVar(&cfg.UploadSparseIndexHeaders, "compactor.upload-sparse-index-headers", false, "If enabled, the compactor constructs and uploads sparse index headers to object storage during each compaction cycle. This allows store-gateway instances to use the sparse headers from object storage instead of recreating them locally.")

	// compactor concurrency options
	f.IntVar(&cfg.MaxOpeningBlocksConcurrency, "compactor.max-opening-blocks-concurrency", 1, "Number of goroutines opening blocks before compaction.")
	f.IntVar(&cfg.MaxClosingBlocksConcurrency, "compactor.max-closing-blocks-concurrency", 1, "Max number of blocks that can be closed concurrently during split compaction. Note that closing a newly compacted block uses a lot of memory for writing the index.")
	f.IntVar(&cfg.SymbolsFlushersConcurrency, "compactor.symbols-flushers-concurrency", 1, "Number of symbols flushers used when doing split compaction.")
	f.IntVar(&cfg.MaxBlockUploadValidationConcurrency, "compactor.max-block-upload-validation-concurrency", 1, "Max number of uploaded blocks that can be validated concurrently. 0 = no limit.")
	f.IntVar(&cfg.UpdateBlocksConcurrency, "compactor.update-blocks-concurrency", defaultUpdateBlocksConcurrency, "Number of Go routines to use when updating blocks metadata during bucket index updates.")

	f.Var(&cfg.EnabledTenants, "compactor.enabled-tenants", "Comma separated list of tenants that can be compacted. If specified, only these tenants will be compacted by the compactor, otherwise all tenants can be compacted. Subject to sharding.")
	f.Var(&cfg.DisabledTenants, "compactor.disabled-tenants", "Comma separated list of tenants that cannot be compacted by the compactor. If specified, and the compactor would normally pick a given tenant for compaction (via -compactor.enabled-tenants or sharding), it will be ignored instead.")
}

func (cfg *Config) Validate(logger log.Logger) error {
	// Mimir assumes that smaller blocks are eventually compacted to 24h blocks in
	// various places on the read path (cache TTLs, query splitting). Warn when this
	// isn't the case since it may affect performance.
	if len(cfg.BlockRanges) > 0 {
		if maxRange := slices.Max(cfg.BlockRanges); 24*time.Hour > maxRange {
			level.Warn(logger).Log("msg", "Largest compactor block range is not 24h. This may result in degraded query performance", "range", maxRange)
		}
	}

	// Each block range period should be divisible by the previous one.
	for i := 1; i < len(cfg.BlockRanges); i++ {
		if cfg.BlockRanges[i]%cfg.BlockRanges[i-1] != 0 {
			return errors.Errorf(errInvalidBlockRanges, cfg.BlockRanges[i].String(), cfg.BlockRanges[i-1].String())
		}
	}

	if cfg.MaxOpeningBlocksConcurrency < 1 {
		return errInvalidMaxOpeningBlocksConcurrency
	}
	if cfg.MaxClosingBlocksConcurrency < 1 {
		return errInvalidMaxClosingBlocksConcurrency
	}
	if cfg.SymbolsFlushersConcurrency < 1 {
		return errInvalidSymbolFlushersConcurrency
	}
	if cfg.MaxBlockUploadValidationConcurrency < 0 {
		return errInvalidMaxBlockUploadValidationConcurrency
	}
	if !util.StringsContain(CompactionOrders, cfg.CompactionJobsOrder) {
		return errInvalidCompactionOrder
	}

	return nil
}

// ConfigProvider defines the per-tenant config provider for the MultitenantCompactor.
type ConfigProvider interface {
	bucket.TenantConfigProvider

	// CompactorBlocksRetentionPeriod returns the retention period for a given user.
	CompactorBlocksRetentionPeriod(user string) time.Duration

	// CompactorSplitAndMergeShards returns the number of shards to use when splitting blocks.
	CompactorSplitAndMergeShards(userID string) int

	// CompactorSplitGroups returns the number of groups that blocks used for splitting should
	// be grouped into. Different groups are then split by different jobs.
	CompactorSplitGroups(userID string) int

	// CompactorTenantShardSize returns the number of compactors that this user can use. 0 = all compactors.
	CompactorTenantShardSize(userID string) int

	// CompactorPartialBlockDeletionDelay returns the partial block delay time period for a given user,
	// and whether the configured value is valid. If the value isn't valid, the returned delay is the default one
	// and the caller is responsible for warning the Mimir operator about it.
	CompactorPartialBlockDeletionDelay(userID string) (delay time.Duration, valid bool)

	// CompactorBlockUploadEnabled returns whether block upload is enabled for a given tenant.
	CompactorBlockUploadEnabled(tenantID string) bool

	// CompactorBlockUploadValidationEnabled returns whether block upload validation is enabled for a given tenant.
	CompactorBlockUploadValidationEnabled(tenantID string) bool

	// CompactorBlockUploadVerifyChunks returns whether chunk verification is enabled for a given tenant.
	CompactorBlockUploadVerifyChunks(tenantID string) bool

	// CompactorBlockUploadMaxBlockSizeBytes returns the maximum size in bytes of a block that is allowed to be uploaded or validated for a given user.
	CompactorBlockUploadMaxBlockSizeBytes(userID string) int64

	// CompactorInMemoryTenantMetaCacheSize returns number of parsed *Meta objects that we can keep in memory for the user between compactions.
	CompactorInMemoryTenantMetaCacheSize(userID string) int

	// CompactorMaxLookback returns the duration of the compactor lookback period, blocks uploaded before the lookback period aren't
	// considered in compactor cycles
	CompactorMaxLookback(userID string) time.Duration

	// CompactorMaxPerBlockUploadConcurrency returns the maximum number of TSDB files that can be uploaded concurrently for each block.
	CompactorMaxPerBlockUploadConcurrency(userID string) int
}

// MultitenantCompactor is a multi-tenant TSDB block compactor based on Thanos.
type MultitenantCompactor struct {
	services.Service

	compactorCfg Config
	storageCfg   mimir_tsdb.BlocksStorageConfig
	cfgProvider  ConfigProvider
	logger       log.Logger
	parentLogger log.Logger
	registerer   prometheus.Registerer

	// Functions that create bucket client, grouper, planner and compactor using the context.
	// Useful for injecting mock objects from tests.
	bucketClientFactory    func(ctx context.Context) (objstore.Bucket, error)
	blocksGrouperFactory   BlocksGrouperFactory
	blocksCompactorFactory BlocksCompactorFactory

	// Blocks cleaner is responsible for hard deletion of blocks marked for deletion.
	blocksCleaner *BlocksCleaner

	// Underlying compactor and planner for compacting TSDB blocks.
	blocksCompactor Compactor
	blocksPlanner   Planner

	// Client used to run operations on the bucket storing blocks.
	bucketClient objstore.Bucket

	// Ring used for sharding compactions.
	ringLifecycler         *ring.BasicLifecycler
	ring                   *ring.Ring
	ringSubservices        *services.Manager
	ringSubservicesWatcher *services.FailureWatcher

	shardingStrategy shardingStrategy
	jobsOrder        JobsOrderFunc

	// Metrics.
	compactionRunsStarted          prometheus.Counter
	compactionRunsCompleted        prometheus.Counter
	compactionRunsErred            prometheus.Counter
	compactionRunsShutdown         prometheus.Counter
	compactionRunsLastSuccess      prometheus.Gauge
	compactionRunDiscoveredTenants prometheus.Gauge
	compactionRunSkippedTenants    prometheus.Gauge
	compactionRunSucceededTenants  prometheus.Gauge
	compactionRunFailedTenants     prometheus.Gauge
	compactionRunInterval          prometheus.Gauge
	blocksMarkedForDeletion        prometheus.Counter

	// outOfSpace is a separate metric for out-of-space errors because this is a common issue which often requires an operator to investigate,
	// so alerts need to be able to treat it with higher priority than other compaction errors.
	outOfSpace prometheus.Counter

	// Metrics shared across all BucketCompactor instances.
	bucketCompactorMetrics *BucketCompactorMetrics

	// TSDB syncer metrics
	syncerMetrics *aggregatedSyncerMetrics

	// Block upload metrics
	blockUploadBlocks      *prometheus.CounterVec
	blockUploadBytes       *prometheus.CounterVec
	blockUploadFiles       *prometheus.CounterVec
	blockUploadValidations atomic.Int64

	// Per-tenant meta caches that are passed to MetaFetcher.
	metaCaches map[string]*block.MetaCache
}

// NewMultitenantCompactor makes a new MultitenantCompactor.
func NewMultitenantCompactor(compactorCfg Config, storageCfg mimir_tsdb.BlocksStorageConfig, cfgProvider ConfigProvider, logger log.Logger, registerer prometheus.Registerer) (*MultitenantCompactor, error) {
	bucketClientFactory := func(ctx context.Context) (objstore.Bucket, error) {
		return bucket.NewClient(ctx, storageCfg.Bucket, "compactor", logger, registerer)
	}

	// Configure the compactor and grouper factories only if they weren't already set by a downstream project.
	if compactorCfg.BlocksGrouperFactory == nil || compactorCfg.BlocksCompactorFactory == nil {
		configureSplitAndMergeCompactor(&compactorCfg)
	}

	blocksGrouperFactory := compactorCfg.BlocksGrouperFactory
	blocksCompactorFactory := compactorCfg.BlocksCompactorFactory

	mimirCompactor, err := newMultitenantCompactor(compactorCfg, storageCfg, cfgProvider, logger, registerer, bucketClientFactory, blocksGrouperFactory, blocksCompactorFactory)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create blocks compactor")
	}

	return mimirCompactor, nil
}

func newMultitenantCompactor(
	compactorCfg Config,
	storageCfg mimir_tsdb.BlocksStorageConfig,
	cfgProvider ConfigProvider,
	logger log.Logger,
	registerer prometheus.Registerer,
	bucketClientFactory func(ctx context.Context) (objstore.Bucket, error),
	blocksGrouperFactory BlocksGrouperFactory,
	blocksCompactorFactory BlocksCompactorFactory,
) (*MultitenantCompactor, error) {
	c := &MultitenantCompactor{
		compactorCfg:           compactorCfg,
		storageCfg:             storageCfg,
		cfgProvider:            cfgProvider,
		parentLogger:           logger,
		logger:                 log.With(logger, "component", "compactor"),
		registerer:             registerer,
		syncerMetrics:          newAggregatedSyncerMetrics(registerer),
		bucketClientFactory:    bucketClientFactory,
		blocksGrouperFactory:   blocksGrouperFactory,
		blocksCompactorFactory: blocksCompactorFactory,
		metaCaches:             map[string]*block.MetaCache{},

		compactionRunsStarted: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Name: "cortex_compactor_runs_started_total",
			Help: "Total number of compaction runs started.",
		}),
		compactionRunsCompleted: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Name: "cortex_compactor_runs_completed_total",
			Help: "Total number of compaction runs successfully completed.",
		}),
		compactionRunsErred: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Name:        "cortex_compactor_runs_failed_total",
			Help:        "Total number of compaction runs failed.",
			ConstLabels: map[string]string{"reason": "error"},
		}),
		compactionRunsShutdown: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Name:        "cortex_compactor_runs_failed_total",
			Help:        "Total number of compaction runs failed.",
			ConstLabels: map[string]string{"reason": "shutdown"},
		}),
		compactionRunsLastSuccess: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_last_successful_run_timestamp_seconds",
			Help: "Unix timestamp of the last successful compaction run.",
		}),
		compactionRunDiscoveredTenants: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_tenants_discovered",
			Help: "Number of tenants discovered during the current compaction run. Reset to 0 when compactor is idle.",
		}),
		compactionRunSkippedTenants: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_tenants_skipped",
			Help: "Number of tenants skipped during the current compaction run. Reset to 0 when compactor is idle.",
		}),
		compactionRunSucceededTenants: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_tenants_processing_succeeded",
			Help: "Number of tenants successfully processed during the current compaction run. Reset to 0 when compactor is idle.",
		}),
		compactionRunFailedTenants: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_tenants_processing_failed",
			Help: "Number of tenants failed processing during the current compaction run. Reset to 0 when compactor is idle.",
		}),
		compactionRunInterval: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_compaction_interval_seconds",
			Help: "The configured interval on which compaction is run in seconds. Useful when compared to the last successful run metric to accurately detect multiple failed compaction runs.",
		}),
		outOfSpace: promauto.With(registerer).NewGauge(prometheus.GaugeOpts{
			Name: "cortex_compactor_disk_out_of_space_errors_total",
			Help: "Number of times a compaction failed because the compactor disk was out of space.",
		}),
		blocksMarkedForDeletion: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Name:        blocksMarkedForDeletionName,
			Help:        blocksMarkedForDeletionHelp,
			ConstLabels: prometheus.Labels{"reason": "compaction"},
		}),
		blockUploadBlocks: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_block_upload_api_blocks_total",
			Help: "Total number of blocks successfully uploaded and validated using the block upload API.",
		}, []string{"user"}),
		blockUploadBytes: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_block_upload_api_bytes_total",
			Help: "Total number of bytes from successfully uploaded and validated blocks using block upload API.",
		}, []string{"user"}),
		blockUploadFiles: promauto.With(registerer).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_block_upload_api_files_total",
			Help: "Total number of files from successfully uploaded and validated blocks using block upload API.",
		}, []string{"user"}),
	}

	promauto.With(registerer).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cortex_block_upload_validations_in_progress",
		Help: "Number of block upload validations currently running.",
	}, func() float64 {
		return float64(c.blockUploadValidations.Load())
	})

	c.bucketCompactorMetrics = NewBucketCompactorMetrics(c.blocksMarkedForDeletion, registerer)

	if len(compactorCfg.EnabledTenants) > 0 {
		level.Info(c.logger).Log("msg", "compactor using enabled users", "enabled", compactorCfg.EnabledTenants)
	}
	if len(compactorCfg.DisabledTenants) > 0 {
		level.Info(c.logger).Log("msg", "compactor using disabled users", "disabled", compactorCfg.DisabledTenants)
	}

	c.jobsOrder = GetJobsOrderFunction(compactorCfg.CompactionJobsOrder)
	if c.jobsOrder == nil {
		return nil, errInvalidCompactionOrder
	}

	c.Service = services.NewBasicService(c.starting, c.running, c.stopping)

	// The last successful compaction run metric is exposed as seconds since epoch, so we need to use seconds for this metric.
	c.compactionRunInterval.Set(c.compactorCfg.CompactionInterval.Seconds())

	return c, nil
}

// Start the compactor.
func (c *MultitenantCompactor) starting(ctx context.Context) error {
	var err error

	// Create bucket client.
	c.bucketClient, err = c.bucketClientFactory(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to create bucket client")
	}

	// Create blocks compactor dependencies.
	c.blocksCompactor, c.blocksPlanner, err = c.blocksCompactorFactory(ctx, c.compactorCfg, c.logger, c.registerer)
	if err != nil {
		return errors.Wrap(err, "failed to initialize compactor dependencies")
	}

	// Wrap the bucket client to write block deletion marks in the global location too.
	c.bucketClient = block.BucketWithGlobalMarkers(c.bucketClient)

	// Initialize the compactors ring if sharding is enabled.
	c.ring, c.ringLifecycler, err = newRingAndLifecycler(c.compactorCfg.ShardingRing, c.logger, c.registerer)
	if err != nil {
		return err
	}

	c.ringSubservices, err = services.NewManager(c.ringLifecycler, c.ring)
	if err != nil {
		return errors.Wrap(err, "unable to create compactor ring dependencies")
	}

	c.ringSubservicesWatcher = services.NewFailureWatcher()
	c.ringSubservicesWatcher.WatchManager(c.ringSubservices)
	if err = c.ringSubservices.StartAsync(ctx); err != nil {
		return errors.Wrap(err, "unable to start compactor ring dependencies")
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, c.compactorCfg.ShardingRing.WaitActiveInstanceTimeout)
	defer cancel()
	if err = c.ringSubservices.AwaitHealthy(ctxTimeout); err != nil {
		return errors.Wrap(err, "unable to start compactor ring dependencies")
	}

	// If sharding is enabled we should wait until this instance is ACTIVE within the ring. This
	// MUST be done before starting any other component depending on the users scanner, because
	// the users scanner depends on the ring (to check whether a user belongs to this shard or not).
	level.Info(c.logger).Log("msg", "waiting until compactor is ACTIVE in the ring")
	if err = ring.WaitInstanceState(ctxTimeout, c.ring, c.ringLifecycler.GetInstanceID(), ring.ACTIVE); err != nil {
		return errors.Wrap(err, "compactor failed to become ACTIVE in the ring")
	}

	level.Info(c.logger).Log("msg", "compactor is ACTIVE in the ring")

	// In the event of a cluster cold start or scale up of 2+ compactor instances at the same
	// time, we may end up in a situation where each new compactor instance starts at a slightly
	// different time and thus each one starts with a different state of the ring. It's better
	// to just wait a short time for ring stability.
	if c.compactorCfg.ShardingRing.WaitStabilityMinDuration > 0 {
		minWaiting := c.compactorCfg.ShardingRing.WaitStabilityMinDuration
		maxWaiting := c.compactorCfg.ShardingRing.WaitStabilityMaxDuration

		level.Info(c.logger).Log("msg", "waiting until compactor ring topology is stable", "min_waiting", minWaiting.String(), "max_waiting", maxWaiting.String())
		if err := ring.WaitRingStability(ctx, c.ring, RingOp, minWaiting, maxWaiting); err != nil {
			level.Warn(c.logger).Log("msg", "compactor ring topology is not stable after the max waiting time, proceeding anyway")
		} else {
			level.Info(c.logger).Log("msg", "compactor ring topology is stable")
		}
	}

	allowedTenants := util.NewAllowList(c.compactorCfg.EnabledTenants, c.compactorCfg.DisabledTenants)
	c.shardingStrategy = newSplitAndMergeShardingStrategy(allowedTenants, c.ring, c.ringLifecycler, c.cfgProvider)

	// Create the blocks cleaner (service).
	c.blocksCleaner = NewBlocksCleaner(BlocksCleanerConfig{
		DeletionDelay:                 c.compactorCfg.DeletionDelay,
		CleanupInterval:               util.DurationWithJitter(c.compactorCfg.CleanupInterval, 0.1),
		CleanupConcurrency:            c.compactorCfg.CleanupConcurrency,
		TenantCleanupDelay:            c.compactorCfg.TenantCleanupDelay,
		DeleteBlocksConcurrency:       defaultDeleteBlocksConcurrency,
		GetDeletionMarkersConcurrency: defaultGetDeletionMarkersConcurrency,
		UpdateBlocksConcurrency:       c.compactorCfg.UpdateBlocksConcurrency,
		NoBlocksFileCleanupEnabled:    c.compactorCfg.NoBlocksFileCleanupEnabled,
		CompactionBlockRanges:         c.compactorCfg.BlockRanges,
	}, c.bucketClient, c.shardingStrategy.blocksCleanerOwnsUser, c.cfgProvider, c.parentLogger, c.registerer)

	// Start blocks cleaner asynchronously, don't wait until initial cleanup is finished.
	if err := c.blocksCleaner.StartAsync(ctx); err != nil {
		c.ringSubservices.StopAsync()
		return errors.Wrap(err, "failed to start the blocks cleaner")
	}

	return nil
}

func newRingAndLifecycler(cfg RingConfig, logger log.Logger, reg prometheus.Registerer) (*ring.Ring, *ring.BasicLifecycler, error) {
	reg = prometheus.WrapRegistererWithPrefix("cortex_", reg)
	kvStore, err := kv.NewClient(cfg.Common.KVStore, ring.GetCodec(), kv.RegistererWithKVName(reg, "compactor-lifecycler"), logger)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize compactors' KV store")
	}

	lifecyclerCfg, err := cfg.ToBasicLifecyclerConfig(logger)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to build compactors' lifecycler config")
	}

	var delegate ring.BasicLifecyclerDelegate
	delegate = ring.NewInstanceRegisterDelegate(ring.ACTIVE, lifecyclerCfg.NumTokens)
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, logger)
	if cfg.AutoForgetUnhealthyPeriods > 0 {
		delegate = ring.NewAutoForgetDelegate(time.Duration(cfg.AutoForgetUnhealthyPeriods)*lifecyclerCfg.HeartbeatTimeout, delegate, logger)
	}

	compactorsLifecycler, err := ring.NewBasicLifecycler(lifecyclerCfg, "compactor", ringKey, kvStore, delegate, logger, reg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize compactors' lifecycler")
	}

	compactorsRing, err := ring.New(cfg.toRingConfig(), "compactor", ringKey, logger, reg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to initialize compactors' ring client")
	}

	return compactorsRing, compactorsLifecycler, nil
}

func (c *MultitenantCompactor) stopping(_ error) error {
	ctx := context.Background()

	services.StopAndAwaitTerminated(ctx, c.blocksCleaner) //nolint:errcheck
	if c.ringSubservices != nil {
		return services.StopManagerAndAwaitStopped(ctx, c.ringSubservices)
	}
	return nil
}

func (c *MultitenantCompactor) running(ctx context.Context) error {
	// Run an initial compaction before starting the interval.
	c.compactUsers(ctx)

	ticker := time.NewTicker(util.DurationWithJitter(c.compactorCfg.CompactionInterval, 0.05))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.compactUsers(ctx)
		case <-ctx.Done():
			return nil
		case err := <-c.ringSubservicesWatcher.Chan():
			return errors.Wrap(err, "compactor subservice failed")
		}
	}
}

func (c *MultitenantCompactor) compactUsers(ctx context.Context) {
	succeeded := false
	compactionErrorCount := 0

	c.compactionRunsStarted.Inc()

	defer func() {
		if succeeded && compactionErrorCount == 0 {
			c.compactionRunsCompleted.Inc()
			c.compactionRunsLastSuccess.SetToCurrentTime()
		} else if compactionErrorCount == 0 {
			c.compactionRunsShutdown.Inc()
		} else {
			c.compactionRunsErred.Inc()
		}

		// Reset progress metrics once done.
		c.compactionRunDiscoveredTenants.Set(0)
		c.compactionRunSkippedTenants.Set(0)
		c.compactionRunSucceededTenants.Set(0)
		c.compactionRunFailedTenants.Set(0)
	}()

	level.Info(c.logger).Log("msg", "discovering users from bucket")
	users, err := c.discoverUsersWithRetries(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			compactionErrorCount++
			level.Error(c.logger).Log("msg", "failed to discover users from bucket", "err", err)
		}
		return
	}

	level.Info(c.logger).Log("msg", "discovered users from bucket", "users", len(users))
	c.compactionRunDiscoveredTenants.Set(float64(len(users)))

	// When starting multiple compactor replicas nearly at the same time, running in a cluster with
	// a large number of tenants, we may end up in a situation where the 1st user is compacted by
	// multiple replicas at the same time. Shuffling users helps reduce the likelihood this will happen.
	rand.Shuffle(len(users), func(i, j int) {
		users[i], users[j] = users[j], users[i]
	})

	// Keep track of users owned by this shard, so that we can delete the local files for all other users.
	ownedUsers := map[string]struct{}{}
	for _, userID := range users {
		// Ensure the context has not been canceled (ie. compactor shutdown has been triggered).
		if ctx.Err() != nil {
			level.Info(c.logger).Log("msg", "interrupting compaction of user blocks", "err", err)
			return
		}

		// Ensure the user ID belongs to our shard.
		if owned, err := c.shardingStrategy.compactorOwnsUser(userID); err != nil {
			c.compactionRunSkippedTenants.Inc()
			level.Warn(c.logger).Log("msg", "unable to check if user is owned by this shard", "user", userID, "err", err)
			continue
		} else if !owned {
			c.compactionRunSkippedTenants.Inc()
			level.Debug(c.logger).Log("msg", "skipping user because it is not owned by this shard", "user", userID)
			continue
		}

		ownedUsers[userID] = struct{}{}

		if markedForDeletion, err := mimir_tsdb.TenantDeletionMarkExists(ctx, c.bucketClient, userID); err != nil {
			c.compactionRunSkippedTenants.Inc()
			level.Warn(c.logger).Log("msg", "unable to check if user is marked for deletion", "user", userID, "err", err)
			continue
		} else if markedForDeletion {
			c.compactionRunSkippedTenants.Inc()
			level.Debug(c.logger).Log("msg", "skipping user because it is marked for deletion", "user", userID)
			continue
		}

		level.Info(c.logger).Log("msg", "starting compaction of user blocks", "user", userID)

		if err = c.compactUserWithRetries(ctx, userID); err != nil {
			switch {
			case errors.Is(err, context.Canceled):
				// We don't want to count shutdowns as failed compactions because we will pick up with the rest of the compaction after the restart.
				level.Info(c.logger).Log("msg", "compaction for user was interrupted by a shutdown", "user", userID)
				return
			case errors.Is(err, syscall.ENOSPC):
				c.outOfSpace.Inc()
				fallthrough
			default:
				c.compactionRunFailedTenants.Inc()
				compactionErrorCount++
				level.Error(c.logger).Log("msg", "failed to compact user blocks", "user", userID, "err", err)
			}
			continue
		}

		c.compactionRunSucceededTenants.Inc()
		level.Info(c.logger).Log("msg", "successfully compacted user blocks", "user", userID)
	}

	// Delete local files for unowned tenants, if there are any. This cleans up
	// leftover local files for tenants that belong to different compactors now,
	// or have been deleted completely.
	for userID := range c.listTenantsWithMetaSyncDirectories() {
		if _, owned := ownedUsers[userID]; owned {
			continue
		}

		dir := c.metaSyncDirForUser(userID)
		s, err := os.Stat(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				level.Warn(c.logger).Log("msg", "failed to stat local directory with user data", "dir", dir, "err", err)
			}
			continue
		}

		if s.IsDir() {
			err := os.RemoveAll(dir)
			if err == nil {
				level.Info(c.logger).Log("msg", "deleted directory for user not owned by this shard", "dir", dir)
			} else {
				level.Warn(c.logger).Log("msg", "failed to delete directory for user not owned by this shard", "dir", dir, "err", err)
			}
		}
	}

	succeeded = true
}

func (c *MultitenantCompactor) compactUserWithRetries(ctx context.Context, userID string) error {
	var lastErr error

	retries := backoff.New(ctx, backoff.Config{
		MinBackoff: c.compactorCfg.retryMinBackoff,
		MaxBackoff: c.compactorCfg.retryMaxBackoff,
		MaxRetries: c.compactorCfg.CompactionRetries,
	})

	for retries.Ongoing() {
		lastErr = c.compactUser(ctx, userID)
		if lastErr == nil {
			return nil
		}

		retries.Wait()
	}

	return lastErr
}

func (c *MultitenantCompactor) compactUser(ctx context.Context, userID string) error {
	userBucket := bucket.NewUserBucketClient(userID, c.bucketClient, c.cfgProvider)
	userLogger := util_log.WithUserID(userID, c.logger)

	reg := prometheus.NewRegistry()
	defer c.syncerMetrics.gatherThanosSyncerMetrics(reg, userLogger)

	// Filters out duplicate blocks that can be formed from two or more overlapping
	// blocks that fully submatch the source blocks of the older blocks.
	deduplicateBlocksFilter := NewShardAwareDeduplicateFilter()

	// List of filters to apply (order matters).
	fetcherFilters := []block.MetadataFilter{
		NewLabelRemoverFilter(compactionIgnoredLabels),
		deduplicateBlocksFilter,
		// removes blocks that should not be compacted due to being marked so.
		NewNoCompactionMarkFilter(userBucket),
	}

	var metaCache *block.MetaCache
	metaCacheSize := c.cfgProvider.CompactorInMemoryTenantMetaCacheSize(userID)
	if metaCacheSize == 0 {
		delete(c.metaCaches, userID)
	} else {
		metaCache = c.metaCaches[userID]
		if metaCache == nil || metaCache.MaxSize() != metaCacheSize {
			// We use min compaction level equal to configured block ranges.
			// Blocks created by ingesters start with compaction level 1. When blocks are first compacted (blockRanges[0], possibly split-compaction), compaction level will be 2.
			// Higher the compaction level, higher chance of finding the same block over and over, and that's where cache helps the most.
			// Blocks with 64 sources take at least 1 KiB of memory (each source = 16 bytes). Blocks with many sources are more expensive to reparse over and over again.
			metaCache = block.NewMetaCache(metaCacheSize, len(c.compactorCfg.BlockRanges), 64)
			c.metaCaches[userID] = metaCache
		}
	}

	// Disable maxLookback (set to 0s) when block upload is enabled, block upload enabled implies there will be blocks
	// beyond the lookback period, we don't want the compactor to skip these
	var maxLookback = c.cfgProvider.CompactorMaxLookback(userID)
	if c.cfgProvider.CompactorBlockUploadEnabled(userID) {
		maxLookback = 0
	}

	fetcher, err := block.NewMetaFetcher(
		userLogger,
		c.compactorCfg.MetaSyncConcurrency,
		userBucket,
		c.metaSyncDirForUser(userID),
		reg,
		fetcherFilters,
		metaCache,
		maxLookback,
	)
	if err != nil {
		return err
	}

	syncer, err := newMetaSyncer(
		userLogger,
		reg,
		userBucket,
		fetcher,
		deduplicateBlocksFilter,
		c.blocksMarkedForDeletion,
	)
	if err != nil {
		return errors.Wrap(err, "failed to create syncer")
	}

	compactor, err := NewBucketCompactor(
		userLogger,
		syncer,
		c.blocksGrouperFactory(ctx, c.compactorCfg, c.cfgProvider, userID, userLogger, reg),
		c.blocksPlanner,
		c.blocksCompactor,
		path.Join(c.compactorCfg.DataDir, "compact"),
		userBucket,
		c.compactorCfg.CompactionConcurrency,
		true, // Skip unhealthy blocks, and mark them for no-compaction.
		c.shardingStrategy.ownJob,
		c.jobsOrder,
		c.compactorCfg.CompactionWaitPeriod,
		c.compactorCfg.BlockSyncConcurrency,
		c.bucketCompactorMetrics,
		c.compactorCfg.UploadSparseIndexHeaders,
		c.compactorCfg.SparseIndexHeadersSamplingRate,
		c.compactorCfg.SparseIndexHeadersConfig,
		c.cfgProvider.CompactorMaxPerBlockUploadConcurrency(userID),
	)
	if err != nil {
		return errors.Wrap(err, "failed to create bucket compactor")
	}

	if err := compactor.Compact(ctx, c.compactorCfg.MaxCompactionTime); err != nil {
		return errors.Wrap(err, "compaction")
	}

	if metaCache != nil {
		items, size, hits, misses := metaCache.Stats()
		level.Info(userLogger).Log("msg", "per-user meta cache stats after compacting user", "items", items, "bytes_size", size, "hits", hits, "misses", misses)
	}
	return nil
}

func (c *MultitenantCompactor) discoverUsersWithRetries(ctx context.Context) ([]string, error) {
	var lastErr error

	retries := backoff.New(ctx, backoff.Config{
		MinBackoff: c.compactorCfg.retryMinBackoff,
		MaxBackoff: c.compactorCfg.retryMaxBackoff,
		MaxRetries: c.compactorCfg.CompactionRetries,
	})

	for retries.Ongoing() {
		var users []string

		users, lastErr = c.discoverUsers(ctx)
		if lastErr == nil {
			return users, nil
		}

		retries.Wait()
	}

	return nil, lastErr
}

func (c *MultitenantCompactor) discoverUsers(ctx context.Context) ([]string, error) {
	return mimir_tsdb.ListUsers(ctx, c.bucketClient)
}

// shardingStrategy describes whether compactor "owns" given user or job.
type shardingStrategy interface {
	compactorOwnsUser(userID string) (bool, error)
	// blocksCleanerOwnsUser must be concurrency-safe
	blocksCleanerOwnsUser(userID string) (bool, error)
	ownJob(job *Job) (bool, error)
	// instanceOwningJob returns instance owning the job based on ring. It ignores per-instance allowed tenants.
	instanceOwningJob(job *Job) (ring.InstanceDesc, error)
}

// splitAndMergeShardingStrategy is used by split-and-merge compactor when configured with sharding.
// All compactors from user's shard own the user for compaction purposes, and plan jobs.
// Each job is only owned and executed by single compactor.
// Only one of compactors from user's shard will do cleanup.
type splitAndMergeShardingStrategy struct {
	allowedTenants *util.AllowList
	ring           *ring.Ring
	ringLifecycler *ring.BasicLifecycler
	configProvider ConfigProvider
}

func newSplitAndMergeShardingStrategy(allowedTenants *util.AllowList, ring *ring.Ring, ringLifecycler *ring.BasicLifecycler, configProvider ConfigProvider) *splitAndMergeShardingStrategy {
	return &splitAndMergeShardingStrategy{
		allowedTenants: allowedTenants,
		ring:           ring,
		ringLifecycler: ringLifecycler,
		configProvider: configProvider,
	}
}

// Only a single instance in the subring can run the blocks cleaner for the given user. blocksCleanerOwnsUser is concurrency-safe.
func (s *splitAndMergeShardingStrategy) blocksCleanerOwnsUser(userID string) (bool, error) {
	if !s.allowedTenants.IsAllowed(userID) {
		return false, nil
	}

	r := s.ring.ShuffleShard(userID, s.configProvider.CompactorTenantShardSize(userID))

	return instanceOwnsTokenInRing(r, s.ringLifecycler.GetInstanceAddr(), userID)
}

// ALL compactors should plan jobs for all users.
func (s *splitAndMergeShardingStrategy) compactorOwnsUser(userID string) (bool, error) {
	if !s.allowedTenants.IsAllowed(userID) {
		return false, nil
	}

	r := s.ring.ShuffleShard(userID, s.configProvider.CompactorTenantShardSize(userID))

	return r.HasInstance(s.ringLifecycler.GetInstanceID()), nil
}

// Only a single compactor should execute the job.
func (s *splitAndMergeShardingStrategy) ownJob(job *Job) (bool, error) {
	ok, err := s.compactorOwnsUser(job.UserID())
	if err != nil || !ok {
		return ok, err
	}

	r := s.ring.ShuffleShard(job.UserID(), s.configProvider.CompactorTenantShardSize(job.UserID()))

	return instanceOwnsTokenInRing(r, s.ringLifecycler.GetInstanceAddr(), job.ShardingKey())
}

func (s *splitAndMergeShardingStrategy) instanceOwningJob(job *Job) (ring.InstanceDesc, error) {
	r := s.ring.ShuffleShard(job.UserID(), s.configProvider.CompactorTenantShardSize(job.UserID()))

	rs, err := instancesForKey(r, job.ShardingKey())
	if err != nil {
		return ring.InstanceDesc{}, err
	}

	if len(rs.Instances) != 1 {
		return ring.InstanceDesc{}, fmt.Errorf("unexpected number of compactors in the shard (expected 1, got %d)", len(rs.Instances))
	}

	return rs.Instances[0], nil
}

func instancesForKey(r ring.ReadRing, key string) (ring.ReplicationSet, error) {
	// Hash the key.
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	hash := hasher.Sum32()

	return r.Get(hash, RingOp, nil, nil, nil)
}

func instanceOwnsTokenInRing(r ring.ReadRing, instanceAddr string, key string) (bool, error) {
	// Check whether this compactor instance owns the token.
	rs, err := instancesForKey(r, key)
	if err != nil {
		return false, err
	}

	if len(rs.Instances) != 1 {
		return false, fmt.Errorf("unexpected number of compactors in the shard (expected 1, got %d)", len(rs.Instances))
	}

	return rs.Instances[0].Addr == instanceAddr, nil
}

const compactorMetaPrefix = "compactor-meta-"

// metaSyncDirForUser returns directory to store cached meta files.
// The fetcher stores cached metas in the "meta-syncer/" sub directory,
// but we prefix it with "compactor-meta-" in order to guarantee no clashing with
// the directory used by the Thanos Syncer, whatever is the user ID.
func (c *MultitenantCompactor) metaSyncDirForUser(userID string) string {
	return filepath.Join(c.compactorCfg.DataDir, compactorMetaPrefix+userID)
}

// This function returns tenants with meta sync directories found on local disk. On error, it returns nil map.
func (c *MultitenantCompactor) listTenantsWithMetaSyncDirectories() map[string]struct{} {
	result := map[string]struct{}{}

	files, err := os.ReadDir(c.compactorCfg.DataDir)
	if err != nil {
		return nil
	}

	for _, f := range files {
		if !f.IsDir() {
			continue
		}

		if !strings.HasPrefix(f.Name(), compactorMetaPrefix) {
			continue
		}

		result[f.Name()[len(compactorMetaPrefix):]] = struct{}{}
	}

	return result
}
