FROM golang:1.27.1-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg
ARG SERVICE=gateway
ARG VERSION=dev
ARG GIT_COMMIT=none
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/kh1011/creditproxy/pkg/version.Version=${VERSION} \
              -X github.com/kh1011/creditproxy/pkg/version.Commit=${GIT_COMMIT}" \
    -o /out/service ./cmd/${SERVICE}

FROM alpine:3.24
RUN adduser -D -u 10001 appuser
USER appuser
WORKDIR /app
COPY --from=builder /out/service /app/service
EXPOSE 8080
ENTRYPOINT ["/app/service"]
