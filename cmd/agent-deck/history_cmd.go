package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// handleHistory dispatches history subcommands
func handleHistory(profile string, args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "resume":
			handleHistoryResume(args[1:])
			return
		case "import":
			handleHistoryImport(profile, args[1:])
			return
		case "help", "--help", "-h":
			printHistoryHelp()
			return
		}
	}

	handleHistoryList(profile, args)
}

// handleHistoryList lists historical Claude sessions
func handleHistoryList(profile string, args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	projectFilter := fs.String("project", "", "Filter by project path")
	since := fs.String("since", "", "Show sessions from last N (e.g., 7d, 24h, 1w)")
	search := fs.String("search", "", "Search in slugs and prompts")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	limit := fs.Int("limit", 20, "Max results")

	fs.Usage = func() {
		printHistoryHelp()
	}

	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	// Get Claude config dir
	claudeDir := session.GetClaudeConfigDir()

	// Get agent-deck config dir for cache
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	agentDeckDir := filepath.Join(home, ".agent-deck")

	// Load or refresh index
	index, err := session.LoadOrRefreshIndex(claudeDir, agentDeckDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Parse --since duration
	var sinceDuration time.Duration
	if *since != "" {
		var parseErr error
		sinceDuration, parseErr = parseDuration(*since)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid duration '%s': %v\n", *since, parseErr)
			fmt.Fprintln(os.Stderr, "Examples: 7d, 24h, 30m, 1w")
			os.Exit(1)
		}
	}

	// Apply filters
	sessions := session.FilterHistory(index.Sessions, *projectFilter, *search, sinceDuration)

	totalCount := len(sessions)

	// Apply limit
	if *limit > 0 && len(sessions) > *limit {
		sessions = sessions[:*limit]
	}

	// JSON output
	if *jsonOutput {
		data, marshalErr := json.MarshalIndent(sessions, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", marshalErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	// Default: formatted table grouped by project
	if len(sessions) == 0 {
		fmt.Println("No session history found.")
		return
	}

	fmt.Printf("Session History (showing %d of %d):\n", len(sessions), totalCount)
	fmt.Println()

	// Group by project
	type projectGroup struct {
		project  string
		sessions []session.HistoricalSession
	}

	projectOrder := []string{}
	projectMap := make(map[string][]session.HistoricalSession)

	for _, sess := range sessions {
		proj := shortenHome(sess.ProjectPath)
		if proj == "" {
			proj = "(unknown)"
		}
		if _, exists := projectMap[proj]; !exists {
			projectOrder = append(projectOrder, proj)
		}
		projectMap[proj] = append(projectMap[proj], sess)
	}

	for _, proj := range projectOrder {
		group := projectMap[proj]
		fmt.Printf("  %s (%d sessions)\n", proj, len(group))

		for _, sess := range group {
			shortID := sess.SessionID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}

			slug := sess.Slug
			if slug == "" {
				slug = "(no slug)"
			}
			if len(slug) > 20 {
				slug = slug[:17] + "..."
			}

			prompt := sess.FirstPrompt
			if prompt == "" {
				prompt = sess.LastPrompt
			}
			// Clean up prompt for display (single line, truncated)
			prompt = strings.ReplaceAll(prompt, "\n", " ")
			prompt = strings.TrimSpace(prompt)
			if len(prompt) > 40 {
				prompt = prompt[:37] + "..."
			}

			age := formatAge(sess.LastModified)

			promptDisplay := ""
			if prompt != "" {
				promptDisplay = fmt.Sprintf("  \"%s\"", prompt)
			}

			fmt.Printf("    %8s  %s  %-20s%s\n", age, shortID, slug, promptDisplay)
		}

		fmt.Println()
	}

	fmt.Println("Use 'agent-deck history resume <id>' to resume a session.")
}

// handleHistoryResume resumes a historical session
func handleHistoryResume(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Error: session ID is required")
		fmt.Fprintln(os.Stderr, "Usage: agent-deck history resume <session-id>")
		os.Exit(1)
	}

	prefix := args[0]

	// Load index to find full session ID
	claudeDir := session.GetClaudeConfigDir()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	agentDeckDir := filepath.Join(home, ".agent-deck")

	index, err := session.LoadOrRefreshIndex(claudeDir, agentDeckDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Find session by prefix match
	sess := findSessionByPrefix(index.Sessions, prefix)
	if sess == nil {
		fmt.Fprintf(os.Stderr, "Error: no session found matching '%s'\n", prefix)
		os.Exit(1)
	}

	// Build claude resume command
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: 'claude' not found in PATH")
		os.Exit(1)
	}

	cmdArgs := []string{"claude", "--resume", sess.SessionID}

	// If explicit config dir, set env
	env := os.Environ()
	if session.IsClaudeConfigDirExplicit() {
		env = append(env, fmt.Sprintf("CLAUDE_CONFIG_DIR=%s", claudeDir))
	}

	// Replace the current process with claude --resume
	if err := syscall.Exec(claudeBin, cmdArgs, env); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to exec claude: %v\n", err)
		os.Exit(1)
	}
}

