package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Storage works against any S3-compatible endpoint — AWS S3, Cloudflare R2,
// Hetzner Object Storage, Backblaze B2, MinIO, Wasabi.
type S3Storage struct {
	client *s3.Client
	bucket string
	prefix string
}

// S3Config matches the JSONB blob in the backup_storages.config column.
type S3Config struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	ForcePathStyle  bool   `json:"forcePathStyle"`
	// Prefix is a folder inside the bucket that every key is written under.
	//
	// It was collected by the account backup-storage form, stored in this
	// column and shown back in the UI as "bucket/prefix" for a long time while
	// this struct had no field to unmarshal it into - so it was dropped on
	// load and every archive went to the bucket root, next to whatever else
	// lived there. Optional: empty means the root, which is what those
	// existing rows have actually been doing.
	Prefix string `json:"prefix"`
}

func NewS3(ctx context.Context, raw json.RawMessage) (*S3Storage, error) {
	var cfg S3Config
	if len(raw) == 0 {
		return nil, fmt.Errorf("s3 storage requires config")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid s3 config: %w", err)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 storage requires bucket")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("s3 storage requires access key + secret")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1" // safe default for many providers
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")),
	)
	if err != nil {
		return nil, err
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
		// Do not attach a CRC32 to every request. Since aws-sdk-go-v2 made
		// request checksums the default, a PutObject whose body is neither
		// seekable nor length-known is sent as aws-chunked with the checksum in
		// a TRAILER, and S3-compatible backends that do not implement that
		// trailer read the payload as empty:
		//
		//   BadDigest: The CRC32 checksum you specified did not match what we
		//   received. You provided ... Actual CRC32 was: 00000000
		//
		// Measured against Cloudflare R2. It is not R2-specific - the same
		// default already broke attachment uploads to a plain-HTTP MinIO, which
		// handlers/ticket_attachments.go works around by making that one body
		// seekable. That fix cannot generalise: a backup archive streams from a
		// pipe, so its size is unknowable until it has been written.
		//
		// WhenRequired keeps the checksum for operations that genuinely require
		// one and restores the behaviour every one of these backends was built
		// against. This client is the object store behind BOTH server backups
		// and Core file storage (see storage/s3provider.go), so it is the one
		// place that covers both.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	return &S3Storage{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

func (s *S3Storage) Provider() string { return "s3" }

// key maps a caller's key onto the bucket, under the configured prefix.
// Everything reaching the AWS client goes through here; nothing else in this
// file may build a Key by hand, or one operation ends up addressing a
// different object than the rest.
func (s *S3Storage) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + strings.TrimPrefix(k, "/")
}

// unkey is the inverse, for values coming BACK from the API. A caller that
// wrote "srv-1/x.tar.gz" must read that same string out of List, or retention
// compares prefixed keys against unprefixed ones and matches nothing - which
// would quietly stop pruning instead of failing.
func (s *S3Storage) unkey(k string) string {
	if s.prefix == "" {
		return k
	}
	return strings.TrimPrefix(k, s.prefix+"/")
}

func (s *S3Storage) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
		Body:   r,
	}
	if size > 0 {
		input.ContentLength = aws.Int64(size)
	}
	_, err := s.client.PutObject(ctx, input)
	return err
}

func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		// Treat missing object as a no-op (idempotent delete).
		if isS3NotFound(err) {
			return nil
		}
	}
	return err
}

// isS3NotFound reports whether an AWS API error means the object is not there.
// GetObject and DeleteObject answer "NoSuchKey"; HeadObject answers "NotFound".
// Mirrors the identical helpers in storage/s3provider.go and
// storage/modpack/s3.go - same discrimination, kept per package because the
// three S3 backends deliberately share no code.
func isS3NotFound(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	code := ae.ErrorCode()
	return code == "NoSuchKey" || code == "NotFound"
}

func (s *S3Storage) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.key(prefix)),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			out = append(out, Object{
				Key:          s.unkey(aws.ToString(obj.Key)),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
	}
	return out, nil
}

func (s *S3Storage) Stat(ctx context.Context, key string) (Object, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		// The Storage interface promises a missing key is reported
		// os.ErrNotExist-style, and this returned the raw AWS error, so
		// errors.Is(err, fs.ErrNotExist) was false for the one backend most
		// deployments use - the contract held for LocalStorage only. Wrapped
		// here the same way storage/s3provider.go does it, so a caller can tell
		// "the object is not there" from "the object store did not answer".
		if isS3NotFound(err) {
			return Object{}, fmt.Errorf("s3 Stat %s: %w", key, fs.ErrNotExist)
		}
		return Object{}, err
	}
	_ = types.ObjectStorageClassStandard // pull in types package
	return Object{
		Key:          key,
		Size:         aws.ToInt64(head.ContentLength),
		LastModified: aws.ToTime(head.LastModified),
	}, nil
}

func (s *S3Storage) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	presigner := s3.NewPresignClient(s.client)
	out, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// UploadURL returns a pre-signed PUT URL for `key`. Only Bucket+Key are signed
