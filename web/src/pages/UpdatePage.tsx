import { useCallback, useEffect, useState } from "react";
import { Download, LoaderCircle } from "lucide-react";
import { api } from "../api";
import { ErrorBanner, PageHeader, RefreshButton } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";

interface UpdateInfo {
  current_version: string;
  latest_version?: string;
  update_available: boolean;
  release_notes?: string;
  published_at?: string;
  asset_name?: string;
}

export function UpdatePage() {
  const [info, setInfo] = useState<UpdateInfo | null>(null);
  const [error, setError] = useState("");
  const [updating, setUpdating] = useState(false);
  const [confirmUpdate, setConfirmUpdate] = useState(false);

  const load = useCallback(async () => {
    setError("");
    try {
      setInfo(await api.get<UpdateInfo>("/api/v1/system/update"));
    } catch (reason) {
      setError((reason as Error).message);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  // 更新已触发：轮询等待服务重启完成后刷新页面（新前端资源随二进制一起更新）。
  function waitForRestart() {
    setUpdating(true);
    const started = Date.now();
    const timer = window.setInterval(async () => {
      try {
        const response = await fetch("/healthz");
        if (response.ok) {
          window.clearInterval(timer);
          window.location.reload();
          return;
        }
      } catch { /* 服务尚未恢复 */ }
      if (Date.now() - started > 180_000) {
        window.clearInterval(timer);
        setUpdating(false);
        setError("等待重启超时，请检查服务器日志后手动刷新");
      }
    }, 2000);
  }

  return (
    <>
      <PageHeader title="软件更新" actions={<RefreshButton onRefresh={load} label="重新检查" />} />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      <Reveal><section className="panel">
        <div className="panel-heading">
          <h2>版本状态</h2>
          {info?.update_available && !updating && <span className="update-badge">发现新版本</span>}
        </div>
        <div className="update-body">
          {updating ? (
            <p className="update-progress"><LoaderCircle className="spin" size={15} />正在更新并重启服务，完成后页面将自动刷新…</p>
          ) : !info ? (
            <p className="update-muted">{error ? "无法获取更新信息，可点击右上角重新检查。" : "正在检查更新…"}</p>
          ) : info.update_available ? (
            <>
              <p>当前版本 <code>{info.current_version}</code>，最新版本 <strong>{info.latest_version}</strong>{info.published_at && `（发布于 ${new Date(info.published_at).toLocaleDateString()}）`}</p>
              {info.release_notes && <pre className="release-notes">{info.release_notes.slice(0, 4000)}</pre>}
              {info.asset_name
                ? <button className="primary" onClick={() => setConfirmUpdate(true)}><Download size={15} />更新并重启</button>
                : <p className="update-muted">该版本未提供当前平台的安装包，无法自动更新。</p>}
            </>
          ) : (
            <p>当前版本 <code>{info.current_version}</code>，已是最新。</p>
          )}
        </div>
      </section></Reveal>
      <ConfirmDialog
        open={confirmUpdate}
        title="更新并重启"
        danger={false}
        description={`下载 ${info?.latest_version ?? "新版本"} 并替换当前程序，随后服务将自动重启（通常需要十几秒，期间连接会短暂中断）。`}
        confirmLabel="开始更新"
        onOpenChange={setConfirmUpdate}
        onConfirm={async () => {
          await api.post("/api/v1/system/update", {});
          waitForRestart();
        }}
      />
    </>
  );
}
