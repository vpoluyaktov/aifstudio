package server

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed prompts/system_prompt.txt
var systemPromptTmpl string

// descriptionBlockRE matches <DESCRIPTION>...</DESCRIPTION> case-insensitively,
// including content that spans multiple lines.
var descriptionBlockRE = regexp.MustCompile(`(?is)<description>(.*?)</description>`)

// BuildSystem returns the system prompt for the AI authoring assistant.
// It embeds the current project name, description, and the full Inform 7 source.
// The template lives in prompts/system_prompt.txt and is embedded at compile time.
// See §9 of ARCHITECTURE_AI_CREATE.md for the normative template.
func BuildSystem(name, description, currentSource string) string {
	return fmt.Sprintf(systemPromptTmpl, name, description, currentSource)
}

// BuildGenerateUserMessage returns the user message for the initial generate turn.
// See §9.2 of ARCHITECTURE_AI_CREATE.md.
func BuildGenerateUserMessage(name, description string) string {
	return fmt.Sprintf("Project name: %s\n\nDescription of the game I want:\n%s\n\nPlease write the Inform 7 source for this game.",
		name, description)
}

// ExtractDescriptionBlock finds and removes the <DESCRIPTION>...</DESCRIPTION>
// block from an AI generate response. Returns the extracted description (empty
// string if absent) and the raw string with the block stripped out.
// The match is case-insensitive; the captured content is whitespace-trimmed.
// Only used on Kind=generate turns — never called on chat turns.
func ExtractDescriptionBlock(raw string) (description, stripped string) {
	loc := descriptionBlockRE.FindStringSubmatchIndex(raw)
	if loc == nil {
		return "", raw
	}
	description = strings.TrimSpace(raw[loc[2]:loc[3]])
	stripped = raw[:loc[0]] + raw[loc[1]:]
	return description, stripped
}
