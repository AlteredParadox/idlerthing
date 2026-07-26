package web

import (
	"database/sql"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Units are 1024-based throughout: 1 GB = 1024 MB, 1 TB = 1024 GB.
// All size/bandwidth values are stored in whole MB.

// fmtMB renders whole MB in the friendliest unit (≤2 decimals, zeros trimmed).
func fmtMB(mb int64) string {
	if mb < 1024 {
		return strconv.FormatInt(mb, 10) + " MB"
	}
	if mb < 1024*1024 {
		return trim2(float64(mb)/1024) + " GB"
	}
	return trim2(float64(mb)/(1024*1024)) + " TB"
}

// fmtNullMB renders a nullable MB amount ("—" when NULL).
func fmtNullMB(n sql.NullInt64) string {
	if !n.Valid {
		return "—"
	}
	return fmtMB(n.Int64)
}

// bwCell renders bandwidth for list cells: "∞" when unlimited (NULL).
func bwCell(n sql.NullInt64) listCell {
	if !n.Valid {
		return listCell{Main: "∞", Class: "mono", Badge: false}
	}
	return listCell{Main: fmtMB(n.Int64), Class: "mono"}
}

// bwDisplay renders bandwidth for detail kv pairs.
func bwDisplay(n sql.NullInt64) string {
	if !n.Valid {
		return "∞ Unlimited"
	}
	return fmtMB(n.Int64)
}

func trim2(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// decomposeMB splits whole MB into a form value + unit for pre-fill:
// the largest unit that divides evenly (2048 → "2","GB"), else MB.
func decomposeMB(mb int64) (string, string) {
	if mb > 0 && mb%(1024*1024) == 0 {
		return strconv.FormatInt(mb/(1024*1024), 10), "TB"
	}
	if mb > 0 && mb%1024 == 0 {
		return strconv.FormatInt(mb/1024, 10), "GB"
	}
	if mb <= 0 {
		return "", "MB"
	}
	return strconv.FormatInt(mb, 10), "MB"
}

// unitVal returns the pre-fill number for an MB field (int64 or NullInt64).
func unitVal(v any) string {
	mb, valid := asMB(v)
	if !valid {
		return ""
	}
	s, _ := decomposeMB(mb)
	return s
}

// unitName returns the pre-fill unit for an MB field.
func unitName(v any) string {
	mb, valid := asMB(v)
	if !valid {
		return "MB"
	}
	_, u := decomposeMB(mb)
	return u
}

// mbValid reports whether the value is a set MB amount.
func mbValid(v any) bool {
	_, valid := asMB(v)
	return valid
}

// asMB normalizes int64 / sql.NullInt64 to (value, valid).
func asMB(v any) (int64, bool) {
	switch t := v.(type) {
	case sql.NullInt64:
		return t.Int64, t.Valid
	case int64:
		return t, true
	case int:
		return int64(t), true
	}
	return 0, false
}

// unitFactor converts a unit select value to its MB multiplier.
func unitFactor(u string) float64 {
	switch u {
	case "GB":
		return 1024
	case "TB":
		return 1024 * 1024
	default:
		return 1
	}
}

// sizeFormValue parses a number+unit input-group into whole MB.
func sizeFormValue(r *http.Request, errs map[string]string, name string, maxMB int64) sql.NullInt64 {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return sql.NullInt64{}
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 || math.IsNaN(f) {
		errs[name] = "Must be a number ≥ 0."
		return sql.NullInt64{}
	}
	// Range-check in the FLOAT domain first: int64(NaN/overflow) is
	// implementation-defined (MinInt64) and would slip past a post-convert check.
	total := f * unitFactor(r.FormValue(name+"_unit"))
	if math.IsInf(total, 0) || total > float64(maxMB) {
		errs[name] = "Value too large."
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(math.Round(total)), Valid: true}
}

// bandwidthFormValue parses bandwidth with the unlimited checkbox.
// Checked → NULL (unlimited); the number+unit are ignored then.
func bandwidthFormValue(r *http.Request, errs map[string]string, name string, maxMB int64) sql.NullInt64 {
	if r.FormValue(name+"_unlimited") != "" {
		return sql.NullInt64{}
	}
	return sizeFormValue(r, errs, name, maxMB)
}
