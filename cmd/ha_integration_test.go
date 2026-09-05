package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/stretchr/testify/require"
)

// TestHAProcessIntegration exercises the shipped CLI and native Git against the
// SDK HTTP fixture. Real S3 remains a separate deployment acceptance boundary.
func TestHAProcessIntegration(t *testing.T) { runHAProcessIntegration(t, "") }

func runHAProcessIntegration(t *testing.T, endpoint string) {
	bin := os.Getenv("OGIT_TEST_BINARY")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "ogit")
		build := exec.Command("go", "build", "-o", bin, ".")
		out, err := build.CombinedOutput()
		require.NoError(t, err, string(out))
	}

	euBucket, usBucket := "eu", "us"
	if endpoint == "" {
		f := testutil.NewS3(t)
		endpoint = f.Server.URL
	} else {
		_, euBucket, usBucket = testutil.ExternalS3(t, endpoint)
	}
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	require.NoError(t, os.Mkdir(seed, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(seed, "features.yaml"), []byte("flag: false\n"), 0600))
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		p := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		return p
	}
	euPort, usPort := freePort(), freePort()
	conf := func(bucket string, port int, extra string) string {
		p := filepath.Join(root, bucket+".yaml")
		require.NoError(t, os.WriteFile(p, []byte(fmt.Sprintf("http:\n  port: %d\nssh:\n  enabled: false\nstorage:\n  type: s3\n  s3:\n    bucket: %s\n    endpoint: %s\n    region: us-east-1\n    access-key: test\n    secret-key: test\nauth:\n  read-token: reader\n  write-token: writer\n  admin-token: admin\n%s", port, bucket, endpoint, extra)), 0600))
		return p
	}
	eu := conf(euBucket, euPort, "")
	us := conf(usBucket, usPort, "read-only: true\nreplication:\n  source-bucket: "+euBucket+"\n  source-region: us-east-1\n  source-endpoint: "+endpoint+"\n  repository: flags\n  branch: main\n  interval: 100ms\n  timeout: 2s\n")
	run := func(program string, args ...string) string {
		t.Helper()
		c := exec.Command(program, args...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	run(bin, "bootstrap", "-c", eu, "--repository", "flags", "--seed-directory", seed)
	start := func(cfg string, port int) *exec.Cmd {
		log, err := os.CreateTemp(root, "server-*.log")
		require.NoError(t, err)
		t.Cleanup(func() { _ = log.Close() })
		c := exec.Command(bin, "server", "-c", cfg)
		c.Stdout = log
		c.Stderr = log
		require.NoError(t, c.Start())
		t.Cleanup(func() {
			if c.ProcessState == nil {
				_ = c.Process.Signal(os.Interrupt)
				_ = c.Wait()
			}
		})
		require.Eventually(t, func() bool {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == 200
		}, 10*time.Second, 50*time.Millisecond)
		return c
	}
	euProcess := start(eu, euPort)
	start(us, usPort)
	euURL := fmt.Sprintf("http://git:writer@127.0.0.1:%d/flags.git", euPort)
	usURL := fmt.Sprintf("http://git:reader@127.0.0.1:%d/flags.git", usPort)
	work := filepath.Join(root, "work")
	run("git", "clone", "--depth=1", euURL, work)
	run("git", "-C", work, "config", "user.name", "Test")
	run("git", "-C", work, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(work, "features.yaml"), []byte("flag: true\n"), 0600))
	run("git", "-C", work, "add", "features.yaml")
	run("git", "-C", work, "commit", "-m", "Enable flag")
	run("git", "-C", work, "push", "origin", "main")
	want := run("git", "-C", work, "rev-parse", "HEAD")
	require.Eventually(t, func() bool {
		out, err := exec.Command("git", "ls-remote", usURL, "refs/heads/main").CombinedOutput()
		return err == nil && strings.HasPrefix(string(out), want)
	}, 10*time.Second, 100*time.Millisecond)
	// Reseeding after user edits cannot replace the branch.
	run(bin, "bootstrap", "-c", eu, "--repository", "flags", "--seed-directory", seed)
	require.Contains(t, run("git", "ls-remote", euURL, "refs/heads/main"), want)
	require.NoError(t, euProcess.Process.Signal(os.Interrupt))
	require.NoError(t, euProcess.Wait())
	// Fresh regional readers start from local S3 with the writer process offline.
	region := filepath.Join(root, "region")
	run("git", "clone", "--depth=1", usURL, region)
	body, err := os.ReadFile(filepath.Join(region, "features.yaml"))
	require.NoError(t, err)
	require.Equal(t, "flag: true\n", string(body))
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/flags.git/git-receive-pack", usPort), strings.NewReader("bad"))
	require.NoError(t, err)
	req.SetBasicAuth("git", "admin")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, 403, resp.StatusCode)
}
