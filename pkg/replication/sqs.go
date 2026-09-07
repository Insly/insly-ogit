package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/pkg/telemetry"
)

type SQSClient interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessageBatch(context.Context, *sqs.DeleteMessageBatchInput, ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
}

// SQSListener consumes direct S3 event notifications for one destination mirror.
// Configure a dedicated queue for each source branch and destination bucket.
// OnError reports queue/notification failures; observe reports reconciliation.
type SQSListener struct {
	Mirror                            *Mirror
	Client                            SQSClient
	QueueURL, SourceBucket, SourceKey string
	FallbackInterval, Timeout         time.Duration
	OnError                           func(error)

	// QueueWaitTime and QueueEmptyDelay default to 20s and 100ms when zero.
	QueueWaitTime, QueueEmptyDelay time.Duration
}

type queueResult struct {
	messages  []types.Message
	err       error
	processed chan struct{}
}

func (l *SQSListener) Run(ctx context.Context, observe func(Status)) error {
	if l.Mirror == nil || l.Client == nil || l.QueueURL == "" || l.SourceBucket == "" || l.SourceKey == "" {
		return fmt.Errorf("SQS listener requires mirror, client, queue URL, source bucket and key")
	}
	if l.FallbackInterval <= 0 || l.Timeout <= 0 || l.Timeout > 6*time.Hour-30*time.Second {
		return fmt.Errorf("SQS listener requires positive intervals and timeout no greater than 5h59m30s")
	}
	if err := config.ValidateReplicationQueueTiming(l.QueueWaitTime, l.QueueEmptyDelay); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	freshness := telemetry.WatchReplication()
	defer freshness.Close()
	reconcile := func() error {
		attempt, stop := context.WithTimeout(ctx, l.Timeout)
		defer stop()
		err := l.Mirror.Reconcile(attempt)
		status := l.Mirror.Status()
		freshness.Update(status.LastSuccess, status.PendingSince)
		if observe != nil {
			observe(status)
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_ = reconcile()
	ticker := time.NewTicker(l.FallbackInterval)
	defer ticker.Stop()
	results := make(chan queueResult)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.receive(ctx, results)
	}()
	defer func() {
		cancel()
		<-done
	}()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var result queueResult
		// A received batch's visibility is already running. Drain it before another
		// fallback, even when slow attempts keep the fallback ticker continuously ready.
		select {
		case result = <-results:
		default:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				_ = reconcile()
				continue
			case result = <-results:
			}
		}
		err := result.err
		if err == nil {
			err = l.process(ctx, result.messages, reconcile)
		}
		if err != nil && ctx.Err() == nil && l.OnError != nil {
			l.OnError(err)
		}
		close(result.processed)
	}
}

func (l *SQSListener) receive(ctx context.Context, results chan<- queueResult) {
	wait, emptyDelay := l.QueueWaitTime, l.QueueEmptyDelay
	if wait == 0 {
		wait = 20 * time.Second
	}
	if emptyDelay == 0 {
		emptyDelay = 100 * time.Millisecond
	}
	backoff := time.Second
	for ctx.Err() == nil {
		// Independent of the fallback timer: SQS outages must not stop reconciliation.
		attempt, cancel := context.WithTimeout(ctx, 25*time.Second)
		response, err := l.Client.ReceiveMessage(attempt, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(l.QueueURL),
			WaitTimeSeconds:     int32(wait / time.Second),
			MaxNumberOfMessages: 10,
			// Cover a fallback already in progress, this batch's attempt and API margin.
			VisibilityTimeout: int32((2*l.Timeout + time.Minute + time.Second - 1) / time.Second),
		})
		cancel()
		result := queueResult{err: err, processed: make(chan struct{})}
		if response != nil {
			result.messages = response.Messages
		}
		select {
		case results <- result:
		case <-ctx.Done():
			return
		}
		// No prefetch: visibility must not expire while batches wait in a local queue.
		select {
		case <-result.processed:
		case <-ctx.Done():
			return
		}
		if err != nil || len(result.messages) == 0 {
			delay := backoff
			if err == nil {
				delay = emptyDelay
				backoff = time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			if err != nil {
				backoff = min(backoff*2, 30*time.Second)
			}
		} else {
			backoff = time.Second
		}
	}
}

func (l *SQSListener) process(ctx context.Context, messages []types.Message, reconcile func() error) error {
	var independent, matching []types.DeleteMessageBatchRequestEntry
	var failures []error
	for i, msg := range messages {
		match, err := l.matches(aws.ToString(msg.Body))
		if err != nil || aws.ToString(msg.ReceiptHandle) == "" {
			failures = append(failures, fmt.Errorf("invalid S3 notification or missing receipt handle"))
			continue
		}
		entry := types.DeleteMessageBatchRequestEntry{Id: aws.String(strconv.Itoa(i)), ReceiptHandle: msg.ReceiptHandle}
		if match {
			matching = append(matching, entry)
		} else {
			independent = append(independent, entry)
		}
	}
	if len(matching) > 0 {
		// Notifications are hints: coalesce the batch and reconcile the current head.
		// Old/duplicate events must never select or roll back a Git revision.
		if err := reconcile(); err != nil {
			failures = append(failures, err)
		} else {
			independent = append(independent, matching...)
		}
	}
	if len(independent) > 0 && ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := l.Client.DeleteMessageBatch(attempt, &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(l.QueueURL), Entries: independent})
		if err != nil {
			failures = append(failures, fmt.Errorf("acknowledge SQS notifications: %w", err))
		} else if out == nil || len(out.Failed) > 0 {
			failures = append(failures, fmt.Errorf("SQS did not acknowledge every notification; failed receipts will be redelivered"))
		}
	}
	return errors.Join(failures...)
}

func (l *SQSListener) matches(body string) (bool, error) {
	var event struct {
		Service, Event, Bucket string
		Records                []struct {
			EventSource  string `json:"eventSource"`
			EventVersion string `json:"eventVersion"`
			EventName    string `json:"eventName"`
			S3           struct {
				Bucket struct {
					Name string `json:"name"`
				} `json:"bucket"`
				Object struct {
					Key string `json:"key"`
				} `json:"object"`
			} `json:"s3"`
		} `json:"Records"`
	}
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		return false, fmt.Errorf("invalid S3 notification JSON")
	}
	if event.Service == "Amazon S3" && event.Event == "s3:TestEvent" && event.Bucket != "" && len(event.Records) == 0 {
		return false, nil
	}
	if len(event.Records) == 0 {
		return false, fmt.Errorf("missing S3 event records")
	}
	match := false
	for _, record := range event.Records {
		if record.EventSource != "aws:s3" || !strings.HasPrefix(record.EventVersion, "2.") || record.EventName == "" || record.S3.Bucket.Name == "" || record.S3.Object.Key == "" {
			return false, fmt.Errorf("invalid S3 event record")
		}
		key, err := url.QueryUnescape(record.S3.Object.Key)
		if err != nil {
			return false, fmt.Errorf("invalid S3 object key encoding")
		}
		if record.S3.Bucket.Name == l.SourceBucket && key == l.SourceKey && strings.HasPrefix(record.EventName, "ObjectCreated:") {
			match = true
		}
	}
	return match, nil
}
