-- idlerthing — a lightweight, self-hosted inventory for hosting services.
-- Copyright (C) 2026 AlteredParadox
--
-- This program is free software: you can redistribute it and/or modify it
-- under the terms of the GNU Affero General Public License as published by
-- the Free Software Foundation, either version 3 of the License, or (at your
-- option) any later version.
--
-- This program is distributed in the hope that it will be useful, but WITHOUT
-- ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
-- FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
-- for more details.
--
-- You should have received a copy of the GNU Affero General Public License
-- along with this program. If not, see <https://www.gnu.org/licenses/>.

-- 0009_yabs_caps.sql — single-use ingest capabilities + atomic payload dedup.

-- Dedupe pre-existing (server_id, payload_hash) duplicates, keeping the
-- lowest id, so the partial unique index can be created.
DELETE FROM yabs WHERE payload_hash != '' AND id NOT IN (
    SELECT MIN(id) FROM yabs WHERE payload_hash != ''
    GROUP BY server_id, payload_hash
);

-- Consumed ingest capabilities: (server_id, ts) is the signed unit; a row
-- here means the signed URL was already used.
CREATE TABLE yabs_caps (
    server_id   INTEGER NOT NULL,
    ts          INTEGER NOT NULL,
    consumed_at TEXT,
    PRIMARY KEY (server_id, ts)
);

-- Atomic byte-identical dedup (races safe): one payload hash per server.
CREATE UNIQUE INDEX IF NOT EXISTS idx_yabs_server_payload
    ON yabs(server_id, payload_hash) WHERE payload_hash != '';
