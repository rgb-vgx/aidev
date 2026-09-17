package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"aidev/internal/config"
	"aidev/internal/doctor"
	"aidev/internal/store"
	"aidev/migrations"
)

// runDoctor checks that aidev can work here and says how to fix what cannot.
func runDoctor(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print as JSON")
	if err := fs.Parse(args); err != nil {
		return usagef("aidev doctor: %v", err)
	}
	if fs.NArg() > 0 {
		return usagef("aidev doctor takes no arguments")
	}

	deps := doctor.Deps{
		LoadConfig: func() (config.Config, error) { return config.Load(config.OSLookup) },
		LookPath:   exec.LookPath,
		PingDatabase: func(pingCtx context.Context, databaseURL string) error {
			connectCtx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
			defer cancel()
			db, err := store.Open(connectCtx, databaseURL)
			if err != nil {
				return err
			}
			defer db.Close()
			return nil
		},
		PendingMigrations: func(pendingCtx context.Context, databaseURL string) ([]string, error) {
			connectCtx, cancel := context.WithTimeout(pendingCtx, 10*time.Second)
			defer cancel()
			db, err := store.Open(connectCtx, databaseURL)
			if err != nil {
				return nil, err
			}
			defer db.Close()
			loaded, err := store.LoadMigrations(migrations.FS)
			if err != nil {
				return nil, err
			}
			return db.PendingMigrations(pendingCtx, loaded)
		},
		CheckWorkspace: func(dir string) error {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			f, err := os.CreateTemp(dir, ".aidev-doctor-*")
			if err != nil {
				return err
			}
			name := f.Name()
			if err := f.Close(); err != nil {
				_ = os.Remove(name)
				return err
			}
			return os.Remove(name)
		},
	}

	results := doctor.Run(ctx, deps)
	failed := 0
	for _, r := range results {
		if r.Status == doctor.StatusFail {
			failed++
		}
	}

	if *asJSON {
		if err := writeJSON(env.Stdout, results); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			fmt.Fprintf(env.Stdout, "%s  %-11s %s\n", doctorMark(r.Status), r.Name, r.Summary)
			if r.Fix != "" {
				fmt.Fprintf(env.Stdout, "      fix: %s\n", r.Fix)
			}
		}
		if failed > 0 {
			fmt.Fprintf(env.Stdout, "%d check(s) failed.\n", failed)
		} else {
			fmt.Fprintln(env.Stdout, "aidev is ready.")
		}
	}

	if failed > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	return nil
}

// doctorMark is the four-character status column of the text output.
func doctorMark(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "ok  "
	case doctor.StatusWarn:
		return "WARN"
	case doctor.StatusFail:
		return "FAIL"
	default:
		return "skip"
	}
}
