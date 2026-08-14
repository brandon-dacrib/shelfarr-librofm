# AGENTS.md

## Project purpose

`shelfarr-librofm` is a private-network Shelfarr Custom Acquisition Provider for audiobooks **already owned** by one Libro.fm account. It is not a store browser, purchasing client, or general Libro.fm API.

## Non-negotiable security rules

- Never commit account credentials, bearer tokens, signing keys, session cookies, downloaded books, HTTP fixtures from a real account, or `.env` files.
- Secrets enter only through environment variables backed by the 1Password Operator. Use placeholder values in examples.
- Keep the final container image `FROM scratch`, statically linked, non-root (`65532`), read-only, and without a shell or package manager.
- Keep provider search/acquire endpoints bearer-protected when `PROVIDER_BEARER_TOKEN` is configured. Direct downloads must use short-lived HMAC-signed URLs because Shelfarr does not forward that bearer token.
- Do not loosen `absoluteLibroURL`: authenticated upstream requests may only go to `https://libro.fm`.
- Do not log credentials, cookies, authorization headers, signed URLs, or Libro.fm response bodies.
- Before every public push, run `gitleaks git --redact --verbose .`, inspect `git status`, and run `git diff --check`.

## Libro.fm politeness and rate limits

- Search and acquire must read only the persisted cache; never trigger a Libro.fm library crawl.
- Full library sync is at most once per `SYNC_INTERVAL` (default `24h`; minimum `6h`).
- Manual `POST /sync` is protected and rate-limited by `MANUAL_SYNC_MIN_INTERVAL` (default `6h`), persisted through restarts.
- A download may open a fresh authenticated session only to retrieve the one cached artifact; it must not enumerate `/user/library`.
- Do not add concurrency, retries, or automatic rapid polling without an explicit backoff and a documented reason.

## Development and release

- Use Go standard library where practical; preserve static cross-compilation for `linux/amd64` and `linux/arm64`.
- Run `gofmt -w`, `go test ./...`, and cross-compile both Linux targets before release.
- Keep the GitHub Actions image workflow multi-arch and publish only tagged semantic releases to deployment manifests.
- Docker daemon availability is not assumed; static cross-compiles are the minimum local build verification.

## Kubernetes deployment

- The canonical deployment is in the infrastructure repository at `talos-clusters/dacrib0/apps/doris/shelfarr-librofm/`.
- The `shelfarr-librofm` 1Password item lives in the `dacrib0` vault and must expose exactly: `username`, `password`, `download-signing-key`, and `provider-bearer-token`.
- Keep the cache PVC; without it, a restart defeats the sync-rate limit.
- Validate manifests with `kubectl apply --dry-run=client` before applying. After rollout, verify the OnePasswordItem, Secret, PVC, Deployment readiness, `/health`, and `/sync-status` before adding the provider in Shelfarr.
