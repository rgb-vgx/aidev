package release

import (
	"os"
	"strings"
	"testing"
)

// Nothing runs a GitHub workflow locally, so these checks hold its text to the
// release process: a v* tag runs the gate, builds with the script tested above,
// and publishes the archives and SHA256SUMS that install.sh downloads.
func TestReleaseWorkflowPublishesWhatInstallShDownloads(t *testing.T) {
	body, err := os.ReadFile(repoRoot + "/.github/workflows/release.yml")
	if err != nil {
		t.Fatalf(".github/workflows/release.yml is missing: %v", err)
	}
	text := string(body)
	for _, want := range []struct{ text, why string }{
		{"tags:", "it must run on tags"},
		{"'v*'", "only version tags publish a release"},
		{"contents: write", "creating a release needs write permission on contents"},
		{"actions/checkout@", "it must check out the tagged commit"},
		{"actions/setup-go@", "it must install Go"},
		{"go-version-file: go.mod", "the Go version must come from go.mod, not be repeated"},
		{"make check", "a release must pass the same gate as every merge"},
		{"scripts/build-release.sh", "the archives must come from the tested build script"},
		{"VERSION: ${{ github.ref_name }}", "the binary must report the tag it was built from"},
		{"gh release create", "the release is published with the GitHub CLI"},
		{"SHA256SUMS", "install.sh refuses to install without the checksums"},
		{"GH_TOKEN: ${{ github.token }}", "gh needs the workflow token"},
	} {
		if !strings.Contains(text, want.text) {
			t.Errorf("release.yml lacks %q: %s", want.text, want.why)
		}
	}
	if strings.Contains(text, "pull_request") || strings.Contains(text, "branches:") {
		t.Error("release.yml must run on version tags only, not on branches or pull requests")
	}
}
