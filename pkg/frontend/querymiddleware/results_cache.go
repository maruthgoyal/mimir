// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/results_cache.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querymiddleware

import (
	"cmp"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/types"
	"github.com/grafana/dskit/cache"
	"github.com/grafana/dskit/tenant"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/querier/stats"
	"github.com/grafana/mimir/pkg/util"
)

const (
	// resultsCacheVersion should be increased every time cache should be invalidated (after a bugfix or cache format change).
	resultsCacheVersion = 1

	// cacheControlHeader is the name of the cache control header.
	cacheControlHeader = "Cache-Control"

	// noStoreValue is the value that cacheControlHeader has if the response indicates that the results should not be cached.
	noStoreValue = "no-store"
)

var (
	supportedResultsCacheBackends = []string{cache.BackendMemcached}

	errUnsupportedBackend = errors.New("unsupported cache backend")
)

// ResultsCacheConfig is the config for the results cache.
type ResultsCacheConfig struct {
	cache.BackendConfig `yaml:",inline"`
	Compression         cache.CompressionConfig `yaml:",inline"`
}

// RegisterFlags registers flags.
func (cfg *ResultsCacheConfig) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.Backend, "query-frontend.results-cache.backend", "", fmt.Sprintf("Backend for query-frontend results cache, if not empty. Supported values: %s.", strings.Join(supportedResultsCacheBackends, ", ")))
	cfg.Memcached.RegisterFlagsWithPrefix("query-frontend.results-cache.memcached.", f)
	cfg.Compression.RegisterFlagsWithPrefix(f, "query-frontend.results-cache.")
}

func (cfg *ResultsCacheConfig) Validate() error {
	if cfg.Backend != "" && !util.StringsContain(supportedResultsCacheBackends, cfg.Backend) {
		return errUnsupportedResultsCacheBackend(cfg.Backend)
	}

	switch cfg.Backend {
	case cache.BackendMemcached:
		if err := cfg.Memcached.Validate(); err != nil {
			return errors.Wrap(err, "query-frontend results cache")
		}
	}

	if err := cfg.Compression.Validate(); err != nil {
		return errors.Wrap(err, "query-frontend results cache")
	}
	return nil
}

func errUnsupportedResultsCacheBackend(backend string) error {
	return fmt.Errorf("%w: %q, supported values: %v", errUnsupportedBackend, backend, supportedResultsCacheBackends)
}

type resultsCacheMetrics struct {
	cacheRequests prometheus.Counter
	cacheHits     prometheus.Counter
}

func newResultsCacheMetrics(requestType string, reg prometheus.Registerer) *resultsCacheMetrics {
	return &resultsCacheMetrics{
		cacheRequests: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name:        "cortex_frontend_query_result_cache_requests_total",
			Help:        "Total number of requests (or partial requests) looked up in the results cache.",
			ConstLabels: map[string]string{"request_type": requestType},
		}),
		cacheHits: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name:        "cortex_frontend_query_result_cache_hits_total",
			Help:        "Total number of requests (or partial requests) fetched from the results cache.",
			ConstLabels: map[string]string{"request_type": requestType},
		}),
	}
}

// newResultsCache creates a new results cache based on the input configuration.
func newResultsCache(cfg ResultsCacheConfig, logger log.Logger, reg prometheus.Registerer) (cache.Cache, error) {
	// Add the "component" label similarly to other components, so that metrics don't clash and have the same labels set
	// when running in monolithic mode.
	reg = prometheus.WrapRegistererWith(prometheus.Labels{"component": "query-frontend"}, reg)

	client, err := cache.CreateClient("frontend-cache", cfg.BackendConfig, logger, prometheus.WrapRegistererWithPrefix("thanos_", reg))
	if err != nil {
		return nil, err
	} else if client == nil {
		return nil, errUnsupportedResultsCacheBackend(cfg.Backend)
	}

	return cache.NewVersioned(
		cache.NewSpanlessTracingCache(client, logger, tenant.NewMultiResolver()),
		resultsCacheVersion,
	), nil
}

// Extractor is used by the cache to extract a subset of a response from a cache entry.
type Extractor interface {
	// Extract extracts a subset of a response from the `start` and `end` timestamps in milliseconds in the `from` response.
	Extract(start, end int64, from Response) Response
	ResponseWithoutHeaders(resp Response) Response
}

// PrometheusResponseExtractor helps extracting specific info from Query Response.
type PrometheusResponseExtractor struct{}

