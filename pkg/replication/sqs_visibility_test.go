package replication

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"
)

type slowMissingSource struct {
	*memory.Storage
	calls atomic.Int64
}

func (s *slowMissingSource) Reference(n plumbing.ReferenceName) (*plumbing.Reference, error) {
	if s.calls.Add(1) > 1 {
		time.Sleep(59 * time.Second)
	}
	return nil, plumbing.ErrReferenceNotFound
}

type visibilityQueue struct {
	received time.Time
	lease    time.Duration
	calls    int
	deleted  chan time.Duration
}

func (q *visibilityQueue) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	q.calls++
	if q.calls > 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	time.Sleep(2 * time.Second)
	q.received = time.Now()
	q.lease = time.Duration(in.VisibilityTimeout) * time.Second
	return &sqs.ReceiveMessageOutput{
		Messages: []types.Message{
			{
				Body:          aws.String(`{"Service":"Amazon S3","Event":"s3:TestEvent","Bucket":"eu"}`),
				ReceiptHandle: aws.String("r"),
			},
		},
	}, nil
}

func (q *visibilityQueue) DeleteMessageBatch(context.Context, *sqs.DeleteMessageBatchInput, ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	q.deleted <- time.Since(q.received)
	return &sqs.DeleteMessageBatchOutput{}, nil
}

// Exercise select scheduling repeatedly under virtual time. A pending batch
// must not wait behind multiple slow fallbacks and outlive its SQS visibility.
func TestSQSReceivedBatchStaysWithinVisibility(t *testing.T) {
	for i := 0; i < 100; i++ {
		synctest.Test(t, func(t *testing.T) {
			q := &visibilityQueue{deleted: make(chan time.Duration, 1)}
			l := &SQSListener{
				Mirror: &Mirror{
					Source:      &slowMissingSource{Storage: memory.NewStorage()},
					Destination: memory.NewStorage(),
					Branch:      branch,
				},
				Client:           q,
				QueueURL:         "https://sqs.test/queue",
				SourceBucket:     "eu",
				SourceKey:        "repo/refs/heads/main",
				FallbackInterval: time.Second,
				Timeout:          time.Minute,
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- l.Run(ctx, nil) }()
			elapsed := <-q.deleted
			cancel()
			<-done
			if elapsed > q.lease {
				t.Fatalf("batch acknowledgment waited %s, exceeding actual requested visibility %s; source reference attempts=%d", elapsed, q.lease, l.Mirror.Source.(*slowMissingSource).calls.Load())
			}
		})
	}
}
