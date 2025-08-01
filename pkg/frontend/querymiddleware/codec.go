// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/query_range.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querymiddleware

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/grafana/dskit/grpcutil"
	"github.com/grafana/dskit/user"
	"github.com/munnerz/goautoneg"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	v1 "github.com/prometheus/prometheus/web/api/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	apierror "github.com/grafana/mimir/pkg/api/error"
	"github.com/grafana/mimir/pkg/cardinality"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/querier"
	"github.com/grafana/mimir/pkg/querier/api"
	"github.com/grafana/mimir/pkg/querier/stats"
	"github.com/grafana/mimir/pkg/streamingpromql/compat"
	"github.com/grafana/mimir/pkg/util"
	"github.com/grafana/mimir/pkg/util/chunkinfologger"
	"github.com/grafana/mimir/pkg/util/spanlogger"
)

var (
	errEndBeforeStart = apierror.New(apierror.TypeBadData, `invalid parameter "end": end timestamp must not be before start time`)
	errNegativeStep   = apierror.New(apierror.TypeBadData, `invalid parameter "step": zero or negative query resolution step widths are not accepted. Try a positive integer`)
	errStepTooSmall   = apierror.New(apierror.TypeBadData, "exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")
	allFormats        = []string{formatJSON, formatProtobuf}

	// List of HTTP headers to propagate when a Prometheus request is encoded into a HTTP request.
	// api.ReadConsistencyHeader is propagated as HTTP header -> Request.Context -> Request.Header, so there's no need to explicitly propagate it here.
	codecPropagateHeadersMetrics = []string{compat.ForceFallbackHeaderName, chunkinfologger.ChunkInfoLoggingHeader, api.ReadConsistencyOffsetsHeader, querier.FilterQueryablesHeader}
	// api.ReadConsistencyHeader is propagated as HTTP header -> Request.Context -> Request.Header, so there's no need to explicitly propagate it here.
	codecPropagateHeadersLabels = []string{api.ReadConsistencyOffsetsHeader, querier.FilterQueryablesHeader}
)

const maxResolutionPoints = 11000

const (
	// statusSuccess Prometheus success result.
	statusSuccess = "success"
	// statusSuccess Prometheus error result.
	statusError = "error"

	totalShardsControlHeader = "Sharding-Control"

	operationEncode = "encode"
	operationDecode = "decode"

	formatJSON     = "json"
	formatProtobuf = "protobuf"
)

// Merger is used by middlewares making multiple requests to merge back all responses into a single one.
type Merger interface {
	// MergeResponse merges responses from multiple requests into a single Response
	MergeResponse(...Response) (Response, error)
}

// MetricsQueryRequest represents an instant or query range request that can be process by middlewares.
type MetricsQueryRequest interface {
	// GetID returns the ID of the request used to correlate downstream requests and responses.
	GetID() int64
	// GetPath returns the URL Path of the request
	GetPath() string
	// GetHeaders returns the HTTP headers in the request.
	GetHeaders() []*PrometheusHeader
	// GetStart returns the start timestamp of the query time range in milliseconds.
	GetStart() int64
	// GetEnd returns the end timestamp of the query time range in milliseconds.
	// The start and end timestamp are set to the same value in case of an instant query.
	GetEnd() int64
	// GetStep returns the step of the request in milliseconds, or 0 if this is an instant query.
	GetStep() int64
	// GetQuery returns the query of the request.
	GetQuery() string
	// GetMinT returns the minimum timestamp in milliseconds of data to be queried,
	// as determined from the start timestamp and any range vector or offset in the query.
	GetMinT() int64
	// GetMaxT returns the maximum timestamp in milliseconds of data to be queried,
	// as determined from the end timestamp and any offset in the query.
	GetMaxT() int64
	// GetOptions returns the options for the given request.
	GetOptions() Options
	// GetHints returns hints that could be optionally attached to the request to pass down the stack.
	// These hints can be used to optimize the query execution.
	GetHints() *Hints
	// GetLookbackDelta returns the lookback delta for the request.
	GetLookbackDelta() time.Duration
	// GetStats returns the stats parameter for the request.
	// See WithStats() comment for more details.
	GetStats() string
	// WithID clones the current request with the provided ID.
	WithID(id int64) (MetricsQueryRequest, error)
	// WithStartEnd clone the current request with different start and end timestamp.
	// Implementations must ensure minT and maxT are recalculated when the start and end timestamp change.
	WithStartEnd(startTime int64, endTime int64) (MetricsQueryRequest, error)
	// WithQuery clones the current request with a different query; returns error if query parse fails.
	// Implementations must ensure minT and maxT are recalculated when the query changes.
	WithQuery(string) (MetricsQueryRequest, error)
	// WithHeaders clones the current request with different headers.
	WithHeaders([]*PrometheusHeader) (MetricsQueryRequest, error)
	// WithExpr clones the current `PrometheusRangeQueryRequest` with a new query expression.
	// Implementations must ensure minT and maxT are recalculated when the query changes.
	WithExpr(parser.Expr) (MetricsQueryRequest, error)
	// WithTotalQueriesHint adds the number of total queries to this request's Hints.
	WithTotalQueriesHint(int32) (MetricsQueryRequest, error)
	// WithEstimatedSeriesCountHint WithEstimatedCardinalityHint adds a cardinality estimate to this request's Hints.
	WithEstimatedSeriesCountHint(uint64) (MetricsQueryRequest, error)
	// AddSpanTags writes information about this request to an OpenTracing span
	AddSpanTags(span trace.Span)
	// WithStats returns a copy of the current request with the provided value for the "stats" parameter.
	//
	// This value is passed to the querier to enable per-step statistics collection,
	// which are exposed via querier.Stats. Currently, only the value "all" has an effect in Mimir.
	// Note: unlike Prometheus, Mimir does not return query stats in the response body if stats is set.
	WithStats(string) (MetricsQueryRequest, error)
}

