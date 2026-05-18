package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/fordjent/fordjent/internal/forgejo"
)

var (
	jsonOutput bool
	forgejoURL string
	repoOverride string
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Parse global flags
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-j", "--json":
			jsonOutput = true
			args = args[1:]
		case "-u", "--url":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "Error: --url requires argument")
				os.Exit(1)
			}
			forgejoURL = args[1]
			args = args[2:]
		case "-r", "--repo":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "Error: --repo requires argument")
				os.Exit(1)
			}
			repoOverride = args[1]
			args = args[2:]
		case "-h", "--help":
			printUsage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Error: unknown flag %s\n", args[0])
			os.Exit(1)
		}
	}

	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	// Get client
	client := getClient()
	ctx := context.Background()

	// Dispatch command
	cmd := args[0]
	cmdArgs := args[1:]

	switch cmd {
	case "version":
		cmdVersion(ctx, client)
	case "user":
		cmdUser(ctx, client)
	case "repo":
		cmdRepo(ctx, client, cmdArgs)
	case "issue":
		cmdIssue(ctx, client, cmdArgs)
	case "pr":
		cmdPR(ctx, client, cmdArgs)
	case "comment":
		cmdComment(ctx, client, cmdArgs)
	case "branch":
		cmdBranch(ctx, client, cmdArgs)
	case "hook":
		cmdHook(ctx, client, cmdArgs)
	case "label":
		cmdLabel(ctx, client, cmdArgs)
	case "reaction":
		cmdReaction(ctx, client, cmdArgs)
	case "file":
		cmdFile(ctx, client, cmdArgs)
	case "collab":
		cmdCollab(ctx, client, cmdArgs)
	case "init":
		cmdInit(cmdArgs)
	case "detect":
		cmdDetect()
	case "stats":
		cmdStats(cmdArgs)
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown command %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Forgejo CLI - Common API operations

Usage: fj [global flags] <command> [command args]

Global flags:
  -j, --json      Output as JSON
  -u, --url URL   Forgejo URL (default: $FORGEJO_URL or http://localhost:3000)
  -r, --repo REPO Repository (default: auto-detect from git)
  -h, --help      Show this help

Commands:
  version         Get Forgejo version
  user            Get current user
  repo            Repository operations (list, create)
  issue           Issue operations (list, get, create, close, open)
  pr              Pull request operations (list, get, create, merge, files)
  comment         Comment operations (list, add)
  branch          Branch operations (list, delete)
  hook            Webhook operations (list, create, delete)
  label           Label operations (list, add, remove)
  reaction        Reaction operations (add)
  file            File operations (list, get, create)
  collab          Collaborator operations (list, add)
  init            Initialize .fj config file
  detect          Show detected repository from git
  stats           Token and cost usage statistics

Examples:
  fj issue list owner/repo
  fj issue create owner/repo "Title" --body "Description"
  fj pr list owner/repo --state all
  fj pr files owner/repo 5
  fj file list owner/repo pkg/`)
}

func getClient() *forgejo.Client {
	// Load .fj config file FIRST, before applying defaults
	loadConfig()

	url := forgejoURL
	if url == "" {
		url = os.Getenv("FORGEJO_URL")
	}
	if url == "" {
		url = "http://localhost:3000"
	}

	token := os.Getenv("FORGEJO_TOKEN")
	user := os.Getenv("FORGEJO_USER")
	password := os.Getenv("FORGEJO_PASSWORD")

	if token != "" {
		return forgejo.NewClient(url, token)
	}
	if user != "" && password != "" {
		return forgejo.NewClientWithBasicAuth(url, user, password)
	}
	// No auth - will fail on protected endpoints
	return forgejo.NewClient(url, "")
}

func loadConfig() {
	cwd, _ := os.Getwd()
	for dir := cwd; dir != "/"; dir = filepath.Dir(dir) {
		cfgPath := filepath.Join(dir, ".fj")
		if data, err := os.ReadFile(cfgPath); err == nil {
			// Simple INI parsing
			lines := strings.Split(string(data), "\n")
			section := ""
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
					section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
					continue
				}
				if section == "forgejo" {
					parts := strings.SplitN(line, "=", 2)
					if len(parts) == 2 {
						key := strings.TrimSpace(parts[0])
						val := strings.TrimSpace(parts[1])
						switch key {
						case "url":
							if forgejoURL == "" {
								forgejoURL = val
							}
						case "token":
							os.Setenv("FORGEJO_TOKEN", val)
						case "user":
							os.Setenv("FORGEJO_USER", val)
						case "password":
							os.Setenv("FORGEJO_PASSWORD", val)
						case "repo":
							if repoOverride == "" {
								repoOverride = val
							}
						}
					}
				}
			}
			return
		}
	}
}

func getRepo(args []string) string {
	if repoOverride != "" {
		return repoOverride
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !strings.Contains(args[0], "=") {
		return args[0]
	}
	// Auto-detect from git
	return detectRepo()
}

func detectRepo() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	// Parse SSH: git@host:owner/repo.git
	if ssh := regexp.MustCompile(`git@[^:]+:(.+)`).FindSubmatch([]byte(url)); ssh != nil {
		return strings.TrimSuffix(string(ssh[1]), ".git")
	}
	// Parse HTTPS: https://host/owner/repo.git or https://host/git/owner/repo.git
	https := regexp.MustCompile(`https?://[^/]+/(?:git/)?(.+)`).FindSubmatch([]byte(url))
	if https != nil {
		return strings.TrimSuffix(string(https[1]), ".git")
	}
	return ""
}

func formatJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

func printTable() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

// === COMMANDS ===

func cmdVersion(ctx context.Context, client *forgejo.Client) {
	v, err := client.GetVersion(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Println(v.Version)
}

func cmdUser(ctx context.Context, client *forgejo.Client) {
	user, err := client.GetCurrentUser(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	if jsonOutput {
		fmt.Println(formatJSON(user))
		return
	}
	fmt.Printf("@%s (ID: %d)\n", user.Login, user.ID)
}

func cmdRepo(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj repo <list|create> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	switch subcmd {
	case "list":
		repos, err := client.ListUserRepos(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(repos))
			return
		}
		tw := printTable()
		for _, r := range repos {
			privacy := "public"
			if r.Private {
				privacy = "private"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.FullName, privacy, r.Description)
		}
		tw.Flush()
	case "create":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj repo create <name> [--private] [--description DESC]")
			os.Exit(1)
		}
		name := args[1]
		private := false
		desc := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--private" {
				private = true
			}
			if args[i] == "--description" && i+1 < len(args) {
				desc = args[i+1]
				i++
			}
		}
		repo, err := client.CreateRepository(ctx, name, desc, private)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Created: %s\n", repo.HTMLURL)
	default:
		fmt.Fprintf(os.Stderr, "Unknown repo subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdIssue(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj issue <list|get|create|close|open|comment> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		repo := getRepo(args)
		state := "open"
		for i := 0; i < len(args); i++ {
			if args[i] == "--state" && i+1 < len(args) {
				state = args[i+1]
			}
		}
		issues, err := client.ListIssues(ctx, repo, state, 50)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(issues))
			return
		}
		for _, issue := range issues {
			fmt.Printf("#%d [%s] %s\n", issue.Number, issue.State, issue.Title)
		}
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj issue get <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		issue, err := client.GetIssue(ctx, repo, num)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(issue))
			return
		}
		fmt.Printf("#%d %s\n", issue.Number, issue.Title)
		fmt.Printf("State: %s\n", issue.State)
		fmt.Printf("Author: @%s\n", issue.User.Login)
		fmt.Printf("\n%s\n", issue.Body)
	case "create":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj issue create <repo> <title> [--body BODY]")
			os.Exit(1)
		}
		repo := args[0]
		title := args[1]
		body := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--body" && i+1 < len(args) {
				body = args[i+1]
			}
		}
		issue, err := client.CreateIssue(ctx, repo, title, body)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Created issue #%d\n", issue.Number)
	case "close":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj issue close <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		if err := client.CloseIssue(ctx, repo, num); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Closed issue #%d\n", num)
	case "open":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj issue open <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		if err := client.ReopenIssue(ctx, repo, num); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Reopened issue #%d\n", num)
	case "comment":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj issue comment <repo> <number> <body>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		body := args[2]
		if err := client.PostIssueComment(ctx, repo, num, body); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Commented on #%d\n", num)
	default:
		fmt.Fprintf(os.Stderr, "Unknown issue subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdPR(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj pr <list|get|create|merge|close|files> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		repo := getRepo(args)
		state := "open"
		for i := 0; i < len(args); i++ {
			if args[i] == "--state" && i+1 < len(args) {
				state = args[i+1]
			}
		}
		prs, err := client.ListPRs(ctx, repo, state)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(prs))
			return
		}
		for _, pr := range prs {
			fmt.Printf("#%d [%s] %s (%s)\n", pr.Number, pr.State, pr.Title, pr.Head.Ref)
		}
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj pr get <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		pr, err := client.GetPR(ctx, repo, num)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(pr))
			return
		}
		fmt.Printf("#%d %s\n", pr.Number, pr.Title)
		fmt.Printf("State: %s\n", pr.State)
		fmt.Printf("Head: %s\n", pr.Head.Ref)
		fmt.Printf("Base: %s\n", pr.Base.Ref)
		fmt.Printf("Mergeable: %v\n", pr.Mergeable)
		fmt.Printf("Conflicts: %v\n", pr.HasConflicts)
	case "merge":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj pr merge <repo> <number> [--style merge|squash|rebase-merge]")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		style := "merge"
		for i := 2; i < len(args); i++ {
			if args[i] == "--style" && i+1 < len(args) {
				style = args[i+1]
			}
		}
		if err := client.MergePR(ctx, repo, num, style); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Merged PR #%d\n", num)
	case "create":
		if len(args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: fj pr create <repo> <title> --head HEAD --base BASE [--body BODY]")
			os.Exit(1)
		}
		repo := args[0]
		title := args[1]
		head, base, body := "", "main", ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--head" && i+1 < len(args) {
				head = args[i+1]
			}
			if args[i] == "--base" && i+1 < len(args) {
				base = args[i+1]
			}
			if args[i] == "--body" && i+1 < len(args) {
				body = args[i+1]
			}
		}
		if head == "" {
			fmt.Fprintln(os.Stderr, "Error: --head is required")
			os.Exit(1)
		}
		pr, err := client.CreatePR(ctx, repo, title, body, head, base)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Created PR #%d\n", pr.Number)
	case "close":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj pr close <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		if err := client.ClosePR(ctx, repo, num); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Closed PR #%d\n", num)
	case "files":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj pr files <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		files, err := client.GetPRFiles(ctx, repo, num)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(files))
			return
		}
		for _, f := range files {
			status := f.Status[0:1]
			if status == "a" {
				status = "A"
			} else if status == "m" {
				status = "M"
			} else if status == "r" {
				status = "D"
			}
			fmt.Printf("%s %s (+%d/-%d)\n", status, f.Filename, f.Additions, f.Deletions)
		}
	default:
		fmt.Fprintf(os.Stderr, "Unknown pr subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdComment(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj comment <list|add> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj comment list <repo> <number>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		comments, err := client.ListComments(ctx, repo, num)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(comments))
			return
		}
		for _, c := range comments {
			body := c.Body
			if len(body) > 60 {
				body = body[:60] + "..."
			}
			fmt.Printf("@%s: %s\n", c.User, body)
		}
	case "add":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj comment add <repo> <number> <body>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		body := args[2]
		if err := client.PostIssueComment(ctx, repo, num, body); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Commented on #%d\n", num)
	default:
		fmt.Fprintf(os.Stderr, "Unknown comment subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdBranch(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj branch <list|delete> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		repo := getRepo(args)
		branches, err := client.ListBranches(ctx, repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(branches))
			return
		}
		for _, b := range branches {
			protected := ""
			if b.Protected {
				protected = " [protected]"
			}
			sha := b.CommitID
			if len(sha) > 8 {
				sha = sha[:8]
			}
			fmt.Printf("%s (%s)%s\n", b.Name, sha, protected)
		}
	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj branch delete <repo> <branch>")
			os.Exit(1)
		}
		repo := args[0]
		branch := args[1]
		if err := client.DeleteBranch(ctx, repo, branch); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted branch %s\n", branch)
	default:
		fmt.Fprintf(os.Stderr, "Unknown branch subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdHook(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj hook <list|create|delete> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		repo := getRepo(args)
		hooks, err := client.ListWebhooks(ctx, repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(hooks))
			return
		}
		for _, h := range hooks {
			active := "active"
			if !h.Active {
				active = "inactive"
			}
			url := h.Config["url"]
			fmt.Printf("#%d [%s] %s: %s\n", h.ID, active, h.Type, url)
		}
	case "create":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj hook create <repo> <url> [--secret SECRET] [--events push,pull_request]")
			os.Exit(1)
		}
		repo := args[0]
		url := args[1]
		secret := ""
		events := []string{"push"}
		for i := 2; i < len(args); i++ {
			if args[i] == "--secret" && i+1 < len(args) {
				secret = args[i+1]
			}
			if args[i] == "--events" && i+1 < len(args) {
				events = strings.Split(args[i+1], ",")
			}
		}
		hook, err := client.CreateWebhook(ctx, repo, "forgejo", url, secret, events)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Created webhook #%d: %s\n", hook.ID, url)
	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj hook delete <repo> <id>")
			os.Exit(1)
		}
		repo := args[0]
		var id int
		fmt.Sscanf(args[1], "%d", &id)
		if err := client.DeleteWebhook(ctx, repo, id); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Deleted webhook #%d\n", id)
	default:
		fmt.Fprintf(os.Stderr, "Unknown hook subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdLabel(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj label <list|add|remove> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		repo := getRepo(args)
		labels, err := client.ListLabels(ctx, repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		for _, l := range labels {
			fmt.Println(l.Name)
		}
	case "add":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj label add <repo> <number> <labels>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		labels := strings.Split(args[2], ",")
		if err := client.AddIssueLabels(ctx, repo, num, labels); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Added labels %s to #%d\n", args[2], num)
	case "remove":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj label remove <repo> <number> <label>")
			os.Exit(1)
		}
		repo := args[0]
		var num int
		fmt.Sscanf(args[1], "%d", &num)
		label := args[2]
		if err := client.RemoveIssueLabel(ctx, repo, num, label); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Removed label '%s' from #%d\n", label, num)
	default:
		fmt.Fprintf(os.Stderr, "Unknown label subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdReaction(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: fj reaction add <repo> <number> <emoji> [--comment COMMENT_ID]")
		os.Exit(1)
	}
	repo := args[0]
	var num int
	fmt.Sscanf(args[1], "%d", &num)
	emoji := args[2]
	commentID := 0
	for i := 3; i < len(args); i++ {
		if args[i] == "--comment" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &commentID)
		}
	}
	if err := client.AddReaction(ctx, repo, num, commentID, emoji); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Printf("Added reaction %s\n", emoji)
}

