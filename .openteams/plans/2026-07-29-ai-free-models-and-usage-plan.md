# 免费 AI 模型与每日用量 Implementation Plan

**Status:** 已完成实现与桌面/移动浏览器验证。全量测试、静态检查及生产构建通过；当前 Windows Go 环境为 `CGO_ENABLED=0`，因此 `go test -race` 无法执行。

**Goal:** 通过付费模型黑名单和 Paid-plan 403 自动学习统一过滤模型目录与调用入口，并提供按 Cloudflare 账号和 AI 访问密钥展开的每日估算 Neurons 额度页面。

**Architecture:** 在 `internal/modules/ai` 中增加互相独立的 `ModelPolicy`、`NeuronEstimator` 和 `UsageService`。`Management` 与 `Gateway` 共享同一个模型策略和估算器；模型目录过滤、网关前置拦截、上游错误学习和用量快照都在服务端完成，React 页面只渲染稳定的两层 API 响应。

**Tech Stack:** Go 1.24、`net/http`、`database/sql`、SQLite migrations、React 19、TypeScript、Lucide、现有 CSS design tokens、Go `httptest`。

---

## File Map

| File | Responsibility |
| --- | --- |
| `internal/platform/database/migrations/005_ai_model_policy_usage.sql` | 保存自动学习的付费模型、记录 Neuron 估算来源并建立按日明细索引。 |
| `internal/platform/database/database_test.go` | 验证新迁移在全新数据库中完整生效。 |
| `internal/modules/ai/model_policy.go` | 内置黑名单、持久黑名单、目录过滤、Paid-plan 错误识别和类型化拦截错误。 |
| `internal/modules/ai/model_policy_test.go` | 覆盖策略合并、过滤、持久学习、并发幂等及误判保护。 |
| `internal/modules/ai/neuron_estimator.go` | 提取真实 usage、解析目录费率、使用版本化官方费率和保守回退计算 Neurons。 |
| `internal/modules/ai/neuron_estimator_test.go` | 覆盖真实 usage、文本费率、缺失费率、流式 usage 和失败请求。 |
| `internal/modules/ai/usage.go` | 生成 UTC 日的账号总额度与访问密钥明细报告。 |
| `internal/modules/ai/usage_test.go` | 覆盖日期、零用量账号、密钥状态、未归属调用和聚合一致性。 |
| `internal/modules/ai/management.go` | 将 Cloudflare 分页目录交给共享策略过滤，并把目录费率交给估算器。 |
| `internal/modules/ai/management_test.go` | 验证目录过滤、去重、排序和费率更新。 |
| `internal/modules/ai/gateway.go` | 在账户选择前拦截模型、识别上游 Paid-plan 403、用结构化计量结果写日志。 |
| `internal/modules/ai/gateway_test.go` | 验证本地拦截、首次学习、错误透传、实际 usage 和零计费失败。 |
| `internal/protocol/ai/handler.go` | 将类型化付费模型错误映射为标准 OpenAI 403。 |
| `internal/protocol/ai/handler_test.go` | 验证 `/v1/models` 和推理入口的 OpenAI 错误契约。 |
| `internal/platform/httpapi/api.go` | 返回两层用量报告，校验日期，并统一 Playground 的付费模型错误。 |
| `internal/platform/httpapi/api_test.go` | 验证认证后的用量 API、非法日期和 Playground 403。 |
| `internal/app/server.go` | 构造并共享策略、估算器与用量服务。 |
| `web/src/pages/AIPage.tsx` | 渲染“用量额度”、账号展开与过滤后的模型/Playground。 |
| `web/src/pages/OverviewPage.tsx` | 从新用量响应汇总“今日估算 Neurons”。 |
| `web/src/styles/pages.css` | 账号额度列表、进度条和密钥明细样式。 |
| `web/src/styles/responsive.css` | 用量额度页面的移动布局。 |

## Task 1: Add the Persistence Contract

**Files:**
- Create: `internal/platform/database/migrations/005_ai_model_policy_usage.sql`
- Modify: `internal/platform/database/database_test.go`

