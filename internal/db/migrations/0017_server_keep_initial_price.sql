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

-- 0017_server_keep_initial_price.sql — per-server "keep" flag and initial price.
--
-- keep: whether the server is being kept past its next renewal (defaults on).
-- initial_price / initial_term / initial_currency: what the server cost at
-- first purchase; 0 means it was free (no term or currency). Saving with any
-- part blank copies it from the current pricing, so the initial price is
-- frozen and survives later price, term or currency changes, and even the
-- pricing being removed. Existing servers are backfilled the same way below.
-- NULL remains only for a server that has never had pricing.

ALTER TABLE servers ADD COLUMN keep INT NOT NULL DEFAULT 1;
ALTER TABLE servers ADD COLUMN initial_price REAL;
ALTER TABLE servers ADD COLUMN initial_term INT;
ALTER TABLE servers ADD COLUMN initial_currency TEXT;

UPDATE servers SET
    initial_price = (SELECT p.price FROM pricings p
                     WHERE p.service_type = 1 AND p.service_id = servers.id AND p.active = 1),
    initial_term  = (SELECT p.term FROM pricings p
                     WHERE p.service_type = 1 AND p.service_id = servers.id AND p.active = 1),
    initial_currency = (SELECT p.currency FROM pricings p
                     WHERE p.service_type = 1 AND p.service_id = servers.id AND p.active = 1)
WHERE initial_price IS NULL;
