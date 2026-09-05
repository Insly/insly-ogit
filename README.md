# oGit

ogit serves Git repositories over HTTP using local files or S3-compatible object
storage. It supports authenticated readers and writers, safe single-branch
publication, and regional S3 replicas that serve reads from their own buckets.

## What it supports

- Git Smart HTTP clone, fetch and push, including shallow clones and later fetches.
- Local filesystem and S3-compatible storage backends.
- Reader, writer and administrator tokens, plus a read-only serving mode.
- Conditional S3 reference writes and forward-only regional replication.
- Idempotent S3 bootstrap that preserves existing repository contents.
- Health/readiness endpoints, structured logs, OpenTelemetry traces and metrics.
- A Debian container image tested with native Git and Floci before release.

Pushes update one branch at a time. Branch deletion, tags, non-fast-forward
updates and atomic multi-reference pushes are rejected. SSH remains a demo
transport; it must be disabled when HTTP authentication or read-only mode is
configured.

## Try it locally

Build with Go 1.27.1, then start a local-storage server:

```sh
make build
STORAGE_TYPE=local STORAGE_LOCAL_PATH=./repositories \
  ./tmp/git-server-s3 server --config /dev/null
```

In another terminal, create and clone a repository:

```sh
curl --fail -H 'Content-Type: application/json' \
  -d '{"name":"example"}' http://localhost:8080/api/repo
git clone --depth=1 http://localhost:8080/example.git
```

This example uses anonymous local mode. Configure tokens and TLS before exposing
the service to other users. See the deployment guide for S3 credentials and
bootstrap, or use `./tmp/git-server-s3 server --help` for available flags.

## Configuration and operations

[Deploying and operating ogit](docs/deployment.md) covers server configuration,
access controls, storage guarantees, repository bootstrap, regional replication,
readiness and release verification.

[OpenTelemetry](docs/telemetry.md) explains exporter setup, the metrics to monitor,
trace/log correlation, and the limits of replica freshness measurements.

## Development

```sh
make build          # Build tmp/git-server-s3
make test           # Race-enabled tests, including native Git process tests
make test-coverage  # Write coverage.out and coverage.html
make lint           # golangci-lint v2.13.2, including integration-tagged code
```

Install the pinned linter from the
[official releases](https://github.com/golangci/golangci-lint/releases/tag/v2.13.2).
To run integration tests against Floci:

```sh
docker run --rm -p 4566:4566 public.ecr.aws/floci/floci:2.0.1
# In another terminal:
FLOCI_ENDPOINT=http://127.0.0.1:4566 make test-integration
```

The shared CI workflow checks modules, formatting, lint, tests and the built
container. Release publication uses the image that passed those checks.

## License

See [LICENSE](LICENSE).
