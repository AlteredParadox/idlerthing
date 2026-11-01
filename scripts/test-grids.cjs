// Browser regression checks for customizable grids using synthetic data only.
// Install Playwright separately, then run:
// NODE_PATH=/path/to/node_modules node scripts/test-grids.cjs
const {chromium} = require('playwright');
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

const assets = path.join(__dirname, '../internal/web/assets/static');
const serverTable = `<table class="tbl server-grid-table" data-widths-key="test.servers.widths">
  <colgroup><col data-column="hostname" data-label="Hostname" data-default-width="240">
    <col data-column="type" data-label="Type" data-default-width="88">
    <col data-column="actions" data-default-width="120"></colgroup>
  <thead><tr><th><a href="?sort=hostname">Hostname</a></th><th>Type</th><th class="col-actions">Actions</th></tr></thead>
  <tbody><tr><td><div class="cell-main"><span class="dot dot-ok"></span><a class="mono cell-title" href="#">example.invalid</a></div></td><td>VPS</td><td class="col-actions"><div class="row-actions"><button class="icon-btn" type="button">⇄</button><a class="icon-btn" href="#">✎</a><button class="icon-btn" type="button">🗑</button></div></td></tr></tbody></table>`;
const fixture = `<!doctype html><html><head><link rel="stylesheet" href="/app.css">
  <script defer src="/app.js"></script></head><body data-grid-user="1"><main>
  <div class="server-grid-tools"><span class="field-hint"></span><button id="reset-server-widths">Reset widths</button></div>
  <div id="server-table-wrap">${serverTable}</div>
  <section class="panel"><h2>Labels (1)</h2><table class="tbl mini"><thead class="sr-only"><tr><th>Name</th><th>Used</th><th class="col-actions">Actions</th></tr></thead>
  <tbody><tr><td><input value="Example label"></td><td>2</td><td><button>Delete</button></td></tr></tbody></table></section>
  </main></body></html>`;

