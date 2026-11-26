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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

// serverByHost loads one server row by hostname.
func serverByHost(t *testing.T, st *model.ServerStore, host string) *model.Server {
	t.Helper()
	items, err := st.List(context.Background(), model.ListOptions{Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Hostname == host {
			srv := it.Server
			return &srv
		}
	}
	t.Fatalf("server %q not found", host)
	return nil
}

func TestServerKeepAndInitialPriceForm(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	st := &model.ServerStore{DB: database}

	// New-server form defaults Keep to checked.
	resp, err := client.Get(ts.URL + "/servers/new")
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); !strings.Contains(body, `name="keep" checked`) {
		t.Fatal("new server form should default keep to checked")
	}
	resp.Body.Close()

	createFullServer(t, client, ts, "keep-same", url.Values{"keep": {"on"}})
	createFullServer(t, client, ts, "keep-free", url.Values{"initial_free": {"on"}, "initial_price": {"7"}})
	createFullServer(t, client, ts, "keep-own", url.Values{"keep": {"on"}, "initial_price": {"30"}, "initial_term": {"4"}})

	same := serverByHost(t, st, "keep-same")
	if !same.Keep || same.InitialPrice.Valid || same.InitialTerm.Valid {
		t.Fatalf("keep-same: %+v", same)
	}
	free := serverByHost(t, st, "keep-free")
	if free.Keep || !free.InitialPrice.Valid || free.InitialPrice.Float64 != 0 || free.InitialTerm.Valid {
		t.Fatalf("keep-free: Free must win over the typed amount and keep unchecked must stick: %+v", free)
	}
	own := serverByHost(t, st, "keep-own")
	if own.InitialPrice.Float64 != 30 || own.InitialTerm.Int64 != model.TermAnnual {
		t.Fatalf("keep-own: %+v", own)
	}

	applyCols(t, client, ts, allColKeys())
	resp, err = client.Get(ts.URL + "/servers?sort=initial_price&dir=asc")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"Free", "$30.00/yr", `<span class="badge badge-err">No</span>`, `<span class="badge badge-ok">Yes</span>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("list missing %q", want)
		}
	}
	// Free (0) < same-as-price ($10) < $30.
	iFree, iSame, iOwn := strings.Index(body, "keep-free"), strings.Index(body, "keep-same"), strings.Index(body, "keep-own")
	if !(iFree < iSame && iSame < iOwn) {
		t.Fatalf("initial price sort order wrong: free=%d same=%d own=%d", iFree, iSame, iOwn)
	}

	// A non-free initial price without a price is rejected (no currency).
	resp = postForm(t, client, ts, "/servers", url.Values{"hostname": {"keep-bad"}, "initial_price": {"5"}})
	body = readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther || !strings.Contains(body, "initial price uses its currency") {
		t.Fatalf("initial price without price should fail validation (status %d)", resp.StatusCode)
	}
}

func TestServerKeepAndInitialPriceEdit(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	st := &model.ServerStore{DB: database}
	createFullServer(t, client, ts, "edit-01", url.Values{"keep": {"on"}, "initial_free": {"on"}})
	srv := serverByHost(t, st, "edit-01")
	path := "/servers/" + strconv.FormatInt(srv.ID, 10)

	// Edit form shows the stored state: keep on, Free ticked, amount blank.
	resp, err := client.Get(ts.URL + path + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `name="keep" checked`) || !strings.Contains(body, `name="initial_free" checked`) {
		t.Fatal("edit form should show keep and Free checked")
	}

	// Uncheck keep, swap Free for a typed amount and term.
	vals := url.Values{"hostname": {"edit-01"}, "server_type": {"1"}, "active": {"on"},
		"price": {"10"}, "currency": {"EUR"}, "term": {"1"},
		"initial_price": {"12.5"}, "initial_term": {"3"}}
	resp = postForm(t, client, ts, path+"/update", vals)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	srv = serverByHost(t, st, "edit-01")
	if srv.Keep || srv.InitialPrice.Float64 != 12.5 || srv.InitialTerm.Int64 != model.TermSemiannual {
		t.Fatalf("after edit: %+v", srv)
	}
	resp, err = client.Get(ts.URL + path + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, `name="keep" checked`) || strings.Contains(body, `name="initial_free" checked`) ||
		!strings.Contains(body, `name="initial_price" min="0" step="0.01" placeholder="Same as price" value="12.5"`) ||
		!strings.Contains(body, `<option value="3" selected>`) {
		t.Fatal("edit form should re-render keep off, Free off, 12.5 and Semi-Annual")
	}
	resp, err = client.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "€12.50/6mo") || !strings.Contains(body, "Not keeping") {
		t.Fatal("detail should show the initial price in the pricing currency and Not keeping")
	}

	// Clearing the amount drops the term too ("same as price").
	vals.Set("initial_price", "")
	resp = postForm(t, client, ts, path+"/update", vals)
	resp.Body.Close()
	if srv = serverByHost(t, st, "edit-01"); srv.InitialPrice.Valid || srv.InitialTerm.Valid {
		t.Fatalf("blank initial price should clear amount and term: %+v", srv)
	}
}

func TestServerPriceYrSort(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	// $10/mo = $120/yr; $60/yr; no pricing (stays last both ways).
	createFullServer(t, client, ts, "py-monthly", nil)
	createFullServer(t, client, ts, "py-yearly", url.Values{"price": {"60"}, "term": {"4"}})
	createFullServer(t, client, ts, "py-none", url.Values{"price": {""}})
	applyCols(t, client, ts, allColKeys())

	order := func(dir string) []int {
		resp, err := client.Get(ts.URL + "/servers?sort=price_yr&dir=" + dir)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		return []int{strings.Index(body, "py-yearly"), strings.Index(body, "py-monthly"), strings.Index(body, "py-none")}
	}
	if o := order("asc"); !(o[0] < o[1] && o[1] < o[2]) {
		t.Fatalf("asc: want yearly, monthly, none; got %v", o)
	}
	if o := order("desc"); !(o[1] < o[0] && o[0] < o[2]) {
		t.Fatalf("desc: want monthly, yearly, none; got %v", o)
	}
}

func TestServerKeepInitialExportImportRoundTrip(t *testing.T) {
	ts, dbA := newTestServer(t)
	ctx := context.Background()
	st := &model.ServerStore{DB: dbA}
	if _, err := st.Create(ctx, &model.Server{Hostname: "rt-01", ServerType: model.TypeKVM, Active: true,
		InitialPrice: sql.NullFloat64{Float64: 25, Valid: true},
		InitialTerm:  sql.NullInt64{Int64: model.TermAnnual, Valid: true}}, nil,
		&model.Pricing{Currency: "USD", Price: 5, Term: model.TermMonthly}); err != nil {
		t.Fatal(err)
	}
	doc := exportJSONDoc(t, ts, authedClient(t, ts), "/export/json")
	raw, _ := json.Marshal(doc)
	dbB := freshDB(t)
	if _, err := importer.Import(ctx, dbB, strings.NewReader(string(raw)), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	got := serverByHost(t, &model.ServerStore{DB: dbB}, "rt-01")
	if got.Keep || got.InitialPrice.Float64 != 25 || got.InitialTerm.Int64 != model.TermAnnual {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	// A backup from before keep existed restores as kept.
	old := `{"format": 1, "servers": [{"server": {"id": 1, "hostname": "old-01", "server_type": 1, "active": true}}]}`
	dbC := freshDB(t)
	if _, err := importer.Import(ctx, dbC, strings.NewReader(old), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := serverByHost(t, &model.ServerStore{DB: dbC}, "old-01"); !got.Keep || got.InitialPrice.Valid {
		t.Fatalf("old backup: %+v", got)
	}
}

func TestAPIServerKeepDefaultsAndInitialValidation(t *testing.T) {
	j := &serverJSON{Hostname: "api-01"}
	srv, _, _, errs := j.toModel()
	if len(errs) != 0 || !srv.Keep {
		t.Fatalf("keep should default true: %+v %v", srv, errs)
	}
	neg := -1.0
	j = &serverJSON{Hostname: "api-02", InitialPrice: &neg}
	if _, _, _, errs := j.toModel(); errs["initial_price"] == "" {
		t.Fatal("negative initial price must be rejected")
	}
	five := 5.0
	j = &serverJSON{Hostname: "api-03", InitialPrice: &five}
	if _, _, _, errs := j.toModel(); errs["initial_price"] == "" {
		t.Fatal("initial price without pricing must be rejected")
	}
}

// A descending sort on a COALESCE expression must keep the function's
// argument comma intact (it used to render COALESCE(a DESC, b) DESC → 500).
func TestServerInitialPriceSortDesc(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createFullServer(t, client, ts, "desc-free", url.Values{"initial_free": {"on"}})
	createFullServer(t, client, ts, "desc-own", url.Values{"initial_price": {"30"}})
	for _, dir := range []string{"desc", "asc"} {
		resp, err := client.Get(ts.URL + "/servers?sort=initial_price&dir=" + dir)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", dir, resp.StatusCode)
		}
		own, free := strings.Index(body, "desc-own"), strings.Index(body, "desc-free")
		if (dir == "desc") != (own < free) {
			t.Fatalf("%s: wrong order own=%d free=%d", dir, own, free)
		}
	}
}
