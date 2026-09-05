// Package gitgraph validates a complete Git object graph before ref publication.
package gitgraph

import (
	"context"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Walk verifies hashes, types, commit ancestry and trees. Gitlinks name external
// repositories and are deliberately not traversed. The callback runs only on
// verified objects; callers publish no ref until Walk has completed.
func Walk(ctx context.Context, s storer.EncodedObjectStorer, head plumbing.Hash, visit func(plumbing.EncodedObject) error) (map[plumbing.Hash]bool, error) {
	type node struct {
		hash plumbing.Hash
		typ  plumbing.ObjectType
	}
	queue := []node{{head, plumbing.CommitObject}}
	seen := map[plumbing.Hash]plumbing.ObjectType{}
	commits := map[plumbing.Hash]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if typ, ok := seen[n.hash]; ok {
			if typ != n.typ {
				return nil, fmt.Errorf("object %s has incompatible types", n.hash)
			}
			continue
		}
		if len(seen) >= 100000 {
			return nil, fmt.Errorf("repository exceeds 100000 reachable objects")
		}
		obj, err := s.EncodedObject(n.typ, n.hash)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n.hash, err)
		}
		if obj.Type() != n.typ {
			return nil, fmt.Errorf("wrong object type for %s", n.hash)
		}
		r, err := obj.Reader()
		if err != nil {
			return nil, err
		}
		hasher := plumbing.NewHasher(obj.Type(), obj.Size())
		count, err := io.Copy(hasher, r)
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if count != obj.Size() || hasher.Sum() != n.hash {
			return nil, fmt.Errorf("object hash mismatch: %s", n.hash)
		}
		seen[n.hash] = n.typ
		switch n.typ {
		case plumbing.CommitObject:
			c, err := object.DecodeCommit(s, obj)
			if err != nil {
				return nil, err
			}
			commits[n.hash] = true
			queue = append(queue, node{c.TreeHash, plumbing.TreeObject})
			for _, p := range c.ParentHashes {
				queue = append(queue, node{p, plumbing.CommitObject})
			}
		case plumbing.TreeObject:
			tree, err := object.DecodeTree(s, obj)
			if err != nil {
				return nil, err
			}
			for _, e := range tree.Entries {
				typ := plumbing.BlobObject
				switch e.Mode {
				case filemode.Submodule:
					continue
				case filemode.Dir:
					typ = plumbing.TreeObject
				case filemode.Regular, filemode.Executable, filemode.Symlink, filemode.Deprecated:
				default:
					return nil, fmt.Errorf("invalid tree mode")
				}
				queue = append(queue, node{e.Hash, typ})
			}
		}
		if visit != nil {
			if err := visit(obj); err != nil {
				return nil, err
			}
		}
	}
	return commits, nil
}