// (no Content-Type), so the node can PUT the archive with just a Content-Length
// header. Single-PUT max object size is ~5 GiB (S3/R2 limit); larger BYON
// backups would need multipart-presigned (a follow-up).
func (s *S3Storage) UploadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	presigner := s3.NewPresignClient(s.client)
	out, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// Part-size limits every supported backend accepts. R2 is the strictest: 5 MiB
// to 5 GiB per part, and all parts but the last of EQUAL size. AWS S3 does not
// require equal parts, but choosing a fixed size is what works everywhere.
const (
	MultipartMinPartSize int64 = 5 << 20
	MultipartMaxPartSize int64 = 5 << 30
	multipartMaxParts          = 10000
)

func (s *S3Storage) CreateMultipart(ctx context.Context, key string) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		return "", err
	}
	id := aws.ToString(out.UploadId)
	if id == "" {
		return "", fmt.Errorf("s3 CreateMultipartUpload %s: backend returned no upload id", key)
	}
	return id, nil
}

// UploadPartURL presigns one part the same way UploadURL presigns a whole
// object: only bucket, key, part number and upload id are signed, so a node can
// PUT the part with nothing but a Content-Length.
func (s *S3Storage) UploadPartURL(ctx context.Context, key, uploadID string, partNumber int32, ttl time.Duration) (string, error) {
	if partNumber < 1 || partNumber > multipartMaxParts {
		return "", fmt.Errorf("s3 UploadPartURL %s: part number %d outside 1..%d", key, partNumber, multipartMaxParts)
	}
	if uploadID == "" {
		return "", fmt.Errorf("s3 UploadPartURL %s: empty upload id", key)
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	presigner := s3.NewPresignClient(s.client)
	out, err := presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(s.key(key)),
		PartNumber: aws.Int32(partNumber),
		UploadId:   aws.String(uploadID),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

// partInfo is what CompleteMultipart needs to know about one uploaded part.
type partInfo struct {
	Number int32
	Size   int64
}

// validateParts checks a part list, in the order ListParts returned it (S3
// lists ascending), against the fixed part size of the upload.
//
// It runs before CompleteMultipartUpload on purpose. R2 rejects unequal parts
// only at Complete, and a part that is LARGER than agreed means the uploader
// did not cut the archive the way Core told it to, so neither is left for the
// backend to discover.
func validateParts(parts []partInfo, partSize int64) error {
	if partSize < MultipartMinPartSize || partSize > MultipartMaxPartSize {
		return fmt.Errorf("part size %d outside %d..%d bytes", partSize, MultipartMinPartSize, MultipartMaxPartSize)
	}
	if len(parts) == 0 {
		return errors.New("no parts uploaded")
	}
	for i, p := range parts {
		if p.Number != int32(i+1) {
			return fmt.Errorf("part %d found where part %d was expected: parts must be numbered 1..N without gaps", p.Number, i+1)
		}
		last := i == len(parts)-1
		if !last && p.Size != partSize {
			return fmt.Errorf("part %d is %d bytes, want exactly %d: every part but the last must be the agreed size", p.Number, p.Size, partSize)
		}
		if last && p.Size > partSize {
			return fmt.Errorf("last part %d is %d bytes, larger than the agreed part size %d", p.Number, p.Size, partSize)
		}
	}
	return nil
}

func (s *S3Storage) CompleteMultipart(ctx context.Context, key, uploadID string, partSize int64) (int64, error) {
	if uploadID == "" {
		return 0, fmt.Errorf("s3 CompleteMultipart %s: empty upload id", key)
	}
	var (
		infos     []partInfo
		completed []types.CompletedPart
		total     int64
	)
	pager := s3.NewListPartsPaginator(s.client, &s3.ListPartsInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.key(key)),
		UploadId: aws.String(uploadID),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("s3 ListParts %s: %w", key, err)
		}
		for _, p := range page.Parts {
			infos = append(infos, partInfo{Number: aws.ToInt32(p.PartNumber), Size: aws.ToInt64(p.Size)})
			completed = append(completed, types.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag})
			total += aws.ToInt64(p.Size)
		}
	}
	if err := validateParts(infos, partSize); err != nil {
		return 0, fmt.Errorf("s3 CompleteMultipart %s: %w", key, err)
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(s.key(key)),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return 0, fmt.Errorf("s3 CompleteMultipartUpload %s: %w", key, err)
	}
	return total, nil
}

func (s *S3Storage) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if uploadID == "" {
		return fmt.Errorf("s3 AbortMultipart %s: empty upload id", key)
	}
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(s.key(key)),
		UploadId: aws.String(uploadID),
	})
	if err != nil && !isNoSuchUpload(err) {
		return err
	}
	return nil
}

// isNoSuchUpload reports whether err says the upload is already gone (aborted,
// completed, or expired by the bucket's lifecycle), which for an abort is the
// outcome that was asked for.
func isNoSuchUpload(err error) bool {
	var nsu *types.NoSuchUpload
	if errors.As(err, &nsu) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchUpload" {
		return true
	}
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}
