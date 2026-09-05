package s3

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitstorage "github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/labbs/git-server-s3/pkg/gitgraph"
)

// Bootstrap seeds an absent branch. Existing flags always win, including after
// a crash or a competing initializer. A read error never authorizes a write.
func (s *S3Storage) Bootstrap(ctx context.Context, repo, branch string, files map[string][]byte) (plumbing.Hash, error) {
	name := plumbing.NewBranchReferenceName(branch)
	if err := name.Validate(); err != nil {
		return plumbing.ZeroHash, err
	}
	st := NewS3Storer(s.client, s.bucket, s.getRepoKey(repo), s.Logger).WithContext(ctx)
	existing, err := st.Reference(name)
	if err == nil {
		if _, err := gitgraph.Walk(ctx, st, existing.Hash(), nil); err != nil {
			return plumbing.ZeroHash, err
		}
		if err := st.EnsureRepository(name); err != nil {
			return plumbing.ZeroHash, err
		}
		return existing.Hash(), nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, err
	}
	if len(files) == 0 {
		return plumbing.ZeroHash, fmt.Errorf("bootstrap requires seed files")
	}
	mem := memory.NewStorage()
	fs := memfs.New()
	r, err := git.Init(mem, fs)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	wt, err := r.Worktree()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") || name == ".." || strings.HasPrefix(name, "../") {
			return plumbing.ZeroHash, fmt.Errorf("invalid seed path")
		}
		for _, part := range strings.Split(name, "/") {
			if strings.EqualFold(part, ".git") {
				return plumbing.ZeroHash, fmt.Errorf("seed must not contain .git")
			}
		}
		if err := fs.MkdirAll(path.Dir(name), 0755); err != nil {
			return plumbing.ZeroHash, err
		}
		f, err := fs.Create(name)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		_, err = f.Write(files[name])
		closeErr := f.Close()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if closeErr != nil {
			return plumbing.ZeroHash, closeErr
		}
		if _, err := wt.Add(name); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	head, err := wt.Commit("Initialize feature flags", &git.CommitOptions{Author: &object.Signature{Name: "ogit bootstrap", Email: "ogit@localhost", When: time.Unix(0, 0).UTC()}})
	if err != nil {
		return plumbing.ZeroHash, err
	}
	_, err = gitgraph.Walk(ctx, mem, head, func(o plumbing.EncodedObject) error { _, err := st.SetEncodedObject(o); return err })
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err = gitgraph.Walk(ctx, st, head, nil); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := st.EnsureRepository(name); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := st.CheckAndSetReference(plumbing.NewHashReference(name, head), nil); err != nil {
		if errors.Is(err, gitstorage.ErrReferenceHasChanged) {
			winner, e := st.Reference(name)
			if e == nil {
				if _, e = gitgraph.Walk(ctx, st, winner.Hash(), nil); e == nil {
					return winner.Hash(), nil
				}
			}
		}
		return plumbing.ZeroHash, err
	}
	return head, nil
}

// StorerForRepository also works before initialization, for controlled bootstrap/replication.
func (s *S3Storage) StorerForRepository(repo string) *S3Storer {
	return NewS3Storer(s.client, s.bucket, s.getRepoKey(repo), s.Logger)
}
