import { useId, useState, type KeyboardEvent, type ReactNode } from "react";
import { AlertTriangle, RefreshCw, UserPlus, X } from "lucide-react";
import { motion } from "motion/react";
import { Reveal } from "./Motion";

export function PageHeader({ title, actions, tabs }: { title: string; actions?: ReactNode; tabs?: ReactNode }) {
  return (
    <Reveal>
      <header className={tabs ? "page-header has-tabs" : "page-header"}>
        <div className="page-header-row">
          <h1>{title}</h1>
          <div className="header-actions">{actions}</div>
        </div>
        {tabs}
      </header>
    </Reveal>
  );
}

export interface SegmentedItem<T extends string> {
  id: T;
  label: string;
  icon?: ReactNode;
}

export function Segmented<T extends string>({ value, onChange, items, className, label }: {
  value: T;
  onChange: (value: T) => void;
  items: Array<SegmentedItem<T>>;
  className?: string;
  label?: string;
}) {
  const layoutID = useId();

  function handleKeyDown(event: KeyboardEvent<HTMLButtonElement>) {
    const current = items.findIndex((item) => item.id === value);
    let next = -1;
    if (event.key === "ArrowRight" || event.key === "ArrowDown") next = (current + 1) % items.length;
    else if (event.key === "ArrowLeft" || event.key === "ArrowUp") next = (current - 1 + items.length) % items.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = items.length - 1;
    if (next < 0) return;
    event.preventDefault();
    if (next === current) return;
    onChange(items[next].id);
    const tabs = event.currentTarget.closest('[role="tablist"]')?.querySelectorAll<HTMLButtonElement>('[role="tab"]');
    tabs?.[next]?.focus();
  }

  return (
    <div className={className ? `segmented ${className}` : "segmented"} role="tablist" aria-label={label}>
      {items.map((item) => (
        <button
          key={item.id}
          type="button"
          role="tab"
          tabIndex={value === item.id ? 0 : -1}
          className={value === item.id ? "active" : ""}
          aria-selected={value === item.id}
          onClick={() => onChange(item.id)}
          onKeyDown={handleKeyDown}
        >
          {value === item.id && <motion.span className="segmented-pill" layoutId={layoutID} transition={{ type: "spring", stiffness: 430, damping: 34 }} />}
          {item.icon}
          <span>{item.label}</span>
        </button>
      ))}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <Reveal><div className="empty-state">{children}</div></Reveal>;
}

export function RefreshButton({ onRefresh, label = "刷新" }: { onRefresh: () => Promise<unknown>; label?: string }) {
  const [busy, setBusy] = useState(false);
  return (
    <button
      className="icon-button"
      title={label}
      aria-label={label}
      disabled={busy}
      onClick={() => {
        setBusy(true);
        void Promise.resolve(onRefresh()).finally(() => setBusy(false));
      }}
    >
      <RefreshCw size={16} className={busy ? "spin" : undefined} />
    </button>
  );
}

/** 账号列表为空时的引路空态：功能页都依赖先添加 Cloudflare 账号。 */
export function NoAccountHint() {
  return (
    <Empty>
      <div className="no-account-hint">
        <p>尚未添加 Cloudflare 账号，先添加账号后即可使用此功能。</p>
        <button type="button" className="primary" onClick={() => { window.location.hash = "accounts"; }}>
          <UserPlus size={15} />去添加账号
        </button>
      </div>
    </Empty>
  );
}

export function ErrorBanner({ message, onClose }: { message: string; onClose?: () => void }) {
  return <div className="error-banner" role="alert"><AlertTriangle size={16} /><span>{message}</span>{onClose && <button className="icon-button" onClick={onClose} title="关闭"><X size={15} /></button>}</div>;
}

export function Status({ value, label }: { value: string; label?: string }) {
  const normalized = value.toLowerCase();
  const tone = ["healthy", "completed", "succeeded", "success", "committed", "available"].includes(normalized) ? "good" :
    ["error", "failed", "failure", "denied", "disabled", "delete_failed"].includes(normalized) ? "bad" :
    ["warning", "degraded"].includes(normalized) ? "warn" :
    ["pending", "running", "processing", "deleting"].includes(normalized) ? "live" : "neutral";
  return <span className={`status ${tone}`}><span />{label ?? value}</span>;
}

export function formatBytes(value: number) {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1000)), units.length - 1);
  return `${(value / 1000 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}
