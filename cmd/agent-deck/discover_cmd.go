package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// handleDiscover dispatches discover subcommands
func handleDiscover(profile string, args []string) {
	if len(args) > 0 && args[0] == "adopt" {
		handleDiscoverAdopt(profile, args[1:])
		return
	}

	if len(args) > 0 {
		switch args[0] {
		case "help", "--help", "-h":
			printDiscoverHelp()
			return
		}
	}

	handleDiscoverList(profile, args)
}

// handleDiscoverList lists external agent processes not managed by agent-deck
func handleDiscoverList(profile string, args []string) {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	quiet := fs.Bool("quiet", false, "Minimal output (PIDs only)")
	quietShort := fs.Bool("q", false, "Minimal output (PIDs only)")

	fs.Usage = func() {
		printDiscoverHelp()
	}

	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	quietMode := *quiet || *quietShort

	// Load existing instances to build managedSessionIDs map
	managedSessionIDs := make(map[string]bool)
	_, instances, _, err := loadSessionData(profile)
	if err == nil {
		for _, inst := range instances {
			if inst.ClaudeSessionID != "" {
				managedSessionIDs[inst.ClaudeSessionID] = true
			}
		}
	}

	// Discover running processes
	processes, err := session.DiscoverRunningProcesses(managedSessionIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Filter to only unmanaged processes
	var external []session.ExternalProcess
	for _, p := range processes {
		if !p.IsManaged {
			external = append(external, p)
		}
	}

	// JSON output
	if *jsonOutput {
		data, marshalErr := json.MarshalIndent(external, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", marshalErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	// Quiet output: PIDs only
	if quietMode {
		for _, p := range external {
			fmt.Println(p.PID)
		}
		return
	}

	// Default: formatted table
	if len(external) == 0 {
		fmt.Println("No external agent sessions found.")
		return
	}

	fmt.Println("External Claude Sessions (not managed by agent-deck):")
	fmt.Println()
	fmt.Printf("  %-8s %-9s %-9s %-21s %-21s %s\n", "PID", "Tool", "TTY", "Project", "Session", "Age")

	for _, p := range external {
		project := shortenPath(p.ProjectPath, 20)
		slug := p.Slug
		if slug == "" {
			slug = "(unknown)"
		}
		if len(slug) > 20 {
			slug = slug[:17] + "..."
		}
		tty := p.TTY
		if tty == "" {
			tty = "-"
		}

		age := formatAge(p.StartTime)

		fmt.Printf("  %-8d %-9s %-9s %-21s %-21s %s\n",
			p.PID, p.Tool, tty, project, slug, age)
	}

	fmt.Println()
	fmt.Printf("Found %d external session(s). Use 'agent-deck discover adopt <pid>' to manage them.\n", len(external))
}

// handleDiscoverAdopt imports an external process into agent-deck management
func handleDiscoverAdopt(profile string, args []string) {
	fs := flag.NewFlagSet("discover adopt", flag.ExitOnError)
	title := fs.String("title", "", "Session title")
	titleShort := fs.String("t", "", "Session title (short)")
	group := fs.String("group", "", "Group path")
	groupShort := fs.String("g", "", "Group path (short)")

	fs.Usage = func() {
		fmt.Println("Usage: agent-deck discover adopt <pid> [options]")
		fmt.Println()
		fmt.Println("Import an external agent process into agent-deck management.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}

	pidStr := fs.Arg(0)
	if pidStr == "" {
		fmt.Fprintln(os.Stderr, "Error: PID is required")
		fmt.Fprintln(os.Stderr, "Usage: agent-deck discover adopt <pid>")
		os.Exit(1)
	}

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid PID: %s\n", pidStr)
		os.Exit(1)
	}

	// Discover all processes (nil = don't filter by managed)
	processes, err := session.DiscoverRunningProcesses(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Find the matching process
	var target *session.ExternalProcess
	for i := range processes {
		if processes[i].PID == pid {
			target = &processes[i]
			break
		}
	}

	if target == nil {
		fmt.Fprintf(os.Stderr, "Error: No agent process found with PID %d\n", pid)
		os.Exit(1)
	}

	// Determine title
	sessionTitle := mergeFlags(*title, *titleShort)
	if sessionTitle == "" {
		// Auto-generate from tool + project
		base := "session"
		if target.ProjectPath != "" {
			base = shortenHome(target.ProjectPath)
		}
		sessionTitle = fmt.Sprintf("%s-%s", target.Tool, base)
	}

	// Determine group
	sessionGroup := mergeFlags(*group, *groupShort)

	// Create instance
	newInstance := session.NewInstanceWithGroupAndTool(sessionTitle, target.ProjectPath, sessionGroup, target.Tool)

	// Set Claude session ID if available
	if target.SessionID != "" {
		newInstance.ClaudeSessionID = target.SessionID
		newInstance.ClaudeDetectedAt = time.Now()
	}

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

	fmt.Printf("Adopted PID %d as session '%s' (ID: %s)\n", pid, sessionTitle, newInstance.ID)
}

// formatAge returns a human-readable age string like "2h ago", "15m ago", "3d ago"
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}

	d := time.Since(t)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// shortenPath shortens a path to fit within maxLen, replacing home with ~
func shortenPath(p string, maxLen int) string {
	p = shortenHome(p)
	if len(p) > maxLen {
		return "..." + p[len(p)-maxLen+3:]
	}
	return p
}

// shortenHome replaces the home directory prefix with ~
func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if len(p) > len(home) && p[:len(home)] == home {
		return "~" + p[len(home):]
	}
	if p == home {
		return "~"
	}
	return p
}

// printDiscoverHelp prints help for the discover command
func printDiscoverHelp() {
	fmt.Println("Usage: agent-deck discover [command]")
	fmt.Println()
	fmt.Println("Find agent sessions running outside of agent-deck.")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  (default)          List running agent processes not managed by agent-deck")
	fmt.Println("  adopt <pid>        Import an external process into agent-deck")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --json             Output as JSON")
	fmt.Println("  -q, --quiet        Minimal output (PIDs only)")
	fmt.Println()
	fmt.Println("Adopt Options:")
	fmt.Println("  -t, --title <name>  Session title (default: auto-generated from tool+project)")
	fmt.Println("  -g, --group <path>  Group path (default: auto from project path)")
}
