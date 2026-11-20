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

package model

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestLabelsAssignMaxFour(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	cat := &CatalogStore{DB: database}
	st := &LabelStore{DB: database}

	// Attach a server to assign labels to.
	srv := &ServerStore{DB: database}
	srvID, err := srv.Create(ctx, &Server{Hostname: "labeled-01", ServerType: TypeKVM, Active: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var labelIDs []int64
	for _, name := range []string{"production", "backup", "eu", "promo", "spare"} {
		id, _ := cat.Create(ctx, Catalogs["labels"], name)
		labelIDs = append(labelIDs, id)
	}

	// First four succeed.
	for _, id := range labelIDs[:4] {
		if err := st.Assign(ctx, id, srvID, ServiceServer); err != nil {
			t.Fatalf("Assign: %v", err)
		}
	}
	// Fifth is refused.
	if err := st.Assign(ctx, labelIDs[4], srvID, ServiceServer); !errors.Is(err, ErrTooManyLabels) {
		t.Fatalf("expected ErrTooManyLabels, got %v", err)
	}
	// Re-assigning an existing one is a no-op, not an error.
	if err := st.Assign(ctx, labelIDs[0], srvID, ServiceServer); err != nil {
		t.Fatalf("re-assign: %v", err)
	}

	assigned, err := st.ListFor(ctx, srvID, ServiceServer)
	if err != nil || len(assigned) != 4 {
		t.Fatalf("ListFor: %v %d", err, len(assigned))
	}

	// Unassign frees a slot.
	if err := st.Unassign(ctx, labelIDs[0], srvID, ServiceServer); err != nil {
		t.Fatal(err)
	}
	if err := st.Assign(ctx, labelIDs[4], srvID, ServiceServer); err != nil {
		t.Fatalf("assign after unassign: %v", err)
	}

	// Counts reflect assignments.
	counts, _ := st.AllWithCounts(ctx)
	total := 0
	for _, c := range counts {
		total += c.Used
	}
	if total != 4 {
		t.Fatalf("expected 4 assignments, got %d", total)
	}

	// FindOrCreate is idempotent.
	id1, _ := st.FindOrCreate(ctx, "newlabel")
	id2, _ := st.FindOrCreate(ctx, "newlabel")
	if id1 != id2 {
		t.Fatal("FindOrCreate not idempotent")
	}
}

func TestNotesCRUD(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	srv := &ServerStore{DB: database}
	srvID, _ := srv.Create(ctx, &Server{Hostname: "noted-01", ServerType: TypeKVM, Active: true}, nil, nil)

	st := &NoteStore{DB: database}
	id, err := st.Create(ctx, &Note{
		ServiceID:   sql64(srvID),
		ServiceType: sql64(ServiceServer),
		Body:        "root password rotated",
	})
	if err != nil {
		t.Fatal(err)
	}

	notes, err := st.ListFor(ctx, srvID, ServiceServer)
	if err != nil || len(notes) != 1 || notes[0].Body != "root password rotated" {
		t.Fatalf("ListFor: %v %+v", err, notes)
	}

	all, err := st.ListAll(ctx)
	if err != nil || len(all) != 1 || all[0].Target != "noted-01" {
		t.Fatalf("ListAll: %v %+v", err, all)
	}

	if err := st.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	notes, _ = st.ListFor(ctx, srvID, ServiceServer)
	if len(notes) != 0 {
		t.Fatal("note not deleted")
	}
}

func TestIPsCRUD(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	srv := &ServerStore{DB: database}
	srvID, _ := srv.Create(ctx, &Server{Hostname: "ip-01", ServerType: TypeKVM, Active: true}, nil, nil)

	st := &IPStore{DB: database}
	id, err := st.Create(ctx, &IP{ServiceID: srvID, ServiceType: ServiceServer, Address: "203.0.113.10", IsIPv4: true})
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate (service, address) fails.
	if _, err := st.Create(ctx, &IP{ServiceID: srvID, ServiceType: ServiceServer, Address: "203.0.113.10", IsIPv4: true}); err == nil {
		t.Fatal("expected unique violation")
	}

	if err := st.UpdateWhois(ctx, id, &WhoisData{
		Country: "Germany", Org: "Hetzner", Asn: "24940", FetchedAt: "2026-07-25T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	ip, err := st.Get(ctx, id)
	if err != nil || ip.Country.String != "Germany" || !ip.IsIPv4 {
		t.Fatalf("Get: %v %+v", err, ip)
	}

	all, err := st.ListAll(ctx)
	if err != nil || len(all) != 1 || all[0].Target != "ip-01" {
		t.Fatalf("ListAll: %v %+v", err, all)
	}

	if err := st.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestDNSCRUDAndSetNull(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	srv := &ServerStore{DB: database}
	srvID, _ := srv.Create(ctx, &Server{Hostname: "dns-01", ServerType: TypeKVM, Active: true}, nil, nil)

	st := &DNSStore{DB: database}
	id, err := st.Create(ctx, &DNSRecord{
		Hostname: "www.example.com", DNSType: "A", Address: "203.0.113.5",
		ServerID: sql64(srvID),
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := st.Get(ctx, id)
	if err != nil || rec.ServerID.Int64 != srvID {
		t.Fatalf("Get: %v %+v", err, rec)
	}

	// Server delete nulls the link (ON DELETE SET NULL).
	if err := srv.Delete(ctx, srvID); err != nil {
		t.Fatal(err)
	}
	rec, err = st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ServerID.Valid {
		t.Fatal("expected server_id to be nulled after server delete")
	}

	// Update + list + delete.
	rec.Hostname = "mail.example.com"
	rec.DNSType = "MX"
	if err := st.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}
	items, err := st.List(ctx)
	if err != nil || len(items) != 1 || items[0].Hostname != "mail.example.com" {
		t.Fatalf("List: %v %+v", err, items)
	}
	if err := st.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func sql64(i int64) sql.NullInt64 { return sql.NullInt64{Int64: i, Valid: true} }

// TestDeleteRemovesPolymorphicChildren verifies pricing/ip/note/label rows
// die with their service.
func TestDeleteRemovesPolymorphicChildren(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	srv := &ServerStore{DB: database}
	id, err := srv.Create(ctx, &Server{Hostname: "doomed-01", ServerType: TypeKVM, Active: true},
		nil, &Pricing{Currency: "USD", Price: 10, Term: TermMonthly})
	if err != nil {
		t.Fatal(err)
	}
	ips := &IPStore{DB: database}
	ips.Create(ctx, &IP{ServiceID: id, ServiceType: ServiceServer, Address: "203.0.113.50", IsIPv4: true})
	notes := &NoteStore{DB: database}
	notes.Create(ctx, &Note{ServiceID: sql64(id), ServiceType: sql64(ServiceServer), Body: "note"})
	labels := &LabelStore{DB: database}
	labelID, _ := labels.FindOrCreate(ctx, "doomed")
	labels.Assign(ctx, labelID, id, ServiceServer)

	if err := srv.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	count := func(q string) int {
		var n int
		database.QueryRow(q, id).Scan(&n)
		return n
	}
	if n := count("SELECT COUNT(*) FROM pricings WHERE service_id = ? AND service_type = 1"); n != 0 {
		t.Fatalf("pricing orphaned: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM ips WHERE service_id = ? AND service_type = 1"); n != 0 {
		t.Fatalf("ip orphaned: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM notes WHERE service_id = ? AND service_type = 1"); n != 0 {
		t.Fatalf("note orphaned: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM labels_assigned WHERE service_id = ? AND service_type = 1"); n != 0 {
		t.Fatalf("label assignment orphaned: %d", n)
	}
}

// TestDeleteHostingRemovesChildren covers the shared hostingStore path too.
func TestDeleteHostingRemovesChildren(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	st := &SharedStore{DB: database}
	id, err := st.Create(ctx, &SharedHosting{MainDomain: "doomed.example.com", Active: true},
		&Pricing{Currency: "USD", Price: 5, Term: TermMonthly})
	if err != nil {
		t.Fatal(err)
	}
	notes := &NoteStore{DB: database}
	notes.Create(ctx, &Note{ServiceID: sql64(id), ServiceType: sql64(ServiceShared), Body: "note"})

	if err := st.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	var n int
	database.QueryRow("SELECT COUNT(*) FROM pricings WHERE service_id = ? AND service_type = 2", id).Scan(&n)
	if n != 0 {
		t.Fatalf("pricing orphaned: %d", n)
	}
	database.QueryRow("SELECT COUNT(*) FROM notes WHERE service_id = ? AND service_type = 2", id).Scan(&n)
	if n != 0 {
		t.Fatalf("note orphaned: %d", n)
	}
}

// TestYABSDeleteRecomputesHasYabs: deleting the last run clears the flag,
// deleting one of several keeps it.
func TestYABSDeleteRecomputesHasYabs(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	srv := &ServerStore{DB: database}
	id, err := srv.Create(ctx, &Server{Hostname: "y-01", ServerType: TypeKVM, Active: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := &YABSStore{DB: database}
	run := func() int64 {
		rid, err := st.Create(ctx, &YABS{ServerID: id, RunAt: sql.NullString{String: "2026-01-01", Valid: true}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}
	r1 := run()
	r2 := run()

	hasYabs := func() int {
		var n int
		database.QueryRow("SELECT has_yabs FROM servers WHERE id = ?", id).Scan(&n)
		return n
	}
	if hasYabs() != 1 {
		t.Fatal("has_yabs should be set after ingest")
	}
	if err := st.Delete(ctx, id, r1); err != nil {
		t.Fatal(err)
	}
	if hasYabs() != 1 {
		t.Fatal("has_yabs should stay set with one run left")
	}
	if err := st.Delete(ctx, id, r2); err != nil {
		t.Fatal(err)
	}
	if hasYabs() != 0 {
		t.Fatal("has_yabs should clear after the last run is deleted")
	}
}

// A run's raw payload is stored with it, read back scoped to its server, and
// its unmeasured speeds come back as 0 from NULL columns.
func TestYABSPayloadAndUnmeasuredSpeeds(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	srv := &ServerStore{DB: database}
	a, err := srv.Create(ctx, &Server{Hostname: "p-01", ServerType: TypeKVM, Active: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := srv.Create(ctx, &Server{Hostname: "p-02", ServerType: TypeKVM, Active: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := &YABSStore{DB: database}

	withPayload, err := st.CreateWithPayload(ctx, &YABS{ServerID: a},
		[]YABSDiskSpeed{{BlockSize: "4k", ReadMbps: 99.5}},
		[]YABSNetworkSpeed{{Location: "busy", LatencyMs: 190}, {Location: "ok", SendMbps: 683, RecvMbps: 343, LatencyMs: 12}},
		`{"raw":true}`)
	if err != nil {
		t.Fatal(err)
	}
	without, err := st.Create(ctx, &YABS{ServerID: a}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if body, err := st.Payload(ctx, a, withPayload); err != nil || body != `{"raw":true}` {
		t.Fatalf("Payload = %q, %v", body, err)
	}
	if _, err := st.Payload(ctx, b, withPayload); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Payload through another server: %v, want sql.ErrNoRows", err)
	}
	if _, err := st.Payload(ctx, a, without); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Payload of a run without one: %v, want sql.ErrNoRows", err)
	}
	for id, want := range map[int64]bool{withPayload: true, without: false} {
		if got, err := st.HasPayload(ctx, id); err != nil || got != want {
			t.Errorf("HasPayload(%d) = %v, %v, want %v", id, got, err, want)
		}
	}

	_, disks, network, err := st.Get(ctx, withPayload)
	if err != nil {
		t.Fatalf("Get a run with NULL speeds: %v", err)
	}
	if len(disks) != 1 || disks[0].ReadMbps != 99.5 || disks[0].WriteMbps != 0 {
		t.Errorf("disks = %+v", disks)
	}
	if len(network) != 2 || network[0].SendMbps != 0 || network[0].LatencyMs != 190 || network[1].SendMbps != 683 {
		t.Errorf("network = %+v", network)
	}
	var nulls int
	database.QueryRow(`SELECT
		(SELECT COUNT(*) FROM yabs_network_speed WHERE send_mbps IS NULL AND recv_mbps IS NULL) +
		(SELECT COUNT(*) FROM yabs_disk_speed WHERE write_mbps IS NULL)`).Scan(&nulls)
	if nulls != 2 {
		t.Errorf("unmeasured values stored as NULL = %d, want 2", nulls)
	}

	if err := st.Delete(ctx, a, withPayload); err != nil {
		t.Fatal(err)
	}
	var left int
	database.QueryRow("SELECT COUNT(*) FROM yabs_payload").Scan(&left)
	if left != 0 {
		t.Errorf("payload rows after Delete = %d, want 0", left)
	}
}