// LabelsSeriesQueryRequest represents a label names, label values, or series query request that can be process by middlewares.
type LabelsSeriesQueryRequest interface {
	// GetLabelName returns the label name param from a Label Values request `/api/v1/label/<label_name>/values`
	// or an empty string for a Label Names request `/api/v1/labels`
	GetLabelName() string
	// GetStart returns the start timestamp of the request in milliseconds
	GetStart() int64
	// GetStartOrDefault returns the start timestamp of the request in milliseconds,
	// or the Prometheus v1 API MinTime if no start timestamp was provided on the original request.
	GetStartOrDefault() int64
	// GetEnd returns the start timestamp of the request in milliseconds
	GetEnd() int64
	// GetEndOrDefault returns the end timestamp of the request in milliseconds,
	// or the Prometheus v1 API MaxTime if no end timestamp was provided on the original request.
	GetEndOrDefault() int64
	// GetLabelMatcherSets returns the label matchers a.k.a series selectors for Prometheus label query requests,
	// as retained in their original string format. This enables the request to be symmetrically decoded and encoded
	// to and from the http request format without needing to undo the Prometheus parser converting between formats
	// like `up{job="prometheus"}` and `{__name__="up, job="prometheus"}`, or other idiosyncrasies.
	GetLabelMatcherSets() []string
	// GetLimit returns the limit of the number of items in the response.
	GetLimit() uint64
	// GetHeaders returns the HTTP headers in the request.
	GetHeaders() []*PrometheusHeader
	// WithLabelName clones the current request with a different label name param.
	WithLabelName(string) (LabelsSeriesQueryRequest, error)
	// WithLabelMatcherSets clones the current request with different label matchers.
	WithLabelMatcherSets([]string) (LabelsSeriesQueryRequest, error)
	// WithHeaders clones the current request with different headers.
	WithHeaders([]*PrometheusHeader) (LabelsSeriesQueryRequest, error)
	// AddSpanTags writes information about this request to an OpenTracing span
	AddSpanTags(span trace.Span)
}

// Response represents a query range response.
type Response interface {
	proto.Message
	// GetHeaders returns the HTTP headers in the response.
	GetHeaders() []*PrometheusHeader
	// GetPrometheusResponse is a helper for where multiple types implement a PrometheusResponse
	GetPrometheusResponse() (*PrometheusResponse, bool)
	Close()
}

type codecMetrics struct {
	duration *prometheus.HistogramVec
	size     *prometheus.HistogramVec
}

func newCodecMetrics(registerer prometheus.Registerer) *codecMetrics {
	factory := promauto.With(registerer)
	second := 1.0
	ms := second / 1000
	kb := 1024.0
	mb := 1024 * kb

	return &codecMetrics{
		duration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cortex_frontend_query_response_codec_duration_seconds",
			Help:    "Total time spent encoding or decoding query result payloads, in seconds.",
			Buckets: prometheus.ExponentialBucketsRange(1*ms, 2*second, 10),
		}, []string{"operation", "format"}),
		size: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cortex_frontend_query_response_codec_payload_bytes",
			Help:    "Total size of query result payloads, in bytes.",
			Buckets: prometheus.ExponentialBucketsRange(1*kb, 512*mb, 10),
		}, []string{"operation", "format"}),
	}
}

// Codec is used to encode/decode query requests and responses so they can be passed down to middlewares.
type Codec struct {
	metrics                                         *codecMetrics
	lookbackDelta                                   time.Duration
	preferredQueryResultResponseFormat              string
	propagateHeadersMetrics, propagateHeadersLabels []string
}

type formatter interface {
	EncodeQueryResponse(resp *PrometheusResponse) ([]byte, error)
	EncodeLabelsResponse(resp *PrometheusLabelsResponse) ([]byte, error)
	EncodeSeriesResponse(resp *PrometheusSeriesResponse) ([]byte, error)
	DecodeQueryResponse([]byte) (*PrometheusResponse, error)
	DecodeLabelsResponse([]byte) (*PrometheusLabelsResponse, error)
	DecodeSeriesResponse([]byte) (*PrometheusSeriesResponse, error)
	Name() string
	ContentType() v1.MIMEType
}

var jsonFormatterInstance = jsonFormatter{}

var knownFormats = []formatter{
	jsonFormatterInstance,
	protobufFormatter{},
}

func NewCodec(
	registerer prometheus.Registerer,
	lookbackDelta time.Duration,
	queryResultResponseFormat string,
	propagateHeaders []string,
) Codec {
	return Codec{
		metrics:                            newCodecMetrics(registerer),
		lookbackDelta:                      lookbackDelta,
		preferredQueryResultResponseFormat: queryResultResponseFormat,
		propagateHeadersMetrics:            append(codecPropagateHeadersMetrics, propagateHeaders...),
		propagateHeadersLabels:             append(codecPropagateHeadersLabels, propagateHeaders...),
	}
}

// MergeResponse merges responses from multiple requests into a single Response
func (Codec) MergeResponse(responses ...Response) (Response, error) {
	if len(responses) == 0 {
		return newEmptyPrometheusResponse(), nil
	}

	promResponses := make([]*PrometheusResponse, 0, len(responses))
	promCloses := make([]func(), 0, len(responses))
	promWarningsMap := make(map[string]struct{}, 0)
	promInfosMap := make(map[string]struct{}, 0)
	var present struct{}

	for _, res := range responses {
		pr, ok := res.GetPrometheusResponse()
		if !ok {
			return nil, fmt.Errorf("error invalid response type: %T, expected a Prometheus response", res)
		}
		if pr.Status != statusSuccess {
			return nil, fmt.Errorf("can't merge an unsuccessful response")
		} else if pr.Data == nil {
			return nil, fmt.Errorf("can't merge response with no data")
		} else if pr.Data.ResultType != model.ValMatrix.String() {
			return nil, fmt.Errorf("can't merge result type %q", pr.Data.ResultType)
		}

		promResponses = append(promResponses, pr)
		for _, warning := range pr.Warnings {
			promWarningsMap[warning] = present
		}
		for _, info := range pr.Infos {
			promInfosMap[info] = present
		}
		promCloses = append(promCloses, res.Close)
	}

	var promWarnings []string
	for warning := range promWarningsMap {
		promWarnings = append(promWarnings, warning)
	}

	var promInfos []string
	for info := range promInfosMap {
		promInfos = append(promInfos, info)
	}

	// Merge the responses.
	slices.SortFunc(promResponses, func(a, b *PrometheusResponse) int {
		aTime := int64(-1)
		if len(a.Data.Result) > 0 && len(a.Data.Result[0].Samples) > 0 {
			aTime = a.Data.Result[0].Samples[0].TimestampMs
		}
		bTime := int64(-1)
		if len(b.Data.Result) > 0 && len(b.Data.Result[0].Samples) > 0 {
			bTime = b.Data.Result[0].Samples[0].TimestampMs
		}
		return cmp.Compare(aTime, bTime)
	})

	return &PrometheusResponseWithFinalizer{
		PrometheusResponse: &PrometheusResponse{
			Status: statusSuccess,
			Data: &PrometheusData{
				ResultType: model.ValMatrix.String(),
				Result:     matrixMerge(promResponses),
			},
			Warnings: promWarnings,
			Infos:    promInfos,
		},
		finalizer: func() {
			for _, close := range promCloses {
				close()
			}
		},
	}, nil
}

