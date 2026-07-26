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
