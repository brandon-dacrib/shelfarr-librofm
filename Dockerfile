# The first stage is intentionally distro-less. The final runtime stage is also scratch.
FROM scratch AS no-distro

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/librofm-shelfarr-provider ./cmd/librofm-shelfarr-provider

FROM scratch
COPY --from=build /out/librofm-shelfarr-provider /librofm-shelfarr-provider
# A scratch image has no system trust store. Copy only CA roots needed for
# Libro.fm HTTPS; no distribution userspace is included.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/librofm-shelfarr-provider"]
