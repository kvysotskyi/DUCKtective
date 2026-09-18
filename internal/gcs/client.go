// Package gcs wraps the GCS operations the app needs, authenticated purely via ADC.
package gcs

import (
	"context"
	"io"
	"sort"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

type BucketInfo struct {
	Name string `json:"name"`
}

type ObjectInfo struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

type Client struct {
	sc *storage.Client
}

func NewClient(ctx context.Context) (*Client, error) {
	sc, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &Client{sc: sc}, nil
}

func (c *Client) Close() error {
	return c.sc.Close()
}

// ListBuckets lists buckets visible to the ADC identity in the given project.
func (c *Client) ListBuckets(ctx context.Context, projectID string) ([]BucketInfo, error) {
	var buckets []BucketInfo
	it := c.sc.Buckets(ctx, projectID)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, BucketInfo{Name: attrs.Name})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	return buckets, nil
}

// ListObjects lists objects under a key prefix, ordered as GCS returns them (lexicographic by name).
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	var objs []ObjectInfo
	it := c.sc.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		objs = append(objs, ObjectInfo{Name: attrs.Name, Size: attrs.Size, LastModified: attrs.Updated})
	}
	return objs, nil
}

// OpenObject streams an object's contents — callers must Close the reader.
func (c *Client) OpenObject(ctx context.Context, bucket, name string) (io.ReadCloser, error) {
	return c.sc.Bucket(bucket).Object(name).NewReader(ctx)
}