// DecodeMetricsQueryRequest decodes a MetricsQueryRequest from an http request.
func (c Codec) DecodeMetricsQueryRequest(_ context.Context, r *http.Request) (MetricsQueryRequest, error) {
	switch {
	case IsRangeQuery(r.URL.Path):
		return c.decodeRangeQueryRequest(r)
	case IsInstantQuery(r.URL.Path):
		return c.decodeInstantQueryRequest(r)
	default:
		return nil, fmt.Errorf("unknown metrics query API endpoint %s", r.URL.Path)
	}
}

func (c Codec) decodeRangeQueryRequest(r *http.Request) (MetricsQueryRequest, error) {
	reqValues, err := util.ParseRequestFormWithoutConsumingBody(r)
	if err != nil {
		return nil, apierror.New(apierror.TypeBadData, err.Error())
	}

	start, end, step, err := DecodeRangeQueryTimeParams(&reqValues)
	if err != nil {
		return nil, err
	}

	query := reqValues.Get("query")
	queryExpr, err := parser.ParseExpr(query)
	if err != nil {
		return nil, DecorateWithParamName(err, "query")
	}

	var options Options
	decodeOptions(r, &options)

	stats := reqValues.Get("stats")
	req := NewPrometheusRangeQueryRequest(
		r.URL.Path, httpHeadersToProm(r.Header), start, end, step, c.lookbackDelta, queryExpr, options, nil, stats,
	)
	return req, nil
}

func (c Codec) decodeInstantQueryRequest(r *http.Request) (MetricsQueryRequest, error) {
	reqValues, err := util.ParseRequestFormWithoutConsumingBody(r)
	if err != nil {
		return nil, apierror.New(apierror.TypeBadData, err.Error())
	}

	time, err := DecodeInstantQueryTimeParams(&reqValues)
	if err != nil {
		return nil, DecorateWithParamName(err, "time")
	}

	query := reqValues.Get("query")
	queryExpr, err := parser.ParseExpr(query)
	if err != nil {
		return nil, DecorateWithParamName(err, "query")
	}

	var options Options
	decodeOptions(r, &options)

	stats := reqValues.Get("stats")

	req := NewPrometheusInstantQueryRequest(
		r.URL.Path, httpHeadersToProm(r.Header), time, c.lookbackDelta, queryExpr, options, nil, stats,
	)
	return req, nil
}

func httpHeadersToProm(httpH http.Header) []*PrometheusHeader {
	if len(httpH) == 0 {
		return nil
	}
	headers := make([]*PrometheusHeader, 0, len(httpH))
	for h, hv := range httpH {
		headers = append(headers, &PrometheusHeader{Name: h, Values: slices.Clone(hv)})
	}
	slices.SortFunc(headers, func(a, b *PrometheusHeader) int {
		return strings.Compare(a.Name, b.Name)
	})
	return headers
}

// DecodeLabelsSeriesQueryRequest decodes a LabelsSeriesQueryRequest from an http request.
func (Codec) DecodeLabelsSeriesQueryRequest(_ context.Context, r *http.Request) (LabelsSeriesQueryRequest, error) {
	if !IsLabelsQuery(r.URL.Path) && !IsSeriesQuery(r.URL.Path) {
		return nil, fmt.Errorf("unknown labels or series query API endpoint %s", r.URL.Path)
	}

	reqValues, err := util.ParseRequestFormWithoutConsumingBody(r)
	if err != nil {
		return nil, apierror.New(apierror.TypeBadData, err.Error())
	}
	// see DecodeLabelsSeriesQueryTimeParams for notes on time param parsing compatibility
	// between label names, label values, and series requests
	start, end, err := DecodeLabelsSeriesQueryTimeParams(&reqValues)
	if err != nil {
		return nil, err
	}

	labelMatcherSets := reqValues["match[]"]

	limit := uint64(0) // 0 means unlimited
	if limitStr := reqValues.Get("limit"); limitStr != "" {
		limit, err = strconv.ParseUint(limitStr, 10, 64)
		if err != nil {
			return nil, apierror.New(apierror.TypeBadData, fmt.Sprintf("limit parameter must be greater than or equal to 0, got %s", limitStr))
		}
	}
	headers := httpHeadersToProm(r.Header)

	if IsSeriesQuery(r.URL.Path) {
		return &PrometheusSeriesQueryRequest{
			Path:             r.URL.Path,
			Headers:          headers,
			Start:            start,
			End:              end,
			LabelMatcherSets: labelMatcherSets,
			Limit:            limit,
		}, nil
	}
	if IsLabelNamesQuery(r.URL.Path) {
		return &PrometheusLabelNamesQueryRequest{
			Path:             r.URL.Path,
			Headers:          headers,
			Start:            start,
			End:              end,
			LabelMatcherSets: labelMatcherSets,
			Limit:            limit,
		}, nil
	}
	// else, must be Label Values Request due to IsLabelsQuery check at beginning of func
	return &PrometheusLabelValuesQueryRequest{
		Path:             r.URL.Path,
		Headers:          headers,
		LabelName:        labelValuesPathSuffix.FindStringSubmatch(r.URL.Path)[1],
		Start:            start,
		End:              end,
		LabelMatcherSets: labelMatcherSets,
		Limit:            limit,
	}, nil
}

