package responsescompat

import (
	"context"
	"fmt"

	"github.com/tidwall/gjson"
)

func TranslateResponse(model string, originalRequest, chatRequest, body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, &Error{Status: 502, Code: "invalid_upstream_response", Message: "Workers AI returned an invalid JSON response."}
	}
	root := gjson.ParseBytes(body)
	if root.Get("error").Exists() {
		return nil, &Error{Status: 502, Code: "upstream_error", Message: "Workers AI returned an error object."}
	}
	translated := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), model, originalRequest, chatRequest, body, nil)
	if !gjson.ValidBytes(translated) {
		return nil, &Error{Status: 502, Code: "invalid_upstream_response", Message: fmt.Sprintf("Workers AI returned an invalid response for model %q.", model)}
	}
	return translated, nil
}
