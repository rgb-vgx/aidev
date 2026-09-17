// Package setup prepares a machine for aidev: the configuration file and the
// PostgreSQL container it points at.
package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"aidev/internal/config"
)

// ConfigOptions describes the conf.json that WriteConfig creates.
type ConfigOptions struct {
	// Path is the absolute path of the file to create.
	Path string
	// DatabaseURL is written as database.url. Required.
	DatabaseURL string
	// WorkspaceRoot is written as workspace_root when not empty; otherwise the
	// key is left out and aidev uses its default.
	WorkspaceRoot string
}

// configDocument is the JSON shape WriteConfig writes: only what it was
// given, leaving every other setting to aidev's defaults.
type configDocument struct {
	Database      map[string]string `json:"database"`
	WorkspaceRoot string            `json:"workspace_root,omitempty"`
}

// WriteConfig creates the configuration file described by opts.
//
// It never overwrites: when a file already exists at opts.Path it reports
// created=false and leaves the file untouched, so running setup twice cannot
// destroy a configuration someone has edited. The file is written privately
// (mode 0600, parent directories 0700) because it holds the database
// password, and it is validated with config.LoadFile before it appears at
// its final path. Validation errors name the field they reject and never
// include the database URL itself.
func WriteConfig(opts ConfigOptions) (created bool, err error) {
	// Validate before touching the disk so bad input leaves nothing behind.
	if opts.Path == "" {
		return false, fmt.Errorf("setup: Path must not be empty")
	}
	if !filepath.IsAbs(opts.Path) {
		return false, fmt.Errorf("setup: Path must be absolute: %q", opts.Path)
	}
	if opts.DatabaseURL == "" {
		return false, fmt.Errorf("setup: DatabaseURL must not be empty")
	}
	if opts.WorkspaceRoot != "" && !filepath.IsAbs(opts.WorkspaceRoot) {
		return false, fmt.Errorf("setup: WorkspaceRoot must be empty or absolute: %q", opts.WorkspaceRoot)
	}

	// A file that is already there belongs to its owner, not to setup.
	if _, err := os.Stat(opts.Path); err == nil {
		return false, nil
	}

	doc := configDocument{Database: map[string]string{"url": opts.DatabaseURL}}
	if opts.WorkspaceRoot != "" {
		doc.WorkspaceRoot = opts.WorkspaceRoot
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, fmt.Errorf("setup: encode config for Path %s: %w", opts.Path, err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return false, fmt.Errorf("setup: create parent directory for Path %s: %w", opts.Path, err)
	}

	// Write beside the destination so the final link stays on one filesystem
	// and validation sees exactly the bytes that will be installed.
	tmp, err := os.CreateTemp(filepath.Dir(opts.Path), ".conf-*.tmp")
	if err != nil {
		return false, fmt.Errorf("setup: create temporary file for Path %s: %w", opts.Path, err)
	}
	tmpPath := tmp.Name()
	// Every failure below must leave no temporary file behind.
	failed := true
	defer func() {
		if failed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("setup: chmod temporary file for Path %s: %w", opts.Path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("setup: write temporary file for Path %s: %w", opts.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("setup: write temporary file for Path %s: %w", opts.Path, err)
	}

	// PostgreSQL's own parser, via aidev's loader, is the authority on
	// whether the URL connects; its error is already redacted and safe.
	if _, err := config.LoadFile(tmpPath); err != nil {
		return false, err
	}

	// Link instead of renaming so a file that appeared meanwhile is kept:
	// Link fails when the destination exists, rename would replace it.
	if err := os.Link(tmpPath, opts.Path); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("setup: create config file at Path %s: %w", opts.Path, err)
	}
	failed = false
	if err := os.Remove(tmpPath); err != nil {
		return true, fmt.Errorf("setup: remove temporary file for Path %s: %w", opts.Path, err)
	}
	return true, nil
}
