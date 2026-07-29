package ai

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

type TokenUsage struct {
	Input  int64
	Output int64
	Exact  bool
}

type UsageMeasurement struct {
	InputTokens  int64
	OutputTokens int64
	Neurons      float64
	Source       string
}

type NeuronRate struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

type NeuronEstimator struct {
	mu    sync.RWMutex
	rates map[string]NeuronRate
}

func NewNeuronEstimator() *NeuronEstimator {
	rates := make(map[string]NeuronRate, len(bundledNeuronRates))
	for model, rate := range bundledNeuronRates {
		rates[model] = rate
	}
	return &NeuronEstimator{rates: rates}
}

func (e *NeuronEstimator) UpdateCatalog(models []map[string]any) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, model := range models {
		id := catalogModelID(model)
		if id == "" {
			continue
		}
		if rate, ok := catalogNeuronRate(model); ok {
			e.rates[id] = rate
		}
	}
}

func (e *NeuronEstimator) Measure(model string, usage TokenUsage, success bool) UsageMeasurement {
	measurement := UsageMeasurement{InputTokens: usage.Input, OutputTokens: usage.Output}
	if !success && !usage.Exact {
		measurement.Source = "failed_without_usage"
		return measurement
	}
	rate, known := e.rate(model)
	if known {
		measurement.Neurons = float64(usage.Input)*rate.InputPerMillion/1_000_000 +
			float64(usage.Output)*rate.OutputPerMillion/1_000_000
		if usage.Exact {
			measurement.Source = "official_rate_actual_tokens"
		} else {
			measurement.Source = "official_rate_estimated_tokens"
		}
		return measurement
	}

	rate, source := fallbackNeuronRate(model)
	measurement.Neurons = float64(usage.Input)*rate.InputPerMillion/1_000_000 +
		float64(usage.Output)*rate.OutputPerMillion/1_000_000
	measurement.Source = source
	return measurement
}

func (e *NeuronEstimator) rate(model string) (NeuronRate, bool) {
	if e == nil {
		return NeuronRate{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	rate, ok := e.rates[strings.TrimSpace(model)]
	return rate, ok
}

func ExtractTokenUsage(body []byte) (TokenUsage, bool) {
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return TokenUsage{}, false
	}
	return tokenUsageFromValue(payload)
}

type usageCaptureReader struct {
	source  io.Reader
	capture *streamUsageCapture
}

func newUsageCaptureReader(source io.Reader) *usageCaptureReader {
	return &usageCaptureReader{source: source, capture: &streamUsageCapture{}}
}

func (r *usageCaptureReader) Read(buffer []byte) (int, error) {
	count, err := r.source.Read(buffer)
	if count > 0 {
		r.capture.feed(buffer[:count])
	}
	if err == io.EOF {
		r.capture.finish()
	}
	return count, err
}

func (r *usageCaptureReader) Usage() (TokenUsage, bool) {
	return r.capture.usage, r.capture.found
}

type streamUsageCapture struct {
	pending bytes.Buffer
	usage   TokenUsage
	found   bool
}

func (c *streamUsageCapture) feed(data []byte) {
	if c.pending.Len()+len(data) > 1<<20 {
		c.pending.Reset()
		return
	}
	_, _ = c.pending.Write(data)
	for {
		line, err := c.pending.ReadString('\n')
		if err != nil {
			_, _ = c.pending.WriteString(line)
			return
		}
		c.parseLine(line)
	}
}

func (c *streamUsageCapture) finish() {
	if c.pending.Len() > 0 {
		c.parseLine(c.pending.String())
		c.pending.Reset()
	}
}

func (c *streamUsageCapture) parseLine(line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	if usage, ok := ExtractTokenUsage([]byte(data)); ok {
		c.usage = usage
		c.found = true
	}
}

func tokenUsageFromValue(value any) (TokenUsage, bool) {
	switch typed := value.(type) {
	case map[string]any:
		if raw, ok := typed["usage"].(map[string]any); ok {
			if usage, ok := tokenUsageFromMap(raw); ok {
				return usage, true
			}
		}
		if usage, ok := tokenUsageFromMap(typed); ok {
			return usage, true
		}
		for _, item := range typed {
			if usage, ok := tokenUsageFromValue(item); ok {
				return usage, true
			}
		}
	case []any:
		for _, item := range typed {
			if usage, ok := tokenUsageFromValue(item); ok {
				return usage, true
			}
		}
	}
	return TokenUsage{}, false
}

func tokenUsageFromMap(value map[string]any) (TokenUsage, bool) {
	input, inputOK := numericInt64(value, "prompt_tokens", "input_tokens")
	output, outputOK := numericInt64(value, "completion_tokens", "output_tokens")
	if !inputOK && !outputOK {
		return TokenUsage{}, false
	}
	return TokenUsage{Input: input, Output: output, Exact: true}, true
}

func numericInt64(value map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch number := value[key].(type) {
		case float64:
			return int64(number), true
		case json.Number:
			parsed, err := number.Int64()
			return parsed, err == nil
		case int64:
			return number, true
		case int:
			return int64(number), true
		}
	}
	return 0, false
}