// Extract extracts response for specific a range from a response.
// The from Response is not closed, nor is it finalizer returned as part of the new response.
// It is the responsibility of the caller to close their input resources.
func (PrometheusResponseExtractor) Extract(start, end int64, from Response) Response {
	promRes, ok := from.GetPrometheusResponse()
	if !ok {
		panic("expected PrometheusResponse")
	}
	var data *PrometheusData
	if promRes.Data != nil {
		data = &PrometheusData{
			ResultType: promRes.Data.ResultType,
			Result:     extractMatrix(start, end, promRes.Data.Result),
		}
	}
	return &PrometheusResponse{
		Status:   promRes.Status,
		Data:     data,
		Headers:  promRes.Headers,
		Warnings: promRes.Warnings,
		Infos:    promRes.Infos,
	}
}

// ResponseWithoutHeaders is useful in caching data without headers since
// we anyways do not need headers for sending back the response so this saves some space by reducing size of the objects.
// The supplied Response is not closed, nor is it finalizer returned as part of the new response.
// It is the responsibility of the caller to close their input resources.
func (PrometheusResponseExtractor) ResponseWithoutHeaders(resp Response) Response {
	promRes, ok := resp.GetPrometheusResponse()
	if !ok {
		panic("expected PrometheusResponse")
	}
	var data *PrometheusData
	if promRes.Data != nil {
		data = &PrometheusData{
			ResultType: promRes.Data.ResultType,
			Result:     promRes.Data.Result,
		}
	}
	return &PrometheusResponse{
		Status:   promRes.Status,
		Data:     data,
		Warnings: promRes.Warnings,
		Infos:    promRes.Infos,
	}
}

// ErrUnsupportedRequest is intended to be used with CacheKeyGenerator
var ErrUnsupportedRequest = errors.New("request is not cacheable")

// CacheKeyGenerator generates cache keys. This is a useful interface for downstream
// consumers who wish to implement their own strategies.
type CacheKeyGenerator interface {
	// QueryRequest should generate a cache key based on the tenant ID and MetricsQueryRequest.
	QueryRequest(ctx context.Context, tenantID string, r MetricsQueryRequest) string

	// QueryRequestError should generate a cache key based on errors for the tenant ID and MetricsQueryRequest.
	QueryRequestError(ctx context.Context, tenantID string, r MetricsQueryRequest) string

	// QueryRequestLimiter should generate a cache key based on the tenant ID and MetricsQueryRequest.
	QueryRequestLimiter(ctx context.Context, tenantID string, r MetricsQueryRequest) string

	// LabelValues should return a cache key for a label values request. The cache key does not need to contain the tenant ID.
	// LabelValues can return ErrUnsupportedRequest, in which case the response won't be treated as an error, but the item will still not be cached.
	// LabelValues should return a nil *GenericQueryCacheKey when it returns an error and
	// should always return non-nil *GenericQueryCacheKey when the returned error is nil.
	LabelValues(r *http.Request) (*GenericQueryCacheKey, error)

	// LabelValuesCardinality should return a cache key for a label values cardinality request. The cache key does not need to contain the tenant ID.
	// LabelValuesCardinality can return ErrUnsupportedRequest, in which case the response won't be treated as an error, but the item will still not be cached.
	// LabelValuesCardinality should return a nil *GenericQueryCacheKey when it returns an error and
	// should always return non-nil *GenericQueryCacheKey when the returned error is nil.
	LabelValuesCardinality(r *http.Request) (*GenericQueryCacheKey, error)
}

type DefaultCacheKeyGenerator struct {
	codec Codec
	// interval is a constant split interval when determining cache keys for QueryRequest.
	interval time.Duration
}

func NewDefaultCacheKeyGenerator(codec Codec, interval time.Duration) DefaultCacheKeyGenerator {
	return DefaultCacheKeyGenerator{
		codec:    codec,
		interval: interval,
	}
}

// QueryRequest generates a cache key based on the userID, MetricsQueryRequest and interval.
func (g DefaultCacheKeyGenerator) QueryRequest(_ context.Context, tenantID string, r MetricsQueryRequest) string {
	startInterval := r.GetStart() / g.interval.Milliseconds()
	stepOffset := r.GetStart() % r.GetStep()

	// Use original format for step-aligned request, so that we can use existing cached results for such requests.
	if stepOffset == 0 {
		return fmt.Sprintf("%s:%s:%d:%d", tenantID, r.GetQuery(), r.GetStep(), startInterval)
	}

	return fmt.Sprintf("%s:%s:%d:%d:%d", tenantID, r.GetQuery(), r.GetStep(), startInterval, stepOffset)
}

