-- 0011_notes_orphan_clause.sql — notes with service_id set but service_type
-- NULL are schema-legal orphans: every 0007 parent comparison is NULL for
-- them, so they survived the cleanup even with the parent gone.
DELETE FROM notes WHERE service_id IS NOT NULL AND service_type IS NULL;
