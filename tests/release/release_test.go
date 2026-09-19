// Package release checks what a new user downloads: the release archives, the
// script that installs them, and the workflow that publishes them.
package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const repoRoot = "../.."

// The four platforms aidev runs on. It relies on Unix process groups
// (internal/procexec), so there is no Windows build.
var platforms = []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}

func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// untar returns the regular files in a .tar.gz with their modes and contents.
func untar(t *testing.T, path string) map[string]struct {
	mode int64
	body []byte
} {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not gzip: %v", filepath.Base(path), err)
	}
	out := map[string]struct {
		mode int64
		body []byte
	}{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("%s is not a tar archive: %v", filepath.Base(path), err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = struct {
			mode int64
			body []byte
		}{h.Mode, body}
	}
}

// The build script is what CI runs on a tag: one archive per platform, named
// without the version so that GitHub's releases/latest/download/<name> URL
// always finds the newest, and a SHA256SUMS file the installer checks.
func TestBuildReleaseMakesAnArchivePerPlatformWithChecksums(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles four binaries")
	}
	dist := t.TempDir()
	cmd := exec.Command("sh", "scripts/build-release.sh")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "VERSION=v0.0.0-test", "DIST="+dist)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scripts/build-release.sh: %v\n%s", err, out)
	}

	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"SHA256SUMS"}
	for _, p := range platforms {
		want = append(want, "aidev_"+p+".tar.gz")
	}
	if strings.Join(names, " ") != strings.Join(slices.Sorted(slices.Values(want)), " ") {
		t.Fatalf("dist holds %v, want exactly %v", names, slices.Sorted(slices.Values(want)))
	}

	sums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	if len(lines) != len(platforms) {
		t.Errorf("SHA256SUMS has %d lines, want one per archive:\n%s", len(lines), sums)
	}
	for _, p := range platforms {
		name := "aidev_" + p + ".tar.gz"
		// The format sha256sum -c and shasum -a 256 -c both read.
		line := sha256Hex(t, filepath.Join(dist, name)) + "  " + name
		if !strings.Contains(string(sums), line+"\n") {
			t.Errorf("SHA256SUMS lacks %q", line)
		}

		files := untar(t, filepath.Join(dist, name))
		bin, ok := files["aidev"]
		if !ok || len(files) != 1 {
			t.Errorf("%s holds %d file(s), want exactly one named aidev", name, len(files))
			continue
		}
		if bin.mode&0o111 == 0 {
			t.Errorf("aidev in %s is not executable (mode %o)", name, bin.mode)
		}
		if p == runtime.GOOS+"_"+runtime.GOARCH {
			path := filepath.Join(t.TempDir(), "aidev")
			if err := os.WriteFile(path, bin.body, 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(path, "version").Output()
			if err != nil || strings.TrimSpace(string(out)) != "aidev v0.0.0-test" {
				t.Errorf("%s version = %q, %v; want the tag the release was built from", name, out, err)
			}
		}
		if strings.HasPrefix(p, "linux_") && !isStatic(t, bin.body) {
			t.Errorf("aidev in %s is dynamically linked; build with CGO_ENABLED=0 so it runs on any distribution", name)
		}
	}
}

// A release built without a version would print a meaningless one forever.
func TestBuildReleaseRequiresAVersion(t *testing.T) {
	cmd := exec.Command("sh", "scripts/build-release.sh")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "VERSION=", "DIST="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("scripts/build-release.sh built without VERSION:\n%s", out)
	}
	if !strings.Contains(string(out), "VERSION") {
		t.Errorf("the error does not name VERSION:\n%s", out)
	}
}

// isStatic reports whether an ELF binary asks for no dynamic loader.
func isStatic(t *testing.T, body []byte) bool {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("not an ELF binary: %v", err)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false
		}
	}
	return true
}
