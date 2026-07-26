# CF-R2Manager v1 Implementation Plan

**Goal:** Build a Linux-first Go control plane with a unified R2 pool, S3 and WebDAV protocols, D1 administration, and a Workers AI gateway.

**Architecture:** A modular monolith shares encrypted account credentials, SQLite persistence, jobs, audit events, and metrics. React assets are embedded in the Go binary, while admin, S3, WebDAV, and AI use separate loopback listeners.

**Tech Stack:** Go 1.26, React, TypeScript, Vite, SQLite, Cloudflare Go SDK, AWS SDK v2, x/net/webdav, Prometheus, systemd.

---

## Delivery Order

1. Scaffold the Go module, embedded React application, configuration, CLI, CI, and Linux packaging.
2. Add SQLite migrations, AES-GCM secrets, Argon2id admin authentication, accounts, capabilities, jobs, audit, and metrics.
3. Add the R2 object index, placement policy, recovery state machine, S3 core API, and WebDAV filesystem.
4. Add D1 CRUD, schema/data browsing, conservative SQL classification, imports, exports, backups, and Time Travel endpoints.
5. Add Workers AI OpenAI-compatible proxying, account routing, usage estimates, logs, and AI Gateway management endpoints.
6. Complete the operational React UI, fault tests, live smoke-test workflow, systemd hardening, release artifacts, checksums, and SBOM.

## Verification

Every vertical slice adds unit tests, HTTP contract tests, and an executable UI path. The release gate runs `go test -race ./...`, frontend lint/build, Playwright, protocol smoke tests, cross-compilation, and optional Cloudflare live tests.

## Implementation Status (2026-07-25)

Completed:

- Go/React embedded executable, YAML configuration, SQLite WAL migrations, CLI, encrypted secrets, administrator sessions, accounts, capability detection, jobs, audit, and metrics.
- Unified R2 index, quota-aware placement, bounded-memory disk spooling, S3 SigV4 and presigned requests, ListObjects V1/V2 delimiter pagination, copy, batch delete, and persistent multipart create/upload/copy/list/complete/abort operations.
- WebDAV methods and persistent locks.
- R2 adoption, orphan findings, startup recovery, additive index rebuild, and explicit source-to-target rebalance jobs and management APIs.
- D1 CRUD, conservative SQL write protection, parameter binding, query history, R2 backup, and backup-gated Time Travel restore.
- Workers AI OpenAI-compatible/native endpoints, SSE passthrough, account routing, local Neuron estimates, request metadata logs, and AI Gateway management.
- Operational React console, Linux systemd/reverse-proxy/install assets, Apache-2.0 license, CI, cross-compilation, checksums, and SBOM workflows.

Required before `v1.0`:

- WebDAV Litmus and multipart process-crash fault suites.
- D1 import UI and GraphQL analytics (schema/data browsing, R2 export, and local inefficient-query analysis are complete).
- Workers AI fine-tuning, datasets, evaluations, provider configuration, and dynamic routing management (model catalog and metadata logs are complete).
- Playwright CI and private live Cloudflare smoke tests.
- Workers, KV, Queues, Vectorize, Hyperdrive, and Pages modules after the core release stabilizes.
