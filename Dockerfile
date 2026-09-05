FROM golang:1.27.1 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=development
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/app ./cmd

FROM alpine:3.23 AS release
RUN apk add --no-cache ca-certificates git
COPY --from=builder /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
CMD ["server", "-c", "/config/config.yaml"]
