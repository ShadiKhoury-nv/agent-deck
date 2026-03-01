package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/platform"
)

var procLog = logging.ForComponent(logging.CompSession)

// ExternalProcess represents an agent process discovered via /proc or ps.
type ExternalProcess struct {
	PID         int
	SessionID   string // CLAUDE_SESSION_ID from /proc/<pid>/environ
	ProjectPath string // from /proc/<pid>/cwd symlink
	TTY         string // terminal device
	Tool        string // "claude", "gemini", "codex", "opencode"
	Slug        string // from JSONL enrichment
	LastPrompt  string // last user message from JSONL
	StartTime   time.Time
	IsManaged   bool // true if AGENTDECK_INSTANCE_ID is set
}

// knownAgentTools lists the binary basenames we consider agent processes.
var knownAgentTools = []string{"claude", "gemini", "codex", "opencode"}

// DiscoverRunningProcesses scans for running agent processes.
// On Linux/WSL it reads /proc; on macOS it falls back to ps(1).
// managedSessionIDs contains session IDs already managed by agent-deck;
// processes with AGENTDECK_INSTANCE_ID set are marked IsManaged=true.
func DiscoverRunningProcesses(managedSessionIDs map[string]bool) ([]ExternalProcess, error) {
	p := platform.Detect()
	switch p {
	case platform.PlatformLinux, platform.PlatformWSL1, platform.PlatformWSL2:
		return discoverViaProc(managedSessionIDs)
	case platform.PlatformMacOS:
		return discoverViaPSCommand()
	default:
		procLog.Debug("proc discovery not supported on this platform", slog.String("platform", string(p)))
		return nil, nil
	}
}

// discoverViaProc scans /proc for agent processes (Linux/WSL).
func discoverViaProc(managedSessionIDs map[string]bool) ([]ExternalProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("failed to read /proc: %w", err)
	}

	selfPID := os.Getpid()
	var results []ExternalProcess

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue // not a PID directory
		}

		if pid == selfPID {
			continue
		}

		proc, ok := inspectProcPID(pid, managedSessionIDs)
		if !ok {
			continue
		}

		results = append(results, proc)
	}

	return results, nil
}

// inspectProcPID reads /proc/<pid>/* to determine if the process is an agent
// and extracts metadata. Returns (process, true) if it is an agent process.
func inspectProcPID(pid int, managedSessionIDs map[string]bool) (ExternalProcess, bool) {
	pidDir := fmt.Sprintf("/proc/%d", pid)

	// Read cmdline to identify the process
	cmdlineBytes, err := os.ReadFile(filepath.Join(pidDir, "cmdline"))
	if err != nil {
		// Permission denied or process gone — normal, skip silently
		return ExternalProcess{}, false
	}

	args := parseProcCmdline(cmdlineBytes)
	if len(args) == 0 {
		return ExternalProcess{}, false
	}

	// Skip sandbox wrappers — bwrap cmdlines contain "/tmp/claude" as a directory
	// path (e.g., --bind /tmp/claude /tmp/claude), which false-matches "claude" basename.
	firstBase := filepath.Base(args[0])
	if firstBase == "bwrap" || firstBase == "socat" || firstBase == "bash" || firstBase == "sh" {
		return ExternalProcess{}, false
	}

	tool, found := isAgentProcess(args)
	if !found {
		return ExternalProcess{}, false
	}

	proc := ExternalProcess{
		PID:  pid,
		Tool: tool,
	}

	// Read environment variables
	environBytes, err := os.ReadFile(filepath.Join(pidDir, "environ"))
	if err != nil {
		procLog.Debug("cannot read environ", slog.Int("pid", pid), slog.String("error", err.Error()))
	} else {
		envVars := parseProcEnviron(environBytes)
		proc.SessionID = extractSessionID(envVars, tool)
		if _, ok := envVars["AGENTDECK_INSTANCE_ID"]; ok {
			proc.IsManaged = true
		}
	}

	// Read cwd symlink
	cwdPath, err := os.Readlink(filepath.Join(pidDir, "cwd"))
	if err != nil {
		procLog.Debug("cannot read cwd", slog.Int("pid", pid), slog.String("error", err.Error()))
	} else {
		proc.ProjectPath = cwdPath
	}

	// Read TTY from fd/0 symlink
	fd0Path, err := os.Readlink(filepath.Join(pidDir, "fd", "0"))
	if err == nil && strings.Contains(fd0Path, "pts/") {
		proc.TTY = fd0Path
	}

	// Process start time from /proc/<pid> stat
	info, err := os.Stat(pidDir)
	if err == nil {
		proc.StartTime = info.ModTime()
	}

	// Enrich from JSONL if we have a session ID and project path
	if proc.SessionID != "" && proc.ProjectPath != "" {
		slug, lastPrompt := enrichFromJSONL(proc.ProjectPath, proc.SessionID)
		proc.Slug = slug
		proc.LastPrompt = lastPrompt
	}

	return proc, true
}

// parseProcCmdline splits a null-delimited /proc/<pid>/cmdline into args.
func parseProcCmdline(data []byte) []string {
	if len(data) == 0 {
		return nil
	}

	// Remove trailing null byte if present
	if data[len(data)-1] == 0 {
		data = data[:len(data)-1]
	}

	var args []string
	for _, chunk := range splitNull(data) {
		if chunk != "" {
			args = append(args, chunk)
		}
	}
	return args
}

// parseProcEnviron parses null-delimited /proc/<pid>/environ into a map.
func parseProcEnviron(data []byte) map[string]string {
	env := make(map[string]string)
	for _, entry := range splitNull(data) {
		if idx := strings.IndexByte(entry, '='); idx > 0 {
			env[entry[:idx]] = entry[idx+1:]
		}
	}
	return env
}