// handleHistoryImport creates a managed instance from a historical session
func handleHistoryImport(profile string, args []string) {
	fs := flag.NewFlagSet("history import", flag.ExitOnError)
	title := fs.String("title", "", "Session title")
	titleShort := fs.String("t", "", "Session title (short)")
	group := fs.String("group", "", "Group path")
	groupShort := fs.String("g", "", "Group path (short)")

	fs.Usage = func() {
		fmt.Println("Usage: agent-deck history import <session-id> [options]")
		fmt.Println()
		fmt.Println("Import a historical session into agent-deck management.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	prefix := fs.Arg(0)
	if prefix == "" {
		fmt.Fprintln(os.Stderr, "Error: session ID is required")
		fmt.Fprintln(os.Stderr, "Usage: agent-deck history import <session-id>")
		os.Exit(1)
	}

	// Load index
	claudeDir := session.GetClaudeConfigDir()
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	agentDeckDir := filepath.Join(home, ".agent-deck")

	index, err := session.LoadOrRefreshIndex(claudeDir, agentDeckDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Find session by prefix
	sess := findSessionByPrefix(index.Sessions, prefix)
	if sess == nil {
		fmt.Fprintf(os.Stderr, "Error: no session found matching '%s'\n", prefix)
		os.Exit(1)
	}

	// Determine title
	sessionTitle := mergeFlags(*title, *titleShort)
	if sessionTitle == "" {
		if sess.Slug != "" {
			sessionTitle = sess.Slug
		} else {
			sessionTitle = fmt.Sprintf("imported-%s", sess.SessionID[:8])
		}
	}

	// Determine group
	sessionGroup := mergeFlags(*group, *groupShort)

	// Create instance
	projectPath := sess.ProjectPath
	newInstance := session.NewInstanceWithGroupAndTool(sessionTitle, projectPath, sessionGroup, "claude")

	// Set Claude session ID
	newInstance.ClaudeSessionID = sess.SessionID
	newInstance.ClaudeDetectedAt = time.Now()

	// Load and save
	storage, instances, groups, err := loadSessionData(profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	instances = append(instances, newInstance)
	groupTree := session.NewGroupTreeWithGroups(instances, groups)
	if newInstance.GroupPath != "" {
		groupTree.CreateGroup(newInstance.GroupPath)
	}

	if err := storage.SaveWithGroups(instances, groupTree); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to save session: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Imported session '%s' (ID: %s, Claude session: %s)\n",
		sessionTitle, newInstance.ID, sess.SessionID[:8])
}

// findSessionByPrefix finds a historical session by prefix match on session ID
func findSessionByPrefix(sessions []session.HistoricalSession, prefix string) *session.HistoricalSession {
	prefix = strings.ToLower(prefix)

	var matches []session.HistoricalSession
	for _, sess := range sessions {
		if strings.HasPrefix(strings.ToLower(sess.SessionID), prefix) {
			matches = append(matches, sess)
		}
	}

	if len(matches) == 0 {
		return nil
	}

	if len(matches) == 1 {
		return &matches[0]
	}

	// Multiple matches — return the most recent
	best := matches[0]
	for _, m := range matches[1:] {
		if m.LastModified.After(best.LastModified) {
			best = m
		}
	}
	return &best
}

// parseDuration parses duration strings like "7d", "24h", "30m", "1w"
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	// Check for our custom suffixes first
	lastChar := s[len(s)-1]
	numStr := s[:len(s)-1]

	switch lastChar {
	case 'w', 'W':
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return 0, fmt.Errorf("invalid number: %s", numStr)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	case 'd', 'D':
		n, err := strconv.Atoi(numStr)
		if err != nil {
			return 0, fmt.Errorf("invalid number: %s", numStr)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		// Fall back to Go's time.ParseDuration for h, m, s, etc.
		return time.ParseDuration(s)
	}
}

// printHistoryHelp prints help for the history command
func printHistoryHelp() {
	fmt.Println("Usage: agent-deck history [command] [options]")
	fmt.Println()
	fmt.Println("Browse and resume historical Claude sessions.")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  (default)              List historical sessions")
	fmt.Println("  resume <session-id>    Resume a historical session (prefix match)")
	fmt.Println("  import <session-id>    Import session into agent-deck management")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --project <path>      Filter by project path")
	fmt.Println("  --since <duration>    Show sessions from last N (e.g., 7d, 24h, 1w)")
	fmt.Println("  --search <query>      Search in slugs and prompts")
	fmt.Println("  --limit <N>           Max results (default: 20)")
	fmt.Println("  --json                Output as JSON")
}
