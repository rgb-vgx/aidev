package config

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// database.url is validated with pgxpool.ParseConfig, which accepts more than
// user:password@host. Each of these forms carries a password, and `aidev config`
// prints the redacted value: none of them may reach the output. The secret is a
// word that appears nowhere else in the string, so finding it means it leaked.
func TestRedactionCoversEveryFormPgxAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// keep are parts that are not secret and that a reader needs to see which
		// database is meant.
		keep []string
	}{
		{"keyword/value", "host=127.0.0.1 port=5434 user=aidev password=hunter2 dbname=aidev sslmode=disable",
			[]string{"host=127.0.0.1", "user=aidev", "dbname=aidev", "sslmode=disable"}},
		{"keyword/value, quoted", "host=127.0.0.1 user=aidev password='hunter2 with spaces' dbname=aidev",
			[]string{"host=127.0.0.1", "dbname=aidev"}},
		{"keyword/value, spaces around =", "host=127.0.0.1 password = hunter2 dbname=aidev",
			[]string{"host=127.0.0.1", "dbname=aidev"}},
		// An "@" in a keyword/value password must not be mistaken for the end of
		// URL userinfo.
		{"keyword/value, @ in the password", "host=127.0.0.1 user=aidev password=hunter2@x dbname=aidev",
			[]string{"host=127.0.0.1", "dbname=aidev"}},
		// pgx decodes query keys before it reads them.
		{"query parameter, percent-encoded key", "postgres://127.0.0.1/aidev?pass%77ord=hunter2&sslmode=disable",
			[]string{"127.0.0.1/aidev", "sslmode=disable"}},
		// libpq, and pgx after it, read a backslash in an unquoted value as an
		// escape: this password is "x hunter2", and all of it is secret.
		{"keyword/value, escaped space in the password", `host=127.0.0.1 password=x\ hunter2 dbname=aidev`,
			[]string{"host=127.0.0.1", "dbname=aidev"}},
		{"query parameter", "postgres://aidev@127.0.0.1:5434/aidev?password=hunter2&sslmode=disable",
			[]string{"127.0.0.1:5434/aidev", "sslmode=disable"}},
		{"query parameter first of several", "postgres://127.0.0.1/aidev?sslmode=disable&password=hunter2&application_name=x",
			[]string{"sslmode=disable", "application_name=x"}},
		{"userinfo and query", "postgres://aidev:hunter2@127.0.0.1/aidev?password=hunter2",
			[]string{"127.0.0.1/aidev"}},
		{"ssl key password", "postgres://aidev@127.0.0.1/aidev?sslpassword=hunter2&sslmode=verify-full",
			[]string{"sslmode=verify-full"}},
		{"postgresql scheme, uppercase key", "postgresql://aidev@127.0.0.1/aidev?PASSWORD=hunter2",
			[]string{"127.0.0.1/aidev"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Only forms pgx accepts matter: anything else never reaches Redacted.
			// The uppercase key is the exception pgx rejects; it is kept because a
			// redactor that is case-sensitive is one careless edit from a leak.
			if !strings.Contains(tc.in, "PASSWORD") {
				if _, err := pgxpool.ParseConfig(tc.in); err != nil {
					t.Fatalf("pgx does not accept %q, so the case proves nothing: %v", tc.in, err)
				}
			}
			got := RedactURL(tc.in)
			if strings.Contains(got, "hunter2") {
				t.Errorf("RedactURL(%q) = %q, which still contains the password", tc.in, got)
			}
			for _, k := range tc.keep {
				if !strings.Contains(got, k) {
					t.Errorf("RedactURL(%q) = %q, which lost %q", tc.in, got, k)
				}
			}
		})
	}
}

// The whole configuration is what gets printed, so the guarantee is checked there
// too.
func TestRedactedConfigHidesAKeywordValuePassword(t *testing.T) {
	cfg := Config{DatabaseURL: "host=127.0.0.1 user=aidev password=hunter2 dbname=aidev"}
	if strings.Contains(cfg.Redacted().DatabaseURL, "hunter2") {
		t.Errorf("Redacted() leaked the password: %q", cfg.Redacted().DatabaseURL)
	}
}
