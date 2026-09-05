import type { Account } from "../types";

const capabilityNames: Record<string, string> = {
  api_token: "API Token 验证",
  r2: "R2 桶列表读取",
  d1: "D1 数据库列表读取",
  ai: "Workers AI 模型列表读取",
  analytics: "账号分析查询",
};

export function capabilityName(name: string) {
  return capabilityNames[name] ?? name;
}

export function accountHealthLabel(status: string) {
  return ({ healthy: "正常", error: "检测失败", degraded: "部分不可用", unknown: "待检测" } as Record<string, string>)[status] ?? status;
}

function diagnosticDetail(detail: string) {
  // Older saved checks contain only HTTP status text; explain it without inventing missing diagnostics.
  if (detail === "Unauthorized") {
    return "Cloudflare 身份验证未通过（HTTP 401 / Unauthorized）。请检查 API Token 是否复制完整、过期或被撤销，以及 Token 的 IP 限制。API Token 不能用 Global API Key 或 R2 Secret Access Key 代替。旧检测记录未保存接口和错误码，请重新检测获取详情。";
  }
  if (detail === "Forbidden") {
    return "Cloudflare 拒绝访问（HTTP 403 / Forbidden）。请检查 API Token 的权限、账号资源范围是否包含当前 Cloudflare Account ID，以及 IP 限制。旧检测记录未保存接口和错误码，请重新检测获取详情。";
  }
  if (detail === "one or more Cloudflare capabilities are unavailable") {
    return "部分 Cloudflare 能力检测未通过，请查看各项失败详情，检查对应权限与账号资源范围。";
  }
  return detail;
}

export function AccountDiagnostics({ account }: { account: Account }) {
  const failed = account.capabilities?.filter((capability) => !capability.available) ?? [];
  if (!account.health_error && failed.length === 0) return null;

  const primary = account.health_error || failed[0]?.detail || "能力检测未通过，尚未返回具体原因，请重新检测。";
  const remaining = failed.filter((capability) => capability.detail !== primary);
  return (
    <tr className="account-diagnostics-row">
      <td colSpan={5}>
        <div className="account-diagnostics" aria-label={`${account.name} 的检测详情`}>
          <p className="account-diagnostic-primary">{diagnosticDetail(primary)}</p>
          {remaining.length > 0 && <details>
            <summary>{remaining.length} 项失败检查详情</summary>
            <dl>{remaining.map((capability) => <div key={capability.name}>
              <dt>{capabilityName(capability.name)}</dt>
              <dd>{diagnosticDetail(capability.detail || "未返回具体原因，请重新检测获取详情。")}</dd>
            </div>)}</dl>
          </details>}
        </div>
      </td>
    </tr>
  );
}
