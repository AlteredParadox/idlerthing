-- 0002_yabs_hash.sql — payload hash for duplicate yabs submission detection.

ALTER TABLE yabs ADD COLUMN payload_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_yabs_payload_hash ON yabs(payload_hash);
