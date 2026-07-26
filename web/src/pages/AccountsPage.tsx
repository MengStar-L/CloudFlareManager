import { FormEvent, useEffect, useState } from "react";
import { Plus, ShieldCheck, Trash2, X } from "lucide-react";
import { api } from "../api";
import type { Account } from "../types";
import { Empty, ErrorBanner, PageHeader, RefreshButton, Status } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

export function AccountsPage() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [showForm, setShowForm] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<Account | null>(null);
  const toast = useToast();

  async function load() {
    try {
      const data = await api.get<{ accounts: Account[] }>("/api/v1/accounts");
      setAccounts(data.accounts ?? []);
    } catch (reason) { setError((reason as Error).message); }
    finally { setLoading(false); }
  }
  useEffect(() => { void load(); }, []);

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    const form = new FormData(event.currentTarget);
    try {
      await api.post("/api/v1/accounts", {
        name: form.get("name"), cloudflare_account_id: form.get("account_id"), api_token: form.get("api_token"),
        r2_access_key_id: form.get("r2_access_key"), r2_secret_access_key: form.get("r2_secret_key"),
      });
      setShowForm(false);
      toast.show("账号已添加，能力检测任务已创建");
      await load();
    } catch (reason) { setError((reason as Error).message); } finally { setBusy(false); }
  }

  async function remove(account: Account) {
    try { await api.delete(`/api/v1/accounts/${account.id}`); toast.show("账号已删除"); await load(); }
    catch (reason) { setError((reason as Error).message); throw reason; }
  }

  async function redetect(account: Account) {
    try {
      await api.post(`/api/v1/accounts/${account.id}/verify`, {});
      toast.show("能力检测任务已创建，稍后刷新查看结果", "info");
    } catch (reason) { setError((reason as Error).message); }
  }

  return (
    <>
      <PageHeader title="Cloudflare 账号" actions={<>
        <RefreshButton onRefresh={load} />
        <button className="primary" onClick={() => setShowForm(!showForm)}>{showForm ? <X size={16} /> : <Plus size={16} />}{showForm ? "取消" : "添加账号"}</button>
      </>} />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      {showForm && <Reveal><section className="panel form-panel">
        <div className="panel-heading"><h2>添加 Cloudflare 账号</h2></div>
        <form className="panel-form account-form" onSubmit={create}>
          <div className="form-grid">
            <label>显示名称<input name="name" placeholder="例如：主账号" required /></label>
            <label>Cloudflare Account ID<input name="account_id" className="mono" placeholder="在 Cloudflare 概览页右侧可复制" required /></label>
            <label className="field-token">API Token
              <input name="api_token" type="password" autoComplete="off" required />
              <small className="field-hint">自定义 Token 建议勾选：Workers R2 Storage Read、D1 Edit、Workers AI Read+Edit、AI Gateway Read+Edit、Account Analytics Read。保存后会自动检测各项能力。</small>
            </label>
            <label>R2 Access Key ID（可选）<input name="r2_access_key" autoComplete="off" /></label>
            <label>R2 Secret Access Key（可选）
              <input name="r2_secret_key" type="password" autoComplete="off" />
              <small className="field-hint">在 R2 → Manage R2 API Tokens 创建（Object Read &amp; Write）。留空时 D1 与 AI 功能不受影响，仅 R2 对象操作不可用。</small>
            </label>
            <div className="form-actions"><button className="primary" disabled={busy} type="submit"><Plus size={16} />保存并检测</button></div>
          </div>
        </form>
      </section></Reveal>}
      <section className="panel">
        {loading ? <TableSkeleton columns={5} /> : accounts.length === 0 ? <Empty>
          <div className="no-account-hint">
            <p>暂无 Cloudflare 账号</p>
            {!showForm && <button type="button" className="primary" onClick={() => setShowForm(true)}><Plus size={15} />添加第一个账号</button>}
          </div>
        </Empty> : <div className="table-wrap"><table>
          <thead><tr><th>名称</th><th>Account ID</th><th>健康</th><th>能力</th><th aria-label="操作" /></tr></thead>
          <tbody>{accounts.map((account) => <tr key={account.id}>
            <td><strong>{account.name}</strong>{account.health_error && <small className="danger-text">{account.health_error}</small>}</td>
            <td className="mono">{account.cloudflare_account_id}</td>
            <td><Status value={account.health_status} /></td>
            <td><div className="capabilities">{account.capabilities?.map((capability) => <span className={capability.available ? "enabled" : "disabled"} key={capability.name}>{capability.name}</span>)}</div></td>
            <td className="row-actions"><button className="icon-button" onClick={() => void redetect(account)} title="重新检测能力"><ShieldCheck size={15} /></button><button className="icon-button danger" onClick={() => setDeleteTarget(account)} title="删除账号"><Trash2 size={15} /></button></td>
          </tr>)}</tbody>
        </table></div>}
      </section>
      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title="删除 Cloudflare 账号"
        description={`删除“${deleteTarget?.name ?? ""}”及其本地凭据？已登记的资源映射不会被静默转移。`}
        confirmLabel="删除账号"
        onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        onConfirm={() => deleteTarget ? remove(deleteTarget) : Promise.resolve()}
      />
    </>
  );
}
