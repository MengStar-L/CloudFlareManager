package responsescompat

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTranslateResponseMapsTextToolsAndUsage(t *testing.T) {
	request := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	chat := []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1700000000,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"answer","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}},{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":2}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`)

	translated, err := TranslateResponse("m", request, request, chat)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(translated) {
		t.Fatalf("invalid response: %s", translated)
	}
	assertJSONPath(t, translated, "status", "completed")
	assertJSONPath(t, translated, "output.0.type", "message")
	assertJSONPath(t, translated, "output.0.content.0.text", "answer")
	assertJSONPath(t, translated, "output.1.type", "function_call")
	assertJSONPath(t, translated, "output.2.call_id", "call_2")
	assertJSONPath(t, translated, "usage.input_tokens", int64(11))
	assertJSONPath(t, translated, "usage.input_tokens_details.cached_tokens", int64(3))
	assertJSONPath(t, translated, "usage.output_tokens", int64(7))
	assertJSONPath(t, translated, "usage.output_tokens_details.reasoning_tokens", int64(2))
	assertJSONPath(t, translated, "usage.total_tokens", int64(18))
}

func TestTranslateResponseDoesNotInventUsage(t *testing.T) {
	chat := []byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	translated, err := TranslateResponse("m", []byte(`{"model":"m","input":"hi"}`), nil, chat)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(translated, "usage").Exists() {
		t.Fatalf("usage should remain absent: %s", translated)
	}
}

func TestNormalizeUpstreamError(t *testing.T) {
	tests := []struct {
		body string
		code string
		msg  string
	}{
		{`{"error":{"code":"invalid_prompt","message":"bad"}}`, "invalid_prompt", "bad"},
		{`{"errors":[{"message":"bad"}]}`, "upstream_error", "bad"},
		{`plain text`, "upstream_error", "plain text"},
		{"", "upstream_error", "Workers AI returned an empty error response."},
	}
	for _, test := range tests {
		t.Run(test.code+test.msg, func(t *testing.T) {
			body := NormalizeUpstreamError(400, []byte(test.body))
			var envelope struct {
				Error struct {
					Type    string `json:"type"`
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Type != "upstream_error" || envelope.Error.Code != test.code || envelope.Error.Message != test.msg {
				t.Fatalf("envelope = %s", body)
			}
		})
	}
}