func (g DefaultCacheKeyGenerator) QueryRequestError(_ context.Context, tenantID string, r MetricsQueryRequest) string {
	start := r.GetStart()
	end := r.GetEnd()
	if start == end {
		// For the case of an instant query, don't rely on the query's time in the errors caching key.
		// I.e. if a recording rule's query fails, it will likely fail on subsequent evaluations with the updated time.
		start = 0
		end = 0
	}
	return fmt.Sprintf("EC:%s:%s:%d:%d:%d", tenantID, r.GetQuery(), start, end, r.GetStep())
}

func (g DefaultCacheKeyGenerator) QueryRequestLimiter(_ context.Context, tenantID string, r MetricsQueryRequest) string {
	return fmt.Sprintf("QL:%s:%s", tenantID, r.GetQuery())
}

// shouldCacheFn checks whether the current request should go to cache
// or not. If not, just send the request to next handler.
type shouldCacheFn func(r MetricsQueryRequest) bool

// resultsCacheAlwaysEnabled is a shouldCacheFn function always returning true.
var resultsCacheAlwaysEnabled = func(_ MetricsQueryRequest) bool { return true }

var resultsCacheAlwaysDisabled = func(_ MetricsQueryRequest) bool { return false }

var resultsCacheEnabledByOption = func(r MetricsQueryRequest) bool {
	return !r.GetOptions().CacheDisabled
}

// isRequestCachable says whether the request is eligible for caching.
func isRequestCachable(req MetricsQueryRequest, maxCacheTime int64, cacheUnalignedRequests bool, logger log.Logger) (cachable bool, reason string) {
	// We can run with step alignment disabled because Grafana does it already. Mimir automatically aligning start and end is not
	// PromQL compatible. But this means we cannot cache queries that do not have their start and end aligned.
	if !cacheUnalignedRequests && !isRequestStepAligned(req) {
		return false, notCachableReasonUnalignedTimeRange
	}

	// Do not cache it at all if the query time range is more recent than the configured max cache freshness.
	if req.GetStart() > maxCacheTime {
		return false, notCachableReasonTooNew
	}

	if cachable, reason := areEvaluationTimeModifiersCachable(req, maxCacheTime, logger); !cachable {
		return false, reason
	}

	return true, ""
}

// isResponseCachable returns true if a response hasn't explicitly disabled caching
// via an HTTP header, false otherwise.
func isResponseCachable(r Response) bool {
	for _, hv := range r.GetHeaders() {
		if hv.GetName() == cacheControlHeader {
			return !slices.Contains(hv.GetValues(), noStoreValue)
		}
	}

	return true
}

var (
	errAtModifierAfterEnd = errors.New("at modifier after end")
	errNegativeOffset     = errors.New("negative offset")
)

// areEvaluationTimeModifiersCachable returns true if the @ modifier and the offset modifier results are safe to cache,
// false otherwise. The reason for not being safe to cache is returned as a string.
func areEvaluationTimeModifiersCachable(r MetricsQueryRequest, maxCacheTime int64, logger log.Logger) (bool, string) {
	// There are 3 cases when evaluation time modifiers are not safe to cache:
	//   1. When @ modifier points to time beyond the maxCacheTime.
	//   2. If the @ modifier time is > the query range end while being
	//      below maxCacheTime. In such cases if any tenant is intentionally
	//      playing with old data, we could cache empty result if we look
	//      beyond query end.
	//   3. When query contains a negative offset.
	query := r.GetQuery()
	if !strings.Contains(query, "@") && !strings.Contains(query, "offset") {
		return true, ""
	}
	expr, err := parser.ParseExpr(query)
	if err != nil {
		// We are being pessimistic in such cases.
		return false, notCachableReasonModifiersNotCachableFailedParse
	}

	// This resolves the start() and end() used with the @ modifier.
	expr, err = promql.PreprocessExpr(expr, timestamp.Time(r.GetStart()), timestamp.Time(r.GetEnd()), time.Duration(r.GetStep())*time.Millisecond)
	if err != nil {
		// We are being pessimistic in such cases.
		return false, notCachableReasonModifiersNotCachableFailedPreprocess
	}

	end := r.GetEnd()
	cachable := true
	check := func(ts *int64, offset time.Duration) error {
		if offset < 0 {
			cachable = false
			return errNegativeOffset
		}
		if ts != nil && (*ts > end || *ts > maxCacheTime) {
			cachable = false
			return errAtModifierAfterEnd
		}
		return nil
	}

	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		switch e := n.(type) {
		case *parser.VectorSelector:
			return check(e.Timestamp, e.OriginalOffset)
		case *parser.SubqueryExpr:
			return check(e.Timestamp, e.OriginalOffset)
		}
		return nil
	})

	if !cachable {
		return false, notCachableReasonModifiersNotCachable
	}
	return true, ""
}

