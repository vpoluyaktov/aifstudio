package runner

import (
	"fmt"
	"os/exec"
	"strings"
)

// SelectInterpreter returns the interpreter name and command for the given story file.
// name is "dfrotz", "glulxe", or "frob"; returns an error for unsupported extensions.
// The name is recorded in Firestore so that on resume the same interpreter is
// used without re-inferring from the file extension (§A.5.0).
//
// dfrotz flags:
//   -p: plain ASCII output (no formatting codes)
//   -q: quiet mode (suppress startup banner)
//   -m: turn off MORE prompts (prevents pagination pauses)
//   -w 255: screen width (prevents mid-word wrapping)
//
// frob flags (from Debian frobtads package):
//   -i plain: plain text interface (no curses; required for piped I/O)
//   -p: no pause before quitting
//   The `frob` binary auto-detects TADS 2 (.gam) vs TADS 3 (.t3) from the file header.
func SelectInterpreter(storyPath string) (name string, cmd *exec.Cmd, err error) {
	lower := strings.ToLower(storyPath)
	switch {
	case hasSuffix(lower, ".z3", ".z4", ".z5", ".z6", ".z7", ".z8", ".zblorb"):
		return "dfrotz", exec.Command("dfrotz", "-p", "-q", "-m", "-w", "255", storyPath), nil
	case hasSuffix(lower, ".ulx", ".gblorb"):
		return "glulxe", exec.Command("glulxe", storyPath), nil
	case hasSuffix(lower, ".gam", ".t3"):
		return "frob", exec.Command("frob", "-i", "plain", "-p", storyPath), nil
	default:
		return "", nil, fmt.Errorf("unsupported_format: no interpreter for %s", storyPath)
	}
}

// interpreterCommandByName spawns the named interpreter with the given story file.
// Used on resume (§A.5.0) to replay exactly the same interpreter without re-inferring
// the name from the file extension.
func interpreterCommandByName(name, storyPath string) (*exec.Cmd, error) {
	switch name {
	case "dfrotz":
		return exec.Command("dfrotz", "-p", "-q", "-m", "-w", "255", storyPath), nil
	case "glulxe":
		return exec.Command("glulxe", storyPath), nil
	case "frob":
		return exec.Command("frob", "-i", "plain", "-p", storyPath), nil
	default:
		return nil, fmt.Errorf("unknown interpreter: %s", name)
	}
}

func hasSuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
