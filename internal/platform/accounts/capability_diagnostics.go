package accounts

import (
	"fmt"
	"net/http"
	"strings"
)

func capabilityLabel(name string) string {
	switch name {
	case "api_token":
		return "API Token 验证"
	case "r2":
		return "R2 桶列表读取"
	case "d1":
		return "D1 数据库列表读取"
	case "ai":
		return "Workers AI 模型列表读取"
	case "analytics":
		return "账号分析 GraphQL 查询"
	default:
		return name
	}
}

func capabilityFailure(name, method, path string, apiErr *CloudflareAPIError) string {
	advice := "Cloudflare 拒绝了检查。请根据返回信息检查 Token 状态、权限、账号范围及服务是否已开通。"
	switch apiErr.StatusCode {
	case http.StatusUnauthorized:
		advice = "身份验证未通过。请检查 API Token 是否复制完整、过期或被撤销，以及 Token 的 IP 限制；不能用 Global API Key 或 R2 Secret Access Key 代替 API Token。"
	case http.StatusForbidden:
		advice = "访问被拒绝。请检查 Token 的账号资源范围是否包含当前 Cloudflare Account ID，以及权限和 IP 限制。"
		switch name {
		case "r2":
			advice += "读取桶列表需要 Workers R2 Storage Read 权限；此项检查使用 API Token，未验证 R2 Access Key / Secret Access Key。"
		case "d1":
			advice += "请检查 D1 Read 权限（数据库写入还需 D1 Edit）。"
		case "ai":
			advice += "请检查 Workers AI Read 权限。"
		case "analytics":
			advice += "请检查 Account Analytics Read 权限。"
		}
	case http.StatusNotFound:
		advice = "未找到检测接口或账号资源。请核对 Cloudflare Account ID、对应服务是否已开通，以及代理是否改写了请求。"
	case http.StatusTooManyRequests:
		advice = "Cloudflare 请求频率受限，请稍后重新检测。"
	default:
		if apiErr.StatusCode >= http.StatusInternalServerError {
			advice = "Cloudflare 或中间代理服务异常，请稍后重试；若持续失败，请检查服务器网络与代理。"
		}
	}
	code := ""
	if apiErr.Code != 0 {
		code = fmt.Sprintf("；Cloudflare 错误码 %d", apiErr.Code)
	}
	return fmt.Sprintf("%s失败：%s\n%s %s；HTTP %d%s；%s", capabilityLabel(name), advice, method, path, apiErr.StatusCode, code, apiErr.Message)
}

func redactProbeError(message, token string) string {
	if token != "" {
		return strings.ReplaceAll(message, token, "[redacted]")
	}
	return message
}
