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


-- 0014_yabs_gb_url_placeholder.sql — clear non-URL geekbench "links".
--
-- yabs.sh writes a placeholder into the geekbench url field when there is no
-- result page ("Local (--no-upload)" under --no-upload), and ingest used to
-- store it verbatim. The detail page then linked to it, and because
-- idx_yabs_server_gburl is unique per (server_id, gb_url), every later
-- no-upload run on the same server was dropped as a duplicate of the first.
-- Ingest now keeps gb_url only for http(s) URLs; this clears the placeholders
-- already stored. The runs themselves are kept. (LIKE is ASCII
-- case-insensitive in SQLite, so HTTPS://… survives too.)

UPDATE yabs SET gb_url = NULL
 WHERE gb_url IS NOT NULL
   AND gb_url NOT LIKE 'http://%'
   AND gb_url NOT LIKE 'https://%';
