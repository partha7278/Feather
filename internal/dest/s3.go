package dest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/unifers/feather/internal/config"
)

const (
	// 16 MiB parts keep the uploader's buffer footprint modest on a small VPS
	// while still allowing very large archives (10k parts = 160 GiB).
	uploadPartSize    = 16 << 20
	uploadConcurrency = 3

	metaSHA256   = "sha256"
	metaDocCount = "doc-count"
	metaConfig   = "config"
	metaRunID    = "run-id"
	metaColl     = "collection"
)

// S3Destination writes to anything speaking the S3 API: AWS itself, or an
// S3-compatible endpoint such as Oracle Cloud Object Storage or MinIO.
type S3Destination struct {
	info     Info
	client   *s3.Client
	uploader *manager.Uploader
}

// NewS3Destination builds a client for one configured destination.
func NewS3Destination(ctx context.Context, cfg config.Destination) (*S3Destination, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}

	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)))
	}
	// With no explicit keys we fall through to the default provider chain, so an
	// EC2 instance role or a shared profile still works for AWS destinations.

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("destination %s: loading AWS config: %w", cfg.Alias, err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.ForcePathStyle
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.Provider != config.ProviderS3 {
			// The SDK adds x-amz-checksum-crc32 to every upload by default.
			// S3-compatible services that predate that header reject the
			// request outright, so ask for checksums only where the API
			// actually requires them.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	})

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = uploadPartSize
		u.Concurrency = uploadConcurrency
		// Abandoned parts on a failed upload would be billed forever, and the
		// run is going to retry from scratch anyway.
		u.LeavePartsOnError = false
	})

	return &S3Destination{
		info: Info{
			Alias:        cfg.Alias,
			Provider:     string(cfg.Provider),
			Bucket:       cfg.Bucket,
			Region:       cfg.Region,
			Endpoint:     cfg.Endpoint,
			StorageClass: cfg.StorageClass,
			Prefix:       cfg.Prefix,
		},
		client:   client,
		uploader: uploader,
	}, nil
}

func (d *S3Destination) Alias() string { return d.info.Alias }
func (d *S3Destination) Info() Info    { return d.info }

func (d *S3Destination) Check(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(d.info.Bucket)})
	if err != nil {
		return fmt.Errorf("destination %s: cannot access bucket %s: %w", d.info.Alias, d.info.Bucket, friendly(err))
	}
	return nil
}

// Key applies the destination's own prefix to a run-generated object key, so
// two destinations can file the same archive under different layouts.
func (d *S3Destination) Key(key string) string {
	if d.info.Prefix == "" {
		return key
	}
	return strings.TrimSuffix(d.info.Prefix, "/") + "/" + key
}

func (d *S3Destination) Upload(ctx context.Context, key string, body io.Reader, size int64, meta Metadata) (string, error) {
	input := &s3.PutObjectInput{
		Bucket:        aws.String(d.info.Bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentType:   aws.String(contentTypeFor(key)),
		ContentLength: aws.Int64(size),
		Metadata: map[string]string{
			metaSHA256:   meta.SHA256,
			metaDocCount: strconv.FormatInt(meta.DocCount, 10),
			metaConfig:   meta.Config,
			metaRunID:    meta.RunID,
			metaColl:     meta.Collection,
		},
	}
	if d.info.StorageClass != "" {
		input.StorageClass = types.StorageClass(d.info.StorageClass)
	}

	out, err := d.uploader.Upload(ctx, input)
	if err != nil {
		return "", fmt.Errorf("uploading to %s (%s/%s): %w", d.info.Alias, d.info.Bucket, key, friendly(err))
	}
	return aws.ToString(out.ETag), nil
}

// Verify re-reads the object's metadata. Size is compared always; the checksum
// is compared when the service returned the metadata we set. ETag is
// deliberately not used — for a multipart upload it is not the object's MD5.
func (d *S3Destination) Verify(ctx context.Context, key string, wantSize int64, wantSHA256 string) error {
	head, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.info.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("verifying %s (%s/%s): %w", d.info.Alias, d.info.Bucket, key, friendly(err))
	}

	if got := aws.ToInt64(head.ContentLength); got != wantSize {
		return fmt.Errorf("verifying %s (%s/%s): stored object is %d bytes, expected %d", d.info.Alias, d.info.Bucket, key, got, wantSize)
	}

	// Metadata keys come back lowercased; look the value up case-insensitively
	// rather than trusting a particular service's normalisation.
	for k, v := range head.Metadata {
		if strings.EqualFold(k, metaSHA256) {
			if !strings.EqualFold(v, wantSHA256) {
				return fmt.Errorf("verifying %s (%s/%s): stored checksum %s does not match %s", d.info.Alias, d.info.Bucket, key, v, wantSHA256)
			}
			return nil
		}
	}
	return nil
}

func contentTypeFor(key string) string {
	switch {
	case strings.HasSuffix(key, ".zst"):
		return "application/zstd"
	case strings.HasSuffix(key, ".gz"):
		return "application/gzip"
	default:
		return "application/octet-stream"
	}
}

// friendly unwraps the SDK's error envelope so logs show the actual service
// code (NoSuchBucket, AccessDenied, SignatureDoesNotMatch) rather than a wall
// of operation metadata.
func friendly(err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %s", apiErr.ErrorCode(), apiErr.ErrorMessage())
	}
	return err
}
