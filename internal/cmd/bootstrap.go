package cmd

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/internal/flags"
	"github.com/labbs/git-server-s3/pkg/logger"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
	"github.com/urfave/cli/v3"
)

func NewBootstrap() *cli.Command {
	list := append(flags.GenericFlags(), flags.StorageFlags()...)
	list = append(list, flags.LoggerFlags()...)
	list = append(list, &cli.StringFlag{Name: "repository", Required: true}, &cli.StringFlag{Name: "branch", Value: "main"}, &cli.StringFlag{Name: "seed-directory", Required: true})
	return &cli.Command{Name: "bootstrap", Usage: "Seed an absent S3 branch without replacing existing flags", Flags: list, Action: func(ctx context.Context, c *cli.Command) error {
		if config.Storage.Type != "s3" {
			return fmt.Errorf("bootstrap requires S3 storage")
		}
		repo := c.String("repository")
		if strings.ContainsAny(repo, "/\\") || repo == "." || repo == ".." {
			return fmt.Errorf("repository must be one name")
		}
		files, err := seedFiles(c.String("seed-directory"))
		if err != nil {
			return err
		}
		store := s3store.NewS3Storage(logger.NewLogger(config.Logger.Level, config.Logger.Pretty, c.Root().Version))
		if err := store.Configure(); err != nil {
			return err
		}
		hash, err := store.Bootstrap(ctx, repo, c.String("branch"), files)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(c.Root().Writer, hash.String())
		return err
	}}
}
func seedFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	var total int64
	err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			return fmt.Errorf("seed directory must contain flag files, not .git")
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("seed files must be regular files")
		}
		total += info.Size()
		if total > 16<<20 {
			return fmt.Errorf("seed files exceed 16 MiB")
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = body
		return nil
	})
	return files, err
}
