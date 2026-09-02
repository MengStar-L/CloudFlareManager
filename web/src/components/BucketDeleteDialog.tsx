import { useEffect, useState, type FormEvent } from "react";
import { AlertTriangle, ArchiveX, LoaderCircle, Trash2 } from "lucide-react";
import { Dialog, Heading, Modal, ModalOverlay } from "react-aria-components";
import { APIError } from "../api";
import type { BucketDeleteConfirmation, BucketDeletionMode } from "../types";

const deletionErrorMessages: Record<string, string> = {
  bucket_not_empty: "存储桶中仍有文件。请先删除桶内所有文件，或选择“一键清空并删除桶”。",
  bucket_busy: "存储桶正在处理其他写入，请稍后重试。",
  bucket_deleting: "这个存储桶已经在执行删除任务，请等待任务完成。",
  bucket_locked: "存储桶受 Cloudflare Bucket Lock 或保留策略保护，请先在 Cloudflare 中解除后再重试。",
  permission_denied: "Cloudflare API Token 没有删除存储桶所需的权限，请授予 Workers R2 Storage Write 权限后重试。",
  s3_credentials_required: "桶内存在无法通过 Cloudflare API 清理的分片上传。请先配置 R2 S3 访问密钥，再重试一键清空并删除。",
  external_writes_detected: "清空期间检测到新的文件写入。请停止其他程序向该桶写入后重试。",
  bucket_identity_changed: "远端同名存储桶已经发生变化，为避免误删，任务已停止。",
  bucket_identity_unverifiable: "无法确认远端存储桶身份，为避免误删，任务已停止。",
  unsupported_jurisdiction: "当前版本暂不支持删除这个数据管辖区域中的存储桶。",
  partial_delete_failed: "部分文件删除失败，存储桶尚未删除。请检查权限或网络后重试。",
  rate_limited: "Cloudflare 请求过于频繁，删除任务尚未创建，请稍后手动重试。",
  cloudflare_unavailable: "Cloudflare 服务暂时不可用，请稍后重试。",
  local_finalize_failed: "远端存储桶已删除，但本地记录清理失败。请保留此页面状态并重试。",
  unauthenticated: "管理员密码不正确，请重新输入。",
  invalid_confirmation: "输入的桶名与要删除的存储桶不一致。",
  confirmation_failed: "管理员密码不正确，请重新输入。",
  confirmation_name_mismatch: "输入的桶名与要删除的存储桶不一致。",
  not_found: "找不到这个存储桶，它可能已经被删除。",
};

function deletionErrorMessage(reason: unknown) {
  if (reason instanceof APIError && reason.code && deletionErrorMessages[reason.code]) {
    return deletionErrorMessages[reason.code];
  }
  if (reason instanceof APIError && reason.message) return reason.message;
  return "网络连接失败，删除请求未能完成，请检查网络后重试。";
}

