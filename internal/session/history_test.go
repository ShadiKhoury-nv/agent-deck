package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanSessionHistory(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a project directory with UUID-named JSONL files
	projectDir := filepath.Join(tmpDir, "projects", "-home-testuser")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("Failed to create project dir: %v", err)
	}

	// Session 1: valid UUID file
	jsonl1 := `{"type":"system","sessionId":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","slug":"bright-sailing-turing","timestamp":"2026-03-01T10:00:00Z"}
{"type":"user","message":{"role":"user","content":"Hello, can you help me?"},"timestamp":"2026-03-01T10:00:01Z","version":"2.1.52","gitBranch":"main"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Of course!"}]},"timestamp":"2026-03-01T10:00:02Z"}
`
	if err := os.WriteFile(filepath.Join(projectDir, "a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl"), []byte(jsonl1), 0644); err != nil {
		t.Fatalf("Failed to write session file: %v", err)
	}

	// Session 2: another valid UUID file
	jsonl2 := `{"type":"system","sessionId":"b2c3d4e5-f6a7-8901-bcde-f23456789012","slug":"cool-red-eagle","timestamp":"2026-03-01T09:00:00Z"}
{"type":"user","message":{"role":"user","content":"Fix the database connection"},"timestamp":"2026-03-01T09:00:01Z","version":"2.0.0","gitBranch":"feature/db"}
`
	if err := os.WriteFile(filepath.Join(projectDir, "b2c3d4e5-f6a7-8901-bcde-f23456789012.jsonl"), []byte(jsonl2), 0644); err != nil {
		t.Fatalf("Failed to write session file: %v", err)
	}

	// Agent file (should be skipped)
	agentJsonl := `{"type":"system","sessionId":"agent-12345","slug":"agent-session"}`
	if err := os.WriteFile(filepath.Join(projectDir, "agent-12345.jsonl"), []byte(agentJsonl), 0644); err != nil {
		t.Fatalf("Failed to write agent file: %v", err)
	}

	// Non-UUID file (should be skipped)
	if err := os.WriteFile(filepath.Join(projectDir, "notes.jsonl"), []byte(`{"test":true}`), 0644); err != nil {
		t.Fatalf("Failed to write notes file: %v", err)
	}

	sessions, err := ScanSessionHistory(tmpDir)
	if err != nil {
		t.Fatalf("ScanSessionHistory failed: %v", err)
	}

	if len(sessions) != 2 {
		t.Fatalf("Expected 2 sessions, got %d", len(sessions))
	}

	// Verify sorted by LastModified descending (most recent first)
	if sessions[0].LastModified.Before(sessions[1].LastModified) {
		t.Error("Sessions should be sorted by LastModified descending")
	}

	// Verify project path reconstruction
	for _, sess := range sessions {
		if sess.ProjectDir != "-home-testuser" {
			t.Errorf("Expected ProjectDir '-home-testuser', got %q", sess.ProjectDir)
		}
		if sess.ProjectPath != "/home/testuser" {
			t.Errorf("Expected ProjectPath '/home/testuser', got %q", sess.ProjectPath)
		}
	}
}

func TestScanSessionHistoryNonExistentDir(t *testing.T) {
	sessions, err := ScanSessionHistory("/nonexistent/path/to/claude")
	if err != nil {
		t.Fatalf("Expected nil error for nonexistent dir, got: %v", err)
	}
	if sessions != nil {
		t.Error("Expected nil sessions for nonexistent dir")
	}
}

