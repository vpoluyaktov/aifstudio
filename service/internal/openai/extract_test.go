// extract_test.go — unit tests for ExtractFencedInform7 (§6.3, §17.1 of
// ARCHITECTURE_AI_CREATE.md).
//
// Test priority:
//  1. Normal inform7 fence → correct source + reply, nil error.
//  2. No fence at all → ErrNoFence, full text is reply.
//  3. i7 fence fallback → extracted correctly.
//  4. Language-less fence fallback → extracted correctly.
//  5. Prose before + after fence → reply is joined prose.
//  6. Multiple fenced blocks → only the first matching fence is used.
//  7. Empty source inside block → source == "", no error.
//  8. Unclosed fence → falls through to ErrNoFence (no ErrUnclosedFence
//     in the actual implementation — the function tries the next tag).
//
// Note: we do NOT test zero-length regex match counts here because
// ExtractFencedInform7 uses strings.Index (not regexp), so zero-length
// regex edge cases are irrelevant (§ template-standards §2).
package openai

import (
	"errors"
	"testing"
)

func TestExtractFencedInform7(t *testing.T) {
	// canned source / reply used in several cases
	const (
		simpleSource = "\"Blue Door\" by Alex.\n\nThe Hallway is a room."
		simpleReply  = "Here it is:"
	)

	tests := []struct {
		name       string
		input      string
		wantSource string
		wantReply  string
		wantErr    error
	}{
		// ── No fence ─────────────────────────────────────────────────────────────
		{
			name:       "empty string returns ErrNoFence",
			input:      "",
			wantSource: "",
			wantReply:  "",
			wantErr:    ErrNoFence,
		},
		{
			name:       "single-line reply with no fence returns ErrNoFence",
			input:      "Just a conversational reply.",
			wantSource: "",
			wantReply:  "Just a conversational reply.",
			wantErr:    ErrNoFence,
		},
		{
			name:       "multi-line reply with no fence returns full trimmed text",
			input:      "First line.\nSecond line.\nThird line.",
			wantSource: "",
			wantReply:  "First line.\nSecond line.\nThird line.",
			wantErr:    ErrNoFence,
		},

		// ── inform7 fence (priority 1) ───────────────────────────────────────────
		{
			name: "inform7 fence only — no surrounding prose",
			input: "```inform7\n" +
				simpleSource + "\n" +
				"```",
			wantSource: simpleSource,
			wantReply:  "",
			wantErr:    nil,
		},
		{
			name: "inform7 fence with prose before and after",
			input: simpleReply + "\n" +
				"```inform7\n" +
				simpleSource + "\n" +
				"```\n" +
				"Good luck!",
			wantSource: simpleSource,
			wantReply:  simpleReply + "\n" + "Good luck!",
			wantErr:    nil,
		},
		{
			name: "inform7 fence with prose only before",
			input: simpleReply + "\n" +
				"```inform7\n" +
				simpleSource + "\n" +
				"```",
			wantSource: simpleSource,
			wantReply:  simpleReply,
			wantErr:    nil,
		},
		{
			name: "inform7 fence with prose only after",
			input: "```inform7\n" +
				simpleSource + "\n" +
				"```\n" +
				"Good luck!",
			wantSource: simpleSource,
			wantReply:  "Good luck!",
			wantErr:    nil,
		},

		// ── i7 fence fallback (priority 2) ──────────────────────────────────────
		{
			name: "i7 fence extracted when no inform7 fence present",
			input: simpleReply + "\n" +
				"```i7\n" +
				simpleSource + "\n" +
				"```",
			wantSource: simpleSource,
			wantReply:  simpleReply,
			wantErr:    nil,
		},

		// ── language-less fence fallback (priority 3) ───────────────────────────
		{
			name: "plain fence extracted when no tagged fence present",
			input: simpleReply + "\n" +
				"```\n" +
				simpleSource + "\n" +
				"```",
			wantSource: simpleSource,
			wantReply:  simpleReply,
			wantErr:    nil,
		},

		// ── Multiple blocks ──────────────────────────────────────────────────────
		{
			name: "multiple inform7 blocks — only first block source extracted",
			input: "```inform7\n" +
				"first source\n" +
				"```\n" +
				"Some prose\n" +
				"```inform7\n" +
				"second source\n" +
				"```",
			wantSource: "first source",
			// Everything after the first closing fence becomes part of the reply.
			wantReply: "Some prose\n```inform7\nsecond source\n```",
			wantErr:   nil,
		},

		// ── Empty source in block ────────────────────────────────────────────────
		{
			// Empty source triggers a 409 `empty_source` in the handler
			// (§17.1), but ExtractFencedInform7 itself returns nil error.
			name: "empty source inside inform7 block — source is empty string, nil error",
			input: "Cleared it:\n" +
				"```inform7\n" +
				"\n" +
				"```\n" +
				"Done.",
			wantSource: "",
			wantReply:  "Cleared it:\nDone.",
			wantErr:    nil,
		},

		// ── Unclosed fence ───────────────────────────────────────────────────────
		{
			// When the inform7 fence has no closing ```, the function falls
			// through all three tag variants and returns ErrNoFence (not a
			// distinct ErrUnclosedFence — the implementation skips unclosed
			// fences and tries the next tag before giving up).
			name:       "unclosed inform7 fence falls through to ErrNoFence",
			input:      "Some prose\n```inform7\nincomplete source without closing fence",
			wantSource: "",
			// The entire text (trimmed) becomes the reply.
			wantReply: "Some prose\n```inform7\nincomplete source without closing fence",
			wantErr:   ErrNoFence,
		},

		// ── Priority: inform7 wins over plain fence ──────────────────────────────
		{
			// When both a plain ``` block and an inform7 block are present,
			// the inform7 block wins regardless of position in the text.
			name: "inform7 fence takes priority over earlier plain fence",
			input: "```\n" +
				"not the source\n" +
				"```\n" +
				"But this is it:\n" +
				"```inform7\n" +
				"real source\n" +
				"```",
			wantSource: "real source",
			// Everything before the inform7 fence (including the plain block) is the reply.
			wantReply: "```\nnot the source\n```\nBut this is it:",
			wantErr:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSource, gotReply, gotErr := ExtractFencedInform7(tt.input)

			if !errors.Is(gotErr, tt.wantErr) {
				t.Errorf("err = %v; want %v", gotErr, tt.wantErr)
			}
			if gotSource != tt.wantSource {
				t.Errorf("source = %q; want %q", gotSource, tt.wantSource)
			}
			if gotReply != tt.wantReply {
				t.Errorf("reply = %q; want %q", gotReply, tt.wantReply)
			}
		})
	}
}

// TestExtractFencedInform7EdgeCaseSingleLineSource checks that a source
// consisting of a single line (no newline at end before closing fence) is
// handled. This verifies the alternate closing-fence detection path
// `strings.HasSuffix(…, "\n```")`.
func TestExtractFencedInform7EdgeCaseSingleLineSource(t *testing.T) {
	// Note: the closing ``` must be on its own line per the SSE framing spec (§7.4).
	// Normal case has \n before ```.
	input := "```inform7\n\"Blue Door\" by Alex.\n```"
	src, reply, err := ExtractFencedInform7(input)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if src != "\"Blue Door\" by Alex." {
		t.Errorf("source = %q; want single-line source", src)
	}
	if reply != "" {
		t.Errorf("reply = %q; want empty", reply)
	}
}
