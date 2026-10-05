// Package manifest embeds this module's muxcore.json so the reported
// version has a single source (ADR-0021).
package manifest

import _ "embed"

// ManifestJSON is the embedded muxcore.json. Info().Version is derived from it
// via modulesdk.ManifestVersion (ADR-0021).
//
//go:embed muxcore.json
var ManifestJSON []byte
