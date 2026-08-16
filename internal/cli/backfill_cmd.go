package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/andrhamm/claude-mem-lan-sync/internal/clientdb"
	"github.com/andrhamm/claude-mem-lan-sync/internal/paths"
	"github.com/andrhamm/claude-mem-lan-sync/internal/settings"
)

var osHostname = os.Hostname

func runBackfill(args []string, env Env) int {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dryRun := fs.Bool("dry-run", false, "report what would be queued without changing anything")
	force := fs.Bool("force", false, "run even while the claude-mem worker is writing")
	project := fs.String("project", "", "only queue memories from this project")
	undo := fs.Bool("undo", false, "reverse the last backfill")

	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	dbPath, err := paths.ClaudeMemDB()
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	db, err := clientdb.Open(dbPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	if *undo {
		n, err := db.UndoBackfill(env.Now)
		if err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "restored %d baseline entries; queued memories were un-queued\n", n)
		return 0
	}

	opts := clientdb.BackfillOpts{
		DryRun:           *dryRun,
		Force:            *force,
		Project:          *project,
		ExcludedProjects: excludedProjects(),
		Now:              env.Now,
	}

	res, err := db.Backfill(opts)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	total := 0
	for _, n := range res.PerTable {
		total += n
	}

	if *dryRun {
		fmt.Fprint(env.Stdout, "dry run — nothing was changed\n\n")
	}
	fmt.Fprintf(env.Stdout, "  observations  %d\n", res.PerTable["observations"])
	fmt.Fprintf(env.Stdout, "  summaries     %d\n", res.PerTable["session_summaries"])
	fmt.Fprintf(env.Stdout, "  prompts       %d\n", res.PerTable["user_prompts"])
	fmt.Fprintf(env.Stdout, "  total         %d rows, roughly %s of content\n", total, humanBytes(res.Bytes))

	if len(opts.ExcludedProjects) > 0 {
		fmt.Fprintf(env.Stdout, "\n  excluded projects: %s\n", strings.Join(opts.ExcludedProjects, ", "))
	}

	if *dryRun {
		fmt.Fprintln(env.Stdout, "\nRun without --dry-run to queue these for upload.")
		return 0
	}

	fmt.Fprintf(env.Stdout, "\nbackup       %s\n", res.BackupPath)
	fmt.Fprintln(env.Stdout, "undo with    cmemlan backfill --undo")
	fmt.Fprintln(env.Stdout, "\nThe worker uploads these in the background. Watch with: cmemlan status")
	return 0
}

// excludedProjects reads claude-mem's own exclusion list.
//
// A user who excluded a project from capture would be startled to find its
// entire history uploaded by a backfill.
func excludedProjects() []string {
	path, err := paths.ClaudeMemSettings()
	if err != nil {
		return nil
	}
	current, err := settings.Read(path)
	if err != nil {
		return nil
	}
	raw := settings.Value(current, "CLAUDE_MEM_EXCLUDED_PROJECTS")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	// The setting is a comma-separated list, but tolerate a JSON array too.
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			return arr
		}
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func humanBytes(n int64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
