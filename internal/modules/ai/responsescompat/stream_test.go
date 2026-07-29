package responsescompat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTranslateStreamProducesSub2APIEvents(t *testing.T) {
	upstream := strings.Join([]string{
		"data:{\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"@cf/meta/llama\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"},\"finish_reason\":null}]}\r\n\r\n",
		": keepalive\r\n\r\n",
		"event: message\ndata: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"@cf/meta/llama\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	}, "")
	var output bytes.Buffer
	err := TranslateStream(
		context.Background(),
		"@cf/meta/llama",
		[]byte(`{"model":"@cf/meta/llama","input":"hi","stream":true}`),
		[]byte(`{"model":"@cf/meta/llama","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		&oneByteReader{reader: strings.NewReader(upstream)},
		&output,
	)
	if err != nil {
		t.Fatalf("%v output=%s", err, output.String())
	}

	events := responseEvents(t, output.String())
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(events) != len(wantOrder) {
		t.Fatalf("events = %v\n%s", eventNames(events), output.String())
	}
	lastSequence := int64(0)
	for i, event := range events {
		if event.name != wantOrder[i] {
			t.Fatalf("event %d = %q, want %q", i, event.name, wantOrder[i])
		}
		sequence := event.data.Get("sequence_number").Int()
		if sequence <= lastSequence {
			t.Fatalf("sequence %d after %d", sequence, lastSequence)
		}
		lastSequence = sequence
	}
	if events[4].data.Get("delta").String() != "hel" || events[5].data.Get("delta").String() != "lo" {
		t.Fatalf("deltas = %q, %q", events[4].data.Get("delta").String(), events[5].data.Get("delta").String())
	}
}

func TestTranslateStreamIncludesLateUsage(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")
	var output bytes.Buffer
	if err := TranslateStream(context.Background(), "m", []byte(`{"model":"m","input":"hi","stream":true}`), nil, strings.NewReader(upstream), &output); err != nil {
		t.Fatalf("%v output=%s", err, output.String())
	}
	events := responseEvents(t, output.String())
	completed := events[len(events)-1].data
	if completed.Get("response.usage.input_tokens").Int() != 11 ||
		completed.Get("response.usage.output_tokens").Int() != 7 ||
		completed.Get("response.usage.total_tokens").Int() != 18 ||
		completed.Get("response.usage.input_tokens_details.cached_tokens").Int() != 3 ||
		completed.Get("response.usage.output_tokens_details.reasoning_tokens").Int() != 2 {
		t.Fatalf("completed usage = %s", completed.Raw)
	}
}

func TestTranslateStreamRejectsIncompleteAndMalformedStreams(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{
			name: "early EOF",
			body: `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n",
			want: ErrIncompleteStream,
		},
		{
			name: "malformed JSON",
			body: "data: {\n\n",
			want: ErrInvalidUpstreamStream,
		},
		{
			name: "done without finish reason",
			body: `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\ndata: [DONE]\n\n",
			want: ErrIncompleteStream,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := TranslateStream(context.Background(), "m", []byte(`{"model":"m","input":"hi","stream":true}`), nil, strings.NewReader(test.body), &output)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(output.String(), "event: response.completed") {
				t.Fatalf("unexpected completion: %s", output.String())
			}
			if !strings.Contains(output.String(), "event: error") {
				t.Fatalf("missing error event: %s", output.String())
			}
		})
	}
}

type oneByteReader struct {
	reader io.Reader
}

func (r *oneByteReader) Read(data []byte) (int, error) {
	if len(data) > 1 {
		data = data[:1]
	}
	return r.reader.Read(data)
}

type parsedResponseEvent struct {
	name string
	data gjson.Result
}

func responseEvents(t *testing.T, stream string) []parsedResponseEvent {
	t.Helper()
	var result []parsedResponseEvent
	for _, block := range strings.Split(strings.ReplaceAll(stream, "\r\n", "\n"), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if name == "" || !gjson.Valid(data) {
			t.Fatalf("invalid SSE block %q", block)
		}
		result = append(result, parsedResponseEvent{name: name, data: gjson.Parse(data)})
	}
	return result
}

func eventNames(events []parsedResponseEvent) []string {
	names := make([]string, len(events))
	for i := range events {
		names[i] = events[i].name
	}
	return names
}
