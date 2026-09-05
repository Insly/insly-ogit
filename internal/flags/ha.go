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
	}
}
