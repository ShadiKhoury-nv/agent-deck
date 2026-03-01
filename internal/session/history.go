package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
)

var historyLog = logging.ForComponent(logging.CompSession)

// uuidSessionPattern matches UUID-named .jsonl files (excludes agent-*.jsonl)
var uuidSessionPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

// HistoricalSession represents a session from Claude's history files
type HistoricalSession struct {
	SessionID     string    `json:"session_id"`
	Slug          string    `json:"slug"`
	ProjectPath   string    `json:"project_path"`
	ProjectDir    string    `json:"project_dir"`
	FilePath      string    `json:"file_path"`
	FileSize      int64     `json:"file_size"`
	CreatedAt     time.Time `json:"created_at"`
	LastModified  time.Time `json:"last_modified"`
	MessageCount  int       `json:"message_count"`
	FirstPrompt   string    `json:"first_prompt"`
	LastPrompt    string    `json:"last_prompt"`
	GitBranch     string    `json:"git_branch,omitempty"`
	ClaudeVersion string    `json:"claude_version,omitempty"`
}

// HistoryIndex holds a cached index of all historical sessions
type HistoryIndex struct {
	Version  int                 `json:"version"`
	BuiltAt  time.Time           `json:"built_at"`
	Sessions []HistoricalSession `json:"sessions"`
}

// ScanSessionHistory walks the Claude projects directory and returns all historical sessions
func ScanSessionHistory(claudeDir string) ([]HistoricalSession, error) {
	projectsDir := filepath.Join(claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading projects directory: %w", err)
	}

	var sessions []HistoricalSession

	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}

		dirName := dirEntry.Name()
		dirPath := filepath.Join(projectsDir, dirName)

		files, globErr := filepath.Glob(filepath.Join(dirPath, "*.jsonl"))
		if globErr != nil {
			continue
		}

		projectPath := dirNameToProjectPath(dirName)

		for _, filePath := range files {
			baseName := filepath.Base(filePath)

			if !uuidSessionPattern.MatchString(baseName) {
				continue
			}

			info, statErr := os.Stat(filePath)
			if statErr != nil {
				continue
			}

			sessionID := strings.TrimSuffix(baseName, ".jsonl")

			sess := HistoricalSession{
				SessionID:    sessionID,
				ProjectPath:  projectPath,
				ProjectDir:   dirName,
				FilePath:     filePath,
				FileSize:     info.Size(),
				LastModified: info.ModTime(),
			}

			enrichHistoricalSession(&sess)

			sessions = append(sessions, sess)
		}
	}

	// Sort by LastModified descending (most recent first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastModified.After(sessions[j].LastModified)
	})

	return sessions, nil
}

// enrichHistoricalSession reads the JSONL file to extract metadata
func enrichHistoricalSession(sess *HistoricalSession) {
	f, err := os.Open(sess.FilePath)
	if err != nil {
		return
	}
	defer f.Close()

	// Read first 30 lines to extract metadata
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024) // 1MB buffer for safety

	lineCount := 0
	headLines := 30
	firstUserFound := false

	for scanner.Scan() && lineCount < headLines {
		lineCount++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var record historyRecord
		if jsonErr := json.Unmarshal(line, &record); jsonErr != nil {
			continue
		}

		// Extract slug from system entries
		if sess.Slug == "" && record.Slug != "" {
			sess.Slug = record.Slug
		}

		// Extract version
		if sess.ClaudeVersion == "" && record.Version != "" {
			sess.ClaudeVersion = record.Version
		}

		// Extract git branch
		if sess.GitBranch == "" && record.GitBranch != "" {
			sess.GitBranch = record.GitBranch
		}

		// Extract CreatedAt from first timestamp
		if sess.CreatedAt.IsZero() && record.Timestamp != "" {
			if t, parseErr := time.Parse(time.RFC3339, record.Timestamp); parseErr == nil {
				sess.CreatedAt = t
			}
		}

		// Extract first user prompt
		if !firstUserFound && record.Type == "user" && len(record.Message) > 0 {
			text := extractUserText(record.Message)
			if text != "" {
				sess.FirstPrompt = truncate(text, 200)
				firstUserFound = true
			}
		}
	}

	// Count total lines (approximate message count)
	// Continue scanning from where we left off
	for scanner.Scan() {
		lineCount++
	}
	sess.MessageCount = lineCount

	// Extract last prompt from the tail of the file
	sess.LastPrompt = extractLastPrompt(sess.FilePath)
}

