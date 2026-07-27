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

// Route bases shared by section handlers and their redirects (go:S1192 —
// duplicated route-path literals).
const (
	routeServers   = "/servers"
	routeSeedboxes = "/seedboxes"
	routeDomains   = "/domains"
	routeMisc      = "/misc"
	routeNotes     = "/notes"
	routeSettings  = "/settings"
)
