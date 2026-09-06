package r2

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

// DetectAccountAccess verifies the separate S3 credentials without changing remote objects.
func (s Service) DetectAccountAccess(ctx context.Context, account accounts.Account) []accounts.Capability {
	check := accounts.Capability{Name: "r2_s3", CheckedAt: time.Now()}
	if !account.HasR2Credentials {
		check.Detail = "未配置 R2 S3 密钥，对象访问尚未验证。需要使用文件或 WebDAV 功能时，请补充 Access Key ID / Secret Access Key。"
		return []accounts.Capability{check}
	}
	buckets, err := s.Index.ListBuckets(ctx)
	var names []string
	if err == nil {
		for _, bucket := range buckets {
			if bucket.AccountID == account.ID && bucket.LifecycleState == BucketActive {
				names = append(names, bucket.Name)
			}
		}
	}
	if err == nil && len(names) == 0 {
		var remote []accounts.RemoteBucket
		remote, err = (accounts.RemoteClient{}).R2Buckets(ctx, account.CloudflareAccountID, account.APIToken)
		for _, bucket := range remote {
			names = append(names, bucket.Name)
		}
	}
	if err != nil {
		check.Detail = "无法获取待检测的 R2 存储桶，S3 密钥尚未验证。请检查 API Token 或先登记目标桶。"
		return []accounts.Capability{check}
	}
	if len(names) == 0 {
		check.Detail = "当前没有可检测的默认管辖区存储桶，S3 密钥尚未验证。请创建或登记目标桶后重新检测。"
		return []accounts.Capability{check}
	}
	backend, err := s.maintenanceBackend()
	if err != nil {
		check.Detail = Diagnostic(err)
		return []accounts.Capability{check}
	}
	var failures []string
	for _, name := range names {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := backend.ListRemote(probeCtx, Target{AccountID: account.ID, CloudflareAccountID: account.CloudflareAccountID,
			AccessKeyID: account.R2AccessKeyID, SecretAccessKey: account.R2SecretAccessKey, Bucket: name}, "", "", 1)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("桶 %s：%s", name, Diagnostic(err)))
		}
		if ctx.Err() != nil {
			break
		}
	}
	check.Available = len(failures) == 0
	check.Detail = fmt.Sprintf("已验证 %d 个桶的 S3 签名及对象列表读取权限；此检测未写入或删除对象，不代表写入权限已验证。", len(names))
	if !check.Available {
		check.Detail = strings.Join(failures, "\n")
	}
	return []accounts.Capability{check}
}
