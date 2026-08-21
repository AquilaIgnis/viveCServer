// Package assets carries the administration panel's image files, compiled into the binary.
//
// Embedded rather than read from disk for the reason the migrations are (see migrations/embed.go):
// a self-hoster copies one file, and an icon cannot go missing from a deployment that has the code
// which references it.
//
// A package at the repository root rather than a directory under internal/httpapi, because these
// are drawings a person edits in Inkscape and `assets/` is where that person looks for them.
// `go:embed` cannot reach upwards out of its own directory, so the alternative would have been to
// move the artwork somewhere it is harder to find.
package assets

import "embed"

// Files holds the artwork, served verbatim.
//
// Verbatim includes the editor metadata Inkscape writes -- `sodipodi:namedview`, window geometry,
// the last zoom level. It is about a kilobyte per file, it is cached immutably by the digest in the
// served path, and stripping it would mean the copy in this repository is no longer the copy that
// opens cleanly in the editor it came out of.
//
//go:embed *.svg
var Files embed.FS
