package git

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// ServeShallowUploadPack implements absolute commit depth and complete trees.
// It sends a self-contained pack; haves need not be trusted to omit objects.
func ServeShallowUploadPack(w io.Writer, s storer.Storer, r *packp.UploadPackRequest, completion ...bool) error {
	done := true
	if len(completion) > 0 {
		done = completion[0]
	}
	depth, ok := r.Depth.(packp.DepthCommits)
	if !ok || depth < 0 || (depth == 0 && len(r.Shallows) == 0) {
		return fmt.Errorf("commit depth or existing shallow boundary required")
	}
	oldBoundaries := map[plumbing.Hash]bool{}
	for _, h := range r.Shallows {
		oldBoundaries[h] = true
	}
	type node struct {
		hash  plumbing.Hash
		depth int
	}
	var queue []node
	for _, h := range r.Wants {
		queue = append(queue, node{h, 1})
	}
	seen := map[plumbing.Hash]bool{}
	boundaries := map[plumbing.Hash]bool{}
	var hashes []plumbing.Hash
	add := func(h plumbing.Hash) {
		if !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	var trees func(plumbing.Hash) error
	trees = func(h plumbing.Hash) error {
		if seen[h] {
			return nil
		}
		tree, err := object.GetTree(s, h)
		if err != nil {
			return err
		}
		add(h)
		for _, e := range tree.Entries {
			if e.Mode == filemode.Submodule {
				continue
			}
			if e.Mode == filemode.Dir {
				if err := trees(e.Hash); err != nil {
					return err
				}
			} else {
				if _, err := s.EncodedObject(plumbing.BlobObject, e.Hash); err != nil {
					return err
				}
				add(e.Hash)
			}
		}
		return nil
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n.hash] {
			continue
		}
		obj, err := s.EncodedObject(plumbing.AnyObject, n.hash)
		if err != nil {
			return err
		}
		if obj.Type() == plumbing.TagObject {
			tag, err := object.DecodeTag(s, obj)
			if err != nil {
				return err
			}
			add(n.hash)
			queue = append(queue, node{tag.Target, n.depth})
			continue
		}
		commit, err := object.DecodeCommit(s, obj)
		if err != nil {
			return err
		}
		add(n.hash)
		if err := trees(commit.TreeHash); err != nil {
			return err
		}
		if (depth > 0 && n.depth == int(depth)) || (depth == 0 && oldBoundaries[n.hash]) {
			if len(commit.ParentHashes) > 0 {
				boundaries[n.hash] = true
			}
			continue
		}
		for _, h := range commit.ParentHashes {
			queue = append(queue, node{h, n.depth + 1})
		}
	}
	update := packp.ShallowUpdate{}
	for h := range boundaries {
		update.Shallows = append(update.Shallows, h)
	}
	for _, h := range r.Shallows {
		if seen[h] && !boundaries[h] {
			update.Unshallows = append(update.Unshallows, h)
		}
	}
	if depth > 0 {
		if err := update.Encode(w); err != nil {
			return err
		}
	}
	if !done {
		if depth == 0 {
			return pktline.NewEncoder(w).EncodeString("NAK\n")
		}
		return nil
	}
	if err := pktline.NewEncoder(w).EncodeString("NAK\n"); err != nil {
		return err
	}
	output := w
	band := false
	if r.Capabilities.Supports(capability.Sideband64k) {
		output = sideband.NewMuxer(sideband.Sideband64k, w)
		band = true
	} else if r.Capabilities.Supports(capability.Sideband) {
		output = sideband.NewMuxer(sideband.Sideband, w)
		band = true
	}
	if _, err := packfile.NewEncoder(output, s, false).Encode(hashes, 0); err != nil {
		return err
	}
	if band {
		return pktline.NewEncoder(w).Flush()
	}
	return nil
}
