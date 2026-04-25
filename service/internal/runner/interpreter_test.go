// Package runner — interpreter_test.go tests SelectInterpreter and the
// unexported interpreterCommandByName using an internal (package runner) test
// so that the unexported function is accessible.
package runner

import (
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// SelectInterpreter — full extension table (ARCHITECTURE.md §A.5)
// ─────────────────────────────────────────────────────────────────────────────

func TestSelectInterpreterTable(t *testing.T) {
	tests := []struct {
		path     string
		wantName string
		wantErr  bool
	}{
		// Z-machine
		{"/tmp/g.z3", "dfrotz", false},
		{"/tmp/g.z4", "dfrotz", false},
		{"/tmp/g.z5", "dfrotz", false},
		{"/tmp/g.z6", "dfrotz", false},
		{"/tmp/g.z7", "dfrotz", false},
		{"/tmp/g.z8", "dfrotz", false},
		{"/tmp/g.zblorb", "dfrotz", false},
		// Glulx
		{"/tmp/g.ulx", "glulxe", false},
		{"/tmp/g.gblorb", "glulxe", false},
		// TADS 2/3 — frob auto-detects from file header
		{"/tmp/g.gam", "frob", false},
		{"/tmp/g.t3", "frob", false},
		// Case-insensitive
		{"/tmp/g.GAM", "frob", false},
		{"/tmp/g.T3", "frob", false},
		{"/tmp/g.Z5", "dfrotz", false},
		// Unsupported — must return error
		{"/tmp/g.alan", "", true},
		{"/tmp/g.exe", "", true},
		{"/tmp/g", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			name, cmd, err := SelectInterpreter(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Errorf("SelectInterpreter(%q) error = nil; want error", tt.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectInterpreter(%q) error = %v; want nil", tt.path, err)
			}
			if name != tt.wantName {
				t.Errorf("SelectInterpreter(%q) name = %q; want %q", tt.path, name, tt.wantName)
			}
			if cmd == nil {
				t.Errorf("SelectInterpreter(%q) cmd = nil; want non-nil", tt.path)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// interpreterCommandByName — resume path (ARCHITECTURE.md §A.5.0)
//
// The interpreter name is persisted in Firestore at session start; on resume
// interpreterCommandByName reconstructs the exact same command without
// re-inferring from the file extension.
// ─────────────────────────────────────────────────────────────────────────────

func TestInterpreterCommandByNameTable(t *testing.T) {
	tests := []struct {
		name     string
		wantBin  string
		wantErr  bool
	}{
		{"dfrotz", "dfrotz", false},
		{"glulxe", "glulxe", false},
		{"frob", "frob", false},
		{"unknown", "", true},
		{"", "", true},
		{"FROB", "", true}, // case-sensitive — stored name must match exactly
	}

	const storyPath = "/tmp/run-001/story.gam"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := interpreterCommandByName(tt.name, storyPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("interpreterCommandByName(%q) error = nil; want error", tt.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("interpreterCommandByName(%q) error = %v; want nil", tt.name, err)
			}
			if cmd == nil {
				t.Fatalf("interpreterCommandByName(%q) cmd = nil; want non-nil", tt.name)
			}
			if len(cmd.Args) == 0 || cmd.Args[0] != tt.wantBin {
				t.Errorf("interpreterCommandByName(%q) cmd.Args[0] = %q; want %q", tt.name, cmd.Args[0], tt.wantBin)
			}
		})
	}
}

func TestInterpreterCommandByNameFrobFlags(t *testing.T) {
	// frob must carry -i plain and -p; same flags SelectInterpreter would use.
	const storyPath = "/tmp/run-002/game.t3"
	cmd, err := interpreterCommandByName("frob", storyPath)
	if err != nil {
		t.Fatalf("interpreterCommandByName(frob) error = %v", err)
	}
	hasIPlain, hasP := false, false
	for i, a := range cmd.Args {
		if a == "-i" && i+1 < len(cmd.Args) && cmd.Args[i+1] == "plain" {
			hasIPlain = true
		}
		if a == "-p" {
			hasP = true
		}
	}
	if !hasIPlain {
		t.Errorf("frob cmd missing \"-i plain\"; args = %v", cmd.Args)
	}
	if !hasP {
		t.Errorf("frob cmd missing \"-p\"; args = %v", cmd.Args)
	}
	// Story path must appear in args.
	found := false
	for _, a := range cmd.Args {
		if a == storyPath {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("story path %q not found in frob cmd.Args = %v", storyPath, cmd.Args)
	}
}

func TestInterpreterCommandByNameDfrotzFlags(t *testing.T) {
	// dfrotz must carry -p -q -m -w 255.
	const storyPath = "/tmp/run-003/game.z5"
	cmd, err := interpreterCommandByName("dfrotz", storyPath)
	if err != nil {
		t.Fatalf("interpreterCommandByName(dfrotz) error = %v", err)
	}
	args := cmd.Args
	wantFlags := []string{"-p", "-q", "-m", "-w", "255"}
	for _, f := range wantFlags {
		found := false
		for _, a := range args {
			if a == f {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("dfrotz cmd missing flag %q; args = %v", f, args)
		}
	}
}

func TestInterpreterCommandByNameConsistentWithSelectInterpreter(t *testing.T) {
	// The command produced by interpreterCommandByName must have the same
	// binary and flag set as the one produced by SelectInterpreter for the same
	// extension, so that resume sessions are identical to fresh sessions.
	cases := []struct {
		interpName string
		storyPath  string
	}{
		{"dfrotz", "/tmp/g.z5"},
		{"glulxe", "/tmp/g.ulx"},
		{"frob", "/tmp/g.gam"},
		{"frob", "/tmp/g.t3"},
	}

	for _, tc := range cases {
		_, selectCmd, err := SelectInterpreter(tc.storyPath)
		if err != nil {
			t.Fatalf("SelectInterpreter(%q) error = %v", tc.storyPath, err)
		}
		resumeCmd, err := interpreterCommandByName(tc.interpName, tc.storyPath)
		if err != nil {
			t.Fatalf("interpreterCommandByName(%q) error = %v", tc.interpName, err)
		}
		if selectCmd.Args[0] != resumeCmd.Args[0] {
			t.Errorf("binary mismatch for %q: select=%q resume=%q",
				tc.storyPath, selectCmd.Args[0], resumeCmd.Args[0])
		}
		if len(selectCmd.Args) != len(resumeCmd.Args) {
			t.Errorf("arg count mismatch for %q: select=%v resume=%v",
				tc.storyPath, selectCmd.Args, resumeCmd.Args)
		}
	}
}
