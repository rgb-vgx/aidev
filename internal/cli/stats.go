package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"aidev/internal/view"
)

// runStats reports finished attempts grouped by the model that ran them and
// the hardness of their task, so the routing table can be checked against
// what happened rather than guessed at.
func runStats(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, `usage: aidev stats [--json]

Reports finished attempts by the model that ran them and the hardness of their
task: how many runs each model did on each kind of work, and how they ended.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usagef("aidev stats: %v", err)
	}
	if fs.NArg() > 0 {
		return usagef("aidev stats takes no arguments")
	}

	app, err := openApp(ctx)
	if err != nil {
		return err
	}
	defer app.close()

	rows, err := app.store.StatsByModelAndHardness(ctx)
	if err != nil {
		return err
	}

	views := make([]view.StatsRow, 0, len(rows))
	for _, r := range rows {
		kinds := map[string]int{}
		for k, n := range r.FailureKinds {
			kinds[k] = n
		}
		views = append(views, view.StatsRow{
			Model:        r.Model,
			Hardness:     r.Hardness,
			Runs:         r.Runs,
			Succeeded:    r.Succeeded,
			Failed:       r.Failed,
			Cancelled:    r.Cancelled,
			FailureKinds: kinds,
		})
	}

	if *asJSON {
		return writeJSON(env.Stdout, view.StatsReport{Rows: views})
	}

	if len(views) == 0 {
		fmt.Fprintln(env.Stdout, "no finished attempts recorded yet")
		return nil
	}

	fmt.Fprintf(env.Stdout, "%-32s  %-8s  %4s  %9s  %6s  %9s  %s\n",
		"model", "hardness", "runs", "succeeded", "failed", "cancelled", "failure kinds")
	for _, r := range views {
		fmt.Fprintf(env.Stdout, "%-32s  %-8s  %4d  %9d  %6d  %9d  %s\n",
			r.Model, r.Hardness, r.Runs, r.Succeeded, r.Failed, r.Cancelled, formatFailureKinds(r.FailureKinds))
	}
	return nil
}

// formatFailureKinds renders the breakdown sorted by kind, so the output is
// stable. An empty breakdown is a dash rather than a blank that looks like
// missing output.
func formatFailureKinds(kinds map[string]int) string {
	if len(kinds) == 0 {
		return "-"
	}
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, fmt.Sprintf("%s=%d", name, kinds[name]))
	}
	return strings.Join(pairs, ", ")
}
