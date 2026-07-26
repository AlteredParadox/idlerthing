package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/yabs"
)

// yabsSigWindow is how long an ingest signature stays valid.
//
// Threat note: the signature covers only {server_id}.{ts} — yabs.sh POSTs
// its JSON to the URL verbatim (the `-s <url>` contract), so a body hash
// cannot be part of the URL without a wrapper script. A leaked sig could
// replay MODIFIED payloads within the window; dedup only rejects
// byte-identical payloads. Mitigations: the window is kept short (2h),
// (server, payload_hash) dedup makes byte-identical replays idempotent,
// and the admin can rotate IDLER_SECRET (or delete <db>.secret) to kill
// leaked signatures. Residual modified-payload window is accepted.
const yabsSigWindow = 2 * time.Hour

// signYABS produces the HMAC-SHA256 signature for "{server_id}.{ts}".
func (s *Server) signYABS(serverID int64, ts int64) string {
	mac := hmac.New(sha256.New, s.secret)
	fmt.Fprintf(mac, "%d.%d", serverID, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// validYABSSig verifies ts freshness + signature in constant time.
func (s *Server) validYABSSig(serverID int64, ts int64, sig string) bool {
	now := time.Now().Unix()
	if ts < now-int64(yabsSigWindow/time.Second) || ts > now+300 {
		return false
	}
	want := s.signYABS(serverID, ts)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

// yabsCommand builds the ingest command shown on the server detail page.
// IDLER_BASE_URL wins when set; otherwise the request's scheme+Host is
// used (Host-header derived, which may be wrong behind proxies).
func (s *Server) yabsCommand(r *http.Request, serverID int64) string {
	ts := time.Now().Unix()
	base := s.baseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return fmt.Sprintf("curl -sL yabs.sh | bash -s -- -s %s/api/yabs/%d?sig=%s&ts=%d",
		base, serverID, s.signYABS(serverID, ts), ts)
}

// handleYABSIngest handles POST /api/yabs/{id} — public, signature-authed.
func (s *Server) handleYABSIngest(w http.ResponseWriter, r *http.Request) {
	serverID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	ts, _ := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
	sig := r.URL.Query().Get("sig")
	if !s.validYABSSig(serverID, ts, sig) {
		writeAPIError(w, http.StatusForbidden, "invalid or expired signature")
		return
	}

	// Server must exist.
	var exists int
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM servers WHERE id = ?", serverID).Scan(&exists); err != nil || exists == 0 {
		writeAPIError(w, http.StatusNotFound, "server not found")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	result, err := yabs.Parse(body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid yabs JSON")
		return
	}

	st := &model.YABSStore{DB: s.db}
	dup, err := st.IsDuplicate(r.Context(), serverID, result.GbURL, result.PayloadHash)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if dup {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	run := &model.YABS{
		ServerID:         serverID,
		CPU:              sqlNs(result.CPU),
		CPUCores:         sqlNi(int64(result.CPUCores)),
		RAM:              sqlNs(result.RAM),
		Swap:             sqlNs(result.Swap),
		Distro:           sqlNs(result.Distro),
		Kernel:           sqlNs(result.Kernel),
		Uptime:           sqlNs(result.Uptime),
		GeekbenchVersion: sqlNi(int64(result.GeekbenchVersion)),
		GbSingle:         sqlNi(int64(result.GbSingle)),
		GbMulti:          sqlNi(int64(result.GbMulti)),
		GbURL:            sqlNs(result.GbURL),
		PayloadHash:      sqlNs(result.PayloadHash),
	}
	if result.RunAt != "" {
		run.RunAt = sqlNs(result.RunAt)
	} else {
		run.RunAt = sqlNs(time.Now().UTC().Format(time.RFC3339))
	}
	var disks []model.YABSDiskSpeed
	for _, d := range result.Disks {
		disks = append(disks, model.YABSDiskSpeed{BlockSize: d.BlockSize, ReadMbps: d.ReadMbps, WriteMbps: d.WriteMbps})
	}
	var network []model.YABSNetworkSpeed
	for _, n := range result.Network {
		network = append(network, model.YABSNetworkSpeed{
			Location: n.Location, Provider: n.Provider,
			SendMbps: n.SendMbps, RecvMbps: n.RecvMbps, LatencyMs: n.LatencyMs,
		})
	}

	id, err := st.Create(r.Context(), run, disks, network)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.touchDashboard()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "yabs_id": id})
}

// ---------- Views ----------

// handleYABSIndex renders GET /yabs (all runs).
func (s *Server) handleYABSIndex(w http.ResponseWriter, r *http.Request) {
	items, err := (&model.YABSStore{DB: s.db}).ListAll(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "YABS", "yabs")
	data.Data = yabsIndexView{Rows: yabsRows(items)}
	s.render(w, r, "yabs", data)
}

// handleServerYABS renders GET /servers/{id}/yabs (runs for one server).
func (s *Server) handleServerYABS(w http.ResponseWriter, r *http.Request) {
	serverID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	srv, _, _, err := s.servers.Get(r.Context(), serverID)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	items, err := (&model.YABSStore{DB: s.db}).ListFor(r.Context(), serverID)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "YABS · "+srv.Hostname, "yabs")
	data.Data = yabsIndexView{
		Rows:       yabsRows(items),
		ServerName: srv.Hostname,
	}
	s.render(w, r, "yabs", data)
}

// yabsIndexView is the payload for yabs list pages.
type yabsIndexView struct {
	Rows       []yabsRow
	ServerName string // set on per-server pages
}

// yabsRow is one run row.
type yabsRow struct {
	ID         int64
	ServerName string
	ServerURL  string
	URL        string
	Date       string
	CPU        string
	GbSingle   string
	GbMulti    string
}

func yabsRows(items []model.YABSListItem) []yabsRow {
	var rows []yabsRow
	for _, it := range items {
		date := it.CreatedAt
		if it.RunAt.Valid {
			date = it.RunAt.String
		}
		rows = append(rows, yabsRow{
			ID:         it.ID,
			ServerName: it.ServerHostname,
			ServerURL:  "/servers/" + strconv.FormatInt(it.ServerID, 10),
			URL:        "/servers/" + strconv.FormatInt(it.ServerID, 10) + "/yabs/" + strconv.FormatInt(it.ID, 10),
			Date:       dateOnly(date),
			CPU:        it.CPU.String,
			GbSingle:   niDisplay(it.GbSingle),
			GbMulti:    niDisplay(it.GbMulti),
		})
	}
	return rows
}

// handleServerYABSDetail renders GET /servers/{id}/yabs/{yid}.
func (s *Server) handleServerYABSDetail(w http.ResponseWriter, r *http.Request) {
	serverID, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	yabsID, err2 := strconv.ParseInt(r.PathValue("yid"), 10, 64)
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	srv, _, _, err := s.servers.Get(r.Context(), serverID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	y, disks, network, err := (&model.YABSStore{DB: s.db}).Get(r.Context(), yabsID)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if y.ServerID != serverID {
		http.NotFound(w, r)
		return
	}
	data := s.newPageData(w, r, "YABS · "+srv.Hostname, "yabs")
	data.Data = yabsDetailView{
		ServerID:   serverID,
		ServerName: srv.Hostname,
		Run:        y,
		Disks:      disks,
		Network:    network,
	}
	s.render(w, r, "yabs_detail", data)
}

// yabsDetailView is the payload for the run detail page.
type yabsDetailView struct {
	ServerID   int64
	ServerName string
	Run        *model.YABS
	Disks      []model.YABSDiskSpeed
	Network    []model.YABSNetworkSpeed
}

// handleServerYABSDelete handles POST /servers/{id}/yabs/{yid}/delete.
func (s *Server) handleServerYABSDelete(w http.ResponseWriter, r *http.Request) {
	serverID, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	yabsID, err2 := strconv.ParseInt(r.PathValue("yid"), 10, 64)
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	if err := (&model.YABSStore{DB: s.db}).Delete(r.Context(), serverID, yabsID); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	setFlash(w, "ok", "YABS run deleted.")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/servers/"+strconv.FormatInt(serverID, 10)+"/yabs")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/servers/"+strconv.FormatInt(serverID, 10)+"/yabs", http.StatusSeeOther)
}

func sqlNs(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func sqlNi(i int64) sql.NullInt64 {
	if i == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: i, Valid: true}
}

func niDisplay(n sql.NullInt64) string {
	if !n.Valid {
		return "—"
	}
	return strconv.FormatInt(n.Int64, 10)
}
