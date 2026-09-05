package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gitstorage "github.com/go-git/go-git/v5/storage"
	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestConcurrentReferencePublication(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprint("create=", create), func(t *testing.T) {
			f := testutil.NewS3(t)
			key := "b/repo.git/refs/heads/main"
			old := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash(strings.Repeat("1", 40)))
			if !create {
				f.Put(key, []byte(old.Hash().String()), "")
			}
			var n atomic.Int32
			barrier := make(chan struct{})
			f.Before = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/refs/heads/main") {
					if n.Add(1) == 2 {
						close(barrier)
					}
					<-barrier
				}
				return false
			}
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, digit := range []string{"2", "3"} {
				wg.Add(1)
				go func(d string) {
					defer wg.Done()
					s := NewS3Storer(f.Client(), "b", "repo.git", zerolog.Nop())
					expected := old
					if create {
						expected = nil
					}
					results <- s.CheckAndSetReference(plumbing.NewHashReference(old.Name(), plumbing.NewHash(strings.Repeat(d, 40))), expected)
				}(digit)
			}
			wg.Wait()
			close(results)
			wins := 0
			for err := range results {
				if err == nil {
					wins++
				}
			}
			require.Equal(t, 1, wins, "exactly one independent writer may commit")
		})
	}
}
func TestReferenceErrorsAreNotAbsence(t *testing.T) {
	f := testutil.NewS3(t)
	f.Before = func(w http.ResponseWriter, r *http.Request) bool { w.WriteHeader(403); return true }
	s := NewS3Storer(f.Client(), "b", "repo.git", zerolog.Nop())
	_, err := s.Reference("refs/heads/main")
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
}
func TestReferenceDeletionDisabled(t *testing.T) {
	f := testutil.NewS3(t)
	f.Put("b/repo.git/refs/heads/main", []byte(strings.Repeat("1", 40)), "")
	s := NewS3Storer(f.Client(), "b", "repo.git", zerolog.Nop())
	require.Error(t, s.RemoveReference("refs/heads/main"))
	_, err := s.Reference("refs/heads/main")
	require.NoError(t, err)
}

func TestCreateRepositoryRecoversPartialInitialization(t *testing.T) {
	f := testutil.NewS3(t)
	s := &S3Storage{Logger: zerolog.Nop(), client: f.Client(), bucket: "b"}
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/refs/heads/main") {
			w.WriteHeader(503)
			return true
		}
		return false
	}
	require.Error(t, s.CreateRepository("flags"))
	f.Before = nil
	require.NoError(t, s.CreateRepository("flags"))
	st, err := s.GetStorer("flags")
	require.NoError(t, err)
	head, err := st.Reference("refs/heads/main")
	require.NoError(t, err)
	require.Error(t, s.CreateRepository("flags"))
	after, err := st.Reference("refs/heads/main")
	require.NoError(t, err)
	require.Equal(t, head.String(), after.String())
}
func TestCreateRepositoryCannotTreatForbiddenAsMissing(t *testing.T) {
	f := testutil.NewS3(t)
	s := &S3Storage{Logger: zerolog.Nop(), client: f.Client(), bucket: "b"}
	var writes atomic.Int32
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "GET" || r.Method == "HEAD" {
			w.WriteHeader(403)
			return true
		}
		if r.Method == "PUT" {
			writes.Add(1)
		}
		return false
	}
	require.Error(t, s.CreateRepository("flags"))
	require.Zero(t, writes.Load(), "a denied read cannot authorize initialization")
}

func TestBootstrapSeedsOnceAndRecovers(t *testing.T) {
	f := testutil.NewS3(t)
	s := &S3Storage{Logger: zerolog.Nop(), client: f.Client(), bucket: "b"}
	ctx := context.Background()
	files := map[string][]byte{"default/features.yaml": []byte("flag: false")}
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/refs/heads/main") {
			w.WriteHeader(503)
			return true
		}
		return false
	}
	_, err := s.Bootstrap(ctx, "flags", "main", files)
	require.Error(t, err)
	f.Before = nil
	h, err := s.Bootstrap(ctx, "flags", "main", files)
	require.NoError(t, err)
	// Simulate a later user edit and retry the same bootstrap with different seeds.
	st, err := s.GetStorer("flags")
	require.NoError(t, err)
	commit, err := object.GetCommit(st, h)
	require.NoError(t, err)
	commit.Message = "User edit"
	commit.ParentHashes = []plumbing.Hash{h}
	encoded := st.NewEncodedObject()
	require.NoError(t, commit.Encode(encoded))
	edited, err := st.SetEncodedObject(encoded)
	require.NoError(t, err)
	require.NoError(t, st.CheckAndSetReference(plumbing.NewHashReference("refs/heads/main", edited), plumbing.NewHashReference("refs/heads/main", h)))
	again, err := s.Bootstrap(ctx, "flags", "main", map[string][]byte{"default/features.yaml": []byte("different")})
	require.NoError(t, err)
	require.Equal(t, edited, again)
	c, err := object.GetCommit(st, again)
	require.NoError(t, err)
	file, err := c.File("default/features.yaml")
	require.NoError(t, err)
	body, err := file.Contents()
	require.NoError(t, err)
	require.Equal(t, "flag: false", body)
}

