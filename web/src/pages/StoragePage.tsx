import { FormEvent, useEffect, useRef, useState } from "react";
import { ArrowRightLeft, Boxes, File, FolderInput, Gauge, Plus, RefreshCw, RotateCcw, ScanSearch, Search, Trash2, Wrench } from "lucide-react";
import { api } from "../api";
import type { Account, Bucket, R2AccountUsage, R2Object } from "../types";
import { Empty, ErrorBanner, NoAccountHint, PageHeader, RefreshButton, Segmented, Status, formatBytes } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { SelectField } from "../components/SelectField";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

interface RemoteBucketView {
  name: string;
  creation_date?: string;
  payload_bytes?: number;
  metadata_bytes?: number;
  object_count?: number;
  managed: boolean;
  bucket_id?: string;
  health_status?: string;
  remote_missing?: boolean;
}

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
  const [deleteTarget, setDeleteTarget] = useState<Bucket | null>(null);
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
      setRemoteList(data.buckets ?? []);
      setUsageSummary(data.usage ?? null);
    } catch (reason) {
      if (epoch === remoteEpoch.current) { setRemoteList([]); setUsageSummary(null); setRemoteError((reason as Error).message); }
    } finally {
      if (epoch === remoteEpoch.current) setRemoteLoading(false);
    }
  }
  useEffect(() => { setNewBucketName(""); void loadRemote(bucketAccountID); }, [bucketAccountID]);

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

  async function deleteBucket(bucket: Bucket) {
    try { await api.delete(`/api/v1/r2/buckets/${bucket.id}`); setOverview(null); toast.show("已移出阵列（Cloudflare 中的桶与对象不受影响）"); await Promise.all([load(), loadRemote()]); }
    catch (reason) { setError((reason as Error).message); throw reason; }
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
        actions={<RefreshButton onRefresh={() => Promise.all([load(), tab === "overview" ? loadOverview() : loadRemote()])} />}
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
              <tbody>{[...entryBuckets].sort((a, b) => Number(b.managed) - Number(a.managed) || a.name.localeCompare(b.name)).map((view) => <tr key={view.name} className={view.managed ? "row-managed" : ""}>
                <td><strong>{view.name}</strong>{view.creation_date && <small>创建于 {new Date(view.creation_date).toLocaleDateString()}</small>}</td>
                <td>{view.payload_bytes != null ? formatBytes(view.payload_bytes) : "—"}</td>
                <td>{view.object_count != null ? view.object_count.toLocaleString() : "—"}</td>
                <td>{view.remote_missing ? <Status value="error" label="远端已不存在" /> : view.managed ? <Status value={view.health_status || "healthy"} label="阵列中" /> : <Status value="unmanaged" label="未纳入" />}</td>
              </tr>)}</tbody>
            </table></div>)}
          </section>;
        })
      ) : tab === "buckets" ? !loading && accounts.length === 0 ? <NoAccountHint /> : <>
        <div className="context-bar"><SelectField label="账号" value={bucketAccountID} onChange={setBucketAccountID} options={accounts.map((account) => ({ value: account.id, label: account.name }))} placeholder="选择账号" /></div>
        {remoteError ? <>
          <div className="inline-notice">无法从 Cloudflare 拉取桶列表（{remoteError}），以下仅显示已登记的阵列桶，可手动登记。</div>
          <form className="form-band inline-form" onSubmit={addBucket}>
            <label>物理桶名称<input name="name" required /></label>
            <label className="checkbox-label"><input type="checkbox" name="adopted" />接管已有对象</label>
            <button className="primary" type="submit" disabled={!bucketAccountID || busy}><Plus size={16} />登记物理桶</button>
          </form>
          <section className="panel">{loading ? <TableSkeleton columns={5} /> : buckets.length === 0 ? <Empty>暂无物理桶</Empty> : <div className="table-wrap"><table>
            <thead><tr><th>桶名称</th><th>账号</th><th>实际 / 预留</th><th>状态</th><th /></tr></thead>
            <tbody>{buckets.map((bucket) => <tr key={bucket.id}><td><strong>{bucket.name}</strong></td><td>{accounts.find((item) => item.id === bucket.account_id)?.name ?? bucket.account_id}</td><td>{formatBytes(bucket.storage_bytes)} / {formatBytes(bucket.reserved_storage_bytes)}</td><td><Status value={bucket.health_status} /></td><td className="row-actions"><button className="icon-button" title="接管扫描" disabled={busy} onClick={() => void schedule(`/api/v1/r2/buckets/${bucket.id}/adopt`)}><FolderInput size={15} /></button><button className="icon-button" title="孤立对象扫描" disabled={busy} onClick={() => void schedule(`/api/v1/r2/buckets/${bucket.id}/orphans/scan`)}><ScanSearch size={15} /></button><button className="icon-button danger" title="移出阵列" onClick={() => setDeleteTarget(bucket)}><Trash2 size={15} /></button></td></tr>)}</tbody>
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
            {remoteLoading ? <TableSkeleton columns={5} /> : remoteList.length === 0 ? <Empty>该账号下暂无存储桶，先在上方创建一个</Empty> : <div className="table-wrap"><table>
              <thead><tr><th>桶名称</th><th>用量</th><th>对象数</th><th>阵列状态</th><th /></tr></thead>
              <tbody>{[...remoteList].sort((a, b) => Number(b.managed) - Number(a.managed) || a.name.localeCompare(b.name)).map((view) => {
                const local = view.bucket_id ? buckets.find((bucket) => bucket.id === view.bucket_id) : undefined;
                return <tr key={view.name}>
                  <td><strong>{view.name}</strong>{view.creation_date && <small>创建于 {new Date(view.creation_date).toLocaleDateString()}</small>}</td>
                  <td>{view.payload_bytes != null ? formatBytes(view.payload_bytes) : "—"}</td>
                  <td>{view.object_count != null ? view.object_count.toLocaleString() : "—"}</td>
                  <td>{view.remote_missing ? <Status value="error" label="远端已不存在" /> : view.managed ? <Status value={view.health_status || "healthy"} label="阵列中" /> : <Status value="unmanaged" label="未纳入" />}</td>
                  <td className="row-actions">
                    {view.managed ? <>
                      {!view.remote_missing && <>
                        <button className="icon-button" title="接管扫描" disabled={busy} onClick={() => void schedule(`/api/v1/r2/buckets/${view.bucket_id}/adopt`)}><FolderInput size={15} /></button>
                        <button className="icon-button" title="孤立对象扫描" disabled={busy} onClick={() => void schedule(`/api/v1/r2/buckets/${view.bucket_id}/orphans/scan`)}><ScanSearch size={15} /></button>
                      </>}
                      <button className="icon-button danger" title="移出阵列（不影响 Cloudflare 中的桶）" onClick={() => { if (local) setDeleteTarget(local); }}><Trash2 size={15} /></button>
                    </> : <button className="primary secondary-command" disabled={busy} onClick={() => void enroll(view)}><FolderInput size={15} />纳入阵列</button>}
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
        open={Boolean(deleteTarget)}
        title="移除物理桶"
        description={`移除“${deleteTarget?.name ?? ""}”的本地登记？Cloudflare 中的桶和对象不会被删除。`}
        confirmLabel="移除登记"
        onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        onConfirm={() => deleteTarget ? deleteBucket(deleteTarget) : Promise.resolve()}
      />
    </>
  );
}
