# PulsedMedia Provider Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a PulsedMedia provider to idlerthing whose API key drives a cached customer-API sync and a fleet view, without ever dialing a seedbox or exposing the key.

**Architecture:** A Provider catalog row holds the key; a new `internal/pulsedmedia` package fetches and parses the feed (ported from pmss-fleet-manager's `internal/pmapi`); a `pm_services` cache table holds the feed per provider; a sync reconciles durable fields into `pricings`/`seedboxes` with a three-way merge against the previous feed value; a new `/seedboxes/fleet` route renders the cached live figures. No SSH, no inbound listener.

**Tech Stack:** Go 1.26 stdlib + `modernc.org/sqlite` + `html/template`; hand-rolled `PRAGMA user_version` migrations; `go test ./...`.

**Spec:** `docs/superpowers/specs/2026-10-18-pulsedmedia-provider-design.md`

## Global Constraints

- No new module dependencies. `net/http`, `encoding/json`, `database/sql` only.
- Never hold the SQLite writer connection across a network call: fetch first, then open the transaction (the pool is `SetMaxOpenConns(1)`).
- The PMAPI key must never appear in: `/export/json`, the CSV zip, `/api/providers`, any HTML attribute, any log line, or an import.
- `providers` stays name-only in the generic path: `model.CatalogItem{ID, Name}` must not gain the key field.
- A service is active iff `strings.EqualFold(status, "active")`; everything else is hidden by default.
- Inventory stores whole MB (1024-based); the feed sends bytes.
- Every first-party file carries the AGPL header (CI: `make license-check`).
- Ported code keeps its AGPL header and must pass `make notices-check`.

## Review Focus

- A 401/403 from the feed must surface as an auth error on the fleet view, not wipe the cached rows.
- A feed response that is HTML (wrong URL, captive portal) must not panic the parser.
- A `billing_cycle` idlerthing does not understand must leave `price` and `term` unwritten rather than write a price that reports a wrong monthly cost.
- `next_due` must advance on the second sync (the whole point of the three-way merge).
- A human-edited `next_due`/`price` must survive repeated syncs.
- The key must not appear in a full export even after a real sync.

---

### Task 1: Migration and the `pm_services` cache

**Files:**
- Create: `internal/db/migrations/0014_pulsedmedia.sql`
- Create: `internal/model/pm.go`
- Create: `internal/model/pm_test.go`
- Modify: `internal/db/migrate_test.go:510` (expected max version constant)

**Interfaces:**
- Consumes: `db.Migrate(*sql.DB) ([]int, error)`, `model.QuerierFrom(ctx, db)`.
- Produces:
  - `type PMService struct { ProviderID, ServiceID int64; Product, Group, Domain, Status, Registered, NextDue, BillingCycle, Currency, Username, LoginURL, PermanentAddress, TrackedSince, CPUSince, SyncedAt string; RecurringAmount float64; SentBytes, ReceivedBytes, CPUHours, DiskUsedBytes, DiskQuotaBytes, Torrents int64 }`
  - `type PMStore struct { DB *sql.DB }`
  - `func (s *PMStore) Replace(ctx context.Context, providerID int64, services []PMService) error`
  - `func (s *PMStore) List(ctx context.Context, providerID int64) ([]PMService, error)`

- [ ] **Step 1: Write the failing test**

In `internal/model/pm_test.go`, build an in-memory DB via the existing test helper used by `model_test.go`, run `db.Migrate`, then:

```go
func TestPMStoreReplaceIsWholesale(t *testing.T) {
    // Replace with two services, assert List returns both ordered by ServiceID.
    // Replace with one service, assert the other is gone.
}
func TestPMStoreReplaceIsPerProvider(t *testing.T) {
    // Replace provider 1 with one service; Replace provider 2 with one service;
    // List(1) must still return exactly one.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/model/ -run TestPMStore -v`
Expected: FAIL — `undefined: PMStore`.

- [ ] **Step 3: Write the migration**

`internal/db/migrations/0014_pulsedmedia.sql`:

```sql
ALTER TABLE providers ADD COLUMN pm_api_key TEXT NOT NULL DEFAULT '';
ALTER TABLE seedboxes ADD COLUMN pm_service_id INTEGER;
CREATE TABLE pm_services (
  provider_id INTEGER NOT NULL,
  service_id INTEGER NOT NULL,
  product TEXT NOT NULL DEFAULT '', grp TEXT NOT NULL DEFAULT '',
  domain TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
  registered TEXT NOT NULL DEFAULT '', next_due TEXT NOT NULL DEFAULT '',
  billing_cycle TEXT NOT NULL DEFAULT '',
  recurring_amount REAL NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '', login_url TEXT NOT NULL DEFAULT '',
  permanent_address TEXT NOT NULL DEFAULT '',
  tracked_since TEXT NOT NULL DEFAULT '',
  sent_bytes INTEGER NOT NULL DEFAULT 0,
  received_bytes INTEGER NOT NULL DEFAULT 0,
  cpu_hours INTEGER NOT NULL DEFAULT 0,
  cpu_since TEXT NOT NULL DEFAULT '',
  disk_used_bytes INTEGER NOT NULL DEFAULT 0,
  disk_quota_bytes INTEGER NOT NULL DEFAULT 0,
  torrents INTEGER NOT NULL DEFAULT 0,
  generated_at TEXT NOT NULL DEFAULT '',
  synced_at TEXT NOT NULL,
  PRIMARY KEY (provider_id, service_id)
);
```

- [ ] **Step 4: Implement `internal/model/pm.go`**

`Replace` runs `DELETE FROM pm_services WHERE provider_id = ?` then one prepared `INSERT` per service inside a single `tx`; `SyncedAt` is set by the caller. `List` selects the columns in the struct's field order, `ORDER BY service_id`.

- [ ] **Step 5: Update the expected max migration version**

`internal/db/migrate_test.go` derives `wantVersion` from the embedded set; if it is hardcoded at line 510, bump it to 14.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/model/ ./internal/db/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/db/migrations/0014_pulsedmedia.sql internal/model/pm.go internal/model/pm_test.go internal/db/migrate_test.go
git commit -m "feat(db): add pm_services cache and provider key column"
```

---

### Task 2: The feed client

**Files:**
- Create: `internal/pulsedmedia/feed.go`
- Create: `internal/pulsedmedia/feed_test.go`
- Create: `internal/pulsedmedia/testdata/feed.json`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `const FeedURL = "https://pulsedmedia.com/remote/pmapi-feed.php"`
  - `type Feed struct { ClientID int64; GeneratedAt, Currency string; Services []Service }`
  - `type Service struct { ID int64; Product, Group, Domain, Status, Registered, NextDue, BillingCycle string; RecurringAmount Amount; Seedbox *Access }`
  - `type Access struct { Username, LoginURL, PermanentAddress string; Usage *Usage }`
  - `type Usage struct { TrackedSince string; SentBytes, ReceivedBytes, CPUHours, DiskUsedBytes, DiskQuotaBytes int64; CPUSince string; Torrents int }`
  - `type Amount float64` with tolerant `UnmarshalJSON`
  - `func Parse(body []byte) (*Feed, error)`
  - `func Fetch(ctx context.Context, url, key string) (*Feed, error)` (`url == ""` means `FeedURL`; `IDLER_PMAPI_URL` overrides when the caller passes it)
  - `func MonthsPerCycle(cycle string) int` and `func (s Service) Monthly() float64`
  - `func TermForCycle(cycle string) int` returning `model.TermMonthly|Quarterly|Semiannual|Annual|Biennial|Triennial`, `0` for anything else
  - `func IsActive(status string) bool`
  - `var ErrAuth = errors.New("API key rejected")`

- [ ] **Step 1: Write the failing tests**

`internal/pulsedmedia/feed_test.go`, using `testdata/feed.json` copied from a real response (start from pmss-fleet-manager's `internal/pmapi/pmapi_test.go` fixtures):

```go
func TestParseRealFeed(t *testing.T)      // two services, usage block decoded, Amount parsed from a string
func TestParseHTMLBody(t *testing.T)      // []byte("<!doctype html>") -> error, no panic
func TestAmountParsing(t *testing.T)      // "11.94", "€11.94", "1,234.56", "11,94", 12.5, and "" -> 0
func TestTermForCycle(t *testing.T)       // monthly->1, annually->4, "Biennially"->5, "weird"->0
func TestIsActive(t *testing.T)           // "active", "Active", "Active (Paid)"->false, "Cancelled"->false
func TestFetchRefusesRedirect(t *testing.T)  // httptest server 302 -> error mentioning the target host, key not sent
func TestFetchAuthError(t *testing.T)     // 401 -> errors.Is(err, ErrAuth)
func TestFetchBodyCap(t *testing.T)       // >4MiB response -> error, not OOM
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pulsedmedia/ -v`
Expected: FAIL — package does not exist / undefined symbols.

- [ ] **Step 3: Implement `feed.go`**

Port pmss-fleet-manager's `internal/pmapi/pmapi.go` wholesale: `Amount.UnmarshalJSON`, `parseAmount`, `MonthsPerCycle`, `Monthly`, `Parse`, `Fetch`. Keep `Fetch`'s `CheckRedirect` that refuses a redirect (the key travels in a custom header Go would forward), the 30 s timeout, the 4 MiB `io.LimitReader`, and the 401/403 → `ErrAuth` mapping. Add `TermForCycle` and `IsActive` (one-liners) and an `IDLER_PMAPI_URL` read where the caller resolves the URL.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/pulsedmedia/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pulsedmedia/
git commit -m "feat(pulsedmedia): port the customer-API feed client"
```

---

### Task 3: The provider key, stored and never leaked

**Files:**
- Modify: `internal/model/catalog.go` (add key accessors; do **not** touch `CatalogItem`)
- Modify: `internal/web/catalogs.go` (key field on provider rows)
- Modify: `internal/web/assets/templates/catalogs.html` (key input, `has_key` hint, clear)
- Modify: `internal/web/export.go` (prove nothing leaks; no change expected)
- Modify: `internal/importer/importer.go` (ignore `pm_api_key`)
- Test: `internal/web/security_test.go` (add cases), `internal/model/pm_test.go`

**Interfaces:**
- Consumes: Task 1's schema.
- Produces on `*model.CatalogStore`:
  - `func (s *CatalogStore) ProviderKey(ctx context.Context, id int64) (string, error)`
  - `func (s *CatalogStore) SetProviderKey(ctx context.Context, id int64, key string) error`
  - `func (s *CatalogStore) HasProviderKey(ctx context.Context, id int64) (bool, error)`
  - `func (s *CatalogStore) ProviderIDsWithKey(ctx context.Context) ([]int64, error)`

- [ ] **Step 1: Write the failing tests**

```go
// internal/model/pm_test.go
func TestProviderKeyRoundTrip(t *testing.T)   // set, read, HasProviderKey true, set "" -> false
func TestProviderKeyNotInCatalogItem(t *testing.T) // List() returns names only; reflect over CatalogItem has no key field

// internal/web/security_test.go
func TestExportOmitsProviderKey(t *testing.T) // seed a provider key, GET /export/json, body must not contain the key string
func TestCatalogPageOmitsProviderKey(t *testing.T) // GET /catalogs/providers, body must not contain the key
func TestAPICatalogOmitsProviderKey(t *testing.T)  // GET /api/providers, body must not contain the key
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/model/ ./internal/web/ -run 'ProviderKey|OmitsProviderKey' -v`
Expected: FAIL.

- [ ] **Step 3: Implement the accessors**

`catalog.go`: the four methods operate on `providers` only (return an error for other kinds). `List` stays `SELECT id, name`.

- [ ] **Step 4: Add the key control to the provider catalog page**

`catalogView` gains `ShowKeys bool` (true for `kindStr == "providers"`) and `Keys map[int64]string` where the value is `""` when a key is set and `""` otherwise — **never the key itself**. Add `HasKey map[int64]bool`. In `catalogs.html`, inside `{{if $v.ShowKeys}}`, add a `<form>` per row posting to `/catalogs/providers/{id}/key` with a `type="password"` input (empty placeholder "key set" when `HasKey`), plus a "clear" checkbox. Route: `POST /catalogs/providers/{id}/key` → `handleProviderKey` in `catalogs.go`, which reads `pm_api_key`, and on empty input keeps the existing key unless `clear_key` is set.

- [ ] **Step 5: Ignore the key on import**

In `importer.go`, the catalogs loop imports only `name`; add an explicit comment and, if it copies whole maps, strip `pm_api_key` before `Create`.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/model/ ./internal/web/ ./internal/importer/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/model/catalog.go internal/web/catalogs.go internal/web/assets/templates/catalogs.html internal/importer/importer.go internal/model/pm_test.go internal/web/security_test.go
git commit -m "feat(web): provider PMAPI key with no export or API leak"
```

---

### Task 4: Sync and the three-way precedence merge

**Files:**
- Create: `internal/pulsedmedia/sync.go`
- Create: `internal/pulsedmedia/sync_test.go`

**Interfaces:**
- Consumes: Task 1 `PMStore`, Task 2 `Fetch`/`TermForCycle`/`IsActive`, `model.PricingStore`, `model.SeedboxStore`.
- Produces:
  - `type Result struct { Synced int; Matched int; Filled int; Err error; At time.Time }`
  - `func Sync(ctx context.Context, db *sql.DB, providerID int64, key, url string, now time.Time) (Result, error)`
  - `func effective(current string, previous, next string) string` (exported for the test as `Effective`)

- [ ] **Step 1: Write the failing tests**

```go
func TestEffective(t *testing.T) // table: empty->next; current==previous->next; current!=previous->current
func TestSyncAdvancesRenewal(t *testing.T)
// seed a seedbox + pm_services row with next_due 2026-01-01; sync a mock feed
// saying 2026-02-01; assert pricings.next_due_date == 2026-02-01
func TestSyncKeepsHumanEdit(t *testing.T)
// same, but pricings.next_due_date was set to 2026-03-03 by hand; assert it stays
func TestSyncFillsEmptyPriceAndTerm(t *testing.T)
func TestSyncUnknownCycleWritesNoPrice(t *testing.T) // billing_cycle "weird" -> price/term untouched
func TestSyncQuotaZeroWritesNoDisk(t *testing.T)
func TestSyncAuthErrorKeepsCache(t *testing.T) // 401 -> pm_services rows unchanged, error returned
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pulsedmedia/ -run TestSync -v`
Expected: FAIL — `undefined: Sync`.

- [ ] **Step 3: Implement `Sync`**

Order: (1) `Fetch` — on error return it **before** touching the DB; (2) `PMStore.List` to snapshot the previous values; (3) build `[]model.PMService`; (4) open `db.BeginTx`, `Replace`, then for each linked seedbox apply `Effective` to `next_due_date`, `price`, `currency`, `term`, `disk_as_mb`, and commit. Create the pricing row with `upsertPricingTx` semantics when the seedbox has none (currency from the feed; skip entirely if `term == 0` for a non-zero price). `disk_quota_bytes == 0` writes nothing.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/pulsedmedia/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pulsedmedia/sync.go internal/pulsedmedia/sync_test.go
git commit -m "feat(pulsedmedia): reconcile feed into inventory with manual-edit precedence"
```

---

### Task 5: Matching a seedbox to a feed service

**Files:**
- Modify: `internal/model/seedbox.go` (add `PMServiceID sql.NullInt64` to `Seedbox`, `seedboxColumns`, `scanSeedbox`, `Create`, `Update`)
- Modify: `internal/web/seedboxes.go` (`PMServiceID` in `parseSeedboxForm`, `seedboxFormView.PMServices`)
- Modify: `internal/web/assets/templates/seedbox_form.html` (dropdown, only when the provider has a key)
- Create: `internal/pulsedmedia/match.go`
- Create: `internal/pulsedmedia/match_test.go`

**Interfaces:**
- Consumes: Task 1 `PMStore`, Task 3 `ProviderIDsWithKey`.
- Produces:
  - `func Match(ctx context.Context, db *sql.DB, providerID int64) (int, error)` — sets `pm_service_id` on unlinked seedboxes of that provider.
- Matching order: exact `permanent_address` host or `domain` equal to `hostname` (case-insensitive) → `hostname` contains `username` → provider has exactly one service. Stop at the first hit; never overwrite a non-NULL `pm_service_id`.

- [ ] **Step 1: Write the failing tests**

```go
func TestMatchByDomain(t *testing.T)
func TestMatchByUsernameSubstring(t *testing.T)
func TestMatchSingleServiceAccount(t *testing.T)
func TestMatchDoesNotOverwrite(t *testing.T)   // existing pm_service_id stays
func TestMatchNoGuessOnAmbiguity(t *testing.T) // two services, no domain/username hit -> NULL
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pulsedmedia/ -run TestMatch -v`
Expected: FAIL.

- [ ] **Step 3: Implement `Match`, then thread `PMServiceID` through the model and form**

`Seedbox.Update` must preserve `pm_service_id` when the form omits it (blank select = clear is an explicit choice; use a hidden marker input so "unchanged" is distinguishable). The dropdown lists `pm_services` for the seedbox's provider, with an empty option meaning "not linked".

- [ ] **Step 4: Run tests**

Run: `go test ./internal/model/ ./internal/web/ ./internal/pulsedmedia/ -v`
Expected: PASS. Existing seedbox form tests may need the new field added to their POST bodies.

- [ ] **Step 5: Commit**

```bash
git add internal/model/seedbox.go internal/web/seedboxes.go internal/web/assets/templates/seedbox_form.html internal/pulsedmedia/match.go internal/pulsedmedia/match_test.go
git commit -m "feat(pulsedmedia): link seedboxes to feed services"
```

---

### Task 6: The fleet view

**Files:**
- Create: `internal/web/fleet.go`
- Create: `internal/web/assets/templates/seedbox_fleet.html`
- Modify: `internal/web/server.go` (two routes)
- Modify: `internal/web/routes.go` (add `routeSeedboxFleet = routeSeedboxes + "/fleet"`)
- Modify: `internal/web/seedboxes.go` (link to the fleet view from the section page)
- Test: `internal/web/fleet_test.go`

**Interfaces:**
- Consumes: Task 1 `PMStore`, Task 4 `Sync`, Task 3 `ProviderIDsWithKey`/`ProviderKey`.
- Produces:
  - `GET /seedboxes/fleet` → `handleSeedboxFleet`
  - `POST /seedboxes/fleet/sync` → `handleSeedboxFleetSync` (protected, CSRF, same-origin)
  - `type seedboxFleetView struct { Boxes []fleetBox; Totals fleetTotals; ShowCancelled bool; SyncedAt time.Time; SyncError string; Providers []model.CatalogItem }`

- [ ] **Step 1: Write the failing tests**

```go
func TestFleetHidesCancelledByDefault(t *testing.T) // feed has one active + one cancelled service
func TestFleetShowsCancelledWithParam(t *testing.T) // ?cancelled=1 shows both
func TestFleetExcludesCancelledFromTotals(t *testing.T) // storage/cost tiles in both states
func TestFleetRequiresLogin(t *testing.T)          // GET without session -> redirect to /login
func TestFleetExcludesPMBoxesFromPublic(t *testing.T) // GET /public after the fleet data exists: no PM figure in the body
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/web/ -run TestFleet -v`
Expected: FAIL.

- [ ] **Step 3: Implement the handler and template**

Read `pm_services` for every provider with a key, join to the linked seedboxes; render durable fields from `seedboxes`/`pricings` and live figures from `pm_services`. `ShowCancelled` comes from `r.URL.Query().Get("cancelled") == "1"`. Headline tiles sum only `IsActive` services. A stale `synced_at` (>6h) or a stored sync error renders a banner; the page never fetches synchronously. `POST /seedboxes/fleet/sync` calls `Sync` for each keyed provider and redirects back.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/web/ -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite and the repo gates**

Run: `go test ./... && make vet && make license-check && make notices-check`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/web/fleet.go internal/web/assets/templates/seedbox_fleet.html internal/web/server.go internal/web/routes.go internal/web/seedboxes.go internal/web/fleet_test.go
git commit -m "feat(web): PulsedMedia fleet view from the cached feed"
```

---

## Self-review notes

- Spec requirement "PM data must not appear on `/public`" is covered by `TestFleetExcludesPMBoxesFromPublic` (Task 6).
- Spec requirement "key never in export/API/HTML" is covered by Task 3.
- Spec requirement "cancelled hidden by default, excluded from totals" is covered by Task 6.
- Spec requirement "no SSH / no inbound" needs no test; no such code is added.
- Deliberately not planned: threshold alerts, auto-created seedboxes, a new import format.
