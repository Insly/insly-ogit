package testutil

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

// ExternalS3 uses disposable buckets on a caller-provided emulator endpoint.
func ExternalS3(t testing.TB, endpoint string) (*s3.Client, string, string) {
	t.Helper()
	require.NotEmpty(t, endpoint, "FLOCI_ENDPOINT is required for integration tests")
	client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	prefix := fmt.Sprintf("ogit-%d", time.Now().UnixNano())
	eu, us := prefix+"-eu", prefix+"-us"
	for _, bucket := range []string{eu, us} {
		_, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					t.Errorf("cleanup list %s: %v", bucket, err)
					return
				}
				for _, obj := range page.Contents {
					_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: obj.Key})
					if err != nil {
						t.Errorf("cleanup object: %v", err)
					}
				}
			}
			_, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
			if err != nil {
				t.Errorf("cleanup bucket: %v", err)
			}
		})
	}
	return client, eu, us
}
