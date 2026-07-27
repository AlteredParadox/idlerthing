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

import "net/url"

// listNav carries the sort/status/search state shared by list pages and
// provides the header-link builders both list views use.
type listNav struct {
	Base   string // list path, e.g. "/servers"
	Status string
	Q      string
	Sort   string
	Dir    string
}

// SortLink builds the href for a sortable header, toggling direction.
func (v listNav) SortLink(col string) string {
	dir := "asc"
	if v.Sort == col && v.Dir == "asc" {
		dir = "desc"
	}
	return v.queryString(col, dir)
}

// SortArrow returns the accent arrow for the active sort column.
func (v listNav) SortArrow(col string) string {
	if v.Sort != col {
		return ""
	}
	if v.Dir == "desc" {
		return "↓"
	}
	return "↑"
}

// SortClass marks the active sort column header.
func (v listNav) SortClass(col string) string {
	if v.Sort == col {
		return "sorted"
	}
	return ""
}

func (v listNav) queryString(sort, dir string) string {
	q := "status=" + v.Status
	if v.Q != "" {
		q += "&q=" + url.QueryEscape(v.Q)
	}
	return v.Base + "?" + q + "&sort=" + sort + "&dir=" + dir
}

// StatusLink preserves the current search across tab switches.
func (v listNav) StatusLink(status string) string {
	q := v.Base + "?status=" + status
	if v.Q != "" {
		q += "&q=" + url.QueryEscape(v.Q)
	}
	return q
}
