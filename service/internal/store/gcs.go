package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// UploadBlob writes r to GCS at path with the given content type.
// Sets CacheControl: "private, no-store" for session objects. Uses uniform bucket-level access.
func (s *FirestoreStore) UploadBlob(ctx context.Context, path, contentType string, r io.Reader) error {
	obj := s.bucket.Object(path)
	wc := obj.NewWriter(ctx)
	wc.ContentType = contentType
	wc.CacheControl = "private, no-store"

	if _, err := io.Copy(wc, r); err != nil {
		wc.Close() //nolint:errcheck
		return fmt.Errorf("upload blob %s: %w", path, err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close blob writer %s: %w", path, err)
	}
	return nil
}

// DownloadBlob reads the GCS object at path and writes it to w.
func (s *FirestoreStore) DownloadBlob(ctx context.Context, path string, w io.Writer) error {
	rc, err := s.bucket.Object(path).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("open blob %s: %w", path, err)
	}
	defer rc.Close()
	if _, err := io.Copy(w, rc); err != nil {
		return fmt.Errorf("read blob %s: %w", path, err)
	}
	return nil
}

// SignedReadURL returns a V4 signed GET URL for the GCS object at path, valid
// for ttl. The runtime SA must have iam.serviceAccounts.signBlob permission.
func (s *FirestoreStore) SignedReadURL(ctx context.Context, path string, ttl time.Duration) (SignedURL, error) {
	expires := time.Now().Add(ttl)
	url, err := s.bucket.SignedURL(path, &storage.SignedURLOptions{
		Method:  "GET",
		Expires: expires,
		Scheme:  storage.SigningSchemeV4,
	})
	if err != nil {
		return SignedURL{}, fmt.Errorf("sign url %s: %w", path, err)
	}
	return SignedURL{URL: url, ExpiresAt: expires}, nil
}

// DeleteBlobPrefix removes every GCS object under the given path prefix.
// Concurrency is capped at 10 in-flight deletes. Returns the count deleted.
// An object listing error aborts early; individual delete errors are logged and skipped.
func (s *FirestoreStore) DeleteBlobPrefix(ctx context.Context, prefix string) (int, error) {
	query := &storage.Query{Prefix: prefix}
	iter := s.bucket.Objects(ctx, query)

	const maxConcurrent = 10
	sem := make(chan struct{}, maxConcurrent)
	var mu sync.Mutex
	var wg sync.WaitGroup
	total := 0
	var firstErr error

	for {
		attrs, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			wg.Wait()
			return total, fmt.Errorf("list objects with prefix %q: %w", prefix, err)
		}

		name := attrs.Name
		sem <- struct{}{}
		wg.Add(1)
		go func(objName string) {
			defer wg.Done()
			defer func() { <-sem }()
			if delErr := s.bucket.Object(objName).Delete(ctx); delErr != nil {
				slog.Error("delete blob failed", "object", objName, "err", delErr)
				mu.Lock()
				if firstErr == nil {
					firstErr = delErr
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			total++
			mu.Unlock()
		}(name)
	}

	wg.Wait()
	return total, firstErr
}

// gcsSourceKey returns the GCS object key for a project's Inform 7 source.
func gcsSourceKey(projectID string) string {
	return "projects/" + projectID + "/source.i7"
}

// gcsAITurnKey returns the GCS object key for an AITurn source snapshot.
// which must be "before" or "after".
func gcsAITurnKey(projectID, turnID, which string) string {
	return "ai-turns/" + projectID + "/" + turnID + "/" + which + ".i7"
}

// writeAITurnBlobs writes SourceBefore and SourceAfter for t to GCS.
// Empty strings are skipped. Returns on the first write failure.
func (s *FirestoreStore) writeAITurnBlobs(ctx context.Context, t *AITurn) error {
	if t.SourceBefore != "" {
		key := gcsAITurnKey(t.ProjectID, t.ID, "before")
		if err := s.UploadBlob(ctx, key, "text/plain; charset=utf-8", bytes.NewBufferString(t.SourceBefore)); err != nil {
			return fmt.Errorf("write sourceBefore blob: %w", err)
		}
	}
	if t.SourceAfter != "" {
		key := gcsAITurnKey(t.ProjectID, t.ID, "after")
		if err := s.UploadBlob(ctx, key, "text/plain; charset=utf-8", bytes.NewBufferString(t.SourceAfter)); err != nil {
			return fmt.Errorf("write sourceAfter blob: %w", err)
		}
	}
	return nil
}

// readAITurnBlobs fetches SourceBefore and SourceAfter from GCS for each turn
// in parallel. Missing blobs are silently treated as empty string. Other errors
// are logged and the field is left empty so the list call can still succeed.
func (s *FirestoreStore) readAITurnBlobs(ctx context.Context, turns []*AITurn) {
	var wg sync.WaitGroup
	for _, t := range turns {
		t := t
		wg.Add(1)
		go func() {
			defer wg.Done()

			fetch := func(key, which string) string {
				rc, err := s.bucket.Object(key).NewReader(ctx)
				if err != nil {
					if !errors.Is(err, storage.ErrObjectNotExist) {
						slog.Warn("readAITurnBlobs: open failed",
							"turn", t.ID, "which", which, "err", err)
					}
					return ""
				}
				defer rc.Close()
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, rc); err != nil {
					slog.Warn("readAITurnBlobs: read failed",
						"turn", t.ID, "which", which, "err", err)
					return ""
				}
				return buf.String()
			}

			t.SourceBefore = fetch(gcsAITurnKey(t.ProjectID, t.ID, "before"), "before")
			t.SourceAfter = fetch(gcsAITurnKey(t.ProjectID, t.ID, "after"), "after")
		}()
	}
	wg.Wait()
}

