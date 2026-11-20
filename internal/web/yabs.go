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

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/yabs"
)

// yabsSigWindow is how long an ingest signature stays valid (defined in
// the model package — cap pruning depends on the same value).
//
// Threat note: the signature covers only {server_id}.{ts} — yabs.sh POSTs
// its JSON to the URL verbatim (the `-s <url>` contract), so a body hash
// cannot be part of the URL without a wrapper script. A leaked sig could
// replay MODIFIED payloads within the window; dedup only rejects
// byte-identical payloads. Mitigations: the window is kept short (2h),
// (server, payload_hash) dedup makes byte-identical replays idempotent,
// and the admin can rotate IDLER_SECRET (or delete <db>.secret) to kill
// leaked signatures. Residual modified-payload window is accepted.
const yabsSigWindow = model.YABSSigWindow

// signYABS produces the HMAC-SHA256 signature for "{server_id}.{ts}".
func (s *Server) signYABS(serverID, ts int64) string {
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
// The URL is POSIX-single-quoted — unquoted, the &ts= would background the
// shell command and the ingest would fail with a 403. IDLER_BASE_URL wins
// when set (validated http(s), no single-quote chars); otherwise the
// request's Host is used, https when TLS or behind a TLS proxy.
//
// Returns "" when the effective URL is plain http on a non-loopback host:
// an on-path attacker could alter the unsigned first submission, so the
// command is withheld and the card shows a hint instead. http stays allowed
// on loopback, and on LAN hosts when IDLER_ALLOW_HTTP_INGEST opted in.
func (s *Server) yabsCommand(r *http.Request, serverID int64) string {
	ts := time.Now().Unix()
	base := s.baseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || s.behindTLSProxy {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	if !s.ingestURLOK(base) {
		return ""
	}
	return fmt.Sprintf("curl -fsSL --proto '=https' https://yabs.sh | bash -s -- -s %s",
		shellQuote(fmt.Sprintf("%s/api/yabs/%d?sig=%s&ts=%d",
			base, serverID, s.signYABS(serverID, ts), ts)))
}

// ingestURLOK reports whether an ingest URL over this base is safe to show:
// https always; http only for exact loopback (127/8, ::1, localhost) — plus
// LAN hosts (RFC1918/link-local/ULA) when IDLER_ALLOW_HTTP_INGEST opted in.
func (s *Server) ingestURLOK(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false // routable hostname over plain http
	}
	if ip.IsLoopback() {
		return true
	}
	return s.allowHTTPIngest && (ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// shellQuote single-quotes a string for POSIX shells (' → '"'"').
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// handleYABSIngest handles POST /api/yabs/{id} — public, signature-authed.
func (s *Server) handleYABSIngest(w http.ResponseWriter, r *http.Request) {
	serverID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
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

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, model.YABSPayloadMax))
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}

	st := &model.YABSStore{DB: s.db}

	// (a) Byte-identical retry? Answer WITHOUT parsing — but make sure the
	// capability is consumed, so the URL can't be re-parsed for hours.
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])
	dup, err := st.IsDuplicate(r.Context(), serverID, "", hashHex)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	if dup {
		if _, err := st.ConsumeCap(r.Context(), serverID, ts); err != nil {
			writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	// (b) Novel payload: consume the capability in its OWN committed
	// transaction BEFORE parsing — a later rollback (e.g. gb_url unique
	// violation) can never un-consume it, and a stolen URL gets 403 here
	// without its body ever being parsed.
	consumed, err := st.ConsumeCap(r.Context(), serverID, ts)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	if !consumed {
		writeAPIError(w, http.StatusForbidden, "capability consumed")
		return
	}

	// (c) Only now parse + insert. gb_url duplicates surface as a
	// unique-index violation at insert time (migration 0010) and map to
	// duplicate inside Create; the cap stays consumed either way.
	id, err := s.storeYABSRun(r.Context(), serverID, body)
	switch {
	case errors.Is(err, errYABSInvalid):
		writeAPIError(w, http.StatusBadRequest, "invalid yabs JSON")
	case errors.Is(err, model.ErrDuplicatePayload):
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "yabs_id": id})
	}
}

// errYABSInvalid means a body could not be parsed as yabs.sh output.
var errYABSInvalid = errors.New("invalid yabs JSON")

