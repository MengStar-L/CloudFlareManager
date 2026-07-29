import { useEffect, useRef, useState } from "react";
import { Database, Download, HardDrive, LoaderCircle, Users } from "lucide-react";
import { animate, useReducedMotion } from "motion/react";
import { api } from "../api";
import type { Account, R2Object } from "../types";
import { ErrorBanner, PageHeader, Status, formatBytes } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { TableSkeleton } from "../components/Skeleton";

interface UpdateInfo {
  current_version: string;
  latest_version?: string;
  update_available: boolean;
  release_notes?: string;
  published_at?: string;
  asset_name?: string;
}

interface AIUsageSummary {
  accounts: Array<{ estimated_used_neurons: number }>;
}

export function OverviewPage() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [objects, setObjects] = useState<R2Object[]>([]);
  const [jobs, setJobs] = useState<Array<Record<string, unknown>>>([]);
  const [usage, setUsage] = useState<AIUsageSummary>({ accounts: [] });
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [updateInfo, setUpdateInfo] = useState<UpdateInfo | null>(null);
  const [updateError, setUpdateError] = useState("");
  const [updating, setUpdating] = useState(false);
  const [confirmUpdate, setConfirmUpdate] = useState(false);

  useEffect(() => {
    // allSettled：任一数据源失败时，其余的仍照常展示。
    Promise.allSettled([
      api.get<{ accounts: Account[] }>("/api/v1/accounts"),
      api.get<{ objects: R2Object[] }>("/api/v1/r2/objects?limit=10"),
      api.get<{ jobs: Array<Record<string, unknown>> }>("/api/v1/jobs?limit=8"),
      api.get<AIUsageSummary>("/api/v1/ai/usage"),
    ]).then(([accountData, objectData, jobData, usageData]) => {
      if (accountData.status === "fulfilled") setAccounts(accountData.value.accounts ?? []);
      if (objectData.status === "fulfilled") setObjects(objectData.value.objects ?? []);
      if (jobData.status === "fulfilled") setJobs(jobData.value.jobs ?? []);
      if (usageData.status === "fulfilled") setUsage(usageData.value);
      const failed = [accountData, objectData, jobData, usageData]
        .filter((item): item is PromiseRejectedResult => item.status === "rejected");
      if (failed.length > 0) setError((failed[0].reason as Error).message);
    }).finally(() => setLoading(false));
    api.get<UpdateInfo>("/api/v1/system/update")
      .then(setUpdateInfo)
      .catch((reason) => setUpdateError((reason as Error).message));
  }, []);

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
        setUpdateError("等待重启超时，请检查服务器日志后手动刷新");
      }
    }, 2000);
  }

  const neurons = usage.accounts.reduce((sum, item) => sum + Number(item.estimated_used_neurons ?? 0), 0);
  const pendingJobs = jobs.filter((item) => item.status === "pending" || item.status === "running").length;

  return (
    <>
      <PageHeader title="概览" />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      <Reveal><section className="stat-band" aria-label="关键指标">
        <Stat label="Cloudflare 账号" value={String(accounts.length)} />
        <Stat label="索引对象" value={String(objects.length)} />
        <Stat label="今日估算 Neurons" value={neurons.toFixed(2)} />
        <Stat label="活动任务" value={String(pendingJobs)} />
      </section></Reveal>
      <div className="two-column">
        <Reveal delay={.06}><section className="panel">
          <div className="panel-heading"><h2>账号状态</h2><Users size={17} /></div>
          {loading ? <TableSkeleton columns={3} /> : <div className="table-wrap">
            <table><thead><tr><th>账号</th><th>状态</th><th>能力</th></tr></thead><tbody>
              {accounts.map((account) => <tr key={account.id}>
                <td><strong>{account.name}</strong><small>{account.cloudflare_account_id}</small></td>
                <td><Status value={account.health_status} /></td>
                <td>{account.capabilities?.filter((item) => item.available).length ?? 0} / {account.capabilities?.length ?? 0}</td>
              </tr>)}
              {accounts.length === 0 && <tr><td colSpan={3} className="table-empty">暂无账号</td></tr>}
            </tbody></table>
          </div>
          }
        </section></Reveal>
        <Reveal delay={.11}><section className="panel">
          <div className="panel-heading"><h2>最近对象</h2><HardDrive size={17} /></div>
          {loading ? <TableSkeleton columns={3} /> : <div className="table-wrap">
            <table><thead><tr><th>对象键</th><th>大小</th><th>状态</th></tr></thead><tbody>
              {objects.slice(0, 8).map((object) => <tr key={object.key}>
                <td className="mono">{object.key}</td><td>{formatBytes(object.size)}</td><td><Status value={object.state} /></td>
              </tr>)}
              {objects.length === 0 && <tr><td colSpan={3} className="table-empty">暂无对象</td></tr>}
            </tbody></table>
          </div>
          }
        </section></Reveal>
      </div>
      <Reveal delay={.16}><section className="panel compact-panel">
        <div className="panel-heading"><h2>最近任务</h2><Database size={17} /></div>
        {loading ? <TableSkeleton columns={4} /> : <div className="table-wrap"><table><thead><tr><th>类型</th><th>状态</th><th>进度</th><th>更新时间</th></tr></thead><tbody>
          {jobs.map((job) => <tr key={String(job.id)}><td>{String(job.type)}</td><td><Status value={String(job.status)} /></td><td>{Math.round(Number(job.progress) * 100)}%</td><td>{new Date(String(job.updated_at)).toLocaleString()}</td></tr>)}
          {jobs.length === 0 && <tr><td colSpan={4} className="table-empty">暂无任务</td></tr>}
        </tbody></table></div>}
      </section></Reveal>
      <Reveal delay={.2}><section className="panel compact-panel">
        <div className="panel-heading">
          <h2>软件更新</h2>
          {updateInfo?.update_available && !updating && <span className="update-badge">发现新版本</span>}
        </div>
        <div className="update-body">
          {updating ? (
            <p className="update-progress"><LoaderCircle className="spin" size={15} />正在更新并重启服务，完成后页面将自动刷新…</p>
          ) : updateError ? (
            <p className="update-muted">无法获取更新信息：{updateError}</p>
          ) : !updateInfo ? (
            <p className="update-muted">正在检查更新…</p>
          ) : updateInfo.update_available ? (
            <>
              <p>当前版本 <code>{updateInfo.current_version}</code>，最新版本 <strong>{updateInfo.latest_version}</strong>{updateInfo.published_at && `（发布于 ${new Date(updateInfo.published_at).toLocaleDateString()}）`}</p>
              {updateInfo.release_notes && <pre className="release-notes">{updateInfo.release_notes.slice(0, 800)}</pre>}
              {updateInfo.asset_name
                ? <button className="primary" onClick={() => setConfirmUpdate(true)}><Download size={15} />更新并重启</button>
                : <p className="update-muted">该版本未提供当前平台的安装包，无法自动更新。</p>}
            </>
          ) : (
            <p className="update-muted">当前版本 <code>{updateInfo.current_version}</code>，已是最新。</p>
          )}
        </div>
      </section></Reveal>
      <ConfirmDialog
        open={confirmUpdate}
        title="更新并重启"
        danger={false}
        description={`下载 ${updateInfo?.latest_version ?? "新版本"} 并替换当前程序，随后服务将自动重启（通常需要十几秒，期间连接会短暂中断）。`}
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

function Stat({ label, value }: { label: string; value: string }) {
  return <div className="stat"><span>{label}</span><AnimatedValue value={value} /></div>;
}

function AnimatedValue({ value }: { value: string }) {
  const reduced = useReducedMotion();
  const previous = useRef(0);
  const [display, setDisplay] = useState(value);

  useEffect(() => {
    const next = Number(value);
    if (reduced || !Number.isFinite(next)) { setDisplay(value); previous.current = next; return; }
    const decimals = value.includes(".") ? value.split(".")[1].length : 0;
    const controls = animate(previous.current, next, {
      duration: .55,
      ease: [0.22, 1, 0.36, 1],
      onUpdate: (current) => setDisplay(current.toFixed(decimals)),
    });
    previous.current = next;
    return () => controls.stop();
  }, [reduced, value]);

  return <strong>{display}</strong>;
}
