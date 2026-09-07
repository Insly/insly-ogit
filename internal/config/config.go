package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

var (
	// Version is set during the startup process.
	Version string

	// ConfigFile is the path to the configuration file.
	// This is used to load the configuration at startup.
	ConfigFile string

	// Server is the configuration for the HTTP fiber server.
	// Port is the port on which the server listens.
	// HttpLogs enables or disables HTTP request logging.
	Server struct {
		Port     int
		HttpLogs bool
	}

	// SSH is the configuration for the SSH Git server.
	// Enabled controls whether the SSH server starts.
	// Port is the port on which the SSH server listens.
	// HostKeyPath is the path to the SSH host key file.
	SSH struct {
		Enabled     bool
		Port        int
		HostKeyPath string
	}

	// Debug enables or disables debug endpoints.
	Debug struct {
		Endpoints bool
	}

	// Logger is the configuration for the zerolog logger.
	// Level is the log level for the logger.
	// Pretty enables or disables pretty printing of logs (non JSON logs).
	Logger struct {
		Level  string
		Pretty bool
	}

	// StorageType is the type of storage to use (e.g., local, s3).
	Storage struct {
		Type string

		S3 struct {
			Bucket       string
			Endpoint     string
			AccessKey    string
			SecretKey    string
			SessionToken string
			Region       string
		}

		Local struct {
			Path string
		}
	}
)

type AccessConfig struct {
	ReadToken, WriteToken, AdminToken string
	ReadOnly                          bool
}

var Access AccessConfig

type ReplicationConfig struct {
	SourceBucket, SourceRegion, SourceEndpoint, SourceRepository, Repository, Branch string
	Interval, Timeout, AuditInterval                                                 time.Duration
	Mode, QueueURL, QueueEndpoint                                                    string
	FallbackInterval                                                                 time.Duration
	QueueWaitTime, QueueEmptyDelay                                                   time.Duration
}

var Replication ReplicationConfig

func ValidateServer() error {
	protected := Access.ReadToken != "" || Access.WriteToken != "" || Access.AdminToken != "" || Access.ReadOnly
	if protected && SSH.Enabled {
		return fmt.Errorf("disable SSH when HTTP authorization or read-only mode is enabled")
	}
	seen := map[string]bool{}
	for _, v := range []string{Access.ReadToken, Access.WriteToken, Access.AdminToken} {
		if v != "" {
			if seen[v] {
				return fmt.Errorf("access role tokens must be distinct")
			}
			seen[v] = true
		}
	}
	r := Replication
	if r.Mode != "" && r.Mode != "poll" && r.Mode != "sqs" {
		return fmt.Errorf("replication mode must be poll or sqs")
	}
	if r.Mode == "sqs" {
		if err := ValidateReplicationQueueTiming(r.QueueWaitTime, r.QueueEmptyDelay); err != nil {
			return err
		}
		queue, err := url.Parse(r.QueueURL)
		if err != nil || queue.Host == "" || (queue.Scheme != "https" && queue.Scheme != "http") {
			return fmt.Errorf("SQS replication requires an HTTP(S) queue URL")
		}
		// A received batch may wait behind one fallback attempt before its own.
		if r.SourceBucket == "" || r.FallbackInterval <= 0 || r.Timeout > 6*time.Hour-30*time.Second {
			return fmt.Errorf("SQS replication requires a source bucket, positive fallback interval and timeout no greater than 5h59m30s")
		}
	} else if r.QueueURL != "" || r.QueueEndpoint != "" {
		return fmt.Errorf("replication queue settings require sqs mode")
	}
	if r.SourceBucket != "" {
		if r.AuditInterval < 0 {
			return fmt.Errorf("replication audit interval must not be negative")
		}
		if Storage.Type != "s3" || !Access.ReadOnly || SSH.Enabled {
			return fmt.Errorf("replication requires S3 storage, read-only HTTP and disabled SSH")
		}
		if r.SourceRegion == "" || r.Repository == "" || r.Branch == "" || (r.Mode != "sqs" && r.Interval <= 0) || r.Timeout <= 0 {
			return fmt.Errorf("replication source region, repository, branch and positive durations are required")
		}
		if strings.ContainsAny(r.Repository, "/\\") || r.Repository == "." || r.Repository == ".." {
			return fmt.Errorf("replication repository must be one repository name")
		}
		if err := plumbing.NewBranchReferenceName(r.Branch).Validate(); err != nil {
			return err
		}
		if r.SourceBucket == Storage.S3.Bucket && r.SourceEndpoint == Storage.S3.Endpoint && (r.SourceRepository == "" || r.SourceRepository == r.Repository) {
			return fmt.Errorf("source and destination repositories must differ")
		}
	}
	return nil
}

// ValidateReplicationQueueTiming checks SQS timing before startup can perform any I/O.
// Zero values preserve defaults for callers that do not configure queue timing.
func ValidateReplicationQueueTiming(wait, emptyDelay time.Duration) error {
	// AWS accepts only whole seconds, with a maximum long poll of 20 seconds.
	if wait < 0 || wait > 20*time.Second || wait%time.Second != 0 {
		return fmt.Errorf("replication queue-wait-time must be whole seconds from 1s to 20s (zero uses 20s)")
	}
	if emptyDelay < 0 {
		return fmt.Errorf("replication queue-empty-delay must not be negative (zero uses 100ms)")
	}
	return nil
}
