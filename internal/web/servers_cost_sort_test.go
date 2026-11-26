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
	"database/sql"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

func costItem(id int64, host string, p *model.Pricing) model.ServerListItem {
	it := model.ServerListItem{Pricing: p}
	it.ID, it.Hostname = id, host
	return it
}

func priceKey(rates map[string]float64) func(*model.ServerListItem) costKey {
	return func(it *model.ServerListItem) costKey { return pricingCostKey(it.Pricing, rates) }
}

func hosts(items []model.ServerListItem) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = items[i].Hostname
	}
	return out
}

// The Price sort compares real cost: per-month USD across terms and
// currencies, one-time prices after recurring ones, unplaceable rows last.
func TestSortByCostPrice(t *testing.T) {
	rates := map[string]float64{"EUR": 0.5} // 1 USD = 0.5 EUR
	build := func() []model.ServerListItem {
		return []model.ServerListItem{
			costItem(1, "none", nil),
			costItem(2, "yearly-60", &model.Pricing{Currency: "USD", Price: 60, Term: model.TermAnnual}), // $5/mo
			costItem(3, "monthly-10", &model.Pricing{Currency: "USD", Price: 10, Term: model.TermMonthly}),
			costItem(4, "eur-3", &model.Pricing{Currency: "EUR", Price: 3, Term: model.TermMonthly}), // $6/mo
			costItem(5, "once-100", &model.Pricing{Currency: "USD", Price: 100, Term: model.TermOneTime}),
			costItem(6, "once-eur-20", &model.Pricing{Currency: "EUR", Price: 20, Term: model.TermOneTime}), // $40
			costItem(7, "chf-1", &model.Pricing{Currency: "CHF", Price: 1, Term: model.TermMonthly}),
		}
	}
	items := build()
	sortByCost(items, "asc", priceKey(rates))
	want := []string{"yearly-60", "eur-3", "monthly-10", "once-eur-20", "once-100", "none", "chf-1"}
	if got := hosts(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("asc:\n got %v\nwant %v", got, want)
	}
	items = build()
	sortByCost(items, "desc", priceKey(rates))
	want = []string{"monthly-10", "eur-3", "yearly-60", "once-100", "once-eur-20", "none", "chf-1"}
	if got := hosts(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("desc:\n got %v\nwant %v", got, want)
	}

	// Without rates (not fetched yet) USD rows still order; others go last.
	items = build()
	sortByCost(items, "asc", priceKey(nil))
	want = []string{"yearly-60", "monthly-10", "once-100", "none", "eur-3", "once-eur-20", "chf-1"}
	if got := hosts(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("nil rates:\n got %v\nwant %v", got, want)
	}
}

// Initial Price sorts by its own term and currency; Free counts as $0.
func TestSortByCostInitialPrice(t *testing.T) {
	rates := map[string]float64{"EUR": 0.5}
	withInitial := func(id int64, host string, price float64, term int, cur string) model.ServerListItem {
		it := costItem(id, host, &model.Pricing{Currency: "USD", Price: 1, Term: model.TermMonthly})
		it.InitialPrice = sql.NullFloat64{Float64: price, Valid: true}
		if term != 0 {
			it.InitialTerm = sql.NullInt64{Int64: int64(term), Valid: true}
		}
		if cur != "" {
			it.InitialCurrency = sql.NullString{String: cur, Valid: true}
		}
		return it
	}
	items := []model.ServerListItem{
		costItem(1, "unset", nil),
		withInitial(2, "monthly-10", 10, model.TermMonthly, "USD"),
		withInitial(3, "free", 0, 0, ""),
		withInitial(4, "yearly-eur-30", 30, model.TermAnnual, "EUR"), // $60/yr = $5/mo
		withInitial(5, "once-50", 50, model.TermOneTime, "USD"),
		withInitial(6, "defaults-7", 7, 0, ""), // shown as $7.00/mo
	}
	sortByCost(items, "asc", func(it *model.ServerListItem) costKey { return initialCostKey(it, rates) })
	want := []string{"free", "yearly-eur-30", "defaults-7", "monthly-10", "once-50", "unset"}
	if got := hosts(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("asc:\n got %v\nwant %v", got, want)
	}
}

