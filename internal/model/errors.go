// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package model

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrConflict is returned by write paths whose UNIQUE constraint refused the
// row (a catalog name or an IP already attached to that service). Handlers
// test for it with errors.Is instead of matching driver message text.
var ErrConflict = errors.New("already exists")

// sqliteCode reports whether err is a modernc.org/sqlite error carrying the
// given EXTENDED result code. The driver returns the operation's own rc,
// which SQLite makes extended for constraint failures (2067 for UNIQUE,
// 787 for FOREIGN KEY) — checked against the driver rather than assumed.
// Classifying on the code instead of the message means a driver release
// that rewords or re-prefixes its messages cannot turn "already exists"
// into a generic failure.
func sqliteCode(err error, code int) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == code
}

// IsUniqueViolation reports whether err is a UNIQUE constraint failure.
func IsUniqueViolation(err error) bool { return sqliteCode(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE) }

// IsForeignKeyViolation reports whether err is a FOREIGN KEY constraint failure.
func IsForeignKeyViolation(err error) bool {
	return sqliteCode(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY)
}
