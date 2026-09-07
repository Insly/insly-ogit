package cmd

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go/metrics/smithyotelmetrics"
	"github.com/aws/smithy-go/tracing/smithyoteltracing"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/pkg/common"
	"github.com/labbs/git-server-s3/pkg/replication"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
)

type replicationRunner func(context.Context, func(replication.Status)) error

func newReplicationRunner(mirror *replication.Mirror, sourceClient *s3.Client, rc config.ReplicationConfig, log zerolog.Logger) replicationRunner {
	if rc.Mode == "sqs" {
		// Reuse the source's refreshable credential provider and signing region.
		// Queue endpoints are independent of S3 emulator endpoints.
		source := sourceClient.Options()
		client := sqs.NewFromConfig(aws.Config{
			Region:      source.Region,
			Credentials: source.Credentials,
		}, func(o *sqs.Options) {
			o.TracerProvider = smithyoteltracing.Adapt(otel.GetTracerProvider())
			o.MeterProvider = smithyotelmetrics.Adapt(otel.GetMeterProvider())
			if rc.QueueEndpoint != "" {
				o.BaseEndpoint = aws.String(rc.QueueEndpoint)
			}
		})
		sourceRepo := rc.SourceRepository
		if sourceRepo == "" {
			sourceRepo = rc.Repository
		}
		listener := &replication.SQSListener{
			Mirror:           mirror,
			Client:           client,
			QueueURL:         rc.QueueURL,
			SourceBucket:     rc.SourceBucket,
			SourceKey:        "repositories/" + common.NormalizeRepoPath(sourceRepo) + "/" + mirror.Branch.String(),
			FallbackInterval: rc.FallbackInterval,
			Timeout:          rc.Timeout,
			QueueWaitTime:    rc.QueueWaitTime,
			QueueEmptyDelay:  rc.QueueEmptyDelay,
			OnError: func(err error) {
				log.Warn().Err(err).Msg("Replication notification processing failed")
			},
		}
		return listener.Run
	}
	return func(ctx context.Context, observe func(replication.Status)) error {
		return mirror.Run(ctx, rc.Interval, rc.Timeout, observe)
	}
}
