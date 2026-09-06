# Deploying and operating ogit

This guide covers running the service, controlling access, choosing storage,
initializing repositories and operating regional replicas. For exporter settings
and alert guidance, see [OpenTelemetry](telemetry.md).

## Configure the server

ogit reads YAML configuration, command-line flags and environment variables.
Use `ogit server --config /config/config.yaml` to start it, or `ogit server --help`
to see the available flags. A local build produces `tmp/git-server-s3`; container
images use `/app` as the entrypoint.

A minimal local-storage configuration is:

```yaml
http:
  port: 8080
  logs: true
ssh:
  enabled: false
storage:
  type: local
  local:
    path: /data/repositories
```

Mount writable storage at the configured path for local repositories. For S3,
replace the storage section with:

```yaml
storage:
  type: s3
  s3:
    bucket: git-source
    region: us-east-1
```

The container runs as UID/GID 65532. It needs read access to its configuration
and seed files, plus write access to the local repository directory when using
local storage. S3-backed repositories keep their durable state in the bucket.
Set `RELEASE_TAG` to an available release tag, then use the published image
with a mounted configuration:

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  "ghcr.io/insly/insly-ogit:${RELEASE_TAG}"
```

Provide the required AWS identity and access tokens separately as described below.

## Access controls

Provision `AUTH_READ_TOKEN`, `AUTH_WRITE_TOKEN`, and `AUTH_ADMIN_TOKEN` through
your secret manager. Tokens must be distinct; omit unused roles to disable
them. Basic auth accepts the token as password (username can be `git`), and
Bearer auth is also supported. Readers can advertise/fetch, including POST
upload-pack. Writers can additionally push; only administrators can create or
list repositories and use debug endpoints. With no tokens configured, local
compatibility mode permits anonymous access: always provision tokens in AWS.
Use TLS at the private ingress; credentials are not safe over public HTTP.

`READ_ONLY=true` denies client mutations even to administrators. SSH must be
disabled when authentication or read-only mode is enabled; startup rejects a
bypass configuration. SSH does not yet implement these authorization controls.

## S3 credentials and bucket permissions

Omit `storage.s3.access-key`, `secret-key`, and `session-token` in AWS. The SDK
uses its default credential chain, including refreshable workload role
credentials. Region may come from the normal AWS configuration. Custom
endpoints and complete explicit credentials remain available for Floci/local
use; partial explicit credentials fail startup.

Use private, versioned, encrypted buckets with public access blocked. Give the writing
ogit service access only to its repository prefixes. A replica controller needs read
access to the source and read/write access to its destination; normal regional
Git clients are denied writes by ogit. For strict IAM separation, run ogit's
replication-enabled processes as the regional synchronization identity and a
separate read-only ogit pool with read-only destination IAM, using the same
local bucket. The integrated controller and HTTP server in one process share
the destination SDK credentials; HTTP authorization does not create a second
IAM identity.

## Initialize an S3 repository

Place the initial repository files in a seed directory, preserving
their relative paths. The directory must contain regular files,
without symlinks or `.git`, totaling at most 16 MiB.

```sh
ogit bootstrap --config /config/config.yaml \
  --repository example --branch main --seed-directory /seed
```

Bootstrap validates reads and resumes interrupted initialization. It preserves
an existing valid branch and returns its commit hash, even if seeds changed.
A storage permission/availability error never counts as an absent repository.
Run bootstrap only against the writer. Replica repositories are initialized
by replication. ogit validates Git integrity, not the schema of the files stored in the repository.

## Regional read replicas

Choose one source repository as the writer and a separate S3 bucket for each
read replica. A replica polls the source branch, copies missing Git objects,
validates the complete graph, and conditionally publishes its local branch.
Git clients read from the local endpoint; they do not contact the writer.

```yaml
http:
  port: 8080
ssh:
  enabled: false
read-only: true
storage:
  type: s3
  s3:
    bucket: git-replica
    region: us-west-2
replication:
  source-bucket: git-source
  source-region: us-east-1
  source-repository: example
  repository: example
  branch: main
  interval: 5s
  timeout: 1m
