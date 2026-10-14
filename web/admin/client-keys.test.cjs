// 运行：node --test web/admin/client-keys.test.cjs（无需构建或 npm 依赖）。
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const source = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];

function harness(keys) {
  const elements = {clientKeyTable: {className: 'loading', innerHTML: '', textContent: ''}, addClientKey: {}};
  const context = vm.createContext({
    state: {locale: 'zh-CN', clientKeysLoadVersion: 0},
    $: id => elements[id] ||= {},
    request: async () => ({writable: true, client_api_keys: keys}),
    apiURL: value => value,
    esc: value => String(value ?? '').replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch])),
    t: (value, params = {}) => value.replace(/\{(\w+)\}/g, (_, key) => String(params[key] ?? '')),
    localizedHTML: (value, params = {}) => value.replace(/\{(\w+)\}/g, (_, key) => String(params[key] ?? '')),
    formatDate: value => value,
    translateDOM: () => {},
    document: {querySelectorAll: () => []},
  });
  const match = source.match(/^async function loadClientKeys\([\s\S]*?(?=\n(?:async )?function )/m);
  assert.ok(match, 'loadClientKeys exists');
  vm.runInContext(match[0], context);
  return {context, elements};
}

test('client key ID row renders escaped hint inline without placeholder for older keys', async () => {
  const {context, elements} = harness([
    {id: 'client-a', hint: 'sk_sx****ab', enabled: true, provider_access: {mode: 'all'}, effective_provider_ids: [], effective_model_count: 2},
    {id: 'older-key', enabled: true, provider_access: {mode: 'all'}, effective_provider_ids: [], effective_model_count: 0},
    {id: 'builtin-local', hint: 'sk_****zz', builtin: true, enabled: true, provider_access: {mode: 'all'}, effective_provider_ids: [], effective_model_count: 0},
  ]);
  assert.equal(await context.loadClientKeys(), true);
  const table = elements.clientKeyTable.innerHTML;
  assert.match(table, /client-a/);
  assert.match(table, /class="client-key-hint"[^>]*>sk_sx\*\*\*\*ab<\/span>/);
  assert.match(table, /class="client-key-id-value" title="client-a"/);
  assert.match(table, /title="older-key"/);
  assert.doesNotMatch(table.match(/<tr><td class="key-id" title="older-key"[\s\S]*?<\/tr>/)[0], /client-key-hint/);
  const builtinRow = table.match(/<tr><td class="key-id" title="builtin-local"[\s\S]*?<\/tr>/)[0];
  assert.match(builtinRow, /内置/);
  assert.doesNotMatch(builtinRow, /client-key-hint/);
});

test('client key ID and hint are independently HTML-escaped', async () => {
  const {context, elements} = harness([
    {id: '<img src=x onerror=alert(1)>', hint: '<b>hint</b>', enabled: true, provider_access: {mode: 'all'}, effective_provider_ids: [], effective_model_count: 0},
  ]);
  await context.loadClientKeys();
  const table = elements.clientKeyTable.innerHTML;
  assert.match(table, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(table, /&lt;b&gt;hint&lt;\/b&gt;/);
  assert.doesNotMatch(table, /<img src=x|<b>hint/);
});

test('client key columns adapt by viewport while retaining narrow-screen horizontal scrolling', () => {
  assert.match(html, /\.client-key-table\{min-width:920px\}/);
  assert.match(html, /@media\(max-width:1000px\)\{\.client-key-table\{min-width:860px\}/);
  assert.match(html, /@media\(max-width:760px\)\{\.client-key-table\{min-width:820px\}/);
  assert.match(html, /\.table-wrap\{[^}]*overflow:auto/);
  assert.match(html, /\.client-key-table \.link-btn\{padding:1px 4px/);
  assert.match(html, /\.client-key-identity\{[^}]*display:flex;[^}]*min-width:0/);
  assert.match(html, /\.client-key-id-value\{[^}]*min-width:0;[^}]*text-overflow:ellipsis;[^}]*white-space:nowrap/);
  assert.match(html, /\.client-key-hint\{flex:none;[^}]*font-size:11px;[^}]*font-weight:400;white-space:nowrap/);
  assert.match(html, /\.client-key-table th:nth-child\(1\)\{width:24%\}/);
  assert.match(html, /\.client-key-table th:nth-child\(1\)\{width:25%\}/);
});
