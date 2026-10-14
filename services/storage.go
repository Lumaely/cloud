package services

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type BlobStorage interface {
	Upload(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error
}

type MinIOStorage struct {
	client     *minio.Client
	bucketName string
}

func NewMinIOStorage(endpoint, accessKey, secretKey, bucketName string, useSSL bool) (*MinIOStorage, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}

	storage := &MinIOStorage{
		client:     client,
		bucketName: bucketName,
	}

	ctx := context.Background()
	exists, err := client.BucketExists(ctx, bucketName)
	if err == nil && !exists {
		_ = client.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
	}

	return storage, nil
}

func (m *MinIOStorage) Upload(ctx context.Context, objectKey string, reader io.Reader, size int64, contentType string) error {
	ctxBg := context.Background()
	exists, err := m.client.BucketExists(ctxBg, m.bucketName)
	if err == nil && !exists {
		_ = m.client.MakeBucket(ctxBg, m.bucketName, minio.MakeBucketOptions{})
	}

	_, err = m.client.PutObject(ctx, m.bucketName, objectKey, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}
