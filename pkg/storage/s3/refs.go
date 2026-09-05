package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-git/go-git/v5/plumbing"
	gitstorage "github.com/go-git/go-git/v5/storage"
	"github.com/labbs/git-server-s3/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

func (s *S3Storer) context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// WithContext returns an independent request-scoped storer sharing only the SDK client.
func (s *S3Storer) WithContext(ctx context.Context) *S3Storer {
	copy := *s
	copy.ctx = ctx
	return &copy
}
func statusCode(err error) int {
	var e interface{ HTTPStatusCode() int }
	if errors.As(err, &e) {
		return e.HTTPStatusCode()
	}
	return 0
}
func refBody(ref *plumbing.Reference) string {
	if ref.Type() == plumbing.SymbolicReference {
		return "ref: " + ref.Target().String()
	}
	return ref.Hash().String()
}
func (s *S3Storer) readReference(name plumbing.ReferenceName) (*plumbing.Reference, string, error) {
	if err := name.Validate(); err != nil {
		return nil, "", err
	}
	out, err := s.client.GetObject(s.context(), &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.getObjectKey(name.String()))})
	if err != nil {
		if statusCode(err) == 404 {
			return nil, "", plumbing.ErrReferenceNotFound
		}
		return nil, "", err
	}
	defer out.Body.Close()
	body, err := io.ReadAll(io.LimitReader(out.Body, 4097))
	if err != nil {
		return nil, "", err
	}
	v := strings.TrimSpace(string(body))
	if strings.HasPrefix(v, "ref: ") {
		target := plumbing.ReferenceName(strings.TrimPrefix(v, "ref: "))
		if err := target.Validate(); err != nil {
			return nil, "", err
		}
		return plumbing.NewSymbolicReference(name, target), aws.ToString(out.ETag), nil
	}
	if len(v) != 40 || !plumbing.IsHash(v) {
		return nil, "", fmt.Errorf("invalid reference %s", name)
	}
	return plumbing.NewHashReference(name, plumbing.NewHash(v)), aws.ToString(out.ETag), nil
}
func (s *S3Storer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	ref, _, err := s.readReference(name)
	return ref, err
}

// SetReference is initialization-only. Updates must supply their expected old ref.
func (s *S3Storer) SetReference(ref *plumbing.Reference) error {
	return s.CheckAndSetReference(ref, nil)
}
func (s *S3Storer) CheckAndSetReference(next, old *plumbing.Reference) (resultErr error) {
	ctx, end := telemetry.StartOperation(s.context(), "git.ref.publish")
	s = s.WithContext(ctx)
	operation := "update"
	if old == nil {
		operation = "create"
	}
	defer func() {
		outcome := telemetry.Outcome(resultErr)
		telemetry.Count(ctx, "ogit.ref.publications", outcome, attribute.String("ogit.ref.operation", operation))
		end(outcome)
	}()

	if next == nil {
		return fmt.Errorf("nil reference")
	}
	if err := next.Name().Validate(); err != nil {
		return err
	}
	input := &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.getObjectKey(next.Name().String())), Body: strings.NewReader(refBody(next))}
	if old == nil {
		input.IfNoneMatch = aws.String("*")
	} else {
		if old.Name() != next.Name() {
			return gitstorage.ErrReferenceHasChanged
		}
		current, etag, err := s.readReference(old.Name())
		if err != nil {
			return err
		}
		if current.String() != old.String() {
			return gitstorage.ErrReferenceHasChanged
		}
		if etag == "" {
			return fmt.Errorf("S3 returned no ETag for %s", old.Name())
		}
		input.IfMatch = aws.String(etag)
	}
	_, err := s.client.PutObject(s.context(), input)
	code := statusCode(err)
	conflict := code == 412 || code == 409
	// A response can be lost after S3 commits. SDK retries then fail the old
	// condition, so reconcile conflicts as well as transport errors. An identical
	// durable ref satisfies this publication; a different successor never does.
	if err != nil && (code == 0 || conflict) && s.context().Err() == nil {
		current, readErr := s.Reference(next.Name())
		if readErr == nil && current.String() == next.String() {
			return nil
		}
	}
	if conflict {
		return gitstorage.ErrReferenceHasChanged
	}
	return err
}
func (s *S3Storer) RemoveReference(name plumbing.ReferenceName) error {
	return fmt.Errorf("reference deletion disabled: %s; publish a revert commit instead", name)
}

// EnsureRepository initializes metadata without changing an existing repository.
func (s *S3Storer) EnsureRepository(branch plumbing.ReferenceName) error {
	if !branch.IsBranch() {
		return fmt.Errorf("branch required")
	}
	if err := branch.Validate(); err != nil {
		return err
	}
	_, err := s.client.PutObject(s.context(), &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.getObjectKey("config")), Body: strings.NewReader("[core]\n\trepositoryformatversion = 0\n\tbare = true\n"), IfNoneMatch: aws.String("*")})
	if err != nil && statusCode(err) != 412 {
		return err
	}
	head, err := s.Reference(plumbing.HEAD)
	if err == nil {
		if head.Type() != plumbing.SymbolicReference || head.Target() != branch {
			return fmt.Errorf("HEAD points to a different branch")
		}
		return nil
	}
	if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	err = s.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch))
	if errors.Is(err, gitstorage.ErrReferenceHasChanged) {
		head, readErr := s.Reference(plumbing.HEAD)
		if readErr == nil && head.Type() == plumbing.SymbolicReference && head.Target() == branch {
			return nil
		}
	}
	return err
}
