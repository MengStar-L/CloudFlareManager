import { Fragment, FormEvent, useEffect, useState } from "react";
import { Boxes, ChevronRight, Gauge, LoaderCircle, Network, Play, Plus, ScrollText, Send, Trash2 } from "lucide-react";
import { api } from "../api";
import type { Account } from "../types";
import { Empty, ErrorBanner, NoAccountHint, PageHeader, RefreshButton, Segmented, Status } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { SelectField } from "../components/SelectField";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

interface CredentialUsage {
  credential_id: string;
  name: string;
  status: "active" | "revoked" | "deleted" | "unattributed";
  estimated_used_neurons: number;
  requests: number;
  errors: number;
}
interface AccountUsage {
  account_id: string;
  account_name: string;
  estimated_used_neurons: number;
  estimated_remaining_neurons: number;
  requests: number;
  errors: number;
  credentials: CredentialUsage[];
}
interface DailyUsageReport {
  date: string;
  timezone: "UTC";
  daily_limit_neurons: number;
  estimated: true;
  accounts: AccountUsage[];
}
interface RequestLog { id: string; account_id: string; model: string; status_code: number; input_tokens: number; output_tokens: number; estimated_neurons: number; duration_ms: number; error_class?: string; created_at: string }
type AIModel = Record<string, unknown>;
type Gateway = Record<string, unknown>;
type AITab = "playground" | "models" | "usage" | "logs" | "gateways";

