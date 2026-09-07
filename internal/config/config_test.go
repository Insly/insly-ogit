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
