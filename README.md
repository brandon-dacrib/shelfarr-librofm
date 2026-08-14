# Libro.fm Shelfarr Acquisition Provider

This is a private, self-hosted [Shelfarr Custom Acquisition Provider](https://github.com/Pedro-Revez-Silva/shelfarr/blob/main/docs/custom-acquisition-providers.md) for **audiobooks already owned by the configured Libro.fm account**. It does not browse the Libro.fm store, buy books, or expose Libro.fm credentials to Shelfarr.

## Why Go

Go is the best fit here: one small statically linked binary, standard-library HTTP/cookie handling, simple deployment, and a genuine `scratch` runtime image with no shell, package manager, or operating-system files. Python would reuse more of the referenced sync scripts, but requires a Python runtime and dependencies; this provider is a request-time HTTP proxy and benefits more from Go's compact, dependency-free deployment.

## What it does

1. Shelfarr calls `POST /search`.
2. The provider logs into Libro.fm with `LIBROFM_USERNAME` and `LIBROFM_PASSWORD`, reads the owned-library pages, and returns matching titles that offer an M4B download.
3. Shelfarr calls `POST /acquire` for a selected title.
4. The provider returns a short-lived HMAC-signed `/download/...` URL.
5. Shelfarr downloads that URL; the provider opens a fresh authenticated Libro.fm session and streams the M4B.

The signed URL never contains a Libro.fm credential, and the provider does not retain credentials or downloaded media. It deliberately supports M4B only: Libro.fm can split MP3 downloads into multiple ZIPs, whereas Shelfarr requires one concrete artifact per acquisition. The two reference projects use the same documented website-session approach and M4B-first strategy.

Libro.fm has no public API; it can change its website at any time. Run this only with your own account and titles you are entitled to download.

## Configuration

Required environment variables:

| Variable | Purpose |
| --- | --- |
| `LIBROFM_USERNAME` | Libro.fm account email |
| `LIBROFM_PASSWORD` | Libro.fm password |
| `DOWNLOAD_SIGNING_KEY` | 32+ random characters used only for temporary direct-download URLs |
| `PUBLIC_BASE_URL` | URL Shelfarr uses to reach this provider, e.g. `http://librofm-shelfarr-provider:8080` |

Optional variables:

| Variable | Default |
| --- | --- |
| `PROVIDER_BEARER_TOKEN` | unset; protect `/search` and `/acquire` with Shelfarr's configured bearer token |
| `DOWNLOAD_URL_TTL` | `15m`; maximum `1h` |
| `LISTEN_ADDR` | `:8080` |

Never put these values in Git. With the 1Password Operator, create an item whose generated Kubernetes Secret has `username`, `password`, `download_signing_key`, and (if used) `provider_bearer_token` fields. The example manifests in [`deploy/kubernetes`](deploy/kubernetes) consume exactly those fields.

## Shelfarr setup

In **Admin → Acquisition Providers**, add:

| Field | Value |
| --- | --- |
| Name | `Libro.fm owned library` |
| Base URL | `http://librofm-shelfarr-provider:8080` |
| Bearer Token | Same as `PROVIDER_BEARER_TOKEN`, or blank if disabled |
| Media Types | Audiobooks |
| Allow private network | Enabled (it is an in-cluster Service) |

Use **Test** to call `/health`. Shelfarr must be able to resolve and connect to the `PUBLIC_BASE_URL`, too.

## Build and test

```sh
go test ./...
docker build -t librofm-shelfarr-provider:dev .
```

The Dockerfile begins with a no-distro `scratch` stage and ends in `scratch`; the Go builder stage is never included in the runtime image.

## Limitations and security

- The service is intended for a trusted private network; do not publish it publicly.
- A caller with a valid signed URL can download its one selected audiobook until the URL expires. Keep `DOWNLOAD_SIGNING_KEY` secret and rotate it to invalidate all outstanding URLs.
- Search/acquire are protected by `PROVIDER_BEARER_TOKEN` when configured. Direct artifact requests use their individual signed URL because Shelfarr direct downloads do not forward the provider bearer token.
- Libro.fm HTML changes can break parsing. Failures return `502` and are logged without credentials.
