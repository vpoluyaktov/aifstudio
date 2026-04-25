// transcript_test.go — table-driven unit tests for compressTranscript.
//
// compressTranscript is an unexported function in package server; it is accessed
// here via the export_test.go shim (server.CompressTranscript).
//
// Test priority order (per QA standards):
//  1. Edge inputs: empty, single-line, exactly-10-line boundary.
//  2. Threshold crossing (11 lines): middle preamble lines stripped.
//  3. Rule-based keep: player commands (^>[0-9]), *** markers, Score, error/Warning.
//  4. Blank-run collapsing: consecutive blanks after command → single blank.
//  5. First-5 / last-3 anchoring: always kept regardless of content.
//  6. Response kept: lines after a command are kept (up to 10 per command).
//  7. Response cap at 10: 12 response lines → only first 10 kept.
//  8. Preamble stripped: lines between banner and first command are stripped.
//  9. Signal line in preamble: signal keywords keep the line even before first command.
// 10. Long transcript (50+ lines): preamble stripped, response lines kept, output shorter.
package server_test

import (
	"fmt"
	"strings"
	"testing"

	"aifstudio/internal/server"
)

// jl joins the given strings with "\n" and appends a trailing "\n".
// It is used in test cases to build transcripts that match what a real game
// would emit (each line terminated by a newline).
func jl(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestCompressTranscript(t *testing.T) {
	// Enable compression for all sub-tests — disabled in production by default.
	*server.TranscriptCompressionEnabled = true
	t.Cleanup(func() { *server.TranscriptCompressionEnabled = false })

	// longInput is built once and closed over by the 50-line test case below.
	// Lines 0-4 and 47-49 are header/footer; lines 10, 20, 30, 40 are player
	// commands; lines 5-9 are preamble (before first command at i=10, stripped);
	// all other middle lines are response lines (kept up to 10 per command;
	// between each pair of commands there are only 9, so cap is never hit here).
	var longSB strings.Builder
	for i := 0; i < 50; i++ {
		switch {
		case i < 5 || i >= 47:
			fmt.Fprintf(&longSB, "Header or Footer line %d\n", i)
		case i == 10 || i == 20 || i == 30 || i == 40:
			fmt.Fprintf(&longSB, ">%d go north\n", i)
		default:
			fmt.Fprintf(&longSB, "You are in room %d, exploring.\n", i)
		}
	}
	longInput := longSB.String()

	tests := []struct {
		name  string
		input string
		check func(t *testing.T, got string)
	}{
		// ── 1. Edge inputs ──────────────────────────────────────────────────

		{
			// Empty string → empty string (no lines at all).
			name:  "empty string",
			input: "",
			check: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("got %q; want empty string", got)
				}
			},
		},
		{
			// Single line without trailing newline (1 ≤ 10) → unchanged.
			name:  "single line no trailing newline",
			input: "Hello, world.",
			check: func(t *testing.T, got string) {
				const want = "Hello, world."
				if got != want {
					t.Errorf("got %q; want %q (unchanged)", got, want)
				}
			},
		},
		{
			// Exactly 10 lines with trailing newline — the boundary that must NOT
			// trigger compression (≤10 → return unchanged).
			name: "10 lines with trailing newline — boundary unchanged",
			input: jl(
				"Line 1", "Line 2", "Line 3", "Line 4", "Line 5",
				"Line 6", "Line 7", "Line 8", "Line 9", "Line 10",
			),
			check: func(t *testing.T, got string) {
				want := jl(
					"Line 1", "Line 2", "Line 3", "Line 4", "Line 5",
					"Line 6", "Line 7", "Line 8", "Line 9", "Line 10",
				)
				if got != want {
					t.Errorf("10-line transcript should be unchanged\ngot:  %q\nwant: %q", got, want)
				}
			},
		},

		// ── 2. Threshold crossing ────────────────────────────────────────────

		{
			// 11 lines — first boundary that triggers compression.
			// Middle lines (indices 5-7) are preamble (no command seen) → stripped.
			// First 5 (indices 0-4) and last 3 (indices 8-10) must be kept.
			//
			//   n=11, n-3=8 → last 3: i=8,9,10
			name: "11 lines — middle preamble lines stripped",
			input: jl(
				"header-0", "header-1", "header-2", "header-3", "header-4", // 0-4
				"middle-5", "middle-6", "middle-7", // 5-7: preamble, stripped
				"footer-8", "footer-9", "footer-10", // 8-10: last 3, kept
			),
			check: func(t *testing.T, got string) {
				for _, want := range []string{"header-0", "header-1", "header-2", "header-3", "header-4"} {
					if !strings.Contains(got, want) {
						t.Errorf("first-5 line %q missing from output\ngot: %q", want, got)
					}
				}
				for _, want := range []string{"footer-8", "footer-9", "footer-10"} {
					if !strings.Contains(got, want) {
						t.Errorf("last-3 line %q missing from output\ngot: %q", want, got)
					}
				}
				for _, absent := range []string{"middle-5", "middle-6", "middle-7"} {
					if strings.Contains(got, absent) {
						t.Errorf("preamble line %q should be stripped\ngot: %q", absent, got)
					}
				}
			},
		},

		// ── 3a. Winning game — commands + *** marker ────────────────────────

		{
			// 14-line winning transcript.
			//   n=14, n-3=11 → last 3: i=11,12,13
			//
			// Preamble: only "You stand at the cave mouth." at i=5 (before first
			// command at i=6) is stripped.
			//
			// Response lines after commands are within the 10-line cap and kept:
			//   i=7 "Inner Cavern", i=8 "Darkness surrounds you." (after >1, responseLines 0,1)
			//   i=10 "Taken." (after >2, responseLines 0)
			name: "winning game — preamble stripped, response lines kept, win marker kept",
			input: jl(
				"Story: Cave Adventure",        // 0: always kept
				"Release 1 / Serial 20240101",  // 1: always kept
				"An Interactive Fiction",        // 2: always kept
				"",                             // 3: blank, always kept
				"Cave Entrance",                // 4: always kept (i<5)
				"You stand at the cave mouth.", // 5: preamble, stripped
				">1 enter cave",               // 6: kept (^>[0-9])
				"Inner Cavern",                // 7: response (kept, responseLines=0)
				"Darkness surrounds you.",     // 8: response (kept, responseLines=1)
				">2 take torch",               // 9: kept (^>[0-9]), resets counter
				"Taken.",                      // 10: response (kept, responseLines=0)
				"*** You have won. ***",       // 11: kept (*** signal AND last-3)
				"Score: 10 / 20",              // 12: kept (last-3)
				"Press any key.",              // 13: kept (last-3)
			),
			check: func(t *testing.T, got string) {
				present := []string{
					">1 enter cave",
					">2 take torch",
					"*** You have won. ***",
					"Inner Cavern",
					"Darkness surrounds you.",
					"Taken.",
				}
				for _, s := range present {
					if !strings.Contains(got, s) {
						t.Errorf("expected %q in output\ngot: %q", s, got)
					}
				}
				// Only the preamble line (before first command) should be stripped.
				if strings.Contains(got, "You stand at the cave mouth.") {
					t.Errorf("preamble line should be stripped\ngot: %q", got)
				}
			},
		},

		// ── 3b. Dying game ───────────────────────────────────────────────────

		{
			// 12-line dying transcript.
			//   n=12, n-3=9 → last 3: i=9,10,11
			//
			// "You stand in a dark forest." (i=5) is preamble → stripped.
			// "The Pit" (i=7) and "You fall into darkness." (i=8) are response
			// lines after >1 south → kept (within 10-line cap).
			name: "dying game — preamble stripped, response lines kept, death marker kept",
			input: jl(
				"Adventure Game",               // 0
				"Release 1",                    // 1
				"An IF",                        // 2
				"",                             // 3: blank
				"Forest",                       // 4
				"You stand in a dark forest.",  // 5: preamble, stripped
				">1 south",                     // 6: kept (^>[0-9])
				"The Pit",                      // 7: response (kept, responseLines=0)
				"You fall into darkness.",      // 8: response (kept, responseLines=1)
				"*** You have died. ***",       // 9: kept (*** signal AND last-3)
				"Enter RESTORE or QUIT.",       // 10: kept (last-3)
				"Finished.",                    // 11: kept (last-3)
			),
			check: func(t *testing.T, got string) {
				present := []string{
					">1 south",
					"The Pit",
					"You fall into darkness.",
					"*** You have died. ***",
				}
				for _, s := range present {
					if !strings.Contains(got, s) {
						t.Errorf("expected %q in output\ngot: %q", s, got)
					}
				}
				if strings.Contains(got, "You stand in a dark forest.") {
					t.Errorf("preamble line should be stripped\ngot: %q", got)
				}
			},
		},

		// ── 3c. Score change line ────────────────────────────────────────────

		{
			// 12-line transcript with a Score line at index 5.
			//   n=12, n-3=9 → last 3: i=9,10,11
			//
			// "Score: 5 / 100" at i=5 is a signal → kept despite being in preamble.
			// "Room A" (i=6) and "You are in room A." (i=7) are preamble with no
			// signal keyword and no command → stripped.
			name: "score change line kept, non-signal preamble stripped",
			input: jl(
				"Game",              // 0
				"Release 1",         // 1
				"",                  // 2: blank
				"",                  // 3: blank
				"Beginning",         // 4
				"Score: 5 / 100",    // 5: kept (signal: contains "Score")
				"Room A",            // 6: preamble, stripped
				"You are in room A.", // 7: preamble, stripped
				">1 go east",        // 8: kept (^>[0-9])
				"Room B",            // 9: kept (last-3)
				"Ending 1",          // 10: kept (last-3)
				"Ending 2",          // 11: kept (last-3)
			),
			check: func(t *testing.T, got string) {
				if !strings.Contains(got, "Score: 5 / 100") {
					t.Errorf("Score signal line missing from output\ngot: %q", got)
				}
				if strings.Contains(got, "Room A") {
					t.Errorf("preamble room name present in output\ngot: %q", got)
				}
				if strings.Contains(got, "You are in room A.") {
					t.Errorf("preamble room desc present in output\ngot: %q", got)
				}
			},
		},

		// ── 3d. Error and warning lines ──────────────────────────────────────

		{
			// 12-line transcript with an "error" line at i=5 and a "Warning" line
			// at i=8 — both are signals and must be kept regardless of preamble state.
			// Non-signal preamble lines (i=6, 7) are stripped.
			//   n=12, n-3=9 → last 3: i=9,10,11
			name: "error and warning lines kept",
			input: jl(
				"Interpreter v1",         // 0
				"Release 1",              // 1
				"",                       // 2: blank
				"",                       // 3: blank
				"Start Room",             // 4
				"An error has occurred!", // 5: kept (signal: contains "error")
				"Room A",                 // 6: preamble, stripped
				"Room desc.",             // 7: preamble, stripped
				"A Warning: memory low",  // 8: kept (signal: contains "Warning")
				">1 go east",             // 9: kept (last-3)
				"Final 1",                // 10: kept (last-3)
				"Final 2",                // 11: kept (last-3)
			),
			check: func(t *testing.T, got string) {
				if !strings.Contains(got, "An error has occurred!") {
					t.Errorf("error signal line missing from output\ngot: %q", got)
				}
				if !strings.Contains(got, "A Warning: memory low") {
					t.Errorf("warning signal line missing from output\ngot: %q", got)
				}
				if strings.Contains(got, "Room A") {
					t.Errorf("preamble room name present in output\ngot: %q", got)
				}
				if strings.Contains(got, "Room desc.") {
					t.Errorf("preamble room desc present in output\ngot: %q", got)
				}
			},
		},

		// ── 4. Blank-run collapsing ──────────────────────────────────────────

		{
			// 15-line transcript with three consecutive blank lines after the
			// first command (indices 7-9). Blank run-collapse applies only after
			// the first command; the three blanks must collapse to one.
			//
			//   n=15, n-3=12 → last 3: i=12,13,14
			name: "consecutive blanks after command collapse to one",
			input: jl(
				"Header 1", "Header 2", "Header 3", "Header 4", "Header 5", // 0-4
				"preamble line",                              // 5: preamble, stripped
				">1 go north",                               // 6: command, kept
				"", "", "",                                  // 7-9: three blanks → one blank
				"Response line",                             // 10: response (kept)
				"More response",                             // 11: response (kept)
				"Foot 1", "Foot 2", "Foot 3",              // 12-14: last 3
			),
			check: func(t *testing.T, got string) {
				// No double-blank run should remain.
				if strings.Contains(got, "\n\n\n") {
					t.Errorf("double-blank run not collapsed\ngot: %q", got)
				}
				// A single blank (two consecutive newlines) must be present.
				if !strings.Contains(got, "\n\n") {
					t.Errorf("single blank should be preserved\ngot: %q", got)
				}
				// Command and response lines present.
				if !strings.Contains(got, ">1 go north") {
					t.Errorf("command line missing from output\ngot: %q", got)
				}
				if !strings.Contains(got, "Response line") {
					t.Errorf("response line missing from output\ngot: %q", got)
				}
				// Preamble stripped.
				if strings.Contains(got, "preamble line") {
					t.Errorf("preamble line should be stripped\ngot: %q", got)
				}
			},
		},

		// ── 5. First-5 / last-3 anchoring ───────────────────────────────────

		{
			// 12-line transcript where even the first 5 and last 3 lines are room
			// descriptions (content that would normally be stripped). They must
			// still be present because their positions anchor them.
			//
			//   n=12, n-3=9 → last 3: i=9,10,11
			//   Stripped: "Stripped 5"–"Stripped 8" (i=5..8, preamble, no signal, no command)
			name: "first 5 and last 3 always kept regardless of content",
			input: jl(
				"Room Desc 0", "Room Desc 1", "Room Desc 2", "Room Desc 3", "Room Desc 4", // 0-4
				"Stripped 5", "Stripped 6", "Stripped 7", "Stripped 8", // 5-8: preamble, stripped
				"Room Desc 9", "Room Desc 10", "Room Desc 11", // 9-11: last 3, kept
			),
			check: func(t *testing.T, got string) {
				// First 5 always present.
				for i := 0; i <= 4; i++ {
					label := fmt.Sprintf("Room Desc %d", i)
					if !strings.Contains(got, label) {
						t.Errorf("first-5 line %q missing from output\ngot: %q", label, got)
					}
				}
				// Last 3 always present.
				for i := 9; i <= 11; i++ {
					label := fmt.Sprintf("Room Desc %d", i)
					if !strings.Contains(got, label) {
						t.Errorf("last-3 line %q missing from output\ngot: %q", label, got)
					}
				}
				// Middle preamble lines (5-8) all stripped.
				for i := 5; i <= 8; i++ {
					label := fmt.Sprintf("Stripped %d", i)
					if strings.Contains(got, label) {
						t.Errorf("preamble line %q should be absent\ngot: %q", label, got)
					}
				}
			},
		},

		// ── 6. Response lines kept ───────────────────────────────────────────

		{
			// 11-line transcript verifying that game-response lines following a
			// player command are kept in the output (new behavior).
			//
			//   n=11, n-3=8 → last 3: i=8,9,10
			//   i=5 is the first command (not preamble — commands are never preamble).
			name: "response lines after command are kept",
			input: jl(
				"H0", "H1", "H2", "H3", "H4",          // 0-4: first-5
				">1 look",                               // 5: command (not preamble)
				"You are in a bright room.",             // 6: response (kept, responseLines=0)
				"There is a door to the north.",         // 7: response (kept, responseLines=1)
				"Final A",                               // 8: last-3
				"Final B",                               // 9: last-3
				"Final C",                               // 10: last-3
			),
			check: func(t *testing.T, got string) {
				present := []string{
					">1 look",
					"You are in a bright room.",
					"There is a door to the north.",
				}
				for _, s := range present {
					if !strings.Contains(got, s) {
						t.Errorf("expected %q in output\ngot: %q", s, got)
					}
				}
			},
		},

		// ── 7. Response cap at 10 ────────────────────────────────────────────

		{
			// 22-line transcript where a command at i=6 is followed by 12 response
			// lines (i=7..18). Only the first 10 must be kept; the remaining 2 dropped.
			//
			//   n=22, n-3=19 → last 3: i=19,20,21
			//   responseLines increments for every non-blank non-command line after
			//   the command. At responseLines=10 the line at i=17 ("Response 11")
			//   and i=18 ("Response 12") are dropped.
			name: "response cap: only first 10 response lines per command kept",
			input: jl(
				"H0", "H1", "H2", "H3", "H4",             // 0-4: first-5
				"preamble",                                 // 5: preamble, stripped
				">1 solve puzzle",                          // 6: command
				"Response 1", "Response 2", "Response 3",  // 7-9
				"Response 4", "Response 5", "Response 6",  // 10-12
				"Response 7", "Response 8", "Response 9",  // 13-15
				"Response 10",                             // 16: kept (responseLines=9 < 10)
				"Response 11",                             // 17: dropped (responseLines=10 ≥ 10)
				"Response 12",                             // 18: dropped (responseLines=11 ≥ 10)
				"Foot A", "Foot B", "Foot C",              // 19-21: last-3
			),
			check: func(t *testing.T, got string) {
				// First 10 response lines must all be present.
				for i := 1; i <= 10; i++ {
					s := fmt.Sprintf("Response %d", i)
					if !strings.Contains(got, s) {
						t.Errorf("expected %q in output (within cap)\ngot: %q", s, got)
					}
				}
				// Lines 11 and 12 must be dropped (exceed the cap).
				for i := 11; i <= 12; i++ {
					s := fmt.Sprintf("Response %d", i)
					if strings.Contains(got, s) {
						t.Errorf("expected %q to be dropped (exceeds cap)\ngot: %q", s, got)
					}
				}
			},
		},

		// ── 8. Preamble stripped ─────────────────────────────────────────────

		{
			// 14-line transcript with a clear preamble section (i=5..8) between
			// the initial banner (first-5) and the first player command (i=9).
			// Preamble lines must be absent; command and response lines are kept.
			//
			//   n=14, n-3=11 → last 3: i=11,12,13
			name: "preamble between banner and first command is stripped",
			input: jl(
				"Game Title",          // 0
				"Release 1",           // 1
				"An IF by Dev",        // 2
				"",                    // 3
				"Serial 20240101",     // 4
				"Starting Room",       // 5: preamble, stripped
				"A lush meadow.",      // 6: preamble, stripped
				"Exits: north, east.", // 7: preamble, stripped
				"There is a flower.",  // 8: preamble, stripped
				">1 go north",         // 9: command, kept
				"Rocky Path.",         // 10: response (kept, responseLines=0)
				"A steep path north.", // 11: last-3 (i=11, n-3=11)
				"End A",               // 12: last-3
				"End B",               // 13: last-3
			),
			check: func(t *testing.T, got string) {
				present := []string{">1 go north", "Rocky Path."}
				for _, s := range present {
					if !strings.Contains(got, s) {
						t.Errorf("expected %q in output\ngot: %q", s, got)
					}
				}
				preamble := []string{
					"Starting Room", "A lush meadow.",
					"Exits: north, east.", "There is a flower.",
				}
				for _, s := range preamble {
					if strings.Contains(got, s) {
						t.Errorf("preamble line %q should be stripped\ngot: %q", s, got)
					}
				}
			},
		},

		// ── 9. Signal line in preamble ───────────────────────────────────────

		{
			// 12-line transcript where a "Warning" line appears in the preamble
			// (before the first player command). Signal lines are always kept even
			// when the algorithm has not yet seen a player command.
			//
			//   n=12, n-3=9 → last 3: i=9,10,11
			name: "signal line in preamble is kept",
			input: jl(
				"H0", "H1", "H2", "H3", "H4", // 0-4: first-5
				"Ordinary preamble A",          // 5: preamble, stripped
				"Warning: low memory",          // 6: signal → kept (despite preamble)
				"Ordinary preamble B",          // 7: preamble, stripped
				">1 go south",                  // 8: command, kept
				"Desert.",                      // 9: last-3 (response)
				"Foot A",                       // 10: last-3
				"Foot B",                       // 11: last-3
			),
			check: func(t *testing.T, got string) {
				if !strings.Contains(got, "Warning: low memory") {
					t.Errorf("signal line in preamble missing from output\ngot: %q", got)
				}
				if strings.Contains(got, "Ordinary preamble A") {
					t.Errorf("ordinary preamble line A should be stripped\ngot: %q", got)
				}
				if strings.Contains(got, "Ordinary preamble B") {
					t.Errorf("ordinary preamble line B should be stripped\ngot: %q", got)
				}
			},
		},

		// ── 10. Very long transcript ─────────────────────────────────────────

		{
			// 50-line transcript.  Lines 0-4 and 47-49 are anchored header/footer.
			// Lines 10, 20, 30, 40 are player commands (^>[0-9]).
			// Lines 5-9 are preamble (before first command at i=10) → stripped.
			// Lines 11-19, 21-29, 31-39, 41-46 are response lines; each block has
			// ≤9 lines so all are within the 10-line cap and are kept.
			//
			// Assertions:
			//   • Output is shorter than input (preamble removed).
			//   • All four player commands present in output.
			//   • Anchored lines present.
			//   • Preamble line at i=6 is absent.
			//   • Response line at i=15 IS present (within cap, after command at i=10).
			name: "50-line transcript — preamble stripped, response lines kept, output shorter",
			input: longInput,
			check: func(t *testing.T, got string) {
				if len(got) >= len(longInput) {
					t.Errorf("output not shorter than input: len(got)=%d len(input)=%d",
						len(got), len(longInput))
				}
				// All player commands must survive compression.
				for _, cmd := range []string{
					">10 go north", ">20 go north", ">30 go north", ">40 go north",
				} {
					if !strings.Contains(got, cmd) {
						t.Errorf("player command %q missing from compressed output\ngot: %q", cmd, got)
					}
				}
				// Anchor lines.
				if !strings.Contains(got, "Header or Footer line 0") {
					t.Errorf("first header line missing\ngot: %q", got)
				}
				if !strings.Contains(got, "Header or Footer line 49") {
					t.Errorf("last footer line missing\ngot: %q", got)
				}
				// Preamble line (i=6, before first command at i=10) must be absent.
				if strings.Contains(got, "You are in room 6, exploring.") {
					t.Errorf("preamble line at index 6 should be stripped\ngot: %q", got)
				}
				// Response line (i=15, after command at i=10) must be present.
				if !strings.Contains(got, "You are in room 15, exploring.") {
					t.Errorf("response line at index 15 should be kept\ngot: %q", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := server.CompressTranscript(tt.input)
			tt.check(t, got)
		})
	}
}
