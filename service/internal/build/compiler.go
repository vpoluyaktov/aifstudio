package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// compileResult holds the output of the Inform 7 compiler.
type compileResult struct {
	Log      string
	Duration time.Duration
	Err      error
}

// runCompiler invokes the Inform 7 CLI toolchain on the project at projectRoot.
// The compiler must be available at /usr/local/bin/inform7.
func runCompiler(ctx context.Context, projectRoot string, timeout time.Duration) compileResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	// Inform 7 CLI invocation:
	// inform7 --internal /usr/local/share/inform7 --external . --project <projectRoot>
	cmd := exec.CommandContext(ctx, "/usr/local/bin/inform7",
		"--internal", "/usr/local/share/inform7",
		"--external", ".",
		"--project", projectRoot,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	dur := time.Since(start)

	log := stdout.String() + stderr.String()

	if ctx.Err() == context.DeadlineExceeded {
		return compileResult{
			Log:      log,
			Duration: dur,
			Err:      fmt.Errorf("build timed out after %v", timeout),
		}
	}

	return compileResult{
		Log:      log,
		Duration: dur,
		Err:      wrapCompileError(err, stdout.String(), stderr.String()),
	}
}

// wrapCompileError turns an *exec.ExitError into a user-facing message.
// The raw "exit status N" is opaque to end users; we map well-known exit
// codes to explanations and include the most informative snippet of the
// compiler output (preferring Inform 7's stdout problem markers over the
// usually-uninformative stderr tail).
func wrapCompileError(err error, stdout, stderr string) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// Non-ExitError (PathError, context cancel, etc.) — surface as-is.
		return err
	}
	code := exitErr.ExitCode()
	hint := compilerHint(stdout+"\n"+stderr, stderr)

	switch code {
	case 127:
		// 127 = shell "command not found". Reserved for the placeholder
		// shim installed when the Inform 7 CLI is absent from the image.
		return fmt.Errorf("Inform 7 compiler is not installed in this build (exit 127)%s", hint)
	case 1:
		return fmt.Errorf("Inform 7 rejected the source text (exit 1)%s", hint)
	case 2:
		return fmt.Errorf("Inform 7 wrapper called with bad arguments (exit 2)%s", hint)
	case 3:
		return fmt.Errorf("Inform 6 back-end failed to produce a Glulx story (exit 3)%s", hint)
	default:
		return fmt.Errorf("Inform 7 compiler failed (exit %d)%s", code, hint)
	}
}

// compilerHint picks the most informative error snippet from stdout and
// stderr, formatted for inclusion in an error message.
//
// Priority order:
//  1. Inform 7's "Translation failed: N problems found" marker plus all
//     ">--> …" problem blocks (up to 3), flattened to a single line (from
//     stdout). This is by far the most useful output for the end user when
//     the Inform 7 source text has compile-time defects.
//  2. Last non-empty line of stderr. Works for wrapper-internal failures
//     or when the compiler never reached the "Translation failed" stage.
//
// Returns empty string when neither source yields anything useful.
func compilerHint(stdout, stderr string) string {
	if s := informProblemSummary(stdout); s != "" {
		return ": " + s
	}
	return stderrHint(stderr)
}

// informProblemSummary extracts an Inform 7 compiler problem summary from
// stdout: the "Translation failed: N problems found" marker plus all
// ">--> …" problem blocks (up to 3), joined with "; also: ". Returns an
// empty string when stdout does not contain Inform 7 problem markers.
func informProblemSummary(stdout string) string {
	const marker = "Translation failed:"
	idx := strings.Index(stdout, marker)
	if idx == -1 {
		// Sometimes the marker is absent (e.g. crash mid-translation);
		// fall back to the raw ">-->" lines if any are present.
		if problems := allProblemLines(stdout, 3); problems != "" {
			return "compile problem — " + problems
		}
		return ""
	}

	countPhrase := stdout[idx:]
	if eol := strings.IndexByte(countPhrase, '\n'); eol != -1 {
		countPhrase = countPhrase[:eol]
	}
	countPhrase = strings.TrimSpace(countPhrase) // "Translation failed: 3 problems found"

	if problems := allProblemLines(stdout, 3); problems != "" {
		return countPhrase + "; first: " + problems
	}
	return countPhrase
}

// allProblemLines collects up to max ">--> …" blocks from stdout, trims
// the marker and leading whitespace, flattens internal whitespace to single
// spaces, truncates each block at 600 bytes (on a rune boundary), and joins
// multiple blocks with "; also: ".
func allProblemLines(stdout string, max int) string {
	const blockMarker = ">-->"
	const maxLen = 600
	endMarkers := []string{">-->", "++ Ended:", "\n In Volume ", "\nIn Volume "}

	var blocks []string
	remaining := stdout
	for len(blocks) < max {
		start := strings.Index(remaining, blockMarker)
		if start == -1 {
			break
		}
		tail := strings.TrimLeft(remaining[start+len(blockMarker):], " \t")

		end := len(tail)
		for _, m := range endMarkers {
			if i := strings.Index(tail, m); i != -1 && i < end {
				end = i
			}
		}
		block := tail[:end]
		flat := strings.Join(strings.Fields(block), " ")
		if len(flat) > maxLen {
			cut := maxLen
			for cut > 0 && (flat[cut]&0xC0) == 0x80 {
				cut--
			}
			flat = flat[:cut] + "…"
		}
		if flat != "" {
			blocks = append(blocks, flat)
		}

		// Advance remaining to the next end marker (which may be another ">-->").
		if end < len(tail) {
			remaining = tail[end:]
		} else {
			break
		}
	}

	return strings.Join(blocks, "; also: ")
}

// stderrHint returns the last non-empty line of stderr, formatted for
// inclusion in an error message. Empty string if stderr is empty.
func stderrHint(stderr string) string {
	stderr = strings.TrimRight(stderr, "\n\r\t ")
	if stderr == "" {
		return ""
	}
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return ": " + line
		}
	}
	return ""
}