- **Step 1: Write a failing migration assertion**

  Extend `TestOpenAppliesMigrationsAndPragmas` to assert the table, column and index:

  ```go
  var paidModelsTable int
  if err := db.QueryRowContext(context.Background(), `
      SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'ai_paid_models'`).Scan(&paidModelsTable); err != nil {
      t.Fatal(err)
  }
  if paidModelsTable != 1 {
      t.Fatal("ai_paid_models migration was not applied")
  }
  rows, err := db.QueryContext(context.Background(), "PRAGMA table_info(ai_request_logs)")
  if err != nil { t.Fatal(err) }
  defer rows.Close()
  foundSource := false
  for rows.Next() {
      var cid, notNull, pk int
      var name, columnType string
      var defaultValue any
      if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil { t.Fatal(err) }
      foundSource = foundSource || name == "neuron_estimation_source"
  }
  if !foundSource { t.Fatal("neuron_estimation_source column was not applied") }
  ```

- **Step 2: Run the database test and verify the red state**

  Run: `go test ./internal/platform/database -run TestOpenAppliesMigrationsAndPragmas -count=1`

  Expected: FAIL because `ai_paid_models` and `neuron_estimation_source` do not exist.

- **Step 3: Add the migration**

  Create `005_ai_model_policy_usage.sql` with:

  ```sql
  CREATE TABLE ai_paid_models (
      model_id TEXT PRIMARY KEY,
      source TEXT NOT NULL,
      reason TEXT NOT NULL DEFAULT '',
      detected_at INTEGER NOT NULL,
      updated_at INTEGER NOT NULL
  );

  ALTER TABLE ai_request_logs
      ADD COLUMN neuron_estimation_source TEXT NOT NULL DEFAULT 'legacy';

  CREATE INDEX ai_request_logs_usage_detail_idx
      ON ai_request_logs(account_id, created_at, protocol_credential_id);
  ```

- **Step 4: Run the database test and verify the green state**

  Run: `go test ./internal/platform/database -count=1`

  Expected: PASS.

- **Step 5: Commit the migration**

  ```bash
  git add internal/platform/database/migrations/005_ai_model_policy_usage.sql internal/platform/database/database_test.go
  git commit -m "feat: add AI model policy persistence"
  ```

## Task 2: Implement the Shared Model Policy

**Files:**
- Create: `internal/modules/ai/model_policy.go`
- Create: `internal/modules/ai/model_policy_test.go`

- **Step 1: Write policy tests for built-in and learned models**

  Add tests that use `database.Open(t.TempDir())` and assert these exact behaviors:

  ```go
  func newTestModelPolicy(t *testing.T) *ModelPolicy {
      t.Helper()
      db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
      if err != nil { t.Fatal(err) }
      t.Cleanup(func() { _ = db.Close() })
      return NewModelPolicy(db)
  }

  func modelIDs(models []map[string]any) []string {
      ids := make([]string, 0, len(models))
      for _, model := range models {
          id, _ := model["name"].(string)
          if id == "" { id, _ = model["id"].(string) }
          ids = append(ids, id)
      }
      return ids
  }

  func TestModelPolicyFiltersBuiltinAndLearnedPaidModels(t *testing.T) {
      policy := newTestModelPolicy(t)
      if err := policy.LearnPaid(context.Background(), "@cf/vendor/learned-paid", "requires a Workers Paid plan"); err != nil {
          t.Fatal(err)
      }
      models := []map[string]any{
          {"name": "@cf/zai-org/glm-5.2"},
          {"name": "@cf/vendor/free"},
          {"id": "@cf/vendor/learned-paid"},
          {"name": "@cf/vendor/free"},
      }
      filtered, err := policy.Filter(context.Background(), models)
      if err != nil { t.Fatal(err) }
      if got := modelIDs(filtered); !reflect.DeepEqual(got, []string{"@cf/vendor/free"}) {
          t.Fatalf("models = %#v", got)
      }
  }
  ```

  Add a second test that creates a new `ModelPolicy` over the same DB after `LearnPaid` and verifies the model remains blocked. Add a goroutine test with 20 concurrent `LearnPaid` calls and assert one database row.

- **Step 2: Write Paid-plan classification tests**

  Table-test `PaidPlanReason(status, body)` with:

  ```go
  {403, `{"errors":[{"message":"Model x requires a Workers Paid plan"}]}`, "Model x requires a Workers Paid plan", true},
  {403, `{"error":{"message":"Workers Paid plan is required for this model"}}`, "Workers Paid plan is required for this model", true},
  {403, `{"error":{"message":"invalid API token"}}`, "", false},
  {403, `{"error":{"message":"content blocked"}}`, "", false},
  {400, `{"error":{"message":"requires a Workers Paid plan"}}`, "", false},
  ```

