package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/internal/flags"
	"github.com/labbs/git-server-s3/pkg/replication"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestReplicationRunnerSelectsSQSWithSourceIdentity(t *testing.T) {
	for _, source := range []string{"cli", "env", "yaml"} {
		t.Run(source, func(t *testing.T) { testReplicationRunnerConfiguration(t, source) })
	}
}

func testReplicationRunnerConfiguration(t *testing.T, source string) {
	oldConfig, oldFile, oldAccess := config.Replication, config.ConfigFile, config.Access
	t.Cleanup(func() { config.Replication, config.ConfigFile, config.Access = oldConfig, oldFile, oldAccess })
	config.ConfigFile = filepath.Join(t.TempDir(), "config.yaml")
	args := []string{"test"}
	switch source {
	case "cli":
		args = append(args, "--replication.queue-wait-time", "7s", "--replication.queue-empty-delay", "400ms")
	case "env":
		t.Setenv("REPLICATION_QUEUE_WAIT_TIME", "7s")
		t.Setenv("REPLICATION_QUEUE_EMPTY_DELAY", "400ms")
	case "yaml":
		require.NoError(t, os.WriteFile(config.ConfigFile, []byte("replication:\n  queue-wait-time: 7s\n  queue-empty-delay: 400ms\n"), 0600))
	}
	command := &cli.Command{Flags: flags.HAFlags(), Action: func(context.Context, *cli.Command) error { return nil }}
	require.NoError(t, command.Run(context.Background(), args))

	src, dst := memory.NewStorage(), memory.NewStorage()
	branch := plumbing.NewBranchReferenceName("main")
	makeCommit := func(parent plumbing.Hash, message string) plumbing.Hash {
		tree := object.Tree{}
		enc := src.NewEncodedObject()
		require.NoError(t, tree.Encode(enc))
		treeHash, err := src.SetEncodedObject(enc)
		require.NoError(t, err)
		c := object.Commit{TreeHash: treeHash, Message: message, Author: object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1, 0)}}
		c.Committer = c.Author
		if !parent.IsZero() {
			c.ParentHashes = []plumbing.Hash{parent}
		}
		enc = src.NewEncodedObject()
		require.NoError(t, c.Encode(enc))
		h, err := src.SetEncodedObject(enc)
		require.NoError(t, err)
		require.NoError(t, src.SetReference(plumbing.NewHashReference(branch, h)))
		return h
	}
	a := makeCommit(plumbing.ZeroHash, "initial")
	mirror := &replication.Mirror{Source: src, Destination: dst, Branch: branch}
	trigger := make(chan struct{})
	receiving := make(chan string, 10)
	deleted := make(chan struct{}, 1)
	var delivered atomic.Bool
	var receiveCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch r.Header.Get("X-Amz-Target") {
		case "AmazonSQS.ReceiveMessage":
			var input sqs.ReceiveMessageInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			assert.Equal(t, int32(7), input.WaitTimeSeconds)
			receiving <- r.Header.Get("Authorization")
			if receiveCount.Add(1) == 1 {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			if delivered.Load() {
				<-r.Context().Done()
				return
			}
			select {
			case <-trigger:
			case <-r.Context().Done():
				return
			}
			delivered.Store(true)
			// Source repository normalization must differ from the local repository.
			body := `{"Records":[{"eventSource":"aws:s3","eventVersion":"2.1","eventName":"ObjectCreated:Put","s3":{"bucket":{"name":"eu"},"object":{"key":"repositories%2Fflags.git%2Frefs%2Fheads%2Fmain"}}}]}`
			_ = json.NewEncoder(w).Encode(map[string]any{"Messages": []map[string]string{{"Body": body, "ReceiptHandle": "receipt"}}})
		case "AmazonSQS.DeleteMessageBatch":
			deleted <- struct{}{}
			_, _ = w.Write([]byte(`{"Successful":[{"Id":"0"}]}`))
		default:
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	rc := config.ReplicationConfig{Mode: "sqs", SourceBucket: "eu", SourceRegion: "eu-west-1", SourceRepository: "flags", Repository: "local-copy", Branch: "main", QueueURL: server.URL + "/queue", QueueEndpoint: server.URL, Interval: time.Hour, Timeout: time.Second, FallbackInterval: time.Hour}
	rc.QueueWaitTime = config.Replication.QueueWaitTime
	rc.QueueEmptyDelay = config.Replication.QueueEmptyDelay
	// A cached provider is retained by the SQS client, including session credentials.
	sourceClient := s3.NewFromConfig(aws.Config{Region: rc.SourceRegion, Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("source-key", "test-secret", "session-token"))})
	run := newReplicationRunner(mirror, sourceClient, rc, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	statuses := make(chan replication.Status, 5)
	go func() { done <- run(ctx, func(s replication.Status) { statuses <- s }) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Error("runner did not stop")
		}
	}()
	select {
	case status := <-statuses:
		require.Equal(t, a.String(), status.AppliedRevision)
	case <-time.After(time.Second):
		t.Fatal("no startup reconciliation")
	}
	select {
	case auth := <-receiving:
		require.Contains(t, auth, "Credential=source-key/")
		require.Contains(t, auth, "/eu-west-1/sqs/aws4_request")
	case <-time.After(time.Second):
		t.Fatal("SQS mode did not start a queue consumer")
	}
	select {
	case <-receiving:
		t.Fatal("another receive occurred before the configured empty delay")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-receiving:
	case <-time.After(time.Second):
		t.Fatal("receive did not resume after the configured empty delay")
	}
	b := makeCommit(a, "updated by notification")
	close(trigger)
	select {
	case <-deleted:
	case <-time.After(time.Second):
		t.Fatal("notification did not publish and acknowledge")
	}
	ref, err := dst.Reference(branch)
	require.NoError(t, err)
	require.Equal(t, b, ref.Hash())
}
