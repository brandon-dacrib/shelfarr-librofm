# Libro.fm Shelfarr Acquisition Provider

This is a private, self-hosted [Shelfarr Custom Acquisition Provider](https://github.com/Pedro-Revez-Silva/shelfarr/blob/main/docs/custom-acquisition-providers.md) for **audiobooks already owned by the configured Libro.fm account**. It does not browse the Libro.fm store, buy books, or expose Libro.fm credentials to Shelfarr.

## Why Go

Go is the best fit here: one small statically linked binary, standard-library HTTP/cookie handling, simple deployment, and a genuine `scratch` runtime image with no shell, package manager, or operating-system files. Python would reuse more of the referenced sync scripts, but requires a Python runtime and dependencies; this provider is a request-time HTTP proxy and benefits more from Go's compact, dependency-free deployment.

## What it does

1. Shelfarr calls `POST /search`.
2. A conservative background sync logs into Libro.fm with `LIBROFM_USERNAME` and `LIBROFM_PASSWORD`, reads the owned-library pages, and persists titles that offer an M4B download.
3. Shelfarr calls `POST /acquire` for a selected title.
4. The provider returns a short-lived HMAC-signed `/download/...` URL.
5. Shelfarr downloads that URL; the provider opens a fresh authenticated Libro.fm session and streams the M4B.

Search and acquisition read that local cache; they never scrape Libro.fm. A download opens only a fresh authenticated session and streams the previously cached M4B link—without enumerating the library. The signed URL never contains a Libro.fm credential, and the provider does not retain credentials or downloaded media. It deliberately supports M4B only: Libro.fm can split MP3 downloads into multiple ZIPs, whereas Shelfarr requires one concrete artifact per acquisition.

Libro.fm has no public API; it can change its website at any time. Run this only with your own account and titles you are entitled to download.

## Configuration

Required environment variables:

| Variable | Purpose |
| --- | --- |
| `LIBROFM_USERNAME` | Libro.fm account email |
| `LIBROFM_PASSWORD` | Libro.fm password |
| `DOWNLOAD_SIGNING_KEY` | 32+ random characters used only for temporary direct-download URLs |
| `PUBLIC_BASE_URL` | URL Shelfarr uses to reach this provider, e.g. `http://shelfarr-librofm:8080` |

Optional variables:

| Variable | Default |
| --- | --- |
| `PROVIDER_BEARER_TOKEN` | unset; protect `/search` and `/acquire` with Shelfarr's configured bearer token |
| `DOWNLOAD_URL_TTL` | `15m`; maximum `1h` |
| `LISTEN_ADDR` | `:8080` |
| `SYNC_INTERVAL` | `24h`; must be between `6h` and `168h` |
| `SYNC_ON_START` | `true`; runs one sync after process start |
| `STATE_DIR` | `/state`; must persist across pod restarts |

Never put these values in Git. With the 1Password Operator, create an item whose generated Kubernetes Secret has `username`, `password`, `download-signing-key`, and (if used) `provider-bearer-token` fields. The example manifests in [`deploy/kubernetes`](deploy/kubernetes) consume exactly those fields.

## Shelfarr setup

In **Admin → Acquisition Providers**, add:

| Field | Value |
| --- | --- |
| Name | `Libro.fm owned library` |
| Base URL | `http://shelfarr-librofm:8080` |
| Bearer Token | Same as `PROVIDER_BEARER_TOKEN`, or blank if disabled |
| Media Types | Audiobooks |
| Allow private network | Enabled (it is an in-cluster Service) |

Use **Test** to call `/health`. Shelfarr must be able to resolve and connect to the `PUBLIC_BASE_URL`, too.

`POST /sync` and `GET /sync-status` are bearer-protected administrative endpoints. The deployment keeps the cache on a small PVC and syncs at most once every 24 hours by default; the service never performs an expensive full-library scan in response to a Shelfarr search. Use `POST /sync` only after a new Libro.fm purchase when you need it immediately.

## Build and test

```sh
go test ./...
docker build -t librofm-shelfarr-provider:dev .
```

The Dockerfile begins with a no-distro `scratch` stage and ends in `scratch`; the Go builder stage is never included in the runtime image.

The image is multi-architecture: `linux/amd64` and `linux/arm64`. The included GitHub Actions workflow publishes a single multi-arch manifest to GHCR on pushes to `main` and version tags. Locally, use Buildx:

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/OWNER/shelfarr-librofm:TAG --push .
```

## Limitations and security

- The service is intended for a trusted private network; do not publish it publicly.
- A caller with a valid signed URL can download its one selected audiobook until the URL expires. Keep `DOWNLOAD_SIGNING_KEY` secret and rotate it to invalidate all outstanding URLs.
- Search/acquire are protected by `PROVIDER_BEARER_TOKEN` when configured. Direct artifact requests use their individual signed URL because Shelfarr direct downloads do not forward the provider bearer token.
- Libro.fm HTML changes can break parsing. Failures return `502` and are logged without credentials.
