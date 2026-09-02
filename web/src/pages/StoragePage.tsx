import { FormEvent, useEffect, useRef, useState } from "react";
import { ArrowRightLeft, Boxes, File, FolderInput, Gauge, Plus, RefreshCw, RotateCcw, ScanSearch, Search, Trash2, Unlink, Wrench } from "lucide-react";
import { api } from "../api";
import type {
  Account,
  BackgroundJob,
  Bucket,
  BucketDeleteConfirmation,
  BucketDeletionJobPayload,
  BucketDeletionMode,
  R2AccountUsage,
  R2Object,
  RemoteBucketView,
} from "../types";
import { Empty, ErrorBanner, NoAccountHint, PageHeader, RefreshButton, Segmented, Status, formatBytes } from "../components/UI";
import { BucketDeleteDialog } from "../components/BucketDeleteDialog";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { SelectField } from "../components/SelectField";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

interface RemoteUsageSummary {
  free_tier_bytes: number;
  total_bytes?: number;
  remaining_bytes?: number;
  usage_error?: string;
}

interface AccountBucketsView {
  account_id: string;
  account_name: string;
  buckets?: RemoteBucketView[] | null;
  usage?: RemoteUsageSummary;
  error?: string;
}

interface RemoteDeleteTarget {
  view: RemoteBucketView;
  accountID: string;
  accountName?: string;
}

const bucketDeletionStageLabels: Record<string, string> = {
  queued: "等待执行",
  fenced: "隔离本地访问",
  settled: "本地写入已收敛",
  clearing: "清空桶内文件",
  deleting_bucket: "删除存储桶",
  validate_identity: "验证桶身份",
  fence_local: "隔离本地访问",
  settle_local: "等待写入结束",
  clear_objects: "清空桶内文件",
  abort_multipart: "终止分片上传",
  delete_bucket: "删除存储桶",
  finalize_local: "清理本地登记",
};

const bucketDeletionErrorLabels: Record<string, string> = {
  bucket_not_empty: "桶内仍有文件。请先删除桶内所有文件，或重试并选择“一键清空并删除桶”。",
  bucket_busy: "桶仍有写入或分片任务正在收尾，请稍后重试。",
  bucket_deleting: "该桶已有删除任务正在运行。",
  bucket_locked: "该桶受 Cloudflare Bucket Lock 或保留策略保护，请先在 Cloudflare 中解除后再重试。",
  permission_denied: "Cloudflare API Token 没有删除该桶所需的权限。",
  s3_credentials_required: "桶内存在未完成的分片上传，请先配置 R2 S3 访问密钥。",
  external_writes_detected: "清空期间检测到新的写入，请停止其他程序写入后重试。",
  bucket_identity_changed: "检测到同名桶已经变化，为避免误删，任务已停止。",
  bucket_identity_unverifiable: "无法核验远端桶身份，为避免误删，任务已停止。",
  unsupported_jurisdiction: "当前版本仅支持删除默认管辖区的桶。",
  partial_delete_failed: "部分文件删除失败。未成功删除的文件仍保留在桶内，请检查权限后重试。",
  rate_limited: "Cloudflare 请求多次受限，自动重试已停止，请稍后手动重试。",
  cloudflare_unavailable: "Cloudflare 多次不可用，自动重试已停止，请稍后手动重试。",
  local_finalize_failed: "远端桶已处理，但本地登记清理失败，请重试。",
};

function deletionPayload(job?: BackgroundJob): BucketDeletionJobPayload {
  if (!job?.payload) return {};
  if (typeof job.payload === "string") {
    try { return JSON.parse(job.payload) as BucketDeletionJobPayload; }
    catch { return {}; }
  }
  return job.payload as BucketDeletionJobPayload;
}