// TimeParamType enumerates the types of time parameters in Prometheus API.
// https://prometheus.io/docs/prometheus/latest/querying/api/
type TimeParamType int

const (
	// RFC3339OrUnixMS represents the <rfc3339 | unix_timestamp> type in Prometheus Querying API docs
	RFC3339OrUnixMS TimeParamType = iota
	// DurationMS represents the <duration> type in Prometheus Querying API docs
	DurationMS
	// DurationMSOrFloatMS represents the <duration | float> in Prometheus Querying API docs
	DurationMSOrFloatMS
)

// PromTimeParamDecoder provides common functionality for decoding Prometheus time parameters.
type PromTimeParamDecoder struct {
	paramName     string
	timeType      TimeParamType
	isOptional    bool
	defaultMSFunc func() int64
}

func (p PromTimeParamDecoder) Decode(reqValues *url.Values) (int64, error) {
	rawValue := reqValues.Get(p.paramName)
	if rawValue == "" {
		if p.isOptional {
			if p.defaultMSFunc != nil {
				return p.defaultMSFunc(), nil
			}
			return 0, nil
		}
		return 0, apierror.New(apierror.TypeBadData, fmt.Sprintf("missing required parameter %q", p.paramName))
	}

	var t int64
	var err error
	switch p.timeType {
	case RFC3339OrUnixMS:
		t, err = util.ParseTime(rawValue)
	case DurationMS, DurationMSOrFloatMS:
		t, err = util.ParseDurationMS(rawValue)
	default:
		return 0, apierror.New(apierror.TypeInternal, fmt.Sprintf("unknown time type %v", p.timeType))
	}
	if err != nil {
		return 0, DecorateWithParamName(err, p.paramName)
	}

	return t, nil
}

var rangeStartParamDecodable = PromTimeParamDecoder{"start", RFC3339OrUnixMS, false, nil}
var rangeEndParamDecodable = PromTimeParamDecoder{"end", RFC3339OrUnixMS, false, nil}
var rangeStepEndParamDecodable = PromTimeParamDecoder{"step", DurationMSOrFloatMS, false, nil}

// DecodeRangeQueryTimeParams encapsulates Prometheus instant query time param parsing,
// emulating the logic in prometheus/prometheus/web/api/v1#API.query_range.
func DecodeRangeQueryTimeParams(reqValues *url.Values) (start, end, step int64, err error) {
	start, err = rangeStartParamDecodable.Decode(reqValues)
	if err != nil {
		return 0, 0, 0, err
	}

	end, err = rangeEndParamDecodable.Decode(reqValues)
	if err != nil {
		return 0, 0, 0, err
	}

	if end < start {
		return 0, 0, 0, errEndBeforeStart
	}

	step, err = rangeStepEndParamDecodable.Decode(reqValues)
	if err != nil {
		return 0, 0, 0, err
	}

	if step <= 0 {
		return 0, 0, 0, errNegativeStep
	}

	// For safety, limit the number of returned points per timeseries.
	// This is sufficient for 60s resolution for a week or 1h resolution for a year.
	if (end-start)/step > maxResolutionPoints {
		return 0, 0, 0, errStepTooSmall
	}

	return start, end, step, nil
}

func instantTimeParamNow() int64 {
	return time.Now().UTC().UnixMilli()
}

var instantTimeParamDecodable = PromTimeParamDecoder{"time", RFC3339OrUnixMS, true, instantTimeParamNow}

// DecodeInstantQueryTimeParams encapsulates Prometheus instant query time param parsing,
// emulating the logic in prometheus/prometheus/web/api/v1#API.query.
func DecodeInstantQueryTimeParams(reqValues *url.Values) (time int64, err error) {
	time, err = instantTimeParamDecodable.Decode(reqValues)
	if err != nil {
		return 0, err
	}

	return time, err
}

// Label names, label values, and series codec applies the prometheus/web/api/v1.MinTime and MaxTime defaults on read
// with GetStartOrDefault/GetEndOrDefault, so we don't need to apply them with a defaultMSFunc here.
// This allows the object to be symmetrically decoded and encoded to and from the http request format,
// as well as indicating when an optional time parameter was not included in the original request.
var labelsStartParamDecodable = PromTimeParamDecoder{"start", RFC3339OrUnixMS, true, nil}
var labelsEndParamDecodable = PromTimeParamDecoder{"end", RFC3339OrUnixMS, true, nil}

// DecodeLabelsSeriesQueryTimeParams encapsulates Prometheus query time param parsing
// for label names, label values, and series endpoints, emulating prometheus/prometheus/web/api/v1.
// Note: the Prometheus HTTP API spec claims that the series endpoint `start` and `end` parameters
// are not optional, but the Prometheus implementation allows them to be optional.
// Until this changes we can reuse the same PromTimeParamDecoder structs as the label names and values endpoints.
func DecodeLabelsSeriesQueryTimeParams(reqValues *url.Values) (start, end int64, err error) {
	start, err = labelsStartParamDecodable.Decode(reqValues)
	if err != nil {
		return 0, 0, err
	}

	end, err = labelsEndParamDecodable.Decode(reqValues)
	if err != nil {
		return 0, 0, err
	}

	if end != 0 && end < start {
		return 0, 0, errEndBeforeStart
	}

	return start, end, err
}

// DecodeCardinalityQueryParams strictly handles validation for cardinality API endpoint parameters.
// The current decoding of the cardinality requests is handled in the cardinality package
// which is not yet compatible with the codec's approach of using interfaces
// and multiple concrete proto implementations to represent different query types.
func DecodeCardinalityQueryParams(r *http.Request) (any, error) {
	var err error

	reqValues, err := util.ParseRequestFormWithoutConsumingBody(r)
	if err != nil {
		return nil, apierror.New(apierror.TypeBadData, err.Error())
	}

	var parsedReq any
	switch {
	case strings.HasSuffix(r.URL.Path, cardinalityLabelNamesPathSuffix):
		parsedReq, err = cardinality.DecodeLabelNamesRequestFromValues(reqValues)

	case strings.HasSuffix(r.URL.Path, cardinalityLabelValuesPathSuffix):
		parsedReq, err = cardinality.DecodeLabelValuesRequestFromValues(reqValues)

	case strings.HasSuffix(r.URL.Path, cardinalityActiveSeriesPathSuffix):
		parsedReq, err = cardinality.DecodeActiveSeriesRequestFromValues(reqValues)

	default:
		return nil, errors.New("unknown cardinality API endpoint")
	}

	if err != nil {
		return nil, apierror.New(apierror.TypeBadData, err.Error())
	}
	return parsedReq, nil
}

