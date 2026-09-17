package docs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"aidev/internal/config"
)

// `make jaeger-env` and `make langfuse-env` exist to point aidev at a local tracing
// backend. They still print `export OTEL_...` lines, and the docs still say
// `eval "$(make jaeger-env)"`, but aidev reads no OTEL_* variable any more: tracing
// lives in the "tracing" object of conf.json. Following the docs turns tracing on
// in nothing, silently. docs/research.md records this as a known defect.
//
// The targets now print a JSON object, {"tracing": {...}}, that a reader pastes
// into conf.json. These tests hold them to that by loading what they print.

// runMake runs a make target quietly from the repository root and returns stdout.
func runMake(t *testing.T, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	cmd := exec.Command("make", append([]string{"-s", "--no-print-directory"}, args...)...)
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("make %s: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

// loadTracingObject checks that out is exactly one JSON object whose only key is
// "tracing", then loads it as part of a real conf.json so the settings are the ones
// aidev would use, not merely well-formed JSON.
func loadTracingObject(t *testing.T, target string, out []byte) config.Config {
	t.Helper()
	var printed map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&printed); err != nil {
		t.Fatalf("make %s does not print a JSON object: %v\noutput:\n%s", target, err, out)
	}
	if dec.More() {
		t.Fatalf("make %s prints more than one JSON value:\n%s", target, out)
	}
	if len(printed) != 1 || printed["tracing"] == nil {
		t.Fatalf("make %s must print {\"tracing\": {...}} and nothing else, got keys %v", target, keysOf(printed))
	}
	if bytes.Contains(out, []byte("export ")) || bytes.Contains(out, []byte("OTEL_")) {
		t.Errorf("make %s still prints environment exports, which aidev ignores:\n%s", target, out)
	}

	conf := map[string]json.RawMessage{
		"database": json.RawMessage(`{"url": "postgres://aidev:aidev@127.0.0.1:5434/aidev?sslmode=disable"}`),
		"tracing":  printed["tracing"],
	}
	body, err := json.Marshal(conf)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "conf.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("aidev rejects the tracing object make %s prints: %v\noutput:\n%s", target, err, out)
	}
	return cfg
}

func keysOf(m map[string]json.RawMessage) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestJaegerEnvPrintsATracingObjectAidevAccepts(t *testing.T) {
	cfg := loadTracingObject(t, "jaeger-env", runMake(t, "jaeger-env"))
	if got, want := cfg.Tracing.Endpoint, "http://localhost:4318"; got != want {
		t.Errorf("tracing.endpoint = %q, want %q (Jaeger's OTLP HTTP port)", got, want)
	}
	if got, want := cfg.Tracing.ServiceName, "aidev"; got != want {
		t.Errorf("tracing.service_name = %q, want %q", got, want)
	}
	if len(cfg.Tracing.Headers) != 0 {
		t.Errorf("tracing.headers = %v, want none: local Jaeger needs no authorization", cfg.Tracing.Headers)
	}
}

// langfuse-env reads the project keys from $(LANGFUSE_ENV). The test points that
// variable at a file of its own, by absolute path, so it never generates or reads
// the real deployments/langfuse/.env.
func TestLangfuseEnvPrintsATracingObjectAidevAccepts(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "langfuse.env")
	const public, secret = "pk-lf-test", "sk-lf-test"
	if err := os.WriteFile(envFile, []byte(
		"LANGFUSE_INIT_PROJECT_PUBLIC_KEY="+public+"\n"+
			"LANGFUSE_INIT_PROJECT_SECRET_KEY="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := loadTracingObject(t, "langfuse-env", runMake(t, "langfuse-env", "LANGFUSE_ENV="+envFile))
	if got, want := cfg.Tracing.Endpoint, "http://localhost:3000/api/public/otel"; got != want {
		t.Errorf("tracing.endpoint = %q, want %q", got, want)
	}
	if got, want := cfg.Tracing.ServiceName, "aidev"; got != want {
		t.Errorf("tracing.service_name = %q, want %q", got, want)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(public+":"+secret))
	if got := cfg.Tracing.Headers["Authorization"]; got != wantAuth {
		t.Errorf("tracing.headers.Authorization = %q, want %q", got, wantAuth)
	}
	if got, want := cfg.Tracing.Headers["x-langfuse-ingestion-version"], "4"; got != want {
		t.Errorf("tracing.headers.x-langfuse-ingestion-version = %q, want %q", got, want)
	}
}

// `eval "$(make jaeger-env)"` would now try to run JSON as shell. The docs must say
// to put the printed object into conf.json instead.
func TestDocsDoNotEvalTheTracingTargets(t *testing.T) {
	for _, path := range docFiles {
		text := readDoc(t, path)
		for _, target := range []string{"jaeger-env", "langfuse-env"} {
			if strings.Contains(text, `eval "$(make `+target+`)"`) {
				t.Errorf("%s still says eval \"$(make %s)\"; the target prints a tracing object for conf.json", path, target)
			}
		}
	}
	for _, path := range []string{"../../README.md", "../../docs/architecture.md"} {
		text := readDoc(t, path)
		for _, target := range []string{"make jaeger-env", "make langfuse-env"} {
			if !strings.Contains(text, target) {
				t.Errorf("%s no longer mentions %s, the way to get the tracing object", path, target)
			}
		}
	}
}

// AIDEV_CONFIG unset and a file that cannot be read are different mistakes with
// different messages. The guide conflated them: it said a missing file makes
// `aidev config` report that AIDEV_CONFIG is not set, but LoadFile reports
// "read config file <path>: ...". A reader matching the text to the screen must
// find the message they actually see.
func TestGuideQuotesTheMessageForAMissingConfigFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err := config.LoadFile(missing)
	if err == nil || !strings.Contains(err.Error(), "read config file") {
		t.Fatalf("LoadFile(missing) = %v; this test assumes it says \"read config file\"", err)
	}
	for _, path := range []string{
		"../../docs/guide/getting-started.html",
		"../../docs/guide/vi/getting-started.html",
	} {
		text := readDoc(t, path)
		if !strings.Contains(text, "read config file") {
			t.Errorf("%s does not quote the message for a missing conf.json (\"read config file ...\")", path)
		}
		if !strings.Contains(text, "AIDEV_CONFIG is not set") {
			t.Errorf("%s does not quote the message for an unset AIDEV_CONFIG (\"AIDEV_CONFIG is not set\")", path)
		}
	}
}

// `make check` sets AIDEV_REQUIRE_DB=1, which turns an integration test's skip into
// a failure. The reference says the tests skip without TEST_DATABASE_URL, which is
// only half the story.
func TestReferenceExplainsAidevRequireDB(t *testing.T) {
	for _, path := range []string{
		"../../docs/guide/reference.html",
		"../../docs/guide/vi/reference.html",
	} {
		if !strings.Contains(readDoc(t, path), "AIDEV_REQUIRE_DB") {
			t.Errorf("%s does not mention AIDEV_REQUIRE_DB, which makes a skipped integration test fail", path)
		}
	}
}
