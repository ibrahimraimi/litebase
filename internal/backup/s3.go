package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	EnvS3Endpoint        = "LITEBASE_S3_ENDPOINT"
	EnvS3Region          = "LITEBASE_S3_REGION"
	EnvS3Bucket          = "LITEBASE_S3_BUCKET"
	EnvS3Prefix          = "LITEBASE_S3_PREFIX"
	EnvS3AccessKeyID     = "LITEBASE_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "LITEBASE_S3_SECRET_ACCESS_KEY"
	EnvBackupDir         = "LITEBASE_BACKUP_DIR"

	defaultMultipartPartSize = 16 * 1024 * 1024
)

type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
}

type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
}

func LoadBlobStoreFromEnv() (BlobStore, error) {
	bucket := strings.TrimSpace(os.Getenv(EnvS3Bucket))
	prefix := strings.Trim(strings.TrimSpace(os.Getenv(EnvS3Prefix)), "/")
	if bucket != "" {
		endpointRaw := strings.TrimSpace(os.Getenv(EnvS3Endpoint))
		if endpointRaw == "" {
			endpointRaw = "s3.amazonaws.com"
		}
		hostPort, useSSL := parseS3Endpoint(endpointRaw)
		return NewS3Store(S3Config{
			Endpoint:        hostPort,
			Region:          strings.TrimSpace(os.Getenv(EnvS3Region)),
			Bucket:          bucket,
			Prefix:          prefix,
			AccessKeyID:     strings.TrimSpace(os.Getenv(EnvS3AccessKeyID)),
			SecretAccessKey: strings.TrimSpace(os.Getenv(EnvS3SecretAccessKey)),
			UseSSL:          useSSL,
		})
	}
	localBackupDir := strings.TrimSpace(os.Getenv(EnvBackupDir))
	if localBackupDir != "" {
		return NewLocalDirStore(localBackupDir, prefix)
	}
	return nil, nil
}

func parseS3Endpoint(raw string) (string, bool) {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parsed, err := url.Parse(raw)
		if err == nil && parsed.Host != "" {
			return parsed.Host, parsed.Scheme == "https"
		}
	}
	useSSL := !strings.HasPrefix(raw, "127.0.0.1") && !strings.HasPrefix(raw, "localhost")
	return raw, useSSL
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("S3 bucket name is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("S3 credentials (LITEBASE_S3_ACCESS_KEY_ID and LITEBASE_S3_SECRET_ACCESS_KEY) are required")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3 client: %w", err)
	}
	return &S3Store{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(strings.TrimSpace(cfg.Prefix), "/"),
	}, nil
}

func (s *S3Store) fullKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return path.Join(s.prefix, key)
}

func (s *S3Store) stripPrefix(full string) (string, bool) {
	if s.prefix == "" {
		return full, true
	}
	withSlash := s.prefix + "/"
	if !strings.HasPrefix(full, withSlash) {
		return "", false
	}
	return strings.TrimPrefix(full, withSlash), true
}

func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string) error {
	if err := ValidateObjectKey(key); err != nil {
		return err
	}
	putSize := size
	if putSize < 0 {
		putSize = -1
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.fullKey(key), r, putSize, minio.PutObjectOptions{
		PartSize:     defaultMultipartPartSize,
		UserMetadata: meta,
	})
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateObjectKey(key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.fullKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, mapS3Error(err)
	}
	if _, statErr := obj.Stat(); statErr != nil {
		_ = obj.Close()
		return nil, mapS3Error(statErr)
	}
	return obj, nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]BlobInfo, error) {
	cleanPrefix := strings.Trim(strings.TrimSpace(prefix), "/")
	if cleanPrefix != "" {
		if err := ValidateObjectKey(cleanPrefix); err != nil {
			return nil, err
		}
	}
	searchPrefix := s.fullKey(cleanPrefix)
	if searchPrefix != "" && !strings.HasSuffix(searchPrefix, "/") {
		searchPrefix += "/"
	}
	ch := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    searchPrefix,
		Recursive: true,
	})
	var items []BlobInfo
	for obj := range ch {
		if obj.Err != nil {
			return nil, obj.Err
		}
		logicalKey, ok := s.stripPrefix(obj.Key)
		if !ok || logicalKey == "" {
			continue
		}
		items = append(items, BlobInfo{
			Key:          logicalKey,
			Size:         obj.Size,
			LastModified: obj.LastModified.UTC(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Key < items[j].Key
	})
	return items, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := ValidateObjectKey(key); err != nil {
		return err
	}
	err := s.client.RemoveObject(ctx, s.bucket, s.fullKey(key), minio.RemoveObjectOptions{})
	if err != nil && !errors.Is(mapS3Error(err), os.ErrNotExist) {
		return err
	}
	return nil
}

func mapS3Error(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.StatusCode == 404 {
		return os.ErrNotExist
	}
	return err
}