// splitNull splits data on null bytes and returns non-empty strings.
func splitNull(data []byte) []string {
	var parts []string
	for _, part := range strings.Split(string(data), "\x00") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

// isAgentProcess checks if any arg's basename matches a known agent tool.
// Returns (toolName, true) if found.
func isAgentProcess(args []string) (string, bool) {
	// Only check the first arg (the executable) and any arg that starts with
	// a typical bin path. Avoid matching directory paths like "/tmp/claude".
	for i, arg := range args {
		base := filepath.Base(arg)
		for _, tool := range knownAgentTools {
			if base == tool {
				// For arg[0], always trust it (it's the executable)
				if i == 0 {
					return tool, true
				}
				// For other args, only match if the arg looks like an executable path
				// (contains /bin/ or /node_modules/.bin/) not a plain directory
				if strings.Contains(arg, "/bin/") || strings.Contains(arg, "node_modules") {
					return tool, true
				}
				// Also match bare command names (no path component)
				if !strings.Contains(arg, "/") {
					return tool, true
				}
			}
		}
	}
	return "", false
}

// extractSessionID extracts the session ID from environment variables.
// Different tools use different env var names.
func extractSessionID(envVars map[string]string, tool string) string {
	// Try tool-specific env var first
	switch tool {
	case "claude":
		if id, ok := envVars["CLAUDE_SESSION_ID"]; ok {
			return id
		}
	case "gemini":
		if id, ok := envVars["GEMINI_SESSION_ID"]; ok {
			return id
		}
	case "codex":
		if id, ok := envVars["CODEX_SESSION_ID"]; ok {
			return id
		}
	case "opencode":
		if id, ok := envVars["OPENCODE_SESSION_ID"]; ok {
			return id
		}
	}

	// Fall back to generic
	if id, ok := envVars["AGENT_SESSION_ID"]; ok {
		return id
	}

	return ""
}

// enrichFromJSONL reads the session JSONL file to extract the slug and last user prompt.
// Returns ("", "") if the file cannot be read or parsed.
func enrichFromJSONL(projectPath, sessionID string) (string, string) {
	configDir := GetClaudeConfigDir()
	dirName := ConvertToClaudeDirName(projectPath)
	jsonlPath := filepath.Join(configDir, "projects", dirName, sessionID+".jsonl")

	f, err := os.Open(jsonlPath)
	if err != nil {
		procLog.Debug("cannot open JSONL for enrichment",
			slog.String("path", jsonlPath),
			slog.String("error", err.Error()))
		return "", ""
	}
	defer f.Close()

	var slug, lastPrompt string
	scanner := bufio.NewScanner(f)
	linesRead := 0
	maxLines := 20

	for scanner.Scan() && linesRead < maxLines {
		linesRead++
		line := scanner.Bytes()

		var entry procJSONLEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		switch entry.Type {
		case "system":
			if entry.Slug != "" {
				slug = entry.Slug
			}
		case "user":
			if entry.UserMessage != "" {
				lastPrompt = entry.UserMessage
			}
		}
	}

	return slug, lastPrompt
}

// procJSONLEntry represents the fields we care about in a JSONL session file
// for process discovery enrichment. Separate from jsonlEntry in analytics.go
// which has a different structure focused on token usage.
type procJSONLEntry struct {
	Type        string `json:"type"`
	Slug        string `json:"slug,omitempty"`
	UserMessage string `json:"message,omitempty"`
}

// discoverViaPSCommand discovers agent processes via ps(1) on macOS.
// Less information is available compared to /proc: no env vars, so SessionID will be empty.
func discoverViaPSCommand() ([]ExternalProcess, error) {
	out, err := exec.Command("ps", "-eo", "pid,comm,tty").Output()
	if err != nil {
		return nil, fmt.Errorf("ps command failed: %w", err)
	}

	selfPID := os.Getpid()
	var results []ExternalProcess

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	// Skip header line
	if scanner.Scan() {
		// consumed header
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == selfPID {
			continue
		}

		comm := fields[1]
		base := filepath.Base(comm)

		var matchedTool string
		for _, tool := range knownAgentTools {
			if base == tool {
				matchedTool = tool
				break
			}
		}
		if matchedTool == "" {
			continue
		}

		proc := ExternalProcess{
			PID:  pid,
			Tool: matchedTool,
		}

		if len(fields) >= 3 {
			proc.TTY = fields[2]
		}

		// Try to get CWD via lsof on macOS
		cwd := getCWDVialsof(pid)
		if cwd != "" {
			proc.ProjectPath = cwd
		}

		// Try to get start time from ps
		proc.StartTime = time.Now() // approximate; ps start time parsing is unreliable

		results = append(results, proc)
	}

	return results, nil
}

// getCWDVialsof attempts to read the current working directory of a process on macOS.
func getCWDVialsof(pid int) string {
	if runtime.GOOS != "darwin" {
		return ""
	}

	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-Fn").Output()
	if err != nil {
		procLog.Debug("lsof failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		return ""
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	foundCwd := false
	for scanner.Scan() {
		line := scanner.Text()
		// lsof -Fn outputs "fcwd" for the current directory field, then "n<path>"
		if line == "fcwd" {
			foundCwd = true
			continue
		}
		if foundCwd && strings.HasPrefix(line, "n") {
			return line[1:] // strip the "n" prefix
		}
		if foundCwd {
			foundCwd = false // reset if next line wasn't the path
		}
	}

	return ""
}
