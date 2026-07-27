package web

import (
	"fmt"
	"html"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// pingRunner is injectable for tests. It returns latency in ms, or an error.
var pingRunner = execPing

// pingBinary is resolved ONCE, at startup, preferring fixed absolute paths so
// a PATH entry pointing at a writable directory cannot substitute a shim for
// the ping we exec. The PATH lookup is only a last resort for layouts that
// put it elsewhere; "ping" alone (relative, resolved per-exec) is never used.
var pingBinary = resolvePing()

func resolvePing() string {
	for _, p := range []string{"/bin/ping", "/usr/bin/ping", "/sbin/ping", "/usr/sbin/ping"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	if p, err := exec.LookPath("ping"); err == nil {
		return p
	}
	return "" // absent — execPing reports "ping not available"
}

// execPing runs one ICMP ping via the system ping binary.
func execPing(host string) (float64, error) {
	if pingBinary == "" {
		return 0, fmt.Errorf("ping not available")
	}
	cmd := exec.Command(pingBinary, "-c", "1", "-W", "2", host)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if execErr, ok := err.(*exec.Error); ok && execErr.Err == exec.ErrNotFound {
			return 0, fmt.Errorf("ping not available")
		}
		return 0, fmt.Errorf("unreachable")
	}
	// Parse "time=12.3 ms" or "time<1 ms".
	s := string(out)
	if i := strings.Index(s, "time="); i >= 0 {
		s = s[i+5:]
	} else if i := strings.Index(s, "time<"); i >= 0 {
		s = "0." + s[i+5:]
	} else {
		return 0, fmt.Errorf("unreachable")
	}
	end := strings.Index(s, " ms")
	if end < 0 {
		return 0, fmt.Errorf("unreachable")
	}
	ms, err := strconv.ParseFloat(strings.TrimSpace(s[:end]), 64)
	if err != nil {
		return 0, fmt.Errorf("unreachable")
	}
	return ms, nil
}

// hostnameRe is a strict hostname pattern (no metacharacters possible).
var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?$`)

// validPingTarget accepts IPs or strict hostnames.
func validPingTarget(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return hostnameRe.MatchString(host) && !strings.Contains(host, "..")
}

// handlePing handles POST /tools/ping and returns a small status partial.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromCtx(r.Context())
	if sess == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !s.pingLimit.allow(sess.Token) {
		writePingStatus(w, "off", "rate limited")
		return
	}

	host := strings.TrimSpace(r.FormValue("host"))
	if !validPingTarget(host) {
		writePingStatus(w, "off", "invalid host")
		return
	}

	ms, err := pingRunner(host)
	if err != nil {
		if err.Error() == "ping not available" {
			writePingStatus(w, "off", "ping n/a")
			return
		}
		writePingStatus(w, "err", "unreachable")
		return
	}
	writePingStatus(w, "ok", fmt.Sprintf("%.1f ms", ms))
}

// writePingStatus renders the swap-in status span.
func writePingStatus(w http.ResponseWriter, class, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<span class="dot dot-%s"></span> <span class="cell-sub">%s</span>`, class, html.EscapeString(text))
}
