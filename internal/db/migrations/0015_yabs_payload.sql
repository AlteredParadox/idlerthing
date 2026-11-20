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

-- 0015_yabs_payload.sql — keep the raw yabs.sh JSON for each run.
--
-- Ingest parses a payload into the yabs/yabs_disk_speed/yabs_network_speed
-- columns and, until now, kept only its SHA-256. Anything the parser did not
-- pick up (virtualization type, ISP/ASN, IOPS, mixed read/write speeds, the
-- yabs.sh version) was gone for good. Keeping the body lets later features
-- read fields the schema does not have yet, for every run ingested from here
-- on. Runs stored before this migration have no row: their payload was never
-- kept.
--
-- A side table rather than a column on yabs, so list and aggregate queries
-- over yabs never read the JSON. One row per run at most; ingest caps a body
-- at 64 KB and real ones are 2-4 KB. Removed with the run by the cascade.

CREATE TABLE yabs_payload (
    yabs_id INTEGER PRIMARY KEY REFERENCES yabs(id) ON DELETE CASCADE,
    body TEXT NOT NULL
);