func decodeQueryMinMaxTime(queryExpr parser.Expr, start, end, step int64, lookbackDelta time.Duration) (minTime, maxTime int64) {
	evalStmt := &parser.EvalStmt{
		Expr:          queryExpr,
		Start:         util.TimeFromMillis(start),
		End:           util.TimeFromMillis(end),
		Interval:      time.Duration(step) * time.Millisecond,
		LookbackDelta: lookbackDelta,
	}

	minTime, maxTime = promql.FindMinMaxTime(evalStmt)
	return minTime, maxTime
}

func decodeOptions(r *http.Request, opts *Options) {
	opts.CacheDisabled = decodeCacheDisabledOption(r)

	for _, value := range r.Header.Values(totalShardsControlHeader) {
		shards, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			continue
		}
		opts.TotalShards = int32(shards)
		if opts.TotalShards < 1 {
			opts.ShardingDisabled = true
		}
	}
}

func decodeCacheDisabledOption(r *http.Request) bool {
	for _, value := range r.Header.Values(cacheControlHeader) {
		if strings.Contains(value, noStoreValue) {
			return true
		}
	}

	return false
}

// EncodeMetricsQueryRequest encodes a MetricsQueryRequest into an http request.
func (c Codec) EncodeMetricsQueryRequest(ctx context.Context, r MetricsQueryRequest) (*http.Request, error) {
	var u *url.URL
	switch r := r.(type) {
	case *PrometheusRangeQueryRequest:
		values := url.Values{
			"start": []string{encodeTime(r.GetStart())},
			"end":   []string{encodeTime(r.GetEnd())},
			"step":  []string{encodeDurationMs(r.GetStep())},
			"query": []string{r.GetQuery()},
		}
		if s := r.GetStats(); s != "" {
			values["stats"] = []string{s}
		}
		u = &url.URL{
			Path:     r.GetPath(),
			RawQuery: values.Encode(),
		}
	case *PrometheusInstantQueryRequest:
		values := url.Values{
			"time":  []string{encodeTime(r.GetTime())},
			"query": []string{r.GetQuery()},
		}
		if s := r.GetStats(); s != "" {
			values["stats"] = []string{s}
		}
		u = &url.URL{
			Path:     r.GetPath(),
			RawQuery: values.Encode(),
		}

	default:
		return nil, fmt.Errorf("unsupported request type %T", r)
	}

	req := &http.Request{
		Method:     "GET",
		RequestURI: u.String(), // This is what the httpgrpc code looks at.
		URL:        u,
		Body:       http.NoBody,
		Header:     http.Header{},
	}

	encodeOptions(req, r.GetOptions())

	switch c.preferredQueryResultResponseFormat {
	case formatJSON:
		req.Header.Set("Accept", jsonMimeType)
	case formatProtobuf:
		req.Header.Set("Accept", mimirpb.QueryResponseMimeType+","+jsonMimeType)
	default:
		return nil, fmt.Errorf("unknown query result response format '%s'", c.preferredQueryResultResponseFormat)
	}

	if level, ok := api.ReadConsistencyLevelFromContext(ctx); ok {
		req.Header.Add(api.ReadConsistencyHeader, level)
	}

	// Propagate allowed HTTP headers.
	for _, h := range r.GetHeaders() {
		if !slices.Contains(c.propagateHeadersMetrics, h.Name) {
			continue
		}

		for _, v := range h.Values {
			// There should only be one value, but add all of them for completeness.
			req.Header.Add(h.Name, v)
		}
	}

	// Inject auth from context.
	if err := user.InjectOrgIDIntoHTTPRequest(ctx, req); err != nil {
		return nil, err
	}

	return req.WithContext(ctx), nil
}

// EncodeLabelsSeriesQueryRequest encodes a LabelsSeriesQueryRequest into an http request.
func (c Codec) EncodeLabelsSeriesQueryRequest(ctx context.Context, req LabelsSeriesQueryRequest) (*http.Request, error) {
	var u *url.URL
	switch req := req.(type) {
	case *PrometheusLabelNamesQueryRequest:
		urlValues := url.Values{}
		if req.GetStart() != 0 {
			urlValues["start"] = []string{encodeTime(req.Start)}
		}
		if req.GetEnd() != 0 {
			urlValues["end"] = []string{encodeTime(req.End)}
		}
		if len(req.GetLabelMatcherSets()) > 0 {
			urlValues["match[]"] = req.GetLabelMatcherSets()
		}
		if req.GetLimit() > 0 {
			urlValues["limit"] = []string{strconv.FormatUint(req.GetLimit(), 10)}
		}
		u = &url.URL{
			Path:     req.Path,
			RawQuery: urlValues.Encode(),
		}
	case *PrometheusLabelValuesQueryRequest:
		// repeated from PrometheusLabelNamesQueryRequest case; Go type cast switch
		// does not support accessing struct members on a typeA|typeB switch
		urlValues := url.Values{}
		if req.GetStart() != 0 {
			urlValues["start"] = []string{encodeTime(req.Start)}
		}
		if req.GetEnd() != 0 {
			urlValues["end"] = []string{encodeTime(req.End)}
		}
		if len(req.GetLabelMatcherSets()) > 0 {
			urlValues["match[]"] = req.GetLabelMatcherSets()
		}
		if req.GetLimit() > 0 {
			urlValues["limit"] = []string{strconv.FormatUint(req.GetLimit(), 10)}
		}
		u = &url.URL{
			Path:     req.Path, // path still contains label name
			RawQuery: urlValues.Encode(),
		}
	case *PrometheusSeriesQueryRequest:
		urlValues := url.Values{}
		if req.GetStart() != 0 {
			urlValues["start"] = []string{encodeTime(req.Start)}
		}
		if req.GetEnd() != 0 {
			urlValues["end"] = []string{encodeTime(req.End)}
		}
		if len(req.GetLabelMatcherSets()) > 0 {
			urlValues["match[]"] = req.GetLabelMatcherSets()
		}
		if req.GetLimit() > 0 {
			urlValues["limit"] = []string{strconv.FormatUint(req.GetLimit(), 10)}
		}
		u = &url.URL{
			Path:     req.Path,
			RawQuery: urlValues.Encode(),
		}

	default:
		return nil, fmt.Errorf("unsupported request type %T", req)
	}

	r := &http.Request{
		Method:     "GET",
		RequestURI: u.String(), // This is what the httpgrpc code looks at.
		URL:        u,
		Body:       http.NoBody,
		Header:     http.Header{},
	}

	switch c.preferredQueryResultResponseFormat {
	case formatJSON:
		r.Header.Set("Accept", jsonMimeType)
	case formatProtobuf:
		r.Header.Set("Accept", mimirpb.QueryResponseMimeType+","+jsonMimeType)
	default:
		return nil, fmt.Errorf("unknown query result response format '%s'", c.preferredQueryResultResponseFormat)
	}

	if level, ok := api.ReadConsistencyLevelFromContext(ctx); ok {
		r.Header.Add(api.ReadConsistencyHeader, level)
	}

	// Propagate allowed HTTP headers.
	for _, h := range req.GetHeaders() {
		if !slices.Contains(c.propagateHeadersLabels, h.Name) {
			continue
		}

		for _, v := range h.Values {
			// There should only be one value, but add all of them for completeness.
			r.Header.Add(h.Name, v)
		}
	}

	// Inject auth from context.
	if err := user.InjectOrgIDIntoHTTPRequest(ctx, r); err != nil {
		return nil, err
	}

	return r.WithContext(ctx), nil
}

