package s3

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsCfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/metrics/smithyotelmetrics"
	"github.com/aws/smithy-go/tracing/smithyoteltracing"
	"github.com/labbs/git-server-s3/internal/config"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
)

type S3Config struct {
	Logger zerolog.Logger
	Client *awss3.Client
}

// NewClient retains the refreshable AWS default chain unless credentials are explicit.
func NewClient(ctx context.Context, region, endpoint, accessKey, secretKey, sessionToken string) (*awss3.Client, error) {
	if (accessKey == "") != (secretKey == "") || (sessionToken != "" && accessKey == "") {
		return nil, fmt.Errorf("explicit S3 credentials require both access-key and secret-key")
	}
	opts := []func(*awsCfg.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsCfg.WithRegion(region))
	}
	if accessKey != "" {
		opts = append(opts, awsCfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)))
	}
	cfg, err := awsCfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.TracerProvider = smithyoteltracing.Adapt(otel.GetTracerProvider())
		o.MeterProvider = smithyotelmetrics.Adapt(otel.GetMeterProvider())
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	}), nil
}
func (c *S3Config) Configure() error {
	s := config.Storage.S3
	client, err := NewClient(context.Background(), s.Region, s.Endpoint, s.AccessKey, s.SecretKey, s.SessionToken)
	if err != nil {
		return err
	}
	c.Client = client
	return nil
}
