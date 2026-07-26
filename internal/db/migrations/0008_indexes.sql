-- 0008_indexes.sql — indexes for hot lookup paths.

CREATE INDEX idx_server_disks_server_id ON server_disks(server_id);
CREATE INDEX idx_yabs_disk_speed_yabs_id ON yabs_disk_speed(yabs_id);
CREATE INDEX idx_yabs_network_speed_yabs_id ON yabs_network_speed(yabs_id);
CREATE INDEX idx_notes_service ON notes(service_id, service_type);
CREATE INDEX idx_dns_domain_id ON dns(domain_id);
