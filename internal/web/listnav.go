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
