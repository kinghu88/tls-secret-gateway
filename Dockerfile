FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.26.4-alpine AS builder

ENV GOPROXY=https://goproxy.cn,direct
ENV GO111MODULE=on

WORKDIR /src

# Cache modules first
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a statically linked binary for a minimal final image.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/tls-secret-gateway ./cmd/server

# ---- runtime stage ----
FROM swr.cn-north-4.myhuaweicloud.com/ddn-k8s/gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.title="tls-secret-gateway" \
      org.opencontainers.image.description="Secure gateway for reading kubernetes.io/tls Secrets"

COPY --from=builder /out/tls-secret-gateway /tls-secret-gateway

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/tls-secret-gateway"]
