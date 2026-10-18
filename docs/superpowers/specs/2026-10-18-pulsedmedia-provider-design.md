# PulsedMedia provider integration for idlerthing

Status: draft for approval
Target repo: idlerthing
Repo path for this file: `docs/superpowers/specs/2026-10-18-pulsedmedia-provider-design.md`

## Intent

pmss-fleet-manager and idlerthing are sibling Go/SQLite apps that both describe the
same boxes. idlerthing is the general inventory; pmss-fleet-manager is the
PulsedMedia-specific live view. The goal is to end up with one app — idlerthing —
that gains the PulsedMedia view, and to retire pmss-fleet-manager without ever
building a network path between the privacy fleet and the NetBird tailnet.

The reason the merge is small: pmss-fleet-manager's live half (SSH probe,
qBittorrent, Docker, `du`) is not needed, because `qui` already covers
qBittorrent and the PulsedMedia customer API covers disk, traffic, CPU and
torrent count. Dropping the SSH half drops the entire reason the app had to sit
inside the privacy fleet. What remains is one outbound HTTPS `GET` to
`pulsedmedia.com/remote/pmapi-feed.php` — the same direction as idlerthing's
existing exchange-rate call, with no inbound listener, no box dialed and no
tailnet↔privacy-fleet link.

## Goals

- A PulsedMedia provider in idlerthing whose API key drives a cached sync of the
  customer-API feed.