- **Step 3: Run the policy tests and verify they fail to compile**

  Run: `go test ./internal/modules/ai -run 'TestModelPolicy|TestIsPaidPlan' -count=1`

  Expected: FAIL because `ModelPolicy`, `LearnPaid`, `Filter` and `PaidPlanReason` do not exist.

- **Step 4: Implement the policy API**

  Use these stable types and methods:

  ```go
  const paidPlanSource = "cloudflare_paid_plan_403"

  type ModelPolicy struct {
      DB *sql.DB
  }

  type ModelBlockedError struct {
      Model string
  }

  func (e *ModelBlockedError) Error() string
  func NewModelPolicy(db *sql.DB) *ModelPolicy
  func (p *ModelPolicy) IsBlocked(ctx context.Context, model string) (bool, error)
  func (p *ModelPolicy) Filter(ctx context.Context, models []map[string]any) ([]map[string]any, error)
  func (p *ModelPolicy) LearnPaid(ctx context.Context, model, reason string) error
  func PaidPlanReason(status int, body []byte) (reason string, matched bool)
  ```

  Normalize model IDs with `strings.TrimSpace` but retain exact case. Seed the built-in map only with models confirmed by observed Cloudflare Free-plan errors, beginning with `@cf/zai-org/glm-5.2`. `Filter` must drop empty IDs, deduplicate by normalized ID and sort by ID while preserving each retained Cloudflare model object.

  `LearnPaid` must use one UPSERT that preserves the first `detected_at` and updates `reason`/`updated_at`:

  ```sql
  INSERT INTO ai_paid_models(model_id, source, reason, detected_at, updated_at)
  VALUES(?, ?, ?, ?, ?)
  ON CONFLICT(model_id) DO UPDATE SET
      source = excluded.source,
      reason = excluded.reason,
      updated_at = excluded.updated_at
  ```

- **Step 5: Run the policy tests**

  Run: `go test ./internal/modules/ai -run 'TestModelPolicy|TestIsPaidPlan' -race -count=1`

  Expected: PASS, including the concurrent UPSERT test.

- **Step 6: Commit the policy component**

  ```bash
  git add internal/modules/ai/model_policy.go internal/modules/ai/model_policy_test.go
  git commit -m "feat: add paid AI model policy"
  ```

## Task 3: Filter Every Model Catalog Through One Policy

**Files:**
- Modify: `internal/modules/ai/management.go`
- Modify: `internal/modules/ai/management_test.go`
- Modify: `internal/app/server.go`
- Modify: `internal/protocol/ai/handler_test.go`

- **Step 1: Add a failing management filtering test**

  Extend `management_test.go` with a Cloudflare catalog containing a free model, `@cf/zai-org/glm-5.2`, a learned paid model and a duplicate. Construct `Management{Accounts: accountStore, Policy: policy, ...}` and expect only the free model once.

  ```go
  if got := modelIDs(models); !reflect.DeepEqual(got, []string{"@cf/meta/llama-3.2-1b-instruct"}) {
      t.Fatalf("filtered models = %#v", got)
  }
  ```

- **Step 2: Run the management test and verify it fails**

  Run: `go test ./internal/modules/ai -run TestManagementFiltersPaidModels -count=1`

  Expected: FAIL because `Management` has no `Policy` and returns the unfiltered catalog.

- **Step 3: Inject the shared policy into management**

  Add `Policy *ModelPolicy` to `Management`. After pagination, call `Policy.Filter`; when `Policy` is nil, retain the current unfiltered behavior so focused tests and library use do not panic.

  In `server.go`, create exactly one policy and share it:

  ```go
  modelPolicy := aimodule.NewModelPolicy(db)
  aiGateway := &aimodule.Gateway{
      Accounts: accountStore, DB: db, Policy: modelPolicy,
      NeuronSoftLimit: s.Config.AI.NeuronSoftLimit,
      MaxRetryAccounts: s.Config.AI.MaxRetryAccounts,
  }
  aiManagement := &aimodule.Management{Accounts: accountStore, Policy: modelPolicy}
  ```

  The existing protocol `Models` callback and `httpapi.aiModels` already call `Management.ListModels`; do not introduce a second filtering path.

