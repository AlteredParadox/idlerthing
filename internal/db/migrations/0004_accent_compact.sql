-- 0004_accent_compact.sql — UI customization: accent color + compact tables.

ALTER TABLE settings ADD COLUMN accent_color TEXT NOT NULL DEFAULT '#5b9cf8';
ALTER TABLE settings ADD COLUMN compact_mode INT NOT NULL DEFAULT 0;
