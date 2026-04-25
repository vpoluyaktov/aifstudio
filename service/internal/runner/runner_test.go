// Package runner_test tests the exported runner API: download size limits,
// zip bomb extraction guards, session management, and error sentinel types.
//
// Unexported functions (downloadArtifact, extractZip internal call path) are
// covered indirectly via the exported wrappers DownloadArtifact and ExtractZip.
package runner_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aifstudio/internal/runner"
	"aifstudio/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: newTestManager creates a Manager with nil store (cleanupLoop guards
// st != nil before calling, so nil is safe for unit tests).
// ─────────────────────────────────────────────────────────────────────────────

func newTestManager(t *testing.T) *runner.Manager {
	t.Helper()
	return runner.NewManager(nil, runner.DefaultConfig())
}

// ─────────────────────────────────────────────────────────────────────────────
// DefaultConfig — ARCHITECTURE.md §9.3
// ─────────────────────────────────────────────────────────────────────────────

func TestDefaultConfigValues(t *testing.T) {
	cfg := runner.DefaultConfig()
	if cfg.DownloadSizeLimitBytes != 50*1024*1024 {
		t.Errorf("DownloadSizeLimitBytes = %d; want 52428800 (50 MiB)", cfg.DownloadSizeLimitBytes)
	}
	if cfg.MaxExtractFiles != 100 {
		t.Errorf("MaxExtractFiles = %d; want 100", cfg.MaxExtractFiles)
	}
	if cfg.SessionMax <= 0 {
		t.Errorf("SessionMax = %v; want positive duration", cfg.SessionMax)
	}
	if cfg.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v; want positive duration", cfg.IdleTimeout)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Session management — ARCHITECTURE.md §3.4
// ─────────────────────────────────────────────────────────────────────────────

func TestManagerCreateAndGetSession(t *testing.T) {
	mgr := newTestManager(t)

	run := &store.Run{
		ID:         "r-00000000000000000000000001",
		SourceType: "url",
		Status:     "pending",
		CreatedAt:  time.Now(),
	}

	sess := mgr.CreateSession(run)
	if sess == nil {
		t.Fatal("CreateSession returned nil")
	}

	got := mgr.GetSession(run.ID)
	if got == nil {
		t.Fatal("GetSession returned nil after CreateSession")
	}
	if got != sess {
		t.Error("GetSession returned different session pointer than CreateSession")
	}
}

func TestManagerGetSessionUnknown(t *testing.T) {
	mgr := newTestManager(t)
	sess := mgr.GetSession("r-00000000000000000000000099")
	if sess != nil {
		t.Errorf("GetSession(unknown) = %v; want nil", sess)
	}
}

func TestManagerMultipleSessionsIndependent(t *testing.T) {
	mgr := newTestManager(t)

	run1 := &store.Run{ID: "r-00000000000000000000000001", SourceType: "url", Status: "pending", CreatedAt: time.Now()}
	run2 := &store.Run{ID: "r-00000000000000000000000002", SourceType: "ifdb", Status: "pending", CreatedAt: time.Now()}

	mgr.CreateSession(run1)
	mgr.CreateSession(run2)

	if mgr.GetSession(run1.ID) == nil {
		t.Error("session 1 not found")
	}
	if mgr.GetSession(run2.ID) == nil {
		t.Error("session 2 not found")
	}
	if mgr.GetSession(run1.ID) == mgr.GetSession(run2.ID) {
		t.Error("different runs must have different sessions")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Session status constants — ARCHITECTURE.md §4.2
// ─────────────────────────────────────────────────────────────────────────────

func TestStatusConstants(t *testing.T) {
	tests := []struct {
		got  string
		want string
	}{
		{runner.StatusPending, "pending"},
		{runner.StatusRunning, "running"},
		{runner.StatusSuspended, "suspended"},
		{runner.StatusFinished, "finished"},
		{runner.StatusFailed, "failed"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("status constant = %q; want %q", tt.got, tt.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ErrDownloadTooLarge — ARCHITECTURE.md §9.3
// ─────────────────────────────────────────────────────────────────────────────

func TestErrDownloadTooLargeType(t *testing.T) {
	err := runner.ErrDownloadTooLarge{Size: 50*1024*1024 + 1}
	if err.Error() == "" {
		t.Error("ErrDownloadTooLarge.Error() returned empty string")
	}
	if !runner.IsDownloadTooLarge(err) {
		t.Error("IsDownloadTooLarge(ErrDownloadTooLarge{}) = false; want true")
	}
}

func TestIsDownloadTooLargeFalseForOtherErrors(t *testing.T) {
	if runner.IsDownloadTooLarge(fmt.Errorf("some other error")) {
		t.Error("IsDownloadTooLarge(generic error) = true; want false")
	}
	if runner.IsDownloadTooLarge(nil) {
		t.Error("IsDownloadTooLarge(nil) = true; want false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DownloadArtifact — ARCHITECTURE.md §9.3
// ─────────────────────────────────────────────────────────────────────────────

const fiftyMiB = 50 * 1024 * 1024

func TestDownloadSizeLimitContentLengthExceeded(t *testing.T) {
	bigSize := int64(fiftyMiB + 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", bigSize))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := runner.DefaultConfig()
	cfg.DownloadSizeLimitBytes = fiftyMiB

	err := runner.DownloadArtifact(context.Background(), srv.URL, cfg, io.Discard)
	if err == nil {
		t.Error("DownloadArtifact with Content-Length > 50 MiB should return error")
	}
	if !runner.IsDownloadTooLarge(err) {
		t.Errorf("error type: got %T (%v); want ErrDownloadTooLarge", err, err)
	}
}

func TestDownloadSizeLimitStreamExceeded(t *testing.T) {
	sixtyMiB := fiftyMiB + 10*1024*1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		buf := make([]byte, 65536)
		written := 0
		for written < sixtyMiB {
			n := len(buf)
			if written+n > sixtyMiB {
				n = sixtyMiB - written
			}
			w.Write(buf[:n]) //nolint:errcheck
			written += n
		}
	}))
	defer srv.Close()

	cfg := runner.DefaultConfig()
	cfg.DownloadSizeLimitBytes = fiftyMiB

	err := runner.DownloadArtifact(context.Background(), srv.URL, cfg, io.Discard)
	if err == nil {
		t.Error("DownloadArtifact with 60 MiB stream should return error")
	}
	if !runner.IsDownloadTooLarge(err) {
		t.Errorf("error should be ErrDownloadTooLarge, got: %v", err)
	}
}

func TestDownloadZeroBytesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := runner.DefaultConfig()
	err := runner.DownloadArtifact(context.Background(), srv.URL, cfg, io.Discard)
	if err == nil {
		t.Error("DownloadArtifact with 0-byte response should return error")
	}
}

func TestDownloadSmallFileSucceeds(t *testing.T) {
	content := []byte("FORM\x00\x00\x00\x10IFRS")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(content) //nolint:errcheck
	}))
	defer srv.Close()

	cfg := runner.DefaultConfig()
	var buf bytes.Buffer
	err := runner.DownloadArtifact(context.Background(), srv.URL, cfg, &buf)
	if err != nil {
		t.Fatalf("DownloadArtifact small file error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Errorf("downloaded content mismatch: got %d bytes, want %d bytes", buf.Len(), len(content))
	}
}

func TestDownloadNon2xxError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := runner.DefaultConfig()
	err := runner.DownloadArtifact(context.Background(), srv.URL, cfg, io.Discard)
	if err == nil {
		t.Error("DownloadArtifact with 404 response should return error")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// IsBlorb — magic bytes detection
// ─────────────────────────────────────────────────────────────────────────────

func TestIsBlorbFalse(t *testing.T) {
	f, err := os.CreateTemp("", "*.z5")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	f.Write([]byte("NOT A BLORB FILE AT ALL")) //nolint:errcheck
	f.Close()

	if runner.IsBlorb(f.Name()) {
		t.Error("IsBlorb = true for non-blorb file; want false")
	}
}

func TestIsBlorbTrue(t *testing.T) {
	// FORM at bytes 0-3, IFRS at bytes 8-11.
	content := make([]byte, 12)
	copy(content[0:4], "FORM")
	copy(content[8:12], "IFRS")

	f, err := os.CreateTemp("", "*.zblorb")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	f.Write(content) //nolint:errcheck
	f.Close()

	if !runner.IsBlorb(f.Name()) {
		t.Error("IsBlorb = false for valid blorb file; want true")
	}
}

func TestIsBlorbMissingFile(t *testing.T) {
	if runner.IsBlorb("/tmp/this-file-does-not-exist-qa-runner-test") {
		t.Error("IsBlorb = true for missing file; want false")
	}
}

func TestIsBlorbTooShort(t *testing.T) {
	f, err := os.CreateTemp("", "*.zblorb")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write([]byte("FORM")); err != nil { // only 4 bytes, need 12
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()

	if runner.IsBlorb(f.Name()) {
		t.Error("IsBlorb = true for 4-byte file; want false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ExtractZip — zip bomb protection (ARCHITECTURE.md §9.4)
// ─────────────────────────────────────────────────────────────────────────────

// makeZip creates an in-memory zip with the given files.
func makeZip(t *testing.T, files []struct{ name, content string }) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, f := range files {
		fw, err := w.Create(f.name)
		if err != nil {
			t.Fatalf("zip create %s: %v", f.name, err)
		}
		fmt.Fprint(fw, f.content)
	}
	w.Close() //nolint:errcheck
	return buf.Bytes()
}

func TestExtractZipTooManyFiles(t *testing.T) {
	// 101 files → archive_too_many_files (limit is 100).
	var files []struct{ name, content string }
	for i := 0; i < 101; i++ {
		files = append(files, struct{ name, content string }{
			name:    fmt.Sprintf("file%03d.z5", i),
			content: "zork",
		})
	}
	zipData := makeZip(t, files)

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "test.zip")
	if err := os.WriteFile(zipPath, zipData, 0600); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	cfg := runner.DefaultConfig()
	_, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err == nil {
		t.Error("ExtractZip with 101 files should return error")
	}
	if !runner.IsArchiveTooManyFiles(err) {
		t.Errorf("error should be archive_too_many_files, got: %v", err)
	}
}

func TestExtractZipPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("../../etc/passwd")
	fmt.Fprint(fw, "root:x:0:0:root:/root:/bin/bash")
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "traversal.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	_, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err == nil {
		t.Error("ExtractZip with path traversal entry should return error")
	}
	if !runner.IsArchiveInvalidPath(err) {
		t.Errorf("error should be archive_invalid_path, got: %v", err)
	}
}

func TestExtractZipAbsolutePath(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("/absolute/evil.z5")
	fmt.Fprint(fw, "game data")
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "absolute.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	_, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err == nil {
		t.Error("ExtractZip with absolute path entry should return error")
	}
	if !runner.IsArchiveInvalidPath(err) {
		t.Errorf("error should be archive_invalid_path, got: %v", err)
	}
}

func TestExtractZipEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "empty.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	_, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err == nil {
		t.Error("ExtractZip with empty archive should return error")
	}
	if !runner.IsArchiveEmpty(err) {
		t.Errorf("error should be archive_empty, got: %v", err)
	}
}

func TestExtractZipSingleIFFile(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("story.z5")
	fmt.Fprint(fw, "FORM")
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "game.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	extracted, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err != nil {
		t.Fatalf("ExtractZip single IF file error = %v", err)
	}
	if extracted == "" {
		t.Error("extracted path is empty")
	}
	if _, statErr := os.Stat(extracted); os.IsNotExist(statErr) {
		t.Errorf("extracted file does not exist: %s", extracted)
	}
}

func TestExtractZipNoKnownIFExtension(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("readme.txt")
	fmt.Fprint(fw, "This is not a game.")
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "noext.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	_, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err == nil {
		t.Error("ExtractZip with no IF extension should return error")
	}
	if !runner.IsUnsupportedFormat(err) {
		t.Errorf("error should be unsupported_format, got: %v", err)
	}
}

func TestExtractZipExactlyAtFileLimit(t *testing.T) {
	// 100 files at limit should succeed if one has an IF extension.
	var files []struct{ name, content string }
	for i := 0; i < 99; i++ {
		files = append(files, struct{ name, content string }{
			name:    fmt.Sprintf("extra%03d.txt", i),
			content: "text",
		})
	}
	files = append(files, struct{ name, content string }{"story.z5", "FORM"})

	zipData := makeZip(t, files)
	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "limit.zip")
	os.WriteFile(zipPath, zipData, 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	extracted, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err != nil {
		t.Errorf("ExtractZip with exactly 100 files should succeed, got: %v", err)
	}
	if extracted == "" {
		t.Error("extracted path is empty at exact file limit")
	}
}

func TestIsArchiveErrorSentinelsCrossMatch(t *testing.T) {
	if runner.IsArchiveTooManyFiles(runner.ErrArchiveEmpty) {
		t.Error("IsArchiveTooManyFiles(ErrArchiveEmpty) = true; want false")
	}
	if runner.IsArchiveEmpty(runner.ErrArchiveTooManyFiles) {
		t.Error("IsArchiveEmpty(ErrArchiveTooManyFiles) = true; want false")
	}
	if runner.IsArchiveInvalidPath(runner.ErrArchiveEmpty) {
		t.Error("IsArchiveInvalidPath(ErrArchiveEmpty) = true; want false")
	}
	if runner.IsUnsupportedFormat(runner.ErrArchiveEmpty) {
		t.Error("IsUnsupportedFormat(ErrArchiveEmpty) = true; want false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SelectInterpreter — TADS 2/3 support via frob (frobtads package)
// ─────────────────────────────────────────────────────────────────────────────

func TestSelectInterpreterFrobForGam(t *testing.T) {
	name, cmd, err := runner.SelectInterpreter("/tmp/game.gam")
	if err != nil {
		t.Fatalf("SelectInterpreter(.gam) error = %v; want nil", err)
	}
	if name != "frob" {
		t.Errorf("SelectInterpreter(.gam) name = %q; want \"frob\"", name)
	}
	if cmd == nil {
		t.Error("SelectInterpreter(.gam) cmd = nil; want non-nil")
	}
}

func TestSelectInterpreterFrobForT3(t *testing.T) {
	name, cmd, err := runner.SelectInterpreter("/tmp/game.t3")
	if err != nil {
		t.Fatalf("SelectInterpreter(.t3) error = %v; want nil", err)
	}
	if name != "frob" {
		t.Errorf("SelectInterpreter(.t3) name = %q; want \"frob\"", name)
	}
	if cmd == nil {
		t.Error("SelectInterpreter(.t3) cmd = nil; want non-nil")
	}
}

func TestSelectInterpreterFrobCaseInsensitive(t *testing.T) {
	// SelectInterpreter lower-cases the path before matching — mixed/upper case
	// extensions must still select frob.
	tests := []string{
		"/tmp/game.GAM",
		"/tmp/game.T3",
		"/tmp/game.Gam",
		"/tmp/GAME.t3",
	}
	for _, path := range tests {
		name, cmd, err := runner.SelectInterpreter(path)
		if err != nil {
			t.Errorf("SelectInterpreter(%q) error = %v; want nil", path, err)
			continue
		}
		if name != "frob" {
			t.Errorf("SelectInterpreter(%q) name = %q; want \"frob\"", path, name)
		}
		if cmd == nil {
			t.Errorf("SelectInterpreter(%q) cmd = nil; want non-nil", path)
		}
	}
}

func TestSelectInterpreterFrobCommandFlags(t *testing.T) {
	// frob must be invoked with -i plain (text interface) and -p (no pause).
	// interpreterCommandByName("frob", …) is unexported; the same code path is
	// exercised here via SelectInterpreter so resume sessions get identical flags.
	for _, storyPath := range []string{"/tmp/game.gam", "/tmp/game.t3"} {
		_, cmd, err := runner.SelectInterpreter(storyPath)
		if err != nil {
			t.Fatalf("SelectInterpreter(%q) error = %v", storyPath, err)
		}
		args := cmd.Args
		if len(args) == 0 || args[0] != "frob" {
			t.Errorf("SelectInterpreter(%q) cmd.Args[0] = %q; want \"frob\"", storyPath, args[0])
		}
		hasIPlain := false
		hasP := false
		for i, a := range args {
			if a == "-i" && i+1 < len(args) && args[i+1] == "plain" {
				hasIPlain = true
			}
			if a == "-p" {
				hasP = true
			}
		}
		if !hasIPlain {
			t.Errorf("frob cmd for %q missing \"-i plain\"; args = %v", storyPath, args)
		}
		if !hasP {
			t.Errorf("frob cmd for %q missing \"-p\"; args = %v", storyPath, args)
		}
	}
}

func TestSelectInterpreterFrobStoryPathInArgs(t *testing.T) {
	// The story file path must appear in the frob command arguments.
	storyPath := "/tmp/sessions/run-001/story.gam"
	_, cmd, err := runner.SelectInterpreter(storyPath)
	if err != nil {
		t.Fatalf("SelectInterpreter error = %v", err)
	}
	found := false
	for _, a := range cmd.Args {
		if a == storyPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("story path %q not found in frob cmd.Args = %v", storyPath, cmd.Args)
	}
}

func TestSelectInterpreterFrobDoesNotMatchZMachine(t *testing.T) {
	// Z-machine extensions must still select dfrotz, not frob.
	for _, path := range []string{"/tmp/game.z5", "/tmp/game.zblorb"} {
		name, _, err := runner.SelectInterpreter(path)
		if err != nil {
			t.Fatalf("SelectInterpreter(%q) error = %v", path, err)
		}
		if name != "dfrotz" {
			t.Errorf("SelectInterpreter(%q) name = %q; want \"dfrotz\"", path, name)
		}
	}
}

func TestSelectInterpreterFrobDoesNotMatchGlulx(t *testing.T) {
	// Glulx extensions must still select glulxe, not frob.
	for _, path := range []string{"/tmp/game.ulx", "/tmp/game.gblorb"} {
		name, _, err := runner.SelectInterpreter(path)
		if err != nil {
			t.Fatalf("SelectInterpreter(%q) error = %v", path, err)
		}
		if name != "glulxe" {
			t.Errorf("SelectInterpreter(%q) name = %q; want \"glulxe\"", path, name)
		}
	}
}

// TestExtractZipFrobExtensions verifies that .gam and .t3 files are recognised
// as valid IF extensions when extracting a zip, so a TADS game bundled in a zip
// is extracted and playable.
func TestExtractZipGamExtension(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("mystery.gam")
	fmt.Fprint(fw, "TADS game data")
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "tads2.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	extracted, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err != nil {
		t.Fatalf("ExtractZip(.gam) error = %v; want nil", err)
	}
	if extracted == "" {
		t.Error("ExtractZip(.gam) returned empty path")
	}
	if !strings.HasSuffix(extracted, ".gam") {
		t.Errorf("ExtractZip(.gam) extracted = %q; want a .gam path", extracted)
	}
}

func TestExtractZipT3Extension(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("adventure.t3")
	fmt.Fprint(fw, "TADS3 game data")
	w.Close() //nolint:errcheck

	tmpDir := t.TempDir()
	zipPath := filepath.Join(tmpDir, "tads3.zip")
	os.WriteFile(zipPath, buf.Bytes(), 0600) //nolint:errcheck

	cfg := runner.DefaultConfig()
	extracted, err := runner.ExtractZip(zipPath, tmpDir, cfg)
	if err != nil {
		t.Fatalf("ExtractZip(.t3) error = %v; want nil", err)
	}
	if extracted == "" {
		t.Error("ExtractZip(.t3) returned empty path")
	}
	if !strings.HasSuffix(extracted, ".t3") {
		t.Errorf("ExtractZip(.t3) extracted = %q; want a .t3 path", extracted)
	}
}
