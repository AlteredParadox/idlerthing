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

-- 0016_yabs_unmeasured_null.sql — a speed that was not measured is NULL.
--
-- yabs.sh reports "busy" when an iperf endpoint refuses a run, and the parser
-- turns anything it cannot read into 0. Stored as 0, an unmeasured result is
-- indistinguishable from a measured one to AVG()/MIN() and would drag every
-- per-host and per-provider statistic down. No real fio or iperf result is
-- exactly 0, so 0 here has only ever meant "no result".
--
-- From this migration on the store writes NULL for an unmeasured value and
-- reads it back as 0 (the views already render 0 as "—"). This converts the
-- rows written before that.

UPDATE yabs_network_speed SET send_mbps = NULL WHERE send_mbps = 0;
UPDATE yabs_network_speed SET recv_mbps = NULL WHERE recv_mbps = 0;
UPDATE yabs_network_speed SET latency_ms = NULL WHERE latency_ms = 0;
UPDATE yabs_disk_speed SET read_mbps = NULL WHERE read_mbps = 0;
UPDATE yabs_disk_speed SET write_mbps = NULL WHERE write_mbps = 0;
