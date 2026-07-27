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

// idlerthing app.js — progressive enhancement, no inline scripts (CSP-safe).

// Make table rows with .row-link clickable (except interactive children).
// data-href is always a server-built path (e.g. "/servers/12"), never user
// input — keep it that way, it is assigned straight to location.
document.addEventListener('click', function (e) {
  const row = e.target.closest('tr.row-link');
  if (!row?.dataset.href) return;
  if (e.target.closest('a, button, form, input, select, textarea')) return;
  window.location.href = row.dataset.href;
});

// Copy buttons (.copy-btn with data-copy text).
document.addEventListener('click', function (e) {
  const btn = e.target.closest('.copy-btn');
  if (!btn) return;
  const text = btn.dataset.copy || '';
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text).then(function () {
      btn.textContent = '✓';
      setTimeout(function () { btn.textContent = '⧉'; }, 1500);
    });
  }
});

// Keep the accent color picker and its hex text field in sync (settings page).
document.addEventListener('input', function (e) {
  if (e.target.classList.contains('color-swatch')) {
    const text = e.target.closest('.input-group')?.querySelector('input[type="text"]');
    if (text) text.value = e.target.value;
  }
  if (e.target.name === 'accent_color') {
    const picker = e.target.closest('.input-group')?.querySelector('.color-swatch');
    if (picker && /^#[0-9a-fA-F]{6}$/.test(e.target.value)) picker.value = e.target.value;
  }
});
