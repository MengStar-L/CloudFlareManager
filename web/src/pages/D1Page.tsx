import { FormEvent, useEffect, useRef, useState } from "react";
import Editor, { loader } from "@monaco-editor/react";
import * as monaco from "monaco-editor/esm/vs/editor/editor.api";
import "monaco-editor/esm/vs/basic-languages/sql/sql.contribution.js";
import EditorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import {
  Activity, ChevronLeft, ChevronRight, Database, DatabaseBackup, Play, Plus,
  ShieldAlert, SquareTerminal, Table2, Trash2,
} from "lucide-react";
import { api } from "../api";
import type { Account } from "../types";
import { Empty, ErrorBanner, NoAccountHint, PageHeader, RefreshButton, Segmented, Status } from "../components/UI";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Reveal } from "../components/Motion";
import { SelectField } from "../components/SelectField";
import { TableSkeleton } from "../components/Skeleton";
import { useToast } from "../components/Toast";

interface D1Database { uuid: string; name: string; num_tables?: number; file_size?: number }
interface SchemaObject { name: string; type: string; table_name: string; sql?: string }
interface TablePage { table: string; columns: string[]; rows: Array<Record<string, unknown>>; limit: number; offset: number; has_more: boolean }
interface QueryInsight { history_id: string; severity: string; category: string; message: string; sql: string; rows_read: number; duration_ms: number; created_at: string }
interface D1Backup { id: string; r2_object_key: string; status: string; created_at: string }
type WorkspaceTab = "console" | "data" | "insights" | "backups";

self.MonacoEnvironment = { getWorker: () => new EditorWorker() };
loader.config({ monaco });