// mergeCacheExtentsForRequest merges the provided cache extents for the input request and returns merged extents.
// The input extents can be overlapping and are not required to be sorted.
func mergeCacheExtentsForRequest(ctx context.Context, r MetricsQueryRequest, merger Merger, extents []Extent) ([]Extent, error) {
	// Fast path.
	if len(extents) <= 1 {
		return extents, nil
	}

	slices.SortFunc(extents, func(a, b Extent) int {
		if a.Start == b.Start {
			// as an optimization, for two extents starts at the same time, we
			// put the bigger extent at the front of the slice, which helps
			// to reduce the amount of merge we have to do later.
			return cmp.Compare(b.End, a.End)
		}
		return cmp.Compare(a.Start, b.Start)
	})

	// Merge any extents - potentially overlapping
	accumulator, err := newAccumulator(extents[0])
	if err != nil {
		return nil, err
	}
	mergedExtents := make([]Extent, 0, len(extents))

	for i := 1; i < len(extents); i++ {
		if accumulator.End+r.GetStep() < extents[i].Start {
			mergedExtents, err = mergeCacheExtentsWithAccumulator(mergedExtents, accumulator)
			if err != nil {
				return nil, err
			}
			accumulator, err = newAccumulator(extents[i])
			if err != nil {
				return nil, err
			}
			continue
		}

		if accumulator.End >= extents[i].End {
			continue
		}
		accumulator.TraceId = otelTraceID(ctx)
		// Calculate the samples processed per step in the subrange of the extent that is being merged with the accumulator.
		samples := extractSamplesProcessedPerStep(extents[i], max(accumulator.End, extents[i].Start), extents[i].End)
		accumulator.SamplesProcessedPerStep = mergeSamplesProcessedPerStep(accumulator.SamplesProcessedPerStep, samples)
		accumulator.End = extents[i].End
		currentRes, err := extents[i].toResponse()
		if err != nil {
			return nil, err
		}
		merged, err := merger.MergeResponse(accumulator.Response, currentRes)
		if err != nil {
			return nil, err
		}
		accumulator.Response = merged

		if accumulator.QueryTimestampMs > 0 && extents[i].QueryTimestampMs > 0 {
			// Keep older (minimum) timestamp.
			accumulator.QueryTimestampMs = min(accumulator.QueryTimestampMs, extents[i].QueryTimestampMs)
		} else {
			// Some old extents may have zero timestamps. In that case we keep the non-zero one.
			// (Hopefully one of them is not zero, since we're only merging if there are some new extents.)
			accumulator.QueryTimestampMs = max(accumulator.QueryTimestampMs, extents[i].QueryTimestampMs)
		}
	}

	return mergeCacheExtentsWithAccumulator(mergedExtents, accumulator)
}

type accumulator struct {
	Response
	Extent
}

func mergeCacheExtentsWithAccumulator(extents []Extent, acc *accumulator) ([]Extent, error) {
	promRes, ok := acc.GetPrometheusResponse()
	if !ok {
		panic("expected PrometheusResponse or PrometheusResponseWithFinalizer")
	}
	marshalled, err := types.MarshalAny(promRes)

	if err != nil {
		return nil, err
	}
	return append(extents, Extent{
		Start:                   acc.Start,
		End:                     acc.End,
		Response:                marshalled,
		TraceId:                 acc.TraceId,
		QueryTimestampMs:        acc.QueryTimestampMs,
		SamplesProcessedPerStep: acc.SamplesProcessedPerStep,
	}), nil
}

func newAccumulator(base Extent) (*accumulator, error) {
	res, err := base.toResponse()
	if err != nil {
		return nil, err
	}
	return &accumulator{
		Response: res,
		Extent:   base,
	}, nil
}

func toExtent(ctx context.Context, req MetricsQueryRequest, res Response, queryTime time.Time, perStepStats []stats.StepStat) (Extent, error) {
	marshalled, err := types.MarshalAny(res)
	if err != nil {
		return Extent{}, err
	}

	return Extent{
		Start:                   req.GetStart(),
		End:                     req.GetEnd(),
		Response:                marshalled,
		TraceId:                 otelTraceID(ctx),
		QueryTimestampMs:        queryTime.UnixMilli(),
		SamplesProcessedPerStep: perStepStats,
	}, nil
}

