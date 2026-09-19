package release

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// install.sh is the one command a new user runs:
//
//	curl -fsSL https://raw.githubusercontent.com/rgb-vgx/aidev/main/install.sh | sh
//
// It picks the archive for this machine, checks it against SHA256SUMS, installs
// the binary and says what to do next. These tests serve a release from a local
// server laid out like GitHub's (AIDEV_DOWNLOAD_BASE replaces
// https://github.com/rgb-vgx/aidev/releases) and run the script with sh, the
// POSIX shell a piped install gets.

const testVersion = "v9.9.9-test"

// release is a fake GitHub release: the host's archive and SHA256SUMS, served at
// both /latest/download/<name> and /download/<tag>/<name>.
type release struct {
	dir      string
	mu       sync.Mutex
	requests []string
}

func (r *release) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func newRelease(t *testing.T) (*release, *httptest.Server) {
	t.Helper()
	r := &release{dir: t.TempDir()}

	bin := filepath.Join(t.TempDir(), "aidev")
	build := exec.Command("go", "build", "-ldflags", "-X main.version="+testVersion, "-o", bin, "./cmd/aidev")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	name := "aidev_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	writeArchive(t, filepath.Join(r.dir, name), bin)
	writeSums(t, r.dir, name, sha256Hex(t, filepath.Join(r.dir, name)))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requests = append(r.requests, req.URL.Path)
		r.mu.Unlock()
		file := filepath.Base(req.URL.Path)
		dir := filepath.Dir(req.URL.Path)
		if dir != "/latest/download" && dir != "/download/"+testVersion {
			http.NotFound(w, req)
			return
		}
		http.ServeFile(w, req, filepath.Join(r.dir, file))
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

func writeArchive(t *testing.T, path, bin string) {
	t.Helper()
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "aidev", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func writeSums(t *testing.T, dir, name, sum string) {
	t.Helper()
	line := fmt.Sprintf("%s  %s\n", sum, name)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

type installRun struct {
	stdout, stderr string
	err            error
	installDir     string
	home           string
}

// runInstall runs install.sh with sh. extraPath, when set, is put first on PATH
// (to replace uname).
func runInstall(t *testing.T, base string, env []string, extraPath string) installRun {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget is installed")
		}
	}
	home := t.TempDir()
	installDir := filepath.Join(home, "bin")
	path := os.Getenv("PATH")
	if extraPath != "" {
		path = extraPath + string(os.PathListSeparator) + path
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Dir = repoRoot
	cmd.Env = append([]string{
		"HOME=" + home,
		"PATH=" + path,
		"AIDEV_DOWNLOAD_BASE=" + base,
		"AIDEV_INSTALL_DIR=" + installDir,
	}, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return installRun{stdout: stdout.String(), stderr: stderr.String(), err: err, installDir: installDir, home: home}
}

func TestInstallFetchesTheLatestReleaseChecksItAndSaysWhatIsNext(t *testing.T) {
	rel, srv := newRelease(t)
	run := runInstall(t, srv.URL, nil, "")
	if run.err != nil {
		t.Fatalf("install.sh: %v\nstdout:\n%s\nstderr:\n%s", run.err, run.stdout, run.stderr)
	}

	bin := filepath.Join(run.installDir, "aidev")
	out, err := exec.Command(bin, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "aidev "+testVersion {
		t.Fatalf("installed aidev version = %q, %v; want %s", out, err, testVersion)
	}

	archive := "/latest/download/aidev_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	got := strings.Join(rel.requested(), " ")
	if !strings.Contains(got, archive) || !strings.Contains(got, "/latest/download/SHA256SUMS") {
		t.Errorf("requests %v, want %s and its SHA256SUMS", rel.requested(), archive)
	}

	// The install directory is not on this PATH: say how to add it.
	if !strings.Contains(run.stdout, run.installDir) || !strings.Contains(run.stdout, "PATH") {
		t.Errorf("output does not say how to put %s on PATH:\n%s", run.installDir, run.stdout)
	}
	for _, want := range []string{"aidev setup", "opencode"} {
		if !strings.Contains(run.stdout, want) {
			t.Errorf("output does not mention %q as a next step:\n%s", want, run.stdout)
		}
	}
	// Nothing is left beside the binary: no archive, no checksum file.
	entries, _ := os.ReadDir(run.installDir)
	if len(entries) != 1 {
		t.Errorf("the install directory holds %d entries, want only aidev", len(entries))
	}
}

// Pinning a version downloads that release, not the latest.
func TestInstallAPinnedVersion(t *testing.T) {
	rel, srv := newRelease(t)
	run := runInstall(t, srv.URL, []string{"AIDEV_VERSION=" + testVersion}, "")
	if run.err != nil {
		t.Fatalf("install.sh: %v\n%s\n%s", run.err, run.stdout, run.stderr)
	}
	for _, p := range rel.requested() {
		if !strings.HasPrefix(p, "/download/"+testVersion+"/") {
			t.Errorf("requested %s; a pinned install must fetch only from /download/%s/", p, testVersion)
		}
	}
}

// A download that does not match SHA256SUMS is not installed: a corrupted or
// tampered binary must never land on PATH.
func TestInstallRefusesAnArchiveThatFailsItsChecksum(t *testing.T) {
	rel, srv := newRelease(t)
	name := "aidev_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	writeSums(t, rel.dir, name, strings.Repeat("0", 64))

	run := runInstall(t, srv.URL, nil, "")
	if run.err == nil {
		t.Fatalf("install.sh accepted an archive whose checksum does not match\n%s", run.stdout)
	}
	if !strings.Contains(strings.ToLower(run.stderr), "checksum") {
		t.Errorf("stderr does not say the checksum failed:\n%s", run.stderr)
	}
	if _, err := os.Stat(filepath.Join(run.installDir, "aidev")); !os.IsNotExist(err) {
		t.Errorf("aidev was installed despite the failed checksum (stat: %v)", err)
	}
}

// Replacing an installed aidev keeps the old one when the new one fails.
func TestAFailedInstallKeepsThePreviousBinary(t *testing.T) {
	rel, srv := newRelease(t)
	name := "aidev_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	writeSums(t, rel.dir, name, strings.Repeat("0", 64))

	home := t.TempDir()
	installDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(installDir, "aidev")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "install.sh")
	cmd.Dir = repoRoot
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"),
		"AIDEV_DOWNLOAD_BASE=" + srv.URL, "AIDEV_INSTALL_DIR=" + installDir}
	if err := cmd.Run(); err == nil {
		t.Fatal("install.sh succeeded with a bad checksum")
	}
	body, _ := os.ReadFile(old)
	if string(body) != "#!/bin/sh\necho old\n" {
		t.Errorf("the previous aidev was replaced or removed: %q", body)
	}
}

// fakeUname puts a uname that reports the given system and machine first on PATH.
func fakeUname(t *testing.T, system, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n-s) echo %s ;;\n-m) echo %s ;;\n*) echo %s ;;\nesac\n", system, machine, system)
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// uname names machines in several ways; the archives use Go's names.
func TestInstallMapsTheMachineToTheArchiveName(t *testing.T) {
	for _, c := range []struct{ system, machine, archive string }{
		{"Linux", "x86_64", "aidev_linux_amd64.tar.gz"},
		{"Linux", "aarch64", "aidev_linux_arm64.tar.gz"},
		{"Linux", "arm64", "aidev_linux_arm64.tar.gz"},
		{"Darwin", "x86_64", "aidev_darwin_amd64.tar.gz"},
		{"Darwin", "arm64", "aidev_darwin_arm64.tar.gz"},
	} {
		t.Run(c.system+"_"+c.machine, func(t *testing.T) {
			rel, srv := newRelease(t)
			run := runInstall(t, srv.URL, nil, fakeUname(t, c.system, c.machine))
			if !strings.Contains(strings.Join(rel.requested(), " "), "/latest/download/"+c.archive) {
				t.Errorf("requests %v, want %s\nstderr:\n%s", rel.requested(), c.archive, run.stderr)
			}
		})
	}
}

