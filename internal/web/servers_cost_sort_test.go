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
			costItem(5, "once-100", &model.Pricing{Currency: "USD", Price: 100, Term: 99}),
			costItem(6, "once-eur-20", &model.Pricing{Currency: "EUR", Price: 20, Term: 99}), // $40
			costItem(7, "chf-1", &model.Pricing{Currency: "CHF", Price: 1, Term: model.TermMonthly}),
		}
	}
	items := build()
	sortByCost(items, rates, "asc", pricingCostKey)
	want := []string{"yearly-60", "eur-3", "monthly-10", "once-eur-20", "once-100", "none", "chf-1"}
	if got := hosts(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("asc:\n got %v\nwant %v", got, want)
	}
	items = build()
	sortByCost(items, rates, "desc", pricingCostKey)
	want = []string{"monthly-10", "eur-3", "yearly-60", "once-100", "once-eur-20", "none", "chf-1"}
	if got := hosts(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("desc:\n got %v\nwant %v", got, want)
	}

	// Without rates (not fetched yet) USD rows still order; others go last.
	items = build()
	sortByCost(items, nil, "asc", pricingCostKey)
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
		withInitial(5, "once-50", 50, 99, "USD"),
		withInitial(6, "defaults-7", 7, 0, ""), // shown as $7.00/mo
	}
	sortByCost(items, rates, "asc", initialCostKey)
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
		return []int{strings.Index(body, "pc-yearly"), strings.Index(body, "pc-monthly"), strings.Index(body, "pc-none")}
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
