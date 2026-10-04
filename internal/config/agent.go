package config

// Backend names the agent implementation that runs tasks.
type Backend string

const (
	// BackendOpenCode runs tasks through the OpenCode CLI. This is the default.
	BackendOpenCode Backend = "opencode"
	// BackendCodex runs tasks through the Codex CLI.
	BackendCodex Backend = "codex"
)

// DefaultCodexCommand is the executable name, resolved through PATH.
const DefaultCodexCommand = "codex"

// Valid reports whether b is a known backend.
func (b Backend) Valid() bool {
	return b == BackendOpenCode || b == BackendCodex
}

func (b Backend) String() string { return string(b) }

// AllBackends lists every backend, for error messages and documentation.
func AllBackends() []Backend {
	return []Backend{BackendOpenCode, BackendCodex}
}
