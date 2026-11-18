// Package extensions embeds the bundled extension manifests
// (<id>/extension.json) for internal/harness, which go:embed cannot reach
// from its own directory.
package extensions

import "embed"

// FS holds <id>/extension.json for each bundled extension.
//
//go:embed */extension.json
var FS embed.FS
