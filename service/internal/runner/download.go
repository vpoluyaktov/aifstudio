// Package runner manages game subprocess sessions and WebSocket bridging.
package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const downloadSizeCap = 50 * 1024 * 1024 // 50 MiB

// ErrDownloadTooLarge is returned when an artifact exceeds the size cap.
type ErrDownloadTooLarge struct{ Size int64 }

func (e ErrDownloadTooLarge) Error() string {
	return fmt.Sprintf("artifact exceeds size limit (got %d bytes)", e.Size)
}

// IsDownloadTooLarge reports whether err is an ErrDownloadTooLarge.
func IsDownloadTooLarge(err error) bool {
	var e ErrDownloadTooLarge
	return errors.As(err, &e)
}

// ErrUpstreamHTTP is returned when the artifact host responds with a non-2xx status.
type ErrUpstreamHTTP struct{ StatusCode int }

func (e ErrUpstreamHTTP) Error() string {
	return fmt.Sprintf("upstream %d fetching artifact", e.StatusCode)
}

// IsUpstreamHTTPError reports whether err is an ErrUpstreamHTTP.
func IsUpstreamHTTPError(err error) bool {
	var e ErrUpstreamHTTP
	return errors.As(err, &e)
}

// DownloadArtifact downloads rawURL enforcing cfg.DownloadSizeLimitBytes, writing
// the response body to w. This exported function is primarily used by tests.
func DownloadArtifact(ctx context.Context, rawURL string, cfg Config, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "StoryCloud/0.1 (contact: vpoluyaktov@gmail.com)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch artifact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrUpstreamHTTP{StatusCode: resp.StatusCode}
	}

	limit := cfg.DownloadSizeLimitBytes
	if limit <= 0 {
		limit = downloadSizeCap
	}

	// Check Content-Length pre-flight.
	if cl := resp.ContentLength; cl > 0 && cl > limit {
		return ErrDownloadTooLarge{Size: cl}
	}

	// Stream with LimitReader: cap at limit+1 to detect oversize.
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if n > limit {
		return ErrDownloadTooLarge{Size: n}
	}
	if n == 0 {
		return fmt.Errorf("artifact empty")
	}
	return nil
}

// downloadArtifact fetches rawURL to a temp file in destDir.
// Returns the local path, file extension, and SHA-256 hex of the content.
func downloadArtifact(rawURL, destDir string, sizeLimitBytes int64) (localPath, ext, sha256hex string, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "StoryCloud/0.1 (contact: vpoluyaktov@gmail.com)")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch artifact: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", ErrUpstreamHTTP{StatusCode: resp.StatusCode}
	}

	// Check Content-Length pre-flight.
	if cl := resp.ContentLength; cl > sizeLimitBytes {
		return "", "", "", ErrDownloadTooLarge{Size: cl}
	}

	// Infer extension from URL path. Prefer the final URL after any redirect
	// (e.g. IFDB download links often redirect to the actual archive URL which
	// has the file extension in the path). Fall back to the original URL if the
	// final URL has no recognisable extension.
	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	urlPath := strings.ToLower(strings.Split(finalURL, "?")[0])
	ext = filepath.Ext(urlPath)
	if ext == "" {
		// Redirect destination had no extension; try the original URL.
		urlPath = strings.ToLower(strings.Split(rawURL, "?")[0])
		ext = filepath.Ext(urlPath)
	}
	if ext == "" {
		ext = ".bin"
	}

	tmpFile, err := os.CreateTemp(destDir, "artifact*"+ext)
	if err != nil {
		return "", "", "", fmt.Errorf("create temp file: %w", err)
	}

	// Stream with LimitReader: cap at sizeLimitBytes+1 to detect oversize.
	lr := io.LimitReader(resp.Body, sizeLimitBytes+1)
	h := sha256.New()
	mw := io.MultiWriter(tmpFile, h)

	n, err := io.Copy(mw, lr)
	tmpFile.Close()
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", "", "", fmt.Errorf("write artifact: %w", err)
	}

	if n > sizeLimitBytes {
		os.Remove(tmpFile.Name())
		return "", "", "", ErrDownloadTooLarge{Size: n}
	}
	if n == 0 {
		os.Remove(tmpFile.Name())
		return "", "", "", fmt.Errorf("artifact empty")
	}

	return tmpFile.Name(), ext, fmt.Sprintf("%x", h.Sum(nil)), nil
}
