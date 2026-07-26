-- 0005_prometheus.sql — Prometheus live-monitoring config.

ALTER TABLE settings ADD COLUMN prometheus_enabled INT NOT NULL DEFAULT 0;
ALTER TABLE settings ADD COLUMN prometheus_url TEXT NOT NULL DEFAULT '';
