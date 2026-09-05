package cmd

import (
	"context"
	"fmt"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/labbs/git-server-s3/pkg/common"
	"github.com/labbs/git-server-s3/pkg/replication"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/labbs/git-server-s3/internal/config"
	flags "github.com/labbs/git-server-s3/internal/flags"
	"github.com/labbs/git-server-s3/internal/server"
	"github.com/labbs/git-server-s3/pkg/logger"
	"github.com/labbs/git-server-s3/pkg/storage"

	"github.com/urfave/cli/v3"
)

// NewInstance creates a new 'server' command instance for urfave cli
func NewInstance(version string) *cli.Command {
	config.Version = version
	serverFlags := getFlags()

	return &cli.Command{
		Name:   "server",
		Usage:  "Serve Git repositories",
		Flags:  serverFlags,
		Action: runServer,
	}
}

// getFlags returns the list of flags for the server command.
func getFlags() (list []cli.Flag) {
	list = append(list, flags.GenericFlags()...)
	list = append(list, flags.ServerFlags()...)
	list = append(list, flags.LoggerFlags()...)
	list = append(list, flags.StorageFlags()...)
	list = append(list, flags.HAFlags()...)
	return
}

// runServer starts the server following the configuration.
func runServer(ctx context.Context, c *cli.Command) error {
	if err := config.ValidateServer(); err != nil {
		return err
	}
	l := logger.NewLogger(config.Logger.Level, config.Logger.Pretty, c.Root().Version)
	shutdown, err := startTelemetry(ctx, c.Root().Version, l)
	if err != nil {
		return err
	}
	defer shutdown()

	str, err := storage.NewGitRepositoryStorage(l)
	if err != nil {
		return err
	}

	// Configure the storage backend
	if err := str.Configure(); err != nil {
		l.Error().Err(err).Msg("Failed to configure storage")
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mirror *replication.Mirror
	if config.Replication.SourceBucket != "" {
		local, ok := str.(*s3store.S3Storage)
		if !ok {
			return fmt.Errorf("replication requires S3 storage")
		}
		rc := config.Replication
		sc := config.Storage.S3
		client, err := s3store.NewClient(ctx, rc.SourceRegion, rc.SourceEndpoint, sc.AccessKey, sc.SecretKey, sc.SessionToken)
		if err != nil {
			return err
		}
		sourceRepo := rc.SourceRepository
		if sourceRepo == "" {
			sourceRepo = rc.Repository
		}
		source := s3store.NewS3Storer(client, rc.SourceBucket, "repositories/"+common.NormalizeRepoPath(sourceRepo), l)
		mirror = &replication.Mirror{Source: source, Destination: local.StorerForRepository(rc.Repository), Branch: plumbing.NewBranchReferenceName(rc.Branch)}
	}
	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	// WaitGroup to wait for all servers to shutdown
	var wg sync.WaitGroup
	if mirror != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mirror.Run(ctx, config.Replication.Interval, config.Replication.Timeout, func(status replication.Status) {
				event := l.Info()
				if status.Error != "" {
					event = l.Warn()
				}
				event.Str("source_revision", status.SourceRevision).Str("applied_revision", status.AppliedRevision).Str("error", status.Error).Time("last_success", status.LastSuccess).Msg("Regional replication reconciled")
			})
		}()
	}

	// Configure HTTP server
	var httpConfig server.HttpConfig
	httpConfig.Port = config.Server.Port
	httpConfig.HttpLogs = config.Server.HttpLogs
	httpConfig.Logger = l
	httpConfig.Storage = str
	httpConfig.Mirror = mirror

	// Start HTTP server in a goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		l.Info().Int("port", config.Server.Port).Msg("Starting HTTP server")
		if err := httpConfig.NewServer(); err != nil {
			l.Error().Err(err).Msg("HTTP server failed")
		}
	}()

	var sshConfig *server.GitSSHConfig

	// Start SSH server if enabled
	if config.SSH.Enabled {
		sshConfig = &server.GitSSHConfig{
			Port:        config.SSH.Port,
			HostKeyPath: config.SSH.HostKeyPath,
			Logger:      l,
			Storage:     str,
		}

		if err := sshConfig.Configure(); err != nil {
			l.Error().Err(err).Msg("Failed to configure SSH server")
			return err
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Info().Int("port", config.SSH.Port).Msg("Starting Git SSH server")
			if err := sshConfig.NewServer(); err != nil {
				l.Error().Err(err).Msg("Git SSH server failed")
			}
		}()
	}

	// Wait for interrupt signal
	select {
	case <-sigChan:
	case <-ctx.Done():
	}
	cancel()
	l.Info().Msg("Shutdown signal received, stopping servers...")

	// Shutdown servers gracefully
	go func() {
		if err := httpConfig.Shutdown(); err != nil {
			l.Error().Err(err).Msg("Error shutting down HTTP server")
		}
		if sshConfig != nil {
			if err := sshConfig.Shutdown(); err != nil {
				l.Error().Err(err).Msg("Error shutting down SSH server")
			}
		}
	}()

	// Give servers time to shutdown gracefully
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		l.Info().Msg("All servers stopped gracefully")
	case <-time.After(30 * time.Second):
		l.Warn().Msg("Shutdown timeout reached, forcing exit")
	}

	return nil
}
