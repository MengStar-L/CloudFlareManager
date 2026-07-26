# Security model

CF-R2Manager is designed for one administrator behind a trusted HTTPS reverse
proxy. The admin and metrics listeners should remain on loopback.

- Cloudflare tokens and R2 S3 keys are encrypted with AES-256-GCM.
- The 32-byte master key is injected by systemd as the `master-key` credential.
- The administrator password is stored with Argon2id.
- Admin sessions, S3 keys, WebDAV users, and AI keys are independent and can be
  scoped, rotated, and revoked independently.
- Management writes require a session CSRF token.
- Prompt and response bodies are not written to request logs.
- Temporary upload files use mode `0600` and are removed after use.

Grant each Cloudflare account token only the products that account will use.
R2 S3 credentials are optional but required before a physical bucket can serve
data. Treat a managed physical bucket as exclusively owned by this application;
out-of-band writes are visible only after adoption or index rebuild.

The supplied systemd unit removes Linux capabilities, limits address families,
uses a private temporary directory, and makes the host filesystem read-only
except for `/var/lib/cf-r2-manager`.