func catalogNeuronRate(model map[string]any) (NeuronRate, bool) {
	properties, ok := model["properties"].([]any)
	if !ok {
		return NeuronRate{}, false
	}
	for _, property := range properties {
		entry, ok := property.(map[string]any)
		if !ok || !strings.Contains(strings.ToLower(stringValue(entry["property_id"])), "price") {
			continue
		}
		value := entry["value"]
		if encoded, ok := value.(string); ok {
			var decoded any
			if json.Unmarshal([]byte(encoded), &decoded) == nil {
				value = decoded
			}
		}
		input, inputOK := recursiveNumber(value, "input_neurons_per_million_tokens", "neurons_per_million_input_tokens")
		output, outputOK := recursiveNumber(value, "output_neurons_per_million_tokens", "neurons_per_million_output_tokens")
		if inputOK || outputOK {
			return NeuronRate{InputPerMillion: input, OutputPerMillion: output}, true
		}
	}
	return NeuronRate{}, false
}

func recursiveNumber(value any, keys ...string) (float64, bool) {
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	var search func(any) (float64, bool)
	search = func(current any) (float64, bool) {
		switch typed := current.(type) {
		case map[string]any:
			for key, item := range typed {
				if _, ok := wanted[strings.ToLower(key)]; ok {
					if number, ok := item.(float64); ok {
						return number, true
					}
				}
			}
			for _, item := range typed {
				if number, ok := search(item); ok {
					return number, true
				}
			}
		case []any:
			for _, item := range typed {
				if number, ok := search(item); ok {
					return number, true
				}
			}
		}
		return 0, false
	}
	return search(value)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func fallbackNeuronRate(model string) (NeuronRate, string) {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "embedding") || strings.Contains(lower, "bge"):
		return NeuronRate{InputPerMillion: 20_000}, "fallback_embedding"
	case strings.Contains(lower, "vision") || strings.Contains(lower, "image") || strings.Contains(lower, "flux"):
		return NeuronRate{InputPerMillion: 50_000, OutputPerMillion: 100_000}, "fallback_multimodal"
	default:
		return NeuronRate{InputPerMillion: 50_000, OutputPerMillion: 250_000}, "fallback_text"
	}
}

