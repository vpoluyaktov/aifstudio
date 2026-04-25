package runner

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Zip bomb protection constants (per §9.4).
const (
	maxExtractFiles   = 100
	maxExtractTotal   = 100 * 1024 * 1024 // 100 MiB
	maxExtractPerFile = 50 * 1024 * 1024  // 50 MiB
)

// Sentinel errors for zip extraction.
var (
	ErrArchiveTooManyFiles = errors.New("archive_too_many_files")
	ErrArchiveEmpty        = errors.New("archive_empty")
	ErrArchiveInvalidPath  = errors.New("archive_invalid_path")
	ErrUnsupportedFormat   = errors.New("unsupported_format")
	ErrArchiveTooLarge     = errors.New("archive_too_large")
)

// IsArchiveTooManyFiles reports whether err is ErrArchiveTooManyFiles.
func IsArchiveTooManyFiles(err error) bool { return errors.Is(err, ErrArchiveTooManyFiles) }

// IsArchiveEmpty reports whether err is ErrArchiveEmpty.
func IsArchiveEmpty(err error) bool { return errors.Is(err, ErrArchiveEmpty) }

// IsArchiveInvalidPath reports whether err is ErrArchiveInvalidPath.
func IsArchiveInvalidPath(err error) bool { return errors.Is(err, ErrArchiveInvalidPath) }

// IsUnsupportedFormat reports whether err is ErrUnsupportedFormat.
func IsUnsupportedFormat(err error) bool { return errors.Is(err, ErrUnsupportedFormat) }

// blorb magic: IFF FORM chunk with IFRS sub-type.
var blorbMagic = []byte("FORM")
var blorbSubType = []byte("IFRS")

// IsBlorb detects IFF/blorb files by magic bytes (FORM + IFRS at byte 8).
func IsBlorb(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 12)
	if n, _ := f.Read(buf); n < 12 {
		return false
	}
	return string(buf[:4]) == string(blorbMagic) && string(buf[8:12]) == string(blorbSubType)
}

// ExtractZip is the exported variant of extractZip used by tests and callers.
// cfg is accepted for API compatibility; the package constants govern actual limits.
func ExtractZip(srcPath, destDir string, _ Config) (string, error) {
	return extractZip(srcPath, destDir)
}

// extractZip extracts a zip archive at srcPath into destDir.
// Returns the path of the story file to use. Enforces §9.4 limits.
func extractZip(srcPath, destDir string) (string, error) {
	r, err := zip.OpenReader(srcPath)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	if len(r.File) == 0 {
		return "", ErrArchiveEmpty
	}
	if len(r.File) > maxExtractFiles {
		return "", ErrArchiveTooManyFiles
	}

	var totalUncompressed int64
	var bestFile string
	var bestSize int64

	ifExts := map[string]bool{
		".z3": true, ".z4": true, ".z5": true, ".z6": true,
		".z7": true, ".z8": true, ".zblorb": true,
		".ulx": true, ".gblorb": true,
		".gam": true, ".t3": true,
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}

		// Path traversal check.
		clean := filepath.Clean(f.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return "", ErrArchiveInvalidPath
		}

		// Per-entry size cap.
		if f.UncompressedSize64 > maxExtractPerFile {
			return "", ErrArchiveTooLarge
		}

		totalUncompressed += int64(f.UncompressedSize64)
		if totalUncompressed > maxExtractTotal {
			return "", ErrArchiveTooLarge
		}

		ext := strings.ToLower(filepath.Ext(f.Name))
		if !ifExts[ext] {
			continue
		}

		size := int64(f.UncompressedSize64)
		if bestFile == "" || size > bestSize {
			// Extract this candidate.
			outPath := filepath.Join(destDir, "story"+ext)
			if err := extractEntry(f, outPath); err != nil {
				return "", fmt.Errorf("extract entry: %w", err)
			}
			bestFile = outPath
			bestSize = size
		}
	}

	if bestFile == "" {
		return "", ErrUnsupportedFormat
	}
	return bestFile, nil
}

func extractEntry(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Honour per-entry cap while writing.
	lr := io.LimitReader(rc, maxExtractPerFile+1)
	n, err := io.Copy(out, lr)
	if err != nil {
		return err
	}
	if n > maxExtractPerFile {
		return ErrArchiveTooLarge
	}
	return nil
}