func cmdFile(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj file <list|get|create> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		ref := "main"
		// Find --ref flag
		for i := 0; i < len(args); i++ {
			if args[i] == "--ref" && i+1 < len(args) {
				ref = args[i+1]
			}
		}
		// Filter out flags to get positionals
		var pos []string
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-") {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++ // skip flag value
				}
				continue
			}
			pos = append(pos, args[i])
		}
		repo := ""
		path := ""
		if repoOverride != "" {
			repo = repoOverride
			if len(pos) > 0 {
				path = pos[0]
			}
		} else if len(pos) > 0 {
			// First positional: could be repo or path
			// Check if it looks like owner/repo
			if strings.Contains(pos[0], "/") {
				repo = pos[0]
				if len(pos) > 1 {
					path = pos[1]
				}
			} else {
				// Assume it's a path, try auto-detect repo
				repo = detectRepo()
				path = pos[0]
			}
		} else {
			repo = detectRepo()
		}
		if repo == "" {
			fmt.Fprintln(os.Stderr, "Error: no repository specified")
			os.Exit(1)
		}
		files, err := client.ListDir(ctx, repo, ref, path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(files))
			return
		}
		for _, f := range files {
			if f.Type == "dir" {
				fmt.Printf("[DIR]  %s\n", f.Name)
			} else {
				fmt.Printf("       %s\n", f.Name)
			}
		}
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj file get <repo> <path> [--ref REF]")
			os.Exit(1)
		}
		repo := args[0]
		path := args[1]
		ref := "main"
		for i := 2; i < len(args); i++ {
			if args[i] == "--ref" && i+1 < len(args) {
				ref = args[i+1]
			}
		}
		file, err := client.GetFile(ctx, repo, ref, path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(file))
			return
		}
		if file.Encoding == "base64" {
			data, err := base64.StdEncoding.DecodeString(file.Content)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error decoding:", err)
				os.Exit(1)
			}
			fmt.Print(string(data))
		} else {
			fmt.Print(file.Content)
		}
	case "create":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: fj file create <repo> <path> --message MSG --content CONTENT")
			os.Exit(1)
		}
		repo := args[0]
		path := args[1]
		message := ""
		content := ""
		for i := 2; i < len(args); i++ {
			if args[i] == "--message" && i+1 < len(args) {
				message = args[i+1]
			}
			if args[i] == "--content" && i+1 < len(args) {
				content = args[i+1]
			}
		}
		if message == "" || content == "" {
			fmt.Fprintln(os.Stderr, "Error: --message and --content required")
			os.Exit(1)
		}
		if err := client.CreateOrUpdateFile(ctx, repo, path, message, content, ""); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Created/updated %s\n", path)
	default:
		fmt.Fprintf(os.Stderr, "Unknown file subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdCollab(ctx context.Context, client *forgejo.Client, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: fj collab <list|add> ...")
		os.Exit(1)
	}
	subcmd := args[0]
	args = args[1:]

	switch subcmd {
	case "list":
		repo := getRepo(args)
		collabs, err := client.ListCollaborators(ctx, repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		if jsonOutput {
			fmt.Println(formatJSON(collabs))
			return
		}
		for _, c := range collabs {
			fmt.Printf("@%s (%s)\n", c.Login, c.Permission)
		}
	case "add":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: fj collab add <repo> <user> [--permission read|write|admin]")
			os.Exit(1)
		}
		repo := args[0]
		user := args[1]
		permission := "write"
		for i := 2; i < len(args); i++ {
			if args[i] == "--permission" && i+1 < len(args) {
				permission = args[i+1]
			}
		}
		if err := client.AddCollaborator(ctx, repo, user, permission); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		fmt.Printf("Added @%s as collaborator\n", user)
	default:
		fmt.Fprintf(os.Stderr, "Unknown collab subcommand: %s\n", subcmd)
		os.Exit(1)
	}
}

