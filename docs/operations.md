# Operations and recovery

## Health and logs

Use `/healthz` for process liveness and `/readyz` for database readiness. The
systemd service writes structured JSON logs to the journal:

```sh
journalctl -u cf-r2-manager -f
curl --fail http://127.0.0.1:14325/readyz
curl --fail http://127.0.0.1:9090/metrics
```

## Backup and restore

Database backups use SQLite `VACUUM INTO` and refuse to overwrite an existing
file:

```sh
sudo -u cf-r2-manager cf-r2-manager backup \
  --config /opt/CloudFlareManager/config.yaml \
  --output /opt/CloudFlareManager/data/backups/manager-$(date +%F).db
```

Stop the service before restore. `--force` preserves the current database next
to it before activating the validated backup.

```sh
sudo systemctl stop cf-r2-manager
sudo -u cf-r2-manager cf-r2-manager restore \
  --config /opt/CloudFlareManager/config.yaml --input /path/to/manager.db --force
sudo systemctl start cf-r2-manager
```

Back up the master key separately. A database backup without its matching key
cannot decrypt Cloudflare or protocol credentials.

## R2 recovery

Every server start enqueues an idempotent recovery job. It reconciles interrupted
single-part writes and deletes by comparing the internal write ID with upstream
metadata, resolves multipart uploads that reached the completing state, and
retries old-copy cleanup. Manual recovery is also available from the admin API.

Client-created multipart uploads expire after 24 hours without activity. An
hourly job aborts them upstream before releasing their reserved capacity. When
the upstream abort or object state cannot be confirmed, the write intent and
reservation remain in place and the job is retried. This deliberately favors
capacity safety over optimistic reuse.

Use adoption only for a bucket that CF-R2Manager will own exclusively. Orphan
scans record unindexed remote keys in `r2_scan_findings`; they never delete them.
Index rebuilds add missing keys and preserve every existing logical mapping.

Cross-bucket replacements commit the new mapping first and persist an ETag-
guarded cleanup record for the old copy. The source bucket's usage is reduced
only after deletion is confirmed. A cleanup record also fences that physical
bucket/key pair so a later version cannot be deleted by a stale cleanup.

Before upgrading to a release with transactional R2 placement, create a database
backup. The migration is additive, but an older binary does not understand the
reservation ledger. Rolling back therefore requires restoring the pre-upgrade
database rather than starting the old binary against the migrated database.

CI or an operator can run the opt-in two-account R2 smoke test with
`CF_R2_SMOKE=1`. Provide `CF_R2_SMOKE_ACCOUNT_1_ID`,
`CF_R2_SMOKE_ACCOUNT_1_ACCESS_KEY_ID`,
`CF_R2_SMOKE_ACCOUNT_1_SECRET_ACCESS_KEY`, and
`CF_R2_SMOKE_ACCOUNT_1_BUCKET`, plus the corresponding `_2_` variables. The
test uses a unique `cf-r2-manager-smoke/` prefix and removes objects and
multipart uploads before returning.