// End to end: the list's Price header sorts $60/yr below $10/mo.
func TestServerPriceSortByCost(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createFullServer(t, client, ts, "pc-monthly", nil) // $10/mo
	createFullServer(t, client, ts, "pc-yearly", url.Values{"price": {"60"}, "term": {"4"}})
	createFullServer(t, client, ts, "pc-none", url.Values{"price": {""}})
	// Consume the "Server pc-none added." flash so it can't skew the order.
	resp, err := client.Get(ts.URL + "/servers")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	order := func(sortKey, dir string) []int {
		resp, err := client.Get(ts.URL + "/servers?sort=" + sortKey + "&dir=" + dir)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sort=%s&dir=%s: status %d", sortKey, dir, resp.StatusCode)
		}
		pos := []int{strings.Index(body, "pc-yearly"), strings.Index(body, "pc-monthly"), strings.Index(body, "pc-none")}
		for i, p := range pos {
			if p < 0 {
				t.Fatalf("sort=%s&dir=%s: row %d missing from the list", sortKey, dir, i)
			}
		}
		return pos
	}
	for _, key := range []string{"price", "initial_price"} {
		if o := order(key, "asc"); !(o[0] < o[1] && o[1] < o[2]) {
			t.Fatalf("%s asc: want yearly, monthly, none; got %v", key, o)
		}
		if o := order(key, "desc"); !(o[1] < o[0] && o[0] < o[2]) {
			t.Fatalf("%s desc: want monthly, yearly, none; got %v", key, o)
		}
	}
}

// Every section list's Price header sorts by real cost too: $60/yr ($5/mo)
// below $10/mo, either way round, with an unpriced item last.
func TestSectionPriceSortByCost(t *testing.T) {
	cases := []struct {
		base, nameField string
		extra           url.Values
	}{
		{"/shared", "main_domain", url.Values{"svc_type": {"cPanel"}}},
		{"/reseller", "main_domain", url.Values{"svc_type": {"WHM"}}},
		{"/seedboxes", "hostname", url.Values{"seed_box_type": {"ruTorrent"}}},
		{"/domains", "domain", url.Values{"extension": {".com"}}},
		{"/misc", "name", nil},
	}
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	for _, c := range cases {
		create := func(name, price, term string) {
			t.Helper()
			vals := url.Values{c.nameField: {name}, "active": {"on"}, "price": {price}, "currency": {"USD"}, "term": {term}}
			for k, v := range c.extra {
				vals[k] = v
			}
			resp := postForm(t, client, ts, c.base, vals)
			resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("%s: create %s: status %d", c.base, name, resp.StatusCode)
			}
		}
		prefix := strings.Trim(c.base, "/")
		yearly, monthly, none := prefix+"-yearly.example", prefix+"-monthly.example", prefix+"-none.example"
		create(yearly, "60", "4")
		create(monthly, "10", "1")
		create(none, "", "1")
		// Consume the last create's flash so it can't skew the order.
		resp, err := client.Get(ts.URL + c.base)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		for _, dir := range []string{"asc", "desc"} {
			resp, err := client.Get(ts.URL + c.base + "?sort=price&dir=" + dir)
			if err != nil {
				t.Fatal(err)
			}
			body := readBody(t, resp)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status %d", c.base, dir, resp.StatusCode)
			}
			y, m, n := strings.Index(body, yearly), strings.Index(body, monthly), strings.Index(body, none)
			if y < 0 || m < 0 || n < 0 {
				t.Fatalf("%s %s: rows missing (yearly=%d monthly=%d none=%d)", c.base, dir, y, m, n)
			}
			if dir == "asc" && !(y < m && m < n) || dir == "desc" && !(m < y && y < n) {
				t.Fatalf("%s %s: wrong order (yearly=%d monthly=%d none=%d)", c.base, dir, y, m, n)
			}
		}
	}
}