export function BucketDeleteDialog({
  open,
  bucketName,
  accountName,
  objectCount,
  initialMode = "empty_only",
  remoteMissing = false,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  bucketName: string;
  accountName?: string;
  objectCount?: number;
  initialMode?: BucketDeletionMode;
  remoteMissing?: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: (confirmation: BucketDeleteConfirmation) => Promise<void>;
}) {
  const [mode, setMode] = useState<BucketDeletionMode>(initialMode);
  const [confirmationName, setConfirmationName] = useState("");
  const [adminPassword, setAdminPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  function reset() {
    setMode(initialMode);
    setConfirmationName("");
    setAdminPassword("");
    setBusy(false);
    setError("");
  }

  useEffect(() => {
    setMode(initialMode);
    setConfirmationName("");
    setAdminPassword("");
    setBusy(false);
    setError("");
  }, [open, initialMode]);

  function changeOpen(nextOpen: boolean) {
    if (busy) return;
    if (!nextOpen) reset();
    onOpenChange(nextOpen);
  }

  function changeMode(nextMode: BucketDeletionMode) {
    setMode(nextMode);
    setConfirmationName("");
    setError("");
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    const effectiveMode = remoteMissing ? "empty_only" : mode;
    const nameMatches = effectiveMode === "empty_only" || confirmationName === bucketName;
    if (busy || !adminPassword || !nameMatches) return;

    setBusy(true);
    setError("");
    try {
      await onConfirm({ mode: effectiveMode, confirmationName, adminPassword });
      reset();
      onOpenChange(false);
    } catch (reason) {
      setError(deletionErrorMessage(reason));
      setBusy(false);
    }
  }

  const deletingBucket = !remoteMissing && mode === "empty_and_delete";
  const nameMatches = confirmationName === bucketName;
  const disabled = busy || !adminPassword || (deletingBucket && !nameMatches);
  const nameErrorID = "bucket-delete-name-error";
  const submitRequirementID = "bucket-delete-submit-requirement";
  const submitRequirement = busy
    ? "删除请求正在提交。"
    : !adminPassword
      ? "请输入管理员密码。"
      : deletingBucket && !nameMatches
        ? "请输入完全一致的存储桶名称。"
        : "可以提交。";

  return (
    <ModalOverlay className="modal-overlay" isOpen={open} onOpenChange={changeOpen} isDismissable={!busy}>
      <Modal className="bucket-delete-modal">
        <Dialog className="confirm-dialog bucket-delete-dialog" role="alertdialog">
          <form onSubmit={(event) => void submit(event)}>
            <div className="dialog-icon"><AlertTriangle size={19} aria-hidden="true" /></div>
            <Heading slot="title">{remoteMissing ? "清理本地存储桶登记" : "清空或删除存储桶"}</Heading>
            {remoteMissing ? <p className="bucket-delete-summary">
              Cloudflare 中已找不到存储桶 <strong>{bucketName}</strong>。本操作只清理本地登记和未完成的本地写入状态，不会删除远端数据。
            </p> : <p className="bucket-delete-summary">
              存储桶 <strong>{bucketName}</strong>
              {accountName && <> 位于账号 <strong>{accountName}</strong></>}
              {typeof objectCount === "number" && <>，当前有 {objectCount.toLocaleString()} 个对象</>}。
            </p>}

            {!remoteMissing && <fieldset className="bucket-delete-modes" disabled={busy}>
              <legend>选择操作</legend>
              <label className={mode === "empty_only" ? "selected" : ""}>
                <input
                  type="radio"
                  name="bucket-delete-mode"
                  value="empty_only"
                  checked={mode === "empty_only"}
                  onChange={() => changeMode("empty_only")}
                />
                <ArchiveX size={17} aria-hidden="true" />
                <span><strong>仅删除空桶</strong><small>桶内有任何文件或分片上传时拒绝操作，不会删除内容。</small></span>
              </label>
              <label className={mode === "empty_and_delete" ? "selected danger" : "danger"}>
                <input
                  type="radio"
                  name="bucket-delete-mode"
                  value="empty_and_delete"
                  checked={mode === "empty_and_delete"}
                  onChange={() => changeMode("empty_and_delete")}
                />
                <Trash2 size={17} aria-hidden="true" />
                <span><strong>一键清空并删除桶</strong><small>永久删除所有内容和存储桶，此操作不可恢复。删除期间请停止外部写入，也不要重建同名桶。</small></span>
              </label>
            </fieldset>}

            {deletingBucket && (
              <label className="dialog-field">
                输入完整桶名 <code>{bucketName}</code> 以确认
                <input
                  value={confirmationName}
                  onChange={(event) => { setConfirmationName(event.target.value); setError(""); }}
                  autoComplete="off"
                  spellCheck={false}
                  autoFocus
                  required
                  aria-invalid={Boolean(confirmationName && !nameMatches)}
                  aria-describedby={confirmationName && !nameMatches ? nameErrorID : undefined}
                />
                {confirmationName && !nameMatches && <small id={nameErrorID} className="form-error">桶名不一致</small>}
              </label>
            )}
            <label className="dialog-field">
              管理员密码
              <input
                type="password"
                value={adminPassword}
                onChange={(event) => { setAdminPassword(event.target.value); setError(""); }}
                autoComplete="current-password"
                autoFocus={!deletingBucket}
                required
              />
            </label>
            {error && <p className="form-error dialog-error" role="alert">{error}</p>}
            <span id={submitRequirementID} className="visually-hidden" aria-live="polite">{submitRequirement}</span>
            <div className="dialog-actions">
              <button type="button" onClick={() => changeOpen(false)} disabled={busy}>取消</button>
              <button type="submit" className="danger-confirm" disabled={disabled} aria-describedby={submitRequirementID}>
                {busy && <LoaderCircle className="spin" size={15} />}
                {busy ? "正在提交" : remoteMissing ? "清理本地登记" : deletingBucket ? "清空并删除" : "删除空桶"}
              </button>
            </div>
          </form>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}
