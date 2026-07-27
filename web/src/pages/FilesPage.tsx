import { useCallback, useEffect, useMemo, useRef, useState, type ChangeEvent, type DragEvent } from "react";
import {
  ArrowLeft, ChevronRight, ChevronUp, FolderOpen, FolderPlus,
  LoaderCircle, MoreHorizontal, Upload,
} from "lucide-react";
import { APIError, api } from "../api";
import { ConfirmDialog } from "../components/ConfirmDialog";
import {
  downloadFile, FileContextMenu, FileDetailsDialog, FileEntryIcon, FilePreview,
  filePreviewKind, MoveDialog, NameDialog, UploadConflictDialog, type FileMenuAction,
} from "../components/FileManagerDialogs";
import { Empty, ErrorBanner, PageHeader, RefreshButton, formatBytes } from "../components/UI";
import { UploadQueue, type UploadItem } from "../components/UploadQueue";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";
import type { BackgroundJob, FileDirectoryList, FileEntry } from "../types";

let nextUploadID = 1;

interface ContextMenuState { entry: FileEntry; x: number; y: number }
interface MoveConflict { entry: FileEntry; destination: string }

export function FilesPage({ mountID, path, onNavigate }: { mountID: string; path: string; onNavigate: (mountID: string, path: string) => void }) {
  const [entries, setEntries] = useState<FileEntry[]>([]);
  const [directoryCount, setDirectoryCount] = useState(0);
  const [fileCount, setFileCount] = useState(0);
  const [mountName, setMountName] = useState("");
  const [nextMarker, setNextMarker] = useState("");
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState("");
  const [dragging, setDragging] = useState(false);
  const [contextMenu, setContextMenu] = useState<ContextMenuState | null>(null);
  const [previewTarget, setPreviewTarget] = useState<FileEntry | null>(null);
  const [detailsTarget, setDetailsTarget] = useState<FileEntry | null>(null);
  const [renameTarget, setRenameTarget] = useState<FileEntry | null>(null);
  const [moveTarget, setMoveTarget] = useState<FileEntry | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<FileEntry | null>(null);
  const [moveConflict, setMoveConflict] = useState<MoveConflict | null>(null);
  const [newFolderOpen, setNewFolderOpen] = useState(false);
  const [trackedJobs, setTrackedJobs] = useState<BackgroundJob[]>([]);
  const [uploads, setUploads] = useState<UploadItem[]>([]);
  const [uploadConflictID, setUploadConflictID] = useState<number | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const loadEpoch = useRef(0);
  const dragDepth = useRef(0);
  const uploadControllers = useRef(new Map<number, AbortController>());
  const startedUploads = useRef(new Set<number>());
  const conflictPolicy = useRef<"ask" | "overwrite" | "skip">("ask");
  const pollingJobs = useRef(false);
  const toast = useToast();

  const load = useCallback(async () => {
    const epoch = ++loadEpoch.current;
    setLoading(true);
    setError("");
    try {
      const query = new URLSearchParams({ path, limit: "200" });
      if (mountID) query.set("mount_id", mountID);
      const data = await api.get<FileDirectoryList>(`/api/v1/files?${query}`);
      if (epoch !== loadEpoch.current) return;
      setEntries(data.entries ?? []);
      setDirectoryCount(data.directory_count ?? 0);
      setFileCount(data.file_count ?? 0);
      setMountName(data.mount_name ?? "");
      setNextMarker(data.next_marker ?? "");
    } catch (reason) {
      if (epoch === loadEpoch.current) setError((reason as Error).message);
    } finally {
      if (epoch === loadEpoch.current) setLoading(false);
    }
  }, [mountID, path]);

  useEffect(() => { void load(); }, [load]);

  async function loadMore() {
    if (!nextMarker || loadingMore) return;
    setLoadingMore(true);
    try {
      const query = new URLSearchParams({ path, after: nextMarker, limit: "200" });
      if (mountID) query.set("mount_id", mountID);
      const data = await api.get<FileDirectoryList>(`/api/v1/files?${query}`);
      setEntries((current) => [...current, ...(data.entries ?? [])]);
      setNextMarker(data.next_marker ?? "");
    } catch (reason) { setError((reason as Error).message); }
    finally { setLoadingMore(false); }
  }

  function openEntry(entry: FileEntry) {
    setContextMenu(null);
    if (entry.kind === "mount") {
      if (entry.mount_id) onNavigate(entry.mount_id, "");
      return;
    }
    if (entry.kind === "directory") {
      onNavigate(mountID, entry.key);
      return;
    }
    if (!filePreviewKind(entry)) {
      downloadFile(entry);
      toast.show("已开始下载", "info");
      return;
    }
    setPreviewTarget(entry);
  }

  function showContextMenu(entry: FileEntry, x: number, y: number) {
    setContextMenu({ entry, x, y });
  }

  function contextAction(action: FileMenuAction) {
    const entry = contextMenu?.entry;
    setContextMenu(null);
    if (!entry) return;
    switch (action) {
    case "open": openEntry(entry); break;
    case "details": setDetailsTarget(entry); break;
    case "download": downloadFile(entry); break;
    case "rename": setRenameTarget(entry); break;
    case "move": setMoveTarget(entry); break;
    case "delete": setDeleteTarget(entry); break;
    }
  }

  function trackJob(job: BackgroundJob) {
    setTrackedJobs((current) => current.some((item) => item.id === job.id) ? current : [...current, job]);
  }

  const jobIDs = useMemo(() => trackedJobs.map((job) => job.id).sort().join(","), [trackedJobs]);
  useEffect(() => {
    if (!jobIDs) return;
    async function poll() {
      if (pollingJobs.current) return;
      pollingJobs.current = true;
      try {
        const ids = jobIDs.split(",");
        const responses = await Promise.allSettled(ids.map((id) => api.get<{ job: BackgroundJob }>(`/api/v1/jobs/${id}`)));
        const updates = new Map<string, BackgroundJob>();
        for (const response of responses) if (response.status === "fulfilled") updates.set(response.value.job.id, response.value.job);
        const succeeded = [...updates.values()].filter((job) => job.status === "succeeded");
        const failed = [...updates.values()].filter((job) => job.status === "failed");
        setTrackedJobs((current) => current.flatMap((job) => {
          const update = updates.get(job.id) ?? job;
          if (update.status === "succeeded" || update.status === "failed") return [];
          return [update];
        }));
        if (succeeded.length > 0) {
          toast.show("文件夹操作已完成");
          await load();
        }
        if (failed.length > 0) setError(failed[0].error || "文件夹操作失败");
      } finally { pollingJobs.current = false; }
    }
    void poll();
    const timer = window.setInterval(() => void poll(), 1000);
    return () => window.clearInterval(timer);
  }, [jobIDs, load, toast]);

  async function performMove(entry: FileEntry, destination: string, overwrite = false) {
    try {
      const result = await api.post<{ status: string; job?: BackgroundJob }>("/api/v1/files/operations", {
        mount_id: entry.mount_id ?? mountID, operation: "move", source: entry.key, destination, overwrite,
      });
      if (result.job) {
        trackJob(result.job);
        toast.show("文件夹操作已进入后台队列", "info");
      } else {
        toast.show("移动完成");
        await load();
      }
    } catch (reason) {
      if (reason instanceof APIError && reason.status === 409 && !overwrite) {
        setMoveConflict({ entry, destination });
        return;
      }
      throw reason;
    }
  }

  async function remove(entry: FileEntry) {
    const result = await api.post<{ status: string; job?: BackgroundJob }>("/api/v1/files/operations", {
      mount_id: entry.mount_id ?? mountID, operation: "delete", source: entry.key,
    });
    if (result.job) {
      trackJob(result.job);
      toast.show("递归删除已进入后台队列", "info");
    } else {
      toast.show("文件已删除");
      await load();
    }
  }

  function enqueueFiles(files: File[]) {
    if (!mountID || files.length === 0) return;
    if (!uploads.some((item) => ["queued", "uploading", "conflict"].includes(item.status))) conflictPolicy.current = "ask";
    const items = files.map<UploadItem>((file) => ({
      id: nextUploadID++, file, key: path + file.name, mountID, status: "queued", progress: 0, overwrite: false,
    }));
    setUploads((current) => [...current, ...items]);
  }

  useEffect(() => {
    const active = uploads.filter((item) => item.status === "uploading").length;
    const candidates = uploads.filter((item) => item.status === "queued" && !startedUploads.current.has(item.id)).slice(0, Math.max(0, 3 - active));
    if (candidates.length === 0) return;
    const ids = new Set(candidates.map((item) => item.id));
    candidates.forEach((item) => startedUploads.current.add(item.id));
    setUploads((current) => current.map((item) => ids.has(item.id) ? { ...item, status: "uploading" } : item));
    candidates.forEach((item) => void startUpload(item));
  }, [uploads]);

  async function startUpload(item: UploadItem) {
    const controller = new AbortController();
    uploadControllers.current.set(item.id, controller);
    try {
      const query = new URLSearchParams({ mount_id: item.mountID, key: item.key, overwrite: String(item.overwrite) });
      await api.upload(`/api/v1/files/content?${query}`, item.file, (progress) => {
        setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, progress } : currentItem));
      }, controller.signal);
      setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, status: "success", progress: 1, error: undefined } : currentItem));
      toast.show(`${item.file.name} 上传完成`);
      await load();
    } catch (reason) {
      if (reason instanceof DOMException && reason.name === "AbortError") {
        setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, status: "cancelled", error: undefined } : currentItem));
      } else if (reason instanceof APIError && reason.status === 409) {
        if (conflictPolicy.current === "overwrite" && !item.overwrite) {
          setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, status: "queued", overwrite: true, progress: 0 } : currentItem));
        } else if (conflictPolicy.current === "skip") {
          setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, status: "skipped", progress: 0 } : currentItem));
        } else {
          setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, status: "conflict", progress: 0 } : currentItem));
          setUploadConflictID((current) => current ?? item.id);
        }
      } else {
        setUploads((current) => current.map((currentItem) => currentItem.id === item.id ? { ...currentItem, status: "error", error: (reason as Error).message } : currentItem));
      }
    } finally {
      uploadControllers.current.delete(item.id);
      startedUploads.current.delete(item.id);
    }
  }

  function resolveUploadConflict(choice: "overwrite" | "skip" | "overwrite-all" | "skip-all") {
    if (uploadConflictID == null) return;
    if (choice === "overwrite-all") conflictPolicy.current = "overwrite";
    if (choice === "skip-all") conflictPolicy.current = "skip";
    setUploads((current) => current.map((item) => {
      if (choice === "overwrite-all" && (item.status === "conflict" || item.status === "queued")) return { ...item, status: "queued", overwrite: true, progress: 0 };
      if (choice === "skip-all" && item.status === "conflict") return { ...item, status: "skipped", progress: 0 };
      if (item.id !== uploadConflictID) return item;
      if (choice === "overwrite") return { ...item, status: "queued", overwrite: true, progress: 0 };
      if (choice === "skip") return { ...item, status: "skipped", progress: 0 };
      return item;
    }));
    const next = choice.endsWith("all") ? undefined : uploads.find((item) => item.status === "conflict" && item.id !== uploadConflictID);
    setUploadConflictID(next?.id ?? null);
  }

  function onFileInput(event: ChangeEvent<HTMLInputElement>) {
    enqueueFiles(Array.from(event.target.files ?? []));
    event.target.value = "";
  }

  function onDragEnter(event: DragEvent) {
    event.preventDefault();
    dragDepth.current++;
    if (event.dataTransfer.types.includes("Files")) setDragging(true);
  }

  function onDragLeave(event: DragEvent) {
    event.preventDefault();
    dragDepth.current--;
    if (dragDepth.current <= 0) { dragDepth.current = 0; setDragging(false); }
  }

  function onDrop(event: DragEvent) {
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    enqueueFiles(Array.from(event.dataTransfer.files));
  }

  const segments = path.split("/").filter(Boolean);
  const uploadConflict = uploads.find((item) => item.id === uploadConflictID);

  return (
    <div className={dragging ? "file-manager is-dragging" : "file-manager"} onDragEnter={onDragEnter} onDragOver={(event) => event.preventDefault()} onDragLeave={onDragLeave} onDrop={onDrop}>
      <PageHeader title="文件管理" actions={<>
        <RefreshButton onRefresh={load} />
        {mountID && <><button className="file-command" onClick={() => setNewFolderOpen(true)}><FolderPlus size={16} />新建文件夹</button>
          <button className="primary" onClick={() => fileInput.current?.click()}><Upload size={16} />上传文件</button>
          <input ref={fileInput} className="visually-hidden" type="file" multiple onChange={onFileInput} /></>}
      </>} />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      {trackedJobs.length > 0 && <div className="file-task-strip" role="status">
        {trackedJobs.map((job) => <div key={job.id}><LoaderCircle className="spin" size={15} /><span>{job.type === "r2.files.delete" ? "正在删除文件夹" : "正在移动文件夹"}</span><progress value={job.progress} max={1} /><strong>{Math.round(job.progress * 100)}%</strong></div>)}
      </div>}
      <section className="panel file-browser">
        <div className="file-toolbar">
          <div className="file-history-actions">
            <button className="icon-button" title="后退" onClick={() => window.history.back()}><ArrowLeft size={16} /></button>
            <button className="icon-button" title="上一级" disabled={!mountID} onClick={() => path ? onNavigate(mountID, parentPath(path)) : onNavigate("", "")}><ChevronUp size={17} /></button>
          </div>
          <nav className="file-breadcrumb" aria-label="当前文件夹">
            <button className={!mountID ? "active" : ""} onClick={() => onNavigate("", "")}><FolderOpen size={16} /><span>根目录</span></button>
            {mountID && <span><ChevronRight size={14} /><button className={!path ? "active" : ""} onClick={() => onNavigate(mountID, "")}>{mountName}</button></span>}
            {segments.map((segment, index) => <span key={`${segment}-${index}`}><ChevronRight size={14} /><button className={index === segments.length - 1 ? "active" : ""} onClick={() => onNavigate(mountID, segments.slice(0, index + 1).join("/") + "/")}>{segment}</button></span>)}
          </nav>
          {mountID && <div className="file-count">{directoryCount} 个文件夹，{fileCount} 个文件</div>}
        </div>
        {loading ? <TableSkeleton columns={5} rows={7} /> : entries.length === 0 ? <Empty><div className="file-empty"><FolderOpen size={32} /><p>此文件夹为空</p></div></Empty> : <div className="table-wrap"><table className="file-table">
          <thead><tr><th>名称</th><th>大小</th><th>类型</th><th>修改时间</th><th aria-label="操作" /></tr></thead>
          <tbody>{entries.map((entry) => <tr key={`${entry.kind}:${entry.mount_id ?? ""}:${entry.key}`} className={entry.disabled ? "file-mount-disabled" : ""} onClick={() => openEntry(entry)} onContextMenu={(event) => { if (entry.kind === "mount") return; event.preventDefault(); showContextMenu(entry, event.clientX, event.clientY); }}>
            <td><button className="file-entry-name" onClick={(event) => { event.stopPropagation(); openEntry(entry); }}><FileEntryIcon entry={entry} /><span>{entry.name}</span></button></td>
            <td>{entry.kind !== "file" ? "--" : formatBytes(entry.size)}</td>
            <td>{entry.kind === "mount" ? (entry.disabled ? "WebDAV 挂载点（已撤销）" : "WebDAV 挂载点") : entry.kind === "directory" ? "文件夹" : entry.content_type || "未知"}</td>
            <td>{new Date(entry.last_modified).toLocaleString()}</td>
            <td className="row-actions">{entry.kind !== "mount" && <button className="icon-button file-more" title="更多操作" onClick={(event) => { event.stopPropagation(); const rect = event.currentTarget.getBoundingClientRect(); showContextMenu(entry, rect.right - 210, rect.bottom + 4); }}><MoreHorizontal size={16} /></button>}</td>
          </tr>)}</tbody>
        </table></div>}
        {nextMarker && <div className="file-load-more"><button onClick={() => void loadMore()} disabled={loadingMore}>{loadingMore && <LoaderCircle className="spin" size={15} />}加载更多</button></div>}
      </section>
      <UploadQueue
        items={uploads}
        onCancel={(item) => uploadControllers.current.get(item.id)?.abort()}
        onRetry={(item) => setUploads((current) => current.map((candidate) => candidate.id === item.id ? { ...candidate, status: "queued", progress: 0, error: undefined, overwrite: false } : candidate))}
        onRemove={(item) => setUploads((current) => current.filter((candidate) => candidate.id !== item.id))}
        onClearFinished={() => setUploads((current) => current.filter((item) => !["success", "skipped", "cancelled"].includes(item.status)))}
      />
      {contextMenu && <FileContextMenu {...contextMenu} onAction={contextAction} onClose={() => setContextMenu(null)} />}
      <FilePreview entry={previewTarget} open={Boolean(previewTarget)} onOpenChange={(open) => { if (!open) setPreviewTarget(null); }} />
      <FileDetailsDialog entry={detailsTarget} open={Boolean(detailsTarget)} onOpenChange={(open) => { if (!open) setDetailsTarget(null); }} />
      <NameDialog
        open={newFolderOpen} title="新建文件夹" label="文件夹名称" confirmLabel="创建"
        onOpenChange={setNewFolderOpen}
        onConfirm={async (name) => { await api.post("/api/v1/files/directories", { mount_id: mountID, path: `${path}${name}/` }); toast.show("文件夹已创建"); await load(); }}
      />
      <NameDialog
        open={Boolean(renameTarget)} title="重命名" label="新名称" initialValue={renameTarget?.name} confirmLabel="保存"
        onOpenChange={(open) => { if (!open) setRenameTarget(null); }}
        onConfirm={async (name) => {
          if (!renameTarget) return;
          const destination = `${parentPath(renameTarget.key)}${name}${renameTarget.kind === "directory" ? "/" : ""}`;
          if (destination === renameTarget.key) throw new Error("名称未发生变化");
          await performMove(renameTarget, destination);
        }}
      />
      <MoveDialog
        entry={moveTarget} open={Boolean(moveTarget)} onOpenChange={(open) => { if (!open) setMoveTarget(null); }}
        onMove={async (directory) => { if (moveTarget) await performMove(moveTarget, `${directory}${moveTarget.name}${moveTarget.kind === "directory" ? "/" : ""}`); }}
      />
      <ConfirmDialog
        open={Boolean(deleteTarget)} title={deleteTarget?.kind === "directory" ? "删除文件夹" : "删除文件"}
        description={deleteTarget?.kind === "directory" ? `确定递归删除“${deleteTarget.name}”及其中全部内容？此操作无法撤销。` : `确定删除“${deleteTarget?.name ?? ""}”？此操作无法撤销。`}
        confirmLabel="删除" onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        onConfirm={() => deleteTarget ? remove(deleteTarget) : Promise.resolve()}
      />
      <ConfirmDialog
        open={Boolean(moveConflict)} title="目标位置已有同名内容"
        description={moveConflict?.entry.kind === "directory" ? "确认后将合并文件夹并覆盖同路径文件，目标文件夹中的其他内容会保留。" : "确认覆盖目标位置的同名文件？"}
        confirmLabel="确认覆盖" danger={false} onOpenChange={(open) => { if (!open) setMoveConflict(null); }}
        onConfirm={() => moveConflict ? performMove(moveConflict.entry, moveConflict.destination, true) : Promise.resolve()}
      />
      <UploadConflictDialog name={uploadConflict?.file.name ?? ""} open={Boolean(uploadConflict)} onChoice={resolveUploadConflict} />
      {dragging && <div className="file-drop-overlay" aria-hidden="true"><Upload size={32} /></div>}
    </div>
  );
}

function parentPath(key: string) {
  const trimmed = key.endsWith("/") ? key.slice(0, -1) : key;
  const index = trimmed.lastIndexOf("/");
  return index >= 0 ? trimmed.slice(0, index + 1) : "";
}
