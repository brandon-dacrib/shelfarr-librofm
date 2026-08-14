# The first stage is intentionally distro-less. The final runtime stage is also scratch.
FROM scratch AS no-distro

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/librofm-shelfarr-provider ./cmd/librofm-shelfarr-provider

FROM scratch
COPY --from=build /out/librofm-shelfarr-provider /librofm-shelfarr-provider
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/librofm-shelfarr-provider"]
