import { FormEvent, useEffect, useState } from "react";
import { Boxes, Gauge, LoaderCircle, Network, Play, Plus, ScrollText, Send, Trash2 } from "lucide-react";
import { api } from "../api";
import type { Account } from "../types";
import { Empty, ErrorBanner, NoAccountHint, PageHeader, RefreshButton, Segmented, Status } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { SelectField } from "../components/SelectField";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

interface Usage { account_id: string; date: string; estimated_neurons: number; input_tokens: number; output_tokens: number; requests: number; errors: number }
interface RequestLog { id: string; account_id: string; model: string; status_code: number; input_tokens: number; output_tokens: number; estimated_neurons: number; duration_ms: number; error_class?: string; created_at: string }
type AIModel = Record<string, unknown>;
type Gateway = Record<string, unknown>;
type AITab = "playground" | "models" | "usage" | "logs" | "gateways";

export function AIPage() {
  const [tab, setTab] = useState<AITab>("playground");
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [accountID, setAccountID] = useState("");
  const [usage, setUsage] = useState<Usage[]>([]);
  const [logs, setLogs] = useState<RequestLog[]>([]);
  const [models, setModels] = useState<AIModel[]>([]);
  const [gateways, setGateways] = useState<Gateway[]>([]);
  const [model, setModel] = useState("@cf/meta/llama-3.1-8b-instruct");
  const [prompt, setPrompt] = useState("");
  const [output, setOutput] = useState("");
  const [error, setError] = useState("");
  const [running, setRunning] = useState(false);
  const [creating, setCreating] = useState(false);
  const [loading, setLoading] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<Gateway | null>(null);
  const toast = useToast();

  async function load() {
    const [accountData, usageData, logData] = await Promise.allSettled([
      api.get<{ accounts: Account[] }>("/api/v1/accounts"),
      api.get<{ usage: Usage[] }>("/api/v1/ai/usage"),
      api.get<{ logs: RequestLog[] }>("/api/v1/ai/logs?limit=200"),
    ]);
    if (accountData.status === "fulfilled") {
      const aiAccounts = (accountData.value.accounts ?? []).filter((account) => account.capabilities?.some((capability) => capability.name === "ai" && capability.available));
      setAccounts(aiAccounts);
      setAccountID((current) => aiAccounts.some((account) => account.id === current) ? current : (aiAccounts[0]?.id ?? ""));
    }
    if (usageData.status === "fulfilled") setUsage(usageData.value.usage ?? []);
    if (logData.status === "fulfilled") setLogs(logData.value.logs ?? []);
    const failed = [accountData, usageData, logData].filter((item): item is PromiseRejectedResult => item.status === "rejected");
    if (failed.length > 0) setError((failed[0].reason as Error).message);
    setLoading(false);
  }

  async function loadAccountDetails(id: string) {
    if (!id) { setModels([]); setGateways([]); return; }
    try {
      const [modelData, gatewayData] = await Promise.all([
        api.get<{ models: AIModel[] }>(`/api/v1/ai/models?account_id=${encodeURIComponent(id)}`),
        api.get<{ gateways: Gateway[] }>(`/api/v1/ai/gateways?account_id=${encodeURIComponent(id)}`),
      ]);
      setModels(modelData.models ?? []); setGateways(gatewayData.gateways ?? []);
    } catch (reason) { setError((reason as Error).message); }
  }

  useEffect(() => { void load(); }, []);
  useEffect(() => { void loadAccountDetails(accountID); }, [accountID]);

  async function run() {
    setRunning(true); setError(""); setOutput("");
    try {
      const data = await api.post<Record<string, unknown>>("/api/v1/ai/playground", {
        model, messages: [{ role: "user", content: prompt }], stream: false,
      });
      setOutput(JSON.stringify(data, null, 2));
      await load();
    } catch (reason) { setError((reason as Error).message); } finally { setRunning(false); }
  }

  async function createGateway(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const data = new FormData(formElement);
    const id = String(data.get("id") ?? "").trim();
    if (!id || !accountID || creating) return;
    setCreating(true);
    try {
      await api.post("/api/v1/ai/gateways", { account_id: accountID, gateway: { id, collect_logs: true } });
      formElement.reset();
      toast.show("AI Gateway 已创建");
      await loadAccountDetails(accountID);
    } catch (reason) { setError((reason as Error).message); }
    finally { setCreating(false); }
  }

  async function deleteGateway(gateway: Gateway) {
    const id = String(gateway.id ?? "");
    if (!id) return;
    try {
      await api.delete(`/api/v1/ai/gateways/${encodeURIComponent(id)}?account_id=${encodeURIComponent(accountID)}`);
      toast.show("AI Gateway 已删除");
      await loadAccountDetails(accountID);
    } catch (reason) { setError((reason as Error).message); throw reason; }
  }

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
            { id: "usage", label: "用量", icon: <Gauge size={15} /> },
            { id: "logs", label: "日志", icon: <ScrollText size={15} /> },
            { id: "gateways", label: "AI Gateway", icon: <Network size={15} /> },
          ]}
        />}
      />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}

      <Reveal key={tab}>{tab === "playground" && <section className="playground">
        <div className="playground-input">
          <SelectField label="模型" value={model} onChange={setModel} searchable options={[...new Set([model, ...models.map(modelName)])].filter(Boolean).map((name) => ({ value: name, label: name }))} />
          <label>消息<textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} rows={10} /></label>
          <button className="primary" onClick={() => void run()} disabled={!prompt.trim() || running}>{running ? <LoaderCircle className="spin" size={16} /> : <Send size={16} />}发送</button>
        </div>
        <pre className="playground-output">{running ? "正在请求模型…" : output || "执行结果将显示在这里。"}</pre>
      </section>}

      {tab === "models" && (!loading && accounts.length === 0 ? <NoAccountHint /> : <>
        <AccountSelector accounts={accounts} accountID={accountID} onChange={setAccountID} />
        <section className="panel">{loading ? <TableSkeleton columns={4} /> : models.length === 0 ? <Empty>暂无模型</Empty> : <div className="table-wrap"><table><thead><tr><th>模型</th><th>任务</th><th>描述</th><th>操作</th></tr></thead><tbody>{models.map((item, index) => <tr key={`${modelName(item)}-${index}`}><td className="mono">{modelName(item)}</td><td>{modelTask(item)}</td><td>{String(item.description ?? "-")}</td><td><button className="icon-button" title="在 Playground 使用" onClick={() => { setModel(modelName(item)); setTab("playground"); }}><Play size={14} /></button></td></tr>)}</tbody></table></div>}</section>
      </>)}

      {tab === "usage" && <section className="panel"><div className="notice">Neuron 数值为本地估算，不是 Cloudflare 官方账单。</div>{loading ? <TableSkeleton columns={6} /> : usage.length === 0 ? <Empty>暂无 AI 用量</Empty> : <div className="table-wrap"><table>
        <thead><tr><th>日期</th><th>账号</th><th>估算 Neurons</th><th>输入 / 输出 tokens</th><th>请求</th><th>错误</th></tr></thead>
        <tbody>{usage.map((item) => <tr key={`${item.account_id}-${item.date}`}><td>{item.date}</td><td>{accounts.find((account) => account.id === item.account_id)?.name ?? item.account_id}</td><td>{item.estimated_neurons.toFixed(3)}</td><td>{item.input_tokens.toLocaleString()} / {item.output_tokens.toLocaleString()}</td><td>{item.requests}</td><td>{item.errors}</td></tr>)}</tbody>
      </table></div>}</section>}

      {tab === "logs" && <section className="panel">{loading ? <TableSkeleton columns={7} /> : logs.length === 0 ? <Empty>暂无请求日志</Empty> : <div className="table-wrap"><table><thead><tr><th>时间</th><th>账号</th><th>模型</th><th>状态</th><th>输入 / 输出</th><th>估算 Neurons</th><th>耗时</th></tr></thead><tbody>{logs.map((item) => <tr key={item.id}><td>{new Date(item.created_at).toLocaleString()}</td><td>{accounts.find((account) => account.id === item.account_id)?.name ?? item.account_id}</td><td className="mono">{item.model || "-"}</td><td><Status value={item.status_code >= 400 ? "error" : "success"} /></td><td>{item.input_tokens.toLocaleString()} / {item.output_tokens.toLocaleString()}</td><td>{item.estimated_neurons.toFixed(3)}</td><td>{item.duration_ms.toFixed(1)} ms</td></tr>)}</tbody></table></div>}</section>}

      {tab === "gateways" && (!loading && accounts.length === 0 ? <NoAccountHint /> : <>
        <AccountSelector accounts={accounts} accountID={accountID} onChange={setAccountID} />
        <form className="form-band inline-form gateway-form" onSubmit={createGateway}><label>Gateway ID<input name="id" className="mono" required /></label><button className="primary" disabled={!accountID || creating}><Plus size={15} />创建</button></form>
        <section className="panel">{loading ? <TableSkeleton columns={4} /> : gateways.length === 0 ? <Empty>暂无 AI Gateway</Empty> : <div className="table-wrap"><table><thead><tr><th>名称</th><th>ID</th><th>状态</th><th>操作</th></tr></thead><tbody>
          {gateways.map((gateway, index) => <tr key={String(gateway.id ?? index)}><td>{String(gateway.name ?? gateway.id ?? "-")}</td><td className="mono">{String(gateway.id ?? "-")}</td><td><Status value={String(gateway.status ?? "available")} /></td><td><button className="icon-button danger" title="删除 Gateway" onClick={() => setDeleteTarget(gateway)}><Trash2 size={14} /></button></td></tr>)}
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

function AccountSelector({ accounts, accountID, onChange }: { accounts: Account[]; accountID: string; onChange: (id: string) => void }) {
  return <div className="context-bar"><SelectField label="账号" value={accountID} onChange={onChange} options={accounts.map((account) => ({ value: account.id, label: account.name }))} placeholder="选择账号" /></div>;
}

function modelName(model: AIModel) {
  return String(model.name ?? model.id ?? "");
}

function modelTask(model: AIModel) {
  const task = model.task;
  if (task && typeof task === "object" && "name" in task) return String((task as { name: unknown }).name);
  return String(task ?? "-");
}
