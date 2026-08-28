import { useCallback, useEffect, useRef, useState } from "react";
import { Database, HardDrive, Users } from "lucide-react";
import { animate, useReducedMotion } from "motion/react";
import { api } from "../api";
import type { Account, R2Object } from "../types";
import { ErrorBanner, PageHeader, RefreshButton, Status, formatBytes } from "../components/UI";
import { Reveal } from "../components/Motion";
import { TableSkeleton } from "../components/Skeleton";

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

  const load = useCallback(async () => {
    setError("");
    // allSettled：任一数据源失败时，其余的仍照常展示。
    const [accountData, objectData, jobData, usageData] = await Promise.allSettled([
      api.get<{ accounts: Account[] }>("/api/v1/accounts"),
      api.get<{ objects: R2Object[] }>("/api/v1/r2/objects?limit=10"),
      api.get<{ jobs: Array<Record<string, unknown>> }>("/api/v1/jobs?limit=8"),
      api.get<AIUsageSummary>("/api/v1/ai/usage"),
    ]);
    if (accountData.status === "fulfilled") setAccounts(accountData.value.accounts ?? []);
    if (objectData.status === "fulfilled") setObjects(objectData.value.objects ?? []);
    if (jobData.status === "fulfilled") setJobs(jobData.value.jobs ?? []);
    if (usageData.status === "fulfilled") setUsage(usageData.value);
    const failed = [accountData, objectData, jobData, usageData]
      .filter((item): item is PromiseRejectedResult => item.status === "rejected");
    if (failed.length > 0) setError((failed[0].reason as Error).message);
    setLoading(false);
  }, []);

  useEffect(() => { void load(); }, [load]);

  const neurons = usage.accounts.reduce((sum, item) => sum + Number(item.estimated_used_neurons ?? 0), 0);
  const pendingJobs = jobs.filter((item) => item.status === "pending" || item.status === "running").length;

  return (
    <>
      <PageHeader title="概览" actions={<RefreshButton onRefresh={load} />} />
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
