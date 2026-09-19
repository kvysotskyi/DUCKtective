// Package source abstracts "where a wiretap's files live" behind one small interface, so a new
// source type (local filesystem, S3, ...) only has to implement it, never touch the storage layer.
package source

import (
	"context"
	"io"
	"time"
)

type ObjectInfo struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

type Source interface {
	ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error)
	OpenObject(ctx context.Context, name string) (io.ReadCloser, error)
}
