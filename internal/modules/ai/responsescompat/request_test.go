package responsescompat

import (
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTranslateRequestAcceptsSub2APIProbe(t *testing.T) {
	raw := []byte(`{
		"model":"@cf/meta/llama-3.1-8b-instruct",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"instructions":"You are a helpful assistant.",
		"stream":true
	}`)

	request, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "@cf/meta/llama-3.1-8b-instruct" || !request.Stream {
		t.Fatalf("request = %#v", request)
	}
	assertJSONPath(t, request.ChatBody, "messages.0.role", "system")
	assertJSONPath(t, request.ChatBody, "messages.1.role", "user")
	assertJSONPath(t, request.ChatBody, "messages.1.content.0.text", "hi")
}

func TestTranslateRequestMapsParametersAndStructuredOutput(t *testing.T) {
	raw := []byte(`{
		"model":"m","input":"hi","max_output_tokens":200,"temperature":0.2,
		"top_p":0.8,"frequency_penalty":0.1,"presence_penalty":0.3,
		"top_logprobs":5,"reasoning":{"effort":"high"},
		"text":{"format":{"type":"json_schema","name":"answer","description":"result","schema":{"type":"object"},"strict":true}},
		"tools":[{"type":"function","name":"lookup","description":"look up","parameters":{"type":"object"},"strict":true}],
		"tool_choice":{"type":"function","name":"lookup"}
	}`)

	request, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"max_tokens":                         int64(200),
		"temperature":                        0.2,
		"top_p":                              0.8,
		"frequency_penalty":                  0.1,
		"presence_penalty":                   0.3,
		"top_logprobs":                       int64(5),
		"logprobs":                           true,
		"reasoning_effort":                   "high",
		"response_format.type":               "json_schema",
		"response_format.json_schema.name":   "answer",
		"response_format.json_schema.strict": true,
		"tools.0.function.strict":            true,
		"tool_choice.function.name":          "lookup",
	}
	for path, want := range checks {
		assertJSONPath(t, request.ChatBody, path, want)
	}
	if !gjson.GetBytes(request.ChatBody, "response_format.json_schema.schema").IsObject() {
		t.Fatalf("missing JSON schema: %s", request.ChatBody)
	}
}

func TestTranslateRequestConvertsImageAndFunctionHistory(t *testing.T) {
	raw := []byte(`{
		"model":"m",
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"describe"},
				{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"low"}
			]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	request, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONPath(t, request.ChatBody, "messages.0.content.1.type", "image_url")
	assertJSONPath(t, request.ChatBody, "messages.0.content.1.image_url.detail", "low")
	assertJSONPath(t, request.ChatBody, "messages.1.tool_calls.0.id", "call_1")
	assertJSONPath(t, request.ChatBody, "messages.2.tool_call_id", "call_1")
}

func TestTranslateRequestRejectsUnsupportedFeatures(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		code  string
		param string
	}{
		{"invalid JSON", `{`, "invalid_request_error", "body"},
		{"missing model", `{"input":"hi"}`, "invalid_request_error", "model"},
		{"missing input", `{"model":"m"}`, "invalid_request_error", "input"},
		{"background", `{"model":"m","input":"hi","background":true}`, "unsupported_feature", "background"},
		{"stored response", `{"model":"m","input":"hi","store":true}`, "unsupported_feature", "store"},
		{"previous response", `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, "unsupported_feature", "previous_response_id"},
		{"web search", `{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`, "unsupported_feature", "tools[0].type"},
		{"computer use", `{"model":"m","input":"hi","tools":[{"type":"computer_use_preview"}]}`, "unsupported_feature", "tools[0].type"},
		{"input file", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`, "unsupported_feature", "input[0].content[0].type"},
		{"streamed schema", `{"model":"m","input":"hi","stream":true,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`, "unsupported_feature", "stream"},
		{"orphan tool output", `{"model":"m","input":[{"type":"function_call_output","call_id":"call_1","output":"x"}]}`, "invalid_request_error", "input[0].call_id"},
		{"duplicate tool output", `{"model":"m","input":[{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"x"},{"type":"function_call_output","call_id":"call_1","output":"y"}]}`, "invalid_request_error", "input[2].call_id"},
		{"bad function choice", `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{}}],"tool_choice":{"type":"function"}}`, "invalid_request_error", "tool_choice.name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := TranslateRequest([]byte(test.body))
			var compatibilityErr *Error
			if !errors.As(err, &compatibilityErr) {
				t.Fatalf("error = %v", err)
			}
			if compatibilityErr.Code != test.code || compatibilityErr.Param != test.param || compatibilityErr.Status != 400 {
				t.Fatalf("error = %#v", compatibilityErr)
			}
		})
	}
}

func assertJSONPath(t *testing.T, raw []byte, path string, want any) {
	t.Helper()
	got := gjson.GetBytes(raw, path)
	if !got.Exists() {
		t.Fatalf("%s missing in %s", path, raw)
	}
	switch expected := want.(type) {
	case string:
		if got.String() != expected {
			t.Fatalf("%s = %q, want %q", path, got.String(), expected)
		}
	case bool:
		if got.Bool() != expected {
			t.Fatalf("%s = %v, want %v", path, got.Bool(), expected)
		}
	case int64:
		if got.Int() != expected {
			t.Fatalf("%s = %d, want %d", path, got.Int(), expected)
		}
	case float64:
		if got.Float() != expected {
			t.Fatalf("%s = %v, want %v", path, got.Float(), expected)
		}
	default:
		t.Fatalf("unsupported expected type %T", want)
	}
}
