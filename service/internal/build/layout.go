// Package build manages Inform 7 compilation sessions.
package build

import (
	"fmt"
	"os"
	"path/filepath"
)

// createProjectLayout creates the Inform 7 project directory structure at
// /tmp/build/<uuid>/<Project>.inform/Source/story.ni and returns the project root.
func createProjectLayout(buildID, source string) (projectRoot string, err error) {
	buildDir := filepath.Join(os.TempDir(), "build", buildID)
	projectRoot = filepath.Join(buildDir, "Project.inform")
	sourceDir := filepath.Join(projectRoot, "Source")

	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		return "", fmt.Errorf("mkdir source dir: %w", err)
	}

	storyPath := filepath.Join(sourceDir, "story.ni")
	if err := os.WriteFile(storyPath, []byte(source), 0600); err != nil {
		return "", fmt.Errorf("write story.ni: %w", err)
	}

	return projectRoot, nil
}

// artifactPath returns the expected output .ulx path after a successful compile.
func artifactPath(projectRoot string) string {
	return filepath.Join(projectRoot, "Build", "output.ulx")
}

// cleanupBuildDir removes the build working directory.
func cleanupBuildDir(buildID string) {
	os.RemoveAll(filepath.Join(os.TempDir(), "build", buildID))
}

// ExportedCreateLayout is a test helper that exposes createProjectLayout.
// Returns the projectRoot path.
func ExportedCreateLayout(buildID, source string) (string, error) {
	return createProjectLayout(buildID, source)
}

// ExportedCleanup is a test helper that removes the build directory.
func ExportedCleanup(buildID string) {
	cleanupBuildDir(buildID)
}
