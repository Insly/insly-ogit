//go:build integration

package s3

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type barrierTransport struct {
	ready   chan struct{}
	arrived atomic.Int32
}

func (b *barrierTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/refs/heads/main") {
		if b.arrived.Add(1) == 2 {
			close(b.ready)
		}
		select {
		case <-b.ready:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}
func TestFlociConcurrentConditionalPublication(t *testing.T) {
	base, bucket, _ := testutil.ExternalS3(t, os.Getenv("FLOCI_ENDPOINT"))
	for _, create := range []bool{false, true} {
		t.Run(map[bool]string{true: "create", false: "update"}[create], func(t *testing.T) {
			prefix := "repo-update.git"
			if create {
				prefix = "repo-create.git"
			}
			old := plumbing.NewHashReference("refs/heads/main", plumbing.NewHash(strings.Repeat("a", 40)))
			if !create {
				st := NewS3Storer(base, bucket, prefix, zerolog.Nop())
				require.NoError(t, st.SetReference(old))
			}
			barrier := &barrierTransport{ready: make(chan struct{})}
			results := make(chan error, 2)
			for _, d := range []string{"b", "c"} {
				go func(d string) {
					client := awss3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1}, func(o *awss3.Options) {
						o.BaseEndpoint = aws.String(os.Getenv("FLOCI_ENDPOINT"))
						o.UsePathStyle = true
						o.HTTPClient = &http.Client{Transport: barrier}
						o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
					})
					st := NewS3Storer(client, bucket, prefix, zerolog.Nop())
					expected := old
					if create {
						expected = nil
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10e9)
					defer cancel()
					results <- st.WithContext(ctx).CheckAndSetReference(plumbing.NewHashReference(old.Name(), plumbing.NewHash(strings.Repeat(d, 40))), expected)
				}(d)
			}
			wins := 0
			for i := 0; i < 2; i++ {
				if <-results == nil {
					wins++
				}
			}
			require.Equal(t, 1, wins)
			// The stored pointer remains readable via an independent SDK client.
			out, err := base.GetObject(context.Background(), &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix + "/refs/heads/main")})
			require.NoError(t, err)
			require.NoError(t, out.Body.Close())
			require.NotEmpty(t, aws.ToString(out.ETag))
		})
	}
}
