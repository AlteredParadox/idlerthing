#!/bin/bash
# Stamps the AGPL-3.0-or-later notice on every first-party source file, or
# with --check reports the ones missing it (used by `make license-check`).
#
# Scope is FIRST-PARTY SOURCE only: .go (tests included), the hand-written
# front-end assets, the templates, and the SQL migrations. Deliberately NOT
# stamped: build tooling (Makefile, these scripts — they carry a descriptive
# comment instead), vendored third-party assets (htmx.min.js, the woff2 fonts
# and their vendor/*.LICENSE files, which carry their OWN licences), generated
# files, and anything outside the repo's own source.
#
# Comment syntax per type matters:
#   .go/.js   //          plain line comments
#   .css      /* … */     block comment
#   .sql      --          SQL line comments
#   .html     {{/* … */}} GO TEMPLATE comment, not <!-- -->, so the notice is
#             stripped at render instead of shipping in every HTTP response
#
# Usage: scripts/license-headers.sh [--check]
set -euo pipefail
export LC_ALL=C
cd "$(git rev-parse --show-toplevel)"

CHECK=0
[[ "${1:-}" == "--check" ]] && CHECK=1

MARKER="GNU Affero General Public License"
YEAR=2026
HOLDER="AlteredParadox"
TAGLINE="idlerthing — a lightweight, self-hosted inventory for hosting services."

# body <line-prefix>: the notice, each line prefixed (trailing space trimmed).
body() {
	local p=$1
	printf '%s\n' \
		"$p$TAGLINE" \
		"${p}Copyright (C) $YEAR $HOLDER" \
		"$p" \
		"${p}This program is free software: you can redistribute it and/or modify it" \
		"${p}under the terms of the GNU Affero General Public License as published by" \
		"${p}the Free Software Foundation, either version 3 of the License, or (at your" \
		"${p}option) any later version." \
		"$p" \
		"${p}This program is distributed in the hope that it will be useful, but WITHOUT" \
		"${p}ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or" \
		"${p}FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License" \
		"${p}for more details." \
		"$p" \
		"${p}You should have received a copy of the GNU Affero General Public License" \
		"${p}along with this program. If not, see <https://www.gnu.org/licenses/>." \
		| sed 's/[[:space:]]*$//'
}

header_for() {
	case "$1" in
	*.go | *.js) body '// ' ;;
	*.sql) body '-- ' ;;
	*.css) { echo '/*'; body ' * '; echo ' */'; } ;;
	*.html) { echo '{{/*'; body ''; echo '*/}}'; } ;;
	*) return 1 ;;
	esac
}

# Tracked, first-party, stampable files.
files() {
	git ls-files -- '*.go' '*.js' '*.css' '*.sql' '*.html' \
		| grep -v '^internal/web/assets/static/htmx.min.js$' \
		| grep -v '^internal/web/assets/vendor/'
}

missing=()
stamped=0
while IFS= read -r f; do
	[[ -z "$f" ]] && continue
	if head -20 "$f" | grep -qF "$MARKER"; then continue; fi
	if [[ "$CHECK" = 1 ]]; then
		missing+=("$f")
		continue
	fi
	hdr=$(header_for "$f") || continue
	tmp=$(mktemp)
	printf '%s\n\n' "$hdr" >"$tmp"
	cat "$f" >>"$tmp"
	mv "$tmp" "$f"
	stamped=$((stamped + 1))
done < <(files)

if [[ "$CHECK" = 1 ]]; then
	if ((${#missing[@]})); then
		printf 'FAIL: missing the AGPL notice (run scripts/license-headers.sh):\n' >&2
		printf '  %s\n' "${missing[@]}" >&2
		exit 1
	fi
	echo "license-headers: all first-party sources carry the AGPL notice"
	exit 0
fi
echo "license-headers: stamped $stamped file(s)"
