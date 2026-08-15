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

// Open streams the stored object back. The ETag handle is not usable as a
// locator on S3, so the key is what finds the object.
func (d *S3Destination) Open(ctx context.Context, key, _ string) (io.ReadCloser, error) {
	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.info.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s (%s/%s): %w", d.info.Alias, d.info.Bucket, key, friendly(err))
	}
	return out.Body, nil
}

// Verify downloads the object and hashes it.
//
// A HEAD would be cheaper, and that is what this used to do — comparing the
// length and the sha256 user-metadata value. But that metadata is a string
// feather set on the way up, so comparing it to feather's own variable tests
// nothing about the bytes that landed, and a service that drops user metadata
// (some S3-compatible gateways do, on multipart) turned the check into a
// length comparison that silently passed. Documents were deleted on that.
//
// The HEAD is still worth issuing first: it fails fast and cheaply on a
// missing or truncated object, and it is where the storage class is checked.
func (d *S3Destination) Verify(ctx context.Context, obj StoredObject) error {
	head, err := d.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.info.Bucket),
		Key:    aws.String(obj.Key),
	})
	if err != nil {
		return fmt.Errorf("verifying %s (%s/%s): %w", d.info.Alias, d.info.Bucket, obj.Key, friendly(err))
	}
	if got := aws.ToInt64(head.ContentLength); got != obj.Size {
		return fmt.Errorf("verifying %s (%s/%s): stored object is %d bytes, expected %d",
			d.info.Alias, d.info.Bucket, obj.Key, got, obj.Size)
	}

	// An archived-tier object answers HEAD but cannot be read without a restore
	// job that takes hours. Saying so here is much kinder than a download that
	// fails with InvalidObjectState.
	if class := string(head.StorageClass); isArchivalClass(class) {
		return fmt.Errorf("verifying %s (%s/%s): the object is in storage class %s and cannot be read back without a restore, so this copy cannot be verified",
			d.info.Alias, d.info.Bucket, obj.Key, class)
	}

	return VerifyByReading(ctx, d, obj)
}

// isArchivalClass reports whether objects in this storage class need an
// asynchronous restore before they can be read.
func isArchivalClass(class string) bool {
	switch strings.ToUpper(strings.TrimSpace(class)) {
	case "GLACIER", "DEEP_ARCHIVE", "GLACIER_IR", "ARCHIVE":
		// GLACIER_IR is readable immediately, but only on AWS; the compatible
		// services that borrow the name are not consistent about it. Treating
		// it as archival costs a config change and prevents a false verify.
		return true
	default:
		return false
	}
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
