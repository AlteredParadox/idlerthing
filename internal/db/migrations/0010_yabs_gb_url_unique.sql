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

-- 0010_yabs_gb_url_unique.sql — atomic geekbench-URL dedup.

-- Dedupe pre-existing (server_id, gb_url) duplicates, keeping the lowest id.
DELETE FROM yabs WHERE gb_url != '' AND id NOT IN (
    SELECT MIN(id) FROM yabs WHERE gb_url != ''
    GROUP BY server_id, gb_url
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_yabs_server_gburl
    ON yabs(server_id, gb_url) WHERE gb_url != '';
