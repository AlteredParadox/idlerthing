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
-- initial_price / initial_term: what the server cost at first purchase, in
-- the currency of its pricing row. NULL initial_price means "same as the
-- current price" and is resolved when read; 0 means it was free. NULL
-- initial_term means "same term as the current price".

ALTER TABLE servers ADD COLUMN keep INT NOT NULL DEFAULT 1;
ALTER TABLE servers ADD COLUMN initial_price REAL;
ALTER TABLE servers ADD COLUMN initial_term INT;
