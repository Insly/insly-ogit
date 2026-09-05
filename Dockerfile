FROM public.ecr.aws/docker/library/golang:1.27.1 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=development
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/app ./cmd

FROM public.ecr.aws/docker/library/debian:trixie-slim AS release
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
CMD ["server", "-c", "/config/config.yaml"]
