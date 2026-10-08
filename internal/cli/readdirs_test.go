package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkReadDir refuses every directory that would undo what the worktree is
// for. Each case names the reason a person reads in the error.
func TestCheckReadDir(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	workspace := filepath.Join(base, "workspace")
	confDir := filepath.Join(base, "config")
	allowed := filepath.Join(base, "opt", "vendor")
	for _, d := range []string{repo, filepath.Join(repo, "sub"), workspace, filepath.Join(workspace, "TASK-1-a1"), confDir, allowed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(confDir, "conf.json")
	if err := os.WriteFile(conf, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	refused := map[string]string{
		"/":                                   "root",
		repo:                                  "repository",
		filepath.Join(repo, "sub"):            "repository",
		base:                                  "repository",
		workspace:                             "workspace",
		filepath.Join(workspace, "TASK-1-a1"): "workspace",
		confDir:                               "configuration",
		conf:                                  "not a directory",
		filepath.Join(base, "missing"):        "not a directory",
	}
	for dir, why := range refused {
		if _, err := checkReadDir(dir, repo, workspace, conf); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("checkReadDir(%s) = %v, want a refusal mentioning %q", dir, err, why)
		}
	}

	got, err := checkReadDir(allowed+"/", repo, workspace, conf)
	if err != nil || got != allowed {
		t.Errorf("checkReadDir(%s/) = %q, %v; want %s accepted and cleaned", allowed, got, err, allowed)
	}
	// A symbolic link is resolved, so the stored path is the one OpenCode
	// will compare against.
	link := filepath.Join(base, "link")
	if err := os.Symlink(allowed, link); err != nil {
		t.Fatal(err)
	}
	if got, err := checkReadDir(link, repo, workspace, conf); err != nil || got != allowed {
		t.Errorf("checkReadDir(link) = %q, %v; want the resolved %s", got, err, allowed)
	}
}
