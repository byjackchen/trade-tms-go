# Multi-stage build for the single `tms` binary.
# (BuildKit cache mounts are supported natively by Docker 23+ — no
#  `# syntax=` frontend pin needed, which also avoids a registry pull.)
#
#   docker build -t tms:dev \
#     --build-arg VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev) \
#     --build-arg COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) .

FROM golang:1.26 AS builder

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/byjackchen/trade-tms-go/internal/app.Version=${VERSION} \
      -X github.com/byjackchen/trade-tms-go/internal/app.Commit=${COMMIT} \
      -X github.com/byjackchen/trade-tms-go/internal/app.BuildDate=${BUILD_DATE}" \
    -o /out/tms ./cmd/tms

# Distroless static: no shell, no package manager, runs as nonroot.
# Migrations are embedded in the binary, so nothing else is needed.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/tms /usr/local/bin/tms

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/tms"]
CMD ["version"]
