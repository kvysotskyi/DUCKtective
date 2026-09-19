package source

import (
	"context"
	"io"

	"ducktective/internal/gcs"
)

type gcsSource struct {
	client *gcs.Client
	bucket string
}

// NewGCS binds a bucket to an already-authenticated GCS client, satisfying Source.
func NewGCS(client *gcs.Client, bucket string) Source {
	return &gcsSource{client: client, bucket: bucket}
}

func (s *gcsSource) ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	objs, err := s.client.ListObjects(ctx, s.bucket, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectInfo, len(objs))
	for i, o := range objs {
		out[i] = ObjectInfo{Name: o.Name, Size: o.Size, LastModified: o.LastModified}
	}
	return out, nil
}

func (s *gcsSource) OpenObject(ctx context.Context, name string) (io.ReadCloser, error) {
	return s.client.OpenObject(ctx, s.bucket, name)
}