export function D1Page() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [accountID, setAccountID] = useState("");
  const [databases, setDatabases] = useState<D1Database[]>([]);
  const [databaseID, setDatabaseID] = useState("");
  const [tab, setTab] = useState<WorkspaceTab>("console");
  const [sql, setSQL] = useState("SELECT name, type\nFROM sqlite_schema\nORDER BY name;");
  const [password, setPassword] = useState("");
  const [result, setResult] = useState<unknown>(null);
  const [schema, setSchema] = useState<SchemaObject[]>([]);
  const [table, setTable] = useState("");
  const [tablePage, setTablePage] = useState<TablePage | null>(null);
  const [insights, setInsights] = useState<QueryInsight[]>([]);
  const [backups, setBackups] = useState<D1Backup[]>([]);
  const [error, setError] = useState("");
  const [running, setRunning] = useState(false);
  const [creating, setCreating] = useState(false);
  const [loading, setLoading] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<D1Database | null>(null);
  const toast = useToast();
  // 请求纪元：快速切换账号/数据库/表时，只有最后一次发起的响应才允许落地。
  const databasesEpoch = useRef(0);
  const detailsEpoch = useRef(0);
  const rowsEpoch = useRef(0);

  useEffect(() => {
    api.get<{ accounts: Account[] }>("/api/v1/accounts").then((data) => {
      const available = (data.accounts ?? []).filter((account) => account.capabilities?.some((item) => item.name === "d1" && item.available));
      setAccounts(available);
      if (available[0]) setAccountID(available[0].id);
      else setLoading(false);
    }).catch((reason) => { setError(reason.message); setLoading(false); });
  }, []);

  async function loadDatabases(id = accountID) {
    if (!id) return;
    const epoch = ++databasesEpoch.current;
    try {
      const data = await api.get<{ databases: D1Database[] }>(`/api/v1/d1/databases?account_id=${encodeURIComponent(id)}`);
      if (epoch !== databasesEpoch.current) return;
      const items = data.databases ?? [];
      setDatabases(items);
      setDatabaseID((current) => items.some((item) => item.uuid === current) ? current : (items[0]?.uuid ?? ""));
    } catch (reason) { if (epoch === databasesEpoch.current) setError((reason as Error).message); }
    finally { setLoading(false); }
  }

  async function loadDatabaseDetails(id = databaseID) {
    if (!id || !accountID) return;
    const epoch = ++detailsEpoch.current;
    try {
      const base = `/api/v1/d1/databases/${encodeURIComponent(id)}`;
      const [schemaData, insightData, backupData] = await Promise.all([
        api.get<{ schema: SchemaObject[] }>(`${base}/schema?account_id=${encodeURIComponent(accountID)}`),
        api.get<{ insights: QueryInsight[] }>(`${base}/insights?account_id=${encodeURIComponent(accountID)}`),
        api.get<{ backups: D1Backup[] }>(`${base}/backups?account_id=${encodeURIComponent(accountID)}`),
      ]);
      if (epoch !== detailsEpoch.current) return;
      const objects = (schemaData.schema ?? []).filter((item) => item.type === "table" || item.type === "view");
      setSchema(objects);
      setInsights(insightData.insights ?? []);
      setBackups(backupData.backups ?? []);
      const nextTable = objects.some((item) => item.name === table) ? table : (objects[0]?.name ?? "");
      setTable(nextTable);
      // 切换数据库后表名可能不变（两个库有同名表），此时 [tab, table] effect 不会重跑，需要主动刷新行数据。
      if (tab === "data" && nextTable && nextTable === table) void loadTableRows(nextTable, 0);
    } catch (reason) { if (epoch === detailsEpoch.current) setError((reason as Error).message); }
  }

  async function loadTableRows(name = table, offset = 0) {
    if (!databaseID || !name) return;
    const epoch = ++rowsEpoch.current;
    try {
      const path = `/api/v1/d1/databases/${encodeURIComponent(databaseID)}/tables/${encodeURIComponent(name)}/rows?account_id=${encodeURIComponent(accountID)}&limit=50&offset=${offset}`;
      const page = await api.get<TablePage>(path);
      if (epoch === rowsEpoch.current) setTablePage(page);
    } catch (reason) { if (epoch === rowsEpoch.current) setError((reason as Error).message); }
  }

  useEffect(() => { if (accountID) void loadDatabases(accountID); }, [accountID]);
  useEffect(() => {
    setTablePage(null);
    if (databaseID) {
      void loadDatabaseDetails(databaseID);
    } else {
      // 没有选中的库（空账号或删光了）时清空工作台，避免残留上一个库的数据。
      setSchema([]); setInsights([]); setBackups([]); setTable(""); setResult(null);
    }
  }, [databaseID]);
  useEffect(() => { if (tab === "data" && table) void loadTableRows(table, 0); }, [tab, table]);

  async function createDatabase(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    if (creating) return;
    setCreating(true);
    try {
      await api.post("/api/v1/d1/databases", { account_id: accountID, name: form.get("name") });
      formElement.reset();
      toast.show("D1 数据库已创建");
      await loadDatabases();
    } catch (reason) { setError((reason as Error).message); }
    finally { setCreating(false); }
  }

  async function runQuery() {
    if (!databaseID) return;
    setRunning(true); setError("");
    try {
      const data = await api.post(`/api/v1/d1/databases/${databaseID}/query`, { account_id: accountID, sql, params: [], admin_password: password });
      setResult(data); setPassword("");
      await loadDatabaseDetails();
    } catch (reason) { setError((reason as Error).message); } finally { setRunning(false); }
  }

  async function createBackup() {
    if (!databaseID) return;
    setRunning(true); setError("");
    try {
      await api.post(`/api/v1/d1/databases/${databaseID}/backup`, { account_id: accountID });
      toast.show("D1 备份任务已创建");
      await loadDatabaseDetails();
    } catch (reason) { setError((reason as Error).message); } finally { setRunning(false); }
  }

  async function deleteDatabase(database: D1Database, password: string) {
    try {
      await api.delete(`/api/v1/d1/databases/${database.uuid}`, { account_id: accountID, admin_password: password });
      toast.show("D1 数据库已删除");
      await loadDatabases();
    } catch (reason) { setError((reason as Error).message); throw reason; }
  }

  return (
    <>
      <PageHeader title="D1 数据库" actions={<RefreshButton onRefresh={() => Promise.all([loadDatabases(), loadDatabaseDetails()])} />} />
      {error && <ErrorBanner message={error} onClose={() => setError("")} />}
      <div className="context-bar"><SelectField label="账号" value={accountID} onChange={(value) => { setAccountID(value); setDatabaseID(""); }} options={accounts.map((account) => ({ value: account.id, label: account.name }))} placeholder="选择账号" /></div>
      <div className="d1-layout">
        <aside className="database-list panel">
          <form onSubmit={createDatabase} className="create-row"><input name="name" aria-label="新数据库名称" placeholder="数据库名称" required disabled={!accountID} /><button className="icon-button" title="创建数据库" disabled={!accountID || creating}><Plus size={16} /></button></form>
          {loading ? <TableSkeleton columns={1} rows={5} /> : accounts.length === 0 ? <NoAccountHint /> : databases.length === 0 ? <Empty>暂无数据库</Empty> : databases.map((database) => <div key={database.uuid} className={databaseID === database.uuid ? "database-item active" : "database-item"}>
            <button type="button" className="database-select" onClick={() => setDatabaseID(database.uuid)}>
              <Database size={15} /><span><strong>{database.name}</strong><small>{database.uuid}</small></span>
            </button>
            <button type="button" className="icon-button danger" title="删除数据库" aria-label={`删除数据库 ${database.name}`} onClick={() => setDeleteTarget(database)}><Trash2 size={14} /></button>
          </div>)}
        </aside>
        <section className="sql-workspace">
          <Segmented
            className="d1-tabs"
            label="D1 工作台视图"
            value={tab}
            onChange={setTab}
            items={[
              { id: "console", label: "SQL", icon: <SquareTerminal size={14} /> },
              { id: "data", label: "数据", icon: <Table2 size={14} /> },
              { id: "insights", label: "分析", icon: <Activity size={14} /> },
              { id: "backups", label: "备份", icon: <DatabaseBackup size={14} /> },
            ]}
          />

          <Reveal key={tab}>{tab === "console" && <>
            <div className="editor-toolbar"><strong>SQL 控制台</strong><button className="primary" onClick={() => void runQuery()} disabled={!databaseID || running}><Play size={15} />执行</button></div>
            <div className="editor-frame"><Editor height="310px" defaultLanguage="sql" value={sql} onChange={(value) => setSQL(value ?? "")} options={{ minimap: { enabled: false }, fontSize: 13, lineNumbersMinChars: 3, scrollBeyondLastLine: false, automaticLayout: true }} /></div>
            <div className="write-confirm"><ShieldAlert size={15} /><label>写操作确认<input type="password" placeholder="管理员密码" value={password} onChange={(event) => setPassword(event.target.value)} /></label></div>
            <pre className="result-view">{result === null ? "" : JSON.stringify(result, null, 2)}</pre>
          </>}

          {tab === "data" && <section className="panel d1-tool-panel">
            <div className="data-toolbar"><SelectField label="表或视图" value={table} onChange={setTable} options={schema.map((item) => ({ value: item.name, label: item.name, description: item.type }))} placeholder="选择表或视图" /><div className="pagination"><button className="icon-button" title="上一页" disabled={!tablePage || tablePage.offset === 0} onClick={() => void loadTableRows(table, Math.max(0, (tablePage?.offset ?? 0) - 50))}><ChevronLeft size={16} /></button><span>{tablePage ? `${tablePage.offset + 1}-${tablePage.offset + tablePage.rows.length}` : "0"}</span><button className="icon-button" title="下一页" disabled={!tablePage?.has_more} onClick={() => void loadTableRows(table, (tablePage?.offset ?? 0) + 50)}><ChevronRight size={16} /></button></div></div>
            {!table ? <Empty>暂无表或视图</Empty> : !tablePage || tablePage.rows.length === 0 ? <Empty>暂无数据</Empty> : <div className="table-wrap"><table><thead><tr>{tablePage.columns.map((column) => <th key={column}>{column}</th>)}</tr></thead><tbody>{tablePage.rows.map((row, index) => <tr key={`${tablePage.offset}-${index}`}>{tablePage.columns.map((column) => <td key={column} className="mono">{formatCell(row[column])}</td>)}</tr>)}</tbody></table></div>}
          </section>}

          {tab === "insights" && <section className="panel d1-tool-panel">{insights.length === 0 ? <Empty>暂无低效查询</Empty> : <div className="table-wrap"><table><thead><tr><th>级别</th><th>类型</th><th>建议</th><th>读取行数</th><th>耗时</th><th>SQL</th></tr></thead><tbody>{insights.map((item) => <tr key={item.history_id}><td><Status value={item.severity === "high" ? "error" : "warning"} /></td><td>{item.category}</td><td>{item.message}</td><td>{item.rows_read.toLocaleString()}</td><td>{item.duration_ms.toFixed(1)} ms</td><td className="mono">{item.sql}</td></tr>)}</tbody></table></div>}</section>}

          {tab === "backups" && <section className="panel d1-tool-panel"><div className="panel-heading"><h2>R2 备份</h2><button className="primary" onClick={() => void createBackup()} disabled={!databaseID || running}><DatabaseBackup size={15} />创建备份</button></div>{backups.length === 0 ? <Empty>暂无备份</Empty> : <div className="table-wrap"><table><thead><tr><th>时间</th><th>状态</th><th>R2 对象</th></tr></thead><tbody>{backups.map((backup) => <tr key={backup.id}><td>{new Date(backup.created_at).toLocaleString()}</td><td><Status value={backup.status} /></td><td className="mono">{backup.r2_object_key}</td></tr>)}</tbody></table></div>}</section>}</Reveal>
        </section>
      </div>
      <ConfirmDialog
        open={Boolean(deleteTarget)}
        title="删除 D1 数据库"
        description={`删除“${deleteTarget?.name ?? ""}”前请输入管理员密码。该操作会调用 Cloudflare 删除接口。`}
        confirmLabel="删除数据库"
        passwordLabel="管理员密码"
        onOpenChange={(open) => { if (!open) setDeleteTarget(null); }}
        onConfirm={(confirmation) => deleteTarget ? deleteDatabase(deleteTarget, confirmation) : Promise.resolve()}
      />
    </>
  );
}

function formatCell(value: unknown) {
  if (value === null || value === undefined) return "NULL";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}
