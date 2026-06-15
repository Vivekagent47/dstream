FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" -o /out/dstream ./cmd/dstream

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 dstream
COPY --from=build /out/dstream /usr/local/bin/dstream
USER dstream
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/dstream"]
CMD ["server"]
