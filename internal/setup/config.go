// Package setup prepares a machine for aidev: the configuration file and the
// PostgreSQL container it points at.
package setup

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

// WriteConfig creates the configuration file described by opts. Specified by
// config_test.go; not implemented yet.
func WriteConfig(opts ConfigOptions) (created bool, err error) { return false, nil }
