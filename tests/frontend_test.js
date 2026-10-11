// Run with: node tests/frontend_test.js (no packages required).
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, '../static/app.js'), 'utf8');

for (const saved of ['2', '999999', '"invalid']) {
  function lane(id) {
    const classes = new Set(id === '1' ? ['active'] : []);
    return {
      dataset: { tab: id },
      classList: { toggle(name, on) { if (on) classes.add(name); else classes.delete(name); } },
      setAttribute() {},
      active() { return classes.has('active'); }
    };
  }
  const lanes = [lane('1'), lane('2')];
  const tabs = [lane('1'), lane('2')];
  const add = { dataset: { lane: '1' } };
  const document = {
    body: { appendChild() {}, addEventListener() {} },
    createElement() { return { setAttribute() {} }; },
    addEventListener() {},
    getElementById() { return null; },
    querySelector(selector) { return selector === '.add-row [data-open-modal]' ? add : null; },
    querySelectorAll(selector) {
      return selector === '.lane[data-tab]' ? lanes : selector === '.lane-tab' ? tabs : [];
    }
  };
  vm.runInNewContext(source, {
    document, location: { pathname: '/b/main', hash: '' },
    sessionStorage: { getItem() { return saved; }, setItem() {} },
    MutationObserver: class { observe() {} }
  });
  const expected = saved === '2' ? '2' : '1';
  assert.deepEqual(lanes.filter(l => l.active()).map(l => l.dataset.tab), [expected]);
  assert.deepEqual(tabs.filter(l => l.active()).map(l => l.dataset.tab), [expected]);
  assert.equal(add.dataset.lane, expected);
}
console.log('PASS: saved tabs select an existing lane; stale or malformed tabs preserve the default');
