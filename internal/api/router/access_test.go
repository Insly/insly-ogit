package router

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/pkg/storage/local"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestAccessRolesAndReadOnly(t *testing.T) {
	saved := config.Access
	t.Cleanup(func() { config.Access = saved })
	config.Access = config.AccessConfig{ReadToken: "reader", WriteToken: "writer", AdminToken: "admin"}
	savedPath := config.Storage.Local.Path
	t.Cleanup(func() { config.Storage.Local.Path = savedPath })
	config.Storage.Local.Path = t.TempDir()
	storage := local.NewLocalStorage(zerolog.Nop())
	require.NoError(t, storage.Configure())
	require.NoError(t, storage.CreateRepository("flags.git"))
	for _, readOnly := range []bool{false, true} {
		config.Access.ReadOnly = readOnly
		app := fiber.New()
		c := Config{Fiber: app, Storage: storage, Logger: zerolog.Nop()}
		c.Configure()
		cases := []struct {
			method, path, token string
			status              int
		}{
			{"GET", "/flags.git/info/refs?service=git-upload-pack", "", 401},
			{"GET", "/flags.git/info/refs?service=git-upload-pack", "wrong", 401},
			{"GET", "/flags.git/info/refs?service=git-upload-pack", "reader", 200},
			{"POST", "/flags.git/git-upload-pack", "reader", 400}, // allowed through auth, rejected by protocol decoder
			{"GET", "/flags.git/info/refs?service=git-receive-pack", "reader", 403},
			{"POST", "/flags.git/git-receive-pack", "reader", 403},
			{"POST", "/api/repo", "writer", 403},
			{"GET", "/api/repos", "writer", 403},
			{"GET", "/api/repos", "admin", 200},
		}
		if readOnly {
			cases = append(cases, struct {
				method, path, token string
				status              int
			}{"POST", "/flags.git/git-receive-pack", "admin", 403}, struct {
				method, path, token string
				status              int
			}{"POST", "/api/repo", "admin", 403})
		} else {
			cases = append(cases, struct {
				method, path, token string
				status              int
			}{"GET", "/flags.git/info/refs?service=git-receive-pack", "writer", 200})
		}
		for _, tc := range cases {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte("bad request")))
			if tc.token != "" {
				req.SetBasicAuth("git", tc.token)
			}
			resp, err := app.Test(req)
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, tc.status, resp.StatusCode, "readonly=%v %s %s token=%s", readOnly, tc.method, tc.path, tc.token)
		}
	}
	_ = http.MethodGet
}
