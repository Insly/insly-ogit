package config

import (
	"fmt"
	"github.com/go-git/go-git/v5/plumbing"
	"strings"
	"time"
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
	Interval, Timeout                                                                time.Duration
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
	if r.SourceBucket != "" {
		if Storage.Type != "s3" || !Access.ReadOnly || SSH.Enabled {
			return fmt.Errorf("replication requires S3 storage, read-only HTTP and disabled SSH")
		}
		if r.SourceRegion == "" || r.Repository == "" || r.Branch == "" || r.Interval <= 0 || r.Timeout <= 0 {
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
