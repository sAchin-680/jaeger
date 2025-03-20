// Copyright (c) 2021 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package adaptive

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	epmocks "github.com/jaegertracing/jaeger/internal/leaderelection/mocks"
	"github.com/jaegertracing/jaeger/internal/metricstest"
	"github.com/jaegertracing/jaeger/internal/storage/v1/api/samplingstore/mocks"
)

func TestAggregator(t *testing.T) {
	t.Skip("Skipping flaky unit test")
	metricsFactory := metricstest.NewFactory(0)

	mockStorage := &mocks.Store{}
	mockStorage.On("InsertThroughput", mock.AnythingOfType("[]*model.Throughput")).Return(nil)
	mockEP := &epmocks.ElectionParticipant{}
	mockEP.On("Start").Return(nil)
	mockEP.On("Close").Return(nil)
	mockEP.On("IsLeader").Return(true)
	testOpts := Options{
		CalculationInterval:   1 * time.Second,
		AggregationBuckets:    1,
		BucketsForCalculation: 1,
	}
	logger := zap.NewNop()

	a, err := NewAggregator(testOpts, logger, metricsFactory, mockEP, mockStorage)
	require.NoError(t, err)
	a.RecordThroughput("A", http.MethodGet, model.SamplerTypeProbabilistic, 0.001)
	a.RecordThroughput("B", http.MethodPost, model.SamplerTypeProbabilistic, 0.001)
	a.RecordThroughput("C", http.MethodGet, model.SamplerTypeProbabilistic, 0.001)
	a.RecordThroughput("A", http.MethodPost, model.SamplerTypeProbabilistic, 0.001)
	a.RecordThroughput("A", http.MethodGet, model.SamplerTypeProbabilistic, 0.001)
	a.RecordThroughput("A", http.MethodGet, model.SamplerTypeLowerBound, 0.001)

	a.Start()
	defer a.Close()
	for i := 0; i < 10000; i++ {
		counters, _ := metricsFactory.Snapshot()
		if _, ok := counters["sampling_operations"]; ok {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}

	metricsFactory.AssertCounterMetrics(t, []metricstest.ExpectedMetric{
		{Name: "sampling_operations", Value: 4},
		{Name: "sampling_services", Value: 3},
	}...)
}

func TestIncrementThroughput(t *testing.T) {
	metricsFactory := metricstest.NewFactory(0)
	mockStorage := &mocks.Store{}
	mockEP := &epmocks.ElectionParticipant{}
	testOpts := Options{
		CalculationInterval:   1 * time.Second,
		AggregationBuckets:    1,
		BucketsForCalculation: 1,
	}
	logger := zap.NewNop()
	a, err := NewAggregator(testOpts, logger, metricsFactory, mockEP, mockStorage)
	require.NoError(t, err)
	// 20 different probabilities
	for i := 0; i < 20; i++ {
		a.RecordThroughput("A", http.MethodGet, model.SamplerTypeProbabilistic, 0.001*float64(i))
	}
	assert.Len(t, a.(*aggregator).currentThroughput["A"][http.MethodGet].Probabilities, 10)

	a, err = NewAggregator(testOpts, logger, metricsFactory, mockEP, mockStorage)
	require.NoError(t, err)
	// 20 of the same probabilities
	for i := 0; i < 20; i++ {
		a.RecordThroughput("A", http.MethodGet, model.SamplerTypeProbabilistic, 0.001)
	}
	assert.Len(t, a.(*aggregator).currentThroughput["A"][http.MethodGet].Probabilities, 1)
}

func TestLowerboundThroughput(t *testing.T) {
	metricsFactory := metricstest.NewFactory(0)
	mockStorage := &mocks.Store{}
	mockEP := &epmocks.ElectionParticipant{}
	testOpts := Options{
		CalculationInterval:   1 * time.Second,
		AggregationBuckets:    1,
		BucketsForCalculation: 1,
	}
	logger := zap.NewNop()

	a, err := NewAggregator(testOpts, logger, metricsFactory, mockEP, mockStorage)
	require.NoError(t, err)
	a.RecordThroughput("A", http.MethodGet, model.SamplerTypeLowerBound, 0.001)
	assert.EqualValues(t, 0, a.(*aggregator).currentThroughput["A"][http.MethodGet].Count)
	assert.Empty(t, a.(*aggregator).currentThroughput["A"][http.MethodGet].Probabilities["0.001000"])
}

func TestRecordThroughput(t *testing.T) {
	metricsFactory := metricstest.NewFactory(0)
	mockStorage := &mocks.Store{}
	mockEP := &epmocks.ElectionParticipant{}
	testOpts := Options{
		CalculationInterval:   1 * time.Second,
		AggregationBuckets:    1,
		BucketsForCalculation: 1,
	}
	logger := zap.NewNop()
	a, err := NewAggregator(testOpts, logger, metricsFactory, mockEP, mockStorage)
	require.NoError(t, err)

	// Testing non-root span
	span := &model.Span{References: []model.SpanRef{{SpanID: model.NewSpanID(1), RefType: model.ChildOf}}}
	a.HandleRootSpan(span)
	require.Empty(t, a.(*aggregator).currentThroughput)

	// Testing span with service name but no operation
	span.References = []model.SpanRef{}
	span.Process = &model.Process{
		ServiceName: "A",
	}
	a.HandleRootSpan(span)
	require.Empty(t, a.(*aggregator).currentThroughput)

	// Testing span with service name and operation but no probabilistic sampling tags
	span.OperationName = http.MethodGet
	a.HandleRootSpan(span)
	require.Empty(t, a.(*aggregator).currentThroughput)

	// Testing span with service name, operation, and probabilistic sampling tags
	span.Tags = model.KeyValues{
		model.String("sampler.type", "probabilistic"),
		model.String("sampler.param", "0.001"),
	}
	a.HandleRootSpan(span)
	assert.EqualValues(t, 1, a.(*aggregator).currentThroughput["A"][http.MethodGet].Count)
}

func TestRecordThroughputFunc(t *testing.T) {
	metricsFactory := metricstest.NewFactory(0)
	mockStorage := &mocks.Store{}
	mockEP := &epmocks.ElectionParticipant{}
	logger := zap.NewNop()
	testOpts := Options{
		CalculationInterval:   1 * time.Second,
		AggregationBuckets:    1,
		BucketsForCalculation: 1,
	}

	a, err := NewAggregator(testOpts, logger, metricsFactory, mockEP, mockStorage)
	require.NoError(t, err)

	// Testing non-root span
	span := &model.Span{References: []model.SpanRef{{SpanID: model.NewSpanID(1), RefType: model.ChildOf}}}
	a.HandleRootSpan(span)
	require.Empty(t, a.(*aggregator).currentThroughput)

	// Testing span with service name but no operation
	span.References = []model.SpanRef{}
	span.Process = &model.Process{
		ServiceName: "A",
	}
	a.HandleRootSpan(span)
	require.Empty(t, a.(*aggregator).currentThroughput)

	// Testing span with service name and operation but no probabilistic sampling tags
	span.OperationName = http.MethodGet
	a.HandleRootSpan(span)
	require.Empty(t, a.(*aggregator).currentThroughput)

	// Testing span with service name, operation, and probabilistic sampling tags
	span.Tags = model.KeyValues{
		model.String("sampler.type", "probabilistic"),
		model.String("sampler.param", "0.001"),
	}
	a.HandleRootSpan(span)
	assert.EqualValues(t, 1, a.(*aggregator).currentThroughput["A"][http.MethodGet].Count)
}

func TestGetSamplerParams(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name          string
		tags          model.KeyValues
		enableFlag    bool
		expectedType  model.SamplerType
		expectedParam float64
	}{
		{
			name: "Valid probabilistic sampler with string param",
			tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
				model.String("sampler.param", "1e-05"),
			},
			expectedType:  model.SamplerTypeProbabilistic,
			expectedParam: 0.00001,
		},
		{
			name: "Valid probabilistic sampler with float64 param",
			tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
				model.Float64("sampler.param", 0.10404450002098709),
			},
			expectedType:  model.SamplerTypeProbabilistic,
			expectedParam: 0.10404450002098709,
		},
		{
			name: "Valid probabilistic sampler with stringified float param",
			tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
				model.String("sampler.param", "0.10404450002098709"),
			},
			expectedType:  model.SamplerTypeProbabilistic,
			expectedParam: 0.10404450002098709,
		},
		{
			name: "Probabilistic sampler with int64 param",
			tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
				model.Int64("sampler.param", 1),
			},
			expectedType:  model.SamplerTypeProbabilistic,
			expectedParam: 1.0,
		},
		{
			name: "Valid rate limiting sampler",
			tags: model.KeyValues{
				model.String("sampler.type", "ratelimiting"),
				model.String("sampler.param", "1"),
			},
			expectedType:  model.SamplerTypeRateLimiting,
			expectedParam: 1,
		},
		{
			name: "Invalid sampler.type (float instead of string)",
			tags: model.KeyValues{
				model.Float64("sampler.type", 1.5),
			},
			expectedType:  model.SamplerTypeUnrecognized,
			expectedParam: 0,
		},
		{
			name: "Probabilistic sampler missing param",
			tags: model.KeyValues{
				model.String("sampler.type", "probabilistic"),
			},
			expectedType:  model.SamplerTypeUnrecognized,
			expectedParam: 0,
		},
		{
			name: "No tags, feature flag disabled",
			tags: model.KeyValues{},
			enableFlag: false,
			expectedType:  model.SamplerTypeUnrecognized,
			expectedParam: 0,
		},
		{
			name: "No tags, feature flag enabled",
			tags: model.KeyValues{},
			enableFlag: true,
			expectedType:  model.SamplerTypeProbabilistic,
			expectedParam: 1.0,
		},
		{
			name: "Lowerbound sampler with string param",
			tags: model.KeyValues{
				model.String("sampler.type", "lowerbound"),
				model.String("sampler.param", "1"),
			},
			expectedType:  model.SamplerTypeLowerBound,
			expectedParam: 1,
		},
		{
			name: "Lowerbound sampler with int64 param",
			tags: model.KeyValues{
				model.String("sampler.type", "lowerbound"),
				model.Int64("sampler.param", 1),
			},
			expectedType:  model.SamplerTypeLowerBound,
			expectedParam: 1,
		},
		{
			name: "Lowerbound sampler with float64 param",
			tags: model.KeyValues{
				model.String("sampler.type", "lowerbound"),
				model.Float64("sampler.param", 0.5),
			},
			expectedType:  model.SamplerTypeLowerBound,
			expectedParam: 0.5,
		},
		{
			name: "Invalid sampler.param (not a number) for lowerbound",
			tags: model.KeyValues{
				model.String("sampler.type", "lowerbound"),
				model.String("sampler.param", "not_a_number"),
			},
			expectedType:  model.SamplerTypeUnrecognized,
			expectedParam: 0,
		},
		{
			name: "Invalid sampler.type and invalid sampler.param",
			tags: model.KeyValues{
				model.String("sampler.type", "not_a_type"),
				model.String("sampler.param", "not_a_number"),
			},
			expectedType:  model.SamplerTypeUnrecognized,
			expectedParam: 0,
		},
	}

	for i, test := range tests {
		tt := test
		t.Run(strconv.Itoa(i)+"_"+tt.name, func(t *testing.T) {
			// Set the feature flag for this test case
			EnableAdaptiveSamplingWithoutTags = tt.enableFlag

			span := &model.Span{Tags: tt.tags}
			actualType, actualParam := getSamplerParams(span, logger)

			assert.Equal(t, tt.expectedType, actualType, "Test failed for case: %s", tt.name)
			assert.InDelta(t, tt.expectedParam, actualParam, 0.01, "Test failed for case: %s", tt.name)
		})
	}
}
