package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/config"
)

const testURL = "postgres://aidev:s3cret@127.0.0.1:5434/aidev?sslmode=disable"

// The file setup writes must be one aidev accepts, as small as possible (a new user
// reads it), and private: it holds the database password.
func TestWriteConfigCreatesAMinimalPrivateFileAidevAccepts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "aidev")
	path := filepath.Join(dir, "conf.json")
	workspace := filepath.Join(t.TempDir(), "worktrees")

	created, err := WriteConfig(ConfigOptions{Path: path, DatabaseURL: testURL, WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if !created {
		t.Error("created = false for a file that did not exist")
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("aidev rejects the file setup wrote: %v", err)
	}
	if cfg.DatabaseURL != testURL || cfg.WorkspaceRoot != workspace {
		t.Errorf("loaded database.url = %q, workspace_root = %q; want %q, %q", cfg.DatabaseURL, cfg.WorkspaceRoot, testURL, workspace)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for key := range doc {
		if key != "database" && key != "workspace_root" {
			t.Errorf("the file sets %q; setup writes only what it was given and leaves the rest to aidev's defaults", key)
		}
	}
	if !strings.HasSuffix(string(body), "\n") || !strings.Contains(string(body), "\n  ") {
		t.Errorf("the file should be indented JSON ending in a newline, for a person to read:\n%s", body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600: it holds the database password", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("created directory mode = %o, want 700", perm)
	}
}

func TestWriteConfigWithoutWorkspaceLeavesTheKeyOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.json")
	if _, err := WriteConfig(ConfigOptions{Path: path, DatabaseURL: testURL}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "workspace_root") {
		t.Errorf("workspace_root was written although none was given:\n%s", body)
	}
	if _, err := config.LoadFile(path); err != nil {
		t.Errorf("aidev rejects the file: %v", err)
	}
}

// Running setup twice must never destroy a configuration someone has edited.
func TestWriteConfigNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.json")
	const edited = `{"database": {"url": "postgres://me:mine@db.example:5432/aidev"}, "log_level": "debug"}`
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := WriteConfig(ConfigOptions{Path: path, DatabaseURL: testURL})
	if err != nil {
		t.Fatalf("WriteConfig on an existing file: %v", err)
	}
	if created {
		t.Error("created = true for a file that already existed")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != edited {
		t.Errorf("the existing file was changed:\n%s", body)
	}
}

func TestWriteConfigRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]ConfigOptions{
		"relative path":        {Path: "conf.json", DatabaseURL: testURL},
		"empty path":           {Path: "", DatabaseURL: testURL},
		"missing database url": {Path: filepath.Join(dir, "a.json")},
		"unparseable url":      {Path: filepath.Join(dir, "b.json"), DatabaseURL: "postgres://%zz"},
		"relative workspace":   {Path: filepath.Join(dir, "c.json"), DatabaseURL: testURL, WorkspaceRoot: "worktrees"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			created, err := WriteConfig(opts)
			if err == nil || created {
				t.Fatalf("WriteConfig(%+v) = %v, %v; want an error and nothing created", opts, created, err)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("the error shows the password: %v", err)
			}
			if opts.Path != "" && filepath.IsAbs(opts.Path) {
				if _, statErr := os.Stat(opts.Path); statErr == nil {
					t.Errorf("a file was left at %s", opts.Path)
				}
			}
		})
	}
}