// GetAITurnAfterSource reads the after.i7 GCS blob for the given AI turn.
// Returns (content, true, nil) on success, ("", false, nil) when absent,
// or ("", false, err) on other errors.
func (s *FirestoreStore) GetAITurnAfterSource(ctx context.Context, projectID, turnID string) (string, bool, error) {
	key := gcsAITurnKey(projectID, turnID, "after")
	rc, err := s.bucket.Object(key).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("open ai_turn after blob %s: %w", key, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return "", false, fmt.Errorf("read ai_turn after blob %s: %w", key, err)
	}
	return buf.String(), true, nil
}

// GetProjectSource returns the current Inform 7 source for a project.
// It reads from GCS first. If the object does not exist and the Firestore doc
// has a legacy "source" field, it migrates the source to GCS (lazy migration
// per §4.4 of ARCHITECTURE_AI_CREATE.md) and clears the Firestore field.
// Returns "" (not an error) when no source exists in either store.
func (s *FirestoreStore) GetProjectSource(ctx context.Context, projectID string) (string, error) {
	key := gcsSourceKey(projectID)

	// Step 1: try GCS first.
	var buf bytes.Buffer
	rc, err := s.bucket.Object(key).NewReader(ctx)
	if err == nil {
		defer rc.Close()
		if _, err := io.Copy(&buf, rc); err != nil {
			return "", fmt.Errorf("read source from GCS %s: %w", key, err)
		}
		return buf.String(), nil
	}
	if !errors.Is(err, storage.ErrObjectNotExist) {
		return "", fmt.Errorf("open source object %s: %w", key, err)
	}

	// Step 2: GCS object absent — check for legacy Firestore source field.
	snap, err := s.fsClient.Collection("projects").Doc(projectID).Get(ctx)
	if err != nil {
		return "", fmt.Errorf("get project doc for migration %s: %w", projectID, err)
	}
	legacySource, ok := snap.Data()["source"].(string)
	if !ok || legacySource == "" {
		// Legitimately empty project — no source in either store.
		return "", nil
	}

	// Step 3: migrate — upload legacy source to GCS.
	wc := s.bucket.Object(key).NewWriter(ctx)
	wc.ContentType = "text/plain; charset=utf-8"
	wc.CacheControl = "private, no-store"
	if _, err := io.Copy(wc, bytes.NewBufferString(legacySource)); err != nil {
		wc.Close() //nolint:errcheck
		return "", fmt.Errorf("upload legacy source for migration %s: %w", projectID, err)
	}
	if err := wc.Close(); err != nil {
		return "", fmt.Errorf("close legacy source upload %s: %w", projectID, err)
	}

	// Step 4: clear the Firestore source field. updatedAt is intentionally unchanged.
	_, err = s.fsClient.Collection("projects").Doc(projectID).Set(ctx, map[string]interface{}{
		"source": firestore.Delete,
	}, firestore.MergeAll)
	if err != nil {
		// Non-fatal: GCS is the source of truth now. Log and continue.
		slog.Warn("migration: failed to clear legacy source field", "projectId", projectID, "err", err)
	}

	return legacySource, nil
}

