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
single-part writes, retries interrupted deletes, and resolves multipart uploads
that reached the completing state. Manual recovery is also available from the
admin API.

Use adoption only for a bucket that CF-R2Manager will own exclusively. Orphan
scans record unindexed remote keys in `r2_scan_findings`; they never delete them.
Index rebuilds add missing keys and preserve every existing logical mapping.

Rebalance streams each source object into the selected target bucket, commits
the new mapping, then deletes the old physical copy. If source cleanup fails,
the target remains authoritative and a later orphan scan reports the old copy.
