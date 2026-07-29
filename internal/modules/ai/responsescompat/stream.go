package responsescompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

var (
	ErrIncompleteStream      = errors.New("Workers AI stream ended before [DONE]")
	ErrInvalidUpstreamStream = errors.New("Workers AI returned an invalid streaming response")
	ErrUpstreamStream        = errors.New("Workers AI returned a streaming error")
)

const maxSSEEventSize = 1 << 20

// TranslateStream converts a Chat Completions SSE stream into Responses SSE.
// It never emits response.completed unless the upstream explicitly terminates
// with [DONE] after a finish_reason.
func TranslateStream(
	ctx context.Context,
	model string,
	originalRequest []byte,
	chatRequest []byte,
	source io.Reader,
	destination io.Writer,
) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	var state any
	var eventData []string
	eventSize := 0
	sawDone := false

	writeChunks := func(chunks [][]byte) error {
		for _, chunk := range chunks {
			if len(chunk) == 0 {
				continue
			}
			frame := append(append([]byte(nil), chunk...), '\n', '\n')
			if _, err := destination.Write(frame); err != nil {
				return err
			}
			if flusher, ok := destination.(interface{ Flush() }); ok {
				flusher.Flush()
			}
		}
		return nil
	}

	writeError := func(code, message string) {
		sequence := nextStreamSequence(&state)
		payload := map[string]any{
			"type":            "error",
			"sequence_number": sequence,
			"error": map[string]string{
				"type":    "upstream_error",
				"code":    code,
				"message": message,
			},
		}
		raw, _ := json.Marshal(payload)
		frame := append(sseEventData("error", raw), '\n', '\n')
		_, _ = destination.Write(frame)
		if flusher, ok := destination.(interface{ Flush() }); ok {
			flusher.Flush()
		}
	}

	process := func(data []byte) error {
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			return nil
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			if err := writeChunks(ConvertOpenAIChatCompletionsResponseToOpenAIResponses(ctx, model, originalRequest, chatRequest, data, &state)); err != nil {
				return err
			}
			sawDone = true
			if current, ok := state.(*oaiToResponsesState); !ok || !current.CompletedEmitted {
				writeError("incomplete_stream", ErrIncompleteStream.Error())
				return ErrIncompleteStream
			}
			return nil
		}
		if !json.Valid(data) {
			writeError("invalid_upstream_response", "Workers AI returned malformed streaming JSON.")
			return ErrInvalidUpstreamStream
		}
		root := gjson.ParseBytes(data)
		if root.Get("error").Exists() {
			message := root.Get("error.message").String()
			if message == "" {
				message = root.Get("error").String()
			}
			code := root.Get("error.code").String()
			if code == "" {
				code = "upstream_error"
			}
			writeError(code, message)
			return ErrUpstreamStream
		}
		if !root.Get("choices").Exists() && !root.Get("usage").Exists() {
			writeError("invalid_upstream_response", "Workers AI returned a streaming object without choices.")
			return ErrInvalidUpstreamStream
		}
		return writeChunks(ConvertOpenAIChatCompletionsResponseToOpenAIResponses(ctx, model, originalRequest, chatRequest, data, &state))
	}

	flushEvent := func() error {
		if len(eventData) == 0 {
			return nil
		}
		data := []byte(strings.Join(eventData, "\n"))
		eventData = eventData[:0]
		eventSize = 0
		return process(data)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := readSSELine(reader, maxSSEEventSize)
		if err == nil && len(bytes.TrimSpace(line)) == 0 {
			if flushErr := flushEvent(); flushErr != nil {
				return flushErr
			}
		} else if len(line) > 0 {
			if len(line) > maxSSEEventSize-eventSize {
				writeError("invalid_upstream_response", "Workers AI SSE event exceeded the 1 MiB limit.")
				return fmt.Errorf("%w: event exceeds limit", ErrInvalidUpstreamStream)
			}
			eventSize += len(line)
			if line[0] != ':' {
				field, value, hasValue := bytes.Cut(line, []byte(":"))
				if hasValue {
					value = bytes.TrimPrefix(value, []byte(" "))
				}
				if string(field) == "data" && hasValue {
					eventData = append(eventData, string(value))
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			writeError("upstream_error", err.Error())
			return err
		}
	}
	if !sawDone && len(eventData) > 0 {
		if err := flushEvent(); err != nil {
			return err
		}
	}
	if sawDone {
		return nil
	}
	writeError("incomplete_stream", ErrIncompleteStream.Error())
	return ErrIncompleteStream
}

func readSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, isPrefix, err := reader.ReadLine()
		line = append(line, part...)
		if len(line) > limit {
			return line, fmt.Errorf("SSE line exceeds limit")
		}
		if !isPrefix {
			return line, err
		}
		if err != nil {
			return line, err
		}
	}
}

func nextStreamSequence(state *any) int {
	if current, ok := (*state).(*oaiToResponsesState); ok {
		current.Seq++
		return current.Seq
	}
	return 1
}
