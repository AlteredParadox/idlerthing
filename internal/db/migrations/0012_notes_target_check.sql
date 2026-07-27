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

-- 0012_notes_target_check.sql — a note must target exactly one thing:
-- (service_id AND service_type) XOR ip_id. SQLite can't ADD CONSTRAINT,
-- so the invariant lives in the write paths (see NoteStore.Create); this
-- migration deletes historical rows violating it (both-set or neither-set).
DELETE FROM notes WHERE
    (ip_id IS NOT NULL AND (service_id IS NOT NULL OR service_type IS NOT NULL))
 OR (ip_id IS NULL AND (service_id IS NULL OR service_type IS NULL));
