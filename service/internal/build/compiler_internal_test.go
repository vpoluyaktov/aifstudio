// Internal unit tests for compiler.go error-summarisation helpers.
package build

import (
	"strings"
	"testing"
)

// Real-world Inform 7 v10.1.2 stdout excerpt used by the staging reproducer
// (build b-01KPVQ1MNBJR5GG8NJXPY8SHZQ, project p-01KPVDWBCNQFR3X24T5QVZPXZ1).
// Three problems, trailing summary line.
const informFailedStdout = `Inform 7 v10.1.2 has started.
I've now read your source text, which is 1491 words long.
Inform 7 has finished.
++ 5% (Analysing sentences)
++ 15% (Drawing inferences)

In Volume 2 - Map:

  >--> You wrote '"Steps descend into a wet darkness."' (source text, line 68), but in another sentence
    '"Stone walls sweat."' (source text, line 71): Cellar Stairs seems to have two
    different descriptions, perhaps because you intended the second description
    to apply to something mentioned in between.

  >--> You wrote 'The haunt-text of the Cellar is "..."' (source text, line 72),
    but in another sentence 'The haunt-text of the Cellar Stairs is "..."'
    (source text, line 69): this looks like a contradiction.

 In Volume 3 - The Blue Door and Core Objects:

  >--> In the sentence 'It is south of the Foyer and north of Outside' (source
    text, line 76): "Outside" cannot be used as a direction.

++ Ended: Translation failed: 3 problems found
`

const informFailedStderr = `inform7 wrapper: stage 1 (Inform 7) failed (rc=1)
`

func TestCompilerHintPrefersInformStdoutOverStderr(t *testing.T) {
	hint := compilerHint(informFailedStdout, informFailedStderr)

	// Must NOT fall back to stderr — the wrapper line is useless for the user.
	if strings.Contains(hint, "wrapper: stage 1") {
		t.Fatalf("compilerHint fell back to stderr wrapper line: %q", hint)
	}
	// Must include the total-problem summary.
	if !strings.Contains(hint, "Translation failed: 3 problems found") {
		t.Fatalf("compilerHint missing problem count: %q", hint)
	}
	// Must include a fragment from the first >--> block.
	if !strings.Contains(hint, "Cellar Stairs seems to have two") {
		t.Fatalf("compilerHint missing first-problem fragment: %q", hint)
	}
	// Must begin with ": " so wrapCompileError can interpolate it directly.
	if !strings.HasPrefix(hint, ": ") {
		t.Fatalf("compilerHint must start with ': ', got %q", hint)
	}
}

func TestCompilerHintFallsBackToStderrWhenNoInformMarkers(t *testing.T) {
	const stdout = "Inform 7 v10.1.2 has started.\n"
	const stderr = "segmentation fault (core dumped)\n"

	hint := compilerHint(stdout, stderr)
	if hint != ": segmentation fault (core dumped)" {
		t.Fatalf("expected stderr fallback, got %q", hint)
	}
}

func TestCompilerHintEmptyWhenBothSourcesUninformative(t *testing.T) {
	if got := compilerHint("", ""); got != "" {
		t.Fatalf("expected empty hint for empty sources, got %q", got)
	}
	if got := compilerHint("some unrelated output\n", ""); got != "" {
		t.Fatalf("expected empty hint when stdout has no problem markers and stderr empty, got %q", got)
	}
}

func TestAllProblemLinesSingleBlock(t *testing.T) {
	flat := allProblemLines(informFailedStdout, 1)
	if flat == "" {
		t.Fatal("allProblemLines returned empty for valid Inform 7 output")
	}
	// Flatten must collapse newlines/indent runs.
	if strings.Contains(flat, "\n") {
		t.Errorf("allProblemLines should flatten newlines, got: %q", flat)
	}
	// With max=1 must stop before the second >--> block.
	if strings.Contains(flat, "haunt-text") {
		t.Errorf("allProblemLines(max=1) should not include second problem: %q", flat)
	}
	// Must stop before the "In Volume 3" header.
	if strings.Contains(flat, "Volume 3") {
		t.Errorf("allProblemLines should stop at next 'In Volume' header, got: %q", flat)
	}
	// Length cap is now 600; ellipsis "…" is 3 bytes in UTF-8.
	if len(flat) > 603 {
		t.Errorf("allProblemLines exceeded length cap, got %d bytes: %q", len(flat), flat)
	}
}

func TestAllProblemLinesMultiBlock(t *testing.T) {
	got := allProblemLines(informFailedStdout, 3)
	// Must contain fragments from all three >--> blocks.
	if !strings.Contains(got, "Cellar Stairs seems to have two") {
		t.Errorf("missing first block fragment: %q", got)
	}
	if !strings.Contains(got, "haunt-text") {
		t.Errorf("missing second block fragment: %q", got)
	}
	if !strings.Contains(got, "cannot be used as a direction") {
		t.Errorf("missing third block fragment: %q", got)
	}
	// Blocks must be separated by "; also: ".
	if strings.Count(got, "; also: ") != 2 {
		t.Errorf("expected 2 '; also: ' separators, got: %q", got)
	}
}

func TestLogTailTruncation(t *testing.T) {
	// logTail lives in manager.go — smoke-test it here since we ship both
	// changes together.
	s := strings.Repeat("x", 1000) + "THE-END"
	got := logTail(s, 500)
	if !strings.HasSuffix(got, "THE-END") {
		t.Errorf("logTail must preserve the tail, got suffix: ...%s", got[len(got)-20:])
	}
	if !strings.HasPrefix(got, "…[truncated]") {
		t.Errorf("logTail must prefix truncation marker, got prefix: %s...", got[:20])
	}
	// Short inputs pass through unchanged.
	if got := logTail("short", 500); got != "short" {
		t.Errorf("logTail should not modify short inputs, got %q", got)
	}
}
