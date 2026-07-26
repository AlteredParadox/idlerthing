package web

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"idlerthing/internal/model"
)

// publicCacheEntry caches the public page for 60s (per Server).
type publicCacheEntry struct {
	mu   sync.Mutex
	at   time.Time
	rows []publicRow
}

// publicRow is one row of the public servers page.
type publicRow struct {
	Hostname string
	Type     string
	OS       string
	CPU      string
	RAM      string
	Disk     string
	BW       string
	Location string
	Provider string
	Price    string
}

// handlePublic renders GET /public — unauthenticated, only when enabled.
func (s *Server) handlePublic(w http.ResponseWriter, r *http.Request) {
	var enabled int
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT servers_public FROM settings WHERE id = 1").Scan(&enabled); err != nil || enabled == 0 {
		http.NotFound(w, r)
		return
	}

	s.publicCache.mu.Lock()
	rows := s.publicCache.rows
	fresh := rows != nil && s.publicCache.at.After(time.Now().Add(-60*time.Second))
	s.publicCache.mu.Unlock()

	if !fresh {
		var err error
		rows, err = s.computePublicRows(r)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		s.publicCache.mu.Lock()
		s.publicCache.rows = rows
		s.publicCache.at = time.Now()
		s.publicCache.mu.Unlock()
	}

	s.renderPublic(w, r, rows)
}

// computePublicRows loads active+public servers.
func (s *Server) computePublicRows(r *http.Request) ([]publicRow, error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT s.hostname, s.server_type, COALESCE(os.name, ''), s.cpu, s.ram_as_mb,
			COALESCE((SELECT SUM(size_as_mb) FROM server_disks d WHERE d.server_id = s.id), 0),
			s.bandwidth_as_mb, COALESCE(l.name, ''), COALESCE(p.name, ''),
			pr.currency, pr.price, pr.term
		FROM servers s
		LEFT JOIN os ON os.id = s.os_id
		LEFT JOIN locations l ON l.id = s.location_id
		LEFT JOIN providers p ON p.id = s.provider_id
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = 1 AND pr.active = 1
		WHERE s.show_public = 1 AND s.active = 1
		ORDER BY s.hostname COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []publicRow
	for rows.Next() {
		var hostname, osName, locName, provName string
		var serverType, diskMB int64
		var cpu, ramMB, bw interface{}
		var currency *string
		var price *float64
		var term *int64
		if err := rows.Scan(&hostname, &serverType, &osName, &cpu, &ramMB, &diskMB,
			&bw, &locName, &provName, &currency, &price, &term); err != nil {
			return nil, err
		}
		row := publicRow{
			Hostname: hostname,
			Type:     model.ServerTypeLabel(int(serverType)),
			OS:       dash(osName),
			Disk:     "—",
			Location: dash(locName),
			Provider: dash(provName),
			Price:    "—",
		}
		if v, ok := cpu.(int64); ok {
			row.CPU = strconv.FormatInt(v, 10) + "×"
		} else {
			row.CPU = "—"
		}
		if v, ok := ramMB.(int64); ok {
			row.RAM = fmtMB(v)
		} else {
			row.RAM = "—"
		}
		if diskMB > 0 {
			row.Disk = fmtMB(diskMB)
		}
		if v, ok := bw.(int64); ok {
			row.BW = fmtMB(v)
		} else {
			row.BW = "∞"
		}
		if currency != nil && price != nil && term != nil {
			row.Price = priceDisplay(*currency, *price, int(*term))
		}
		out = append(out, row)
	}
	if out == nil {
		out = []publicRow{}
	}
	return out, rows.Err()
}

// renderPublic renders the standalone public page.
func (s *Server) renderPublic(w http.ResponseWriter, r *http.Request, rows []publicRow) {
	tm, ok := s.tmpl.pages["public"]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tm.ExecuteTemplate(w, "public", map[string]any{
		"Theme":   s.currentTheme(r),
		"Rows":    rows,
		"AssetV":  assetVersion,
		"Updated": time.Now().Format("2006-01-02 15:04"),
	}); err != nil {
		slog.Error("render public", "err", err)
	}
}
