// Code generated from Prometheus sources - DO NOT EDIT.

// Copyright 2024 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// Provenance-includes-location: https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/95e8f8fdc2a9dc87230406c9a3cf02be4fd68bea/pkg/translator/prometheusremotewrite/number_data_points.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: Copyright The OpenTelemetry Authors.

package otlp

import (
	"context"
	"log/slog"
	"math"

	"github.com/prometheus/common/model"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/grafana/mimir/pkg/mimirpb"

	"github.com/prometheus/prometheus/model/value"
)

func (c *MimirConverter) addGaugeNumberDataPoints(ctx context.Context, dataPoints pmetric.NumberDataPointSlice,
	resource pcommon.Resource, settings Settings, metadata mimirpb.MetricMetadata, scope scope,
) error {
	for x := 0; x < dataPoints.Len(); x++ {
		if err := c.everyN.checkContext(ctx); err != nil {
			return err
		}

		pt := dataPoints.At(x)
		labels := createAttributes(
			resource,
			pt.Attributes(),
			scope,
			settings,
			nil,
			true,
			metadata,
			model.MetricNameLabel,
			metadata.MetricFamilyName,
		)
		sample := &mimirpb.Sample{
			// convert ns to ms
			TimestampMs: convertTimeStamp(pt.Timestamp()),
		}
		switch pt.ValueType() {
		case pmetric.NumberDataPointValueTypeInt:
			sample.Value = float64(pt.IntValue())
		case pmetric.NumberDataPointValueTypeDouble:
			sample.Value = pt.DoubleValue()
		}
		if pt.Flags().NoRecordedValue() {
			sample.Value = math.Float64frombits(value.StaleNaN)
		}

		c.addSample(sample, labels)
	}

	return nil
}

func (c *MimirConverter) addSumNumberDataPoints(ctx context.Context, dataPoints pmetric.NumberDataPointSlice,
	resource pcommon.Resource, metric pmetric.Metric, settings Settings, metadata mimirpb.MetricMetadata, scope scope, logger *slog.Logger,
) error {
	for x := 0; x < dataPoints.Len(); x++ {
		if err := c.everyN.checkContext(ctx); err != nil {
			return err
		}

		pt := dataPoints.At(x)
		timestamp := convertTimeStamp(pt.Timestamp())
		startTimestampMs := convertTimeStamp(pt.StartTimestamp())
		lbls := createAttributes(
			resource,
			pt.Attributes(),
			scope,
			settings,
			nil,
			true,
			metadata,
			model.MetricNameLabel,
			metadata.MetricFamilyName,
		)
		sample := &mimirpb.Sample{
			// convert ns to ms
			TimestampMs: timestamp,
		}
		switch pt.ValueType() {
		case pmetric.NumberDataPointValueTypeInt:
			sample.Value = float64(pt.IntValue())
		case pmetric.NumberDataPointValueTypeDouble:
			sample.Value = pt.DoubleValue()
		}
		if pt.Flags().NoRecordedValue() {
			sample.Value = math.Float64frombits(value.StaleNaN)
		}

		isMonotonic := metric.Sum().IsMonotonic()
		if isMonotonic {
			c.handleStartTime(startTimestampMs, timestamp, lbls, settings, "sum", sample.Value, logger)
		}

		ts := c.addSample(sample, lbls)
		if ts != nil {
			exemplars, err := getPromExemplars[pmetric.NumberDataPoint](ctx, &c.everyN, pt)
			if err != nil {
				return err
			}
			ts.Exemplars = append(ts.Exemplars, exemplars...)
		}

		// add created time series if needed
		if settings.ExportCreatedMetric && isMonotonic {
			if startTimestampMs == 0 {
				return nil
			}

			createdLabels := make([]mimirpb.LabelAdapter, len(lbls))
			copy(createdLabels, lbls)
			for i, l := range createdLabels {
				if l.Name == model.MetricNameLabel {
					createdLabels[i].Value = metadata.MetricFamilyName + createdSuffix
					break
				}
			}
			c.addTimeSeriesIfNeeded(createdLabels, startTimestampMs, pt.Timestamp())
		}
		logger.Debug("addSumNumberDataPoints", "labels", labelsStringer(lbls), "start_ts", startTimestampMs, "sample_ts", timestamp, "type", "sum")
	}

	return nil
}
