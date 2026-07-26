-- Rename/merge network_type values on servers to the current option set.
UPDATE servers SET network_type = 'IPv4 NAT' WHERE network_type IN ('NAT+IPv4', 'IPv4 (shared)', 'NAT');