- **Step 4: Strengthen the OpenAI catalog regression test**

  Keep `TestModelsReturnsOpenAIModelList` asserting the standard `{object:"list",data:[...]}` envelope and add a case where its callback returns one already-filtered model. This test should prove the protocol adapter only reformats and does not reintroduce models.

- **Step 5: Run catalog tests**

  Run: `go test ./internal/modules/ai ./internal/protocol/ai -run 'Models|Management' -count=1`

  Expected: PASS.

- **Step 6: Commit shared catalog filtering**

  ```bash
  git add internal/modules/ai/management.go internal/modules/ai/management_test.go internal/app/server.go internal/protocol/ai/handler_test.go
  git commit -m "feat: filter paid models from AI catalogs"
  ```

## Task 4: Block Known Models and Learn Upstream Paid-plan Errors

**Files:**
- Modify: `internal/modules/ai/gateway.go`
- Modify: `internal/modules/ai/gateway_test.go`
- Modify: `internal/protocol/ai/handler.go`
- Modify: `internal/protocol/ai/handler_test.go`
- Modify: `internal/platform/httpapi/api.go`
- Modify: `internal/platform/httpapi/api_test.go`

- **Step 1: Write a failing local-block gateway test**

  Build a test gateway with a policy that has learned `@cf/vendor/paid`. Count upstream requests and call `/v1/chat/completions` with that model.

  ```go
  err := gateway.Forward(response, request, "credential")
  var blocked *ModelBlockedError
  if !errors.As(err, &blocked) || blocked.Model != "@cf/vendor/paid" {
      t.Fatalf("error = %v", err)
  }
  if upstreamCalls != 0 { t.Fatalf("upstream calls = %d", upstreamCalls) }
  ```

  Assert `ai_request_logs` contains status 403, `estimated_neurons = 0` and `error_class = 'paid_model_blocked'`.

- **Step 2: Write a failing auto-learning gateway test**

  Make upstream return Cloudflare's observed error shape:

  ```json
  {"error":{"code":"upstream_error","message":"Model @cf/vendor/new-paid is not available on the Workers Free plan: This model requires a Workers Paid plan."}}
  ```

  First call: expect upstream called once and the original normalized 403 response body preserved. Second call: expect a typed local block and no second upstream call. Add a sibling test proving an `invalid API token` 403 is not learned.

- **Step 3: Run focused gateway tests and verify failure**

  Run: `go test ./internal/modules/ai -run 'TestGatewayBlocks|TestGatewayLearns|TestGatewayDoesNotLearn' -count=1`

  Expected: FAIL because the gateway does not consult or update `ModelPolicy`.

- **Step 4: Implement preflight blocking and bounded 403 inspection**

  Add `Policy *ModelPolicy` to `Gateway`. Immediately after `modelFromRequest`, call `IsBlocked`; for a blocked model call a dedicated record helper with status 403 and zero Neurons, then return `&ModelBlockedError{Model: model}` before `selectAccount`.

  For every upstream 403, read at most `2 << 20` bytes before writing headers. If `PaidPlanReason` matches, pass only its parsed message to `LearnPaid`; never store the complete response envelope. Rebuild the response body with `io.NopCloser(bytes.NewReader(body))` so the first caller receives the complete error. Never retry a 403 against another account. Record both local blocks and learned Paid-plan 403 responses by calling the existing `record` method with input/output token counts set to zero, which yields zero Neurons until Task 5 replaces the record signature.

- **Step 5: Map the typed error at both HTTP surfaces**

  In `internal/protocol/ai/handler.go`, add before the generic compatibility branch:

  ```go
  var blocked *aimodule.ModelBlockedError
  if errors.As(err, &blocked) {
      status, code = http.StatusForbidden, "model_not_available"
      err = errors.New(blocked.Error())
  }
  ```

  In `httpapi.aiPlayground`, map the same error to `403` and code `model_not_available`; change the Playground credential ID from the legacy sentinel `"admin"` to `""` so new calls aggregate under “面板及未归属调用”.

- **Step 6: Add HTTP contract assertions**

  In `handler_test.go`, assert direct inference returns:

  ```json
  {"error":{"type":"model_not_available","code":"model_not_available"}}
  ```

  In `api_test.go`, authenticate an admin session, post to `/api/v1/ai/playground`, and assert the same HTTP 403/code pair for a blocked model.

