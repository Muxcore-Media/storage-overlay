package overlay

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Muxcore-Media/core/pkg/contracts"
)

// S3Config connects the overlay to MinIO or any S3-compatible endpoint.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	Prefix    string
	UseSSL    bool
	PathStyle bool
}

// S3Backend is a StorageProvider backed by MinIO / S3 (laptop path: local MinIO).
type S3Backend struct {
	client *minio.Client
	bucket string
	prefix string
}

// NewS3Backend builds a MinIO/S3 backend. Call EnsureBucket before Put when the bucket may not exist.
func NewS3Backend(cfg S3Config) (*S3Backend, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3 endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}
	endpoint := cfg.Endpoint
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimSuffix(endpoint, "/")

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	return &S3Backend{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
	}, nil
}

func (s *S3Backend) key(k string) string {
	k = strings.TrimPrefix(k, "/")
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

// EnsureBucket creates the bucket if missing (laptop MinIO convenience).
func (s *S3Backend) EnsureBucket(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{})
}

func (s *S3Backend) Put(ctx context.Context, key string, data io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.key(key), data, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	return err
}

func (s *S3Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, mapS3Err(err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, mapS3Err(err)
	}
	return obj, nil
}

func (s *S3Backend) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.key(key), minio.RemoveObjectOptions{})
}

func (s *S3Backend) Move(ctx context.Context, src, dst string) error {
	srcKey, dstKey := s.key(src), s.key(dst)
	_, err := s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: dstKey},
		minio.CopySrcOptions{Bucket: s.bucket, Object: srcKey},
	)
	if err != nil {
		return mapS3Err(err)
	}
	return s.client.RemoveObject(ctx, s.bucket, srcKey, minio.RemoveObjectOptions{})
}

func (s *S3Backend) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, s.key(key), minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3Backend) Stat(ctx context.Context, key string) (contracts.ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.key(key), minio.StatObjectOptions{})
	if err != nil {
		return contracts.ObjectInfo{}, mapS3Err(err)
	}
	return contracts.ObjectInfo{
		Key:          key,
		Size:         info.Size,
		ContentType:  info.ContentType,
		ETag:         info.ETag,
		LastModified: info.LastModified,
		Metadata:     info.UserMetadata,
	}, nil
}

func (s *S3Backend) List(ctx context.Context, prefix string) ([]contracts.ObjectInfo, error) {
	full := s.key(prefix)
	if prefix == "" && s.prefix != "" {
		full = s.prefix + "/"
	}
	var out []contracts.ObjectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: full, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		k := obj.Key
		if s.prefix != "" {
			k = strings.TrimPrefix(k, s.prefix+"/")
		}
		out = append(out, contracts.ObjectInfo{
			Key:          k,
			Size:         obj.Size,
			ETag:         obj.ETag,
			LastModified: obj.LastModified,
		})
	}
	return out, nil
}

func mapS3Err(err error) error {
	if err == nil {
		return nil
	}
	if isS3NotFound(err) {
		return contracts.ErrNotFound
	}
	return err
}

func isS3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NotFound" || resp.StatusCode == 404
}

var _ contracts.StorageProvider = (*S3Backend)(nil)
