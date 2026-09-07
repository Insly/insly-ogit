package flags

import (
	"github.com/labbs/git-server-s3/internal/config"
	altsrc "github.com/urfave/cli-altsrc/v3"
	yaml "github.com/urfave/cli-altsrc/v3/yaml"
	"github.com/urfave/cli/v3"
	"time"
)

func HAFlags() []cli.Flag {
	source := func(name, env string) cli.ValueSourceChain {
		return cli.NewValueSourceChain(cli.EnvVar(env), yaml.YAML(name, altsrc.NewStringPtrSourcer(&config.ConfigFile)))
	}
	return []cli.Flag{
		&cli.StringFlag{Name: "auth.read-token", Destination: &config.Access.ReadToken, Sources: source("auth.read-token", "AUTH_READ_TOKEN")},
		&cli.StringFlag{Name: "auth.write-token", Destination: &config.Access.WriteToken, Sources: source("auth.write-token", "AUTH_WRITE_TOKEN")},
		&cli.StringFlag{Name: "auth.admin-token", Destination: &config.Access.AdminToken, Sources: source("auth.admin-token", "AUTH_ADMIN_TOKEN")},
		&cli.BoolFlag{Name: "read-only", Destination: &config.Access.ReadOnly, Sources: source("read-only", "READ_ONLY")},
		&cli.StringFlag{Name: "replication.source-bucket", Destination: &config.Replication.SourceBucket, Sources: source("replication.source-bucket", "REPLICATION_SOURCE_BUCKET")},
		&cli.StringFlag{Name: "replication.source-region", Destination: &config.Replication.SourceRegion, Sources: source("replication.source-region", "REPLICATION_SOURCE_REGION")},
		&cli.StringFlag{Name: "replication.source-endpoint", Destination: &config.Replication.SourceEndpoint, Sources: source("replication.source-endpoint", "REPLICATION_SOURCE_ENDPOINT")},
		&cli.StringFlag{Name: "replication.source-repository", Destination: &config.Replication.SourceRepository, Sources: source("replication.source-repository", "REPLICATION_SOURCE_REPOSITORY")},
		&cli.StringFlag{Name: "replication.repository", Destination: &config.Replication.Repository, Sources: source("replication.repository", "REPLICATION_REPOSITORY")},
		&cli.StringFlag{Name: "replication.branch", Value: "main", Destination: &config.Replication.Branch, Sources: source("replication.branch", "REPLICATION_BRANCH")},
		&cli.DurationFlag{Name: "replication.interval", Value: 5 * time.Second, Destination: &config.Replication.Interval, Sources: source("replication.interval", "REPLICATION_INTERVAL")},
		&cli.DurationFlag{Name: "replication.timeout", Value: time.Minute, Destination: &config.Replication.Timeout, Sources: source("replication.timeout", "REPLICATION_TIMEOUT")},
		&cli.DurationFlag{
			Name:        "replication.audit-interval",
			Usage:       "Full graph audit interval for unchanged revisions (zero uses 1h)",
			Value:       time.Hour,
			Destination: &config.Replication.AuditInterval,
			Sources:     source("replication.audit-interval", "REPLICATION_AUDIT_INTERVAL"),
		},
		&cli.StringFlag{
			Name:        "replication.mode",
			Usage:       "Replication trigger: poll or sqs",
			Value:       "poll",
			Destination: &config.Replication.Mode,
			Sources:     source("replication.mode", "REPLICATION_MODE"),
		},
		&cli.StringFlag{
			Name:        "replication.queue-url",
			Usage:       "Dedicated source-region SQS queue URL",
			Destination: &config.Replication.QueueURL,
			Sources:     source("replication.queue-url", "REPLICATION_QUEUE_URL"),
		},
		&cli.StringFlag{
			Name:        "replication.queue-endpoint",
			Usage:       "SQS endpoint override for emulator testing",
			Destination: &config.Replication.QueueEndpoint,
			Sources:     source("replication.queue-endpoint", "REPLICATION_QUEUE_ENDPOINT"),
		},
		&cli.DurationFlag{
			Name:        "replication.queue-wait-time",
			Usage:       "SQS long-poll wait, whole seconds from 1s to 20s (zero uses 20s)",
			Value:       20 * time.Second,
			Destination: &config.Replication.QueueWaitTime,
			Sources:     source("replication.queue-wait-time", "REPLICATION_QUEUE_WAIT_TIME"),
		},
		&cli.DurationFlag{
			Name:        "replication.queue-empty-delay",
			Usage:       "Pause after an empty SQS receive; longer pauses reduce requests but delay notifications (zero uses 100ms)",
			Value:       100 * time.Millisecond,
			Destination: &config.Replication.QueueEmptyDelay,
			Sources:     source("replication.queue-empty-delay", "REPLICATION_QUEUE_EMPTY_DELAY"),
		},
		&cli.DurationFlag{
			Name:        "replication.fallback-interval",
			Usage:       "Reconciliation interval when notifications are absent or unavailable",
			Value:       5 * time.Minute,
			Destination: &config.Replication.FallbackInterval,
			Sources:     source("replication.fallback-interval", "REPLICATION_FALLBACK_INTERVAL"),
		},
	}
}
