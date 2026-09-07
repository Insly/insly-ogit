package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/labbs/git-server-s3/internal/testutil"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type queueReply struct {
	messages []types.Message
	fail     bool
}

type queueFixture struct {
	client     *sqs.Client
	url        string
	replies    chan queueReply
	receives   chan sqs.ReceiveMessageInput
	deletes    chan sqs.DeleteMessageBatchInput
	failDelete atomic.Bool
}

func newQueue(t *testing.T) *queueFixture {
	t.Helper()
	q := &queueFixture{replies: make(chan queueReply, 20), receives: make(chan sqs.ReceiveMessageInput, 100), deletes: make(chan sqs.DeleteMessageBatchInput, 20)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch r.Header.Get("X-Amz-Target") {
		case "AmazonSQS.ReceiveMessage":
			var in sqs.ReceiveMessageInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
				return
			}
			q.receives <- in
			select {
			case reply := <-q.replies:
				if reply.fail {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"__type":"ServiceUnavailable","message":"retry"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Messages": reply.messages})
			case <-r.Context().Done():
			}
		case "AmazonSQS.DeleteMessageBatch":
			var in sqs.DeleteMessageBatchInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
				return
			}
			q.deletes <- in
			if q.failDelete.Load() {
				_ = json.NewEncoder(w).Encode(map[string]any{"Failed": []map[string]any{{"Id": aws.ToString(in.Entries[0].Id), "Code": "InternalError", "SenderFault": false}}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"Successful": []map[string]string{{"Id": aws.ToString(in.Entries[0].Id)}}})
			}
		default:
			t.Errorf("unexpected SQS operation %s", r.Header.Get("X-Amz-Target"))
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	q.url = server.URL + "/123/replication"
	q.client = sqs.NewFromConfig(aws.Config{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1}, func(o *sqs.Options) { o.BaseEndpoint = aws.String(server.URL) })
	return q
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for listener")
	}
	var zero T
	return zero
}

func event(bucket, key, kind, receipt string) types.Message {
	body, _ := json.Marshal(map[string]any{"Records": []any{map[string]any{
		"eventSource": "aws:s3", "eventVersion": "2.1", "eventName": kind,
		"s3": map[string]any{"bucket": map[string]string{"name": bucket}, "object": map[string]string{"key": url.QueryEscape(key)}},
	}}})
	return types.Message{Body: aws.String(string(body)), ReceiptHandle: aws.String(receipt)}
}

func listenerSetup(t *testing.T) (*SQSListener, *queueFixture, *testutil.S3, *atomic.Int64) {
	t.Helper()
	f := testutil.NewS3(t)
	count := new(atomic.Int64)
	f.Before = func(_ http.ResponseWriter, _ *http.Request) bool { count.Add(1); return false }
	q := newQueue(t)
	// Spaces and literal plus signs exercise S3's form-style URL key encoding.
	src := s3store.NewS3Storer(f.Client(), "eu", "repo + flags.git", zerolog.Nop())
	dst := s3store.NewS3Storer(f.Client(), "us", "repo.git", zerolog.Nop())
	return &SQSListener{Mirror: &Mirror{Source: src, Destination: dst, Branch: branch}, Client: q.client, QueueURL: q.url, SourceBucket: "eu", SourceKey: "repo + flags.git/refs/heads/main", FallbackInterval: time.Hour, Timeout: time.Second}, q, f, count
}

func runListener(t *testing.T, l *SQSListener) (chan Status, chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	statuses := make(chan Status, 100)
	errs := make(chan error, 100)
	l.OnError = func(err error) { errs <- err }
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx, func(s Status) { statuses <- s }) }()
	t.Cleanup(func() { cancel(); require.ErrorIs(t, receive(t, done), context.Canceled) })
	return statuses, errs, cancel
}

func publishSource(t *testing.T, l *SQSListener, parent plumbing.Hash, content string) plumbing.Hash {
	t.Helper()
	h := commit(t, l.Mirror.Source, parent, content)
	if parent.IsZero() {
		require.NoError(t, l.Mirror.Source.SetReference(plumbing.NewHashReference(branch, h)))
	} else {
		require.NoError(t, l.Mirror.Source.CheckAndSetReference(plumbing.NewHashReference(branch, h), plumbing.NewHashReference(branch, parent)))
	}
	return h
}

func assertReplica(t *testing.T, l *SQSListener, want plumbing.Hash, content string) {
	t.Helper()
	ref, err := l.Mirror.Destination.Reference(branch)
	require.NoError(t, err)
	require.Equal(t, want, ref.Hash())
	c, err := object.GetCommit(l.Mirror.Destination, want)
	require.NoError(t, err)
	file, err := c.File("flags.yaml")
	require.NoError(t, err)
	body, err := file.Contents()
	require.NoError(t, err)
	require.Equal(t, content, body)
}

func TestSQSStartupNotificationAndDuplicate(t *testing.T) {
	l, q, _, count := listenerSetup(t)
	a := publishSource(t, l, plumbing.ZeroHash, "old")
	statuses, _, _ := runListener(t, l)
	require.Empty(t, receive(t, statuses).Error)
	assertReplica(t, l, a, "old")
	in := receive(t, q.receives)
	require.Equal(t, int32(20), in.WaitTimeSeconds)
	require.Equal(t, int32(10), in.MaxNumberOfMessages)
	require.GreaterOrEqual(t, in.VisibilityTimeout, int32(62))
	require.Equal(t, q.url, aws.ToString(in.QueueUrl))
	b := publishSource(t, l, a, "new")
	msg := event(l.SourceBucket, l.SourceKey, "ObjectCreated:Put", "first")
	q.replies <- queueReply{messages: []types.Message{msg, event(l.SourceBucket, l.SourceKey, "ObjectCreated:Copy", "second")}}
	require.Empty(t, receive(t, statuses).Error)
	deleted := receive(t, q.deletes)
	require.Len(t, deleted.Entries, 2)
	assertReplica(t, l, b, "new")
	_ = receive(t, q.receives)
	before := count.Load()
	// A delayed notification contains no trustworthy Git revision: read latest head.
	q.replies <- queueReply{messages: []types.Message{msg}}
	require.Empty(t, receive(t, statuses).Error)
	_ = receive(t, q.deletes)
	_ = receive(t, q.receives)
	require.Equal(t, int64(2), count.Load()-before)
}

