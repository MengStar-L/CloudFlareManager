# OpenAI Responses Compatibility Implementation Plan

**Goal:** Make `POST /v1/responses` work consistently with Cloudflare Workers AI text-generation models, including Sub2API streaming probes, multimodal input, function tools, structured output, reasoning, usage, and standard OpenAI errors.

**Architecture:** Add an isolated `internal/modules/ai/responsescompat` package that validates a stateless Responses request, translates it to Chat Completions, and translates Chat JSON/SSE back to Responses. Reuse the focused MIT-licensed conversion core from CLIProxyAPI `v7.2.104` while keeping Cloudflare-specific validation and transport in CF-R2Manager; only the gateway selects accounts, retries requests, writes HTTP, and records usage.

**Tech Stack:** Go 1.26, `net/http`, `bufio`, `encoding/json`, `github.com/tidwall/gjson`, `github.com/tidwall/sjson`, existing SQLite/account router, `httptest`.

---

## File Map

- Create `internal/modules/ai/responsescompat/error.go`: typed client/upstream conversion errors.
- Create `internal/modules/ai/responsescompat/request.go`: Cloudflare capability validation and public request translation API.
- Create `internal/modules/ai/responsescompat/stream.go`: bounded SSE reader and streaming translator facade.
- Create `internal/modules/ai/responsescompat/upstream_common.go`: MIT-derived raw JSON/SSE helpers.
- Create `internal/modules/ai/responsescompat/upstream_request.go`: MIT-derived Responses-to-Chat message conversion.
- Create `internal/modules/ai/responsescompat/upstream_tools.go`: MIT-derived tool conversion and tool-call identity helpers.
- Create `internal/modules/ai/responsescompat/upstream_response.go`: MIT-derived Chat-to-Responses stream/non-stream state machine.
- Create `internal/modules/ai/responsescompat/request_test.go`: Sub2API fixture, parameter, multimodal, tool, and rejection tests.
- Create `internal/modules/ai/responsescompat/stream_test.go`: fragmented SSE and event-order tests.
- Create `internal/modules/ai/responsescompat/response_test.go`: non-stream response and usage tests.
- Create `third_party/licenses/CLIProxyAPI-MIT.txt`: complete upstream MIT text and source pin.
- Modify `internal/modules/ai/gateway.go`: prepare Responses requests, target Chat Completions, translate success/error bodies, preserve retry/logging.
- Modify `internal/modules/ai/gateway_test.go`: gateway integration, stream, retry, and unchanged endpoint tests.
- Modify `internal/protocol/ai/handler.go`: surface typed 400 errors with `param`.
- Modify `internal/protocol/ai/handler_test.go`: standard unsupported/invalid error contract tests.
- Modify `go.mod` and `go.sum`: add only `gjson` and `sjson`, not CLIProxyAPI's full module.
- Modify `NOTICE`: attribute CLIProxyAPI.
- Modify `.github/workflows/release.yml`: package third-party license text in release archives.

## Task 1: Establish Provenance and Minimal Dependencies

**Files:**
- Create: `third_party/licenses/CLIProxyAPI-MIT.txt`
- Modify: `NOTICE`
- Modify: `.github/workflows/release.yml`
- Modify: `go.mod`
- Modify: `go.sum`

- **Step 1: Add the full pinned upstream license record**

Create `third_party/licenses/CLIProxyAPI-MIT.txt` containing the exact MIT license from CLIProxyAPI `v7.2.104`, preceded by:

```text
CLIProxyAPI
Source: https://github.com/router-for-me/CLIProxyAPI
Version: v7.2.104
Commit: c9417c8ae9b16fabc0386ca35d36f13bf8b1d678

MIT License

Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

Append this exact notice to `NOTICE`:

```text