export function AIPage() {
  const [tab, setTab] = useState<AITab>("playground");
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [usageDate, setUsageDate] = useState(todayUTC());
  const [usage, setUsage] = useState<DailyUsageReport>(() => emptyUsage(todayUTC()));
  const [usageLoading, setUsageLoading] = useState(true);
  const [expandedAccounts, setExpandedAccounts] = useState<Set<string>>(() => new Set());
  const [logs, setLogs] = useState<RequestLog[]>([]);
  const [models, setModels] = useState<AIModel[]>([]);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  // 不写死默认模型（Cloudflare 会弃用旧模型），目录加载后自动选第一个文本生成模型。
  const [model, setModel] = useState("");
  const [prompt, setPrompt] = useState("");
  const [output, setOutput] = useState("");
  const [error, setError] = useState("");
  const [running, setRunning] = useState(false);
  const [creating, setCreating] = useState(false);
  const [loading, setLoading] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<Gateway | null>(null);
  const toast = useToast();

  async function load() {
    const [accountData, usageData, logData, modelData, gatewayData] = await Promise.allSettled([
      api.get<{ accounts: Account[] }>("/api/v1/accounts"),
      api.get<DailyUsageReport>(`/api/v1/ai/usage?date=${encodeURIComponent(usageDate)}`),
      api.get<{ logs: RequestLog[] }>("/api/v1/ai/logs?limit=200"),
      api.get<{ models: AIModel[] }>("/api/v1/ai/models"),
      api.get<{ gateways: Gateway[]; warnings?: string[] }>("/api/v1/ai/gateways"),
    ]);
    let aiAccounts: Account[] = [];
    if (accountData.status === "fulfilled") {
      aiAccounts = (accountData.value.accounts ?? []).filter((account) => account.capabilities?.some((capability) => capability.name === "ai" && capability.available));
      setAccounts(aiAccounts);
    }
    if (usageData.status === "fulfilled") setUsage(usageData.value);
    setUsageLoading(false);
    if (logData.status === "fulfilled") setLogs(logData.value.logs ?? []);
    if (modelData.status === "fulfilled") {
      const catalog = modelData.value.models ?? [];
      setModels(catalog);
      const names = catalog.map(modelName).filter(Boolean);
      setModel((current) => current && names.includes(current) ? current : defaultModel(catalog));
    }
    let gatewayWarning = "";
    if (gatewayData.status === "fulfilled") {
      setGateways(gatewayData.value.gateways ?? []);
      if (gatewayData.value.warnings?.length) gatewayWarning = `部分账号的 Gateway 列表获取失败：${gatewayData.value.warnings.join("；")}`;
    }
    const critical = [accountData, usageData, logData].filter((item): item is PromiseRejectedResult => item.status === "rejected");
    const catalog = [modelData, gatewayData].filter((item): item is PromiseRejectedResult => item.status === "rejected");
    if (critical.length > 0) setError((critical[0].reason as Error).message);
    else if (catalog.length > 0 && aiAccounts.length > 0) setError((catalog[0].reason as Error).message);
    else if (gatewayWarning) setError(gatewayWarning);
    setLoading(false);
  }

  async function loadUsage(date: string) {
    setUsageLoading(true);
    try {
      setUsage(await api.get<DailyUsageReport>(`/api/v1/ai/usage?date=${encodeURIComponent(date)}`));
    } catch (reason) {
      setError((reason as Error).message);
    } finally {
      setUsageLoading(false);
    }
  }

  function toggleAccount(accountID: string) {
    setExpandedAccounts((current) => {
      const next = new Set(current);
      if (next.has(accountID)) next.delete(accountID);
      else next.add(accountID);
      return next;
    });
  }

  useEffect(() => { void load(); }, []);

  async function run() {
    setRunning(true); setError(""); setOutput("");
    try {
      const data = await api.post<Record<string, unknown>>("/api/v1/ai/playground", {
        model, messages: [{ role: "user", content: prompt }], stream: false,
      });
      setOutput(JSON.stringify(data, null, 2));
      await load();
    } catch (reason) {
      await load();
      setError((reason as Error).message);
    } finally { setRunning(false); }
  }

  async function createGateway(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const data = new FormData(formElement);
    const id = String(data.get("id") ?? "").trim();
    if (!id || creating) return;
    setCreating(true);
    try {
      await api.post("/api/v1/ai/gateways", { gateway: { id } });
      formElement.reset();
      toast.show("AI Gateway 已创建");
      await load();
    } catch (reason) { setError((reason as Error).message); }
    finally { setCreating(false); }
  }

  async function deleteGateway(gateway: Gateway) {
    const id = String(gateway.id ?? "");
    if (!id) return;
    const owner = typeof gateway.manager_account_id === "string" ? gateway.manager_account_id : "";
    try {
      await api.delete(`/api/v1/ai/gateways/${encodeURIComponent(id)}${owner ? `?account_id=${encodeURIComponent(owner)}` : ""}`);
      toast.show("AI Gateway 已删除");
      await load();
    } catch (reason) { setError((reason as Error).message); throw reason; }
  }

  // 选项顺序保持稳定；已被服务端过滤的当前值不再追加回列表。
  const modelNames = [...new Set(models.map(modelName).filter(Boolean))];

  return (
    <>
      <PageHeader
        title="Workers AI"
        actions={<RefreshButton onRefresh={load} />}
        tabs={<Segmented
          className="ai-tabs"
          label="Workers AI 视图"
          value={tab}
          onChange={setTab}
          items={[
            { id: "playground", label: "Playground", icon: <Play size={15} /> },
            { id: "models", label: "模型", icon: <Boxes size={15} /> },
            { id: "usage", label: "用量额度", icon: <Gauge size={15} /> },
            { id: "logs", label: "日志", icon: <ScrollText size={15} /> },
            { id: "gateways", label: "AI Gateway", icon: <Network size={15} /> },
          ]}
        />}
      />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}

      <Reveal key={tab}>{tab === "playground" && <section className="playground">
        <div className="playground-input">
          <SelectField label="模型" value={model} onChange={setModel} searchable disabled={modelNames.length === 0} options={modelNames.map((name) => ({ value: name, label: name }))} />
          <label>消息<textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} rows={10} /></label>
          <button className="primary" onClick={() => void run()} disabled={!model || !prompt.trim() || running}>{running ? <LoaderCircle className="spin" size={16} /> : <Send size={16} />}发送</button>
        </div>
        <pre className="playground-output">{running ? "正在请求模型…" : output || "执行结果将显示在这里。"}</pre>
      </section>}

      {tab === "models" && (!loading && accounts.length === 0 ? <NoAccountHint /> : <section className="panel">{loading ? <TableSkeleton columns={4} /> : models.length === 0 ? <Empty>暂无模型</Empty> : <div className="table-wrap"><table><thead><tr><th>模型</th><th>任务</th><th>描述</th><th>操作</th></tr></thead><tbody>{models.map((item, index) => <tr key={`${modelName(item)}-${index}`}><td className="mono">{modelName(item)}</td><td>{modelTask(item)}</td><td>{String(item.description ?? "-")}</td><td><button className="icon-button" title="在 Playground 使用" onClick={() => { setModel(modelName(item)); setTab("playground"); }}><Play size={14} /></button></td></tr>)}</tbody></table></div>}</section>)}

      {tab === "usage" && <section className="panel ai-usage-panel">
        <div className="ai-usage-toolbar">
          <label>统计日期（UTC）<input type="date" value={usageDate} onChange={(event) => {
            const date = event.target.value;
            if (!date) return;
            setUsageDate(date);
            void loadUsage(date);
          }} /></label>
          <div className="notice">本地估算，非 Cloudflare 官方账单；每日 00:00 UTC 重置。</div>
        </div>
        {usageLoading ? <TableSkeleton columns={6} /> : usage.accounts.length === 0 ? <Empty>暂无可用 AI 账号</Empty> : <div className="table-wrap"><table className="ai-usage-table">
          <thead><tr><th>账号</th><th>每日额度</th><th>估算已用</th><th>估算剩余</th><th>使用进度</th><th>请求 / 错误</th></tr></thead>
          <tbody>{usage.accounts.map((item) => {
            const expanded = expandedAccounts.has(item.account_id);
            const percent = usage.daily_limit_neurons > 0 ? Math.min(100, item.estimated_used_neurons / usage.daily_limit_neurons * 100) : 0;
            const exceeded = item.estimated_used_neurons > usage.daily_limit_neurons;
            return <Fragment key={item.account_id}>
              <tr className="ai-account-row">
                <td data-label="账号"><button className="ai-account-toggle" onClick={() => toggleAccount(item.account_id)} aria-expanded={expanded} title={expanded ? "收起密钥明细" : "展开密钥明细"}><ChevronRight size={16} /><span><strong>{item.account_name}</strong><small>{item.account_id}</small></span></button></td>
                <td data-label="每日额度">{formatNeurons(usage.daily_limit_neurons)}</td>
                <td data-label="估算已用">{formatNeurons(item.estimated_used_neurons)}</td>
                <td data-label="估算剩余"><strong>{formatNeurons(item.estimated_remaining_neurons)}</strong>{exceeded && <small className="ai-quota-exceeded">已超出 {formatNeurons(item.estimated_used_neurons - usage.daily_limit_neurons)}</small>}</td>
                <td data-label="使用进度"><div className="ai-usage-progress"><span style={{ width: `${percent}%` }} /></div><small>{percent.toFixed(1)}%</small></td>
                <td data-label="请求 / 错误">{item.requests.toLocaleString()} / {item.errors.toLocaleString()}</td>
              </tr>
              {expanded && <tr className="ai-key-detail-row"><td colSpan={6}><div className="ai-key-details">
                <p>所有 AI 访问密钥共享此账号的 {formatNeurons(item.estimated_remaining_neurons)} 估算剩余额度。</p>
                {item.credentials.length === 0 ? <Empty>当天暂无密钥调用</Empty> : <div className="table-wrap"><table>
                  <thead><tr><th>访问密钥</th><th>状态</th><th>估算消耗</th><th>占账号消耗</th><th>请求 / 错误</th></tr></thead>
                  <tbody>{item.credentials.map((credential) => {
                    const share = item.estimated_used_neurons > 0 ? credential.estimated_used_neurons / item.estimated_used_neurons * 100 : 0;
                    return <tr key={credential.credential_id}>
                      <td data-label="访问密钥"><strong>{credential.name}</strong><small>{credential.credential_id === "unattributed" ? "-" : credential.credential_id}</small></td>
                      <td data-label="状态"><Status value={credentialStatusTone(credential.status)} label={credentialStatusLabel(credential.status)} /></td>
                      <td data-label="估算消耗">{formatNeurons(credential.estimated_used_neurons)}</td>
                      <td data-label="占账号消耗">{share.toFixed(1)}%</td>
                      <td data-label="请求 / 错误">{credential.requests.toLocaleString()} / {credential.errors.toLocaleString()}</td>
                    </tr>;
                  })}</tbody>
                </table></div>}
              </div></td></tr>}
            </Fragment>;
          })}</tbody>
        </table></div>}
      </section>}

      {tab === "logs" && <section className="panel">{loading ? <TableSkeleton columns={7} /> : logs.length === 0 ? <Empty>暂无请求日志</Empty> : <div className="table-wrap"><table><thead><tr><th>时间</th><th>账号</th><th>模型</th><th>状态</th><th>输入 / 输出</th><th>估算 Neurons</th><th>耗时</th></tr></thead><tbody>{logs.map((item) => <tr key={item.id}><td>{new Date(item.created_at).toLocaleString()}</td><td>{accounts.find((account) => account.id === item.account_id)?.name ?? item.account_id}</td><td className="mono">{item.model || "-"}</td><td><Status value={item.status_code >= 400 ? "error" : "success"} /></td><td>{item.input_tokens.toLocaleString()} / {item.output_tokens.toLocaleString()}</td><td>{item.estimated_neurons.toFixed(3)}</td><td>{item.duration_ms.toFixed(1)} ms</td></tr>)}</tbody></table></div>}</section>}

      {tab === "gateways" && (!loading && accounts.length === 0 ? <NoAccountHint /> : <>
        <div className="notice">Gateway 会自动创建在可用的 AI 账号下，对外调用统一走本程序的 OpenAI 兼容接口，无需选择账号。</div>
        <form className="form-band inline-form gateway-form" onSubmit={createGateway}><label>Gateway ID<input name="id" className="mono" required /></label><button className="primary" disabled={creating}><Plus size={15} />创建</button></form>
        <section className="panel">{loading ? <TableSkeleton columns={4} /> : gateways.length === 0 ? <Empty>暂无 AI Gateway</Empty> : <div className="table-wrap"><table><thead><tr><th>名称</th><th>ID</th><th>归属账号</th><th>状态</th><th>操作</th></tr></thead><tbody>
          {gateways.map((gateway, index) => <tr key={`${String(gateway.manager_account_id ?? "")}-${String(gateway.id ?? index)}`}><td>{String(gateway.name ?? gateway.id ?? "-")}</td><td className="mono">{String(gateway.id ?? "-")}</td><td>{String(gateway.manager_account_name ?? "-")}</td><td><Status value={String(gateway.status ?? "available")} /></td><td><button className="icon-button danger" title="删除 Gateway" onClick={() => setDeleteTarget(gateway)}><Trash2 size={14} /></button></td></tr>)}
        </tbody></table></div>}</section>
      </>)}</Reveal>
      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title="删除 AI Gateway"
        description={`删除“${String(deleteTarget?.id ?? "")}”及其 Cloudflare Gateway 配置？`}
        confirmLabel="删除 Gateway"
        onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        onConfirm={() => deleteTarget ? deleteGateway(deleteTarget) : Promise.resolve()}
      />
    </>
  );
}

function modelName(model: AIModel) {
  return String(model.name ?? model.id ?? "");
}

function defaultModel(catalog: AIModel[]) {
  const chat = catalog.find((item) => modelTask(item) === "Text Generation");
  return modelName(chat ?? catalog[0] ?? {});
}

function modelTask(model: AIModel) {
  const task = model.task;
  if (task && typeof task === "object" && "name" in task) return String((task as { name: unknown }).name);
  return String(task ?? "-");
}

function todayUTC() {
  return new Date().toISOString().slice(0, 10);
}

function emptyUsage(date: string): DailyUsageReport {
  return { date, timezone: "UTC", daily_limit_neurons: 10_000, estimated: true, accounts: [] };
}

function formatNeurons(value: number) {
  return Number(value || 0).toLocaleString(undefined, { maximumFractionDigits: 2 });
}

function credentialStatusLabel(status: CredentialUsage["status"]) {
  return { active: "启用", revoked: "已撤销", deleted: "已删除", unattributed: "未归属" }[status];
}

function credentialStatusTone(status: CredentialUsage["status"]) {
  return { active: "available", revoked: "disabled", deleted: "error", unattributed: "pending" }[status];
}
