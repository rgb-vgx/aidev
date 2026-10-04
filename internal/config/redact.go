package config

import (
	"net/url"
	"strings"
)

// Redacted returns the configuration with secrets removed, for logging. aidev
// never logs a connection string or a tracing credential verbatim.
func (c Config) Redacted() Config {
	c.DatabaseURL = RedactURL(c.DatabaseURL)
	if c.Tracing.Headers != nil {
		redacted := make(map[string]string, len(c.Tracing.Headers))
		for k := range c.Tracing.Headers {
			redacted[k] = "***"
		}
		c.Tracing.Headers = redacted
	}
	return c
}

// RedactURL removes passwords from a PostgreSQL connection string, in either
// form pgxpool.ParseConfig accepts. A string starting with postgres:// or
// postgresql:// (case-insensitive) is a URL: userinfo is reduced to user:***
// (or *** when there is no user) and the value of any query parameter named
// password or sslpassword (case-insensitive) becomes ***. Anything else is a
// keyword/value string: the value of password or sslpassword
// (case-insensitive keys, optional spaces around '=', quoted or unquoted
// values, backslash escapes as libpq reads them) becomes ***. Query keys are
// compared after percent-decoding, as pgx reads them. It works on a plain
// string rather than net/url so that an unparseable value is redacted
// conservatively instead of being passed through; when the input cannot be
// understood it hides too much rather than leaking, e.g. an unterminated
// quote after password= hides the rest of the string.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		return redactURLString(raw)
	}
	return redactKeywordValue(raw)
}

func redactURLString(raw string) string {
	idx := strings.Index(raw, "://")
	if idx < 0 {
		return raw
	}
	scheme := raw[:idx+3]
	rest := raw[idx+3:]
	authEnd := len(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' || rest[i] == '?' || rest[i] == '#' {
			authEnd = i
			break
		}
	}
	authority := rest[:authEnd]
	remainder := rest[authEnd:]
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo := authority[:at]
		host := authority[at+1:]
		user := userinfo
		if c := strings.Index(userinfo, ":"); c >= 0 {
			user = userinfo[:c]
		}
		if user == "" {
			authority = "***@" + host
		} else {
			authority = user + ":***@" + host
		}
	}
	qPos := strings.Index(remainder, "?")
	if qPos < 0 {
		return scheme + authority + remainder
	}
	fragPos := strings.Index(remainder[qPos:], "#")
	var query, fragment string
	if fragPos < 0 {
		query = remainder[qPos+1:]
	} else {
		query = remainder[qPos+1 : qPos+fragPos]
		fragment = remainder[qPos+fragPos:]
	}
	parts := strings.Split(query, "&")
	for i, p := range parts {
		eq := strings.Index(p, "=")
		if eq < 0 {
			continue
		}
		key := p[:eq]
		// pgx decodes a key before reading it, so pass%77ord is a password.
		decoded, err := url.QueryUnescape(key)
		if err != nil || isSecretKey(decoded) {
			parts[i] = key + "=***"
		}
	}
	return scheme + authority + remainder[:qPos+1] + strings.Join(parts, "&") + fragment
}

func isKVSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

func redactKeywordValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	n := len(s)
	i := 0
	for i < n {
		if isKVSpace(s[i]) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < n && s[j] != '=' && !isKVSpace(s[j]) {
			j++
		}
		eq := j
		for eq < n && isKVSpace(s[eq]) {
			eq++
		}
		if eq >= n || s[eq] != '=' {
			b.WriteString(s[i:eq])
			i = eq
			continue
		}
		v := eq + 1
		for v < n && isKVSpace(s[v]) {
			v++
		}
		end, closed := scanKVValue(s, v)
		if !isSecretKey(s[i:j]) {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		b.WriteString(s[i:v])
		b.WriteString("***")
		if !closed {
			// An open quote: where the password ends is unknown, so
			// nothing after it is printed.
			return b.String()
		}
		i = end
	}
	return b.String()
}

// scanKVValue returns where the value starting at v ends, the way libpq reads
// it: a quoted value runs to its closing quote, an unquoted one to the next
// space, and in both a backslash escapes the character after it. closed is false
// when a quote is never closed.
func scanKVValue(s string, v int) (end int, closed bool) {
	n := len(s)
	quoted := v < n && s[v] == '\''
	p := v
	if quoted {
		p++
	}
	for p < n {
		switch {
		case s[p] == '\\':
			p += 2
			continue
		case quoted && s[p] == '\'':
			return p + 1, true
		case !quoted && isKVSpace(s[p]):
			return p, true
		}
		p++
	}
	if p > n {
		p = n
	}
	return p, !quoted
}

func isSecretKey(key string) bool {
	return strings.EqualFold(key, "password") || strings.EqualFold(key, "sslpassword")
}
