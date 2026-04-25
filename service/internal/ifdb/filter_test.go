// Package ifdb — filter_test.go tests the unexported devsysSupported and
// detectFormat functions using an internal (package ifdb) test file.
package ifdb

import (
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// devsysSupported — authoring-system filter (ARCHITECTURE.md §A.6)
//
// Rules:
//   - empty string      → true  (unknown system, include optimistically)
//   - prefix match      → true  (case-insensitive; "Inform 7" matches "inform")
//   - no match          → false
//   - comma-separated   → true if ANY part matches
// ─────────────────────────────────────────────────────────────────────────────

func TestDevsysSupportedTable(t *testing.T) {
	tests := []struct {
		devsys string
		want   bool
	}{
		// Empty = unknown → include
		{"", true},

		// Inform variants
		{"Inform 7", true},
		{"Inform 6", true},
		{"Inform 5", true},
		{"inform 7", true}, // case-insensitive
		{"INFORM 7", true},

		// ZIL / Z-machine explicit labels
		{"ZIL", true},
		{"zil", true},
		{"Z-machine", true},
		{"z-machine", true},
		{"Zcode", true},

		// Glulx
		{"Glulx", true},
		{"glulx", true},

		// TADS — added in this sprint
		{"TADS", true},
		{"TADS 2", true},
		{"TADS 3", true},
		{"tads 2", true}, // case-insensitive
		{"tads3", true},  // no space also has "tads" prefix

		// Unsupported systems
		{"Hugo", false},
		{"hugo", false},
		{"AGT", false},
		{"Adrift", false},
		{"ADRIFT", false},
		{"The Quill", false},
		{"Quest", false},
		{"Twine", false},

		// Comma-separated: include if ANY part matches
		{"The Quill, Inform 6, ZIL", true},  // Inform 6 and ZIL both match
		{"The Quill, Inform 6", true},        // Inform 6 matches
		{"AGT, Hugo", false},                 // neither matches
		{"Hugo, ADRIFT", false},              // neither matches
		{"Hugo, TADS 2", true},               // TADS 2 matches
	}

	for _, tt := range tests {
		t.Run(tt.devsys, func(t *testing.T) {
			got := devsysSupported(tt.devsys)
			if got != tt.want {
				t.Errorf("devsysSupported(%q) = %v; want %v", tt.devsys, got, tt.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// detectFormat — IFDB format string → canonical extension (ARCHITECTURE.md §A.5)
//
// Priority: explicit format/fileType fields first, then URL path extension.
// TADS-specific mappings added this sprint:
//   "tads3" / "tads 3" → "t3"
//   "tads2" / "tads 2" / "tads" → "gam"
// ─────────────────────────────────────────────────────────────────────────────

func TestDetectFormatTable(t *testing.T) {
	tests := []struct {
		rawURL   string
		format   string
		fileType string
		want     string
	}{
		// ---- TADS explicit format fields ----
		{"", "tads3", "", "t3"},
		{"", "tads 3", "", "t3"},
		{"", "TADS3", "", "t3"}, // case-insensitive
		{"", "tads2", "", "gam"},
		{"", "tads 2", "", "gam"},
		{"", "tads", "", "gam"},
		{"", "TADS", "", "gam"},
		// fileType wins when format is empty
		{"", "", "tads3", "t3"},
		{"", "", "tads2", "gam"},
		{"", "", "tads", "gam"},

		// ---- Z-machine / Glulx explicit format fields ----
		// Note: "zcode" is not in knownFormats and has no explicit mapping, so it
		// falls through to the URL extension fallback — test via URL below.
		{"", "z5", "", "z5"},
		{"", "zblorb", "", "zblorb"},
		{"", "ulx", "", "ulx"},
		{"", "gblorb", "", "gblorb"},

		// ---- URL path extension fallback ----
		{"http://example.com/game.gam", "", "", "gam"},
		{"http://example.com/game.t3", "", "", "t3"},
		{"http://example.com/game.z5", "", "", "z5"},
		{"http://example.com/game.zblorb", "", "", "zblorb"},
		{"http://example.com/game.ulx", "", "", "ulx"},

		// ---- Unknown / unsupported → empty ----
		{"http://example.com/game.alan", "", "", ""},
		{"http://example.com/game.hugo", "hugo", "", ""},
		{"", "alan", "", ""},
		{"", "", "", ""},
	}

	for _, tt := range tests {
		name := tt.rawURL + "|" + tt.format + "|" + tt.fileType
		t.Run(name, func(t *testing.T) {
			got := detectFormat(tt.rawURL, tt.format, tt.fileType)
			if got != tt.want {
				t.Errorf("detectFormat(%q, %q, %q) = %q; want %q",
					tt.rawURL, tt.format, tt.fileType, got, tt.want)
			}
		})
	}
}

func TestDetectFormatFormatFieldTakesPrecedenceOverURL(t *testing.T) {
	// Explicit format field wins even when the URL has a different extension.
	got := detectFormat("http://example.com/story.z5", "tads3", "")
	if got != "t3" {
		t.Errorf("detectFormat with format=tads3, url=*.z5: got %q; want \"t3\"", got)
	}
}