The OpenAI Responses compatibility translator includes portions adapted from
CLIProxyAPI v7.2.104, Copyright (c) Luis Pater and Router-For.ME,
licensed under the MIT License. See third_party/licenses/CLIProxyAPI-MIT.txt.
```

- **Step 2: Include third-party licenses in release archives**

Change the release staging copy from:

```sh
cp LICENSE NOTICE README.md "$stage/"
```

to:

```sh
cp LICENSE NOTICE README.md "$stage/"
cp -R third_party "$stage/"
```

- **Step 3: Add the two focused JSON dependencies**

Run:

```powershell
go get github.com/tidwall/gjson@v1.18.0 github.com/tidwall/sjson@v1.2.5
go mod tidy
```

Expected: `go.mod` contains direct requirements for `gjson` and `sjson`; it does not contain `github.com/router-for-me/CLIProxyAPI` or Redis.

- **Step 4: Verify dependency scope**

Run:

```powershell
go list -m all | Select-String 'CLIProxyAPI|redis/go-redis'
```

Expected: no output.

## Task 2: Build Request Validation Before Conversion

**Files:**
- Create: `internal/modules/ai/responsescompat/error.go`
- Create: `internal/modules/ai/responsescompat/request.go`
- Create: `internal/modules/ai/responsescompat/request_test.go`

- **Step 1: Write the Sub2API regression test**

Add a test using this exact body shape:

```go
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
```

- **Step 2: Write capability and validation table tests**

Cover these exact outcomes:

```go
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
	{"input file", `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`, "unsupported_feature", "input[0].content[0].type"},
	{"streamed schema", `{"model":"m","input":"hi","stream":true,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`, "unsupported_feature", "stream"},
}
```

Also assert malformed message parts, function calls, tool results, function definitions, and tool choices return `invalid_request_error` with the failing path.

- **Step 3: Run request tests and confirm failure**

Run:

```powershell
go test ./internal/modules/ai/responsescompat -run 'TestTranslateRequest|TestValidate' -count=1
```

Expected: FAIL because the package/API does not exist yet.

- **Step 4: Define the public request/error types**

Implement:

```go
type Error struct {
	Status  int
	Code    string
	Message string
	Param   string
}

func (e *Error) Error() string { return e.Message }

type TranslatedRequest struct {
	Model        string
	Stream       bool
	OriginalBody []byte
	ChatBody     []byte
}

func TranslateRequest(raw []byte) (TranslatedRequest, error)
```

`TranslateRequest` must reject unsupported top-level features before calling the derived converter. It must validate message/content/tool item types and function-call IDs without logging prompt contents.

- **Step 5: Implement explicit Cloudflare parameter normalization**

After the mature message converter runs, apply these exact mappings:

```go
max_output_tokens  -> max_tokens
temperature        -> temperature
top_p              -> top_p
frequency_penalty  -> frequency_penalty
presence_penalty   -> presence_penalty
top_logprobs       -> top_logprobs plus logprobs=true
reasoning.effort   -> reasoning_effort
```

Map Responses structured output as:

```json
{"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}}}
```

to:

```json
{"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":true}}}
```

Normalize a Responses function choice `{ "type":"function", "name":"lookup" }` to Chat's `{ "type":"function", "function":{"name":"lookup"} }`. Preserve the already-Chat-compatible nested form.

- **Step 6: Run request tests**

Run:

```powershell
go test ./internal/modules/ai/responsescompat -run 'TestTranslateRequest|TestValidate' -count=1
```

Expected: PASS.

## Task 3: Import the Mature Message and Tool Conversion Core

**Files:**
- Create: `internal/modules/ai/responsescompat/upstream_common.go`
- Create: `internal/modules/ai/responsescompat/upstream_request.go`
- Create: `internal/modules/ai/responsescompat/upstream_tools.go`
- Modify: `internal/modules/ai/responsescompat/request_test.go`

- **Step 1: Copy the pinned upstream files mechanically**

From a clean checkout of CLIProxyAPI `v7.2.104`, copy:

```text
internal/translator/openai/openai/responses/openai_openai-responses_request.go
  -> internal/modules/ai/responsescompat/upstream_request.go
