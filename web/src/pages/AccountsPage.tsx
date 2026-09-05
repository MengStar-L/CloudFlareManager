import { FormEvent, Fragment, useCallback, useEffect, useRef, useState } from "react";
import { Check, KeyRound, LoaderCircle, Plus, ShieldCheck, Trash2, X } from "lucide-react";
import { APIError, api } from "../api";
import type { Account, BackgroundJob } from "../types";
import { Empty, ErrorBanner, PageHeader, RefreshButton, Status } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { AccountDeleteDialog } from "../components/AccountDeleteDialog";
import { AccountDiagnostics, accountHealthLabel, capabilityName } from "../components/AccountDiagnostics";
import { Reveal } from "../components/Motion";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

interface PendingCredentialUpdate {
  account: Account;
  body: Record<string, unknown>;
}

export function AccountsPage() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [showForm, setShowForm] = useState(false);
  const [error, setError] = useState("");
  const [refreshError, setRefreshError] = useState("");
  const [busy, setBusy] = useState(false);
  const [startingVerification, setStartingVerification] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<Account | null>(null);
  const [editTarget, setEditTarget] = useState<Account | null>(null);
  const [removeR2Credentials, setRemoveR2Credentials] = useState(false);
  const [pendingCredentialUpdate, setPendingCredentialUpdate] = useState<PendingCredentialUpdate | null>(null);
  const firstCredentialInput = useRef<HTMLInputElement>(null);
  const loadEpoch = useRef(0);
  const toast = useToast();

  const load = useCallback(async () => {
    const epoch = ++loadEpoch.current;
    try {
      const data = await api.get<{ accounts: Account[] }>("/api/v1/accounts");
      if (epoch !== loadEpoch.current) return;
      setAccounts(data.accounts ?? []);
      setRefreshError("");
    } catch (reason) {
      if (epoch === loadEpoch.current) setRefreshError((reason as Error).message);
    } finally {
      if (epoch === loadEpoch.current) setLoading(false);
    }
  }, []);
  useEffect(() => {
    void load();
    return () => { ++loadEpoch.current; };
  }, [load]);
  const hasActiveVerification = accounts.some((account) => Boolean(account.verification));
  useEffect(() => {
    if (!hasActiveVerification || busy) return;
    let cancelled = false;
    let timer = 0;
    async function poll() {
      await load();
      if (!cancelled) timer = window.setTimeout(() => void poll(), 1500);
    }
    timer = window.setTimeout(() => void poll(), 1500);
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, [hasActiveVerification, busy, load]);
  useEffect(() => {
    if (!editTarget) return;
    const frame = window.requestAnimationFrame(() => {
      firstCredentialInput.current?.focus();
      firstCredentialInput.current?.scrollIntoView({ block: "center" });
    });
    return () => window.cancelAnimationFrame(frame);
  }, [editTarget]);

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    const form = new FormData(event.currentTarget);
    try {
      const result = await api.post<{ account: Account }>("/api/v1/accounts", {
        name: form.get("name"), cloudflare_account_id: form.get("account_id"), api_token: form.get("api_token"),
        r2_access_key_id: form.get("r2_access_key"), r2_secret_access_key: form.get("r2_secret_key"),
      });
      ++loadEpoch.current;
      setAccounts((current) => [...current.filter((account) => account.id !== result.account.id), result.account]);
      setShowForm(false);
      toast.show("账号已添加，正在自动检测能力");
      await load();
    } catch (reason) { setError((reason as Error).message); } finally { setBusy(false); }
  }

  async function saveCredentials(account: Account, body: Record<string, unknown>) {
    setBusy(true);
    try {
      const result = await api.patch<{ account: Account; verification_scheduled: boolean; warning?: string }>(`/api/v1/accounts/${account.id}/credentials`, body);
      ++loadEpoch.current;
      setAccounts((current) => current.map((item) => item.id === account.id ? result.account : item));
      setEditTarget((current) => current?.id === account.id ? null : current);
      setRemoveR2Credentials(false);
      if (result.warning) {
        toast.show("凭证已更新，但未能创建能力检测任务；请手动重新检测", "error");
      } else if (result.verification_scheduled) {
        toast.show("凭证已更新，正在自动检测能力");
      } else if (body.clear_r2_credentials) {
        toast.show("R2 凭证已移除，文件映射仍保留", "info");
      } else {
        toast.show("R2 凭证已更新（尚未验证），文件映射保持不变", "info");
      }
      await load();
    } catch (reason) {
      if (reason instanceof APIError && (reason.status === 0 || reason.code === "invalid_response")) {
        throw new Error("无法确认凭证是否已更新。请刷新账号列表后再决定是否重试。");
      }
      throw reason;
    } finally { setBusy(false); }
  }

  async function updateCredentials(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    const form = new FormData(event.currentTarget);
    const apiToken = String(form.get("api_token") ?? "").trim();
    const r2AccessKey = String(form.get("r2_access_key") ?? "").trim();
    const r2SecretKey = String(form.get("r2_secret_key") ?? "").trim();
    if (!apiToken && !r2AccessKey && !r2SecretKey && !removeR2Credentials) {
      setError("请填写至少一项新凭证，或选择移除 R2 凭证");
      return;
    }
    if (Boolean(r2AccessKey) !== Boolean(r2SecretKey)) {
      setError("R2 Access Key ID 与 Secret Access Key 必须同时填写");
      return;
    }
    if (!editTarget) return;

    const body: Record<string, unknown> = {};
    if (apiToken) body.api_token = apiToken;
    if (r2AccessKey && r2SecretKey) {
      body.r2_access_key_id = r2AccessKey;
      body.r2_secret_access_key = r2SecretKey;
    }
    if (removeR2Credentials) {
      body.clear_r2_credentials = true;
      setPendingCredentialUpdate({ account: editTarget, body });
      return;
    }
    try { await saveCredentials(editTarget, body); }
    catch (reason) { setError((reason as Error).message); }
  }

  async function remove(account: Account) {
    await api.delete(`/api/v1/accounts/${account.id}`);
    ++loadEpoch.current;
    setAccounts((current) => current.filter((item) => item.id !== account.id));
    setEditTarget((current) => current?.id === account.id ? null : current);
    toast.show("账号已删除");
    await load();
  }

  async function redetect(account: Account) {
    if (account.verification || startingVerification) return;
    setBusy(true);
    setStartingVerification(account.id);
    setError("");
    try {
      const { job } = await api.post<{ job: BackgroundJob }>(`/api/v1/accounts/${account.id}/verify`, {});
      ++loadEpoch.current;
      setAccounts((current) => current.map((item) => item.id === account.id ? {
        ...item, verification: { job_id: job.id, status: job.status === "running" ? "running" : "pending", attempts: job.attempts ?? 0 },
      } : item));
      toast.show("正在重新检测能力", "info");
      await load();
    } catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); setStartingVerification(null); }
  }

  return (
    <>
      <PageHeader title="Cloudflare 账号" actions={<>
        <RefreshButton onRefresh={load} />
        <button className="primary" disabled={busy} onClick={() => { setEditTarget(null); setRemoveR2Credentials(false); setShowForm(!showForm); }}>{showForm ? <X size={16} /> : <Plus size={16} />}{showForm ? "取消" : "添加账号"}</button>
      </>} />
      {(error || refreshError) && <ErrorBanner message={error || refreshError} onClose={() => { setError(""); setRefreshError(""); }} />}
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
            <div className="form-actions"><button className="primary" disabled={busy} type="submit">{busy ? <LoaderCircle size={16} className="spin" /> : <Plus size={16} />}{busy ? "正在添加" : "保存并检测"}</button></div>
          </div>
        </form>
      </section></Reveal>}
      {editTarget && <Reveal key={editTarget.id}><section className="panel form-panel">
        <div className="panel-heading">
          <h2>更新账号凭证</h2>
          <button type="button" className="icon-button" aria-label="关闭凭证表单" title="关闭" disabled={busy} onClick={() => { setEditTarget(null); setRemoveR2Credentials(false); }}><X size={16} /></button>
        </div>
        <form className="panel-form account-form credential-update-form" onSubmit={updateCredentials}>
          <div className="form-grid">
            <label>账号<input value={editTarget.name} readOnly /></label>
            <label>Cloudflare Account ID
              <input className="mono" value={editTarget.cloudflare_account_id} readOnly />
              <small className="field-hint">账号标识保持不变，已有桶和文件映射会继续关联到该账号。</small>
            </label>
            <label className="field-token">新 API Token
              <input ref={firstCredentialInput} name="api_token" type="password" autoComplete="off" placeholder="留空则保持原 Token" disabled={busy} />
            </label>
            <label>新 R2 Access Key ID
              <input name="r2_access_key" autoComplete="off" placeholder="留空则保持原密钥" disabled={busy || removeR2Credentials} />
            </label>
            <label>新 R2 Secret Access Key
              <input name="r2_secret_key" type="password" autoComplete="off" placeholder="需与 Access Key ID 同时填写" disabled={busy || removeR2Credentials} />
            </label>
            {editTarget.has_r2_credentials && <label className="checkbox-label"><input type="checkbox" checked={removeR2Credentials} disabled={busy} onChange={(event) => setRemoveR2Credentials(event.target.checked)} />移除已保存的 R2 凭证</label>}
            <div className="form-actions"><button className="primary" disabled={busy} type="submit"><KeyRound size={16} />更新凭证</button></div>
          </div>
        </form>
      </section></Reveal>}
      <section className="panel">
        {loading ? <TableSkeleton columns={5} /> : accounts.length === 0 ? <Empty>
          <div className="no-account-hint">
            <p>暂无 Cloudflare 账号</p>
            {!showForm && <button type="button" className="primary" onClick={() => setShowForm(true)}><Plus size={15} />添加第一个账号</button>}
          </div>
        </Empty> : <div className="table-wrap"><table className="accounts-table">
          <thead><tr><th>名称</th><th>Account ID</th><th>健康</th><th>能力</th><th aria-label="操作" /></tr></thead>
          <tbody>{accounts.map((account) => <Fragment key={account.id}><tr className="account-row">
            <td data-label="名称"><strong>{account.name}</strong></td>
            <td data-label="Account ID" className="mono">{account.cloudflare_account_id}</td>
            <td data-label="健康">{account.verification || startingVerification === account.id ? <span className="account-verifying" role="status"><LoaderCircle size={14} className="spin" aria-hidden="true" />{account.verification?.status === "running" ? "正在检测" : account.verification?.attempts ? "等待重试" : "等待检测"}</span> : <Status value={account.health_status} label={accountHealthLabel(account.health_status)} />}</td>
            <td data-label="能力">{account.verification || startingVerification === account.id ? <div className="account-detection-progress" aria-label="能力检测进行中"><span aria-hidden="true" /></div> : <div className="capabilities">{account.capabilities?.map((capability) => <span className={capability.available ? "enabled" : "failed"} key={capability.name} title={`${capabilityName(capability.name)}：${capability.available ? "检测通过" : "检测未通过"}`} aria-label={`${capabilityName(capability.name)}：${capability.available ? "检测通过" : "检测未通过"}`}>{capability.available ? <Check size={12} aria-hidden="true" /> : <X size={12} aria-hidden="true" />}{capability.name}</span>)}</div>}</td>
            <td className="account-actions"><div className="row-actions"><button className="icon-button" disabled={busy} aria-label={`更新 ${account.name} 的凭证`} onClick={() => { setShowForm(false); setEditTarget(account); setRemoveR2Credentials(false); setError(""); }} title="更新凭证"><KeyRound size={15} /></button><button className="icon-button" disabled={busy || Boolean(account.verification)} aria-label={`重新检测 ${account.name} 的能力`} onClick={() => void redetect(account)} title="重新检测能力"><ShieldCheck size={15} /></button><button className="icon-button danger" disabled={busy} aria-label={`删除账号 ${account.name}`} onClick={() => { setError(""); setDeleteTarget(account); }} title="删除账号"><Trash2 size={15} /></button></div></td>
          </tr>{!account.verification && startingVerification !== account.id && <AccountDiagnostics account={account} />}</Fragment>)}</tbody>
        </table></div>}
      </section>
      <ConfirmDialog
        open={Boolean(pendingCredentialUpdate)}
        title="移除 R2 凭证"
        description="移除后文件映射仍会保留，但 R2 对象读写会停止，直到重新填写有效密钥。旧密钥将从本地永久删除。"
        confirmLabel="移除并保存"
        onOpenChange={(open) => { if (!open) setPendingCredentialUpdate(null); }}
        onConfirm={() => pendingCredentialUpdate ? saveCredentials(pendingCredentialUpdate.account, pendingCredentialUpdate.body) : Promise.resolve()}
      />
      <AccountDeleteDialog
        open={Boolean(deleteTarget)}
        accountName={deleteTarget?.name ?? ""}
        onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        onConfirm={() => deleteTarget ? remove(deleteTarget) : Promise.resolve()}
        onRefresh={load}
      />
    </>
  );
}
