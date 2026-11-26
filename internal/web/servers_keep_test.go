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
	if !same.Keep || same.InitialPrice.Float64 != 10 || same.InitialTerm.Int64 != model.TermMonthly ||
		same.InitialCurrency.String != "USD" {
		t.Fatalf("keep-same: a blank initial price should copy the price, term and currency: %+v", same)
	}
	free := serverByHost(t, st, "keep-free")
	if free.Keep || !free.InitialPrice.Valid || free.InitialPrice.Float64 != 0 || free.InitialTerm.Valid || free.InitialCurrency.Valid {
		t.Fatalf("keep-free: Free must win over the typed amount and keep unchecked must stick: %+v", free)
	}
	own := serverByHost(t, st, "keep-own")
	if own.InitialPrice.Float64 != 30 || own.InitialTerm.Int64 != model.TermAnnual || own.InitialCurrency.String != "USD" {
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
	// By real cost: Free ($0) < $30/yr ($2.50/mo) < copied $10/mo.
	iFree, iSame, iOwn := strings.Index(body, "keep-free"), strings.Index(body, "keep-same"), strings.Index(body, "keep-own")
	if !(iFree < iOwn && iOwn < iSame) {
		t.Fatalf("initial price sort order wrong: free=%d same=%d own=%d", iFree, iSame, iOwn)
	}

	// A non-free initial price with no currency to use is rejected; with
	// its own currency it stands alone.
	resp = postForm(t, client, ts, "/servers", url.Values{"hostname": {"keep-bad"}, "initial_price": {"5"}})
	body = readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther || !strings.Contains(body, "Pick an initial currency") {
		t.Fatalf("initial price without any currency should fail validation (status %d)", resp.StatusCode)
	}
	resp = postForm(t, client, ts, "/servers", url.Values{"hostname": {"keep-gbp"}, "active": {"on"}, "initial_price": {"5"},
		"initial_currency": {"GBP"}, "initial_term": {"4"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("initial price with its own currency should save (status %d)", resp.StatusCode)
	}
	resp, err = client.Get(ts.URL + "/servers?sort=hostname&dir=asc")
	if err != nil {
		t.Fatal(err)
	}
	if body = readBody(t, resp); !strings.Contains(body, "£5.00/yr") {
		t.Fatal("unpriced server should list its own initial price")
	}
	resp.Body.Close()
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
		!strings.Contains(body, `name="initial_price" min="0" step="0.01" placeholder="Copy of price" value="12.5"`) ||
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

	// A later price change leaves the frozen initial price alone. The
	// re-rendered form posts the stored initial currency back.
	resp, err = client.Get(ts.URL + path + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	initialSel := body[strings.Index(body, `name="initial_currency"`):]
	initialSel = initialSel[:strings.Index(initialSel, "</select>")]
	if !strings.Contains(initialSel, `<option value="EUR" selected>EUR</option>`) {
		t.Fatal("edit form should show the copied initial currency")
	}
	vals.Set("initial_currency", "EUR")
	vals.Set("price", "15")
	vals.Set("currency", "USD")
	vals.Set("term", "4")
	resp = postForm(t, client, ts, path+"/update", vals)
	resp.Body.Close()
	srv = serverByHost(t, st, "edit-01")
	if srv.InitialPrice.Float64 != 12.5 || srv.InitialTerm.Int64 != model.TermSemiannual || srv.InitialCurrency.String != "EUR" {
		t.Fatalf("price change must not move the initial price: %+v", srv)
	}

	// Clearing the amount re-copies the whole current price (a chosen term
	// and currency are dropped with the amount).
	vals.Set("initial_price", "")
	resp = postForm(t, client, ts, path+"/update", vals)
	resp.Body.Close()
	srv = serverByHost(t, st, "edit-01")
	if srv.InitialPrice.Float64 != 15 || srv.InitialTerm.Int64 != model.TermAnnual || srv.InitialCurrency.String != "USD" {
		t.Fatalf("blank initial price should copy the current price: %+v", srv)
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
	if got.Keep || got.InitialPrice.Float64 != 25 || got.InitialTerm.Int64 != model.TermAnnual || got.InitialCurrency.String != "USD" {
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
	eur, unlisted, malformed := "EUR", "XYZ", "eu"
	j = &serverJSON{Hostname: "api-04", InitialPrice: &five, InitialCurrency: &eur}
	if srv, _, _, errs := j.toModel(); len(errs) != 0 || srv.InitialCurrency.String != "EUR" || j.unlistedCurrency {
		t.Fatalf("initial price with its own currency should pass: %v", errs)
	}
	// A well-formed unlisted code is deferred to the store (stored-only).
	j = &serverJSON{Hostname: "api-05", InitialPrice: &five, InitialCurrency: &unlisted}
	if _, _, _, errs := j.toModel(); len(errs) != 0 || !j.unlistedCurrency || !j.fieldsSet().InitialCurrencyStoredOnly {
		t.Fatalf("unlisted code should be marked stored-only: %v", errs)
	}
	j = &serverJSON{Hostname: "api-06", InitialPrice: &five, InitialCurrency: &malformed}
	if _, _, _, errs := j.toModel(); errs["initial_currency"] == "" {
		t.Fatal("malformed initial currency must be rejected")
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

// A PUT that omits keep / initial_price / initial_term (a client that
// predates them) keeps the stored values; explicit false / null replace them.
func TestAPIServerPutKeepsOmittedNewFields(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	st := &model.ServerStore{DB: database}
	id, err := st.Create(context.Background(), &model.Server{Hostname: "put-01", ServerType: model.TypeKVM,
		Active: true, Keep: false,
		InitialPrice: sql.NullFloat64{Float64: 25, Valid: true},
		InitialTerm:  sql.NullInt64{Int64: model.TermAnnual, Valid: true}}, nil,
		&model.Pricing{Currency: "USD", Price: 5, Term: model.TermMonthly})
	if err != nil {
		t.Fatal(err)
	}
	put := func(body string) {
		t.Helper()
		req, _ := http.NewRequest("PUT", ts.URL+"/api/servers/"+strconv.FormatInt(id, 10), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s: %d", body, resp.StatusCode)
		}
	}
	pricing := `"pricing": {"currency": "USD", "price": 5, "term": 1}`

	put(`{"hostname": "put-01", "server_type": 1, ` + pricing + `}`)
	got := serverByHost(t, st, "put-01")
	if got.Keep || got.InitialPrice.Float64 != 25 || got.InitialTerm.Int64 != model.TermAnnual || got.InitialCurrency.String != "USD" {
		t.Fatalf("omitted fields should be kept: %+v", got)
	}

	// An old client dropping pricing keeps the frozen initial price too.
	put(`{"hostname": "put-01", "server_type": 1}`)
	if got = serverByHost(t, st, "put-01"); got.InitialPrice.Float64 != 25 || got.InitialCurrency.String != "USD" {
		t.Fatalf("initial price should survive pricing removal: %+v", got)
	}

	// Explicit keep and a null initial price replace: null re-copies the
	// current price, dropping the stored term and currency.
	put(`{"hostname": "put-01", "server_type": 1, "keep": true, "initial_price": null, ` + pricing + `}`)
	got = serverByHost(t, st, "put-01")
	if !got.Keep || got.InitialPrice.Float64 != 5 || got.InitialTerm.Int64 != model.TermMonthly {
		t.Fatalf("explicit keep/null should replace: %+v", got)
	}
}

// A positive initial price with no currency and no pricing is a 422 on
// both POST and PUT (the store enforces it after the PUT merge).
func TestAPIServerInitialNoCurrency(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	do := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do("POST", "/api/servers", `{"hostname": "nc-01", "initial_price": 5}`); c != http.StatusUnprocessableEntity {
		t.Fatalf("POST: %d", c)
	}
	if c := do("POST", "/api/servers", `{"hostname": "nc-02", "pricing": {"currency": "USD", "price": 5, "term": 1}}`); c != http.StatusCreated {
		t.Fatalf("POST priced: %d", c)
	}
	// Stored USD copy, then an explicit null currency and no pricing: no
	// currency left to use.
	if c := do("PUT", "/api/servers/1", `{"hostname": "nc-02", "initial_currency": null}`); c != http.StatusUnprocessableEntity {
		t.Fatalf("PUT: %d", c)
	}
}

// A legacy PUT (no initial_* fields) against a server whose stored initial
// currency came from an import and isn't in the dropdown still succeeds:
// stored history isn't re-validated as new input.
func TestAPIServerPutKeepsImportedCurrency(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	st := &model.ServerStore{DB: database}
	id, err := st.Create(context.Background(), &model.Server{Hostname: "chf-01", ServerType: model.TypeKVM, Active: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE servers SET initial_price = 40, initial_term = 4, initial_currency = 'CHF' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", ts.URL+"/api/servers/"+strconv.FormatInt(id, 10),
		strings.NewReader(`{"hostname": "chf-01", "server_type": 1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy PUT: %d", resp.StatusCode)
	}
	if got := serverByHost(t, st, "chf-01"); got.InitialCurrency.String != "CHF" || got.InitialPrice.Float64 != 40 {
		t.Fatalf("imported initial price changed: %+v", got)
	}

	put := func(body string) int {
		t.Helper()
		req, _ := http.NewRequest("PUT", ts.URL+"/api/servers/"+strconv.FormatInt(id, 10), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Read-modify-write: the stored CHF sent back unchanged is accepted.
	if c := put(`{"hostname": "chf-02", "server_type": 1, "initial_price": 40, "initial_term": 4, "initial_currency": "CHF"}`); c != http.StatusOK {
		t.Fatalf("round-tripped CHF: %d", c)
	}
	if got := serverByHost(t, st, "chf-02"); got.InitialCurrency.String != "CHF" {
		t.Fatalf("CHF lost on round trip: %+v", got)
	}
	// A different unlisted code, or a malformed one, is not.
	for _, cur := range []string{"SEK", "chf"} {
		if c := put(`{"hostname": "chf-02", "server_type": 1, "initial_price": 40, "initial_currency": "` + cur + `"}`); c != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d", cur, c)
		}
	}
	if got := serverByHost(t, st, "chf-02"); got.InitialCurrency.String != "CHF" {
		t.Fatalf("rejected PUT changed the currency: %+v", got)
	}
	// A new server can't start with an unlisted code.
	req, _ = http.NewRequest("POST", ts.URL+"/api/servers",
		strings.NewReader(`{"hostname": "chf-new", "initial_price": 1, "initial_currency": "CHF"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST unlisted currency: %d", resp.StatusCode)
	}
}

// UpdateFields merges unset fields from the row as it is at write time, so
// a change committed after the caller last read the server survives.
func TestUpdateFieldsMergesLiveRow(t *testing.T) {
	_, database := newTestServer(t)
	ctx := context.Background()
	st := &model.ServerStore{DB: database}
	id, err := st.Create(ctx, &model.Server{Hostname: "race-01", ServerType: model.TypeKVM, Active: true, Keep: true}, nil,
		&model.Pricing{Currency: "USD", Price: 5, Term: model.TermMonthly})
	if err != nil {
		t.Fatal(err)
	}
	stale := serverByHost(t, st, "race-01") // a client's earlier read: Keep=true
	// Someone else unticks Keep and sets an initial price.
	if _, err := database.Exec("UPDATE servers SET keep = 0, initial_price = 3 WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	stale.Hostname = "race-renamed"
	if err := st.UpdateFields(ctx, stale, nil, &model.Pricing{Currency: "USD", Price: 5, Term: model.TermMonthly},
		model.ServerFieldsSet{}); err != nil {
		t.Fatal(err)
	}
	got := serverByHost(t, st, "race-renamed")
	if got.Keep || got.InitialPrice.Float64 != 3 {
		t.Fatalf("stale copy overwrote the live keep/initial price: %+v", got)
	}
}

// An imported initial currency outside the dropdown (CHF) is offered on the
// edit form and survives saving an unrelated change, even when the renewal
// price is in another currency. A new value outside the list is still
// refused.
func TestServerFormKeepsImportedInitialCurrency(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	st := &model.ServerStore{DB: database}
	createFullServer(t, client, ts, "chf-form", nil) // USD 10/mo
	srv := serverByHost(t, st, "chf-form")
	if _, err := database.Exec("UPDATE servers SET initial_price = 40, initial_term = 4, initial_currency = 'CHF' WHERE id = ?", srv.ID); err != nil {
		t.Fatal(err)
	}
	path := "/servers/" + strconv.FormatInt(srv.ID, 10)
	resp, err := client.Get(ts.URL + path + "/edit")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `<option value="CHF" selected>CHF</option>`) {
		t.Fatal("edit form should offer and select the stored CHF")
	}

	// What the browser posts back: CHF selected, hostname changed.
	vals := url.Values{"hostname": {"chf-renamed"}, "server_type": {"1"}, "active": {"on"},
		"price": {"10"}, "currency": {"USD"}, "term": {"1"},
		"initial_price": {"40"}, "initial_term": {"4"}, "initial_currency": {"CHF"}}
	resp = postForm(t, client, ts, path+"/update", vals)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if got := serverByHost(t, st, "chf-renamed"); got.InitialCurrency.String != "CHF" || got.InitialPrice.Float64 != 40 {
		t.Fatalf("CHF initial price relabelled: %+v", got)
	}

	// Another unlisted code is not accepted as new input: it falls back to
	// "same as price".
	vals.Set("initial_currency", "SEK")
	resp = postForm(t, client, ts, path+"/update", vals)
	resp.Body.Close()
	if got := serverByHost(t, st, "chf-renamed"); got.InitialCurrency.String != "USD" {
		t.Fatalf("unlisted new currency should not be stored: %+v", got)
	}

	// Updating a server that doesn't exist is a 404.
	resp = postForm(t, client, ts, "/servers/999/update", vals)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing server update: %d", resp.StatusCode)
	}
}

func TestAPIServerInitialTermAndBadJSON(t *testing.T) {
	bad, good := 9, 4
	j := &serverJSON{Hostname: "t-01", InitialTerm: &bad}
	if _, _, _, errs := j.toModel(); errs["initial_term"] == "" {
		t.Fatal("initial_term 9 must be rejected")
	}
	j = &serverJSON{Hostname: "t-02", InitialTerm: &good}
	if srv, _, _, errs := j.toModel(); len(errs) != 0 || srv.InitialTerm.Int64 != 4 {
		t.Fatalf("initial_term 4 should map: %v", errs)
	}

	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	req, _ := http.NewRequest("POST", ts.URL+"/api/servers", strings.NewReader(`{"hostname": "x", "initial_price": "five"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mistyped JSON: %d", resp.StatusCode)
	}
}

func TestServerFormInitialPriceInputs(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	st := &model.ServerStore{DB: database}

	// A typed 0 is the same as ticking Free: no term or currency kept.
	createFullServer(t, client, ts, "zero-01", url.Values{"initial_price": {"0"}, "initial_term": {"4"}, "initial_currency": {"EUR"}})
	if got := serverByHost(t, st, "zero-01"); !got.InitialPrice.Valid || got.InitialPrice.Float64 != 0 ||
		got.InitialTerm.Valid || got.InitialCurrency.Valid {
		t.Fatalf("typed 0 should be free: %+v", got)
	}

	// Not a number: inline error, nothing saved.
	vals := url.Values{"hostname": {"nan-01"}, "server_type": {"1"}, "initial_price": {"abc"}}
	resp := postForm(t, client, ts, "/servers", vals)
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther || !strings.Contains(body, "Initial price must be a number") {
		t.Fatalf("non-numeric initial price should fail validation (status %d)", resp.StatusCode)
	}
}
