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
  audit-interval: 1h
```

Supply `AUTH_READ_TOKEN` separately. The source ogit omits `replication`
and sets `read-only: false`, with the appropriate reader/writer/admin tokens.
Additional replicas use their own destination buckets and the same source.
Every setting above also has a CLI flag; replication environment variables use
`REPLICATION_SOURCE_BUCKET`, `REPLICATION_SOURCE_REGION`,
`REPLICATION_SOURCE_ENDPOINT`, `REPLICATION_SOURCE_REPOSITORY`,
`REPLICATION_REPOSITORY`, `REPLICATION_BRANCH`, `REPLICATION_INTERVAL`, and
`REPLICATION_TIMEOUT`, and `REPLICATION_AUDIT_INTERVAL`. `REPLICATION_SOURCE_ENDPOINT` is for emulator testing;
leave it unset in AWS.

### S3 notifications through SQS

Choose the trigger through `replication.mode` in YAML, `--replication.mode`
on the CLI, or `REPLICATION_MODE` in the environment:

| Mode | Trigger | Required setup |
| --- | --- | --- |
| `poll` (default) | Check both branch refs every `replication.interval` | Source and destination S3 access |
| `sqs` | S3 notifications, with startup and periodic fallback checks | S3 access plus a queue and bucket notification configuration |

For example, use `--replication.mode poll --replication.interval 5s`, or
`--replication.mode sqs --replication.queue-url <queue-url>` with the same
source/destination settings. SQS mode replaces the frequent polling loop;
`fallback-interval` controls its separate recovery checks. Both modes use
`audit-interval` for full validation of unchanged revisions.

To trigger replication on branch updates, add these settings to the replica's
existing `replication` block:

```yaml
replication:
  source-bucket: git-source
  source-region: eu-west-1
  source-repository: flags
  repository: flags
  branch: main
  mode: sqs
  queue-url: https://sqs.eu-west-1.amazonaws.com/123456789012/flags-us-replication
  fallback-interval: 5m
  queue-wait-time: 20s
  queue-empty-delay: 100ms
  timeout: 1m
  audit-interval: 1h
