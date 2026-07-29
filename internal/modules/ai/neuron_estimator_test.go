package ai

import (
	"bytes"
	"io"
	"math"
	"reflect"
	"testing"
)

func TestNeuronEstimatorUsesOfficialRateAndActualTokens(t *testing.T) {
	t.Parallel()
	estimator := NewNeuronEstimator()
	measurement := estimator.Measure("@cf/meta/llama-3.2-1b-instruct", TokenUsage{
		Input: 1_000_000, Output: 1_000_000, Exact: true,
	}, true)
	if measurement.Neurons != 20_709 || measurement.Source != "official_rate_actual_tokens" {
		t.Fatalf("measurement = %#v", measurement)
	}

	estimated := estimator.Measure("@cf/meta/llama-3.2-1b-instruct", TokenUsage{Input: 100, Output: 50}, true)
	if estimated.Source != "official_rate_estimated_tokens" || estimated.Neurons <= 0 {
		t.Fatalf("estimated = %#v", estimated)
	}
	failed := estimator.Measure("@cf/meta/llama-3.2-1b-instruct", TokenUsage{Input: 100}, false)
	if failed.Neurons != 0 || failed.Source != "failed_without_usage" {
		t.Fatalf("failed = %#v", failed)
	}
}

func TestNeuronEstimatorUpdatesRateFromCatalog(t *testing.T) {
	t.Parallel()
	estimator := NewNeuronEstimator()
	estimator.UpdateCatalog([]map[string]any{{
		"name": "@cf/vendor/catalog-priced",
		"properties": []any{map[string]any{
			"property_id": "price",
			"value": map[string]any{
				"input_neurons_per_million_tokens":  10.0,
				"output_neurons_per_million_tokens": 20.0,
			},
		}},
	}})
	measurement := estimator.Measure("@cf/vendor/catalog-priced", TokenUsage{Input: 1_000_000, Output: 1_000_000, Exact: true}, true)
	if measurement.Neurons != 30 || measurement.Source != "official_rate_actual_tokens" {
		t.Fatalf("measurement = %#v", measurement)
	}
}

func TestExtractTokenUsage(t *testing.T) {
	t.Parallel()
	usage, ok := ExtractTokenUsage([]byte(`{"result":{"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}}`))
	if !ok || !reflect.DeepEqual(usage, TokenUsage{Input: 12, Output: 7, Exact: true}) {
		t.Fatalf("usage = %#v, ok = %v", usage, ok)
	}
}

func TestUsageCaptureReaderPreservesFragmentedSSE(t *testing.T) {
	t.Parallel()
	input := []byte("data: {\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n")
	source := &fragmentReader{data: input, size: 3}
	reader := newUsageCaptureReader(source)
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, input) {
		t.Fatalf("output changed: %q", output)
	}
	usage, ok := reader.Usage()
	if !ok || usage.Input != 9 || usage.Output != 4 || !usage.Exact {
		t.Fatalf("usage = %#v, ok = %v", usage, ok)
	}
}

func TestUnknownModelUsesNamedFallback(t *testing.T) {
	t.Parallel()
	measurement := NewNeuronEstimator().Measure("@cf/vendor/unknown", TokenUsage{Input: 10, Output: 10}, true)
	if measurement.Source != "fallback_text" || math.IsNaN(measurement.Neurons) || measurement.Neurons <= 0 {
		t.Fatalf("measurement = %#v", measurement)
	}
}

type fragmentReader struct {
	data []byte
	size int
}

func (r *fragmentReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	count := r.size
	if count > len(r.data) {
		count = len(r.data)
	}
	if count > len(buffer) {
		count = len(buffer)
	}
	copy(buffer, r.data[:count])
	r.data = r.data[count:]
	return count, nil
}
