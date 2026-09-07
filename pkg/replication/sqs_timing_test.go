package replication

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

type timedReceive struct {
	at   time.Time
	wait int32
}

type timingQueue struct {
	mu           sync.Mutex
	calls        []timedReceive
	acknowledged atomic.Bool
}

func (q *timingQueue) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	q.mu.Lock()
	q.calls = append(q.calls, timedReceive{time.Now(), in.WaitTimeSeconds})
	count := len(q.calls)
	q.mu.Unlock()
	switch count {
	case 1:
		select {
		case <-time.After(time.Duration(in.WaitTimeSeconds) * time.Second):
			return &sqs.ReceiveMessageOutput{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case 2:
		return &sqs.ReceiveMessageOutput{Messages: []types.Message{{
			Body:          aws.String(`{"Service":"Amazon S3","Event":"s3:TestEvent","Bucket":"eu"}`),
			ReceiptHandle: aws.String("receipt"),
		}}}, nil
	default:
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

func (q *timingQueue) DeleteMessageBatch(context.Context, *sqs.DeleteMessageBatchInput, ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	q.acknowledged.Store(true)
	return &sqs.DeleteMessageBatchOutput{}, nil
}

func (q *timingQueue) received() []timedReceive {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]timedReceive(nil), q.calls...)
}

func timingListener(q *timingQueue) *SQSListener {
	return &SQSListener{
		Mirror: &Mirror{Source: memory.NewStorage(), Destination: memory.NewStorage(), Branch: branch},
		Client: q, QueueURL: "https://sqs.test/queue", SourceBucket: "eu", SourceKey: "repo/refs/heads/main",
		FallbackInterval: 10 * time.Second, Timeout: time.Minute,
	}
}

func TestSQSReceiveTiming(t *testing.T) {
	for _, tc := range []struct {
		name               string
		wait, delay, cycle time.Duration
		requestWait        int32
	}{
		{"defaults", 0, 0, 20*time.Second + 100*time.Millisecond, 20},
		{"lower idle cost", 20 * time.Second, 40 * time.Second, time.Minute, 20},
		{"custom wait", 7 * time.Second, time.Minute, 67 * time.Second, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := &timingQueue{}
				l := timingListener(q)
				l.QueueWaitTime, l.QueueEmptyDelay = tc.wait, tc.delay
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				var reconciliations atomic.Int64
				go func() { done <- l.Run(ctx, func(Status) { reconciliations.Add(1) }) }()
				synctest.Wait()
				time.Sleep(tc.cycle - time.Nanosecond)
				synctest.Wait()
				require.Len(t, q.received(), 1, "no receive before the configured idle pause expires")
				require.Equal(t, tc.requestWait, q.received()[0].wait)
				require.Greater(t, reconciliations.Load(), int64(1), "fallback continues while the receiver waits")
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				require.Len(t, q.received(), 3, "nonempty batch is acknowledged and immediately followed by another receive")
				require.True(t, q.acknowledged.Load())
				require.Equal(t, tc.cycle, q.received()[1].at.Sub(q.received()[0].at))
				require.Equal(t, q.received()[1].at, q.received()[2].at)
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
			})
		})
	}
}

func TestSQSCancelDuringEmptyDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := &timingQueue{}
		l := timingListener(q)
		l.QueueEmptyDelay = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- l.Run(ctx, nil) }()
		time.Sleep(21 * time.Second)
		synctest.Wait()
		require.Len(t, q.received(), 1)
		start := time.Now()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.Equal(t, time.Duration(0), time.Since(start))
	})
}

func TestSQSRejectsInvalidReceiveTimingBeforeIO(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wait, delay time.Duration
	}{
		{"negative wait", -time.Second, 0},
		{"too long", 21 * time.Second, 0},
		{"fractional wait", 1500 * time.Millisecond, 0},
		{"subsecond wait", time.Millisecond, 0},
		{"negative delay", 0, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := &timingQueue{}
				l := timingListener(q)
				l.QueueWaitTime, l.QueueEmptyDelay = tc.wait, tc.delay
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				var reconciliations atomic.Int64
				err := l.Run(ctx, func(Status) { reconciliations.Add(1) })
				require.ErrorContains(t, err, "queue")
				require.Empty(t, q.received())
				require.Zero(t, reconciliations.Load())
			})
		})
	}
}