// storeYABSRun parses a raw yabs.sh body and stores it as a run of serverID,
// keeping the body itself alongside. It is the one write path for a run:
// signed ingest and the authenticated add-run form both end here, so both
// apply the same bounds and store the same shape. Returns errYABSInvalid for
// an unparseable body and model.ErrDuplicatePayload when the run is already
// stored; authorisation and replay protection are the caller's job.
func (s *Server) storeYABSRun(ctx context.Context, serverID int64, body []byte) (int64, error) {
	result, err := yabs.Parse(body)
	if err != nil {
		return 0, errYABSInvalid
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
	// Cap child rows per run; a note goes nowhere — truncation is silent
	// by design (payloads claiming more are almost certainly abusive).
	var disks []model.YABSDiskSpeed
	for i, d := range result.Disks {
		if i >= 64 {
			break
		}
		disks = append(disks, model.YABSDiskSpeed{BlockSize: d.BlockSize, ReadMbps: d.ReadMbps, WriteMbps: d.WriteMbps})
	}
	var network []model.YABSNetworkSpeed
	for i, n := range result.Network {
		if i >= 64 {
			break
		}
		network = append(network, model.YABSNetworkSpeed{
			Location: n.Location, Provider: n.Provider, Mode: n.Mode,
			SendMbps: n.SendMbps, RecvMbps: n.RecvMbps, LatencyMs: n.LatencyMs,
		})
	}

	// Keep the raw body only when it can be kept exactly (valid UTF-8); the
	// parsed run is stored either way.
	payload := ""
	if model.RetainableYABSPayload(body) {
		payload = string(body)
	}
	id, err := (&model.YABSStore{DB: s.db}).CreateWithPayload(ctx, run, disks, network, payload)
	if err != nil {
		return 0, err
	}
	s.touchDashboard()
	return id, nil
}

// ---------- Views ----------

// yabsIndexView is the payload for the all-servers run list.
type yabsIndexView struct {
	Rows   []yabsRow
	AddURL string
}

// routeYABSNew is the add-run entry point that asks which server.
const routeYABSNew = "/yabs/new"

// serverYABSURL is the run list of one server; run and add-run URLs hang
// off it.
func serverYABSURL(serverID int64) string {
	return routeServers + "/" + strconv.FormatInt(serverID, 10) + "/yabs"
}

// handleYABSNew renders GET /yabs/new: the add-run page reached from the
// all-servers list, where the server is not known yet. It only asks which
// server; ?server=ID (the picker's own submit) forwards to that server's
// add-run page, which is where a missing server turns into a 404.
func (s *Server) handleYABSNew(w http.ResponseWriter, r *http.Request) {
	if id, err := strconv.ParseInt(r.URL.Query().Get("server"), 10, 64); err == nil && id > 0 {
		http.Redirect(w, r, serverYABSURL(id)+"/new", http.StatusSeeOther)
		return
	}
	servers, err := s.serverOptions(r)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "Add YABS run", "yabs")
	data.Data = yabsNewView{Servers: servers, BackURL: "/yabs"}
	s.render(w, r, "yabs_new", data)
}

// yabsNewView is the payload for the add-run page. ServerID is 0 while the
// server is still to be picked.
type yabsNewView struct {
	Servers    []model.CatalogItem // picker options (ServerID == 0)
	ServerID   int64
	ServerName string
	Command    string // signed ingest command; "" when withheld (see yabsCommand)
	Payload    string // pasted text, echoed back after a rejected submit
	Error      string
	BackURL    string
}

// yabsServer resolves the {id} path value to a server, answering 404/500
// itself when it cannot.
func (s *Server) yabsServer(w http.ResponseWriter, r *http.Request) (*model.Server, bool) {
	serverID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	srv, _, _, err := s.servers.Get(r.Context(), serverID)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return nil, false
	}
	return srv, true
}

