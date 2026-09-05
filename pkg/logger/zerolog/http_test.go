package zerolog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	z "github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestHTTPLoggerCorrelatesRequest(t *testing.T) {
	for _, suppliedID := range []string{"", "client-request-123"} {
		t.Run("request_id="+suppliedID, func(t *testing.T) {
			var logs bytes.Buffer
			app := fiber.New()
			app.Use(HTTPLogger(z.New(&logs)))
			app.Use(requestid.New())
			app.Get("/logged", func(c fiber.Ctx) error { return c.Status(202).SendString("accepted") })
			req := httptest.NewRequest("GET", "http://example.test:8080/logged", nil)
			req.Header.Set("User-Agent", "ogit-test-client")
			if suppliedID != "" {
				req.Header.Set("X-Request-ID", suppliedID)
			}
			resp, err := app.Test(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, 202, resp.StatusCode)
			require.Equal(t, "accepted", string(body))
			id := resp.Header.Get("X-Request-ID")
			require.NotEmpty(t, id)
			if suppliedID != "" {
				require.Equal(t, suppliedID, id)
			}
			var entry map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
			require.Equal(t, id, entry["request_id"])
			require.Equal(t, "http", entry["proto"])
			require.Equal(t, "example.test:8080", entry["host"])
			require.Equal(t, "GET", entry["method"])
			require.Equal(t, "/logged", entry["path"])
			require.Equal(t, "ogit-test-client", entry["user_agent"])
			require.Equal(t, float64(202), entry["status"])
		})
	}
}
