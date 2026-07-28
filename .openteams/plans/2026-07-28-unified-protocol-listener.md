# Unified Protocol Listener Implementation Plan

**Goal:** Serve the admin console, S3, WebDAV, and the OpenAI-compatible API from one public HTTP listener while making `/v1/models` consumable by standard OpenAI clients.

**Architecture:** A focused protocol multiplexer classifies requests without rewriting their path or Host, then delegates to the existing handlers. Configuration gains one canonical HTTP listener with an in-memory fallback from the legacy admin address, while model listing uses the existing Cloudflare management client and normalizes its result locally.

**Tech Stack:** Go 1.26, `net/http`, YAML configuration, React 19, TypeScript, Vite, Go `httptest`.

---

### Task 1: Lock Configuration Migration Behavior

**Files:**
- Modify: `internal/platform/config/config.go`
- Modify: `internal/platform/config/config_test.go`
- Modify: `packaging/config.yaml`
- Modify: `config.example.yaml`

- **Step 1: Write failing tests for the canonical listener**

Add cases asserting that `listeners.http` wins over `listeners.admin`, a legacy-only admin address populates the effective HTTP address, old S3/WebDAV/AI defaults are no longer active, and metrics still reject public addresses.

- **Step 2: Run the focused test**

Run: `go test ./internal/platform/config -run 'TestLoad|TestValidate'`
Expected: FAIL because the HTTP listener does not exist.

- **Step 3: Implement the migration**

Add `HTTP` to `Listeners`, make it the only required public listener, preserve the legacy fields for YAML decoding and warnings, and apply `admin` only when the YAML did not explicitly provide `http`.

- **Step 4: Update shipped configuration**

Replace the four public protocol addresses with `http: 0.0.0.0:14325`; retain only the loopback metrics listener.

- **Step 5: Re-run the focused test**

Run: `go test ./internal/platform/config`
Expected: PASS.

### Task 2: Add The Protocol Multiplexer

**Files:**
- Create: `internal/app/protocol_mux.go`
- Create: `internal/app/protocol_mux_test.go`
- Modify: `internal/app/server.go`

- **Step 1: Write table-driven routing tests**

Use marker handlers to assert routing for AWS Authorization, S3 presigned queries, Basic Authorization, PROPFIND/MKCOL/COPY/MOVE/LOCK/UNLOCK/OPTIONS, supported AI paths with and without Bearer tokens, and ordinary panel/API paths. Include signed `/v1/models` and Basic `/v1/models` to verify authentication schemes take priority over path routing.

- **Step 2: Run the focused test**

Run: `go test ./internal/app -run TestProtocolMux`
Expected: FAIL because the multiplexer is undefined.

- **Step 3: Implement classification without mutation**

Add a small handler with `Admin`, `S3`, `WebDAV`, and `AI` dependencies. Detect AWS SigV4 from the Authorization scheme or `X-Amz-Algorithm=AWS4-HMAC-SHA256`, Basic from its Authorization scheme, DAV from its method set, and AI by the same supported paths as the AI handler. Do not strip prefixes or alter `Host`, `URL.Path`, or query parameters.

- **Step 4: Replace four servers with one**

Instrument each delegated handler with its existing protocol label, start one `http` server on `listeners.http`, keep metrics separate, and log ignored legacy protocol addresses once at startup.

- **Step 5: Re-run routing and existing protocol tests**

Run: `go test ./internal/app ./internal/protocol/s3 ./internal/protocol/webdav`
Expected: PASS.

### Task 3: Normalize The OpenAI Model Catalog

**Files:**
- Modify: `internal/modules/ai/management.go`
- Modify: `internal/modules/ai/management_test.go`
- Modify: `internal/protocol/ai/handler.go`
- Create: `internal/protocol/ai/handler_test.go`

- **Step 1: Write failing pagination and normalization tests**

Serve two Cloudflare catalog pages with overlapping model names and assert every page is requested, duplicate IDs are removed, and the normalized response is sorted. Add handler tests for valid credentials, invalid credentials, no AI-capable account, and upstream failure.

- **Step 2: Run the focused tests**

Run: `go test ./internal/modules/ai ./internal/protocol/ai`
Expected: FAIL on pagination and local model-list behavior.

- **Step 3: Add catalog pagination**

Read Cloudflare `result_info` when present, request pages of 100, stop at `total_pages` or a short page, and guard against repeated/empty pages. Keep the existing management call behavior for gateways.

- **Step 4: Serve the OpenAI model shape locally**

Inject a catalog function into the AI handler. After normal AI credential and scope verification, map each non-empty `name` or `id` to `{id, object:"model", created:0, owned_by:"cloudflare"}`, deduplicate, sort, and return `{object:"list", data:[...]}`. Keep all inference paths on the existing forwarding gateway.

- **Step 5: Re-run AI tests**

Run: `go test ./internal/modules/ai ./internal/protocol/ai`
Expected: PASS.

### Task 4: Expose And Display Connection Information

**Files:**
- Modify: `internal/platform/httpapi/api.go`
- Modify: `internal/platform/httpapi/api_test.go`
- Modify: `web/src/pages/AccessPage.tsx`
- Modify: `web/src/styles/pages.css`
- Modify: `web/src/styles/responsive.css`

- **Step 1: Write the protected endpoint test**

Assert unauthenticated access is rejected and authenticated access returns panel, S3, WebDAV, and AI URLs derived from the request origin plus the configured logical bucket.

- **Step 2: Implement endpoint metadata**

Pass the logical bucket through HTTP API dependencies and add `GET /api/v1/system/endpoints`. Build the origin from TLS or `X-Forwarded-Proto` plus the request Host, returning AI with `/v1` and root URLs for the other protocols.

- **Step 3: Add connection details to the access page**

Load endpoint metadata with credentials, display the relevant endpoint and bucket for each credential kind, and provide copy buttons using existing icon-button and toast patterns. Keep secret reveal/rotate/revoke behavior unchanged.

- **Step 4: Build the frontend**

Run: `npm run build --prefix web`
Expected: TypeScript and Vite complete successfully.

### Task 5: Update Deployment Guidance And Verify End To End

**Files:**
- Modify: `README.md`
- Modify: `docs/configuration.md`
- Modify: `docs/api.md`
- Modify: `packaging/scripts/install.sh`
- Modify: `packaging/nginx/cf-r2-manager.conf`
- Modify: `packaging/caddy/Caddyfile`

- **Step 1: Document the breaking endpoint migration**

Replace the four-port instructions with the unified addresses, state that old protocol ports stop listening after restart, document `/v1/models`, and retain the recommendation for HTTPS.

- **Step 2: Collapse proxy examples**

Use one public hostname and one backend on port 14325, preserve Host, disable request/response buffering for uploads and streams, and use unlimited body size where supported.

- **Step 3: Run repository verification**

Run: `go test ./...`
Expected: PASS.

Run: `npm run build --prefix web`
Expected: PASS.

Run: `git diff --check`
Expected: no whitespace errors.

- **Step 4: Verify the running application**

Start a local initialized server and verify on one port: admin health/login, authenticated S3 operations, WebDAV discovery and file operations, AI `/v1/models`, and AI chat. Confirm no process listens on the legacy three ports.

- **Step 5: Verify the UI**

At desktop and mobile widths, confirm every credential row shows a readable endpoint, copy buttons work, and no labels or actions overlap.
