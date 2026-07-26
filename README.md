# CF-R2Manager

CF-R2Manager is a Linux-first, self-hosted Cloudflare control plane written in
Go. It combines authorized Cloudflare accounts into one indexed R2 namespace
and exposes that namespace through S3 and WebDAV. The same executable also
provides D1 administration, a Workers AI gateway, and an embedded React console.

## Project status

The project is usable for development and controlled self-hosted testing, but
has not reached the `v1.0` release gate. Implemented surfaces include:

- encrypted multi-account credentials and capability detection;
- a persistent R2 object index, placement quotas, adoption, orphan scans,
  interrupted-operation recovery, index rebuilds, and explicit rebalancing;
- S3 SigV4 headers and presigned URLs, ListObjects V1/V2, conditional/range
  reads, copy, batch delete, and persistent multipart uploads;
- WebDAV read/write, collections, copy/move, and locks;
- D1 database CRUD, guarded parameterized SQL, history, R2 backups, and
  backup-before-Time-Travel restore;
- Workers AI OpenAI-compatible endpoints, native model execution, SSE,
  multi-account routing, local Neuron estimates, and AI Gateway management;
- SQLite WAL jobs, audit events, Prometheus metrics, and an embedded web UI.

Before calling the project `v1.0`, it still needs WebDAV Litmus coverage,
Cloudflare live smoke tests, broader D1 import/GraphQL analytics tools, AI
dataset/evaluation/fine-tuning management, and the later Workers/KV/Queues/
Vectorize/Hyperdrive/Pages modules.

## Install on Linux

Download the archive matching the server architecture, verify it against
`checksums.txt`, then run:

```sh
sudo ./packaging/scripts/install.sh ./cf-r2-manager
sudo -u cf-r2-manager /usr/local/bin/cf-r2-manager init \
  --config /etc/cf-r2-manager/config.yaml
sudo systemctl enable --now cf-r2-manager
sudo -u cf-r2-manager /usr/local/bin/cf-r2-manager doctor \
  --config /etc/cf-r2-manager/config.yaml
```

`init` prompts for the administrator password and creates a 32-byte master key
with mode `0600`. The systemd unit injects that key as the `master-key`
credential. An encrypted systemd credential override is provided in
`packaging/systemd/encrypted-credential.conf.example`.

Put the listeners behind HTTPS using the examples in `packaging/nginx` or
`packaging/caddy`. Use separate hostnames for the admin, S3, WebDAV, and AI
listeners. Do not expose the metrics listener beyond loopback.

## Development

Requirements are Go 1.26+, Node.js 24+, and npm 11+.

```sh
make web
make test
make build
```

Initialize and run a local instance:

```sh
bin/cf-r2-manager init --config ./config.example.yaml
bin/cf-r2-manager server --config ./config.example.yaml
```

Default listeners are admin `127.0.0.1:8080`, S3 `127.0.0.1:9000`, WebDAV
`127.0.0.1:9001`, Workers AI `127.0.0.1:9002`, and metrics
`127.0.0.1:9090`.

## Documentation

- [Configuration](docs/configuration.md)
- [HTTP API](docs/api.md)
- [Operations and recovery](docs/operations.md)
- [Security model](docs/security.md)

The software is licensed under Apache-2.0. See `LICENSE` and `NOTICE`.
