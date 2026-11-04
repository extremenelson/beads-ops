// Package bops embeds the command manifest so that the binary and every
// generated binding are built from the same bytes.
package bops

import _ "embed"

// ManifestJSON is the raw, normative commands.json (SPEC.md §4).
//
//go:embed commands.json
var ManifestJSON []byte
