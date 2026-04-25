package server

import (
	"regexp"
	"strings"
)

// playerCmdRE matches lines beginning with ">" immediately followed by a digit.
// Inform 7 "test me" scripts emit numbered commands in this format (>1, >2, …).
var playerCmdRE = regexp.MustCompile(`^>[0-9]`)

// maxResponseLines is the maximum number of consecutive game-response lines
// kept after each player command.
const maxResponseLines = 10

// compressTranscript reduces a game transcript to the lines most useful for
// AI evaluation or display while preserving game response text after each
// player command. The algorithm is stateful:
//
//   - First 5 lines verbatim (game title / version / intro banner)
//   - Last 3 lines verbatim (final game state / prompt)
//   - Signal lines anywhere: lines containing "***", "Score", "error", "Error",
//     "problem", "Problem", "warning", or "Warning" — always kept
//   - Before the first player command (after line 5): all lines are skipped
//     (strips the initial room-description preamble)
//   - Player command lines (^>[0-9]): always kept; resets the response counter
//   - Blank lines (after first command): kept with run-collapse (at most one
//     consecutive blank); does not affect the response counter
//   - Response lines (non-blank, non-command, after first command): kept while
//     responseLines < maxResponseLines, then dropped; counter increments each
//     kept or skipped response line and resets on the next command
//
// If the input has ≤ 10 lines the function returns it unchanged — there is no
// benefit to compressing tiny transcripts.
// transcriptCompressionEnabled controls whether compressTranscript applies
// the filtering algorithm. Set to false to pass the raw transcript unchanged.
var transcriptCompressionEnabled = false

func compressTranscript(transcript string) string {
	if !transcriptCompressionEnabled {
		return transcript
	}

	lines := strings.Split(transcript, "\n")

	// Record whether the original ended with a newline so we can restore it.
	trailingNewline := strings.HasSuffix(transcript, "\n")
	// Remove the synthetic empty element that Split produces for a trailing "\n".
	if trailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) <= 10 {
		return transcript
	}

	n := len(lines)
	out := make([]string, 0, n)

	seenCmd := false
	responseLines := 0
	prevBlank := false

	isSignal := func(line string) bool {
		return strings.Contains(line, "***") ||
			strings.Contains(line, "Score") ||
			strings.Contains(line, "error") ||
			strings.Contains(line, "Error") ||
			strings.Contains(line, "problem") ||
			strings.Contains(line, "Problem") ||
			strings.Contains(line, "warning") ||
			strings.Contains(line, "Warning")
	}

	for i, raw := range lines {
		// Trim \r so blank/command detection works for both \n and \r\n content.
		line := strings.TrimRight(raw, "\r")
		isBlank := strings.TrimSpace(line) == ""
		isCmd := playerCmdRE.MatchString(line)

		// Always keep first 5 and last 3 lines verbatim.
		if i < 5 || i >= n-3 {
			out = append(out, raw)
			prevBlank = isBlank
			continue
		}

		// Always keep signal lines regardless of position or state.
		if isSignal(line) {
			if !(isBlank && prevBlank) {
				out = append(out, raw)
				prevBlank = isBlank
			}
			continue
		}

		// Before the first command: skip everything (preamble / room description).
		if !seenCmd && !isCmd {
			continue
		}

		// Command line: always keep; mark seenCmd; reset response counter.
		if isCmd {
			seenCmd = true
			responseLines = 0
			out = append(out, raw)
			prevBlank = false
			continue
		}

		// Past here: seenCmd is true, line is non-command.

		// Blank line: keep with run-collapse; do not touch responseLines.
		if isBlank {
			if !prevBlank {
				out = append(out, raw)
				prevBlank = true
			}
			continue
		}

		// Response line: keep up to maxResponseLines per command block.
		if responseLines < maxResponseLines {
			out = append(out, raw)
			prevBlank = false
		}
		responseLines++
	}

	result := strings.Join(out, "\n")
	if trailingNewline {
		result += "\n"
	}
	return result
}