// renderYABSNew renders the add-run page for one server. A rejected submit
// passes the pasted text and the reason back with a 422.
func (s *Server) renderYABSNew(w http.ResponseWriter, r *http.Request, srv *model.Server, payload, errMsg string) {
	data := s.newPageData(w, r, "Add YABS run · "+srv.Hostname, "yabs")
	data.Data = yabsNewView{
		ServerID:   srv.ID,
		ServerName: srv.Hostname,
		Command:    s.yabsCommand(r, srv.ID),
		Payload:    payload,
		Error:      errMsg,
		BackURL:    serverYABSURL(srv.ID),
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	s.render(w, r, "yabs_new", data)
}

// handleServerYABSNew renders GET /servers/{id}/yabs/new.
func (s *Server) handleServerYABSNew(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.yabsServer(w, r)
	if !ok {
		return
	}
	s.renderYABSNew(w, r, srv, "", "")
}

// pastedYABS normalises text pasted into the add-run form to the bytes
// yabs.sh would have sent: a browser submits a textarea with CRLF line
// breaks and the paste usually carries a trailing newline, and either would
// change the payload hash, so the same run added once by paste and once by
// the signed URL would not be recognised as a duplicate.
func pastedYABS(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// handleServerYABSCreate handles POST /servers/{id}/yabs: a run pasted into
// the add-run form by the signed-in user (session + CSRF, via protect). It
// stores through storeYABSRun, exactly like signed ingest.
func (s *Server) handleServerYABSCreate(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.yabsServer(w, r)
	if !ok {
		return
	}
	raw := r.PostFormValue("payload")
	body := pastedYABS(raw)
	switch {
	case body == "":
		s.renderYABSNew(w, r, srv, "", "Paste the JSON that yabs.sh printed.")
		return
	case len(body) > model.YABSPayloadMax:
		// Not echoed back: there is no point re-sending 64 KB+ of rejected text.
		s.renderYABSNew(w, r, srv, "", "That is larger than a yabs.sh result can be (64 KB).")
		return
	}

	listURL := serverYABSURL(srv.ID)
	duplicate := func() {
		s.setFlash(w, r, "ok", "That run is already recorded.")
		http.Redirect(w, r, listURL, http.StatusSeeOther)
	}
	sum := sha256.Sum256([]byte(body))
	dup, err := (&model.YABSStore{DB: s.db}).IsDuplicate(r.Context(), srv.ID, "", hex.EncodeToString(sum[:]))
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if dup {
		duplicate()
		return
	}

	id, err := s.storeYABSRun(r.Context(), srv.ID, []byte(body))
	switch {
	case errors.Is(err, errYABSInvalid):
		s.renderYABSNew(w, r, srv, raw, "That is not a yabs.sh JSON result. Paste the whole output of yabs.sh -j, from the first { to the last }.")
	case errors.Is(err, model.ErrDuplicatePayload):
		duplicate()
	case err != nil:
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
	default:
		s.setFlash(w, r, "ok", "YABS run added.")
		http.Redirect(w, r, listURL+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
	}
}

// handleServerYABSRaw serves GET /servers/{id}/yabs/{yid}/raw: the yabs.sh
// JSON a run was parsed from, byte for byte. 404 when the run is not this
// server's or has no payload (it predates payloads being kept).
func (s *Server) handleServerYABSRaw(w http.ResponseWriter, r *http.Request) {
	serverID, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	yabsID, err2 := strconv.ParseInt(r.PathValue("yid"), 10, 64)
	if err1 != nil || err2 != nil {
		http.NotFound(w, r)
		return
	}
	body, err := (&model.YABSStore{DB: s.db}).Payload(r.Context(), serverID, yabsID)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	// Stored text that arrived from outside: served as JSON (never HTML —
	// nosniff is set globally) and offered as a download rather than a page.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="yabs-run-`+strconv.FormatInt(yabsID, 10)+`.json"`)
	io.WriteString(w, body)
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
			ServerURL:  routeServers + "/" + strconv.FormatInt(it.ServerID, 10),
			URL:        routeServers + "/" + strconv.FormatInt(it.ServerID, 10) + "/yabs/" + strconv.FormatInt(it.ID, 10),
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
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if y.ServerID != serverID {
		http.NotFound(w, r)
		return
	}
	hasPayload, err := (&model.YABSStore{DB: s.db}).HasPayload(r.Context(), yabsID)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "YABS · "+srv.Hostname, "yabs")
	data.Data = yabsDetailView{
		ServerID:   serverID,
		ServerName: srv.Hostname,
		Run:        y,
		Disks:      disks,
		Network:    network,
		HasPayload: hasPayload,
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
	HasPayload bool // the raw yabs.sh JSON is kept and can be downloaded
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
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", "YABS run deleted.")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", routeServers+"/"+strconv.FormatInt(serverID, 10)+"/yabs")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, routeServers+"/"+strconv.FormatInt(serverID, 10)+"/yabs", http.StatusSeeOther)
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
