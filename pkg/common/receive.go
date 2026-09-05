package common

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/labbs/git-server-s3/pkg/gitgraph"
	"github.com/labbs/git-server-s3/pkg/storage"
	s3store "github.com/labbs/git-server-s3/pkg/storage/s3"
)

// go-git's receive-pack uses SetReference and ignores the client's old hash.
// Keep its advertisement/pack format but own the publication boundary.
type safeTransport struct {
	transport.Transport
	repo storage.GitRepositoryStorage
	path string
}

func (t *safeTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	base, err := t.Transport.NewReceivePackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	return &receiveSession{ReceivePackSession: base, repo: t.repo, path: t.path}, nil
}

type receiveSession struct {
	transport.ReceivePackSession
	repo storage.GitRepositoryStorage
	path string
}

func (s *receiveSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}
func (s *receiveSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	a, err := s.ReceivePackSession.AdvertisedReferencesContext(ctx)
	if err == nil {
		a.Capabilities.Delete(capability.DeleteRefs)
		a.Capabilities.Delete(capability.Atomic)
	}
	return a, err
}
func (s *receiveSession) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	report := &packp.ReportStatus{UnpackStatus: "ok"}
	finish := func(err error) (*packp.ReportStatus, error) {
		for _, c := range req.Commands {
			msg := "ok"
			if err != nil {
				msg = err.Error()
			}
			report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{ReferenceName: c.Name, Status: msg})
		}
		return report, err
	}
	if len(req.Commands) != 1 {
		return finish(fmt.Errorf("push exactly one branch per transaction"))
	}
	c := req.Commands[0]
	if !c.Name.IsBranch() || c.New.IsZero() {
		return finish(fmt.Errorf("only branch creation and forward updates are supported"))
	}
	if err := c.Name.Validate(); err != nil {
		return finish(err)
	}
	if req.Capabilities.Supports(capability.Atomic) {
		return finish(fmt.Errorf("atomic multi-ref pushes are unsupported"))
	}
	st, err := s.repo.GetStorer(s.path)
	if err != nil {
		return finish(err)
	}
	if s3, ok := st.(*s3store.S3Storer); ok {
		st = s3.WithContext(ctx)
	}
	if req.Packfile != nil {
		defer req.Packfile.Close()
		if err := packfile.UpdateObjectStorage(st, req.Packfile); err != nil {
			report.UnpackStatus = err.Error()
			return finish(err)
		}
	}
	ancestors, err := gitgraph.Walk(ctx, st, c.New, nil)
	if err != nil {
		return finish(err)
	}
	var old *plumbing.Reference
	if !c.Old.IsZero() {
		if !ancestors[c.Old] {
			return finish(fmt.Errorf("non-fast-forward update rejected"))
		}
		old = plumbing.NewHashReference(c.Name, c.Old)
	}
	if err := ctx.Err(); err != nil {
		return finish(err)
	}
	return finish(st.CheckAndSetReference(plumbing.NewHashReference(c.Name, c.New), old))
}
