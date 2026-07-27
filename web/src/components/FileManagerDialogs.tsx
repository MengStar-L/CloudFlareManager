import { useEffect, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import {
  Archive, ChevronRight, Download, File, FileJson, FileText, Film, Folder, FolderOpen,
  Image, Info, LoaderCircle, MoreHorizontal, Move, Music, Pencil, Trash2, X,
} from "lucide-react";
import { Dialog, Heading, Modal, ModalOverlay } from "react-aria-components";
import { api } from "../api";
import type { FileDirectoryList, FileEntry } from "../types";
import { formatBytes } from "./UI";

export type PreviewKind = "text" | "image" | "audio" | "video";
export type FileMenuAction = "open" | "details" | "download" | "rename" | "move" | "delete";

export function contentURL(entry: FileEntry, mode: "preview" | "download") {
  return `/api/v1/files/content?key=${encodeURIComponent(entry.key)}&mode=${mode}`;
}

export function downloadFile(entry: FileEntry) {
  const anchor = document.createElement("a");
  anchor.href = contentURL(entry, "download");
  anchor.download = entry.name;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
}

export function filePreviewKind(entry: FileEntry): PreviewKind | "" {
  let contentType = entry.content_type.split(";", 1)[0].toLowerCase();
  const extension = entry.name.includes(".") ? entry.name.slice(entry.name.lastIndexOf(".")).toLowerCase() : "";
  if (!contentType || contentType === "application/octet-stream") {
    if ([".txt", ".log", ".md", ".csv", ".json"].includes(extension)) contentType = extension === ".json" ? "application/json" : "text/plain";
    else if ([".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp", ".ico"].includes(extension)) contentType = `image/${extension.replace(".", "").replace("jpg", "jpeg")}`;
    else if ([".mp3", ".wav", ".ogg", ".m4a"].includes(extension)) contentType = "audio/mpeg";
    else if ([".mp4", ".webm", ".ogv", ".mov"].includes(extension)) contentType = "video/mp4";
  }
  if ((contentType === "application/json" || contentType.endsWith("+json") || (contentType.startsWith("text/") && contentType !== "text/html")) && entry.size <= 1 << 20) return "text";
  if (["image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon"].includes(contentType)) return "image";
  if (["audio/mpeg", "audio/ogg", "audio/wav", "audio/x-wav", "audio/mp4", "audio/webm"].includes(contentType)) return "audio";
  if (["video/mp4", "video/webm", "video/ogg", "video/quicktime"].includes(contentType)) return "video";
  return "";
}

export function FileEntryIcon({ entry, size = 18 }: { entry: FileEntry; size?: number }) {
  if (entry.kind === "directory") return <Folder size={size} />;
  const type = entry.content_type.toLowerCase();
  const extension = entry.name.slice(entry.name.lastIndexOf(".")).toLowerCase();
  if (type.includes("json") || extension === ".json") return <FileJson size={size} />;
  if (type.startsWith("text/") || [".txt", ".log", ".md", ".csv"].includes(extension)) return <FileText size={size} />;
  if (type.startsWith("image/")) return <Image size={size} />;
  if (type.startsWith("audio/")) return <Music size={size} />;
  if (type.startsWith("video/")) return <Film size={size} />;
  if ([".zip", ".rar", ".7z", ".tar", ".gz"].includes(extension)) return <Archive size={size} />;
  return <File size={size} />;
}

export function FileContextMenu({ entry, x, y, onAction, onClose }: {
  entry: FileEntry;
  x: number;
  y: number;
  onAction: (action: FileMenuAction) => void;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current?.querySelector<HTMLButtonElement>("button")?.focus();
    const close = (event: MouseEvent) => { if (!ref.current?.contains(event.target as Node)) onClose(); };
    const keydown = (event: KeyboardEvent) => { if (event.key === "Escape") onClose(); };
    window.addEventListener("mousedown", close);
    window.addEventListener("keydown", keydown);
    window.addEventListener("scroll", onClose, true);
    return () => {
      window.removeEventListener("mousedown", close);
      window.removeEventListener("keydown", keydown);
      window.removeEventListener("scroll", onClose, true);
    };
  }, [onClose]);
  const left = Math.max(8, Math.min(x, window.innerWidth - 224));
  const top = Math.max(8, Math.min(y, window.innerHeight - 294));
  const item = (action: FileMenuAction, icon: ReactNode, label: string, danger = false) => (
    <button type="button" role="menuitem" className={danger ? "danger" : ""} onClick={() => onAction(action)}>
      {icon}<span>{label}</span>
    </button>
  );
  return (
    <div ref={ref} className="file-context-menu" role="menu" aria-label={`${entry.name} 操作`} style={{ left, top }}>
      {item("open", entry.kind === "directory" ? <FolderOpen size={15} /> : <FileText size={15} />, entry.kind === "directory" ? "打开" : "预览")}
      {entry.kind === "file" && item("download", <Download size={15} />, "下载")}
      {item("details", <Info size={15} />, "详细信息")}
      <div className="menu-separator" />
      {item("rename", <Pencil size={15} />, "重命名")}
      {item("move", <Move size={15} />, "移动")}
      {item("delete", <Trash2 size={15} />, "删除", true)}
    </div>
  );
}

