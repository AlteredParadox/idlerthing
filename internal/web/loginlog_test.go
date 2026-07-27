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
	"bytes"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The log lines below are a CONTRACT with deploy/fail2ban/idlerthing.conf.
// Changing their shape silently stops bans working, so the filter's patterns
// are reproduced here and matched against real output.
var (
	reFailedAuth = regexp.MustCompile(`^WARN login: failed authentication from=(\S+)(\s|$)`)
	reRateLimit  = regexp.MustCompile(`^WARN login: rate-limited from=(\S+)$`)
	rePwVerify   = regexp.MustCompile(`^WARN login: failed password-change verification from=(\S+)$`)
	reAuthOK     = regexp.MustCompile(`^INFO login: authenticated from=`)
)

// captureLog swaps the default logger's sink (slog's default handler writes
// through it) and restores the previous flags — the binary runs with
// SetFlags(0) so journald's own timestamp is not duplicated.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(prevFlags)
	})
	return buf
}

func TestLoginLogFailedAuthentication(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	buf := captureLog(t)

	resp := login(t, newClient(t), ts, "wrongpass")
	resp.Body.Close()

	m := reFailedAuth.FindStringSubmatch(strings.TrimSpace(buf.String()))
	if m == nil {
		t.Fatalf("no fail2ban-matching line; got:\n%s", buf.String())
	}
	if m[1] != "127.0.0.1" {
		t.Fatalf("captured host %q, want 127.0.0.1", m[1])
	}
}

// A crafted username must not be able to forge or shift the <HOST> capture:
// it is emitted after the address and slog quotes it.
func TestLoginLogUsernameCannotForgeHost(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	buf := captureLog(t)

	client := newClient(t)
	csrf := getLoginCSRF(t, client, ts)
	resp, err := client.PostForm(ts.URL+"/login", url.Values{
		"csrf_token": {csrf},
		"email":      {"evil from=1.2.3.4 x"},
		"password":   {"nope"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	line := strings.TrimSpace(buf.String())
	m := reFailedAuth.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no matching line; got:\n%s", line)
	}
	if m[1] != "127.0.0.1" {
		t.Fatalf("crafted username shifted the host capture to %q\nline: %s", m[1], line)
	}
}

// Blocked attempts are logged for the watcher, but only once per window —
// otherwise a flood of already-refused requests amplifies into unbounded
// journald writes.
func TestLoginLogRateLimitedOncePerWindow(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	buf := captureLog(t)

	for i := 0; i < 15; i++ {
		resp := login(t, newClient(t), ts, "wrongpass")
		resp.Body.Close()
	}

	var blocked int
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if reRateLimit.MatchString(l) {
			blocked++
		}
	}
	if blocked != 1 {
		t.Fatalf("got %d rate-limited lines, want exactly 1 per window", blocked)
	}
	_ = srv
}

func TestLoginLogSuccessIsAuditedNotFailed(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	buf := captureLog(t)

	resp := login(t, newClient(t), ts, testPassword)
	resp.Body.Close()

	line := strings.TrimSpace(buf.String())
	if !reAuthOK.MatchString(line) {
		t.Fatalf("no audit line for a successful login; got:\n%s", line)
	}
	// The ignoreregex exists because this must never trip a ban.
	if reFailedAuth.MatchString(line) || reRateLimit.MatchString(line) {
		t.Fatalf("a successful login matched a failure pattern:\n%s", line)
	}
}

func TestLoginLogPasswordChangeVerification(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	client := newClient(t)
	login(t, client, ts, testPassword).Body.Close()
	csrf := sessionCSRF(t, client, ts)

	buf := captureLog(t)
	resp := postForm(t, client, ts, "/settings/account", url.Values{
		"csrf_token":       {csrf},
		"action":           {"password"},
		"current_password": {"not-the-password"},
		"new_password":     {"replacement123"},
		"confirm_password": {"replacement123"},
	})
	resp.Body.Close()

	if m := rePwVerify.FindStringSubmatch(strings.TrimSpace(buf.String())); m == nil {
		t.Fatalf("no password-change failure line; got:\n%s", buf.String())
	} else if m[1] != "127.0.0.1" {
		t.Fatalf("captured host %q, want 127.0.0.1", m[1])
	}
}
