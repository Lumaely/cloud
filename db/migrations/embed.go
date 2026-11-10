// Package migrations embeds the versioned SQL schema that goose applies; see README.md.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
