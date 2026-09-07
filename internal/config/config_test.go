package config

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestValidateServerRejectsBypasses(t *testing.T) {
	oldA, oldS, oldR, oldStorage := Access, SSH, Replication, Storage
	t.Cleanup(func() { Access, SSH, Replication, Storage = oldA, oldS, oldR, oldStorage })
	for _, a := range []AccessConfig{{ReadToken: "r"}, {ReadOnly: true}} {
		Access = a
		SSH.Enabled = true
		require.Error(t, ValidateServer())
	}
	SSH.Enabled = false
	Access = AccessConfig{ReadToken: "same", AdminToken: "same"}
	require.Error(t, ValidateServer())
	Access = AccessConfig{}
	Storage.Type = "s3"
	Replication = ReplicationConfig{SourceBucket: "eu", SourceRegion: "eu-west-1", Repository: "flags", Branch: "main", Interval: time.Second, Timeout: time.Second}
	require.Error(t, ValidateServer(), "mirror must force clients read-only")
	Access.ReadOnly = true
	require.NoError(t, ValidateServer())
	Replication.AuditInterval = -time.Second
	require.Error(t, ValidateServer(), "negative audit intervals must not start a broken controller")
	Replication.AuditInterval = time.Hour
	require.NoError(t, ValidateServer())
	Replication.Interval = 0
	require.Error(t, ValidateServer())
}

func TestValidateReplicationListener(t *testing.T) {
	oldA, oldS, oldR, oldStorage := Access, SSH, Replication, Storage
	t.Cleanup(func() { Access, SSH, Replication, Storage = oldA, oldS, oldR, oldStorage })
	Access = AccessConfig{ReadOnly: true}
	SSH.Enabled = false
	Storage.Type = "s3"
	Storage.S3.Bucket = "us"
	valid := ReplicationConfig{
		SourceBucket: "eu", SourceRegion: "eu-west-1", Repository: "flags", Branch: "main",
		Mode: "sqs", QueueURL: "https://sqs.eu-west-1.amazonaws.com/123/replication",
		Interval: time.Second, Timeout: time.Minute, FallbackInterval: 5 * time.Minute,
	}
	for _, tc := range []struct {
		name   string
		change func(*ReplicationConfig)
	}{
		{"unknown mode", func(r *ReplicationConfig) {
			r.Mode = "typo"
			r.QueueURL = ""
		}},
		{"missing queue", func(r *ReplicationConfig) { r.QueueURL = "" }},
		{"missing source", func(r *ReplicationConfig) { r.SourceBucket = "" }},
		{"missing fallback", func(r *ReplicationConfig) { r.FallbackInterval = 0 }},
		{"invalid queue", func(r *ReplicationConfig) { r.QueueURL = "not-a-url" }},
		{"negative queue wait", func(r *ReplicationConfig) { r.QueueWaitTime = -time.Second }},
		{"excessive queue wait", func(r *ReplicationConfig) { r.QueueWaitTime = 21 * time.Second }},
		{"fractional queue wait", func(r *ReplicationConfig) { r.QueueWaitTime = 1500 * time.Millisecond }},
		{"negative queue delay", func(r *ReplicationConfig) { r.QueueEmptyDelay = -time.Second }},
		{"excessive timeout", func(r *ReplicationConfig) { r.Timeout = 6 * time.Hour }},
		{"queue in poll mode", func(r *ReplicationConfig) { r.Mode = "poll" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Replication = valid
			tc.change(&Replication)
			require.Error(t, ValidateServer())
		})
	}
	Replication = valid
	Replication.Interval = 0 // SQS mode uses the fallback interval instead.
	require.NoError(t, ValidateServer())
}
