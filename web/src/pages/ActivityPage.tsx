import { useEffect, useState } from "react";
import { api } from "../api";
import { Empty, ErrorBanner, PageHeader, RefreshButton, Segmented, Status } from "../components/UI";
import { Reveal } from "../components/Motion";
import { TableSkeleton } from "../components/Skeleton";
import type { BackgroundJob, BucketDeletionJobPayload } from "../types";

const deletionStageLabels: Record<string, string> = {
  queued: "等待执行",
  fenced: "隔离本地访问",
  settled: "本地写入已收敛",
  clearing: "清空对象",
  deleting_bucket: "删除远端桶",
  validate_identity: "验证桶身份",
  fence_local: "隔离本地访问",
  settle_local: "等待上传结束",
  clear_objects: "清空对象",
  abort_multipart: "终止分片上传",
  delete_bucket: "删除远端桶",
  finalize_local: "清理本地登记",
};

function deletionPayload(job: BackgroundJob): BucketDeletionJobPayload {
  if (!job.payload) return {};
  if (typeof job.payload === "string") {
    try { return JSON.parse(job.payload) as BucketDeletionJobPayload; }
    catch { return {}; }
  }
  return job.payload as BucketDeletionJobPayload;
}

function jobTypeLabel(job: BackgroundJob) {
  if (job.type !== "r2.bucket.delete-remote") return job.type;
  const payload = deletionPayload(job);
  return <><strong>R2 删除桶</strong>{payload.bucket_name && <small>{payload.bucket_name}</small>}</>;
}

function jobProgress(job: BackgroundJob) {
  if (job.type !== "r2.bucket.delete-remote") return `${Math.round(job.progress * 100)}%`;
  const payload = deletionPayload(job);
  const stage = deletionStageLabels[payload.stage ?? ""] ?? "准备中";
  return <><span>{stage} · {Math.round(job.progress * 100)}%</span><small>已删 {payload.deleted_objects ?? 0} 个对象 · 已终止 {payload.aborted_multipart ?? 0} 个分片</small></>;
}

function jobError(job: BackgroundJob) {
  if (job.status !== "failed") return job.error ?? "";
  if (job.error_code === "rate_limited") return "Cloudflare 请求多次受限，自动重试已停止，请稍后手动重试。";
  if (job.error_code === "cloudflare_unavailable") return "Cloudflare 多次不可用，自动重试已停止，请稍后手动重试。";
  if (job.error_code === "bucket_busy") return "多次等待后桶内仍有写入或分片任务，自动重试已停止，请确认无写入后手动重试。";
  if (job.error_code === "bucket_locked") return "存储桶受 Cloudflare Bucket Lock 或保留策略保护，请先在 Cloudflare 中解除后再重试。";
  return job.error ?? "";
}

export function ActivityPage() {
  const [tab, setTab] = useState<"jobs" | "audit">("jobs");
  const [jobs, setJobs] = useState<BackgroundJob[]>([]);
  const [events, setEvents] = useState<Array<Record<string, unknown>>>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  async function load() {
    const [jobData, auditData] = await Promise.allSettled([
      api.get<{ jobs: BackgroundJob[] }>("/api/v1/jobs?limit=200"),
      api.get<{ events: Array<Record<string, unknown>> }>("/api/v1/audit?limit=200"),
    ]);
    if (jobData.status === "fulfilled") setJobs(jobData.value.jobs ?? []);
    if (auditData.status === "fulfilled") setEvents(auditData.value.events ?? []);
    const failed = [jobData, auditData].filter((item): item is PromiseRejectedResult => item.status === "rejected");
    if (failed.length > 0) setError((failed[0].reason as Error).message);
    setLoading(false);
  }
  useEffect(() => { void load(); }, []);

  const hasActiveJobs = jobs.some((job) => job.status === "pending" || job.status === "running");
  useEffect(() => {
    if (!hasActiveJobs) return;
    let cancelled = false;
    let timer = 0;
    async function pollJobs() {
      try {
        const data = await api.get<{ jobs: BackgroundJob[] }>("/api/v1/jobs?limit=200");
        if (!cancelled) setJobs(data.jobs ?? []);
      } catch (reason) {
        if (!cancelled) setError((reason as Error).message);
      } finally {
        if (!cancelled) timer = window.setTimeout(() => void pollJobs(), 2000);
      }
    }
    timer = window.setTimeout(() => void pollJobs(), 2000);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [hasActiveJobs]);

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
        {jobs.map((job) => <tr key={job.id}><td>{jobTypeLabel(job)}</td><td><Status value={job.status} /></td><td>{job.attempts ?? 0} / {job.max_attempts ?? 0}</td><td>{jobProgress(job)}</td><td className="danger-text">{job.error_code && <small className="mono">{job.error_code}</small>}{jobError(job)}</td><td>{job.updated_at ? new Date(job.updated_at).toLocaleString() : "—"}</td></tr>)}
      </tbody></table></div>) : (events.length === 0 ? <Empty>暂无审计事件</Empty> : <div className="table-wrap"><table><thead><tr><th>时间</th><th>操作</th><th>资源</th><th>协议</th><th>结果</th><th>请求 ID</th></tr></thead><tbody>
        {events.map((event) => <tr key={String(event.id)}><td>{new Date(String(event.created_at)).toLocaleString()}</td><td>{String(event.action)}</td><td className="mono">{String(event.resource)}</td><td>{String(event.protocol)}</td><td><Status value={String(event.result)} /></td><td className="mono">{String(event.request_id)}</td></tr>)}
      </tbody></table></div>)}</section></Reveal>
    </>
  );
}
