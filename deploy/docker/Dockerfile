# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache mounts persist the module + build caches across builds, so go.sum
# changes only re-download the modules that actually changed (not all of them).
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" -o /out/dstream ./cmd/dstream

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 dstream
COPY --from=build /out/dstream /usr/local/bin/dstream
USER dstream
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dstream"]
CMD ["server"]