```

`mode` defaults to `poll`. SQS mode reconciles immediately on startup, on
matching notifications, and every `fallback-interval` (default 5 minutes).
`interval` applies only to polling mode. Equivalent environment variables are
`REPLICATION_MODE`, `REPLICATION_QUEUE_URL`, and `REPLICATION_FALLBACK_INTERVAL`.
For emulator testing, `replication.queue-endpoint` /
`REPLICATION_QUEUE_ENDPOINT` overrides the SQS endpoint independently of S3.

Provision the queue and notification configuration separately; ogit does not
modify AWS infrastructure:

1. Create a **standard SQS queue in the source bucket's region**, with a dead
   letter queue and a redrive policy. Allow several processing attempts before
   redrive, and set retention to cover the expected outage window.
2. Configure the source bucket's `s3:ObjectCreated:*` notifications with prefix
   `repositories/flags.git/refs/heads/`. The consumer checks the exact source
   bucket and URL-decoded `repositories/flags.git/refs/heads/main` key. Object
   writes do not trigger replication. The source repository and branch settings
   determine this key; the destination repository may have a different name.
3. Grant `s3.amazonaws.com` `sqs:SendMessage` on this queue, restricted by
   `aws:SourceArn` to the source bucket and `aws:SourceAccount` to its account.
   Give the replica identity `sqs:ReceiveMessage` and `sqs:DeleteMessage` on the
   queue, alongside its existing source-read/destination-write S3 permissions.
   For a customer-managed KMS key, also grant the S3 publisher
   `kms:GenerateDataKey`/`kms:Decrypt` and consumers `kms:Decrypt`, with matching
   key policy permissions.
4. Use a dedicated queue per source branch **and destination bucket**. Processes
   replicating to the same destination may compete on one queue. For multiple
   destination buckets, fan out through a source-region SNS topic to separate
   queues with **raw message delivery enabled**; wrapped SNS envelopes are not
   supported. Do not let different destinations compete for the same messages.

The SQS client uses the source region and the same refreshable credential
provider as the source S3 client. The replica can run in a different region.
SQS receives wait up to `queue-wait-time` (default 20 seconds) and request up
to 10 messages. One matching
batch triggers one reconciliation of the **current** branch ref; event order
never selects a Git revision. Matching receipts are deleted only after success.
Failed transfers and failed receipt deletions are retried through SQS redelivery.
Malformed messages remain for redrive; valid unrelated notifications and S3 test
events are acknowledged without S3 reads. Keep this queue exclusive to ogit.

AWS [limits each long poll to 20 seconds](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/best-practices-setting-up-long-polling.html).
`queue-wait-time` accepts whole seconds from `1s` to `20s`; lowering it increases
idle requests. To reduce requests further, increase `queue-empty-delay`, the
cancellable pause after an empty receive. It defaults to `100ms` and accepts any
positive duration. Zero uses the default for either setting; negative durations
and fractional or above-limit waits fail startup. Both settings apply only in
SQS mode.

For example, `queue-wait-time: 20s` with `queue-empty-delay: 40s` makes roughly
one idle receive per minute (1,440/day per listener), versus about 4,300/day with
the defaults. This excludes API retries, active traffic and acknowledgments.
Messages arriving during the pause wait up to 40 seconds for the next receive;
messages arriving during a long poll can return immediately. Nonempty batches
are processed and followed by another receive without this pause. Startup and
fallback reconciliation continue independently, including during long pauses.

Set these through YAML as above, CLI flags `--replication.queue-wait-time 20s`
and `--replication.queue-empty-delay 40s`, or environment variables
`REPLICATION_QUEUE_WAIT_TIME` and `REPLICATION_QUEUE_EMPTY_DELAY`.

The listener sets each receive's visibility timeout to `2 × timeout + 60s`
(rounded up to seconds). This covers a fallback attempt already in progress,
the batch's replication attempt, and acknowledgment margin. With the default
1-minute timeout, visibility is 3 minutes. The maximum replication timeout in
SQS mode is 5h59m30s to stay within SQS's 12-hour visibility limit. Only one batch
is in flight per process. Receive failures back off from 1 to 30 seconds while
fallback reconciliation continues independently.

S3 notifications can be duplicated, delayed, or delivered out of order. The
unchanged-revision optimization makes duplicates cheap; fallback reconciliation
covers absent notifications and queue outages. Audits occur on the next event
or fallback after the audit interval expires. Monitor notification processing
warnings, queue age, and the dead letter queue as well as replication status.
`last_success` is the last actual reconciliation, so allow for the fallback
interval when alerting on it. SQS requests are billable; this mode removes idle
S3 polling between fallback checks, not the cost of transferring changed data.

AWS references: [S3 destinations and delivery semantics](https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-how-to-event-types-and-destinations.html),
[key filtering](https://docs.aws.amazon.com/AmazonS3/latest/userguide/notification-how-to-filtering.html),
and [SQS receive behavior](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/APIReference/API_ReceiveMessage.html).

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
- Startup, changed revisions and scheduled audits validate complete history,
  bounded at 100,000 objects. After successful validation, unchanged polls read
  only the source and destination branch refs (two GETs, no object or metadata
  requests). A missing or changed destination ref triggers full reconciliation.
  Failed attempts invalidate the process-local validation checkpoint.
- Polling defaults to 5 seconds, with a 1-minute attempt timeout and a 1-hour
  full audit interval; zero audit interval also means 1 hour. Idle corruption
  detection and missing-object repair are deferred until the next audit or
  restart. Ref checks do not refresh the audit timestamp. Audits run on the next
  reconciliation after they become due. Measure full-walk cost as history grows;
  these intervals are not an end-to-end freshness guarantee.

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

CI provisions the same Floci image for S3 and SQS. Integration tests create
and clean up uniquely named buckets. They test concurrent conditional writes
against Floci and launch real ogit processes for seed, native shallow clone,
Git push, regional replication, bootstrap retry and regional reads. The exact
container binary repeats that process test against Floci before publication.
The process test runs both explicit `poll` and `sqs` modes. The SQS case creates
a real Floci queue and bucket notification rule, waits for initial replication,
then pushes through native Git. Its polling and fallback intervals are one hour,
while convergence must finish within ten seconds, so only an S3 notification can
drive the update. It checks queue acknowledgment and the regional clone's file
contents. No test-generated `SendMessage` substitutes for S3 event delivery.
The in-process HTTP S3 fixture additionally injects missing/corrupt objects,
interrupted copies and lost publication responses. It is a contract fixture,
not a substitute for Floci or AWS acceptance.

Before production, still verify actual AWS role credential rotation and IAM denial,
S3 conditional-write behavior, client compatibility, multi-AZ failure,
regional network partitions, cold reader startup, and backup restoration.
Local/Floci tests do not establish those deployment runtime outcomes.
