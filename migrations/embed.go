// Package migrations embeds the SQL migration set into the binary so the
// schema and the code that expects it ship as a single artifact and cannot
// drift apart in transit.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