(async () => {
  const server = http.createServer((req, res) => {
    const asset = req.url === '/app.js' ? 'app.js' : req.url === '/app.css' ? 'app.css' : null;
    res.setHeader('Content-Type', asset ? (asset.endsWith('.js') ? 'text/javascript' : 'text/css') : 'text/html');
    res.end(asset ? fs.readFileSync(path.join(assets, asset)) : fixture);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let browser;
  try {
    browser = await chromium.launch({headless: true});
    const page = await browser.newPage({viewport: {width: 1280, height: 900}});
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const base = `http://127.0.0.1:${server.address().port}`;
    const ready = () => page.waitForFunction(() => document.querySelectorAll('.column-resizer').length === 4);
    const table = page.locator('.server-grid-table');
    const order = () => table.locator('col').evaluateAll(cols => cols.map(col => col.dataset.column));
    const resize = () => page.getByRole('separator', {name: 'Resize Hostname column', exact: true});
    const width = async () => Number(await resize().getAttribute('aria-valuenow'));
    const aligned = async () => assert(await table.evaluate(table => {
      const keys = [...table.querySelectorAll('col')].map(col => col.dataset.column).join(',');
      return [...table.rows].every(row => [...row.cells].map(cell => cell.dataset.gridColumn).join(',') === keys);
    }), 'headers, values and col widths must move together');
    await page.goto(base);
    await ready();
    const initialWidth = await width();
    const box = await resize().boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2 + 70, box.y + box.height / 2, {steps: 6});
    await page.mouse.up();
    assert.equal(await width(), initialWidth + 70);
    assert.deepEqual(await order(), ['hostname', 'type', 'actions']);

    const target = table.locator('thead th').nth(1);
    const targetBox = await target.boundingBox();
    await page.getByRole('button', {name: 'Move Hostname column', exact: true}).dragTo(target,
      {targetPosition: {x: Math.round(targetBox.width * 0.75), y: Math.round(targetBox.height / 2)}});
    assert.deepEqual(await order(), ['type', 'hostname', 'actions'], 'native drop must accept and persist the move');
    await aligned();
    await page.reload();
    await ready();
    assert.deepEqual(await order(), ['type', 'hostname', 'actions']);
    assert.equal(await width(), initialWidth + 70, 'width follows the named column after reload');
    await aligned();
    await page.locator('.server-grid-tools').getByRole('button', {name: 'Reset order', exact: true}).click();
    assert.deepEqual(await order(), ['hostname', 'type', 'actions']);
    assert.equal(await width(), initialWidth + 70, 'resetting order preserves widths');
    await resize().press('Shift+ArrowRight');
    assert.equal(await width(), initialWidth + 120);
    await page.locator('.server-grid-tools').getByRole('button', {name: 'Reset widths', exact: true}).click();
    assert.equal(await width(), initialWidth);

    // Fit columns sizes each column to its widest visible cell, with no
    // clipping, and the result survives a reload like any other width.
    await page.locator('.server-grid-tools').getByRole('button', {name: 'Fit columns', exact: true}).click();
    const fitted = await width();
    assert(fitted >= 64 && fitted < initialWidth, 'a short hostname needs less than the 240px default');
    assert(await table.evaluate(table => [...table.rows].every(row =>
      [...row.cells].every(cell => cell.scrollWidth <= cell.clientWidth))), 'fitted cells do not clip their contents');
    await page.reload();
    await ready();
    assert.equal(await width(), fitted, 'fitted widths persist');
    // A grid wider than its scroll wrapper must measure the same: the wrapper
    // may not squeeze ellipsizing cells before they are read.
    await page.setViewportSize({width: 320, height: 900});
    await page.locator('.server-grid-tools').getByRole('button', {name: 'Fit columns', exact: true}).click();
    assert.equal(await width(), fitted, 'fitting is independent of the viewport');
    await page.setViewportSize({width: 1280, height: 900});
    await page.locator('.server-grid-tools').getByRole('button', {name: 'Reset widths', exact: true}).click();
    assert.equal(await width(), initialWidth);

    // Fit to page widens the fitted columns until the grid is flush with its
    // wrapper and nothing scrolls; it is disabled while the content alone is
    // wider than the page, and comes back when the page grows again.
    const fitPage = page.locator('.server-grid-tools').getByRole('button', {name: 'Fit to page', exact: true});
    const wrapFit = () => page.evaluate(() => {
      const wrap = document.getElementById('server-table-wrap');
      const grid = wrap.querySelector('table').getBoundingClientRect().width;
      return {scrolls: wrap.scrollWidth > wrap.clientWidth, gap: wrap.clientWidth - grid};
    });
    await fitPage.click();
    const pageWidth = await width();
    assert(pageWidth > fitted, 'with room to spare the columns grow past their content');
    let flush = await wrapFit();
    assert(!flush.scrolls, 'fit to page must not leave a horizontal scrollbar');
    assert(flush.gap >= 0 && flush.gap < 4, 'the grid ends flush with the page, got gap ' + flush.gap);
    // The actions column now fits its one button, so the fixture's content
    // only exceeds the page below about 290px.
    await page.setViewportSize({width: 240, height: 900});
    await page.waitForFunction(() => document.querySelector('[data-fit-grid-page]').disabled);
    await page.setViewportSize({width: 1280, height: 900});
    await page.waitForFunction(() => !document.querySelector('[data-fit-grid-page]').disabled);
    // Wide enough that the hostname column hits its 1600px cap: the share it
    // cannot take must go to the other column, not vanish into a gap.
    await page.setViewportSize({width: 2600, height: 900});
    await fitPage.click();
    assert.equal(await width(), 1600, 'the proportional share is capped at maxWidth');
    flush = await wrapFit();
    assert(!flush.scrolls && flush.gap >= 0 && flush.gap < 4, 'capped slack is redistributed, got gap ' + flush.gap);
    await page.setViewportSize({width: 1280, height: 900});
    await fitPage.click();
    assert.equal(await width(), pageWidth, 'fit to page is deterministic for a given page width');
    await page.reload();
    await ready();
    assert.equal(await width(), pageWidth, 'page-fitted widths persist');
    // A saved actions width narrower than its controls (saved while Compact
    // tables shrank the icons, say) is lifted to the content width on load,
    // since that column has no handle to recover it by hand.
    await page.evaluate(() => {
      const stored = JSON.parse(localStorage.getItem('test.servers.widths') || '{}');
      stored.actions = 64;
      localStorage.setItem('test.servers.widths', JSON.stringify(stored));
    });
    await page.reload();
    await ready();
    assert(await table.locator('col[data-column="actions"]').evaluate(col => parseInt(col.style.width, 10)) > 64,
      'a too-narrow saved actions width is lifted to its content');
    assert(await table.locator('tbody td').last().evaluate(td => td.scrollWidth <= td.clientWidth), 'action controls are not clipped');
    await page.locator('.server-grid-tools').getByRole('button', {name: 'Reset widths', exact: true}).click();

    // Keyboard movement, hidden columns and htmx replacement use the same keys.
    await page.getByRole('button', {name: 'Move Type column', exact: true}).press('Home');
    assert.deepEqual(await order(), ['type', 'hostname', 'actions']);
    await page.evaluate(html => {
      const temp = document.createElement('div'); temp.innerHTML = html;
      temp.querySelector('col[data-column="type"]').remove();
      for (const row of temp.querySelectorAll('tr')) row.cells[1].remove();
      document.getElementById('server-table-wrap').innerHTML = temp.innerHTML;
      document.dispatchEvent(new Event('htmx:afterSwap'));
    }, serverTable);
    assert.deepEqual(await order(), ['hostname', 'actions']);
    await page.evaluate(html => {
      document.getElementById('server-table-wrap').innerHTML = html;
      document.dispatchEvent(new Event('htmx:afterSwap'));
    }, serverTable);
    await ready();
    assert.deepEqual(await order(), ['type', 'hostname', 'actions'], 'hidden columns retain their saved position');
    await aligned();
    await page.evaluate(() => document.dispatchEvent(new Event('htmx:historyRestore')));
    assert.equal(await table.locator('.column-reorder-handle').count(), 2, 'history events do not duplicate controls');

    const mini = page.locator('table.mini');
    assert.equal(await mini.locator('thead.sr-only').count(), 0, 'mini grid headers are accessible');
    await page.getByRole('button', {name: 'Move Name column', exact: true}).press('End');
    assert.deepEqual(await mini.locator('col').evaluateAll(cols => cols.map(col => col.dataset.column)), ['used', 'name', 'actions']);
    assert.equal(await mini.locator('input').inputValue(), 'Example label', 'moving editable cells preserves their inputs');
    const before = await order();
    await page.getByRole('button', {name: 'Move Name column', exact: true}).dragTo(table.locator('th').first());
    assert.deepEqual(await order(), before, 'cross-grid drops are ignored');

    // Each signed-in user has an independent generic-grid preference scope.
    await page.evaluate(() => {
      document.body.dataset.gridUser = '2';
      const table = document.querySelector('table.mini');
      const clone = table.cloneNode(true);
      clone.removeAttribute('data-widths-key');
      table.replaceWith(clone);
      document.dispatchEvent(new Event('htmx:afterSwap'));
    });
    assert(await mini.getAttribute('data-widths-key').then(key => key.includes('.2.')));
    assert.deepEqual(errors, []);
    console.log('PASS: native drag/drop, resizing, fitting, fit to page, persistence, resets, keyboard, hidden columns, htmx, editable grids and isolation');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
