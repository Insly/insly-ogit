package s3

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/labbs/git-server-s3/internal/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultCredentialsAndRefresh(t *testing.T) {
	saved := config.Storage.S3
	t.Cleanup(func() { config.Storage.S3 = saved })
	config.Storage.S3.AccessKey = ""
	config.Storage.S3.SecretKey = ""
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/missing")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/missing")
	var calls atomic.Int32
	role := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		expiry := time.Now().Add(time.Hour)
		if n == 1 {
			expiry = time.Now().Add(-time.Second)
		}
		fmt.Fprintf(w, `{"AccessKeyId":"role%d","SecretAccessKey":"secret","Token":"session%d","Expiration":%q}`, n, n, expiry.UTC().Format(time.RFC3339))
	}))
	defer role.Close()
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", role.URL)
	fixture := testutil.NewS3(t)
	config.Storage.S3.Endpoint = fixture.Server.URL
	config.Storage.S3.Region = "us-east-1"
	var signed []string
	fixture.Before = func(w http.ResponseWriter, r *http.Request) bool {
		signed = append(signed, r.Header.Get("X-Amz-Security-Token"))
		return false
	}
	c := S3Config{Logger: zerolog.Nop()}
	require.NoError(t, c.Configure())
	for i := 0; i < 2; i++ {
		_, err := c.Client.PutObject(context.Background(), &awss3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("probe"), Body: strings.NewReader("ok")})
		require.NoError(t, err)
	}
	require.Equal(t, []string{"session1", "session2"}, signed)
}
func TestPartialExplicitCredentialsRejected(t *testing.T) {
	saved := config.Storage.S3
	t.Cleanup(func() { config.Storage.S3 = saved })
	config.Storage.S3.AccessKey = "only-key"
	config.Storage.S3.SecretKey = ""
	c := S3Config{Logger: zerolog.Nop()}
	require.Error(t, c.Configure())
}