func TestSQSFiltersWithoutS3ReadsAndLeavesMalformedMessages(t *testing.T) {
	l, q, _, count := listenerSetup(t)
	publishSource(t, l, plumbing.ZeroHash, "ready")
	statuses, errs, _ := runListener(t, l)
	require.Empty(t, receive(t, statuses).Error)
	_ = receive(t, q.receives)
	before := count.Load()
	q.replies <- queueReply{messages: []types.Message{
		event("other", l.SourceKey, "ObjectCreated:Put", "other-bucket"),
		event(l.SourceBucket, l.SourceKey+"-other", "ObjectCreated:Put", "other-key"),
		event(l.SourceBucket, l.SourceKey, "ObjectRemoved:Delete", "other-kind"),
		{Body: aws.String(`{"Service":"Amazon S3","Event":"s3:TestEvent","Bucket":"eu"}`), ReceiptHandle: aws.String("test")},
		{Body: aws.String(`{"Records":[{}`), ReceiptHandle: aws.String("malformed")},
	}}
	require.Error(t, receive(t, errs))
	deleted := receive(t, q.deletes)
	var receipts []string
	for _, e := range deleted.Entries {
		receipts = append(receipts, aws.ToString(e.ReceiptHandle))
	}
	require.ElementsMatch(t, []string{"other-bucket", "other-key", "other-kind", "test"}, receipts)
	_ = receive(t, q.receives)
	require.Equal(t, before, count.Load())
}

func TestSQSFailedTransferAndDeleteRetry(t *testing.T) {
	l, q, f, _ := listenerSetup(t)
	a := publishSource(t, l, plumbing.ZeroHash, "old")
	statuses, errs, _ := runListener(t, l)
	require.Empty(t, receive(t, statuses).Error)
	_ = receive(t, q.receives)
	b := publishSource(t, l, a, "new")
	key := "eu/repo + flags.git/objects/" + b.String()[:2] + "/" + b.String()[2:]
	original, ok := f.Get(key)
	require.True(t, ok)
	f.Delete(key)
	msg := event(l.SourceBucket, l.SourceKey, "ObjectCreated:Put", "retry")
	q.replies <- queueReply{messages: []types.Message{msg}}
	require.NotEmpty(t, receive(t, statuses).Error)
	require.Error(t, receive(t, errs))
	_ = receive(t, q.receives)
	require.Empty(t, q.deletes, "failed replication must not acknowledge the receipt")
	assertReplica(t, l, a, "old")
	f.Put(key, original.Body, original.Type)
	q.failDelete.Store(true)
	q.replies <- queueReply{messages: []types.Message{msg}}
	require.Empty(t, receive(t, statuses).Error)
	_ = receive(t, q.deletes)
	require.Error(t, receive(t, errs), "partial batch-delete failure must be observable")
	assertReplica(t, l, b, "new")
	_ = receive(t, q.receives)
	q.failDelete.Store(false)
	q.replies <- queueReply{messages: []types.Message{msg}}
	require.Empty(t, receive(t, statuses).Error)
	deleted := receive(t, q.deletes)
	require.Equal(t, "retry", aws.ToString(deleted.Entries[0].ReceiptHandle))
}

func TestSQSFallbackDuringBlockedReceiveAndReceiveFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			l, q, _, _ := listenerSetup(t)
			l.FallbackInterval = 40 * time.Millisecond
			a := publishSource(t, l, plumbing.ZeroHash, "old")
			statuses, errs, _ := runListener(t, l)
			require.Empty(t, receive(t, statuses).Error)
			_ = receive(t, q.receives)
			if fail {
				q.replies <- queueReply{fail: true}
				require.Error(t, receive(t, errs))
			}
			b := publishSource(t, l, a, "fallback")
			require.Eventually(t, func() bool { return l.Mirror.Status().AppliedRevision == b.String() }, time.Second, 10*time.Millisecond)
			assertReplica(t, l, b, "fallback")
			if fail {
				require.Empty(t, q.receives, "receive errors need backoff, not a busy loop")
			}
		})
	}
}

func TestSQSRejectsMalformedEnvelopes(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"Records":[]}`, `{"Records":[{}]}`, `{"Type":"Notification","Message":"{}"}`, strings.Replace(aws.ToString(event("eu", "key", "ObjectCreated:Put", "r").Body), `"key":"key"`, `"key":"%ZZ"`, 1)} {
		t.Run(body, func(t *testing.T) {
			l, q, _, _ := listenerSetup(t)
			publishSource(t, l, plumbing.ZeroHash, "ready")
			statuses, errs, _ := runListener(t, l)
			require.Empty(t, receive(t, statuses).Error)
			_ = receive(t, q.receives)
			q.replies <- queueReply{messages: []types.Message{{Body: aws.String(body), ReceiptHandle: aws.String("bad")}}}
			require.Error(t, receive(t, errs))
			_ = receive(t, q.receives)
			require.Empty(t, q.deletes)
		})
	}
}
