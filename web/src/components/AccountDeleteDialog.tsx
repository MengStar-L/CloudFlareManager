import { useEffect, useState, type FormEvent } from "react";
import { Activity, AlertTriangle, ArrowRight, HardDrive, LoaderCircle, RefreshCw } from "lucide-react";
import { Dialog, Heading, Modal, ModalOverlay } from "react-aria-components";
import { APIError } from "../api";

interface AccountDeleteBlockerItem {
  id: string;
  name: string;
  status?: string;
}

interface AccountDeleteBlocker {
  kind: string;
  count: number;
  items: AccountDeleteBlockerItem[];
  truncated: boolean;
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function readBlockers(details: unknown): AccountDeleteBlocker[] {
  if (!record(details) || !Array.isArray(details.blockers)) return [];
  return details.blockers.flatMap((value) => {
    if (!record(value) || typeof value.kind !== "string" || !value.kind) return [];
    const items = Array.isArray(value.items) ? value.items.flatMap((item) => {
      if (!record(item) || typeof item.id !== "string" || typeof item.name !== "string") return [];
      return [{
        id: item.id,
        name: item.name,
        status: typeof item.status === "string" ? item.status : undefined,
      }];
    }) : [];
    const declaredCount = typeof value.count === "number" && Number.isFinite(value.count)
      ? Math.max(0, Math.floor(value.count))
      : 0;
    const count = Math.max(declaredCount, items.length);
    return [{
      kind: value.kind,
      count,
      items,
      truncated: value.truncated === true || count > items.length,
    }];
  });
}

function deletionErrorMessage(reason: unknown) {
  if (reason instanceof APIError) {
    if (reason.code === "not_found") return "找不到这个账号，它可能已经被删除。请关闭弹窗并刷新账号列表。";
    if (reason.status === 0) return "网络连接中断，无法确认账号是否已删除。请刷新账号列表确认后再决定是否重试。";
    if (reason.code === "invalid_response") return "服务器响应不完整，无法确认账号是否已删除。请刷新账号列表确认后再决定是否重试。";
    if (reason.code === "internal_error") return "服务器暂时无法完成删除，账号和凭据均未改变。请稍后重试。";
    if (reason.message) return reason.message;
  }
  return "无法确认删除结果。请刷新账号列表确认后再决定是否重试。";
}

function isR2Blocker(kind: string) {
  return kind === "r2_bucket" || kind === "r2_buckets";
}

function isR2DeletionJobBlocker(kind: string) {
  return kind === "r2_bucket_deletion_job";
}

function jobStatusLabel(status?: string) {
  if (status === "pending") return "等待中";
  if (status === "running") return "运行中";
  return status ?? "";
}

export function AccountDeleteDialog({
  open,
  accountName,
  onOpenChange,
  onConfirm,
  onRefresh,
}: {
  open: boolean;
  accountName: string;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => Promise<void>;
  onRefresh: () => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [blockers, setBlockers] = useState<AccountDeleteBlocker[] | null>(null);
  const [refreshRequired, setRefreshRequired] = useState(false);

  function reset() {
    setBusy(false);
    setError("");
    setBlockers(null);
    setRefreshRequired(false);
  }

  useEffect(() => {
    reset();
  }, [open, accountName]);

  function changeOpen(nextOpen: boolean) {
    if (busy) return;
    if (!nextOpen) reset();
    onOpenChange(nextOpen);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || blockers !== null) return;
    setBusy(true);
    setError("");
    try {
      await onConfirm();
      reset();
      onOpenChange(false);
    } catch (reason) {
      if (reason instanceof APIError && reason.status === 409 && reason.code === "account_in_use") {
        setBlockers(readBlockers(reason.details));
      } else {
        setError(deletionErrorMessage(reason));
        setRefreshRequired(!(reason instanceof APIError) || reason.status === 0 || reason.code === "not_found" || reason.code === "invalid_response");
      }
      setBusy(false);
    }
  }

  const blocked = blockers !== null;
  const hasDeletionJob = blocked && blockers.some((item) => isR2DeletionJobBlocker(item.kind));
  const showStorageAction = blocked && !hasDeletionJob && (blockers.length === 0 || blockers.some((item) => isR2Blocker(item.kind)));

  return (
    <ModalOverlay className="modal-overlay" isOpen={open} onOpenChange={changeOpen} isDismissable={!busy}>
      <Modal className="account-delete-modal">
        <Dialog className="confirm-dialog account-delete-dialog" role="alertdialog">
          <form onSubmit={(event) => void submit(event)}>
            <div className="dialog-icon"><AlertTriangle size={19} aria-hidden="true" /></div>
            <Heading slot="title">{blocked ? "账号暂时无法删除" : "删除 Cloudflare 账号"}</Heading>
            {blocked ? (
              <>
                <p className="account-delete-summary">
                  系统没有删除账号 <strong>{accountName}</strong>，已保存的本地凭据也保持不变。请先处理仍与该账号关联的资源。
                </p>
                {blockers.length > 0 ? (
                  <div className="account-delete-blockers" role="alert">
                    {blockers.map((blocker) => (
                      <section className="account-delete-blocker" key={blocker.kind}>
                        <div className="account-delete-blocker-heading">
                          {isR2DeletionJobBlocker(blocker.kind)
                            ? <Activity size={17} aria-hidden="true" />
                            : <HardDrive size={17} aria-hidden="true" />}
                          <strong>{isR2Blocker(blocker.kind)
                            ? "R2 存储桶"
                            : isR2DeletionJobBlocker(blocker.kind) ? "正在执行的删桶任务" : "关联资源"}</strong>
                          <span>{blocker.count.toLocaleString("zh-CN")} 个</span>
                        </div>
                        {blocker.items.length > 0 && (
                          <ul>
                            {blocker.items.map((item, index) => <li key={`${item.id}-${index}`}>
                              <code>{item.name}</code>
                              {isR2DeletionJobBlocker(blocker.kind) && item.status && <span>{jobStatusLabel(item.status)}</span>}
                            </li>)}
                          </ul>
                        )}
                        {blocker.truncated && (
                          <small>当前显示 {blocker.items.length.toLocaleString("zh-CN")} 项，共 {blocker.count.toLocaleString("zh-CN")} 项。</small>
                        )}
                        <p>{isR2Blocker(blocker.kind)
                          ? "请在 R2 存储中按需清空并删除这些桶，或在没有本地对象映射后将其移出阵列。"
                          : isR2DeletionJobBlocker(blocker.kind)
                            ? "请等待远端删桶任务结束，再返回删除账号。"
                            : "请先处理这些关联资源，再返回删除账号。"}</p>
                      </section>
                    ))}
                  </div>
                ) : (
                  <p className="form-error dialog-error" role="alert">服务器未返回资源明细，请先检查 R2 存储中与该账号关联的登记桶。</p>
                )}
              </>
            ) : (
              <p className="account-delete-summary">
                确定删除本地账号 <strong>{accountName}</strong> 及其已保存凭据？此操作不会删除 Cloudflare 上的账号或资源。
              </p>
            )}
            {error && <p className="form-error dialog-error" role="alert">{error}</p>}
            <div className="dialog-actions">
              <button type="button" onClick={() => changeOpen(false)} disabled={busy}>{blocked ? "关闭" : "取消"}</button>
              {refreshRequired ? (
                <button type="button" className="account-delete-route" onClick={() => { changeOpen(false); void onRefresh(); }}>
                  <RefreshCw size={15} aria-hidden="true" />关闭并刷新列表
                </button>
              ) : hasDeletionJob ? (
                <a className="account-delete-route" href="#activity" onClick={() => changeOpen(false)}>
                  <Activity size={15} aria-hidden="true" />查看任务<ArrowRight size={14} aria-hidden="true" />
                </a>
              ) : showStorageAction ? (
                <a className="account-delete-route" href="#storage" onClick={() => changeOpen(false)}>
                  <HardDrive size={15} aria-hidden="true" />前往 R2 存储<ArrowRight size={14} aria-hidden="true" />
                </a>
              ) : !blocked && (
                <button type="submit" className="danger-confirm" disabled={busy}>
                  {busy && <LoaderCircle className="spin" size={15} />}
                  {busy ? "正在删除" : "删除账号"}
                </button>
              )}
            </div>
          </form>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}