```

Supply `AUTH_READ_TOKEN` separately. The source ogit omits `replication`
and sets `read-only: false`, with the appropriate reader/writer/admin tokens.
Additional replicas use their own destination buckets and the same source.
Every setting above also has a CLI flag; replication environment variables use
`REPLICATION_SOURCE_BUCKET`, `REPLICATION_SOURCE_REGION`,
`REPLICATION_SOURCE_ENDPOINT`, `REPLICATION_SOURCE_REPOSITORY`,
`REPLICATION_REPOSITORY`, `REPLICATION_BRANCH`, `REPLICATION_INTERVAL`, and
`REPLICATION_TIMEOUT`. `REPLICATION_SOURCE_ENDPOINT` is for emulator testing;
leave it unset in AWS.

## Publication and replication guarantees

- The S3 layout remains `repositories/<name>.git/{config,HEAD,refs,objects}`.
- Branch creation uses `If-None-Match: *`; updates compare the client's old Git
  hash and use the ETag from the same S3 GET with `If-Match`.
- Only one branch may be pushed per request. Branch deletion, tag pushes,
  non-fast-forward pushes and atomic multi-ref pushes are rejected. Use revert
  commits for rollbacks. This intentionally narrows the older generic Git API
  to a single-branch publication contract.
- Objects are immutable and hash-checked. Object and reference writes must
  finish before success is returned. A conflicting ref write fails visibly;
  an ambiguous transport failure is reconciled against the durable ref.
- `SetReference` is initialization-only in the S3 backend. Internal callers
  updating refs must use `CheckAndSetReference` with the expected old ref.
- Replication is asynchronous, forward-only and restartable. Concurrent
  controllers can stage the same objects; S3 conditionals choose the publisher.
  A missing/corrupt object, timeout or upstream outage leaves the old head.
- No cross-region or multi-ref transaction is claimed. Do not configure S3 CRR
  to write these serving refs. Do not expire/delete reachable Git objects.
- Validation currently walks complete history, bounded at 100,000 objects.
  Measure reconciliation cost and propagation delay as history grows. Polling
  defaults to 5 seconds, with a 1-minute attempt timeout; these are settings,
  not an end-to-end freshness guarantee.

## Readiness and monitoring

`/health` is process liveness. `/ready` on a replication-enabled process checks
the complete local published graph, without contacting the source. Before initial
replication it returns 503; a source outage does not invalidate a complete
regional copy. On an ordinary writer/read-only serving process without a
controller, `/ready` reports process readiness; bootstrap/flag readiness must
also be checked by the consuming workload.

Authenticated `GET /replication/status` returns source/applied revisions, last
attempt/success, first-observed pending time and the last error. Emit/collect
these alongside the structured reconciliation logs, or use the standard
[OpenTelemetry traces and metrics](telemetry.md) for monitoring. Pending time is the
controller's observation time, not the source commit time; a restart resets local
status. Alert on errors, last-success age and differing revisions; measure
end-to-end rollout freshness through consuming applications too.

## Verification and releases

Go is pinned to 1.27.1 in `go.mod`, `.tool-versions`, and Docker. The runtime
uses Debian `trixie-slim`; the Go builder, Debian runtime and Floci test images
are pulled from public ECR. PRs, main pushes, manual checks and release tags
use the shared GitHub test workflow on native AMD64 and ARM64 runners, including
golangci-lint v2.13.2 over application and integration-test code.
Release publication depends on both architectures passing that workflow and
pushes the saved, tested images rather than rebuilding them. Each release tag
selects the appropriate `linux/amd64` or `linux/arm64` image automatically;
architecture-specific tags append `-amd64` or `-arm64` to the release tag.
PR jobs have read-only repository permissions.

```sh
make lint
go test -race -count=1 -timeout=5m ./...
docker run --rm -p 4566:4566 public.ecr.aws/floci/floci:2.0.1
# In another terminal:
FLOCI_ENDPOINT=http://127.0.0.1:4566 make test-integration
```

CI provisions the same Floci image as an S3 service. Integration tests create
and clean up uniquely named buckets. They test concurrent conditional writes
against Floci and launch real ogit processes for seed, native shallow clone,
Git push, regional replication, bootstrap retry and regional reads. The exact
container binary repeats that process test against Floci before publication.
The in-process HTTP S3 fixture additionally injects missing/corrupt objects,
interrupted copies and lost publication responses. It is a contract fixture,
not a substitute for Floci or AWS acceptance.

Before production, still verify actual AWS role credential rotation and IAM denial,
S3 conditional-write behavior, client compatibility, multi-AZ failure,
regional network partitions, cold reader startup, and backup restoration.
Local/Floci tests do not establish those deployment runtime outcomes.
