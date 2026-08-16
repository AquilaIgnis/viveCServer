// Package migrations carries the SQL that defines the database, compiled into the binary.
//
// Embedded rather than read from disk so that "single binary" (H1) is literally true: a self-hoster
// copies one file, and the schema the server expects cannot drift from the code that queries it.
package migrations

import "embed"

// Files holds every migration, in goose's format and naming.
//
// Names are `<version>_<description>.sql` with a five-digit sequential version, which is what
// `goose create -s <name> sql` produces — keeping the convention means the CLI and the embedded
// runner never disagree about what comes next. Applied in ascending version order, and **never
// edited once released**: goose records which versions ran, not what they contained, so an edited
// migration leaves every database that ran the old text silently different from every database that
// runs the new one. Add a migration instead.
//
//go:embed *.sql
var Files embed.FS
