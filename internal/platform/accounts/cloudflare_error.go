package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	maxCloudflareResponseBytes = 4 << 20
	r2BucketNotFoundCode       = 10006
	r2ObjectNotFoundCode       = 10007
)

// CloudflareAPIError preserves the status and API code needed to classify a
// failed Cloudflare operation without parsing its display text.
type CloudflareAPIError struct {
	Operation  string
	StatusCode int
	Code       int
	Message    string
}

func (e *CloudflareAPIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("%s: cloudflare code %d: %s", e.Operation, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: cloudflare HTTP %d: %s", e.Operation, e.StatusCode, e.Message)
}

// IsR2BucketNotFound only accepts Cloudflare's structured NoSuchBucket error.
// A bare HTTP 404 can be produced by a proxy or WAF and is not proof that the
// bucket is absent.
func IsR2BucketNotFound(err error) bool {
	var apiErr *CloudflareAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Code == r2BucketNotFoundCode
}

func IsR2ObjectNotFound(err error) bool {
	var apiErr *CloudflareAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Code == r2ObjectNotFoundCode
}

type cloudflareErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cloudflareEnvelope struct {
	Success    *bool                   `json:"success"`
	Errors     []cloudflareErrorDetail `json:"errors"`
	Result     json.RawMessage         `json:"result"`
	ResultInfo json.RawMessage         `json:"result_info"`
}

func readCloudflareEnvelope(operation string, response *http.Response, secret string) (cloudflareEnvelope, error) {
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, maxCloudflareResponseBytes+1))
	if err != nil {
		return cloudflareEnvelope{}, fmt.Errorf("%s: read cloudflare response: %w", operation, err)
	}
	if len(data) > maxCloudflareResponseBytes {
		return cloudflareEnvelope{}, fmt.Errorf("%s: cloudflare response exceeds %d bytes", operation, maxCloudflareResponseBytes)
	}

	var envelope cloudflareEnvelope
	var decodeErr error
	if len(data) != 0 {
		decodeErr = json.Unmarshal(data, &envelope)
	}

	successStatus := response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
	apiRejected := envelope.Success != nil && !*envelope.Success
	if !successStatus || apiRejected || len(envelope.Errors) != 0 {
		message := http.StatusText(response.StatusCode)
		code := 0
		if decodeErr == nil && len(envelope.Errors) != 0 {
			code = envelope.Errors[0].Code
			if envelope.Errors[0].Message != "" {
				message = envelope.Errors[0].Message
			}
		}
		if apiRejected && len(envelope.Errors) == 0 {
			message = "request rejected"
		}
		if message == "" {
			message = "request rejected"
		}
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
		return cloudflareEnvelope{}, &CloudflareAPIError{
			Operation: operation, StatusCode: response.StatusCode, Code: code, Message: message,
		}
	}
	if len(data) == 0 {
		return envelope, nil
	}
	if decodeErr != nil {
		return cloudflareEnvelope{}, fmt.Errorf("%s: decode cloudflare response: %w", operation, decodeErr)
	}
	return envelope, nil
}

func decodeCloudflareResult(operation string, result json.RawMessage, target any) error {
	if target == nil || len(result) == 0 || string(result) == "null" {
		return nil
	}
	if err := json.Unmarshal(result, target); err != nil {
		return fmt.Errorf("%s: decode cloudflare result: %w", operation, err)
	}
	return nil
}