func encodeOptions(req *http.Request, o Options) {
	if o.CacheDisabled {
		req.Header.Set(cacheControlHeader, noStoreValue)
	}
	if o.ShardingDisabled {
		req.Header.Set(totalShardsControlHeader, "0")
	}
	if o.TotalShards > 0 {
		req.Header.Set(totalShardsControlHeader, strconv.Itoa(int(o.TotalShards)))
	}
}

// DecodeMetricsQueryResponse decodes a Response from an http response.
// The original request is also passed as a parameter this is useful for implementation that needs the request
// to merge result or build the result correctly.
func (c Codec) DecodeMetricsQueryResponse(ctx context.Context, r *http.Response, _ MetricsQueryRequest, logger log.Logger) (Response, error) {
	spanlog := spanlogger.FromContext(ctx, logger)
	buf, err := readResponseBody(r)
	if err != nil {
		return nil, spanlog.Error(err)
	}

	spanlog.LogKV(
		"message", "ParseQueryRangeResponse",
		"status_code", r.StatusCode,
		"bytes", len(buf),
	)

	// Before attempting to decode a response based on the content type, check if the
	// Content-Type header was even set. When the scheduler returns gRPC errors, they
	// are encoded as httpgrpc.HTTPResponse objects with an HTTP status code and the
	// error message as the body of the response with no content type. We need to handle
	// that case here before we decode well-formed success or error responses.
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		switch r.StatusCode {
		case http.StatusServiceUnavailable:
			return nil, apierror.New(apierror.TypeUnavailable, string(buf))
		case http.StatusTooManyRequests:
			return nil, apierror.New(apierror.TypeTooManyRequests, string(buf))
		case http.StatusRequestEntityTooLarge:
			return nil, apierror.New(apierror.TypeTooLargeEntry, string(buf))
		default:
			if r.StatusCode/100 == 5 {
				return nil, apierror.New(apierror.TypeInternal, string(buf))
			}
		}
	}

	formatter := findFormatter(contentType)
	if formatter == nil {
		return nil, apierror.Newf(apierror.TypeInternal, "unknown response content type '%v'", contentType)
	}

	start := time.Now()
	resp, err := formatter.DecodeQueryResponse(buf)
	if err != nil {
		return nil, apierror.Newf(apierror.TypeInternal, "error decoding response: %v", err)
	}

	c.metrics.duration.WithLabelValues(operationDecode, formatter.Name()).Observe(time.Since(start).Seconds())
	c.metrics.size.WithLabelValues(operationDecode, formatter.Name()).Observe(float64(len(buf)))

	if resp.Status == statusError {
		return nil, apierror.New(apierror.Type(resp.ErrorType), resp.Error)
	}

	for h, hv := range r.Header {
		resp.Headers = append(resp.Headers, &PrometheusHeader{Name: h, Values: hv})
	}
	return resp, nil
}

