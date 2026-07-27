// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// The repo-root legal texts are embedded here so every distributed artifact
// carries its own license and the third-party notices for everything
// statically linked or embedded into it. They are handed to the web server
// (SetLegal) and served unauthenticated at /license and
// /third-party-licenses; see scripts/gen-third-party-licenses.sh.
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
