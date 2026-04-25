package openai

import (
	"errors"
	"strings"
)

// ErrNoFence is returned by ExtractFencedInform7 when the model omitted the
// required fenced code block.
var ErrNoFence = errors.New("no fenced code block found in model reply")

// ExtractFencedInform7 scans text for a fenced code block and returns
// (inform7Source, conversationalReply, error).
//
// Fence detection priority:
//  1. ```inform7  … ```     (preferred; system prompt asks for this)
//  2. ```i7       … ```
//  3. ```         … ```     (language-less fence, first one in the message)
//
// The first matching fence wins. Everything before and after the fence is
// joined (with a single "\n") and returned as the conversational reply.
// If no fence is found, the entire text is the reply and source == "".
// Returns ErrNoFence if no fence found.
func ExtractFencedInform7(text string) (source string, reply string, err error) {
	// Try each fence tag in priority order.
	for _, tag := range []string{"inform7", "i7", ""} {
		var fence string
		if tag == "" {
			fence = "```"
		} else {
			fence = "```" + tag
		}

		open := strings.Index(text, fence+"\n")
		if open == -1 {
			// Also try fence with no newline (edge case: fence at end of string).
			open = strings.Index(text, fence)
			if open == -1 {
				continue
			}
		}

		// Find the closing ```.
		afterOpen := open + len(fence)
		// Skip the newline after the opening fence line.
		nl := strings.Index(text[afterOpen:], "\n")
		if nl == -1 {
			continue
		}
		contentStart := afterOpen + nl + 1

		close := strings.Index(text[contentStart:], "\n```")
		if close == -1 {
			// Try closing fence at the very end without trailing newline.
			if strings.HasSuffix(text[contentStart:], "\n```") {
				close = len(text[contentStart:]) - 4
			} else {
				continue
			}
		}

		source = text[contentStart : contentStart+close]
		// Build the reply from everything before the opening fence and after the closing ```.
		beforeFence := strings.TrimSpace(text[:open])
		afterClose := contentStart + close + len("\n```")
		afterFence := strings.TrimSpace(text[afterClose:])

		switch {
		case beforeFence != "" && afterFence != "":
			reply = beforeFence + "\n" + afterFence
		case beforeFence != "":
			reply = beforeFence
		default:
			reply = afterFence
		}
		return source, reply, nil
	}

	// No fence found — whole text is the reply.
	return "", strings.TrimSpace(text), ErrNoFence
}