// There is no Windows or 32-bit build: say so plainly instead of downloading a
// file that does not exist.
func TestInstallRejectsAnUnsupportedPlatform(t *testing.T) {
	for _, c := range []struct{ system, machine, named string }{
		{"MINGW64_NT-10.0", "x86_64", "MINGW64_NT-10.0"},
		{"Linux", "i686", "i686"},
	} {
		t.Run(c.named, func(t *testing.T) {
			rel, srv := newRelease(t)
			run := runInstall(t, srv.URL, nil, fakeUname(t, c.system, c.machine))
			if run.err == nil {
				t.Fatalf("install.sh succeeded on %s %s", c.system, c.machine)
			}
			if !strings.Contains(run.stderr, c.named) {
				t.Errorf("stderr does not name %s:\n%s", c.named, run.stderr)
			}
			if len(rel.requested()) != 0 {
				t.Errorf("requested %v for an unsupported platform", rel.requested())
			}
		})
	}
}

// A missing release (a typo in AIDEV_VERSION, or no release yet) fails with the
// URL that was tried, not with a tar error about an HTML page.
func TestInstallReportsAMissingReleaseByURL(t *testing.T) {
	_, srv := newRelease(t)
	run := runInstall(t, srv.URL, []string{"AIDEV_VERSION=v0.0.0-missing"}, "")
	if run.err == nil {
		t.Fatal("install.sh succeeded for a release that does not exist")
	}
	if !strings.Contains(run.stderr, srv.URL+"/download/v0.0.0-missing/") {
		t.Errorf("stderr does not name the URL that failed:\n%s", run.stderr)
	}
}
