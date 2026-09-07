//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsarn "github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/stretchr/testify/require"
)

func TestFlociProcessIntegration(t *testing.T) {
	endpoint := os.Getenv("FLOCI_ENDPOINT")
	require.NotEmpty(t, endpoint)
	t.Run("poll", func(t *testing.T) {
		runHAProcessIntegration(t, endpoint, func(string) replicationProcessOptions {
			return replicationProcessOptions{settings: "  mode: poll\n  interval: 100ms\n"}
		})
	})
	t.Run("sqs", func(t *testing.T) {
		runHAProcessIntegration(t, endpoint, func(bucket string) replicationProcessOptions {
			return flociNotificationReplication(t, endpoint, bucket)
		})
	})
}

func flociNotificationReplication(t *testing.T, endpoint, bucket string) replicationProcessOptions {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	client := sqs.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})
	queue, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(bucket + "-replication")})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := client.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: queue.QueueUrl})
		require.NoError(t, err)
	})
	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       queue.QueueUrl,
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	require.NoError(t, err)
	arn := attrs.Attributes["QueueArn"]
	require.NotEmpty(t, arn)
	queueARN, err := awsarn.Parse(arn)
	require.NoError(t, err)
	policy, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect":    "Allow",
			"Principal": map[string]string{"Service": "s3.amazonaws.com"},
			"Action":    "sqs:SendMessage",
			"Resource":  arn,
			"Condition": map[string]any{
				"ArnEquals":    map[string]string{"aws:SourceArn": "arn:aws:s3:::" + bucket},
				"StringEquals": map[string]string{"aws:SourceAccount": queueARN.AccountID},
			},
		}},
	})
	require.NoError(t, err)
	_, err = client.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
		QueueUrl:   queue.QueueUrl,
		Attributes: map[string]string{"Policy": string(policy)},
	})
	require.NoError(t, err)
	source, err := s3store.NewClient(ctx, "us-east-1", endpoint, "test", "test", "")
	require.NoError(t, err)
	return replicationProcessOptions{
		// Both timers exceed the ten-second convergence assertion. Only a real
		// S3-generated notification can trigger replication after startup.
		settings: fmt.Sprintf("  mode: sqs\n  interval: 1h\n  fallback-interval: 1h\n  queue-url: %s\n  queue-endpoint: %s\n", aws.ToString(queue.QueueUrl), endpoint),
		beforePush: func() {
			_, err := source.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
				Bucket: aws.String(bucket),
				NotificationConfiguration: &s3types.NotificationConfiguration{
					QueueConfigurations: []s3types.QueueConfiguration{
						{
							Id:       aws.String("branch-updates"),
							QueueArn: aws.String(arn),
							Events:   []s3types.Event{s3types.EventS3ObjectCreated},
							Filter: &s3types.NotificationConfigurationFilter{
								Key: &s3types.S3KeyFilter{
									FilterRules: []s3types.FilterRule{
										{Name: s3types.FilterRuleNamePrefix, Value: aws.String("repositories/flags.git/refs/heads/")},
									},
								},
							},
						},
					},
				},
			})
			require.NoError(t, err)
		},
		afterReplication: func() {
			require.Eventually(t, func() bool {
				attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
					QueueUrl: queue.QueueUrl,
					AttributeNames: []sqstypes.QueueAttributeName{
						sqstypes.QueueAttributeNameApproximateNumberOfMessages,
						sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
					},
				})
				return err == nil && attrs.Attributes["ApproximateNumberOfMessages"] == "0" && attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
			}, 10*time.Second, 100*time.Millisecond, "replicated notifications must be acknowledged")
		},
	}
}