type forgedObject struct {
	plumbing.EncodedObject
	hash plumbing.Hash
}

func (o forgedObject) Hash() plumbing.Hash { return o.hash }
func TestObjectWriteCannotCorruptExistingHash(t *testing.T) {
	f := testutil.NewS3(t)
	st := NewS3Storer(f.Client(), "b", "repo.git", zerolog.Nop())
	original := st.NewEncodedObject()
	original.SetType(plumbing.BlobObject)
	w, _ := original.Writer()
	_, _ = w.Write([]byte("original"))
	require.NoError(t, w.Close())
	hash, err := st.SetEncodedObject(original)
	require.NoError(t, err)
	replacement := st.NewEncodedObject()
	replacement.SetType(plumbing.BlobObject)
	w, _ = replacement.Writer()
	_, _ = w.Write([]byte("replacement"))
	require.NoError(t, w.Close())
	_, err = st.SetEncodedObject(forgedObject{replacement, hash})
	require.Error(t, err)
	saved, err := st.EncodedObject(plumbing.BlobObject, hash)
	require.NoError(t, err)
	r, err := saved.Reader()
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	body, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "original", string(body))
}

func TestReferencePublicationResolvesLostResponse(t *testing.T) {
	f := testutil.NewS3(t)
	st := NewS3Storer(f.Client(), "b", "repo.git", zerolog.Nop())
	old := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash(strings.Repeat("a", 40)))
	next := plumbing.NewHashReference(old.Name(), plumbing.NewHash(strings.Repeat("b", 40)))
	require.NoError(t, st.SetReference(old))
	f.Before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PUT" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				panic(err)
			}
			f.Put(strings.TrimPrefix(r.URL.Path, "/"), body, "")
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				panic(err)
			}
			_ = conn.Close()
			return true
		}
		return false
	}
	require.NoError(t, st.CheckAndSetReference(next, old))
	head, err := st.Reference(old.Name())
	require.NoError(t, err)
	require.Equal(t, next.String(), head.String())
}

func TestReferencePublicationResolvesLostResponseWithSDKRetries(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, superseded := range []bool{false, true} {
			t.Run(fmt.Sprintf("create=%t/superseded=%t", create, superseded), func(t *testing.T) {
				f := testutil.NewS3(t)
				client, err := NewClient(context.Background(), "us-east-1", f.Server.URL, "test", "test", "")
				require.NoError(t, err)
				st := NewS3Storer(client, "b", "repo.git", zerolog.Nop())
				old := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash(strings.Repeat("a", 40)))
				next := plumbing.NewHashReference(old.Name(), plumbing.NewHash(strings.Repeat("b", 40)))
				durable := next
				if superseded {
					durable = plumbing.NewHashReference(old.Name(), plumbing.NewHash(strings.Repeat("c", 40)))
				}
				if create {
					old = nil
				} else {
					require.NoError(t, st.SetReference(old))
				}
				var attempts atomic.Int32
				f.Before = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != "PUT" || attempts.Add(1) != 1 {
						return false
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						panic(err)
					}
					key := strings.TrimPrefix(r.URL.Path, "/")
					f.Put(key, body, "")
					if superseded {
						// Another writer advances the ref before our SDK retries the lost response.
						f.Put(key, []byte(durable.Hash().String()), "")
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						panic(err)
					}
					_ = conn.Close()
					return true
				}
				publishErr := st.CheckAndSetReference(next, old)
				head, err := st.Reference(next.Name())
				require.NoError(t, err)
				require.Equal(t, durable.String(), head.String(), "retries must not overwrite durable state")
				require.GreaterOrEqual(t, attempts.Load(), int32(2), "exercise production SDK retry after lost response")
				if superseded {
					require.ErrorIs(t, publishErr, gitstorage.ErrReferenceHasChanged)
				} else {
					require.NoError(t, publishErr, "a durable successor resolves the retried publication")
				}
			})
		}
	}
}