// DecodeLabelsSeriesQueryResponse decodes a Response from an http response.
// The original request is also passed as a parameter this is useful for implementation that needs the request
// to merge result or build the result correctly.
func (c Codec) DecodeLabelsSeriesQueryResponse(ctx context.Context, r *http.Response, lr LabelsSeriesQueryRequest, logger log.Logger) (Response, error) {
	spanlog := spanlogger.FromContext(ctx, logger)
	buf, err := readResponseBody(r)
	if err != nil {
		return nil, spanlog.Error(err)
	}

	spanlog.LogKV(
		"message", "ParseQueryRangeResponse",
		"status_code", r.StatusCode,
		"bytes", len(buf),
	)

	// Before attempting to decode a response based on the content type, check if the
	// Content-Type header was even set. When the scheduler returns gRPC errors, they
	// are encoded as httpgrpc.HTTPResponse objects with an HTTP status code and the
	// error message as the body of the response with no content type. We need to handle
	// that case here before we decode well-formed success or error responses.
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		switch r.StatusCode {
		case http.StatusServiceUnavailable:
			return nil, apierror.New(apierror.TypeUnavailable, string(buf))
		case http.StatusTooManyRequests:
			return nil, apierror.New(apierror.TypeTooManyRequests, string(buf))
		case http.StatusRequestEntityTooLarge:
			return nil, apierror.New(apierror.TypeTooLargeEntry, string(buf))
		default:
			if r.StatusCode/100 == 5 {
				return nil, apierror.New(apierror.TypeInternal, string(buf))
			}
		}
	}

	formatter := findFormatter(contentType)
	if formatter == nil {
		return nil, apierror.Newf(apierror.TypeInternal, "unknown response content type '%v'", contentType)
	}

	start := time.Now()

	var response Response

	switch lr.(type) {
	case *PrometheusLabelNamesQueryRequest, *PrometheusLabelValuesQueryRequest:
		resp, err := formatter.DecodeLabelsResponse(buf)
		if err != nil {
			return nil, apierror.Newf(apierror.TypeInternal, "error decoding response: %v", err)
		}

		c.metrics.duration.WithLabelValues(operationDecode, formatter.Name()).Observe(time.Since(start).Seconds())
		c.metrics.size.WithLabelValues(operationDecode, formatter.Name()).Observe(float64(len(buf)))

		if resp.Status == statusError {
			return nil, apierror.New(apierror.Type(resp.ErrorType), resp.Error)
		}

		for h, hv := range r.Header {
			resp.Headers = append(resp.Headers, &PrometheusHeader{Name: h, Values: hv})
		}

		response = resp
	case *PrometheusSeriesQueryRequest:
		resp, err := formatter.DecodeSeriesResponse(buf)
		if err != nil {
			return nil, apierror.Newf(apierror.TypeInternal, "error decoding response: %v", err)
		}

		c.metrics.duration.WithLabelValues(operationDecode, formatter.Name()).Observe(time.Since(start).Seconds())
		c.metrics.size.WithLabelValues(operationDecode, formatter.Name()).Observe(float64(len(buf)))

		if resp.Status == statusError {
			return nil, apierror.New(apierror.Type(resp.ErrorType), resp.Error)
		}

		for h, hv := range r.Header {
			resp.Headers = append(resp.Headers, &PrometheusHeader{Name: h, Values: hv})
		}

		response = resp
	default:
		return nil, apierror.Newf(apierror.TypeInternal, "unsupported request type %T", lr)
	}
	return response, nil
}

func findFormatter(contentType string) formatter {
	for _, f := range knownFormats {
		if f.ContentType().String() == contentType {
			return f
		}
	}

	return nil
}

// EncodeMetricsQueryResponse encodes a Response from a MetricsQueryRequest into an http response.
func (c Codec) EncodeMetricsQueryResponse(ctx context.Context, req *http.Request, res Response) (*http.Response, error) {
	_, sp := tracer.Start(ctx, "APIResponse.ToHTTPResponse")
	defer sp.End()

	a, ok := res.GetPrometheusResponse()
	if !ok {
		return nil, apierror.Newf(apierror.TypeInternal, "invalid response format")
	}
	if a.Data != nil {
		sp.SetAttributes(attribute.Int("series", len(a.Data.Result)))
	}

	selectedContentType, formatter := c.negotiateContentType(req.Header.Get("Accept"))
	if formatter == nil {
		return nil, apierror.New(apierror.TypeNotAcceptable, "none of the content types in the Accept header are supported")
	}

	start := time.Now()
	b, err := formatter.EncodeQueryResponse(a)
	if err != nil {
		return nil, apierror.Newf(apierror.TypeInternal, "error encoding response: %v", err)
	}

	encodeDuration := time.Since(start)
	c.metrics.duration.WithLabelValues(operationEncode, formatter.Name()).Observe(encodeDuration.Seconds())
	c.metrics.size.WithLabelValues(operationEncode, formatter.Name()).Observe(float64(len(b)))
	sp.SetAttributes(attribute.Int("bytes", len(b)))

	queryStats := stats.FromContext(ctx)
	queryStats.AddEncodeTime(encodeDuration)

	resp := http.Response{
		Header: http.Header{
			"Content-Type": []string{selectedContentType},
		},
		Body: &prometheusReadCloser{
			Reader:    bytes.NewBuffer(b),
			finalizer: res.Close,
		},
		StatusCode:    http.StatusOK,
		ContentLength: int64(len(b)),
	}
	return &resp, nil
}

// prometheusReadCloser wraps an io.Reader and executes finalizer on Close
type prometheusReadCloser struct {
	io.Reader
	finalizer func()
}

func (prc *prometheusReadCloser) Close() error {
	if prc.finalizer != nil {
		prc.finalizer()
	}
	return nil
}

// EncodeLabelsSeriesQueryResponse encodes a Response from a LabelsSeriesQueryRequest into an http response.
func (c Codec) EncodeLabelsSeriesQueryResponse(ctx context.Context, req *http.Request, res Response, isSeriesResponse bool) (*http.Response, error) {
	_, sp := tracer.Start(ctx, "APIResponse.ToHTTPResponse")
	defer sp.End()

	selectedContentType, formatter := c.negotiateContentType(req.Header.Get("Accept"))
	if formatter == nil {
		return nil, apierror.New(apierror.TypeNotAcceptable, "none of the content types in the Accept header are supported")
	}

	var start time.Time
	var b []byte

	switch isSeriesResponse {
	case false:
		a, ok := res.(*PrometheusLabelsResponse)
		if !ok {
			return nil, apierror.Newf(apierror.TypeInternal, "invalid response format")
		}
		if a.Data != nil {
			sp.SetAttributes(attribute.Int("labels", len(a.Data)))
		}

		start = time.Now()
		var err error
		b, err = formatter.EncodeLabelsResponse(a)
		if err != nil {
			return nil, apierror.Newf(apierror.TypeInternal, "error encoding response: %v", err)
		}
	case true:
		a, ok := res.(*PrometheusSeriesResponse)
		if !ok {
			return nil, apierror.Newf(apierror.TypeInternal, "invalid response format")
		}
		if a.Data != nil {
			sp.SetAttributes(attribute.Int("labels", len(a.Data)))
		}

		start = time.Now()
		var err error
		b, err = formatter.EncodeSeriesResponse(a)
		if err != nil {
			return nil, apierror.Newf(apierror.TypeInternal, "error encoding response: %v", err)
		}
	}

	c.metrics.duration.WithLabelValues(operationEncode, formatter.Name()).Observe(time.Since(start).Seconds())
	c.metrics.size.WithLabelValues(operationEncode, formatter.Name()).Observe(float64(len(b)))
	sp.SetAttributes(attribute.Int("bytes", len(b)))

	resp := http.Response{
		Header: http.Header{
			"Content-Type": []string{selectedContentType},
		},
		Body:          io.NopCloser(bytes.NewBuffer(b)),
		StatusCode:    http.StatusOK,
		ContentLength: int64(len(b)),
	}
	return &resp, nil
}

