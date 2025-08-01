// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/error_translate_queryable_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querier

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grafana/dskit/httpgrpc"
	"github.com/grafana/dskit/user"
	"github.com/grafana/regexp"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/route"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	v1 "github.com/prometheus/prometheus/web/api/v1"
	"github.com/stretchr/testify/require"

	"github.com/grafana/mimir/pkg/util/validation"
)

func TestApiStatusCodes(t *testing.T) {
	for ix, tc := range []struct {
		err            error
		expectedString string
		expectedCode   int
	}{
		{
			err:            errors.New("some random error"),
			expectedString: "some random error",
			expectedCode:   500,
		},

		{
			err:            validation.NewLimitError("limit exceeded"),
			expectedString: "limit exceeded",
			expectedCode:   422,
		},

		{
			err:            promql.ErrTooManySamples("query execution"),
			expectedString: "too many samples",
			expectedCode:   422,
		},

		{
			err:            promql.ErrQueryCanceled("query execution"),
			expectedString: "query was canceled",
			expectedCode:   499,
		},

		{
			err:            promql.ErrQueryTimeout("query execution"),
			expectedString: "query timed out",
			expectedCode:   503,
		},

		// Status code 400 is remapped to 422 (only choice we have)
		{
			err:            httpgrpc.Errorf(http.StatusBadRequest, "test string"),
			expectedString: "test string",
			expectedCode:   422,
		},

		// 404 is also translated to 422
		{
			err:            httpgrpc.Errorf(http.StatusNotFound, "not found"),
			expectedString: "not found",
			expectedCode:   422,
		},

		// 505 is translated to 500
		{
			err:            httpgrpc.Errorf(http.StatusHTTPVersionNotSupported, "test"),
			expectedString: "test",
			expectedCode:   500,
		},

		{
			err:            context.DeadlineExceeded,
			expectedString: "context deadline exceeded",
			expectedCode:   500,
		},

		{
			err:            context.Canceled,
			expectedString: "context canceled",
			expectedCode:   499,
		},
		// Status code 400 is remapped to 422 (only choice we have)
		{
			err:            errors.Wrap(httpgrpc.Errorf(http.StatusBadRequest, "test string"), "wrapped error"),
			expectedString: "test string",
			expectedCode:   422,
		},

		{
			err:            httpgrpc.Errorf(http.StatusServiceUnavailable, "test"),
			expectedString: "timeout",
			expectedCode:   http.StatusServiceUnavailable,
		},
	} {
		for k, q := range map[string]storage.SampleAndChunkQueryable{
			"error from queryable": errorTestQueryable{err: tc.err},
			"error from querier":   errorTestQueryable{q: errorTestQuerier{err: tc.err}},
			"error from seriesset": errorTestQueryable{q: errorTestQuerier{s: errorTestSeriesSet{err: tc.err}}},
		} {
			t.Run(fmt.Sprintf("%s/%d", k, ix), func(t *testing.T) {
				r := createPrometheusAPI(NewErrorTranslateSampleAndChunkQueryable(q))
				rec := httptest.NewRecorder()

				req := httptest.NewRequest("GET", "/api/v1/query?query=up", nil)
				req = req.WithContext(user.InjectOrgID(context.Background(), "test org"))

				r.ServeHTTP(rec, req)

				require.Equal(t, tc.expectedCode, rec.Code)
				require.Contains(t, rec.Body.String(), tc.expectedString)
			})
		}
	}
}

func createPrometheusAPI(q storage.SampleAndChunkQueryable) *route.Router {
	engine := promql.NewEngine(promql.EngineOpts{
		Logger:             promslog.NewNopLogger(),
		Reg:                nil,
		ActiveQueryTracker: nil,
		MaxSamples:         100,
		Timeout:            5 * time.Second,
	})

	api := v1.NewAPI(
		engine,
		q,
		nil,
		nil,
		func(context.Context) v1.ScrapePoolsRetriever { return &DummyTargetRetriever{} },
		func(context.Context) v1.TargetRetriever { return &DummyTargetRetriever{} },
		func(context.Context) v1.AlertmanagerRetriever { return &DummyAlertmanagerRetriever{} },
		func() config.Config { return config.Config{} },
		map[string]string{}, // TODO: include configuration flags
		v1.GlobalURLOptions{},
		func(f http.HandlerFunc) http.HandlerFunc { return f },
		nil,   // Only needed for admin APIs.
		"",    // This is for snapshots, which is disabled when admin APIs are disabled. Hence empty.
		false, // Disable admin APIs.
		promslog.NewNopLogger(),
		func(context.Context) v1.RulesRetriever { return &DummyRulesRetriever{} },
		0, 0, 0, // Remote read samples and concurrency limit.
		false, // Not an agent.
		regexp.MustCompile(".*"),
		func() (v1.RuntimeInfo, error) { return v1.RuntimeInfo{}, errors.New("not implemented") },
		&v1.PrometheusVersion{},
		nil,
		nil,
		prometheus.DefaultGatherer,
		nil,
		nil,
		false,
		nil,
		false,
		false,
		false,
		false,
		0,
		5*time.Minute,
		false,
	)

	promRouter := route.New().WithPrefix("/api/v1")
	api.Register(promRouter)

	return promRouter
}

type errorTestQueryable struct {
	q   storage.Querier
	err error
}

func (t errorTestQueryable) ChunkQuerier(int64, int64) (storage.ChunkQuerier, error) {
	return nil, t.err
}

func (t errorTestQueryable) Querier(int64, int64) (storage.Querier, error) {
	if t.q != nil {
		return t.q, nil
	}
	return nil, t.err
}

type errorTestQuerier struct {
	s   storage.SeriesSet
	err error
}

func (t errorTestQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, t.err
}

func (t errorTestQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, t.err
}

func (t errorTestQuerier) Close() error {
	return nil
}

func (t errorTestQuerier) Select(context.Context, bool, *storage.SelectHints, ...*labels.Matcher) storage.SeriesSet {
	if t.s != nil {
		return t.s
	}
	return storage.ErrSeriesSet(t.err)
}

type errorTestSeriesSet struct {
	err error
}

func (t errorTestSeriesSet) Next() bool {
	return false
}

func (t errorTestSeriesSet) At() storage.Series {
	return nil
}

func (t errorTestSeriesSet) Err() error {
	return t.err
}

func (t errorTestSeriesSet) Warnings() annotations.Annotations {
	return nil
}
