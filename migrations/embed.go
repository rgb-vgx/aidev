// Package migrations embeds aidev's SQL migrations so that a single binary can
// bring a database up to date with no external tooling.
package migrations

import "embed"

// FS holds every migration, named <version>_<description>.sql and applied in
// lexical order.
//
//go:embed *.sql
var FS embed.FS
