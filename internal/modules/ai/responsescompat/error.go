package responsescompat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// Error is safe to expose through the OpenAI-compatible error envelope.
type Error struct {
	Status  int
	Code    string
	Message string
	Param   string
}

func (e *Error) Error() string {
	return e.Message
}

func invalid(param, format string, args ...any) *Error {
	return &Error{
		Status:  400,
		Code:    "invalid_request_error",
		Message: fmt.Sprintf(format, args...),
		Param:   param,
	}
}

func unsupported(param, format string, args ...any) *Error {
	return &Error{
		Status:  400,
		Code:    "unsupported_feature",
		Message: fmt.Sprintf(format, args...),
		Param:   param,
	}
}

func NormalizeUpstreamError(_ int, body []byte) []byte {
	code := "upstream_error"
	message := "Workers AI returned an empty error response."
	trimmed := strings.TrimSpace(string(body))
	if gjson.ValidBytes(body) {
		root := gjson.ParseBytes(body)
		if value := strings.TrimSpace(root.Get("error.code").String()); value != "" {
			code = value
		} else if value := strings.TrimSpace(root.Get("error.type").String()); value != "" {
			code = value
		}
		if value := strings.TrimSpace(root.Get("error.message").String()); value != "" {
			message = value
		} else if value := strings.TrimSpace(root.Get("errors.0.message").String()); value != "" {
			message = value
		} else if root.Get("error").Type == gjson.String {
			message = root.Get("error").String()
		}
	} else if trimmed != "" {
		message = trimmed
	}
	return ErrorResponse(code, message)
}

func ErrorResponse(code, message string) []byte {
	if strings.TrimSpace(code) == "" {
		code = "upstream_error"
	}
	if strings.TrimSpace(message) == "" {
		message = "Workers AI returned an error."
	}
	payload := map[string]any{
		"error": map[string]string{
			"type":    "upstream_error",
			"code":    code,
			"message": message,
		},
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","code":"upstream_error","message":"Workers AI returned an error."}}`)
	}
	return result
}
