package router

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/labbs/git-server-s3/internal/config"
)

func accessPolicy(a config.AccessConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		path := c.Path()
		read := (c.Method() == "GET" && strings.HasSuffix(path, "/info/refs") && c.Query("service") == "git-upload-pack") || (c.Method() == "POST" && strings.HasSuffix(path, "/git-upload-pack")) || (c.Method() == "GET" && path == "/replication/status")
		write := (c.Method() == "GET" && strings.HasSuffix(path, "/info/refs") && c.Query("service") == "git-receive-pack") || (c.Method() == "POST" && strings.HasSuffix(path, "/git-receive-pack"))
		if a.ReadOnly && (write || (!read && c.Method() != "GET" && c.Method() != "HEAD")) {
			return c.SendStatus(403)
		}
		if a.ReadToken == "" && a.WriteToken == "" && a.AdminToken == "" {
			return c.Next()
		}
		token := ""
		header := c.Get("Authorization")
		if strings.HasPrefix(header, "Bearer ") {
			token = strings.TrimPrefix(header, "Bearer ")
		} else if strings.HasPrefix(header, "Basic ") {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
			if err == nil {
				_, token, _ = strings.Cut(string(decoded), ":")
			}
		}
		equal := func(expected string) bool {
			a, b := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(expected))
			return expected != "" && token != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
		}
		admin, writer, reader := equal(a.AdminToken), equal(a.WriteToken), equal(a.ReadToken)
		if !admin && !writer && !reader {
			c.Set("WWW-Authenticate", `Basic realm="ogit"`)
			return c.SendStatus(401)
		}
		if admin || (read && (reader || writer)) || (write && writer) {
			return c.Next()
		}
		return c.SendStatus(403)
	}
}
