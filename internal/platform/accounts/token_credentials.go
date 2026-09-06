package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type CredentialDerivationError struct{ Detail string }

func (e *CredentialDerivationError) Error() string {
	return "无法从 API Token 配置 R2 凭证，本次未保存任何更改。" + e.Detail
}

// R2 uses the verified token ID as its access key and SHA-256(token) as its secret.
func (v Verifier) deriveR2Credentials(ctx context.Context, accountID, token string) (string, string, error) {
	baseURL := strings.TrimRight(v.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var failures []string
	for _, path := range []string{"/user/tokens/verify", "/accounts/" + accountID + "/tokens/verify"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return "", "", &CredentialDerivationError{Detail: "无法创建 Token 验证请求。"}
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			failures = append(failures, path+"：连接 Cloudflare 失败，请检查网络后重试。"+redactProbeError(err.Error(), token))
			continue
		}
		envelope, err := readCloudflareEnvelope("GET "+path, response, token)
		if err != nil {
			var apiErr *CloudflareAPIError
			if errors.As(err, &apiErr) {
				failures = append(failures, capabilityFailure("api_token", http.MethodGet, path, apiErr))
			} else {
				failures = append(failures, redactProbeError(err.Error(), token))
			}
			continue
		}
		var result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(envelope.Result, &result) != nil || result.Status != "active" {
			failures = append(failures, path+"：Token 未处于 active 状态。")
			continue
		}
		id, err := hex.DecodeString(result.ID)
		if err != nil || len(id) != 16 {
			failures = append(failures, path+"：Cloudflare 未返回有效的 Token ID，无法生成 R2 Access Key ID。")
			continue
		}
		digest := sha256.Sum256([]byte(token))
		return hex.EncodeToString(id), fmt.Sprintf("%x", digest), nil
	}
	return "", "", &CredentialDerivationError{Detail: strings.Join(failures, "\n")}
}
