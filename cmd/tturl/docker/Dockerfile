# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=devel
ARG REVISION
ARG TREE=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/tturl ./cmd/tturl
COPY internal ./internal
COPY stats ./stats
COPY tth2 ./tth2

RUN test -n "$TARGETOS" && \
    test -n "$TARGETARCH" && \
    test -n "$REVISION" && \
    case "$TREE" in clean|dirty) ;; *) exit 1 ;; esac && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
      -ldflags="-s -w \
      -X github.com/tantosec/tturl/internal/buildinfo.injectedRelease=$VERSION \
      -X github.com/tantosec/tturl/internal/buildinfo.injectedRevision=$REVISION \
      -X github.com/tantosec/tturl/internal/buildinfo.injectedTree=$TREE" \
      -o /out/tturl ./cmd/tturl

FROM scratch

ARG SOURCE=https://github.com/tantosec/tturl
ARG VERSION=devel
ARG REVISION
ARG DESCRIPTION="HTTP/2 CLI for timeless timing attacks and request races"

LABEL org.opencontainers.image.source="$SOURCE" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="tturl" \
      org.opencontainers.image.description="$DESCRIPTION" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /etc/ssl/certs/ca-certificates.crt \
    /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tturl /tturl
COPY LICENSE /LICENSE
COPY THIRD_PARTY_LICENSES /THIRD_PARTY_LICENSES

USER 65532:65532
ENTRYPOINT ["/tturl"]
