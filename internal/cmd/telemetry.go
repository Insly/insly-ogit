package cmd

import (
	"context"
	"time"

	"github.com/labbs/git-server-s3/pkg/telemetry"
	"github.com/rs/zerolog"
)

func startTelemetry(ctx context.Context, version string, log zerolog.Logger) (func(), error) {
	runtime, err := telemetry.Setup(ctx, version)
	if err != nil {
		return nil, err
	}
	return func() {
		// The command context is canceled during shutdown. Export with a fresh bound.
		flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := runtime.Shutdown(flush); err != nil {
			log.Warn().Err(err).Msg("Telemetry shutdown incomplete")
		}
	}, nil
}