- **Step 7: Run gateway and HTTP tests**

  Run: `go test ./internal/modules/ai ./internal/protocol/ai ./internal/platform/httpapi -run 'Paid|Block|Learn|Playground' -race -count=1`

  Expected: PASS.

- **Step 8: Commit enforcement and learning**

  ```bash
  git add internal/modules/ai/gateway.go internal/modules/ai/gateway_test.go internal/protocol/ai/handler.go internal/protocol/ai/handler_test.go internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go
  git commit -m "feat: learn and block paid Workers AI models"
  ```

## Task 5: Replace Name-based Neuron Guessing with a Structured Estimator

**Files:**
- Create: `internal/modules/ai/neuron_estimator.go`
- Create: `internal/modules/ai/neuron_estimator_test.go`
- Modify: `internal/modules/ai/management.go`
- Modify: `internal/modules/ai/gateway.go`
- Modify: `internal/modules/ai/gateway_test.go`
- Modify: `internal/app/server.go`

- **Step 1: Write estimator unit tests**

  Define fixtures for `@cf/meta/llama-3.2-1b-instruct` using the Cloudflare pricing baseline updated 2026-07-08: 2,457 neurons per million input tokens and 18,252 per million output tokens. Assert:

  ```go
  measurement := estimator.Measure("@cf/meta/llama-3.2-1b-instruct", TokenUsage{
      Input: 1_000_000, Output: 1_000_000, Exact: true,
  }, true)
  if measurement.Neurons != 20_709 || measurement.Source != "official_rate_actual_tokens" {
      t.Fatalf("measurement = %#v", measurement)
  }
  ```

  Add cases for estimated tokens (`official_rate_estimated_tokens`), unknown text model (`fallback_text`), failed request without usage (zero, `failed_without_usage`) and response JSON containing `usage.prompt_tokens` / `usage.completion_tokens`.

- **Step 2: Write stream usage parser tests**

  Feed fragmented SSE bytes containing a final `usage` object through the transparent reader and assert the downstream bytes are byte-for-byte identical while `TokenUsage` captures the final counters. Add a stream without usage and assert the byte-count fallback remains available.

- **Step 3: Run estimator tests and verify compile failure**

  Run: `go test ./internal/modules/ai -run 'TestNeuronEstimator|TestUsageCapture' -count=1`

  Expected: FAIL because estimator types do not exist.

- **Step 4: Implement the estimator contract**

  Use these types:

  ```go
  type TokenUsage struct {
      Input  int64
      Output int64
      Exact  bool
  }

  type UsageMeasurement struct {
      InputTokens  int64
      OutputTokens int64
      Neurons      float64
      Source       string
  }

  type NeuronRate struct {
      InputPerMillion  float64
      OutputPerMillion float64
  }

  type NeuronEstimator struct {
      mu    sync.RWMutex
      rates map[string]NeuronRate
  }

  func NewNeuronEstimator() *NeuronEstimator
  func (e *NeuronEstimator) UpdateCatalog(models []map[string]any)
  func (e *NeuronEstimator) Measure(model string, usage TokenUsage, success bool) UsageMeasurement
  ```

  Seed versioned text/embedding rates from Cloudflare's official pricing page (`https://developers.cloudflare.com/workers-ai/platform/pricing/`, baseline 2026-07-08). Parse compatible numeric Neuron properties from the live model catalog when present; a catalog value replaces the bundled rate for that model. Keep non-token modalities on named task-specific fallbacks and mark their source rather than pretending text-token precision.

- **Step 5: Feed the catalog and share the estimator**

  Add `Estimator *NeuronEstimator` to `Management`; after policy filtering call `Estimator.UpdateCatalog(filtered)`. In `server.go`, construct one estimator and inject it into `Management` and `Gateway`.

- **Step 6: Replace `EstimateNeurons` and capture actual usage**

  Add `Estimator *NeuronEstimator` to `Gateway`. For non-stream JSON, parse the upstream `usage` object before writing the response. For SSE, wrap the upstream body with the transparent usage-capturing reader before passthrough or Responses translation. If usage is absent, retain request/response byte-derived token counts as estimated values.

  Change `record` to accept one `UsageMeasurement` and persist `neuron_estimation_source`:

  ```go
  func (g Gateway) record(ctx context.Context, credentialID, accountID, model string,
      status int, measurement UsageMeasurement, duration time.Duration, requestErr error)
  ```

  Locally blocked calls and upstream failures without usage must pass a zero measurement. Remove the old model-name substring implementation of `EstimateNeurons` after all callers use the estimator.

