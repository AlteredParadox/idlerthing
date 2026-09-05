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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Batch V5 — a signed ingest whose body is not a benchmark (null, [],
// an empty object) is a 400 and persists nothing: no yabs row, has_yabs
// stays 0. The single-use capability is still consumed by design.
func TestYABSIngestRejectsBlankPayload(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "blank-yabs")

	// One pinned base: re-reading the clock per attempt can straddle a
	// second boundary and hand two attempts the SAME (server, ts)
	// capability, which the second then finds consumed (403, not 400).
	base := time.Now().Unix()
	for i, body := range []string{`null`, `[]`, `{}`} {
		now := base - int64(i) // distinct capability per attempt
		url := fmt.Sprintf("%s/api/yabs/1?sig=%s&ts=%d", ts.URL, signYABSTest(srv.secret, 1, now), now)
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, resp.StatusCode)
		}
	}
	var runs, hasYabs int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	database.QueryRow("SELECT has_yabs FROM servers WHERE id = 1").Scan(&hasYabs)
	if runs != 0 || hasYabs != 0 {
		t.Fatalf("blank payloads persisted: runs=%d has_yabs=%d", runs, hasYabs)
	}
}
