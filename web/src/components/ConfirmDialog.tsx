import { useEffect, useState, type FormEvent } from "react";
import { AlertTriangle, LoaderCircle } from "lucide-react";
import { Dialog, Heading, Modal, ModalOverlay } from "react-aria-components";

export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel = "确认",
  danger = true,
  passwordLabel,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  title: string;
  description: string;
  confirmLabel?: string;
  danger?: boolean;
  passwordLabel?: string;
  onOpenChange: (open: boolean) => void;
  onConfirm: (password: string) => Promise<void>;
}) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!open) { setPassword(""); setBusy(false); setError(""); }
  }, [open]);

  async function confirm() {
    setBusy(true);
    setError("");
    try {
      await onConfirm(password);
      onOpenChange(false);
    } catch (reason) {
      // 遮罩会盖住页面里的错误横幅，所以失败原因必须显示在对话框内部。
      setError(reason instanceof Error ? reason.message : "操作失败");
    } finally {
      setBusy(false);
    }
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!busy && !(passwordLabel && !password)) void confirm();
  }

  return (
    <ModalOverlay className="modal-overlay" isOpen={open} onOpenChange={onOpenChange} isDismissable={!busy}>
      <Modal className="confirm-modal">
        <Dialog className="confirm-dialog" role="alertdialog">
          <form onSubmit={submit}>
            <div className="dialog-icon"><AlertTriangle size={19} aria-hidden="true" /></div>
            <Heading slot="title">{title}</Heading>
            <p>{description}</p>
            {passwordLabel && (
              <label className="dialog-field">
                {passwordLabel}
                <input type="password" value={password} onChange={(event) => setPassword(event.target.value)} autoComplete="current-password" autoFocus />
              </label>
            )}
            {error && <p className="form-error dialog-error" role="alert">{error}</p>}
            <div className="dialog-actions">
              <button type="button" onClick={() => onOpenChange(false)} disabled={busy}>取消</button>
              <button
                type="submit"
                className={danger ? "danger-confirm" : ""}
                disabled={busy || Boolean(passwordLabel && !password)}
              >
                {busy && <LoaderCircle className="spin" size={15} />}
                {confirmLabel}
              </button>
            </div>
          </form>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}
