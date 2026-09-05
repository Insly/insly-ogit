package router

import (
	"github.com/gofiber/fiber/v2"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/pkg/replication"
	"github.com/labbs/git-server-s3/pkg/storage"
	"github.com/rs/zerolog"
)

type Config struct {
	Mirror  *replication.Mirror
	Logger  zerolog.Logger
	Fiber   *fiber.App
	Storage storage.GitRepositoryStorage
}

func (c *Config) Configure() {
	c.Logger.Info().Msg("Configuring API routes")

	c.Fiber.Use(accessPolicy(config.Access))
	if c.Mirror != nil {
		c.Fiber.Get("/replication/status", func(ctx *fiber.Ctx) error { return ctx.JSON(c.Mirror.Status()) })
	}
	NewGitRouter(c)
	NewRepoRouter(c)
	if config.Debug.Endpoints {
		NewDebugRouter(c)
	}
}