func cmdInit(args []string) {
	url := "http://localhost:3000"
	token := ""
	user := ""
	password := ""
	repo := ""
	force := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--url", "-u":
			if i+1 < len(args) {
				url = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
		case "--user":
			if i+1 < len(args) {
				user = args[i+1]
				i++
			}
		case "--password":
			if i+1 < len(args) {
				password = args[i+1]
				i++
			}
		case "--repo":
			if i+1 < len(args) {
				repo = args[i+1]
				i++
			}
		case "--force", "-f":
			force = true
		}
	}

	cfgPath := ".fj"
	if _, err := os.Stat(cfgPath); err == nil && !force {
		fmt.Fprintln(os.Stderr, "Error: .fj already exists. Use --force to overwrite.")
		os.Exit(1)
	}

	var sb strings.Builder
	sb.WriteString("[forgejo]\n")
	sb.WriteString(fmt.Sprintf("url = %s\n", url))
	if token != "" {
		sb.WriteString(fmt.Sprintf("token = %s\n", token))
	}
	if user != "" {
		sb.WriteString(fmt.Sprintf("user = %s\n", user))
	}
	if password != "" {
		sb.WriteString(fmt.Sprintf("password = %s\n", password))
	}
	if repo != "" {
		sb.WriteString(fmt.Sprintf("repo = %s\n", repo))
	}

	if err := os.WriteFile(cfgPath, []byte(sb.String()), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
	fmt.Printf("Created %s\n", cfgPath)
	fmt.Print(sb.String())
}