// Cloudflare Workers AI pricing, last verified 2026-07-08.
// Rates are Neurons per million input/output tokens.
var bundledNeuronRates = map[string]NeuronRate{
	"@cf/meta/llama-3.2-1b-instruct":               {InputPerMillion: 2_457, OutputPerMillion: 18_252},
	"@cf/meta/llama-3.2-3b-instruct":               {InputPerMillion: 4_625, OutputPerMillion: 30_475},
	"@cf/meta/llama-3.1-8b-instruct-fp8-fast":      {InputPerMillion: 4_119, OutputPerMillion: 34_868},
	"@cf/meta/llama-3.2-11b-vision-instruct":       {InputPerMillion: 4_410, OutputPerMillion: 61_493},
	"@cf/meta/llama-3.1-70b-instruct-fp8-fast":     {InputPerMillion: 26_668, OutputPerMillion: 204_805},
	"@cf/meta/llama-3.3-70b-instruct-fp8-fast":     {InputPerMillion: 26_668, OutputPerMillion: 204_805},
	"@cf/deepseek-ai/deepseek-r1-distill-qwen-32b": {InputPerMillion: 45_170, OutputPerMillion: 443_756},
	"@cf/mistral/mistral-7b-instruct-v0.1":         {InputPerMillion: 10_000, OutputPerMillion: 17_300},
	"@cf/mistralai/mistral-small-3.1-24b-instruct": {InputPerMillion: 31_876, OutputPerMillion: 50_488},
	"@cf/meta/llama-3.1-8b-instruct":               {InputPerMillion: 25_608, OutputPerMillion: 75_147},
	"@cf/meta/llama-3.1-8b-instruct-fp8":           {InputPerMillion: 13_778, OutputPerMillion: 26_128},
	"@cf/meta/llama-3.1-8b-instruct-awq":           {InputPerMillion: 11_161, OutputPerMillion: 24_215},
	"@cf/meta/llama-3-8b-instruct":                 {InputPerMillion: 25_608, OutputPerMillion: 75_147},
	"@cf/meta/llama-3-8b-instruct-awq":             {InputPerMillion: 11_161, OutputPerMillion: 24_215},
	"@cf/meta/llama-2-7b-chat-fp16":                {InputPerMillion: 50_505, OutputPerMillion: 606_061},
	"@cf/meta/llama-guard-3-8b":                    {InputPerMillion: 44_003, OutputPerMillion: 2_730},
	"@cf/meta/llama-4-scout-17b-16e-instruct":      {InputPerMillion: 24_545, OutputPerMillion: 77_273},
	"@cf/google/gemma-3-12b-it":                    {InputPerMillion: 31_371, OutputPerMillion: 50_560},
	"@cf/qwen/qwq-32b":                             {InputPerMillion: 60_000, OutputPerMillion: 90_909},
	"@cf/qwen/qwen2.5-coder-32b-instruct":          {InputPerMillion: 60_000, OutputPerMillion: 90_909},
	"@cf/qwen/qwen3-30b-a3b-fp8":                   {InputPerMillion: 4_625, OutputPerMillion: 30_475},
	"@cf/openai/gpt-oss-120b":                      {InputPerMillion: 31_818, OutputPerMillion: 68_182},
	"@cf/openai/gpt-oss-20b":                       {InputPerMillion: 18_182, OutputPerMillion: 27_273},
	"@cf/aisingapore/gemma-sea-lion-v4-27b-it":     {InputPerMillion: 31_876, OutputPerMillion: 50_488},
	"@cf/ibm-granite/granite-4.0-h-micro":          {InputPerMillion: 1_542, OutputPerMillion: 10_158},
	"@cf/zai-org/glm-4.7-flash":                    {InputPerMillion: 5_500, OutputPerMillion: 36_400},
	"@cf/zai-org/glm-5.2":                          {InputPerMillion: 127_273, OutputPerMillion: 400_000},
	"@cf/nvidia/nemotron-3-120b-a12b":              {InputPerMillion: 45_455, OutputPerMillion: 136_364},
	"@cf/moonshotai/kimi-k2.5":                     {InputPerMillion: 54_545, OutputPerMillion: 272_727},
	"@cf/moonshotai/kimi-k2.6":                     {InputPerMillion: 86_364, OutputPerMillion: 363_636},
	"@cf/moonshotai/kimi-k2.7-code":                {InputPerMillion: 86_364, OutputPerMillion: 363_636},
	"@cf/google/gemma-4-26b-a4b-it":                {InputPerMillion: 9_091, OutputPerMillion: 27_273},
	"@cf/baai/bge-small-en-v1.5":                   {InputPerMillion: 1_841},
	"@cf/baai/bge-base-en-v1.5":                    {InputPerMillion: 6_058},
	"@cf/baai/bge-large-en-v1.5":                   {InputPerMillion: 18_582},
	"@cf/baai/bge-m3":                              {InputPerMillion: 1_075},
	"@cf/pfnet/plamo-embedding-1b":                 {InputPerMillion: 1_689},
	"@cf/qwen/qwen3-embedding-0.6b":                {InputPerMillion: 1_075},
}
