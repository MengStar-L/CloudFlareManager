import { FormEvent, useEffect, useState } from "react";
import { Ban, Copy, Eye, EyeOff, KeyRound, Plus, RotateCw, Trash2, X } from "lucide-react";
import { api } from "../api";
import type { Credential } from "../types";
import { Empty, ErrorBanner, PageHeader, RefreshButton, Status } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { SelectField } from "../components/SelectField";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

const scopes: Record<Credential["kind"], string[]> = {
  s3: ["r2:read", "r2:write"], webdav: ["r2:read", "r2:write"], ai: ["ai:invoke"],
};

const kindOptions = [
  { value: "s3", label: "S3" },
  { value: "webdav", label: "WebDAV" },
  { value: "ai", label: "AI" },
];

// 各类型密钥在客户端里的实际含义，用于展示时标注字段名。
const fieldLabels: Record<Credential["kind"], { id: string; secret: string }> = {
  s3: { id: "Access Key ID", secret: "Secret Access Key" },
  webdav: { id: "用户名", secret: "密码" },
  ai: { id: "密钥 ID", secret: "调用 Token" },
};

interface RevealResponse { id: string; kind: Credential["kind"]; public_id: string; secret: string }

function fullSecret(kind: Credential["kind"], publicID: string, secret: string) {
  return kind === "ai" ? `${publicID}.${secret}` : secret;
}

