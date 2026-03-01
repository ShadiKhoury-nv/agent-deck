package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsAgentProcess(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantTool string
		wantOK   bool
	}{
		{
			name:     "claude absolute path",
			args:     []string{"/usr/local/bin/claude", "--help"},
			wantTool: "claude",
			wantOK:   true,
		},
		{
			name:     "claude bare name",
			args:     []string{"claude"},
			wantTool: "claude",
			wantOK:   true,
		},
		{
			name:     "gemini in args",
			args:     []string{"/home/user/.local/bin/gemini", "chat"},
			wantTool: "gemini",
			wantOK:   true,
		},
		{
			name:     "codex process",
			args:     []string{"codex", "--model", "gpt-4"},
			wantTool: "codex",
			wantOK:   true,
		},
		{
			name:     "opencode process",
			args:     []string{"/usr/bin/opencode"},
			wantTool: "opencode",
			wantOK:   true,
		},
		{
			name:   "unrelated process bash",
			args:   []string{"/bin/bash", "-c", "echo hello"},
			wantOK: false,
		},
		{
			name:   "unrelated process with claude in arg but not basename",
			args:   []string{"/usr/bin/python3", "/home/user/claude-scripts/run.py"},
			wantOK: false,
		},
		{
			name:   "empty args",
			args:   []string{},
			wantOK: false,
		},
		{
			name:   "nil args",
			args:   nil,
			wantOK: false,
		},
		{
			name:     "claude as second arg (node execution)",
			args:     []string{"/usr/bin/node", "/usr/local/lib/node_modules/claude"},
			wantTool: "claude",
			wantOK:   true,
		},
		{
			name:   "claude-code is not a match (different basename)",
			args:   []string{"claude-code"},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTool, gotOK := isAgentProcess(tt.args)
			if gotOK != tt.wantOK {
				t.Errorf("isAgentProcess(%v) ok = %v, want %v", tt.args, gotOK, tt.wantOK)
			}
			if gotTool != tt.wantTool {
				t.Errorf("isAgentProcess(%v) tool = %q, want %q", tt.args, gotTool, tt.wantTool)
			}
		})
	}
}