// extractLastPrompt reads the last 20KB of a file and finds the last user message
func extractLastPrompt(filePath string) string {
	f, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer f.Close()

	const tailSize = 20 * 1024

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	offset := int64(0)
	if info.Size() > tailSize {
		offset = info.Size() - tailSize
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	// If we seeked to the middle of a file, skip the first partial line
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	if offset > 0 {
		// Skip the first (likely partial) line
		scanner.Scan()
	}

	var lastUserText string
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var record historyRecord
		if jsonErr := json.Unmarshal(line, &record); jsonErr != nil {
			continue
		}

		if record.Type == "user" && len(record.Message) > 0 {
			text := extractUserText(record.Message)
			if text != "" {
				lastUserText = text
			}
		}
	}

	return truncate(lastUserText, 200)
}

// historyRecord represents a JSONL line for history scanning
type historyRecord struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Slug      string          `json:"slug"`
	Timestamp string          `json:"timestamp"`
	Version   string          `json:"version"`
	GitBranch string          `json:"gitBranch"`
	Message   json.RawMessage `json:"message"`
}

// extractUserText extracts text content from a message field
func extractUserText(raw json.RawMessage) string {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	return extractTextContent(msg.Content)
}

// extractTextContent handles both string and array content formats from Claude JSONL
func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try as plain string first
	var contentStr string
	if err := json.Unmarshal(raw, &contentStr); err == nil {
		return contentStr
	}

	// Try as array of content blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// truncate shortens a string to maxLen characters, appending "..." if truncated
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// dirNameToProjectPath converts a Claude project directory name back to a path.
// The directory name format replaces "/" with "-" (e.g., "-home-skhoury" for "/home/skhoury").
// This conversion is lossy for paths that contain actual hyphens.
func dirNameToProjectPath(dirName string) string {
	if dirName == "" {
		return ""
	}
	// Replace leading "-" with "/", then remaining "-" with "/"
	if strings.HasPrefix(dirName, "-") {
		return "/" + strings.ReplaceAll(dirName[1:], "-", "/")
	}
	return strings.ReplaceAll(dirName, "-", "/")
}

// LoadOrRefreshIndex loads the cached history index or rebuilds it if stale
func LoadOrRefreshIndex(claudeDir, cacheDir string) (*HistoryIndex, error) {
	cachePath := filepath.Join(cacheDir, "history-index.json")

	// Check if cache exists and is fresh (< 5 minutes old)
	if info, err := os.Stat(cachePath); err == nil {
		if time.Since(info.ModTime()) < 5*time.Minute {
			data, readErr := os.ReadFile(cachePath)
			if readErr == nil {
				var index HistoryIndex
				if jsonErr := json.Unmarshal(data, &index); jsonErr == nil {
					return &index, nil
				}
			}
		}
	}

	// Cache is missing or stale — rebuild
	sessions, err := ScanSessionHistory(claudeDir)
	if err != nil {
		return nil, fmt.Errorf("scanning session history: %w", err)
	}

	index := &HistoryIndex{
		Version:  1,
		BuiltAt:  time.Now(),
		Sessions: sessions,
	}

	// Write cache atomically
	if mkErr := os.MkdirAll(cacheDir, 0755); mkErr != nil {
		return index, nil // Return index even if caching fails
	}

	data, marshalErr := json.Marshal(index)
	if marshalErr != nil {
		return index, nil
	}

	tmpPath := cachePath + ".tmp"
	if writeErr := os.WriteFile(tmpPath, data, 0644); writeErr != nil {
		return index, nil
	}

	if renameErr := os.Rename(tmpPath, cachePath); renameErr != nil {
		os.Remove(tmpPath)
		return index, nil
	}

	return index, nil
}

// FilterHistory filters sessions by project, search query, and time range
func FilterHistory(sessions []HistoricalSession, projectFilter, searchQuery string, since time.Duration) []HistoricalSession {
	if projectFilter == "" && searchQuery == "" && since == 0 {
		return sessions
	}

	projectFilterLower := strings.ToLower(projectFilter)
	searchQueryLower := strings.ToLower(searchQuery)
	var cutoff time.Time
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}

	var filtered []HistoricalSession
	for _, sess := range sessions {
		// Project filter
		if projectFilter != "" {
			if !strings.Contains(strings.ToLower(sess.ProjectPath), projectFilterLower) {
				continue
			}
		}

		// Search query — fuzzy match on slug, first prompt, last prompt
		if searchQuery != "" {
			slugMatch := strings.Contains(strings.ToLower(sess.Slug), searchQueryLower)
			firstMatch := strings.Contains(strings.ToLower(sess.FirstPrompt), searchQueryLower)
			lastMatch := strings.Contains(strings.ToLower(sess.LastPrompt), searchQueryLower)
			if !slugMatch && !firstMatch && !lastMatch {
				continue
			}
		}

		// Time range filter
		if since > 0 && sess.LastModified.Before(cutoff) {
			continue
		}

		filtered = append(filtered, sess)
	}

	return filtered
}