export function AccessPage() {
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [kind, setKind] = useState<Credential["kind"]>("s3");
  const [created, setCreated] = useState<Credential | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [revealed, setRevealed] = useState<Record<string, string>>({});
  const [revealing, setRevealing] = useState<string | null>(null);
  const [pendingAction, setPendingAction] = useState<{ kind: "rotate" | "revoke" | "delete"; credential: Credential } | null>(null);
  const toast = useToast();

  async function load() {
    try { const data = await api.get<{ credentials: Credential[] }>("/api/v1/credentials"); setCredentials(data.credentials ?? []); }
    catch (reason) { setError((reason as Error).message); }
    finally { setLoading(false); }
  }
  useEffect(() => { void load(); }, []);

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const formElement = event.currentTarget; const form = new FormData(formElement);
    if (busy) return;
    setBusy(true);
    try {
      const credential = await api.post<Credential>("/api/v1/credentials", { kind, name: form.get("name"), public_id: form.get("public_id"), scopes: scopes[kind] });
      setCreated(credential); formElement.reset(); toast.show("访问密钥已创建"); await load();
    } catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }

  async function rotate(credential: Credential) {
    try {
      const value = await api.post<Credential>(`/api/v1/credentials/${credential.id}/rotate`, {});
      setCreated(value);
      setRevealed((previous) => { const next = { ...previous }; delete next[credential.id]; return next; });
      toast.show(credential.disabled ? "密钥已重新启用，新密钥已生成" : "密钥已轮换");
      await load();
    }
    catch (reason) { setError((reason as Error).message); throw reason; }
  }

  async function revoke(credential: Credential) {
    try {
      await api.delete(`/api/v1/credentials/${credential.id}`);
      if (created?.id === credential.id) setCreated(null);
      toast.show("密钥已撤销并停用，记录保留在列表中");
      await load();
    }
    catch (reason) { setError((reason as Error).message); throw reason; }
  }

  async function deleteRecord(credential: Credential) {
    try {
      await api.delete(`/api/v1/credentials/${credential.id}/record`);
      if (created?.id === credential.id) setCreated(null);
      setRevealed((previous) => { const next = { ...previous }; delete next[credential.id]; return next; });
      toast.show("密钥记录已删除");
      await load();
    }
    catch (reason) { setError((reason as Error).message); throw reason; }
  }

  async function toggleReveal(credential: Credential) {
    if (revealed[credential.id] !== undefined) {
      setRevealed((previous) => { const next = { ...previous }; delete next[credential.id]; return next; });
      return;
    }
    setRevealing(credential.id);
    try {
      const data = await api.get<RevealResponse>(`/api/v1/credentials/${credential.id}/secret`);
      setRevealed((previous) => ({ ...previous, [credential.id]: fullSecret(data.kind, data.public_id, data.secret) }));
    } catch (reason) { setError((reason as Error).message); }
    finally { setRevealing(null); }
  }

  const token = created ? fullSecret(created.kind, created.public_id, created.secret ?? "") : "";
  const labels = created ? fieldLabels[created.kind] : null;

  return (
    <>
      <PageHeader title="访问密钥" actions={<RefreshButton onRefresh={load} />} />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      {created && labels && <section className="secret-band">
        <KeyRound size={18} />
        <div>
          <strong>新密钥</strong>
          <code><span className="secret-field-label">{labels.id}</span>{created.public_id}</code>
          <code><span className="secret-field-label">{labels.secret}</span>{token}</code>
          <small>密钥已加密保存，之后可随时在下方列表中点击眼睛图标查看。</small>
        </div>
        <div className="secret-band-actions">
          <button className="icon-button" onClick={() => { void navigator.clipboard.writeText(token); toast.show("密钥已复制", "info"); }} title="复制密钥"><Copy size={16} /></button>
          <button className="icon-button" onClick={() => setCreated(null)} title="关闭"><X size={15} /></button>
        </div>
      </section>}
      <form className="form-band inline-form" onSubmit={create}>
        <SelectField label="类型" value={kind} options={kindOptions} onChange={(value) => setKind(value as Credential["kind"])} />
        <label>名称<input name="name" required /></label>
        <label>公开 ID<input name="public_id" placeholder="留空自动生成" /></label>
        <button className="primary" type="submit" disabled={busy}><Plus size={16} />创建</button>
      </form>
      <section className="panel">{loading ? <TableSkeleton columns={7} /> : credentials.length === 0 ? <Empty>暂无访问密钥</Empty> : <div className="table-wrap"><table className="access-table">
        <thead><tr><th>名称</th><th>类型</th><th>公开 ID</th><th>密钥</th><th>范围</th><th>状态</th><th /></tr></thead>
        <tbody>{credentials.map((credential) => {
          const secretShown = revealed[credential.id];
          return <tr key={credential.id} className={credential.disabled ? "row-muted" : ""}>
            <td data-label="名称"><strong>{credential.name}</strong></td>
            <td data-label="类型">{credential.kind.toUpperCase()}</td>
            <td className="mono" data-label="公开 ID">{credential.public_id}</td>
            <td className="secret-cell" data-label="密钥">
              {secretShown !== undefined
                ? <code className="secret-value">{secretShown}</code>
                : <span className="secret-mask" aria-hidden>••••••••••</span>}
              <button className="icon-button" title={secretShown !== undefined ? "隐藏密钥" : "查看密钥"}
                disabled={revealing === credential.id} onClick={() => void toggleReveal(credential)}>
                {secretShown !== undefined ? <EyeOff size={15} /> : <Eye size={15} />}
              </button>
              {secretShown !== undefined && <button className="icon-button" title="复制密钥"
                onClick={() => { void navigator.clipboard.writeText(secretShown); toast.show("密钥已复制", "info"); }}><Copy size={15} /></button>}
            </td>
            <td data-label="范围">{credential.scopes.join(", ")}</td>
            <td data-label="状态"><Status value={credential.disabled ? "disabled" : "available"} label={credential.disabled ? "已撤销" : "可用"} /></td>
            <td className="row-actions">
              <button className="icon-button" title={credential.disabled ? "重新启用并生成新密钥" : "轮换密钥"} onClick={() => setPendingAction({ kind: "rotate", credential })}><RotateCw size={15} /></button>
              {credential.disabled
                ? <button className="icon-button danger" title="删除记录" onClick={() => setPendingAction({ kind: "delete", credential })}><Trash2 size={15} /></button>
                : <button className="icon-button danger" title="撤销密钥" onClick={() => setPendingAction({ kind: "revoke", credential })}><Ban size={15} /></button>}
            </td>
          </tr>;
        })}</tbody>
      </table></div>}</section>
      <ConfirmDialog
        open={Boolean(pendingAction)}
        title={pendingAction?.kind === "revoke" ? "撤销访问密钥"
          : pendingAction?.kind === "delete" ? "删除密钥记录"
          : pendingAction?.credential.disabled ? "重新启用密钥" : "轮换访问密钥"}
        description={pendingAction?.kind === "revoke"
          ? `撤销“${pendingAction?.credential.name ?? ""}”后，使用它的客户端将立即无法访问；记录会保留在列表中并标记为已撤销。`
          : pendingAction?.kind === "delete"
            ? `永久删除已撤销密钥“${pendingAction?.credential.name ?? ""}”的记录？此操作不可恢复，公开 ID 也将从列表中消失。`
            : pendingAction?.credential.disabled
              ? `重新启用“${pendingAction?.credential.name ?? ""}”并生成一把新密钥？旧密钥不再可用，新密钥可随时在列表中查看。`
              : `轮换“${pendingAction?.credential.name ?? ""}”后，旧密钥将立即失效，新密钥可随时在列表中查看。`}
        confirmLabel={pendingAction?.kind === "revoke" ? "确认撤销"
          : pendingAction?.kind === "delete" ? "永久删除"
          : pendingAction?.credential.disabled ? "启用并生成新密钥" : "确认轮换"}
        onOpenChange={(open) => { if (!open) setPendingAction(null); }}
        onConfirm={() => !pendingAction ? Promise.resolve()
          : pendingAction.kind === "rotate" ? rotate(pendingAction.credential)
          : pendingAction.kind === "delete" ? deleteRecord(pendingAction.credential)
          : revoke(pendingAction.credential)}
      />
    </>
  );
}