func (Codec) negotiateContentType(acceptHeader string) (string, formatter) {
	if acceptHeader == "" {
		return jsonMimeType, jsonFormatterInstance
	}

	for _, clause := range goautoneg.ParseAccept(acceptHeader) {
		for _, formatter := range knownFormats {
			if formatter.ContentType().Satisfies(clause) {
				return formatter.ContentType().String(), formatter
			}
		}
	}

	return "", nil
}

func matrixMerge(resps []*PrometheusResponse) []SampleStream {
	output := map[string]*SampleStream{}
	for _, resp := range resps {
		if resp.Data == nil {
			continue
		}
		for _, stream := range resp.Data.Result {
			metric := mimirpb.FromLabelAdaptersToKeyString(stream.Labels)
			existing, ok := output[metric]
			if !ok {
				existing = &SampleStream{
					Labels: stream.Labels,
				}
			}
			// We need to make sure we don't repeat samples. This causes some visualisations to be broken in Grafana.
			// The prometheus API is inclusive of start and end timestamps.
			if len(existing.Samples) > 0 && len(stream.Samples) > 0 {
				existingEndTs := existing.Samples[len(existing.Samples)-1].TimestampMs
				if existingEndTs == stream.Samples[0].TimestampMs {
					// Typically this the cases where only 1 sample point overlap,
					// so optimize with simple code.
					stream.Samples = stream.Samples[1:]
				} else if existingEndTs > stream.Samples[0].TimestampMs {
					// Overlap might be big, use heavier algorithm to remove overlap.
					stream.Samples = sliceFloatSamples(stream.Samples, existingEndTs)
				} // else there is no overlap, yay!
			}
			existing.Samples = append(existing.Samples, stream.Samples...)

			if len(existing.Histograms) > 0 && len(stream.Histograms) > 0 {
				existingEndTs := existing.Histograms[len(existing.Histograms)-1].TimestampMs
				if existingEndTs == stream.Histograms[0].TimestampMs {
					// Typically this the cases where only 1 sample point overlap,
					// so optimize with simple code.
					stream.Histograms = stream.Histograms[1:]
				} else if existingEndTs > stream.Histograms[0].TimestampMs {
					// Overlap might be big, use heavier algorithm to remove overlap.
					stream.Histograms = sliceHistogramSamples(stream.Histograms, existingEndTs)
				} // else there is no overlap, yay!
			}
			existing.Histograms = append(existing.Histograms, stream.Histograms...)

			output[metric] = existing
		}
	}

	keys := make([]string, 0, len(output))
	for key := range output {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	result := make([]SampleStream, 0, len(output))
	for _, key := range keys {
		result = append(result, *output[key])
	}

	return result
}

// sliceFloatSamples assumes given samples are sorted by timestamp in ascending order and
// return a sub slice whose first element's is the smallest timestamp that is strictly
// bigger than the given minTs. Empty slice is returned if minTs is bigger than all the
// timestamps in samples
func sliceFloatSamples(samples []mimirpb.Sample, minTs int64) []mimirpb.Sample {
	if len(samples) <= 0 || minTs < samples[0].TimestampMs {
		return samples
	}

	if len(samples) > 0 && minTs > samples[len(samples)-1].TimestampMs {
		return samples[len(samples):]
	}

	searchResult := sort.Search(len(samples), func(i int) bool {
		return samples[i].TimestampMs > minTs
	})

	return samples[searchResult:]
}

// sliceHistogramSamples assumes given samples are sorted by timestamp in ascending order and
// return a sub slice whose first element's is the smallest timestamp that is strictly
// bigger than the given minTs. Empty slice is returned if minTs is bigger than all the
// timestamps in samples
func sliceHistogramSamples(samples []mimirpb.FloatHistogramPair, minTs int64) []mimirpb.FloatHistogramPair {
	if len(samples) <= 0 || minTs < samples[0].TimestampMs {
		return samples
	}

	if len(samples) > 0 && minTs > samples[len(samples)-1].TimestampMs {
		return samples[len(samples):]
	}

	searchResult := sort.Search(len(samples), func(i int) bool {
		return samples[i].TimestampMs > minTs
	})

	return samples[searchResult:]
}

func readResponseBody(res *http.Response) ([]byte, error) {
	// Ensure we close the response Body once we've consumed it, as required by http.Response
	// specifications.
	defer res.Body.Close() // nolint:errcheck

	// Attempt to cast the response body to a Buffer and use it if possible.
	// This is because the frontend may have already read the body and buffered it.
	if buffer, ok := res.Body.(interface{ Bytes() []byte }); ok {
		return buffer.Bytes(), nil
	}
	// Preallocate the buffer with the exact size so we don't waste allocations
	// while progressively growing an initial small buffer. The buffer capacity
	// is increased by MinRead to avoid extra allocations due to how ReadFrom()
	// internally works.
	buf := bytes.NewBuffer(make([]byte, 0, res.ContentLength+bytes.MinRead))
	if _, err := buf.ReadFrom(res.Body); err != nil {
		return nil, apierror.Newf(apierror.TypeInternal, "error decoding response with status %d: %v", res.StatusCode, err)
	}
	return buf.Bytes(), nil
}

func encodeTime(t int64) string {
	f := float64(t) / 1.0e3
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func encodeDurationMs(d int64) string {
	return strconv.FormatFloat(float64(d)/float64(time.Second/time.Millisecond), 'f', -1, 64)
}

func DecorateWithParamName(err error, field string) error {
	errTmpl := "invalid parameter %q: %v"
	if status, ok := grpcutil.ErrorToStatus(err); ok {
		return apierror.Newf(apierror.TypeBadData, errTmpl, field, status.Message())
	}
	return apierror.Newf(apierror.TypeBadData, errTmpl, field, err)
}
