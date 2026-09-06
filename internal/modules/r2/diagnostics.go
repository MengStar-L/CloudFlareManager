package r2

import (
	"errors"
	"fmt"
)

var ErrR2Authentication = errors.New("R2 S3 authentication failed")
var ErrWriteRecoveryRequired = errors.New("R2 write requires recovery")
var ErrBucketUnavailable = errors.New("R2 bucket access is unavailable")

type upstreamAuthenticationError struct {
	cause error
	code  string
}

func (e *upstreamAuthenticationError) Error() string        { return Diagnostic(e) }
func (e *upstreamAuthenticationError) Unwrap() error        { return e.cause }
func (e *upstreamAuthenticationError) Is(target error) bool { return target == ErrR2Authentication }

type WriteRecoveryError struct{ Detail string }

func (e *WriteRecoveryError) Error() string {
	return "该路径有未完成的 R2 写入，正在等待恢复。请先修正 R2 凭证或网络，再在 R2 存储的维护页执行恢复状态。" + e.Detail
}
func (e *WriteRecoveryError) Is(target error) bool {
	return target == ErrWriteRecoveryRequired || target == ErrWriteInProgress
}

// Diagnostic returns only controlled text; upstream errors may contain signed URLs or credentials.
func Diagnostic(err error) string {
	var auth *upstreamAuthenticationError
	if errors.As(err, &auth) {
		if auth.code == "SignatureDoesNotMatch" {
			return "R2 S3 签名校验失败（403 / SignatureDoesNotMatch）。请重新填写同一次生成的 Access Key ID 和 Secret Access Key，并核对 Cloudflare Account ID。API Token 检测通过不代表这组密钥有效。"
		}
		return "R2 S3 拒绝访问（401/403）。请检查 Access Key ID / Secret Access Key 是否有效，以及该密钥是否允许访问目标桶和执行当前操作。"
	}
	var recovery *WriteRecoveryError
	if errors.As(err, &recovery) {
		return recovery.Error()
	}
	switch {
	case errors.Is(err, ErrQuotaExceeded):
		return "R2 阵列没有满足容量或操作额度限制的可写桶，请检查容量、额度和存储桶配置。"
	case errors.Is(err, ErrBucketDeleting):
		return "目标桶正在删除，暂时无法访问。"
	case errors.Is(err, ErrBucketUnavailable):
		return "目标桶的 S3 访问检测未通过或尚未完成。请检查 R2 密钥，并在存储桶页面重新执行扫描。"
	case errors.Is(err, ErrR2CredentialsRequired):
		return "未配置 R2 Access Key ID / Secret Access Key，请在账号页面补充成对的 S3 密钥。"
	case errors.Is(err, ErrWriteInProgress):
		return "该路径正在执行另一项写入，请稍后重试。"
	case errors.Is(err, ErrRateLimited):
		return "R2 请求频率受限，请稍后重试。"
	case errors.Is(err, ErrWriteRecoveryAmbiguous):
		return "无法确认远端写入结果，已保留恢复记录，请检查远端对象后执行恢复状态。"
	default:
		return "R2 操作未完成，无法确认远端状态。请检查服务器网络、R2 凭证与存储桶状态后执行恢复状态。"
	}
}

// RecoveryPendingError describes fenced objects which can be repaired while the console is serving.
type RecoveryPendingError struct{ Failures []error }

func (e *RecoveryPendingError) Error() string {
	return fmt.Sprintf("%d 项 R2 写入尚未恢复：%v", len(e.Failures), errors.Join(e.Failures...))
}
func (e *RecoveryPendingError) Unwrap() []error { return e.Failures }