// PutProjectSource writes source to GCS at projects/{projectID}/source.i7 and
// updates Firestore updatedAt. It also issues firestore.Delete on the legacy
// "source" field so that the migration converges even on write paths that bypassed
// GetProjectSource. If the GCS upload fails, Firestore is not touched.
func (s *FirestoreStore) PutProjectSource(ctx context.Context, projectID, source string, updatedAt time.Time) error {
	key := gcsSourceKey(projectID)

	wc := s.bucket.Object(key).NewWriter(ctx)
	wc.ContentType = "text/plain; charset=utf-8"
	wc.CacheControl = "private, no-store"
	if _, err := io.Copy(wc, bytes.NewBufferString(source)); err != nil {
		wc.Close() //nolint:errcheck
		return fmt.Errorf("upload source %s: %w", key, err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close source writer %s: %w", key, err)
	}

	// Update Firestore metadata and clear legacy source field.
	_, err := s.fsClient.Collection("projects").Doc(projectID).Set(ctx, map[string]interface{}{
		"updatedAt": updatedAt,
		"source":    firestore.Delete,
	}, firestore.MergeAll)
	if err != nil {
		return fmt.Errorf("update project metadata after source write %s: %w", projectID, err)
	}
	return nil
}

// DeleteProjectSource removes the GCS source object for a project.
// Idempotent: deleting a missing object is not an error.
func (s *FirestoreStore) DeleteProjectSource(ctx context.Context, projectID string) error {
	key := gcsSourceKey(projectID)
	err := s.bucket.Object(key).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("delete source object %s: %w", key, err)
	}
	return nil
}

// GetProjectSourceSize returns the byte size of the project's GCS source object.
// exists is false (and err is nil) when the object does not exist.
func (s *FirestoreStore) GetProjectSourceSize(ctx context.Context, projectID string) (int64, bool, error) {
	key := gcsSourceKey(projectID)
	attrs, err := s.bucket.Object(key).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("get source attrs %s: %w", key, err)
	}
	return attrs.Size, true, nil
}

// SignedProjectSourceURL returns a V4 signed read URL for the project's source GCS object.
// Returns empty SignedURL (URL == "") if the object does not exist.
func (s *FirestoreStore) SignedProjectSourceURL(ctx context.Context, projectID string, ttl time.Duration) (SignedURL, error) {
	key := gcsSourceKey(projectID)
	// Verify the object exists before signing (signing works even on missing objects
	// but we want to signal "no source" to callers via empty URL).
	_, exists, err := s.GetProjectSourceSize(ctx, projectID)
	if err != nil {
		return SignedURL{}, err
	}
	if !exists {
		return SignedURL{}, nil
	}
	expires := time.Now().Add(ttl)
	url, err := s.bucket.SignedURL(key, &storage.SignedURLOptions{
		Method:  "GET",
		Expires: expires,
		Scheme:  storage.SigningSchemeV4,
	})
	if err != nil {
		return SignedURL{}, fmt.Errorf("sign source url %s: %w", key, err)
	}
	return SignedURL{URL: url, ExpiresAt: expires}, nil
}