function creationIdentity(value?: string) {
  if (!value) return "";
  const match = value.trim().match(/^(.+:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/i);
  if (!match) return "";
  const wholeSecond = Date.parse(`${match[1]}${match[3]}`);
  if (!Number.isFinite(wholeSecond)) return "";
  return `${wholeSecond}:${(match[2] ?? "").padEnd(9, "0")}`;
}

function sameDeletionIdentity(job: BackgroundJob, view: RemoteBucketView) {
  if (view.remote_missing) return true;
  const expected = creationIdentity(deletionPayload(job).expected_creation_date);
  const current = creationIdentity(view.creation_date);
  return expected !== "" && current !== "" && expected === current;
}

function overviewBucketStatus(view: RemoteBucketView) {
  if (view.lifecycle_state === "deleting" || view.deletion_status === "pending" || view.deletion_status === "running") {
    return <Status value="running" label="正在删除" />;
  }
  if (view.lifecycle_state === "delete_failed" || view.deletion_status === "failed") {
    const detail = bucketDeletionErrorLabels[view.deletion_error_code ?? ""] || view.deletion_error;
    return <div className="bucket-job-state"><Status value="error" label="删除失败" />{detail && <small className="danger-text">{detail}</small>}</div>;
  }
  if (view.remote_missing) return <Status value="error" label="远端已不存在" />;
  if (view.managed) return <Status value={view.health_status || "healthy"} label="阵列中" />;
  return <Status value="unmanaged" label="未纳入" />;
}

export function StoragePage() {
  const [tab, setTab] = useState<"objects" | "overview" | "buckets" | "maintenance">("objects");
  const [objects, setObjects] = useState<R2Object[]>([]);
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [accountUsage, setAccountUsage] = useState<R2AccountUsage[]>([]);
  const [prefix, setPrefix] = useState("");
  const [findings, setFindings] = useState<Array<{ id: string; physical_bucket_id: string; physical_key: string; kind: string; detail?: string; found_at: string }>>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [bucketAccountID, setBucketAccountID] = useState("");
  const [remoteList, setRemoteList] = useState<RemoteBucketView[]>([]);
  const [usageSummary, setUsageSummary] = useState<RemoteUsageSummary | null>(null);
  const [remoteError, setRemoteError] = useState("");
  const [remoteLoading, setRemoteLoading] = useState(false);
  const [newBucketName, setNewBucketName] = useState("");
  const remoteEpoch = useRef(0);
  const [overview, setOverview] = useState<AccountBucketsView[] | null>(null);
  const [overviewLoading, setOverviewLoading] = useState(false);
  const overviewEpoch = useRef(0);
  const [sourceBucketID, setSourceBucketID] = useState("");
  const [targetBucketID, setTargetBucketID] = useState("");
  const [unlinkTarget, setUnlinkTarget] = useState<Bucket | null>(null);
  const [remoteDeleteTarget, setRemoteDeleteTarget] = useState<RemoteDeleteTarget | null>(null);
  const [remoteDeleteInitialMode, setRemoteDeleteInitialMode] = useState<BucketDeletionMode>("empty_only");
  const [deletionJobs, setDeletionJobs] = useState<BackgroundJob[]>([]);
  const deletionEpoch = useRef(0);
  const deletionPolling = useRef(false);
  const toast = useToast();

  async function load(filter = prefix) {
    try {
      const [objectData, bucketData, accountData, findingData] = await Promise.all([
        api.get<{ objects: R2Object[] }>(`/api/v1/r2/objects?limit=500&prefix=${encodeURIComponent(filter)}`),
        api.get<{ buckets: Bucket[]; account_usage: R2AccountUsage[] }>("/api/v1/r2/buckets"),
        api.get<{ accounts: Account[] }>("/api/v1/accounts"),
        api.get<{ findings: typeof findings }>("/api/v1/r2/findings?limit=500"),
      ]);
      const nextAccounts = accountData.accounts ?? [];
      const nextBuckets = bucketData.buckets ?? [];
      setObjects(objectData.objects ?? []); setBuckets(nextBuckets); setAccountUsage(bucketData.account_usage ?? []); setAccounts(nextAccounts); setFindings(findingData.findings ?? []);
      setBucketAccountID((current) => nextAccounts.some((item) => item.id === current) ? current : (nextAccounts[0]?.id ?? ""));
      setSourceBucketID((current) => nextBuckets.some((item) => item.id === current) ? current : "");
      setTargetBucketID((current) => nextBuckets.some((item) => item.id === current) ? current : "");
    } catch (reason) { setError((reason as Error).message); }
    finally { setLoading(false); }
  }
  useEffect(() => { void load(""); }, []);

  // 选中账号后从 Cloudflare 拉取全部真实存在的桶（含用量与阵列状态）。
  async function loadRemote(id = bucketAccountID) {
    if (!id) { setRemoteList([]); setUsageSummary(null); return; }
    const epoch = ++remoteEpoch.current;
    setRemoteLoading(true);
    setRemoteError("");
    try {
      const data = await api.get<{ buckets: RemoteBucketView[] | null; usage: RemoteUsageSummary }>(`/api/v1/r2/remote-buckets?account_id=${encodeURIComponent(id)}`);
      if (epoch !== remoteEpoch.current) return;
      const views = data.buckets ?? [];
      setRemoteList(views);
      setUsageSummary(data.usage ?? null);
      const known = new Set(deletionJobs.map((job) => job.id));
      const missingIDs = [...new Set(views.map((view) => view.deletion_job_id).filter(
        (jobID): jobID is string => typeof jobID === "string" && jobID !== "" && !known.has(jobID),
      ))];
      if (missingIDs.length > 0) {
        const responses = await Promise.allSettled(missingIDs.map((jobID) => api.get<{ job: BackgroundJob }>(`/api/v1/jobs/${jobID}`)));
        if (epoch !== remoteEpoch.current) return;
        const recovered = responses.flatMap((result) => result.status === "fulfilled" ? [result.value.job] : []);
        if (recovered.length > 0) {
          setDeletionJobs((current) => {
            const recoveredIDs = new Set(recovered.map((job) => job.id));
            return [...current.filter((job) => !recoveredIDs.has(job.id)), ...recovered];
          });
        }
      }
    } catch (reason) {
      if (epoch === remoteEpoch.current) { setRemoteList([]); setUsageSummary(null); setRemoteError((reason as Error).message); }
    } finally {
      if (epoch === remoteEpoch.current) setRemoteLoading(false);
    }
  }
  async function loadDeletionJobs(id = bucketAccountID) {
    if (!id) { setDeletionJobs([]); return; }
    const epoch = ++deletionEpoch.current;
    const path = `/api/v1/jobs?limit=200&type=r2.bucket.delete-remote&resource_key_prefix=${encodeURIComponent(`${id}/`)}`;
    try {
      const data = await api.get<{ jobs: BackgroundJob[] }>(path);
      if (epoch === deletionEpoch.current) {
        const listed = data.jobs ?? [];
        setDeletionJobs((current) => {
          const listedIDs = new Set(listed.map((job) => job.id));
          const retained = current.filter((job) => job.resource_key?.startsWith(`${id}/`) && !listedIDs.has(job.id));
          return [...listed, ...retained];
        });
      }
    } catch (reason) {
      if (epoch === deletionEpoch.current) setError((reason as Error).message);
    }
  }

  useEffect(() => {
    setNewBucketName("");
    void Promise.all([loadRemote(bucketAccountID), loadDeletionJobs(bucketAccountID)]);
  }, [bucketAccountID]);

  const hasActiveDeletionJobs = deletionJobs.some((job) => job.status === "pending" || job.status === "running");
  useEffect(() => {
    if (!hasActiveDeletionJobs) {
      if (deletionPolling.current) {
        deletionPolling.current = false;
        if (tab === "overview") {
          void Promise.all([loadRemote(bucketAccountID), load(), loadOverview()]);
        } else {
          setOverview(null);
          void Promise.all([loadRemote(bucketAccountID), load()]);
        }
      }
      return;
    }
    deletionPolling.current = true;
    let cancelled = false;
    let timer = window.setTimeout(async function poll() {
      await loadDeletionJobs(bucketAccountID);
      if (!cancelled) timer = window.setTimeout(poll, 2000);
    }, 2000);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [bucketAccountID, hasActiveDeletionJobs, tab]);

  // 跨账号桶概览：进入页签时按需加载；相关操作后置空以便下次重新拉取。
  async function loadOverview() {
    const epoch = ++overviewEpoch.current;
    setOverviewLoading(true);
    try {
      const data = await api.get<{ accounts: AccountBucketsView[] | null }>("/api/v1/r2/overview");
      if (epoch === overviewEpoch.current) setOverview(data.accounts ?? []);
    } catch (reason) { if (epoch === overviewEpoch.current) setError((reason as Error).message); }
    finally { if (epoch === overviewEpoch.current) setOverviewLoading(false); }
  }
  useEffect(() => { if (tab === "overview") void loadOverview(); }, [tab]);

  // 远程列表不可用时的手动登记回退。
  async function addBucket(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    const name = String(form.get("name") ?? "");
    if (busy || !name) return;
    setBusy(true);
    try {
      await api.post("/api/v1/r2/buckets", { account_id: bucketAccountID, name, adopted: form.get("adopted") === "on" });
      formElement.reset(); toast.show("物理桶已登记"); await load();
    } catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }

  async function createRemoteBucket(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const name = newBucketName.trim();
    if (busy || !bucketAccountID || !name) return;
    setBusy(true);
    try {
      await api.post("/api/v1/r2/remote-buckets", { account_id: bucketAccountID, name });
      setNewBucketName("");
      setOverview(null);
      toast.show(`存储桶 ${name} 已创建`);
      await loadRemote();
    } catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }

  // 纳入阵列：本地登记 + 自动触发接管扫描，让已有对象进入索引。
  async function enroll(view: RemoteBucketView) {
    if (busy) return;
    setBusy(true);
    try {
      const bucket = await api.post<{ id: string }>("/api/v1/r2/buckets", { account_id: bucketAccountID, name: view.name, adopted: true });
      await api.post(`/api/v1/r2/buckets/${bucket.id}/adopt`, {});
      setOverview(null);
      toast.show(`${view.name} 已纳入阵列，接管扫描任务已创建`);
      await Promise.all([load(), loadRemote()]);
    } catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }

  async function unlinkBucket(bucket: Bucket) {
    try { await api.delete(`/api/v1/r2/buckets/${bucket.id}`); setOverview(null); toast.show("已移出阵列（Cloudflare 中的桶与对象不受影响）"); await Promise.all([load(), loadRemote()]); }
    catch (reason) { setError((reason as Error).message); throw reason; }
  }

  function openRemoteDeletion(view: RemoteBucketView, mode: BucketDeletionMode = "empty_only") {
    setRemoteDeleteInitialMode(mode);
    setRemoteDeleteTarget({
      view,
      accountID: bucketAccountID,
      accountName: accounts.find((account) => account.id === bucketAccountID)?.name,
    });
  }

  async function deleteRemoteBucket(confirmation: BucketDeleteConfirmation) {
    if (!remoteDeleteTarget) return;
    const target = remoteDeleteTarget;
    const data = await api.post<{ job: BackgroundJob; created: boolean }>(
      `/api/v1/r2/remote-buckets/${encodeURIComponent(target.view.name)}/deletions`,
      {
        account_id: target.accountID,
        jurisdiction: target.view.jurisdiction || "default",
        mode: confirmation.mode,
        confirmation_name: confirmation.confirmationName,
        admin_password: confirmation.adminPassword,
      },
    );
    setDeletionJobs((current) => [data.job, ...current.filter((job) => job.id !== data.job.id)]);
    setOverview(null);
    toast.show(data.created ? `已创建删除任务：${target.view.name}` : `该桶已有删除任务：${target.view.name}`, "info");
    if (target.accountID === bucketAccountID) {
      void Promise.all([loadRemote(target.accountID), loadDeletionJobs(target.accountID)]);
    }
  }

  function findDeletionCandidate(view: RemoteBucketView) {
    const key = `${bucketAccountID}/${view.jurisdiction || "default"}/${view.name}`;
    const candidates = deletionJobs.filter((job) => job.status !== "succeeded" && (job.resource_key === key || job.id === view.deletion_job_id));
    return candidates.find((job) => job.status === "pending" || job.status === "running") ??
      candidates.find((job) => job.id === view.deletion_job_id) ?? candidates[0];
  }

  function findDeletionJob(view: RemoteBucketView) {
    const candidate = findDeletionCandidate(view);
    return candidate && sameDeletionIdentity(candidate, view) ? candidate : undefined;
  }

  function deletionIdentityIssue(view: RemoteBucketView) {
    if (view.remote_missing) return "";
    const candidate = findDeletionCandidate(view);
    if (!candidate) {
      return (view.lifecycle_state === "delete_failed" ||
        (view.lifecycle_state === "deleting" && view.deletion_status === "failed")) && view.deletion_job_id
        ? "无法核验原删除任务与当前存储桶是否为同一个桶，已禁用重试。"
        : "";
    }
    if (sameDeletionIdentity(candidate, view)) return "";
    const identityRequired = candidate.status === "pending" || candidate.status === "running" ||
      view.lifecycle_state === "delete_failed" || candidate.id === view.deletion_job_id;
    if (!identityRequired && creationIdentity(deletionPayload(candidate).expected_creation_date) && creationIdentity(view.creation_date)) {
      return "";
    }
    return creationIdentity(deletionPayload(candidate).expected_creation_date) && creationIdentity(view.creation_date)
      ? "检测到同名存储桶已重新创建，旧删除任务不会用于当前存储桶。"
      : "无法核验原删除任务与当前存储桶是否为同一个桶，已禁用重试。";
  }

  function renderRemoteBucketStatus(view: RemoteBucketView) {
    const identityIssue = deletionIdentityIssue(view);
    if (identityIssue) {
      return <div className="bucket-job-state"><Status value="error" label="桶身份已变化" /><small className="danger-text">{identityIssue}</small></div>;
    }
    const job = findDeletionJob(view);
    if (job?.status === "pending" || job?.status === "running") {
      const payload = deletionPayload(job);
      const stage = bucketDeletionStageLabels[payload.stage ?? ""] ?? "执行删除";
      return <div className="bucket-job-state"><Status value={job.status} label={`${stage} ${Math.round(job.progress * 100)}%`} /><small>已删 {payload.deleted_objects ?? 0} 个文件，已终止 {payload.aborted_multipart ?? 0} 个分片</small></div>;
    }
    if (job?.status === "failed" || view.deletion_status === "failed" || view.lifecycle_state === "delete_failed") {
      const code = job?.error_code || view.deletion_error_code || "";
      const detail = bucketDeletionErrorLabels[code] || job?.error || view.deletion_error || "删除失败，请检查任务详情后重试。";
      return <div className="bucket-job-state"><Status value="error" label="删除失败" /><small className="danger-text">{detail}</small></div>;
    }
    if (view.lifecycle_state === "deleting") {
      return <Status value="running" label="正在删除" />;
    }
    if (view.remote_missing) return <Status value="error" label="远端已不存在" />;
    if (view.managed) return <Status value={view.health_status || "healthy"} label="阵列中" />;
    return <Status value="unmanaged" label="未纳入" />;
  }

  async function schedule(path: string, body: unknown = {}) {
    if (busy) return;
    setBusy(true);
    try {
      const data = await api.post<{ job: { id: string } }>(path, body);
      toast.show(`任务已创建：${data.job.id}`, "info");
      await load();
    } catch (reason) { setError((reason as Error).message); }
    finally { setBusy(false); }
  }

  async function rebalance(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    if (!sourceBucketID || !targetBucketID || sourceBucketID === targetBucketID) return;
    await schedule("/api/v1/r2/rebalance", {
      source_bucket_id: sourceBucketID, target_bucket_id: targetBucketID, prefix: form.get("prefix"),
    });
  }

  return (
    <>
      <PageHeader
        title="R2 统一存储"
        actions={<RefreshButton onRefresh={() => Promise.all([
          load(),
          tab === "overview" ? loadOverview() : loadRemote(),
          tab === "buckets" ? loadDeletionJobs() : Promise.resolve(),
        ])} />}
        tabs={<Segmented
          label="存储视图"
          value={tab}
          onChange={setTab}
          items={[
            { id: "objects", label: "对象", icon: <File size={15} /> },
            { id: "overview", label: "桶概览", icon: <Gauge size={15} /> },
            { id: "buckets", label: "物理桶", icon: <Boxes size={15} /> },
            { id: "maintenance", label: "维护", icon: <Wrench size={15} /> },
          ]}
        />}
      />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      {tab === "overview" && accountUsage.length > 0 && <section className="panel">
        <div className="panel-heading"><h2>账号容量与操作额度</h2></div>
        <div className="table-wrap"><table>
          <thead><tr><th>账号</th><th>纳管 / 未纳管</th><th>预留</th><th>当前总量</th><th>账号上限</th><th>Class A（月）</th><th>Class B（月）</th></tr></thead>
          <tbody>{accountUsage.map((usage) => <tr key={usage.account_id}>
            <td>{accounts.find((account) => account.id === usage.account_id)?.name ?? usage.account_id}</td>
            <td>{formatBytes(usage.managed_bytes)} / {formatBytes(usage.unmanaged_bytes)}</td>
            <td>{formatBytes(usage.reserved_bytes)}</td>
            <td>{formatBytes(usage.managed_bytes + usage.unmanaged_bytes + usage.reserved_bytes)}</td>
            <td>{formatBytes(usage.account_storage_soft_limit_bytes)}</td>
            <td>{usage.class_a_ops.toLocaleString()} / {usage.class_a_soft_limit.toLocaleString()}</td>
            <td>{usage.class_b_ops.toLocaleString()} / {usage.class_b_soft_limit.toLocaleString()}</td>
          </tr>)}</tbody>
        </table></div>
      </section>}
      <Reveal key={tab}>{tab === "objects" ? <>
        <form className="filter-bar" onSubmit={(event) => { event.preventDefault(); void load(prefix); }}>
          <Search size={16} /><input aria-label="对象前缀" placeholder="按对象前缀筛选" value={prefix} onChange={(event) => setPrefix(event.target.value)} /><button type="submit">筛选</button>
        </form>
        <section className="panel">{loading ? <TableSkeleton columns={5} /> : objects.length === 0 ? <Empty>暂无对象</Empty> : <div className="table-wrap"><table>
          <thead><tr><th>对象键</th><th>大小</th><th>类型</th><th>状态</th><th>更新时间</th></tr></thead>
          <tbody>{objects.map((object) => <tr key={object.key}><td className="mono"><File size={14} />{object.key}</td><td>{formatBytes(object.size)}</td><td>{object.content_type || "-"}</td><td><Status value={object.state} /></td><td>{new Date(object.last_modified).toLocaleString()}</td></tr>)}</tbody>
        </table></div>}</section>
      </> : tab === "overview" ? (
        overviewLoading && overview === null ? <section className="panel"><TableSkeleton columns={4} /></section>
        : !loading && accounts.length === 0 ? <NoAccountHint />
        : (overview ?? []).map((entry) => {
          const entryBuckets = entry.buckets ?? [];
          const usage = entry.usage;
          return <section className="panel" key={entry.account_id}>
            <div className="panel-heading">
              <h2>{entry.account_name}</h2>
              {usage && usage.total_bytes != null && <div className="overview-meta">
                <span>已用 {formatBytes(usage.total_bytes)} · 免费剩余 {formatBytes(usage.remaining_bytes ?? 0)} / {formatBytes(usage.free_tier_bytes)}</span>
                <span className="usage-meter"><span style={{
                  width: `${Math.min(100, (usage.total_bytes / usage.free_tier_bytes) * 100)}%`,
                  background: usage.total_bytes / usage.free_tier_bytes > 0.85 ? "var(--warning)" : undefined,
                }} /></span>
              </div>}
            </div>
            {entry.error ? <div className="notice">无法读取该账号的桶列表：{entry.error}</div>
              : usage?.usage_error ? <div className="notice">用量数据暂不可用：{usage.usage_error}</div> : null}
            {!entry.error && (entryBuckets.length === 0 ? <Empty>该账号下暂无存储桶</Empty> : <div className="table-wrap"><table>
              <thead><tr><th>桶名称</th><th>用量</th><th>对象数</th><th>阵列状态</th></tr></thead>
              <tbody>{[...entryBuckets].sort((a, b) => Number(b.managed) - Number(a.managed) || a.name.localeCompare(b.name)).map((view) => <tr key={`${view.jurisdiction || "default"}:${view.name}`} className={view.managed ? "row-managed" : ""}>
                <td><strong>{view.name}</strong>{view.creation_date && <small>创建于 {new Date(view.creation_date).toLocaleDateString()} · {view.jurisdiction || "default"}</small>}</td>
                <td>{view.payload_bytes != null ? formatBytes(view.payload_bytes) : "—"}</td>
                <td>{view.object_count != null ? view.object_count.toLocaleString() : "—"}</td>
                <td>{overviewBucketStatus(view)}</td>
              </tr>)}</tbody>
            </table></div>)}
          </section>;
        })
      ) : tab === "buckets" ? !loading && accounts.length === 0 ? <NoAccountHint /> : <>
        <div className="context-bar"><SelectField label="账号" value={bucketAccountID} onChange={setBucketAccountID} options={accounts.map((account) => ({ value: account.id, label: account.name }))} placeholder="选择账号" disabled={busy || remoteLoading} /></div>
        {remoteError ? <>
          <div className="inline-notice">无法从 Cloudflare 拉取桶列表（{remoteError}），以下仅显示已登记的阵列桶，可手动登记。</div>
          <form className="form-band inline-form" onSubmit={addBucket}>
            <label>物理桶名称<input name="name" required /></label>
            <label className="checkbox-label"><input type="checkbox" name="adopted" />接管已有对象</label>
            <button className="primary" type="submit" disabled={!bucketAccountID || busy}><Plus size={16} />登记物理桶</button>
          </form>
          <section className="panel">{loading ? <TableSkeleton columns={5} /> : buckets.length === 0 ? <Empty>暂无物理桶</Empty> : <div className="table-wrap"><table>
            <thead><tr><th>桶名称</th><th>账号</th><th>实际 / 预留</th><th>状态</th><th /></tr></thead>
            <tbody>{buckets.map((bucket) => {
              const active = !bucket.lifecycle_state || bucket.lifecycle_state === "active";
              return <tr key={bucket.id}><td><strong>{bucket.name}</strong></td><td>{accounts.find((item) => item.id === bucket.account_id)?.name ?? bucket.account_id}</td><td>{formatBytes(bucket.storage_bytes)} / {formatBytes(bucket.reserved_storage_bytes)}</td><td><Status value={bucket.lifecycle_state === "deleting" ? "running" : bucket.lifecycle_state === "delete_failed" ? "error" : bucket.health_status} label={bucket.lifecycle_state === "deleting" ? "正在删除" : bucket.lifecycle_state === "delete_failed" ? "删除失败" : undefined} /></td><td className="row-actions"><button className="icon-button" title="接管扫描" disabled={busy || !active} onClick={() => void schedule(`/api/v1/r2/buckets/${bucket.id}/adopt`)}><FolderInput size={15} /></button><button className="icon-button" title="孤立对象扫描" disabled={busy || !active} onClick={() => void schedule(`/api/v1/r2/buckets/${bucket.id}/orphans/scan`)}><ScanSearch size={15} /></button><button className="icon-button" title="仅移出阵列，不删除 Cloudflare 中的桶" disabled={!active} onClick={() => setUnlinkTarget(bucket)}><Unlink size={15} /></button></td></tr>;
            })}</tbody>
          </table></div>}</section>
        </> : <>
          {usageSummary && <Reveal><section className="stat-band" aria-label="存储用量">
            <div className="stat">
              <span>已用空间</span>
              <strong>{usageSummary.total_bytes != null ? formatBytes(usageSummary.total_bytes) : "—"}</strong>
              {usageSummary.total_bytes != null && <span className="usage-meter"><span style={{
                width: `${Math.min(100, (usageSummary.total_bytes / usageSummary.free_tier_bytes) * 100)}%`,
                background: usageSummary.total_bytes / usageSummary.free_tier_bytes > 0.85 ? "var(--warning)" : undefined,
              }} /></span>}
            </div>
            <div className="stat"><span>免费额度剩余（共 {formatBytes(usageSummary.free_tier_bytes)}）</span><strong>{usageSummary.remaining_bytes != null ? formatBytes(usageSummary.remaining_bytes) : "—"}</strong></div>
            <div className="stat"><span>存储桶</span><strong>{remoteList.length}</strong></div>
            <div className="stat"><span>阵列内</span><strong>{remoteList.filter((item) => item.managed).length}</strong></div>
          </section></Reveal>}
          <form className="form-band inline-form" onSubmit={createRemoteBucket}>
            <label>新建存储桶
              <input value={newBucketName} onChange={(event) => setNewBucketName(event.target.value)} placeholder="my-bucket" pattern="[a-z0-9][a-z0-9-]{1,61}[a-z0-9]" title="3–63 位小写字母、数字或连字符" required />
              <small className="field-hint">直接在 Cloudflare 创建存储桶；Token 需具备 Workers R2 Storage Edit 权限。创建后可选择是否纳入阵列。</small>
            </label>
            <button className="primary" type="submit" disabled={!bucketAccountID || busy || !newBucketName.trim()}><Plus size={16} />创建存储桶</button>
          </form>
          <section className="panel">
            <div className="panel-heading"><h2>账号内全部存储桶</h2></div>
            {usageSummary?.usage_error && <div className="notice">用量数据暂不可用：{usageSummary.usage_error}</div>}
            {remoteLoading ? <TableSkeleton columns={6} /> : remoteList.length === 0 ? <Empty>该账号下暂无存储桶，先在上方创建一个</Empty> : <div className="table-wrap"><table>
              <thead><tr><th>桶名称</th><th>管辖区</th><th>用量</th><th>对象数</th><th>阵列 / 删除状态</th><th /></tr></thead>
              <tbody>{[...remoteList].sort((a, b) => Number(b.managed) - Number(a.managed) || a.name.localeCompare(b.name)).map((view) => {
                const local = view.bucket_id ? buckets.find((bucket) => bucket.id === view.bucket_id) : undefined;
                const jurisdiction = view.jurisdiction || "default";
                const supported = jurisdiction === "default";
                const job = findDeletionJob(view);
                const identityIssue = deletionIdentityIssue(view);
                const failed = !identityIssue && (job?.status === "failed" || view.deletion_status === "failed" || view.lifecycle_state === "delete_failed");
                const deleting = job?.status === "pending" || job?.status === "running" || (view.lifecycle_state === "deleting" && !failed);
                const lastMode = deletionPayload(job).mode;
                const retryMode = (job?.error_code || view.deletion_error_code) === "bucket_not_empty" ? "empty_and_delete" : (lastMode ?? "empty_only");
                const active = !view.lifecycle_state || view.lifecycle_state === "active";
                const deleteActionLabel = view.remote_missing ? "清理远端已不存在的本地登记" : failed ? "重试删除存储桶" : "删除 Cloudflare 存储桶";
                const deleteDisabledReason = !supported
                  ? "当前版本仅支持删除 default 管辖区的存储桶。"
                  : identityIssue || (deleting ? "删除任务正在执行，请等待任务完成。" : "");
                const showSeparateDeleteReason = Boolean(deleteDisabledReason) && !identityIssue;
                const deleteReasonID = `bucket-delete-reason-${jurisdiction}-${view.name}`;
                return <tr key={`${jurisdiction}:${view.name}`}>
                  <td><strong>{view.name}</strong>{view.creation_date && <small>创建于 {new Date(view.creation_date).toLocaleDateString()}</small>}</td>
                  <td><span className="mono">{jurisdiction}</span></td>
                  <td>{view.payload_bytes != null ? formatBytes(view.payload_bytes) : "—"}</td>
                  <td>{view.object_count != null ? view.object_count.toLocaleString() : "—"}</td>
                  <td>{renderRemoteBucketStatus(view)}{showSeparateDeleteReason && <small id={deleteReasonID} className="bucket-action-reason">{deleteDisabledReason}</small>}</td>
                  <td className="row-actions">
                    {view.managed ? <>
                      {!view.remote_missing && active && <>
                        <button className="icon-button" title="接管扫描" disabled={busy} onClick={() => void schedule(`/api/v1/r2/buckets/${view.bucket_id}/adopt`)}><FolderInput size={15} /></button>
                        <button className="icon-button" title="孤立对象扫描" disabled={busy} onClick={() => void schedule(`/api/v1/r2/buckets/${view.bucket_id}/orphans/scan`)}><ScanSearch size={15} /></button>
                      </>}
                      <button className="icon-button" title="仅移出阵列，不删除 Cloudflare 中的桶" disabled={!local || !active} onClick={() => { if (local) setUnlinkTarget(local); }}><Unlink size={15} /></button>
                    </> : <button className="primary secondary-command" title={supported ? "纳入阵列" : "当前版本暂不支持纳管非默认管辖区的桶"} disabled={busy || !supported || deleting} onClick={() => void enroll(view)}><FolderInput size={15} />纳入阵列</button>}
                    <button
                      className="icon-button danger"
                      title={deleteDisabledReason || deleteActionLabel}
                      aria-label={deleteDisabledReason ? `${deleteActionLabel}：${deleteDisabledReason}` : deleteActionLabel}
                      aria-describedby={showSeparateDeleteReason ? deleteReasonID : undefined}
                      disabled={Boolean(deleteDisabledReason)}
                      onClick={() => openRemoteDeletion(view, failed ? retryMode : "empty_only")}
                    >{failed ? <RotateCcw size={15} /> : <Trash2 size={15} />}</button>
                  </td>
                </tr>;
              })}</tbody>
            </table></div>}
          </section>
        </>}
      </> : <>
        <section className="panel maintenance-panel">
          <div className="panel-heading"><h2>维护操作</h2></div>
          <div className="maintenance-item">
            <div><strong>恢复状态</strong><p>校对本地索引与 R2 的实际状态，续跑或清理中断的上传与删除。</p></div>
            <button className="primary secondary-command" disabled={busy} onClick={() => void schedule("/api/v1/r2/recovery")}><RotateCcw size={15} />执行</button>
          </div>
          <div className="maintenance-item">
            <div><strong>重建索引</strong><p>重新扫描已登记的物理桶，从 R2 重建本地对象索引。</p></div>
            <button className="primary secondary-command" disabled={busy} onClick={() => void schedule("/api/v1/r2/index/rebuild")}><RefreshCw size={15} />执行</button>
          </div>
        </section>
        <form className="form-band rebalance-form" onSubmit={rebalance}>
          <SelectField label="源物理桶" value={sourceBucketID} onChange={setSourceBucketID} options={buckets.map((bucket) => ({ value: bucket.id, label: bucket.name }))} required />
          <ArrowRightLeft size={18} />
          <SelectField label="目标物理桶" value={targetBucketID} onChange={setTargetBucketID} options={buckets.filter((bucket) => bucket.id !== sourceBucketID).map((bucket) => ({ value: bucket.id, label: bucket.name }))} required />
          <label>对象前缀<input name="prefix" /></label>
          <button className="primary" type="submit" disabled={!sourceBucketID || !targetBucketID || sourceBucketID === targetBucketID || busy}><ArrowRightLeft size={15} />再均衡</button>
        </form>
        <section className="panel"><div className="panel-heading"><h2>扫描发现</h2></div>{findings.length === 0 ? <Empty>暂无扫描发现</Empty> : <div className="table-wrap"><table><thead><tr><th>类型</th><th>物理桶</th><th>对象键</th><th>详情</th><th>发现时间</th></tr></thead><tbody>{findings.map((finding) => <tr key={finding.id}><td><Status value={finding.kind} /></td><td>{buckets.find((bucket) => bucket.id === finding.physical_bucket_id)?.name ?? finding.physical_bucket_id}</td><td className="mono">{finding.physical_key}</td><td>{finding.detail ?? "-"}</td><td>{new Date(finding.found_at).toLocaleString()}</td></tr>)}</tbody></table></div>}</section>
      </>}</Reveal>
      <ConfirmDialog
        open={Boolean(unlinkTarget)}
        title="移出阵列"
        description={`仅移除“${unlinkTarget?.name ?? ""}”的本地登记？Cloudflare 中的存储桶和所有文件都会保留。`}
        confirmLabel="移除登记"
        onOpenChange={(open) => { if (!open) setUnlinkTarget(null); }}
        onConfirm={() => unlinkTarget ? unlinkBucket(unlinkTarget) : Promise.resolve()}
      />
      <BucketDeleteDialog
        open={Boolean(remoteDeleteTarget)}
        bucketName={remoteDeleteTarget?.view.name ?? ""}
        accountName={remoteDeleteTarget?.accountName}
        objectCount={remoteDeleteTarget?.view.object_count}
        initialMode={remoteDeleteInitialMode}
        remoteMissing={remoteDeleteTarget?.view.remote_missing}
        onOpenChange={(open) => { if (!open) setRemoteDeleteTarget(null); }}
        onConfirm={deleteRemoteBucket}
      />
    </>
  );
}