- A fleet view (pmss-fleet's card/tile layout) over the PM-backed seedboxes,
  rendered entirely from that cache.
- The cancelled/suspended services the feed returns are hidden by default and
  revealable with a toggle; they never count towards totals or cost.
- The API key never leaves the server through any read path, export or API.
- Keep the existing seedbox model, list and forms; add to them rather than
  replacing them.

## Non-goals

- No SSH, qBittorrent, Docker, `du`/storage-directory or host-probe code is
  ported. `qui` owns torrent state.
- No dedicated top-level "PulsedMedia" section; the fleet view is a view of the
  existing seedboxes section.
- No new hand-authorable JSON schema. `GET /export/json` + `idlerthing import`
  already round-trips a fleet; that stays the import/export story.
- No alerts engine in v1. pmss-fleet's disk/traffic threshold alerts are not
  ported; idlerthing's existing due-soon logic is untouched.
- pmss-fleet-manager is not modified. It keeps running until the replacement is
  trusted, then is removed by hand.

## Anti-goals

- Anything that makes idlerthing reachable from the privacy fleet, or makes
  pulsedmedia.com's feed response reachable unauthenticated.
- The PMAPI key appearing in `/export/json`, the CSV zip, `/api/providers`, a
  rendered HTML attribute, a log line, or a database copy that is world-readable.
- Writing PM usage figures (live, volatile) into the user's inventory columns.
- PM data appearing on the unauthenticated routes (`/public`, `/license`,
  `/source`, `/third-party-licenses`).

## Constraints

- idlerthing is passive today (only exchange-rate and whois calls out). This
  adds its first provider fetch; keep it in one package and one code path.
- SQLite is a single writer connection (`SetMaxOpenConns(1)`). Never hold it
  across a network fetch.
- The deployment is behind NetBird; `IDLER_BEHIND_TLS_PROXY` may or may not be
  set. Nothing here depends on the client address.
- Both apps are AGPLv3-or-later; ported code keeps the header and the notices
  pipeline (`make notices`, `make notices-check`).

## Decisions already made

- The key lives on a **Provider** catalog row, not on each seedbox. The feed is
  one account-wide call, so one key and one fetch is the only shape that fits.
- Precedence is **"feed wins, manual edits stick,"** implemented as a three-way
  merge against the previous feed value (see *Precedence* below). No per-field
  dirty flag, no new columns.
- Live/volatile figures never touch inventory columns; they live in a cache
  table and are read-only rendered.

## Precedence

A naive "fill only when empty" rule is wrong here: `next_due` advances every
billing cycle, so a field filled once would go stale and the dashboard would
report the box overdue forever. The previous feed value is already stored in
`pm_services`, so compare against it:

```
if inventory value is empty        -> write the feed value
else if inventory == previous feed -> write the new feed value   (we wrote it)
else                               -> keep inventory              (human edited)
```

That gives the requested behaviour, advances renewals, and needs no dirty flag.
When a human clears a field, the next sync refills it from the feed — acceptable
and documented.

Status: a service is treated as active iff its `status` equals `"active"`
case-insensitively, the same rule pmss-fleet uses (`serviceActive`). Everything
else is hidden by default. Do not enumerate cancelled/suspended/terminated
spellings.

## Design

### Data model (migration `0014`)

```
ALTER TABLE providers ADD COLUMN pm_api_key TEXT NOT NULL DEFAULT '';
ALTER TABLE seedboxes  ADD COLUMN pm_service_id INTEGER;   -- PM service id, no FK

CREATE TABLE pm_services (
  provider_id INTEGER NOT NULL,
  service_id  INTEGER NOT NULL,
  product TEXT, grp TEXT, domain TEXT, status TEXT,
  registered TEXT, next_due TEXT, billing_cycle TEXT,
  recurring_amount REAL, currency TEXT,
  username TEXT, login_url TEXT, permanent_address TEXT,
  -- live usage, rendered only
  tracked_since TEXT, sent_bytes INTEGER, received_bytes INTEGER,
  cpu_hours INTEGER, cpu_since TEXT,
  disk_used_bytes INTEGER, disk_quota_bytes INTEGER, torrents INTEGER,
  generated_at TEXT, synced_at TEXT NOT NULL,
  PRIMARY KEY (provider_id, service_id)
);
```

`pm_service_id` is a plain integer, deliberately not a foreign key: the cache
rows are replaced wholesale on each sync, and a FK would either block that or
cascade into the user's inventory. A non-matching id just means "no feed data
for this seedbox".

### `internal/pulsedmedia`

Near-port of pmss-fleet's `internal/pmapi`: `Feed`, `Service`, `Access`,
`Usage`, the tolerant `Amount` JSON unmarshal, `MonthsPerCycle`, `Monthly`,
`Parse`, and `Fetch`. `Fetch` keeps pmss-fleet's hardening:

- `X-API-Key` header on the exact `FeedURL`; a redirect is refused rather than
  followed (the key must not travel to a host the feed chooses).
- 30 s timeout, `io.LimitReader` at 4 MiB, 401/403 mapped to `ErrAuth`.
- `PMSS_PMAPI_URL` equivalent (`IDLER_PMAPI_URL`) to point tests at a mock.

No other package imports `net/http` for this feature.

### Sync

- Fetch, then open the write transaction — never hold the writer across the
  network.
- Replace the provider's `pm_services` rows in one transaction; on a fetch error
  keep the previous rows and record the error for the view.
- Trigger: a **Sync now** control on the fleet view, plus a background refresh
  when the cache is older than 6 h. Single-flight per provider. The page always
  renders from cache and reports "synced N ago", so a slow or failing fetch
  never blocks a page load.
- After a successful sync, apply the precedence rule above to the durable
  fields on linked seedboxes: `pricings.next_due_date` (`next_due`),
  `pricings.price` (`recurring_amount`), `pricings.currency` (feed `currency`),
  `pricings.term` (from `billing_cycle`), and `seedboxes.disk_as_mb`
  (`disk_quota_bytes`). Create the pricing row if the seedbox has none.

  `pricings.term` is required: idlerthing stores `price` for the whole cycle and
  divides by `TermMonths(term)` for monthly cost, so a price written without its
  term reports the wrong monthly figure. Map `monthly|quarterly|semi-annually|
  annually` (and the `semiannually`/`yearly` spellings) to idlerthing's existing
  term constants; for any other cycle leave both `price` and `term` unwritten
  rather than guess. A `disk_quota_bytes` of 0 writes nothing.

### Linking a seedbox to a feed service

On sync, for each seedbox whose provider holds a key and with no
`pm_service_id` yet, match against that provider's services: exact
`permanent_address`/`login_url` host or `domain` == `seedboxes.hostname`, else
`hostname` contains the service `username`, else the provider has exactly one
service. A match is stored. Unmatched rows keep NULL and the seedbox form gains
a "PulsedMedia service" dropdown (only for providers with a key) to link or
clear by hand.

### Fleet view

`GET /seedboxes/fleet` — a route under the existing seedboxes section, linked
from the section page. (A dedicated handler and template rather than teaching
the generic section machinery a second view mode: smaller diff, and the fleet
rows come from `pm_services`, not from the section's `List`.) Only PM-backed
seedboxes appear.
Layout follows pmss-fleet: headline tiles (total storage, total traffic, next
renewal, monthly cost) over per-box cards (status, disk used vs quota, sent /
received, CPU-hours, torrent count, plan, next due, price).

Durable fields (plan, next due, price) are read from the inventory rows, which
the sync has already reconciled — the fleet view performs no precedence logic of
its own. Only the live figures (disk used, sent/received, CPU-hours, torrent
count, status) come from `pm_services`. One precedence rule, one place.

Cancelled/suspended services: hidden by default. A "show cancelled" toggle
(query parameter, not a stored setting) reveals them muted; they are excluded
from the headline tiles and cost in both states.

### Key handling (hard requirement)

`providers` is a name-only catalog (`CatalogItem{ID, Name}`) feeding generic
list, template, CSV and JSON paths. The key must be invisible to all of them:

- Keep the generic catalog type free of the key. Read it with a dedicated
  query, not through `CatalogStore.List`.
- `/export/json` and the CSV zip: never emit `pm_api_key`. Add a test that
  greps a full export for the key value.
- `/api/providers`: unchanged shape, no key, no `has_key` field.
- Catalog edit form: follow pmss-fleet's `Settings.Public()` pattern — a
  `has_pm_api_key` boolean and an empty field meaning "keep existing"; an
  explicit clear action removes it. Never echo the key into HTML.
- Import: ignore `pm_api_key` on both the idlerthing and my-idlers paths.
- Provider delete/rename does not silently drop the key; deleting the provider
  removes its `pm_services` rows.

### Unauthenticated routes

`/public` renders seedboxes with `show_public`. PM-backed seedboxes are excluded
from `/public` entirely in v1, so PM usage never leaves the login wall. The
licence/source routes are untouched.

## Rollout

1. Land the migration, package and sync behind a provider with no key set (no
   behaviour change for existing installs).
2. Add the fleet view and the linking control.
3. Run pmss-fleet-manager and idlerthing side by side through one renewal and
   one disk-threshold event; compare.
4. Retire pmss-fleet-manager by hand. Its repo stays as-is.

## Left to the builder

- Exact template/CSS reuse from idlerthing's existing cards vs a new partial.
- Whether the fleet view is a query parameter or a path segment. (Settled in
  the plan: a path segment, `GET /seedboxes/fleet`.)
- Unit conversion: idlerthing stores whole MB (1024-based); the feed uses bytes.
- Timezone for `next_due` rollover (idlerthing already has rollover logic — reuse
  it rather than adding a second notion of "due").
- Whether a stale/failed sync shows a banner or a card badge.

## Not in this design

- Threshold alerts (disk %, traffic %, renewal window). Available later from the
  same cache if wanted.
- Auto-creating seedboxes for services with no inventory row ("add box"
  shortcut in pmss-fleet).
- A sanitized "shareable" export, and a hand-authored import schema.
