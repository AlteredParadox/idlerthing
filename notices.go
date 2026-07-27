// Package idlerthing embeds the repo-root legal texts into the binary, so
// every distributed artifact carries its own license and the third-party
// notices for everything statically linked or embedded into it. Served
// unauthenticated at /license and /third-party-licenses; see
// scripts/gen-third-party-licenses.sh.
package main

// Blank import for its side effect: enables the //go:embed directives below
// (the embed package need not be referenced by name).
import _ "embed"

// License is the project's own license (GNU AGPL-3.0-or-later).
//
//go:embed LICENSE
var License []byte

// ThirdPartyLicenses reproduces the license and copyright notices of all
// bundled third-party code: the Go modules linked into the binary and the
// front-end assets embedded from internal/web/assets. Regenerate with
// scripts/gen-third-party-licenses.sh after any dependency or asset change.
//
//go:embed THIRD_PARTY_LICENSES.md
var ThirdPartyLicenses []byte
