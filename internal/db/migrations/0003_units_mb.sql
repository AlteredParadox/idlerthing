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

-- 0003_units_mb.sql — all size/bandwidth values stored in MB (1024-based:
-- 1 GB = 1024 MB, 1 TB = 1024 GB). Bandwidth becomes nullable: NULL = unlimited.

-- Server disks: GB → MB.
ALTER TABLE server_disks RENAME COLUMN size_as_gb TO size_as_mb;
UPDATE server_disks SET size_as_mb = size_as_mb * 1024;

-- Servers: bandwidth TB → MB, 0 → NULL (unlimited).
ALTER TABLE servers RENAME COLUMN bandwidth TO bandwidth_as_mb;
UPDATE servers SET bandwidth_as_mb = bandwidth_as_mb * 1024 * 1024;
UPDATE servers SET bandwidth_as_mb = NULL WHERE bandwidth_as_mb = 0;

-- Shared hosting.
ALTER TABLE shared_hosting RENAME COLUMN disk_as_gb TO disk_as_mb;
UPDATE shared_hosting SET disk_as_mb = disk_as_mb * 1024;
ALTER TABLE shared_hosting RENAME COLUMN bandwidth TO bandwidth_as_mb;
UPDATE shared_hosting SET bandwidth_as_mb = bandwidth_as_mb * 1024 * 1024;
UPDATE shared_hosting SET bandwidth_as_mb = NULL WHERE bandwidth_as_mb = 0;

-- Reseller hosting.
ALTER TABLE reseller_hosting RENAME COLUMN disk_as_gb TO disk_as_mb;
UPDATE reseller_hosting SET disk_as_mb = disk_as_mb * 1024;
ALTER TABLE reseller_hosting RENAME COLUMN bandwidth TO bandwidth_as_mb;
UPDATE reseller_hosting SET bandwidth_as_mb = bandwidth_as_mb * 1024 * 1024;
UPDATE reseller_hosting SET bandwidth_as_mb = NULL WHERE bandwidth_as_mb = 0;

-- Seedboxes.
ALTER TABLE seedboxes RENAME COLUMN disk_as_gb TO disk_as_mb;
UPDATE seedboxes SET disk_as_mb = disk_as_mb * 1024;
ALTER TABLE seedboxes RENAME COLUMN bandwidth TO bandwidth_as_mb;
UPDATE seedboxes SET bandwidth_as_mb = bandwidth_as_mb * 1024 * 1024;
UPDATE seedboxes SET bandwidth_as_mb = NULL WHERE bandwidth_as_mb = 0;
