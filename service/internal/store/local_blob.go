// Package store — local filesystem blob store.
// LocalBlobStore implements the blob-related methods of store.Store using the
// local filesystem rooted at storagePath. It is embedded inside SQLiteStore so
// that the single SQLiteStore type satisfies the full Store interface.
package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalBlobStore implements the filesystem blob operations used by SQLiteStore.
type LocalBlobStore struct {
	storagePath string
}

// NewLocalBlobStore creates a LocalBlobStore rooted at storagePath.
func NewLocalBlobStore(storagePath string) *LocalBlobStore {
	return &LocalBlobStore{storagePath: storagePath}
}

// absPath resolves a logical path (e.g. "sessions/r-123/game.sav") to an
// absolute filesystem path under storagePath. Returns an error if path escapes
// storagePath via ".." traversal.
func (b *LocalBlobStore) absPath(logicalPath string) (string, error) {
	// Reject traversal immediately; Clean won't help against initial "../".
	if strings.Contains(logicalPath, "..") {
		return "", fmt.Errorf("blob path traversal rejected: %q", logicalPath)
	}
	p := filepath.Join(b.storagePath, filepath.FromSlash(logicalPath))
	// Double-check after Join (handles encoded slashes, etc.).
	if !strings.HasPrefix(p, b.storagePath) {
		return "", fmt.Errorf("blob path escapes storage root: %q", logicalPath)
	}
	return p, nil
}

// UploadBlob writes r to the logical path under storagePath using an atomic
// tempfile-then-rename so partial writes are never visible.
func (b *LocalBlobStore) UploadBlob(_ context.Context, path, _ string, r io.Reader) error {
	dst, err := b.absPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdirall %s: %w", filepath.Dir(dst), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op after successful rename
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename blob: %w", err)
	}
	return nil
}

// DownloadBlob copies the blob at path to w. Returns an error if the blob does
// not exist.
func (b *LocalBlobStore) DownloadBlob(_ context.Context, path string, w io.Writer) error {
	src, err := b.absPath(path)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open blob %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("read blob %s: %w", path, err)
	}
	return nil
}

// SignedReadURL is a deprecated no-op stub retained for handler backward
// compatibility. Returns an empty SignedURL.
//
// Deprecated: use same-origin /api/* routes to stream blobs.
func (b *LocalBlobStore) SignedReadURL(_ context.Context, _ string, _ time.Duration) (SignedURL, error) {
	return SignedURL{}, nil
}

// DeleteBlobPrefix removes every file under the given logical path prefix.
// Returns the count of files deleted. Directory entries are also removed if
// they become empty.
func (b *LocalBlobStore) DeleteBlobPrefix(_ context.Context, prefix string) (int, error) {
	dir, err := b.absPath(prefix)
	if err != nil {
		return 0, err
	}
	// If the directory doesn't exist, that's not an error (idempotent).
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return 0, nil
	}
	count := 0
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if removeErr := os.Remove(p); removeErr != nil && !os.IsNotExist(removeErr) {
				return removeErr
			}
			count++
		}
		return nil
	})
	if err != nil {
		return count, fmt.Errorf("walk prefix %s: %w", prefix, err)
	}
	// Best-effort remove of the directory tree.
	_ = os.RemoveAll(dir) //nolint:errcheck
	return count, nil
}