- **Step 7: Run estimator and gateway tests**

  Run: `go test ./internal/modules/ai -run 'TestNeuron|TestUsage|TestGateway' -race -count=1`

  Expected: PASS; existing SSE passthrough remains byte-for-byte compatible.

- **Step 8: Commit structured metering**

  ```bash
  git add internal/modules/ai/neuron_estimator.go internal/modules/ai/neuron_estimator_test.go internal/modules/ai/management.go internal/modules/ai/gateway.go internal/modules/ai/gateway_test.go internal/app/server.go
  git commit -m "feat: estimate Workers AI neurons from model rates"
  ```

## Task 6: Build the Two-level Daily Usage Service and API

**Files:**
- Create: `internal/modules/ai/usage.go`
- Create: `internal/modules/ai/usage_test.go`
- Modify: `internal/platform/httpapi/api.go`
- Modify: `internal/platform/httpapi/api_test.go`
- Modify: `internal/app/server.go`

- **Step 1: Write UTC date parsing tests**

  Test empty input against an injected UTC clock, valid leap day `2028-02-29`, invalid `2026-02-29`, timestamps, and whitespace. The parser must return an exact UTC half-open range.

  ```go
  day, err := ParseUsageDate("2026-07-29", fixedNow)
  if err != nil || day.Location() != time.UTC || day.Format("2006-01-02") != "2026-07-29" {
      t.Fatalf("day = %v, err = %v", day, err)
  }
  ```

- **Step 2: Write the aggregation fixture**

  Create two enabled AI-capable accounts; give the first two active AI credentials, one revoked credential, one deleted credential log, one NULL credential log and one legacy `"admin"` log. Leave the second account without requests. Insert fixed nanosecond timestamps around UTC midnight.

  Assert the report includes both accounts, account 1 child values sum to its total, account 2 has used 0/remaining 10,000, revoked/deleted labels are stable, and NULL plus legacy `admin` rows combine under `unattributed`.

- **Step 3: Run service tests and verify failure**

  Run: `go test ./internal/modules/ai -run 'TestParseUsageDate|TestDailyUsage' -count=1`

  Expected: FAIL because `UsageService` and the report types do not exist.

- **Step 4: Implement report types and aggregation**

  Use this public contract:

  ```go
  const DailyFreeNeurons = 10_000.0

  type CredentialUsage struct {
      CredentialID string  `json:"credential_id"`
      Name string          `json:"name"`
      Status string        `json:"status"`
      EstimatedUsed float64 `json:"estimated_used_neurons"`
      Requests int64       `json:"requests"`
      Errors int64         `json:"errors"`
  }

  type AccountUsage struct {
      AccountID string      `json:"account_id"`
      AccountName string    `json:"account_name"`
      EstimatedUsed float64 `json:"estimated_used_neurons"`
      EstimatedRemaining float64 `json:"estimated_remaining_neurons"`
      Requests int64       `json:"requests"`
      Errors int64         `json:"errors"`
      Credentials []CredentialUsage `json:"credentials"`
  }

  type DailyUsageReport struct {
      Date string           `json:"date"`
      Timezone string       `json:"timezone"`
      DailyLimit float64    `json:"daily_limit_neurons"`
      Estimated bool        `json:"estimated"`
      Accounts []AccountUsage `json:"accounts"`
  }

  type UsageService struct {
      DB *sql.DB
      Accounts *accounts.Store
      Now func() time.Time
  }

  func ParseUsageDate(raw string, now time.Time) (time.Time, error)
  func (s UsageService) Daily(ctx context.Context, day time.Time) (DailyUsageReport, error)
  ```

  Filter account store results to enabled accounts with an available AI capability, independent of current health state. Use `ai_usage_daily` for account totals and one grouped `ai_request_logs LEFT JOIN protocol_credentials` query for child rows. Treat NULL, empty and legacy `admin` IDs as `unattributed`; a non-empty missing credential becomes `deleted`. Sort accounts by name/ID and credentials by used descending then name/ID.

  Remove the obsolete flat `Usage` type and `Gateway.Usage` method from `gateway.go` once `UsageService.Daily` covers their only consumers. Keep `RequestLog` and `Gateway.Logs` unchanged.

