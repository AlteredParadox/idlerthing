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


-- 0013_yabs_network_mode.sql — record the iperf address family.
--
-- yabs.sh tests every location over BOTH IPv4 and IPv6 and tags each row
-- with "mode". Without this column the two runs land as indistinguishable
-- rows sharing a location, so a dual-stack box looked like it had duplicate
-- results. Existing rows predate the split and stay NULL: their family is
-- genuinely unknown, and guessing one would misattribute real measurements.

ALTER TABLE yabs_network_speed ADD COLUMN mode TEXT;