internal/translator/openai/openai/responses/openai_openai-responses_tools.go
  -> internal/modules/ai/responsescompat/upstream_tools.go
```

Change the package to `responsescompat`, replace the upstream common import with local helpers, and add this header to both files:

```go
// Portions derived from CLIProxyAPI v7.2.104.
// Copyright (c) Luis Pater and Router-For.ME.
// Licensed under the MIT License; see third_party/licenses/CLIProxyAPI-MIT.txt.
// Modified for Cloudflare Workers AI by CF-R2Manager contributors.
```

Implement only the three needed helpers in `upstream_common.go`:

```go
func joinRawArray(items [][]byte) []byte
func setRawArrayItems(data []byte, path string, items [][]byte) []byte
func sseEventData(event string, payload []byte) []byte
```

- **Step 2: Preserve strict function schemas and developer-role compatibility**

Patch the derived function tool conversion to write `function.strict` when the Responses tool supplies `strict`. Patch message conversion so a `developer` role becomes `system` before the Chat body is returned.

- **Step 3: Add focused history/tool tests**

Add tests for:

```text
consecutive function_call items -> one assistant message with two tool_calls
interrupted function calls -> separate assistant messages
function_call_output -> tool message with matching tool_call_id
mixed text + input_image -> text and image_url parts with image detail
function tool strict/parameters -> nested Chat function definition
orphan/duplicate function_call_output -> 400 invalid_request_error
```

- **Step 4: Run request package tests**

Run:

```powershell
go test ./internal/modules/ai/responsescompat -run 'TestTranslateRequest|TestFunction|TestImage|TestTool' -count=1
```

Expected: PASS.

## Task 4: Add Streaming Chat-to-Responses Conversion

**Files:**
- Create: `internal/modules/ai/responsescompat/upstream_response.go`
- Create: `internal/modules/ai/responsescompat/stream.go`
- Create: `internal/modules/ai/responsescompat/stream_test.go`

- **Step 1: Write a complete Sub2API-compatible stream test**

Feed these Chat chunks through a deliberately fragmented reader:

```text
data:{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1700000000,"model":"@cf/meta/llama","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}

data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1700000000,"model":"@cf/meta/llama","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}

data: [DONE]

```

Assert the output contains, in order:

```text
response.created
response.in_progress
response.output_item.added
response.content_part.added
response.output_text.delta ("hel")
response.output_text.delta ("lo")
response.output_text.done
response.content_part.done
response.output_item.done
response.completed
```

Assert every `sequence_number` strictly increases and `response.completed` occurs exactly once.

- **Step 2: Add SSE boundary/error tests**

Test LF, CRLF, `data:` with no space, comment lines, multiple events per read, one byte per read, a final event at EOF, an event larger than the configured limit, malformed JSON, context cancellation, and EOF before `[DONE]`.

Expected EOF behavior: emit one standard `event: error` / `type: error` SSE event and return `ErrIncompleteStream`; never synthesize `response.completed`.

- **Step 3: Run stream tests and confirm failure**

Run:

```powershell
go test ./internal/modules/ai/responsescompat -run 'TestTranslateStream|TestSSE' -count=1
```

Expected: FAIL because the stream translator is not implemented.

- **Step 4: Import the pinned response state machine**

Copy CLIProxyAPI `v7.2.104` file:

```text
internal/translator/openai/openai/responses/openai_openai-responses_response.go
  -> internal/modules/ai/responsescompat/upstream_response.go
```

Change package/imports to local helpers and add the same MIT-derived header. Keep its per-choice/per-tool maps, delayed usage handling, stable output indexes, reasoning events, function argument deltas, terminal completion aggregation, and synthesized IDs.

- **Step 5: Implement the bounded SSE facade**

Expose:

```go
var ErrIncompleteStream = errors.New("Workers AI stream ended before [DONE]")

func TranslateStream(
	ctx context.Context,
	model string,
	originalRequest []byte,
	chatRequest []byte,
	source io.Reader,
	destination io.Writer,
) error
```

Use `bufio.Reader`, a 1 MiB event limit, event-boundary parsing, and context checks. Feed each data payload to the derived chunk converter, append `\n\n` to each returned event, and flush when the destination implements `http.Flusher`.

- **Step 6: Add interleaved function and late-usage tests**

Use sparse tool indexes `0` and `2`, interleave argument fragments, finish with `tool_calls`, then send a choices-empty usage chunk before `[DONE]`. Assert distinct output indexes, complete arguments, usage in `response.completed`, and a single terminal event.

- **Step 7: Run streaming tests**

Run:

```powershell
go test ./internal/modules/ai/responsescompat -run 'TestTranslateStream|TestSSE|TestInterleaved|TestLateUsage' -count=1
```

Expected: PASS.

## Task 5: Add Non-Streaming Response and Error Conversion

**Files:**
- Create: `internal/modules/ai/responsescompat/response_test.go`
- Modify: `internal/modules/ai/responsescompat/upstream_response.go`
- Modify: `internal/modules/ai/responsescompat/error.go`

- **Step 1: Write non-stream text/tool tests**

Translate a Chat response containing assistant text, two function calls, and:

```json
{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}
```

Assert a Responses object with `status=completed`, one message output, two function_call outputs, input/output/total tokens, cached tokens, and reasoning tokens. Assert missing upstream usage stays absent rather than estimated.

- **Step 2: Write upstream-error normalization tests**

Cover Cloudflare bodies shaped as `{"error":{"code":"invalid_prompt","message":"bad"}}`, `{"errors":[{"message":"bad"}]}`, plain text, and empty body. All must produce:

```json
{"error":{"message":"bad","type":"upstream_error","code":"invalid_prompt"}}
```

while preserving the original HTTP status.

- **Step 3: Implement public non-stream/error helpers**

Expose:

```go
func TranslateResponse(model string, originalRequest, chatRequest, body []byte) ([]byte, error)
func NormalizeUpstreamError(status int, body []byte) []byte
```

Reject malformed successful Chat bodies as `502 invalid_upstream_response`. Do not put prompt or tool arguments into error messages.

- **Step 4: Run response tests**

Run:

```powershell
go test ./internal/modules/ai/responsescompat -run 'TestTranslateResponse|TestNormalizeUpstreamError' -count=1
```

Expected: PASS.

## Task 6: Integrate the Adapter into the Gateway

**Files:**
- Modify: `internal/modules/ai/gateway.go`
- Modify: `internal/modules/ai/gateway_test.go`

- **Step 1: Write the gateway request-path integration test**

Send the Sub2API body to `/v1/responses`. Assert the fake Cloudflare server receives:

```text
POST /accounts/cloudflare/ai/v1/chat/completions
Content-Type: application/json
Authorization: Bearer token
```

and its body has `messages`, not `input`. Return Chat SSE and assert the client receives `response.output_text.delta` and `response.completed` with `Content-Type: text/event-stream` and `X-Accel-Buffering: no`.

- **Step 2: Add non-stream, retry, error, and regression tests**

Cover:

```text
Responses stream=false -> translated Responses JSON
first account 429 and second succeeds -> one translated response stream
final Cloudflare 400 -> normalized OpenAI error with status 400
local unsupported feature -> no account/upstream call
/v1/chat/completions -> unchanged pass-through
/v1/embeddings -> unchanged pass-through
/v1/run/@cf/model -> unchanged native path
```

- **Step 3: Run gateway tests and confirm failure**

Run:

```powershell
go test ./internal/modules/ai -run 'TestGateway' -count=1
```

Expected: new Responses tests FAIL because the gateway still forwards `/v1/responses` unchanged.

- **Step 4: Prepare Responses once before account retry**

At the start of `Gateway.Forward`, translate only when `request.URL.Path == "/v1/responses"`. Preserve the original body for response metadata and compute model/input estimates from the original request. Store the prepared upstream path/body so retries do not repeat validation or mutate data.

- **Step 5: Translate only successful final responses**

For a 2xx Responses upstream result:

```go
if prepared.stream {
	copy safe SSE headers
	write 200
	err = responsescompat.TranslateStream(ctx, model, original, chatBody, response.Body, counter)
} else {
	body, err := readLimited(response.Body, 16<<20)
	translated, err := responsescompat.TranslateResponse(model, original, chatBody, body)
	write application/json and translated body
}
```

For a final non-2xx result, read at most 2 MiB and write `NormalizeUpstreamError`; retain current 429/5xx account retry before writing any headers.

- **Step 6: Run gateway tests**

Run:

```powershell
go test ./internal/modules/ai -run 'TestGateway' -count=1
```

Expected: PASS.

## Task 7: Surface Standard Client Errors at the Protocol Boundary

**Files:**
- Modify: `internal/protocol/ai/handler.go`
- Modify: `internal/protocol/ai/handler_test.go`

- **Step 1: Write protocol error tests**

Send `background:true` and invalid input with a valid AI credential. Assert status 400 and exact shape:

```json
{
  "error": {
    "message": "Background responses are not supported by Workers AI",
    "type": "unsupported_feature",
    "code": "unsupported_feature",
    "param": "background"
  }
}
```

Assert no error response includes a prompt string from the request.

- **Step 2: Run the protocol test and confirm failure**

Run:

```powershell
go test ./internal/protocol/ai -run 'TestResponses.*Error' -count=1
```

Expected: FAIL because `Handler` currently maps every gateway error to 502 without `param`.

- **Step 3: Map typed compatibility errors**

Use `errors.As` on the error returned by `Gateway.Forward`. For `*responsescompat.Error`, write its status/code/message/param; retain quota mapping and generic 502 behavior for all other errors. Extend the error JSON helper to omit `param` only when empty.

- **Step 4: Run protocol tests**

Run:

```powershell
go test ./internal/protocol/ai -count=1
```

Expected: PASS.

## Task 8: Full Verification and Source Audit

**Files:**
- Verify: all files above

- **Step 1: Format and inspect the focused diff**

Run:

```powershell
gofmt -w internal/modules/ai/responsescompat internal/modules/ai/gateway.go internal/modules/ai/gateway_test.go internal/protocol/ai/handler.go internal/protocol/ai/handler_test.go
git diff --check
```

Expected: no gofmt or whitespace errors.

- **Step 2: Run package tests with the race detector**

Run:

```powershell
go test -race -count=1 ./internal/modules/ai/... ./internal/protocol/ai/...
```

Expected: PASS.

- **Step 3: Run the repository verification suite**

Run:

```powershell
go test -race -count=1 ./...
go vet ./...
go build -trimpath ./cmd/cf-r2-manager
```

Expected: all commands exit 0.

- **Step 4: Verify dependency and license boundaries**

Run:

```powershell
go list -m all | Select-String 'CLIProxyAPI|redis/go-redis'
rg -n 'CLIProxyAPI v7.2.104|c9417c8ae9b16fabc0386ca35d36f13bf8b1d678|MIT License' NOTICE third_party internal/modules/ai/responsescompat
```

Expected: first command has no output; second finds the pin/license in NOTICE, the license file, and derived source headers.

- **Step 5: Review behavior against the approved spec**

Confirm each of these has a passing named test: Sub2API probe, string input, structured input, instructions, image input, function tools, function results, tool choice, JSON Schema, generation parameters, reasoning, usage, complete SSE sequence, non-stream response, unsupported OpenAI-hosted features, and unchanged Chat/Embedding/native endpoints.