- **Step 5: Write failing HTTP response tests**

  Authenticate through the existing `performJSON` helper. Assert `/api/v1/ai/usage?date=2026-07-29` returns `timezone:"UTC"`, `daily_limit_neurons:10000`, `estimated:true` and an `accounts` array. Assert `date=2026-02-29` returns HTTP 400 with code `invalid_date`.

- **Step 6: Wire `UsageService` into the API**

  Add `AIUsage *ai.UsageService` to `httpapi.Dependencies`, construct it in `server.go`, and replace the old `Gateway.Usage` handler call. Return the `DailyUsageReport` directly rather than retaining the old flat `usage` field. Keep `/api/v1/ai/logs` unchanged.

- **Step 7: Run service and HTTP tests**

  Run: `go test ./internal/modules/ai ./internal/platform/httpapi -run 'Usage|InvalidDate' -race -count=1`

  Expected: PASS.

- **Step 8: Commit the report API**

  ```bash
  git add internal/modules/ai/usage.go internal/modules/ai/usage_test.go internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go internal/app/server.go
  git commit -m "feat: report daily AI quota by access key"
  ```

## Task 7: Build the Usage Quota Subpage

**Files:**
- Modify: `web/src/pages/AIPage.tsx`
- Modify: `web/src/styles/pages.css`
- Modify: `web/src/styles/responsive.css`

- **Step 1: Replace the flat frontend usage types**

  Define frontend interfaces that exactly match Task 6:

  ```ts
  interface CredentialUsage {
    credential_id: string;
    name: string;
    status: "active" | "revoked" | "deleted" | "unattributed";
    estimated_used_neurons: number;
    requests: number;
    errors: number;
  }

  interface AccountUsage {
    account_id: string;
    account_name: string;
    estimated_used_neurons: number;
    estimated_remaining_neurons: number;
    requests: number;
    errors: number;
    credentials: CredentialUsage[];
  }

  interface DailyUsageReport {
    date: string;
    timezone: "UTC";
    daily_limit_neurons: number;
    estimated: true;
    accounts: AccountUsage[];
  }
  ```

- **Step 2: Add date and expansion state**

  Store the selected UTC date as `new Date().toISOString().slice(0, 10)` and account IDs in `Set<string>`. Make `load` accept the current date and request `/api/v1/ai/usage?date=${encodeURIComponent(usageDate)}`. Keep refresh using the selected date.

- **Step 3: Render the account quota rows**

  Rename the tab label to “用量额度”. Add an accessible date input and the visible notice “本地估算，非 Cloudflare 官方账单；每日 00:00 UTC 重置”. Each account row uses a chevron icon button with `aria-expanded`, and displays limit, used, remaining, progress, requests and errors. Clamp only the progress width:

  ```ts
  const percent = Math.min(100, (item.estimated_used_neurons / report.daily_limit_neurons) * 100);
  ```

  Preserve actual used/remaining text; when used exceeds 10,000, show an “已超出” status beside zero remaining.

- **Step 4: Render credential contributions**

  Under an expanded account, render one unframed detail table with credential name, status, used Neurons, percentage of account usage, requests and errors. Use existing `Status` mapping and a sentence stating that all keys share the account remainder. Do not render a per-key “remaining” column.

- **Step 5: Remove Playground manual model fallback**

  Always render the model selector. When `modelNames` is empty, disable it and the send button, show the existing error/empty state, and offer refresh through the page header. Remove the free-text model input and do not append a model that is absent from the filtered catalog.

- **Step 6: Add scoped responsive styles**

  In `pages.css`, add `.ai-usage-toolbar`, `.ai-usage-table`, `.ai-account-toggle`, `.ai-usage-meter`, `.ai-key-details` and status styles using existing color tokens. In `responsive.css` at `max-width: 720px`, hide the desktop header and render each account/key row as a label/value grid via `data-label`, following the existing `.access-table` pattern. Ensure long names use `overflow-wrap:anywhere` and numeric cells use tabular numerals.

- **Step 7: Run TypeScript and production build checks**

  Run: `npm --prefix web run lint`

  Expected: PASS with no TypeScript errors.

  Run: `npm --prefix web run build`

  Expected: PASS and Vite emits the production bundle.

