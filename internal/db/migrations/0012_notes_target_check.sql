-- 0012_notes_target_check.sql — a note must target exactly one thing:
-- (service_id AND service_type) XOR ip_id. SQLite can't ADD CONSTRAINT,
-- so the invariant lives in the write paths (see NoteStore.Create); this
-- migration deletes historical rows violating it (both-set or neither-set).
DELETE FROM notes WHERE
    (ip_id IS NOT NULL AND (service_id IS NOT NULL OR service_type IS NOT NULL))
 OR (ip_id IS NULL AND (service_id IS NULL OR service_type IS NULL));
