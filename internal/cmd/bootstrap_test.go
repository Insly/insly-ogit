package cmd

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedFilesPreservesPathsAndRejectsLinks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "default"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "default", "features.yaml"), []byte("flags: []"), 0600))
	files, err := seedFiles(root)
	require.NoError(t, err)
	require.Equal(t, []byte("flags: []"), files["default/features.yaml"])
	require.NoError(t, os.Symlink(filepath.Join(root, "default", "features.yaml"), filepath.Join(root, "link")))
	_, err = seedFiles(root)
	require.Error(t, err)
}
