package modpack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Provider stores .mrpack files in a single S3-compatible bucket. Works
// against AWS, Cloudflare R2, Backblaze B2, MinIO, etc.
type S3Provider struct {
	client *s3.Client
	bucket string
}

// NewS3 builds an S3-backed modpack provider. Region defaults to us-east-1
// when empty (safe default for most providers).
func NewS3(endpoint, region, bucket, accessKey, secretKey string) (*S3Provider, error) {
	if bucket == "" {
		return nil, fmt.Errorf("modpack storage: s3 bucket required")
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("modpack storage: s3 access key + secret required")
	}
	if region == "" {
		region = "us-east-1"
	}

	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("modpack storage: s3 config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		// Same reason as storage/backup/s3.go: the SDK's default request
		// checksum is sent in an aws-chunked trailer for a body of unknown
		// length, and backends that do not implement the trailer reject the
		// upload with BadDigest / "Actual CRC32 was: 00000000".
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	return &S3Provider{client: client, bucket: bucket}, nil
}

func (s *S3Provider) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return fmt.Errorf("modpack storage: s3 put %s: %w", key, err)
	}
	return nil
}

// PutStream uploads size bytes from r with Content-Length set, so the object
// store streams the body instead of Core buffering it. r should be seekable (a
// temp file) so the SDK can rewind on a retry.
func (s *S3Provider) PutStream(ctx context.Context, key string, r io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   r,
	}
	if size > 0 {
		input.ContentLength = aws.Int64(size)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("modpack storage: s3 put-stream %s: %w", key, err)
	}
	return nil
}

func (s *S3Provider) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("modpack storage: s3 get %s: %w", key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("modpack storage: s3 read %s: %w", key, err)
	}
	return data, nil
}

// Stream hands back the GetObject body itself rather than reading it, so the
// object crosses Core as a copy between two sockets instead of landing in the
// heap. ContentLength comes from the same response, so it describes exactly the
// body being returned.
//
// The body is NOT closed here on success - the caller owns it, per the
// interface. It is closed on every error path before returning.
func (s *S3Provider) Stream(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, fmt.Errorf("modpack storage: s3 stream %s: %w", key, err)
	}
	size := SizeUnknown
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// DownloadURL presigns a GET so the client fetches from the object store
// directly and the bytes never enter this process.
//
// A presign is a local signing operation: it contacts nothing and therefore
// cannot tell whether the key exists. A URL for a missing key is issued
// happily and 404s at the object store, which is the same answer the caller
// would have produced anyway.
func (s *S3Provider) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	req, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("modpack storage: s3 presign %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *S3Provider) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil
		}
		return fmt.Errorf("modpack storage: s3 delete %s: %w", key, err)
	}
	return nil
}

func (s *S3Provider) Stat(ctx context.Context, key string) (int64, bool, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("modpack storage: s3 head %s: %w", key, err)
	}
	return aws.ToInt64(head.ContentLength), true, nil
}

// isS3NotFound reports whether an AWS API error indicates a missing object.
// HeadObject returns "NotFound"; GetObject / DeleteObject return "NoSuchKey".
func isS3NotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code := ae.ErrorCode()
		return code == "NoSuchKey" || code == "NotFound"
	}
	return false
}