export function FilePreview({ entry, open, onOpenChange }: { entry: FileEntry | null; open: boolean; onOpenChange: (open: boolean) => void }) {
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const kind = entry ? filePreviewKind(entry) : "";
  useEffect(() => {
    let active = true;
    setText("");
    setError("");
    if (!open || !entry || kind !== "text") return () => { active = false; };
    void api.text(contentURL(entry, "preview")).then((value) => {
      if (!active) return;
      if (entry.content_type.includes("json") || entry.name.toLowerCase().endsWith(".json")) {
        try { setText(JSON.stringify(JSON.parse(value), null, 2)); return; } catch { /* show original text */ }
      }
      setText(value);
    }).catch((reason) => { if (active) setError((reason as Error).message); });
    return () => { active = false; };
  }, [entry, kind, open]);

  if (!entry) return null;
  return (
    <ModalOverlay className="file-preview-overlay" isOpen={open} onOpenChange={onOpenChange} isDismissable>
      <Modal className="file-preview-modal">
        <Dialog className="file-preview-dialog" aria-label={`${entry.name} 预览`}>
          <header>
            <div><FileEntryIcon entry={entry} /><strong>{entry.name}</strong></div>
            <div className="row-actions">
              <button className="icon-button" type="button" title="下载" onClick={() => downloadFile(entry)}><Download size={17} /></button>
              <button className="icon-button" type="button" title="关闭" onClick={() => onOpenChange(false)}><X size={18} /></button>
            </div>
          </header>
          <div className={`file-preview-content ${kind || "unsupported"}`}>
            {error ? <div className="preview-message danger-text">{error}</div> : kind === "text" ? (
              text ? <pre>{text}</pre> : <LoaderCircle className="spin" size={24} aria-label="正在加载预览" />
            ) : kind === "image" ? (
              <img src={contentURL(entry, "preview")} alt={entry.name} />
            ) : kind === "audio" ? (
              <audio src={contentURL(entry, "preview")} controls preload="metadata" />
            ) : kind === "video" ? (
              <video src={contentURL(entry, "preview")} controls preload="metadata" />
            ) : null}
          </div>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}

export function FileDetailsDialog({ entry, open, onOpenChange }: { entry: FileEntry | null; open: boolean; onOpenChange: (open: boolean) => void }) {
  if (!entry) return null;
  return (
    <ModalOverlay className="modal-overlay" isOpen={open} onOpenChange={onOpenChange} isDismissable>
      <Modal className="file-dialog-modal">
        <Dialog className="file-dialog" aria-label={`${entry.name} 详细信息`}>
          <header><Heading slot="title">详细信息</Heading><button className="icon-button" onClick={() => onOpenChange(false)} title="关闭"><X size={17} /></button></header>
          <div className="file-detail-name"><FileEntryIcon entry={entry} size={28} /><strong>{entry.name}</strong></div>
          <dl className="file-detail-list">
            <div><dt>路径</dt><dd className="mono">{entry.key}</dd></div>
            <div><dt>类型</dt><dd>{entry.kind === "directory" ? "文件夹" : entry.content_type || "未知"}</dd></div>
            <div><dt>大小</dt><dd>{entry.kind === "directory" ? "--" : formatBytes(entry.size)}</dd></div>
            <div><dt>修改时间</dt><dd>{new Date(entry.last_modified).toLocaleString()}</dd></div>
            {entry.etag && <div><dt>ETag</dt><dd className="mono">{entry.etag}</dd></div>}
          </dl>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}

export function NameDialog({ open, title, label, initialValue = "", confirmLabel, onOpenChange, onConfirm }: {
  open: boolean;
  title: string;
  label: string;
  initialValue?: string;
  confirmLabel: string;
  onOpenChange: (open: boolean) => void;
  onConfirm: (name: string) => Promise<void>;
}) {
  const [name, setName] = useState(initialValue);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => { if (open) { setName(initialValue); setError(""); } }, [initialValue, open]);
  async function submit(event: FormEvent) {
    event.preventDefault();
    const normalized = name.trim();
    if (!normalized || normalized === "." || normalized === ".." || /[\\/]/.test(normalized)) {
      setError("名称不能为空，且不能包含斜杠或反斜杠");
      return;
    }
    setBusy(true);
    setError("");
    try { await onConfirm(normalized); onOpenChange(false); }
    catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }
  return (
    <ModalOverlay className="modal-overlay" isOpen={open} onOpenChange={onOpenChange} isDismissable={!busy}>
      <Modal className="file-dialog-modal">
        <Dialog className="file-dialog" aria-label={title}>
          <form onSubmit={submit}>
            <header><Heading slot="title">{title}</Heading><button type="button" className="icon-button" onClick={() => onOpenChange(false)} title="关闭"><X size={17} /></button></header>
            <label className="file-dialog-field">{label}<input value={name} onChange={(event) => setName(event.target.value)} autoFocus /></label>
            {error && <p className="form-error">{error}</p>}
            <div className="dialog-actions"><button type="button" onClick={() => onOpenChange(false)}>取消</button><button className="primary" disabled={busy}>{busy && <LoaderCircle className="spin" size={15} />}{confirmLabel}</button></div>
          </form>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}

export function MoveDialog({ entry, open, onOpenChange, onMove }: {
  entry: FileEntry | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onMove: (directory: string) => Promise<void>;
}) {
  const [path, setPath] = useState("");
  const [directories, setDirectories] = useState<FileEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => { if (open) { setPath(""); setError(""); } }, [open, entry]);
  useEffect(() => {
    if (!open) return;
    let active = true;
    setLoading(true);
    void loadAllDirectories(path).then((items) => { if (active) setDirectories(items); })
      .catch((reason) => { if (active) setError((reason as Error).message); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [open, path]);
  const segments = useMemo(() => path.split("/").filter(Boolean), [path]);
  if (!entry) return null;
  const suffix = entry.kind === "directory" ? "/" : "";
  const destination = path + entry.name + suffix;
  const blocked = destination === entry.key || (entry.kind === "directory" && path.startsWith(entry.key));
  async function move() {
    setBusy(true);
    setError("");
    try { await onMove(path); onOpenChange(false); }
    catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }
  return (
    <ModalOverlay className="modal-overlay" isOpen={open} onOpenChange={onOpenChange} isDismissable={!busy}>
      <Modal className="file-move-modal">
        <Dialog className="file-dialog file-move-dialog" aria-label={`移动 ${entry.name}`}>
          <header><Heading slot="title">选择目标文件夹</Heading><button className="icon-button" onClick={() => onOpenChange(false)} title="关闭"><X size={17} /></button></header>
          <nav className="file-picker-breadcrumb" aria-label="目标路径">
            <button onClick={() => setPath("")} className={!path ? "active" : ""}><FolderOpen size={15} />根目录</button>
            {segments.map((segment, index) => <span key={`${segment}-${index}`}><ChevronRight size={14} /><button onClick={() => setPath(segments.slice(0, index + 1).join("/") + "/")}>{segment}</button></span>)}
          </nav>
          <div className="file-picker-list">
            {loading ? <LoaderCircle className="spin" size={22} aria-label="正在加载文件夹" /> : directories.length === 0 ? <p>此文件夹中没有子文件夹</p> : directories.map((directory) => (
              <button key={directory.key} onClick={() => setPath(directory.key)} disabled={entry.kind === "directory" && directory.key.startsWith(entry.key)}>
                <Folder size={18} /><span>{directory.name}</span><ChevronRight size={15} />
              </button>
            ))}
          </div>
          {error && <p className="form-error">{error}</p>}
          <div className="move-destination"><span>目标</span><code>{path || "/"}</code></div>
          <div className="dialog-actions"><button onClick={() => onOpenChange(false)}>取消</button><button className="primary" onClick={() => void move()} disabled={busy || blocked}>{busy && <LoaderCircle className="spin" size={15} />}移动到这里</button></div>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}

async function loadAllDirectories(path: string) {
  const result: FileEntry[] = [];
  let after = "";
  do {
    const query = new URLSearchParams({ path, kind: "directory", limit: "500" });
    if (after) query.set("after", after);
    const page = await api.get<FileDirectoryList>(`/api/v1/files?${query}`);
    result.push(...(page.entries ?? []));
    after = page.next_marker ?? "";
  } while (after);
  return result;
}

export function UploadConflictDialog({ name, open, onChoice }: {
  name: string;
  open: boolean;
  onChoice: (choice: "overwrite" | "skip" | "overwrite-all" | "skip-all") => void;
}) {
  return (
    <ModalOverlay className="modal-overlay" isOpen={open} isDismissable={false}>
      <Modal className="file-dialog-modal">
        <Dialog className="file-dialog upload-conflict-dialog" aria-label="上传冲突">
          <header><Heading slot="title">文件已存在</Heading><MoreHorizontal size={18} /></header>
          <p><strong>{name}</strong> 已存在于当前文件夹。</p>
          <div className="upload-conflict-actions">
            <button onClick={() => onChoice("overwrite")}>覆盖此文件</button>
            <button onClick={() => onChoice("skip")}>跳过此文件</button>
            <button className="primary" onClick={() => onChoice("overwrite-all")}>全部覆盖</button>
            <button onClick={() => onChoice("skip-all")}>跳过全部冲突</button>
          </div>
        </Dialog>
      </Modal>
    </ModalOverlay>
  );
}
