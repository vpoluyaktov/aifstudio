package store_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"aifstudio/internal/store"
)

// newTestBlobStore creates a LocalBlobStore rooted at a temp directory.
func newTestBlobStore(t *testing.T) *store.LocalBlobStore {
	t.Helper()
	return store.NewLocalBlobStore(t.TempDir())
}

// ── UploadBlob ────────────────────────────────────────────────────────────────

func TestLocalBlobUploadAndDownload(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	content := []byte("hello, blob storage!")
	if err := b.UploadBlob(ctx, "sessions/r-001/story.z5", "application/octet-stream", bytes.NewReader(content)); err != nil {
		t.Fatalf("UploadBlob: %v", err)
	}

	var buf bytes.Buffer
	if err := b.DownloadBlob(ctx, "sessions/r-001/story.z5", &buf); err != nil {
		t.Fatalf("DownloadBlob: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Errorf("content mismatch: got %q; want %q", buf.Bytes(), content)
	}
}

func TestLocalBlobUpload_NestedDirectories(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	// Deeply nested path — all intermediate directories must be created.
	path := "builds/b-001/stage/substage/artifact.ulx"
	if err := b.UploadBlob(ctx, path, "", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("UploadBlob nested: %v", err)
	}

	var buf bytes.Buffer
	if err := b.DownloadBlob(ctx, path, &buf); err != nil {
		t.Fatalf("DownloadBlob nested: %v", err)
	}
	if buf.String() != "data" {
		t.Errorf("content = %q; want data", buf.String())
	}
}

func TestLocalBlobUpload_Overwrite(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	path := "sessions/r-overwrite/game.sav"
	_ = b.UploadBlob(ctx, path, "", bytes.NewReader([]byte("v1")))
	_ = b.UploadBlob(ctx, path, "", bytes.NewReader([]byte("v2")))

	var buf bytes.Buffer
	_ = b.DownloadBlob(ctx, path, &buf)
	if buf.String() != "v2" {
		t.Errorf("after overwrite: %q; want v2", buf.String())
	}
}

func TestLocalBlobUpload_EmptyContent(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	// Uploading an empty reader should succeed (zero-byte file).
	if err := b.UploadBlob(ctx, "sessions/r-empty/file.txt", "", bytes.NewReader(nil)); err != nil {
		t.Fatalf("UploadBlob empty: %v", err)
	}

	var buf bytes.Buffer
	if err := b.DownloadBlob(ctx, "sessions/r-empty/file.txt", &buf); err != nil {
		t.Fatalf("DownloadBlob empty: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty file, got %d bytes", buf.Len())
	}
}

// ── DownloadBlob ──────────────────────────────────────────────────────────────

func TestLocalBlobDownload_NotFound(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	err := b.DownloadBlob(ctx, "sessions/r-missing/story.z5", io.Discard)
	if err == nil {
		t.Fatal("expected error for missing blob, got nil")
	}
}

// ── DeleteBlobPrefix ──────────────────────────────────────────────────────────

func TestLocalBlobDeleteBlobPrefix(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	// Upload several files under the same prefix.
	files := []string{
		"sessions/r-002/story.z5",
		"sessions/r-002/game.sav",
		"sessions/r-002/transcript.txt",
	}
	for _, f := range files {
		_ = b.UploadBlob(ctx, f, "", bytes.NewReader([]byte("data")))
	}

	n, err := b.DeleteBlobPrefix(ctx, "sessions/r-002/")
	if err != nil {
		t.Fatalf("DeleteBlobPrefix: %v", err)
	}
	if n != len(files) {
		t.Errorf("deleted = %d; want %d", n, len(files))
	}

	// Verify all blobs are gone.
	for _, f := range files {
		if err := b.DownloadBlob(ctx, f, io.Discard); err == nil {
			t.Errorf("expected %q to be deleted, but DownloadBlob succeeded", f)
		}
	}
}

func TestLocalBlobDeleteBlobPrefix_NonExistent(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	// Deleting a non-existent prefix should return 0 without error (idempotent).
	n, err := b.DeleteBlobPrefix(ctx, "sessions/r-never-existed/")
	if err != nil {
		t.Fatalf("DeleteBlobPrefix non-existent: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted = %d; want 0 for non-existent prefix", n)
	}
}

func TestLocalBlobDeleteBlobPrefix_Empty(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	// Upload one file then delete — count should be 1.
	_ = b.UploadBlob(ctx, "sessions/r-single/story.z5", "", bytes.NewReader([]byte("x")))
	n, err := b.DeleteBlobPrefix(ctx, "sessions/r-single/")
	if err != nil {
		t.Fatalf("DeleteBlobPrefix single: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d; want 1", n)
	}
}

// ── Path traversal safety ─────────────────────────────────────────────────────

func TestLocalBlobPathTraversalRejected(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	adversarial := []string{
		"../etc/passwd",
		"sessions/../../../etc/passwd",
		"sessions/r-001/../../../secret",
	}

	for _, path := range adversarial {
		t.Run(path, func(t *testing.T) {
			err := b.UploadBlob(ctx, path, "", bytes.NewReader([]byte("evil")))
			if err == nil {
				t.Errorf("UploadBlob(%q): expected path traversal error, got nil", path)
			}
			// The error message should mention traversal or path.
			if !strings.Contains(err.Error(), "traversal") && !strings.Contains(err.Error(), "escapes") {
				t.Logf("UploadBlob(%q) error: %v (acceptable, just not descriptive)", path, err)
			}

			err = b.DownloadBlob(ctx, path, io.Discard)
			if err == nil {
				t.Errorf("DownloadBlob(%q): expected path traversal error, got nil", path)
			}

			_, err = b.DeleteBlobPrefix(ctx, path)
			if err == nil {
				t.Errorf("DeleteBlobPrefix(%q): expected path traversal error, got nil", path)
			}
		})
	}
}

// TestLocalBlobStoragePath verifies the storage root is correctly set.
// This is a compile-time check via NewLocalBlobStore.
func TestLocalBlobNewStore(t *testing.T) {
	dir := t.TempDir()
	b := store.NewLocalBlobStore(dir)
	if b == nil {
		t.Fatal("NewLocalBlobStore returned nil")
	}

	// Should be able to upload/download without error.
	ctx := context.Background()
	if err := b.UploadBlob(ctx, "test.txt", "", bytes.NewReader([]byte("ok"))); err != nil {
		t.Fatalf("UploadBlob after NewLocalBlobStore: %v", err)
	}

	var buf bytes.Buffer
	if err := b.DownloadBlob(ctx, "test.txt", &buf); err != nil {
		t.Fatalf("DownloadBlob after NewLocalBlobStore: %v", err)
	}
	if buf.String() != "ok" {
		t.Errorf("content = %q; want ok", buf.String())
	}
}

// TestLocalBlobDeleteBlobPrefix_Idempotent verifies that calling DeleteBlobPrefix
// twice on the same prefix does not error.
func TestLocalBlobDeleteBlobPrefix_Idempotent(t *testing.T) {
	b := newTestBlobStore(t)
	ctx := context.Background()

	_ = b.UploadBlob(ctx, "sessions/r-idem/story.z5", "", bytes.NewReader([]byte("x")))
	_, _ = b.DeleteBlobPrefix(ctx, "sessions/r-idem/")

	// Second call on already-deleted prefix should not error.
	n, err := b.DeleteBlobPrefix(ctx, "sessions/r-idem/")
	if err != nil {
		t.Fatalf("second DeleteBlobPrefix: %v", err)
	}
	if n != 0 {
		t.Errorf("second delete: %d; want 0", n)
	}
}

// TestLocalBlobAbsPath verifies that blob files land under the correct root.
// Indirectly tested via upload + download cycle; this checks the path is sane.
func TestLocalBlobUploadPath(t *testing.T) {
	root := t.TempDir()
	b := store.NewLocalBlobStore(root)
	ctx := context.Background()

	path := "sessions/r-003/story.ulx"
	content := []byte("story binary")
	_ = b.UploadBlob(ctx, path, "application/octet-stream", bytes.NewReader(content))

	// The file should exist at root/sessions/r-003/story.ulx.
	absPath := filepath.Join(root, "sessions", "r-003", "story.ulx")
	// We verify indirectly: DownloadBlob should return the correct bytes.
	var buf bytes.Buffer
	if err := b.DownloadBlob(ctx, path, &buf); err != nil {
		t.Fatalf("DownloadBlob after path check: %v (expected at %s)", err, absPath)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Error("content mismatch after path check")
	}
}