// partitionCacheExtents calculates the required requests to satisfy req given the cached data.
// extents must be in order by start time.
func partitionCacheExtents(req MetricsQueryRequest, extents []Extent, minCacheExtent int64, extractor Extractor) ([]MetricsQueryRequest, []Response, []stats.StepStat, error) {
	var requests []MetricsQueryRequest
	var cachedResponses []Response
	start := req.GetStart()
	var cachedPerStepStat []stats.StepStat

	for _, extent := range extents {
		// If there is no overlap, ignore this extent.
		if extent.GetEnd() < start || extent.Start > req.GetEnd() {
			continue
		}

		// If this extent is tiny and request is not tiny, discard it: more efficient to do a few larger queries.
		// Hopefully tiny request can make tiny extent into not-so-tiny extent.

		// However if the step is large enough, the split_query_by_interval middleware would generate a query with same start and end.
		// For example, if the step size is more than 12h and the interval is 24h.
		// This means the extent's start and end time would be same, even if the timerange covers several hours.
		if (req.GetStart() != req.GetEnd()) && (req.GetEnd()-req.GetStart() > minCacheExtent) && (extent.End-extent.Start < minCacheExtent) {
			continue
		}

		// If there is a bit missing at the front, make a request for that.
		if start < extent.Start {
			r, err := req.WithStartEnd(start, extent.Start)
			if err != nil {
				return nil, nil, nil, err
			}

			requests = append(requests, r)
		}
		res, err := extent.toResponse()
		if err != nil {
			return nil, nil, nil, err
		}
		// extract the overlap from the cached extent.
		cachedResponses = append(cachedResponses, extractor.Extract(start, req.GetEnd(), res))
		// Extract the per step stats for the overlap.
		cachedPerStepStat = mergeSamplesProcessedPerStep(cachedPerStepStat, extractSamplesProcessedPerStep(extent, max(start, extent.Start), min(req.GetEnd(), extent.End)))

		// We want next request to start where extent ends, but we must make sure that
		// next start also has the same offset into the step as original request had, ie.
		// "start % req.Step" must be the same as "req.GetStart() % req.GetStep()".
		// We do that by computing "adjustment". Go's % operator is a "remainder" operator
		// and not "modulo" operator, which means it returns negative numbers in our case or zero
		// (because request.GetStart <= extent.End), and we need to adjust it by one step forward.
		// We don't do adjustments if extent.End is already on the same step-offset as request.Start,
		// although technically we could. But existing unit tests expect existing behaviour.

		adjust := (req.GetStart() - extent.End) % req.GetStep()
		if adjust < 0 {
			adjust += req.GetStep()
		}
		start = extent.End + adjust
	}

	// Lastly, make a request for any data missing at the end.
	if start < req.GetEnd() {
		r, err := req.WithStartEnd(start, req.GetEnd())
		if err != nil {
			return nil, nil, nil, err
		}

		requests = append(requests, r)
	}

	// If start and end are the same (valid in promql), start == req.GetEnd() and we won't do the query.
	// But we should only do the request if we don't have a valid cached response for it.
	if req.GetStart() == req.GetEnd() && len(cachedResponses) == 0 {
		requests = append(requests, req)
	}

	return requests, cachedResponses, cachedPerStepStat, nil
}

func filterRecentCacheExtents(req MetricsQueryRequest, maxCacheFreshness time.Duration, extractor Extractor, extents []Extent) ([]Extent, error) {
	maxCacheTime := (int64(model.Now().Add(-maxCacheFreshness)) / req.GetStep()) * req.GetStep()
	for i := range extents {
		// Never cache data for the latest freshness period.
		if extents[i].End > maxCacheTime {
			perStepStats := extractSamplesProcessedPerStep(extents[i], extents[i].Start, maxCacheTime)
			extents[i].SamplesProcessedPerStep = perStepStats
			extents[i].End = maxCacheTime
			res, err := extents[i].toResponse()
			if err != nil {
				return nil, err
			}
			extracted := extractor.Extract(extents[i].Start, maxCacheTime, res)
			marshalled, err := types.MarshalAny(extracted)
			if err != nil {
				return nil, err
			}
			extents[i].Response = marshalled
		}
	}
	return extents, nil
}