func cmdDetect() {
	repo := detectRepo()
	if repo == "" {
		fmt.Println("No repository detected from git remote.")
		return
	}
	fmt.Printf("Detected: %s\n", repo)
}

func formatNum(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func cmdStats(args []string) {
	fordjentURL := os.Getenv("FORDJENT_URL")
	if fordjentURL == "" {
		fordjentURL = "http://localhost:8080"
	}

	since := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--since" && i+1 < len(args) {
			since = args[i+1]
			i++
		}
		if args[i] == "--url" && i+1 < len(args) {
			fordjentURL = args[i+1]
			i++
		}
	}

	reqURL := fordjentURL + "/status"
	if since != "" {
		reqURL += "?since=" + url.QueryEscape(since)
	}

	resp, err := http.Get(reqURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error fetching stats:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error reading response:", err)
		os.Exit(1)
	}

	if jsonOutput {
		var v interface{}
		json.Unmarshal(body, &v)
		fmt.Println(formatJSON(v))
		return
	}

	var data struct {
		Costs struct {
			TotalSessions int     `json:"total_sessions"`
			TotalTokens   int64   `json:"total_tokens"`
			TotalCostUSD  float64 `json:"total_cost_usd"`
		} `json:"costs"`
		ByModel []struct {
			Provider     string  `json:"provider"`
			Model        string  `json:"model"`
			Calls        int64   `json:"calls"`
			InputTokens  int64   `json:"input_tokens"`
			OutputTokens int64   `json:"output_tokens"`
			TotalTokens  int64   `json:"total_tokens"`
			CostUSD      float64 `json:"cost_usd"`
		} `json:"by_model"`
		BySessionModel []struct {
			SessionKey   string  `json:"session_key"`
			Provider     string  `json:"provider"`
			Model        string  `json:"model"`
			Calls        int64   `json:"calls"`
			InputTokens  int64   `json:"input_tokens"`
			OutputTokens int64   `json:"output_tokens"`
			TotalTokens  int64   `json:"total_tokens"`
			CostUSD      float64 `json:"cost_usd"`
		} `json:"by_session_model"`
		Metrics struct {
			EventsTotal     int64 `json:"events_total"`
			SessionsTotal   int64 `json:"sessions_total"`
			SessionsActive  int64 `json:"sessions_active"`
			LLMCallsTotal   int64 `json:"llm_calls_total"`
			LLMRetriesTotal int64 `json:"llm_retries_total"`
			InputTokens     int64 `json:"input_tokens"`
			OutputTokens    int64 `json:"output_tokens"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		fmt.Fprintln(os.Stderr, "Error parsing response:", err)
		os.Exit(1)
	}

	fmt.Printf("Sessions: %d (active: %d)  |  LLM calls: %d (retries: %d)\n",
		data.Metrics.SessionsTotal, data.Metrics.SessionsActive,
		data.Metrics.LLMCallsTotal, data.Metrics.LLMRetriesTotal)
	fmt.Printf("Total tokens: %s in / %s out  |  Cost: $%.6f\n\n",
		formatNum(data.Metrics.InputTokens), formatNum(data.Metrics.OutputTokens), data.Costs.TotalCostUSD)

	fmt.Println("Per Model:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "PROVIDER\tMODEL\tCALLS\tIN\tOUT\tTOTAL\tCOST\n")
	for _, m := range data.ByModel {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t$%.6f\n",
			m.Provider, m.Model, m.Calls,
			formatNum(m.InputTokens), formatNum(m.OutputTokens), formatNum(m.TotalTokens), m.CostUSD)
	}
	tw.Flush()

	if len(data.BySessionModel) > 0 {
		fmt.Println("\nPer Session:")
		tw2 := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw2, "SESSION\tMODEL\tCALLS\tIN\tOUT\tTOTAL\tCOST\n")
		for _, s := range data.BySessionModel {
			fmt.Fprintf(tw2, "%s\t%s\t%d\t%s\t%s\t%s\t$%.6f\n",
				s.SessionKey, s.Model, s.Calls,
				formatNum(s.InputTokens), formatNum(s.OutputTokens), formatNum(s.TotalTokens), s.CostUSD)
		}
		tw2.Flush()
	}
}