func TestEnrichHistoricalSession(t *testing.T) {
	tmpDir := t.TempDir()

	jsonlContent := `{"type":"system","sessionId":"test-uuid","slug":"bright-sailing-turing","timestamp":"2026-03-01T10:00:00Z"}
{"type":"user","message":{"role":"user","content":"Hello, can you help me?"},"timestamp":"2026-03-01T10:00:01Z","version":"2.1.52","gitBranch":"main"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Of course! What do you need?"}]},"timestamp":"2026-03-01T10:00:02Z"}
{"type":"user","message":{"role":"user","content":"Tell me about Go interfaces"},"timestamp":"2026-03-01T10:00:03Z"}
{"type":"assistant","message":{"role":"assistant","content":"Go interfaces are implicit contracts."},"timestamp":"2026-03-01T10:00:04Z"}
{"type":"user","message":{"role":"user","content":"Thanks, that was helpful!"},"timestamp":"2026-03-01T10:00:05Z"}
`
	filePath := filepath.Join(tmpDir, "test-session.jsonl")
	if err := os.WriteFile(filePath, []byte(jsonlContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	sess := HistoricalSession{
		FilePath: filePath,
	}
	enrichHistoricalSession(&sess)

	// Verify slug extraction
	if sess.Slug != "bright-sailing-turing" {
		t.Errorf("Expected slug 'bright-sailing-turing', got %q", sess.Slug)
	}

	// Verify version
	if sess.ClaudeVersion != "2.1.52" {
		t.Errorf("Expected version '2.1.52', got %q", sess.ClaudeVersion)
	}

	// Verify git branch
	if sess.GitBranch != "main" {
		t.Errorf("Expected git branch 'main', got %q", sess.GitBranch)
	}

	// Verify CreatedAt
	expectedTime, _ := time.Parse(time.RFC3339, "2026-03-01T10:00:00Z")
	if !sess.CreatedAt.Equal(expectedTime) {
		t.Errorf("Expected CreatedAt %v, got %v", expectedTime, sess.CreatedAt)
	}

	// Verify first prompt
	if sess.FirstPrompt != "Hello, can you help me?" {
		t.Errorf("Expected first prompt 'Hello, can you help me?', got %q", sess.FirstPrompt)
	}

	// Verify last prompt
	if sess.LastPrompt != "Thanks, that was helpful!" {
		t.Errorf("Expected last prompt 'Thanks, that was helpful!', got %q", sess.LastPrompt)
	}

	// Verify line count
	if sess.MessageCount != 6 {
		t.Errorf("Expected 6 messages, got %d", sess.MessageCount)
	}
}

func TestEnrichHistoricalSessionTruncation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a long prompt (> 200 chars)
	longPrompt := ""
	for i := 0; i < 250; i++ {
		longPrompt += "x"
	}

	jsonlContent := `{"type":"system","sessionId":"test","slug":"test-slug","timestamp":"2026-03-01T10:00:00Z"}
{"type":"user","message":{"role":"user","content":"` + longPrompt + `"},"timestamp":"2026-03-01T10:00:01Z"}
`
	filePath := filepath.Join(tmpDir, "truncation-test.jsonl")
	if err := os.WriteFile(filePath, []byte(jsonlContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	sess := HistoricalSession{FilePath: filePath}
	enrichHistoricalSession(&sess)

	// First prompt should be truncated to 200 chars + "..."
	if len(sess.FirstPrompt) != 203 {
		t.Errorf("Expected truncated first prompt (203 chars), got %d chars", len(sess.FirstPrompt))
	}
}

func TestFilterHistory(t *testing.T) {
	now := time.Now()
	sessions := []HistoricalSession{
		{
			SessionID:    "sess-1",
			Slug:         "bright-sailing-turing",
			ProjectPath:  "/home/user/project-a",
			FirstPrompt:  "Help me with React hooks",
			LastPrompt:   "Thanks for the help",
			LastModified: now.Add(-1 * time.Hour),
		},
		{
			SessionID:    "sess-2",
			Slug:         "cool-red-eagle",
			ProjectPath:  "/home/user/project-b",
			FirstPrompt:  "Fix the database connection",
			LastPrompt:   "All done",
			LastModified: now.Add(-48 * time.Hour),
		},
		{
			SessionID:    "sess-3",
			Slug:         "warm-green-fox",
			ProjectPath:  "/home/user/project-a",
			FirstPrompt:  "Build a REST API",
			LastPrompt:   "Looks good",
			LastModified: now.Add(-30 * time.Minute),
		},
	}

	// Test project filter
	filtered := FilterHistory(sessions, "project-a", "", 0)
	if len(filtered) != 2 {
		t.Errorf("Project filter: expected 2 results, got %d", len(filtered))
	}

	// Test search query on slug
	filtered = FilterHistory(sessions, "", "eagle", 0)
	if len(filtered) != 1 {
		t.Errorf("Search query (slug): expected 1 result, got %d", len(filtered))
	}
	if len(filtered) > 0 && filtered[0].SessionID != "sess-2" {
		t.Errorf("Search query (slug): expected sess-2, got %q", filtered[0].SessionID)
	}

	// Test search query on first prompt
	filtered = FilterHistory(sessions, "", "react", 0)
	if len(filtered) != 1 {
		t.Errorf("Search query (first prompt): expected 1 result, got %d", len(filtered))
	}

	// Test search query on last prompt (case insensitive)
	filtered = FilterHistory(sessions, "", "ALL DONE", 0)
	if len(filtered) != 1 {
		t.Errorf("Search query (last prompt): expected 1 result, got %d", len(filtered))
	}

	// Test since duration
	filtered = FilterHistory(sessions, "", "", 24*time.Hour)
	if len(filtered) != 2 {
		t.Errorf("Since filter (24h): expected 2 results, got %d", len(filtered))
	}

	// Test combined filters
	filtered = FilterHistory(sessions, "project-a", "rest api", 24*time.Hour)
	if len(filtered) != 1 {
		t.Errorf("Combined filters: expected 1 result, got %d", len(filtered))
	}
	if len(filtered) > 0 && filtered[0].SessionID != "sess-3" {
		t.Errorf("Combined filters: expected sess-3, got %q", filtered[0].SessionID)
	}

	// Test no filters returns all
	filtered = FilterHistory(sessions, "", "", 0)
	if len(filtered) != 3 {
		t.Errorf("No filters: expected 3 results, got %d", len(filtered))
	}
}

func TestLoadOrRefreshIndex(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")

	// Create a project directory with a session file
	projectDir := filepath.Join(tmpDir, "projects", "-home-testuser")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("Failed to create project dir: %v", err)
	}

	jsonl := `{"type":"system","sessionId":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","slug":"test-slug","timestamp":"2026-03-01T10:00:00Z"}
{"type":"user","message":{"role":"user","content":"Hello"},"timestamp":"2026-03-01T10:00:01Z"}
`
	if err := os.WriteFile(filepath.Join(projectDir, "a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl"), []byte(jsonl), 0644); err != nil {
		t.Fatalf("Failed to write session file: %v", err)
	}

	// First call: should scan and create cache
	index1, err := LoadOrRefreshIndex(tmpDir, cacheDir)
	if err != nil {
		t.Fatalf("First LoadOrRefreshIndex failed: %v", err)
	}
	if index1 == nil {
		t.Fatal("Expected non-nil index")
	}
	if len(index1.Sessions) != 1 {
		t.Errorf("Expected 1 session, got %d", len(index1.Sessions))
	}
	if index1.Version != 1 {
		t.Errorf("Expected version 1, got %d", index1.Version)
	}

	// Verify cache file was created
	cachePath := filepath.Join(cacheDir, "history-index.json")
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		t.Fatal("Cache file was not created")
	}

	// Second call: should load from cache (within 5 minutes)
	index2, err := LoadOrRefreshIndex(tmpDir, cacheDir)
	if err != nil {
		t.Fatalf("Second LoadOrRefreshIndex failed: %v", err)
	}
	if len(index2.Sessions) != 1 {
		t.Errorf("Cached index: expected 1 session, got %d", len(index2.Sessions))
	}

	// Verify the timestamps match (came from cache, not re-scanned)
	if !index1.BuiltAt.Equal(index2.BuiltAt) {
		t.Error("Second call should have returned cached index with same BuiltAt")
	}
}

func TestLoadOrRefreshIndexExpiredCache(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")

	// Create a project directory with a session
	projectDir := filepath.Join(tmpDir, "projects", "-home-testuser")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("Failed to create project dir: %v", err)
	}
	jsonl := `{"type":"system","sessionId":"a1b2c3d4-e5f6-7890-abcd-ef1234567890","slug":"test","timestamp":"2026-03-01T10:00:00Z"}
{"type":"user","message":{"role":"user","content":"Hello"},"timestamp":"2026-03-01T10:00:01Z"}
`
	if err := os.WriteFile(filepath.Join(projectDir, "a1b2c3d4-e5f6-7890-abcd-ef1234567890.jsonl"), []byte(jsonl), 0644); err != nil {
		t.Fatalf("Failed to write session file: %v", err)
	}

	// Pre-populate cache with old timestamp
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("Failed to create cache dir: %v", err)
	}
	oldIndex := HistoryIndex{
		Version: 1,
		BuiltAt: time.Now().Add(-10 * time.Minute), // 10 minutes ago
	}
	oldData, _ := json.Marshal(oldIndex)
	cachePath := filepath.Join(cacheDir, "history-index.json")
	if err := os.WriteFile(cachePath, oldData, 0644); err != nil {
		t.Fatalf("Failed to write old cache: %v", err)
	}
	// Set file mtime to 10 minutes ago
	oldTime := time.Now().Add(-10 * time.Minute)
	os.Chtimes(cachePath, oldTime, oldTime)

	// Should rebuild because cache is older than 5 minutes
	index, err := LoadOrRefreshIndex(tmpDir, cacheDir)
	if err != nil {
		t.Fatalf("LoadOrRefreshIndex failed: %v", err)
	}
	if len(index.Sessions) != 1 {
		t.Errorf("Expected 1 session after refresh, got %d", len(index.Sessions))
	}
	// BuiltAt should be recent (rebuilt, not from old cache)
	if time.Since(index.BuiltAt) > 5*time.Second {
		t.Error("Index should have been rebuilt with recent BuiltAt")
	}
}

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain string content",
			input:    `"Hello, world!"`,
			expected: "Hello, world!",
		},
		{
			name:     "array of text blocks",
			input:    `[{"type":"text","text":"First part"},{"type":"text","text":"Second part"}]`,
			expected: "First part\nSecond part",
		},
		{
			name:     "array with mixed types",
			input:    `[{"type":"text","text":"Visible"},{"type":"tool_use","id":"123"}]`,
			expected: "Visible",
		},
		{
			name:     "empty array",
			input:    `[]`,
			expected: "",
		},
		{
			name:     "empty string",
			input:    `""`,
			expected: "",
		},
		{
			name:     "null",
			input:    `null`,
			expected: "",
		},
		{
			name:     "single text block",
			input:    `[{"type":"text","text":"Only block"}]`,
			expected: "Only block",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractTextContent(json.RawMessage(tc.input))
			if result != tc.expected {
				t.Errorf("extractTextContent(%s) = %q, want %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestDirNameToProjectPath(t *testing.T) {
	tests := []struct {
		dirName  string
		expected string
	}{
		{"-home-skhoury", "/home/skhoury"},
		{"-Users-master-Code", "/Users/master/Code"},
		{"", ""},
		{"-home-user-my-project", "/home/user/my/project"},
	}

	for _, tc := range tests {
		result := dirNameToProjectPath(tc.dirName)
		if result != tc.expected {
			t.Errorf("dirNameToProjectPath(%q) = %q, want %q", tc.dirName, result, tc.expected)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is longer than ten", 10, "this is lo..."},
		{"", 10, ""},
	}

	for _, tc := range tests {
		result := truncate(tc.input, tc.maxLen)
		if result != tc.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.maxLen, result, tc.expected)
		}
	}
}

func TestFilterHistoryNoMatch(t *testing.T) {
	sessions := []HistoricalSession{
		{
			SessionID:    "sess-1",
			Slug:         "test-slug",
			ProjectPath:  "/home/user/project",
			FirstPrompt:  "Hello",
			LastPrompt:   "Bye",
			LastModified: time.Now(),
		},
	}

	// Search that matches nothing
	filtered := FilterHistory(sessions, "", "nonexistent-query-xyz", 0)
	if len(filtered) != 0 {
		t.Errorf("Expected 0 results for non-matching query, got %d", len(filtered))
	}

	// Project filter that matches nothing
	filtered = FilterHistory(sessions, "nonexistent-project", "", 0)
	if len(filtered) != 0 {
		t.Errorf("Expected 0 results for non-matching project, got %d", len(filtered))
	}
}
