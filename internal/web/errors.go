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

package web

// Shared error message literals (go:S1192 — duplicated string literals).
const (
	// errMsgInternal is the generic API error message (no internals leak).
	errMsgInternal = "internal error"
	// errMsgNotFound is the generic API 404 message.
	errMsgNotFound = "not found"
	// errMsgBadRequest is the generic 400 body.
	errMsgBadRequest = "bad request"
	// errMsgServerErr is the generic 500 body for page handlers.
	errMsgServerErr = "internal server error"
)
