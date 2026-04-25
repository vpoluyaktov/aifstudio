package build

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"aifstudio/internal/store"
)

// uploadArtifacts uploads the compiled .ulx and build log to GCS.
// Returns the GCS paths for the artifact and log.
func uploadArtifacts(ctx context.Context, st store.Store, buildID, projectRoot, buildLog string) (artifactGCSPath, logGCSPath string, err error) {
	ulxPath := artifactPath(projectRoot)

	// Upload .ulx artifact.
	ulxData, err := os.ReadFile(ulxPath)
	if err != nil {
		return "", "", fmt.Errorf("read artifact: %w", err)
	}
	artifactGCSPath = fmt.Sprintf("builds/%s/story.ulx", buildID)
	if err := st.UploadBlob(ctx, artifactGCSPath, "application/octet-stream", bytes.NewReader(ulxData)); err != nil {
		return "", "", fmt.Errorf("upload artifact: %w", err)
	}

	// Upload build log.
	logGCSPath = fmt.Sprintf("builds/%s/build.log", buildID)
	if err := st.UploadBlob(ctx, logGCSPath, "text/plain; charset=utf-8", bytes.NewReader([]byte(buildLog))); err != nil {
		return "", "", fmt.Errorf("upload build log: %w", err)
	}

	return artifactGCSPath, logGCSPath, nil
}

// uploadLogOnly uploads only the build log (for failed builds with no artifact).
func uploadLogOnly(ctx context.Context, st store.Store, buildID, buildLog string) (string, error) {
	logGCSPath := fmt.Sprintf("builds/%s/build.log", buildID)
	if err := st.UploadBlob(ctx, logGCSPath, "text/plain; charset=utf-8", bytes.NewReader([]byte(buildLog))); err != nil {
		return "", fmt.Errorf("upload build log: %w", err)
	}
	return logGCSPath, nil
}
