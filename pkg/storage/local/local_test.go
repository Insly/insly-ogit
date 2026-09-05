package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/labbs/git-server-s3/internal/config"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestLocalConfigCreatesMissingDirectory(t *testing.T) {
	previous := config.Storage.Local.Path
	t.Cleanup(func() { config.Storage.Local.Path = previous })
	config.Storage.Local.Path = filepath.Join(t.TempDir(), "missing", "repositories")
	cfg := LocalConfig{Logger: zerolog.Nop()}
	require.NotPanics(t, func() { require.NoError(t, cfg.Configure()) })
	info, err := os.Stat(config.Storage.Local.Path)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}