func TestParseProcCmdline(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want []string
	}{
		{
			name: "standard cmdline",
			data: []byte("/usr/bin/claude\x00--help\x00"),
			want: []string{"/usr/bin/claude", "--help"},
		},
		{
			name: "no trailing null",
			data: []byte("/usr/bin/claude\x00--help"),
			want: []string{"/usr/bin/claude", "--help"},
		},
		{
			name: "empty",
			data: []byte{},
			want: nil,
		},
		{
			name: "single arg",
			data: []byte("claude\x00"),
			want: []string{"claude"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseProcCmdline(tt.data)
			if len(got) != len(tt.want) {
				t.Fatalf("parseProcCmdline() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseProcCmdline()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseProcEnviron(t *testing.T) {
	data := []byte("HOME=/home/user\x00CLAUDE_SESSION_ID=abc-123\x00PATH=/usr/bin\x00AGENTDECK_INSTANCE_ID=deck-1\x00")
	env := parseProcEnviron(data)

	if env["HOME"] != "/home/user" {
		t.Errorf("HOME = %q, want %q", env["HOME"], "/home/user")
	}
	if env["CLAUDE_SESSION_ID"] != "abc-123" {
		t.Errorf("CLAUDE_SESSION_ID = %q, want %q", env["CLAUDE_SESSION_ID"], "abc-123")
	}
	if env["AGENTDECK_INSTANCE_ID"] != "deck-1" {
		t.Errorf("AGENTDECK_INSTANCE_ID = %q, want %q", env["AGENTDECK_INSTANCE_ID"], "deck-1")
	}
}

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		tool    string
		want    string
	}{
		{
			name:    "claude session ID",
			envVars: map[string]string{"CLAUDE_SESSION_ID": "sess-001"},
			tool:    "claude",
			want:    "sess-001",
		},
		{
			name:    "gemini session ID",
			envVars: map[string]string{"GEMINI_SESSION_ID": "sess-002"},
			tool:    "gemini",
			want:    "sess-002",
		},
		{
			name:    "codex session ID",
			envVars: map[string]string{"CODEX_SESSION_ID": "sess-003"},
			tool:    "codex",
			want:    "sess-003",
		},
		{
			name:    "opencode session ID",
			envVars: map[string]string{"OPENCODE_SESSION_ID": "sess-004"},
			tool:    "opencode",
			want:    "sess-004",
		},
		{
			name:    "generic fallback",
			envVars: map[string]string{"AGENT_SESSION_ID": "sess-005"},
			tool:    "claude",
			want:    "sess-005",
		},
		{
			name:    "tool-specific takes precedence",
			envVars: map[string]string{"CLAUDE_SESSION_ID": "specific", "AGENT_SESSION_ID": "generic"},
			tool:    "claude",
			want:    "specific",
		},
		{
			name:    "no session ID",
			envVars: map[string]string{"HOME": "/home/user"},
			tool:    "claude",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSessionID(tt.envVars, tt.tool)
			if got != tt.want {
				t.Errorf("extractSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnrichFromJSONL(t *testing.T) {
	// Create a temporary directory structure that mimics Claude's config layout
	tmpDir := t.TempDir()

	// Override CLAUDE_CONFIG_DIR for this test
	origConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	os.Setenv("CLAUDE_CONFIG_DIR", tmpDir)
	defer os.Setenv("CLAUDE_CONFIG_DIR", origConfigDir)

	projectPath := "/home/user/projects/myapp"
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dirName := ConvertToClaudeDirName(projectPath)
	projectDir := filepath.Join(tmpDir, "projects", dirName)

	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	// Write a JSONL file with system and user entries
	entries := []procJSONLEntry{
		{Type: "system", Slug: "my-session-slug"},
		{Type: "user", UserMessage: "hello world"},
		{Type: "assistant", UserMessage: "hi there"},
		{Type: "user", UserMessage: "second prompt"},
	}

	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	f, err := os.Create(jsonlPath)
	if err != nil {
		t.Fatalf("failed to create JSONL: %v", err)
	}
	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	slug, lastPrompt := enrichFromJSONL(projectPath, sessionID)
	if slug != "my-session-slug" {
		t.Errorf("slug = %q, want %q", slug, "my-session-slug")
	}
	if lastPrompt != "second prompt" {
		t.Errorf("lastPrompt = %q, want %q", lastPrompt, "second prompt")
	}
}

func TestEnrichFromJSONL_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	origConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	os.Setenv("CLAUDE_CONFIG_DIR", tmpDir)
	defer os.Setenv("CLAUDE_CONFIG_DIR", origConfigDir)

	slug, lastPrompt := enrichFromJSONL("/nonexistent/path", "fake-session-id")
	if slug != "" || lastPrompt != "" {
		t.Errorf("expected empty results for missing file, got slug=%q lastPrompt=%q", slug, lastPrompt)
	}
}

func TestEnrichFromJSONL_MalformedJSON(t *testing.T) {
	tmpDir := t.TempDir()
	origConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")
	os.Setenv("CLAUDE_CONFIG_DIR", tmpDir)
	defer os.Setenv("CLAUDE_CONFIG_DIR", origConfigDir)

	projectPath := "/home/user/project"
	sessionID := "test-session"
	dirName := ConvertToClaudeDirName(projectPath)
	projectDir := filepath.Join(tmpDir, "projects", dirName)
	os.MkdirAll(projectDir, 0755)

	// Write malformed JSONL
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	os.WriteFile(jsonlPath, []byte("not json\n{bad json}\n"), 0644)

	slug, lastPrompt := enrichFromJSONL(projectPath, sessionID)
	if slug != "" || lastPrompt != "" {
		t.Errorf("expected empty results for malformed JSONL, got slug=%q lastPrompt=%q", slug, lastPrompt)
	}
}

func TestDiscoverRunningProcesses(t *testing.T) {
	// This test verifies the function runs without error.
	// It may return an empty list if no agent processes are running.
	results, err := DiscoverRunningProcesses(nil)
	if err != nil {
		t.Fatalf("DiscoverRunningProcesses() error: %v", err)
	}

	// Verify result types are correct
	for _, proc := range results {
		if proc.PID <= 0 {
			t.Errorf("invalid PID: %d", proc.PID)
		}
		validTools := map[string]bool{"claude": true, "gemini": true, "codex": true, "opencode": true}
		if !validTools[proc.Tool] {
			t.Errorf("unexpected tool: %q", proc.Tool)
		}
	}
	t.Logf("discovered %d running agent processes", len(results))
}

func TestReadProcEnviron_Self(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc filesystem only available on Linux")
	}

	// Read our own environment via /proc/self/environ
	data, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		t.Fatalf("failed to read /proc/self/environ: %v", err)
	}

	env := parseProcEnviron(data)

	// /proc/self/environ reflects the environment at process start, not runtime
	// os.Setenv changes. HOME should always be present in the initial environment.
	if _, ok := env["HOME"]; !ok {
		t.Error("HOME not found in /proc/self/environ")
	}

	// PATH should also be present
	if _, ok := env["PATH"]; !ok {
		t.Error("PATH not found in /proc/self/environ")
	}

	// Verify we parsed a reasonable number of entries
	if len(env) < 3 {
		t.Errorf("expected at least 3 env vars from /proc/self/environ, got %d", len(env))
	}
}

func TestReadProcCmdline_Self(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc filesystem only available on Linux")
	}

	data, err := os.ReadFile("/proc/self/cmdline")
	if err != nil {
		t.Fatalf("failed to read /proc/self/cmdline: %v", err)
	}

	args := parseProcCmdline(data)
	if len(args) == 0 {
		t.Fatal("expected at least one arg from /proc/self/cmdline")
	}

	// The first arg should contain the test binary path
	t.Logf("/proc/self/cmdline args[0] = %q", args[0])
}

func TestSplitNull(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want []string
	}{
		{
			name: "basic split",
			data: []byte("a\x00b\x00c"),
			want: []string{"a", "b", "c"},
		},
		{
			name: "trailing null",
			data: []byte("a\x00b\x00"),
			want: []string{"a", "b"},
		},
		{
			name: "empty",
			data: []byte{},
			want: nil,
		},
		{
			name: "consecutive nulls filtered",
			data: []byte("a\x00\x00b"),
			want: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitNull(tt.data)
			if len(got) != len(tt.want) {
				t.Fatalf("splitNull() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitNull()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDiscoverSkipsSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("proc filesystem only available on Linux")
	}

	results, err := DiscoverRunningProcesses(nil)
	if err != nil {
		t.Fatalf("DiscoverRunningProcesses() error: %v", err)
	}

	selfPID := os.Getpid()
	for _, proc := range results {
		if proc.PID == selfPID {
			t.Errorf("should not discover self (PID %d)", selfPID)
		}
	}
}

func TestDiscoverViaPSCommand_ParsesOutput(t *testing.T) {
	// Simulate ps output parsing by testing the scanner logic
	// This test doesn't require macOS; it tests the parsing portion only
	sampleOutput := strings.Join([]string{
		"  PID COMM             TT",
		" 1234 claude           ttys000",
		" 5678 bash             ttys001",
		" 9012 gemini           ttys002",
		"13579 node             ttys003",
	}, "\n")

	// We can't call discoverViaPSCommand directly since it runs ps,
	// but we can verify our tool detection logic on the parsed fields
	lines := strings.Split(sampleOutput, "\n")
	var found []string
	for i, line := range lines {
		if i == 0 {
			continue // skip header
		}
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		base := filepath.Base(fields[1])
		for _, tool := range knownAgentTools {
			if base == tool {
				found = append(found, tool)
			}
		}
	}

	if len(found) != 2 {
		t.Errorf("expected 2 agent processes in sample output, found %d: %v", len(found), found)
	}
}
