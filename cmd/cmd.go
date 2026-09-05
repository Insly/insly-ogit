package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/labbs/git-server-s3/internal/cmd"
)

var version = "development"

func main() {
	sources := cli.NewValueSourceChain()
	cmd := &cli.Command{
		Name:    "ogit",
		Version: version,
		Usage:   "Git service backed by S3 or local storage",
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			config := cmd.String("config")
			if len(config) > 0 {
				configFile := fmt.Sprintf("%s.yaml", cmd.String("config"))
				if _, err := os.Stat(configFile); os.IsNotExist(err) {
					return ctx, fmt.Errorf("could not load config file: %s", configFile)
				}

				sources.Append(cli.Files(configFile))
				return ctx, nil
			}

			return ctx, nil
		},
		Commands: []*cli.Command{
			cmd.NewInstance(version),
			cmd.NewBootstrap(),
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		log.Fatalf("Error running command: %v", err)
	}
}
