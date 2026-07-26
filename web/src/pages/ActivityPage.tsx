import { useEffect, useState } from "react";
import { api } from "../api";
import { Empty, ErrorBanner, PageHeader, RefreshButton, Segmented, Status } from "../components/UI";
import { Reveal } from "../components/Motion";
import { TableSkeleton } from "../components/Skeleton";

export function ActivityPage() {
  const [tab, setTab] = useState<"jobs" | "audit">("jobs");
  const [jobs, setJobs] = useState<Array<Record<string, unknown>>>([]);
  const [events, setEvents] = useState<Array<Record<string, unknown>>>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  async function load() {
    const [jobData, auditData] = await Promise.allSettled([
      api.get<{ jobs: Array<Record<string, unknown>> }>("/api/v1/jobs?limit=200"),
      api.get<{ events: Array<Record<string, unknown>> }>("/api/v1/audit?limit=200"),
    ]);
    if (jobData.status === "fulfilled") setJobs(jobData.value.jobs ?? []);
    if (auditData.status === "fulfilled") setEvents(auditData.value.events ?? []);
    const failed = [jobData, auditData].filter((item): item is PromiseRejectedResult => item.status === "rejected");
    if (failed.length > 0) setError((failed[0].reason as Error).message);
    setLoading(false);
  }
  useEffect(() => { void load(); }, []);

  return (
    <>
      <PageHeader
        title="任务与审计"
        actions={<RefreshButton onRefresh={load} />}
        tabs={<Segmented
          label="活动视图"
          value={tab}
          onChange={setTab}
          items={[
            { id: "jobs", label: "后台任务" },
            { id: "audit", label: "审计事件" },
          ]}
        />}
      />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      <Reveal key={tab}><section className="panel">{loading ? <TableSkeleton columns={6} /> : tab === "jobs" ? (jobs.length === 0 ? <Empty>暂无任务</Empty> : <div className="table-wrap"><table><thead><tr><th>类型</th><th>状态</th><th>尝试</th><th>进度</th><th>错误</th><th>更新时间</th></tr></thead><tbody>
        {jobs.map((job) => <tr key={String(job.id)}><td>{String(job.type)}</td><td><Status value={String(job.status)} /></td><td>{String(job.attempts)} / {String(job.max_attempts)}</td><td>{Math.round(Number(job.progress) * 100)}%</td><td className="danger-text">{String(job.error ?? "")}</td><td>{new Date(String(job.updated_at)).toLocaleString()}</td></tr>)}
      </tbody></table></div>) : (events.length === 0 ? <Empty>暂无审计事件</Empty> : <div className="table-wrap"><table><thead><tr><th>时间</th><th>操作</th><th>资源</th><th>协议</th><th>结果</th><th>请求 ID</th></tr></thead><tbody>
        {events.map((event) => <tr key={String(event.id)}><td>{new Date(String(event.created_at)).toLocaleString()}</td><td>{String(event.action)}</td><td className="mono">{String(event.resource)}</td><td>{String(event.protocol)}</td><td><Status value={String(event.result)} /></td><td className="mono">{String(event.request_id)}</td></tr>)}
      </tbody></table></div>)}</section></Reveal>
    </>
  );
}
