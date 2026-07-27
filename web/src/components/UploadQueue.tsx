import { Check, LoaderCircle, RotateCcw, Trash2, X } from "lucide-react";
import type { FileEntry } from "../types";
import { FileEntryIcon } from "./FileManagerDialogs";
import { formatBytes } from "./UI";

export type UploadStatus = "queued" | "uploading" | "conflict" | "success" | "error" | "skipped" | "cancelled";

export interface UploadItem {
  id: number;
  file: File;
  key: string;
  mountID: string;
  status: UploadStatus;
  progress: number;
  overwrite: boolean;
  error?: string;
}

export function UploadQueue({ items, onCancel, onRetry, onRemove, onClearFinished }: {
  items: UploadItem[];
  onCancel: (item: UploadItem) => void;
  onRetry: (item: UploadItem) => void;
  onRemove: (item: UploadItem) => void;
  onClearFinished: () => void;
}) {
  if (items.length === 0) return null;
  const finished = items.every((item) => ["success", "skipped", "cancelled"].includes(item.status));
  return (
    <section className="panel upload-queue" aria-label="上传队列">
      <div className="panel-heading"><h2>上传队列</h2>{finished && <button className="queue-clear" onClick={onClearFinished}>清除已完成</button>}</div>
      <div className="upload-list">
        {items.map((item) => {
          const entry: FileEntry = { name: item.file.name, key: item.key, kind: "file", size: item.file.size, content_type: item.file.type, last_modified: "", mount_id: item.mountID };
          return <div className={`upload-row ${item.status}`} key={item.id}>
            <FileEntryIcon entry={entry} size={17} />
            <div className="upload-name"><strong>{item.file.name}</strong><small>{formatBytes(item.file.size)}</small></div>
            <div className="upload-progress">
              <span><span style={{ width: `${Math.round(item.progress * 100)}%` }} /></span>
              <small>{uploadLabel(item)}</small>
            </div>
            <div className="row-actions">
              {item.status === "uploading" && <button className="icon-button" title="取消上传" onClick={() => onCancel(item)}><X size={15} /></button>}
              {item.status === "queued" && <button className="icon-button danger" title="移出队列" onClick={() => onRemove(item)}><Trash2 size={15} /></button>}
              {(["error", "cancelled"].includes(item.status)) && <button className="icon-button" title="重试" onClick={() => onRetry(item)}><RotateCcw size={15} /></button>}
              {item.status === "success" && <Check size={17} className="upload-success" aria-label="上传完成" />}
              {item.status === "uploading" && <LoaderCircle size={16} className="spin upload-spinner" aria-hidden="true" />}
            </div>
            {item.error && <p className="upload-error">{item.error}</p>}
          </div>;
        })}
      </div>
    </section>
  );
}

function uploadLabel(item: UploadItem) {
  switch (item.status) {
  case "queued": return "等待上传";
  case "uploading": return `${Math.round(item.progress * 100)}%`;
  case "conflict": return "等待处理冲突";
  case "success": return "上传完成";
  case "error": return "上传失败";
  case "skipped": return "已跳过";
  case "cancelled": return "已取消";
  }
}
