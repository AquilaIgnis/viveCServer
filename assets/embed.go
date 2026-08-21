package assets

import "embed"

// Files holds the artwork, served verbatim.

//go:embed *.svg
var Files embed.FS