- **Step 8: Commit the Workers AI page**

  ```bash
  git add web/src/pages/AIPage.tsx web/src/styles/pages.css web/src/styles/responsive.css
  git commit -m "feat: add AI quota usage subpage"
  ```

## Task 8: Migrate the Overview Consumer

**Files:**
- Modify: `web/src/pages/OverviewPage.tsx`

- **Step 1: Replace the old response type and state**

  Import or define the minimal report shape used by Overview:

  ```ts
  interface AIUsageSummary {
    accounts: Array<{ estimated_used_neurons: number }>;
  }
  ```

  Initialize it as `{ accounts: [] }` and request `api.get<AIUsageSummary>("/api/v1/ai/usage")`.

- **Step 2: Update the fulfilled-response assignment and total**

  Replace `usageData.value.usage` and the old reducer with:

  ```ts
  if (usageData.status === "fulfilled") setUsage(usageData.value);
  const neurons = usage.accounts.reduce(
    (sum, item) => sum + Number(item.estimated_used_neurons ?? 0),
    0,
  );
  ```

- **Step 3: Run the frontend checks**

  Run: `npm --prefix web run lint && npm --prefix web run build`

  Expected: both commands PASS and the overview no longer references `usageData.value.usage`.

- **Step 4: Commit the consumer migration**

  ```bash
  git add web/src/pages/OverviewPage.tsx
  git commit -m "fix: migrate overview AI usage summary"
  ```

## Task 9: Full Regression and Browser Verification

**Files:**
- Modify only files required to fix failures found by the commands below.

- **Step 1: Format changed Go files**

  Run:

  ```powershell
  gofmt -w internal/modules/ai/model_policy.go internal/modules/ai/model_policy_test.go internal/modules/ai/neuron_estimator.go internal/modules/ai/neuron_estimator_test.go internal/modules/ai/usage.go internal/modules/ai/usage_test.go internal/modules/ai/management.go internal/modules/ai/management_test.go internal/modules/ai/gateway.go internal/modules/ai/gateway_test.go internal/protocol/ai/handler.go internal/protocol/ai/handler_test.go internal/platform/httpapi/api.go internal/platform/httpapi/api_test.go internal/platform/database/database_test.go internal/app/server.go
  ```

  Expected: command exits 0.

- **Step 2: Run the complete backend suite with the race detector**

  Run: `go test -race -count=1 ./...`

  Expected: PASS for every package; no data race from policy learning, rate updates or usage capture.

- **Step 3: Run static analysis and binary build**

  Run: `go vet ./...`

  Expected: PASS.

  Run: `go build -trimpath ./cmd/cf-r2-manager`

  Expected: PASS and produce the application binary.

- **Step 4: Run the final frontend build**

  Run: `npm --prefix web run build`

  Expected: PASS.

- **Step 5: Start a local server for browser QA**

  Use a disposable local config/database and start the unified listener on an unused loopback port. Seed the database through normal application APIs with two AI-capable account fixtures and logs for active, revoked, deleted and unattributed credentials; do not use production credentials.

  Expected: `/api/v1/ai/usage` returns both accounts and the panel opens without console errors.

- **Step 6: Verify desktop behavior**

  At a 1440×900 viewport, verify the Workers AI “用量额度” tab, UTC date switch, account expansion, shared-balance notice, long credential name wrapping, zero-use account and over-limit presentation. Verify `@cf/zai-org/glm-5.2` is absent from both the model table and Playground.

- **Step 7: Verify mobile behavior**

  At a 390×844 viewport, verify account and key rows switch to label/value layout, expand controls remain visible, no text overlaps, and horizontal page scrolling is absent.

- **Step 8: Verify gateway behavior on the unified port**

  With a test AI credential, call `/v1/models` and confirm the paid model is absent. Call a test upstream fixture that returns the Paid-plan 403 twice: the first request reaches upstream and learns; the second is rejected locally with `model_not_available`.

- **Step 9: Inspect the final diff**

  Run: `git diff --check && git status --short`

  Expected: no whitespace errors; only planned source, migration, test, spec and plan files are changed.

- **Step 10: Commit verification fixes**

  If verification required source changes, commit only those fixes:

  ```bash
  git add internal web
  git commit -m "test: harden AI model and quota workflows"
  ```

  If no files changed during verification, skip this commit.
