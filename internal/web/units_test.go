package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestFmtMB(t *testing.T) {
	cases := []struct {
		mb   int64
		want string
	}{
		{0, "0 MB"},
		{512, "512 MB"},
		{1023, "1023 MB"},
		{1024, "1 GB"},
		{1536, "1.5 GB"},
		{16384, "16 GB"},
		{1024 * 1024, "1 TB"},
		{2150400, "2.05 TB"},
		{2 * 1024 * 1024, "2 TB"},
	}
	for _, c := range cases {
		if got := fmtMB(c.mb); got != c.want {
			t.Errorf("fmtMB(%d) = %q, want %q", c.mb, got, c.want)
		}
	}
}

func TestDecomposeMB(t *testing.T) {
	cases := []struct {
		mb    int64
		value string
		unit  string
	}{
		{2048, "2", "GB"},
		{1536, "1536", "MB"}, // 1.5 GB isn't whole → stay MB
		{2 * 1024 * 1024, "2", "TB"},
		{512, "512", "MB"},
		{0, "", "MB"},
	}
	for _, c := range cases {
		v, u := decomposeMB(c.mb)
		if v != c.value || u != c.unit {
			t.Errorf("decomposeMB(%d) = (%q, %q), want (%q, %q)", c.mb, v, u, c.value, c.unit)
		}
	}
}

// formReq builds a fake form request.
func formReq(vals url.Values) *http.Request {
	r, _ := http.NewRequest("POST", "/", strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	return r
}

func TestSizeFormValueUnits(t *testing.T) {
	cases := []struct {
		val  string
		unit string
		want int64
	}{
		{"2", "GB", 2048},
		{"1.5", "TB", 1572864},
		{"512", "MB", 512},
		{"2", "", 2}, // missing unit → MB
		{"0.5", "GB", 512},
	}
	for _, c := range cases {
		errs := map[string]string{}
		r := formReq(url.Values{"size": {c.val}, "size_unit": {c.unit}})
		got := sizeFormValue(r, errs, "size", 1<<30)
		if len(errs) > 0 {
			t.Fatalf("%s %s: unexpected error %v", c.val, c.unit, errs)
		}
		if !got.Valid || got.Int64 != c.want {
			t.Errorf("%s %s → %v, want %d MB", c.val, c.unit, got, c.want)
		}
	}

	// Invalid input errors.
	errs := map[string]string{}
	r := formReq(url.Values{"size": {"abc"}})
	if sizeFormValue(r, errs, "size", 1<<30).Valid || len(errs) == 0 {
		t.Fatal("expected validation error on garbage")
	}
}

func TestBandwidthUnlimited(t *testing.T) {
	errs := map[string]string{}
	r := formReq(url.Values{
		"bandwidth_as_mb":           {"5"},
		"bandwidth_as_mb_unit":      {"TB"},
		"bandwidth_as_mb_unlimited": {"on"},
	})
	got := bandwidthFormValue(r, errs, "bandwidth_as_mb", 1<<30)
	if got.Valid {
		t.Fatal("unlimited checkbox should yield NULL, ignoring number+unit")
	}
}

// TestServerFormUnitConversion posts the real server form with GB/TB units
// and checks the stored MB + unlimited NULL.
func TestServerFormUnitConversion(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, "/servers", url.Values{
		"hostname":             {"unit-srv"},
		"server_type":          {"1"},
		"active":               {"on"},
		"ram_as_mb":            {"2"},
		"ram_as_mb_unit":       {"GB"},
		"bandwidth_as_mb":      {"20"},
		"bandwidth_as_mb_unit": {"TB"},
		"disk1_size":           {"1.5"},
		"disk1_size_unit":      {"TB"},
		"disk1_media":          {"NVMe"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: expected 303, got %d", resp.StatusCode)
	}

	var ram, bw, disk int64
	database.QueryRow("SELECT ram_as_mb, bandwidth_as_mb FROM servers WHERE hostname = 'unit-srv'").Scan(&ram, &bw)
	database.QueryRow("SELECT size_as_mb FROM server_disks").Scan(&disk)
	if ram != 2048 || bw != 20*1024*1024 || disk != 1536*1024 {
		t.Fatalf("unit conversion wrong: ram=%d bw=%d disk=%d", ram, bw, disk)
	}

	// Unlimited checkbox → NULL bandwidth.
	resp = postForm(t, client, ts, "/servers", url.Values{
		"hostname":                  {"unlim-srv"},
		"server_type":               {"1"},
		"active":                    {"on"},
		"bandwidth_as_mb":           {"50"},
		"bandwidth_as_mb_unlimited": {"on"},
	})
	resp.Body.Close()
	var bwNull any
	database.QueryRow("SELECT bandwidth_as_mb FROM servers WHERE hostname = 'unlim-srv'").Scan(&bwNull)
	if bwNull != nil {
		t.Fatalf("expected NULL bandwidth, got %v", bwNull)
	}

	// Edit form pre-fill decomposes 2048 MB → "2" + GB.
	resp, err := client.Get(ts.URL + "/servers/1/edit")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `name="ram_as_mb" min="0" step="any" value="2"`) {
		t.Fatal("ram pre-fill should decompose to 2")
	}
	if !strings.Contains(body, `<option value="GB" selected>GB</option>`) {
		t.Fatal("ram unit should pre-select GB")
	}
	// Display: 2048 MB shows as "2 GB", unlimited shows ∞.
	resp, err = client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "2 GB") || !strings.Contains(body, "∞") {
		t.Fatal("list should show 2 GB and ∞")
	}
}