func otelTraceID(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

func extractMatrix(start, end int64, matrix []SampleStream) []SampleStream {
	result := make([]SampleStream, 0, len(matrix))
	for _, stream := range matrix {
		extracted, ok := extractSampleStream(start, end, stream)
		if ok {
			result = append(result, extracted)
		}
	}
	return result
}

func filterFloatStream(start, end int64, streamSamples []mimirpb.Sample) []mimirpb.Sample {
	result := make([]mimirpb.Sample, 0, len(streamSamples))
	for _, sample := range streamSamples {
		if start <= sample.TimestampMs && sample.TimestampMs <= end {
			result = append(result, sample)
		}
	}
	return result
}

func filterHistogramStream(start, end int64, streamSamples []mimirpb.FloatHistogramPair) []mimirpb.FloatHistogramPair {
	result := make([]mimirpb.FloatHistogramPair, 0, len(streamSamples))
	for _, sample := range streamSamples {
		if start <= sample.TimestampMs && sample.TimestampMs <= end {
			result = append(result, sample)
		}
	}
	return result
}

func extractSampleStream(start, end int64, stream SampleStream) (SampleStream, bool) {
	result := SampleStream{
		Labels: stream.Labels,
	}
	gotSamples := false
	gotHistograms := false
	if len(stream.Histograms) > 0 {
		histograms := filterHistogramStream(start, end, stream.Histograms)
		if len(histograms) > 0 {
			result.Histograms = histograms
			gotHistograms = true
		}
	}
	if len(stream.Samples) > 0 {
		samples := filterFloatStream(start, end, stream.Samples)
		if len(samples) > 0 {
			result.Samples = samples
			gotSamples = true
		}
	}
	if !gotHistograms && !gotSamples {
		return SampleStream{}, false
	}
	return result, true
}

func (e *Extent) toResponse() (Response, error) {
	msg, err := types.EmptyAny(e.Response)
	if err != nil {
		return nil, err
	}

	if err := types.UnmarshalAny(e.Response, msg); err != nil {
		return nil, err
	}

	resp, ok := msg.(Response)
	if !ok {
		return nil, fmt.Errorf("bad cached type")
	}
	return resp, nil
}

// cacheHashKey hashes key into something you can store in the results cache.
func cacheHashKey(key string) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(key)) // This'll never error.

	// Hex because memcache keys must be non-whitespace non-control ASCII
	return hex.EncodeToString(hasher.Sum(nil))
}

// extractSamplesProcessedPerStep extracts the per step samples count for the subrange within the extent.
func extractSamplesProcessedPerStep(extent Extent, start int64, end int64) []stats.StepStat {
	// Validate the subrange is valid and within the extent.
	if end < start || start < extent.Start || end > extent.End {
		return nil
	}

	var result []stats.StepStat
	for _, step := range extent.SamplesProcessedPerStep {
		if start <= step.Timestamp && step.Timestamp <= end {
			result = append(result, step)
		}
	}

	return result
}

func mergeSamplesProcessedPerStep(a, b []stats.StepStat) []stats.StepStat {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}

	merged := make([]stats.StepStat, 0, len(a)+len(b))
	i, j := 0, 0

	for i < len(a) && j < len(b) {
		if a[i].Timestamp < b[j].Timestamp {
			merged = append(merged, a[i])
			i++
		} else if b[j].Timestamp < a[i].Timestamp {
			merged = append(merged, b[j])
			j++
		} else {
			// Same timestamp, take the latter value
			merged = append(merged, b[j])
			i++
			j++
		}
	}

	// Append any remaining elements
	for ; i < len(a); i++ {
		merged = append(merged, a[i])
	}
	for ; j < len(b); j++ {
		merged = append(merged, b[j])
	}

	return merged
}

// sumSamplesProcessedPerStep sums values from multiple arrays of StepStat.
// For duplicate timestamps, later arrays win.
func sumSamplesProcessedPerStep(stats ...[]stats.StepStat) int64 {
	if len(stats) == 0 {
		return 0
	}

	// Fast path: single array - no duplicates possible, just sum directly
	if len(stats) == 1 {
		var total int64
		for _, step := range stats[0] {
			total += step.Value
		}
		return total
	}

	// Multiple arrays: handle duplicates with map (last array wins)
	m := make(map[int64]int64)
	for _, arr := range stats {
		for _, step := range arr {
			m[step.Timestamp] = step.Value
		}
	}

	var total int64
	for _, value := range m {
		total += value
	}

	return total
}
