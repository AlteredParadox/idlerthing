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
  if (e.target.closest('a, button, form, input, select, textarea, details, summary')) return;
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

// Customizable grids. Existing server width keys remain compatible; other tables
// get independent keys based on their page, section and column names.
(() => {
  const minWidth = 64;
  const maxWidth = 1600;
  const grids = new Map();
  let columnDrag = null;

  function initGrid(table) {
    const headers = Array.from(table.tHead?.rows[0]?.cells || []);
    if (!headers.length) return;
    const labels = headers.map(header => {
      const copy = header.cloneNode(true);
      copy.querySelectorAll('.arrow, .column-resizer, .column-reorder-handle').forEach(element => element.remove());
      return copy.textContent.replace(/\s+/g, ' ').trim();
    });
    const keys = labels.map((label, index) =>
      headers[index].classList.contains('col-actions') || label.toLowerCase() === 'actions'
        ? 'actions' : label.toLowerCase().replace(/[^a-z0-9]+/g, '_').replace(/^_|_$/g, '') || 'column_' + index);
    // Mini tables previously hid their headings. Make the headers available
    // visually as well so their resize controls can be reached.
    table.tHead.classList.remove('sr-only');

    const serverGrid = table.classList.contains('server-grid-table');
    let wrap = table.parentElement;
    if (!serverGrid && !wrap.classList.contains('resizable-grid-wrap')) {
      const container = document.createElement('div');
      container.className = 'resizable-grid-wrap';
      table.before(container);
      container.appendChild(table);
      wrap = container;
    }
    let reset;
    if (serverGrid) {
      reset = document.getElementById('reset-server-widths');
    } else {
      let tools = wrap.previousElementSibling;
      if (!tools?.hasAttribute('data-grid-tools')) {
        tools = document.createElement('div');
        tools.className = 'grid-tools' + (table.classList.contains('mini') ? ' grid-tools-mini' : '');
        tools.setAttribute('data-grid-tools', '');
        const hint = document.createElement('span');
        hint.className = 'field-hint';
        hint.textContent = 'Drag headers to reorder; drag edges to resize.';
        reset = document.createElement('button');
        reset.type = 'button';
        reset.className = 'btn';
        reset.textContent = 'Reset widths';
        tools.append(hint, reset);
        wrap.before(tools);
      } else {
        reset = tools.querySelector('button');
      }
    }
    if (!reset) return;
    reset.disabled = false;
    const tools = reset.closest('.server-grid-tools, .grid-tools');
    tools.querySelector('.field-hint').textContent = 'Drag headers to reorder; drag edges to resize.';
    let actions = tools.querySelector('.grid-actions');
    if (!actions) {
      actions = document.createElement('span');
      actions.className = 'grid-actions';
      reset.replaceWith(actions);
      actions.appendChild(reset);
    }
    let resetOrder = actions.querySelector('[data-reset-grid-order]');
    if (!resetOrder) {
      resetOrder = document.createElement('button');
      resetOrder.type = 'button';
      resetOrder.className = 'btn';
      resetOrder.textContent = 'Reset order';
      resetOrder.setAttribute('data-reset-grid-order', '');
      actions.appendChild(resetOrder);
    }
    resetOrder.disabled = false;
    let fit = actions.querySelector('[data-fit-grid-columns]');
    if (!fit) {
      fit = document.createElement('button');
      fit.type = 'button';
      fit.className = 'btn';
      fit.textContent = 'Fit columns';
      fit.title = 'Shrink or grow each column to its widest visible cell';
      fit.setAttribute('data-fit-grid-columns', '');
      reset.after(fit);
    }
    fit.disabled = false;
    let status = tools.querySelector('[data-grid-status]');
    if (!status) {
      status = document.createElement('span');
      status.className = 'sr-only';
      status.setAttribute('data-grid-status', '');
      status.setAttribute('aria-live', 'polite');
      tools.appendChild(status);
    }

    if (!table.querySelector('colgroup')) {
      const group = document.createElement('colgroup');
      const measured = headers.map(header => Math.min(maxWidth, Math.max(minWidth, Math.round(header.getBoundingClientRect().width))));
      keys.forEach((key, index) => {
        const column = document.createElement('col');
        column.dataset.column = keys.indexOf(key) === index ? key : key + '_' + index;
        column.dataset.label = labels[index];
        column.dataset.defaultWidth = measured[index];
        group.appendChild(column);
      });
      table.prepend(group);
    }
    table.classList.add('resizable-grid');

    const columns = Array.from(table.querySelectorAll('colgroup col'));
    const user = document.body.dataset.gridUser || 'public';
    const page = location.pathname.replace(/\/\d+(?=\/|$)/g, '/:id');
    const heading = table.closest('section, .panel')?.querySelector('h2, h3')?.textContent.replace(/\([^)]*\)/g, '').trim() || '';
    // Optional columns may appear/disappear in a partial update. The grid's
    // identity must not depend on its full current column set.
    const identity = table.dataset.gridKey || heading || keys[0];
    const storageKey = table.dataset.widthsKey || 'idlerthing.grid.widths.' + user + '.' + page + ':' + identity;
    table.dataset.widthsKey = storageKey;
    const orderKey = storageKey + '.order';
    columns.forEach((column, index) => {
      if (!column.hasAttribute('data-default-order')) column.dataset.defaultOrder = index;
    });
    const defaultOrder = [...columns].sort((a, b) => Number(a.dataset.defaultOrder) - Number(b.dataset.defaultOrder))
      .map(column => column.dataset.column).filter(key => key !== 'actions');
    const columnNodes = new Map(columns.map(column => [column.dataset.column, column]));
    for (const row of table.rows) {
      if (row.cells.length === columns.length) {
        Array.from(row.cells).forEach((cell, index) => { cell.dataset.gridColumn = columns[index].dataset.column; });
      }
    }
    let saved = Object.create(null);
    let savedOrder = [];
    const widths = Object.create(null);
    const handles = Object.create(null);
    let drag = null;
    let dropTarget = null;
    let suppressClickUntil = 0;
    let scrollFrame = 0;
    let scrollDirection = 0;
    try {
      const stored = JSON.parse(localStorage.getItem(storageKey) || '{}');
      if (stored && typeof stored === 'object' && !Array.isArray(stored)) {
        for (const [key, width] of Object.entries(stored)) {
          if (/^[a-z0-9_]+$/.test(key) && Number.isInteger(width) && width >= minWidth && width <= maxWidth) {
            saved[key] = width;
          }
        }
      }
    } catch (_) { /* Storage restrictions must not prevent resizing. */ }
    try {
      const stored = JSON.parse(localStorage.getItem(orderKey) || '[]');
      if (Array.isArray(stored) && stored.length <= 128) {
        savedOrder = [...new Set(stored.filter(key => typeof key === 'string' &&
          /^[a-z0-9_]{1,64}$/.test(key) && key !== 'actions'))];
      }
    } catch (_) { /* Ordering also works without persistent browser storage. */ }

    function applyOrder(order) {
      const visible = [...new Set([...order, ...defaultOrder])].filter(key => columnNodes.has(key) && key !== 'actions');
      if (columnNodes.has('actions')) visible.push('actions');
      const focused = table.contains(document.activeElement) ? document.activeElement : null;
      const group = table.querySelector('colgroup');
      for (const key of visible) group.appendChild(columnNodes.get(key));
      for (const row of table.rows) {
        if (row.cells.length !== columns.length) continue;
        const cells = new Map(Array.from(row.cells, cell => [cell.dataset.gridColumn, cell]));
        for (const key of visible) row.appendChild(cells.get(key));
      }
      focused?.focus({preventScroll: true});
    }

    function moveColumn(key, target, after) {
      if (key === target || key === 'actions') return;
      const fullOrder = [...new Set([...savedOrder, ...defaultOrder])].filter(item => item !== key);
      const index = target === 'actions' ? fullOrder.length : fullOrder.indexOf(target) + (after ? 1 : 0);
      if (index < 0) return;
      fullOrder.splice(index, 0, key);
      savedOrder = fullOrder;
      applyOrder(savedOrder);
      try { localStorage.setItem(orderKey, JSON.stringify(savedOrder)); } catch (_) {}
      const position = Array.from(table.querySelectorAll('col')).findIndex(column => column.dataset.column === key) + 1;
      status.textContent = 'Moved ' + columnNodes.get(key).dataset.label + ' column to position ' + position + '.';
    }

    function clearDropTarget() {
      dropTarget?.header.classList.remove('column-drop-before', 'column-drop-after');
      dropTarget = null;
    }

    function stopAutoScroll() {
      cancelAnimationFrame(scrollFrame);
      scrollFrame = 0;
      scrollDirection = 0;
    }

    function autoScroll() {
      scrollFrame = 0;
      if (columnDrag?.table !== table || !scrollDirection) return;
      wrap.scrollLeft += scrollDirection * 14;
      scrollFrame = requestAnimationFrame(autoScroll);
    }

    function endColumnDrag() {
      if (columnDrag?.table !== table) return;
      headers.forEach(header => header.classList.remove('column-dragging'));
      clearDropTarget();
      stopAutoScroll();
      columnDrag = null;
      document.documentElement.classList.remove('reordering-columns');
    }

    function overColumn(event) {
      if (columnDrag?.table !== table) return;
      const header = event.target.closest('th');
      if (!header || header.closest('table') !== table) {
        clearDropTarget();
        return;
      }
      event.preventDefault();
      event.dataTransfer.dropEffect = 'move';
      clearDropTarget();
      const target = header.dataset.gridColumn;
      if (target === columnDrag.key) return;
      const rect = header.getBoundingClientRect();
      const after = target !== 'actions' && event.clientX > rect.left + rect.width / 2;
      header.classList.add(after ? 'column-drop-after' : 'column-drop-before');
      dropTarget = {header, key: target, after};
    }

    function dropColumn(event) {
      if (columnDrag?.table !== table) return;
      event.preventDefault();
      if (dropTarget) moveColumn(columnDrag.key, dropTarget.key, dropTarget.after);
      suppressClickUntil = Date.now() + 250;
      endColumnDrag();
    }

    function dragOverGrid(event) {
      if (columnDrag?.table !== table) return;
      const rect = wrap.getBoundingClientRect();
      scrollDirection = event.clientX < rect.left + 32 ? -1 : event.clientX > rect.right - 32 ? 1 : 0;
      if (scrollDirection && !scrollFrame) scrollFrame = requestAnimationFrame(autoScroll);
      if (!scrollDirection) stopAutoScroll();
    }

    function leaveGrid(event) {
      if (!wrap.contains(event.relatedTarget)) {
        stopAutoScroll();
        clearDropTarget();
      }
    }

    function suppressDragClick(event) {
      if (Date.now() < suppressClickUntil) {
        event.preventDefault();
        event.stopImmediatePropagation();
      }
    }

    function applyWidths() {
      let total = 0;
      for (const column of columns) {
        const key = column.dataset.column;
        const width = widths[key];
        column.style.width = width + 'px';
        total += width;
        if (handles[key]) handles[key].setAttribute('aria-valuenow', width);
      }
      table.style.width = total + 'px';
    }

    function setWidth(key, width) {
      widths[key] = Math.min(maxWidth, Math.max(minWidth, Math.round(width)));
      applyWidths();
    }

    function saveWidth(key) {
      saved[key] = widths[key];
      try { localStorage.setItem(storageKey, JSON.stringify(saved)); } catch (_) {}
    }

    function endDrag(event) {
      if (!drag || (event && event.pointerId !== drag.pointerId)) return;
      saveWidth(drag.key);
      handles[drag.key].classList.remove('resizing');
      document.documentElement.classList.remove('resizing-columns');
      drag = null;
    }

    function moveDrag(event) {
      if (!drag || event.pointerId !== drag.pointerId) return;
      setWidth(drag.key, drag.width + event.clientX - drag.x);
    }

    columns.forEach((column, index) => {
      const key = column.dataset.column;
      const defaultWidth = Number(column.dataset.defaultWidth);
      widths[key] = key === 'actions' ? defaultWidth : (saved[key] || defaultWidth);
      if (key === 'actions') return;
      const header = table.tHead.rows[0].cells[index];
      header.querySelector('.column-reorder-handle')?.remove();
      header.draggable = true;
      header.querySelectorAll('a').forEach(link => { link.draggable = false; });
      const reorder = document.createElement('button');
      reorder.type = 'button';
      reorder.className = 'column-reorder-handle';
      reorder.draggable = true;
      reorder.textContent = '⠿';
      reorder.setAttribute('aria-label', 'Move ' + column.dataset.label + ' column');
      reorder.title = 'Drag to move; Left/Right keys move the column; Home/End move to the first/last position';
      reorder.addEventListener('click', event => event.stopPropagation());
      reorder.addEventListener('keydown', event => {
        if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
        event.preventDefault();
        const visible = Array.from(table.querySelectorAll('col'), column => column.dataset.column).filter(key => key !== 'actions');
        const index = visible.indexOf(key);
        if (event.key === 'Home') moveColumn(key, visible[0], false);
        else if (event.key === 'End') moveColumn(key, visible[visible.length - 1], true);
        else if (event.key === 'ArrowLeft' && index > 0) moveColumn(key, visible[index - 1], false);
        else if (event.key === 'ArrowRight' && index < visible.length - 1) moveColumn(key, visible[index + 1], true);
      });
      header.prepend(reorder);
      header.addEventListener('dragstart', event => {
        if (drag || event.target.closest('.column-resizer')) {
          event.preventDefault();
          return;
        }
        columnDrag = {table, key};
        event.dataTransfer.effectAllowed = 'move';
        event.dataTransfer.setData('text/plain', column.dataset.label);
        header.classList.add('column-dragging');
        document.documentElement.classList.add('reordering-columns');
      });
      header.querySelector('.column-resizer')?.remove();
      const handle = document.createElement('span');
      handle.className = 'column-resizer';
      handle.tabIndex = 0;
      handle.setAttribute('role', 'separator');
      handle.setAttribute('aria-orientation', 'vertical');
      handle.setAttribute('aria-label', 'Resize ' + column.dataset.label + ' column');
      handle.setAttribute('aria-valuemin', minWidth);
      handle.setAttribute('aria-valuemax', maxWidth);
      handle.title = 'Drag to resize; double-click to reset';
      handle.addEventListener('pointerdown', event => {
        if (event.button !== 0) return;
        event.preventDefault();
        event.stopPropagation();
        handle.focus({preventScroll: true});
        handle.setPointerCapture(event.pointerId);
        drag = {key, x: event.clientX, width: widths[key], pointerId: event.pointerId};
        handle.classList.add('resizing');
        document.documentElement.classList.add('resizing-columns');
      });
      handle.addEventListener('click', event => event.stopPropagation());
      handle.addEventListener('dragstart', event => { event.preventDefault(); event.stopPropagation(); });
      handle.addEventListener('dblclick', event => {
        event.preventDefault();
        setWidth(key, defaultWidth);
        saveWidth(key);
      });
      handle.addEventListener('keydown', event => {
        if (!['ArrowLeft', 'ArrowRight', 'Home'].includes(event.key)) return;
        event.preventDefault();
        const step = event.shiftKey ? 50 : 10;
        setWidth(key, event.key === 'Home' ? defaultWidth : widths[key] + (event.key === 'ArrowRight' ? step : -step));
        saveWidth(key);
      });
      header.appendChild(handle);
      handles[key] = handle;
    });

    function resetWidths() {
      endDrag();
      saved = Object.create(null);
      try { localStorage.removeItem(storageKey); } catch (_) {}
      for (const column of columns) widths[column.dataset.column] = Number(column.dataset.defaultWidth);
      applyWidths();
    }

    // Size every column to its widest visible cell. Auto table layout does the
    // measuring: with the fixed widths lifted and the table at max-content
    // width (so the scroll wrapper cannot squeeze it), the browser lays each
    // column out at max-content (wrapping cells stop at their max-width), and
    // the header rect is then exactly the width the column needs. The actions
    // column keeps its default width; it holds buttons, not data.
    function fitWidths() {
      endDrag();
      table.classList.add('measuring-columns');
      for (const column of columns) {
        column.style.width = column.dataset.column === 'actions' ? widths.actions + 'px' : 'auto';
      }
      table.style.width = '';
      const measured = Array.from(table.tHead.rows[0].cells, header => header.getBoundingClientRect().width);
      table.classList.remove('measuring-columns');
      Array.from(table.tHead.rows[0].cells).forEach((header, index) => {
        const key = header.dataset.gridColumn;
        if (key === 'actions') return;
        widths[key] = Math.min(maxWidth, Math.max(minWidth, Math.ceil(measured[index])));
        saved[key] = widths[key];
      });
      applyWidths();
      try { localStorage.setItem(storageKey, JSON.stringify(saved)); } catch (_) {}
      status.textContent = 'Fitted columns to their contents.';
    }

    function resetColumnOrder() {
      endColumnDrag();
      savedOrder = [];
      try { localStorage.removeItem(orderKey); } catch (_) {}
      applyOrder(defaultOrder);
      status.textContent = 'Restored the default column order.';
    }

    applyWidths();
    applyOrder(savedOrder);
    reset.addEventListener('click', resetWidths);
    fit.addEventListener('click', fitWidths);
    resetOrder.addEventListener('click', resetColumnOrder);
    table.addEventListener('dragover', overColumn);
    table.addEventListener('drop', dropColumn);
    table.addEventListener('dragend', endColumnDrag);
    table.addEventListener('click', suppressDragClick, true);
    wrap.addEventListener('dragover', dragOverGrid);
    wrap.addEventListener('dragleave', leaveGrid);
    document.addEventListener('pointermove', moveDrag);
    document.addEventListener('pointerup', endDrag);
    document.addEventListener('pointercancel', endDrag);

    grids.set(table, { head: table.tHead, cleanup: () => {
      endDrag();
      endColumnDrag();
      reset.removeEventListener('click', resetWidths);
      fit.removeEventListener('click', fitWidths);
      resetOrder.removeEventListener('click', resetColumnOrder);
      table.removeEventListener('dragover', overColumn);
      table.removeEventListener('drop', dropColumn);
      table.removeEventListener('dragend', endColumnDrag);
      table.removeEventListener('click', suppressDragClick, true);
      wrap.removeEventListener('dragover', dragOverGrid);
      wrap.removeEventListener('dragleave', leaveGrid);
      document.removeEventListener('pointermove', moveDrag);
      document.removeEventListener('pointerup', endDrag);
      document.removeEventListener('pointercancel', endDrag);
    }});
  }

  function initGrids() {
    for (const [table, grid] of grids) {
      if (!table.isConnected || table.tHead !== grid.head) {
        grid.cleanup();
        grids.delete(table);
      }
    }
    const serverReset = document.getElementById('reset-server-widths');
    if (serverReset) {
      const empty = !document.querySelector('#server-table-wrap table');
      serverReset.disabled = empty;
      serverReset.closest('.server-grid-tools')
        .querySelectorAll('[data-reset-grid-order], [data-fit-grid-columns]')
        .forEach(button => { button.disabled = empty; });
    }
    document.querySelectorAll('table.tbl').forEach(table => {
      if (!grids.has(table)) initGrid(table);
    });
  }

  document.addEventListener('DOMContentLoaded', initGrids);
  document.addEventListener('htmx:afterSwap', initGrids);
  document.addEventListener('htmx:historyRestore', initGrids);
})();
