// idlerthing app.js — progressive enhancement, no inline scripts (CSP-safe).

// Make table rows with .row-link clickable (except interactive children).
// data-href is always a server-built path (e.g. "/servers/12"), never user
// input — keep it that way, it is assigned straight to location.
document.addEventListener('click', function (e) {
  const row = e.target.closest('tr.row-link');
  if (!row || !row.dataset.href) return;
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
