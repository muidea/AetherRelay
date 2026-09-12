// Run with: node --test web/admin/usage.test.cjs (no build or npm dependencies).
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync(`${__dirname}/index.html`, 'utf8');
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
const source = scripts.at(-1);

function harness() {
  const elements = {};
  const context = vm.createContext({
    state: {locale: 'zh-CN', range: 'today', events: []}, URLSearchParams,
    $: id => elements[id] ||= {showModal() {}},
    t: s => s, apiURL: s => s,
    formatDate: s => s,
    document: {querySelectorAll: () => []},
    toast: message => { throw new Error(message); },
  });
  vm.runInContext(source.match(/^const esc=.*$/m)[0], context);
  for (const name of ['formatNumber', 'compact', 'usageCacheRate', 'usageCacheHint', 'usageDurationSeconds', 'svgBars', 'svgStacked', 'renderCharts', 'renderKeyTable', 'eventStatusMeta', 'usageUpstreamMeta', 'usageParams', 'loadUsage', 'loadEvents', 'showUsageEvent']) {
    const match = source.match(new RegExp(`^(?:async )?function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function |\\n\\n// events)`, 'm'));
    assert.ok(match, `missing function ${name}`);
    // Some helpers share a line with other declarations; a fresh VM isolates tests.
    vm.runInContext(match[0], context);
  }
  return {context, elements};
}

test('all embedded scripts parse', () => {
  for (const script of scripts) new vm.Script(script);
});

test('cache rate distinguishes no input, missing data, misses and hits', () => {
  const {context: c} = harness();
  assert.equal(c.usageCacheRate({input_tokens: 0, cache_hit_rate: 0}), '—');
  assert.equal(c.usageCacheRate({input_tokens: 100}), '—');
  assert.equal(c.usageCacheRate({input_tokens: 100, cache_hit_rate: 0}), '0%');
  assert.equal(c.usageCacheRate({input_tokens: 1000, cache_hit_rate: 0.1}), '10%');
  assert.equal(c.usageCacheRate({input_tokens: 1000, cache_hit_rate: 0.19230769}), '19.2%');
  assert.equal(c.usageCacheRate({input_tokens: 100, cache_hit_rate: Infinity}), '—');
  assert.equal(c.usageCacheRate({input_tokens: 100, cache_hit_rate: 1.5}), '150%');
});

test('dashboard, chart, key table and events render server cache statistics', async () => {
  const {context: c, elements} = harness();
  const usage = {input_tokens: 1000, output_tokens: 20, total_tokens: 1020, cached_input_tokens: 100, cache_creation_input_tokens: 30, cache_hit_rate: 0.1};
  const event = {...usage, api_key_id: 'key-a', event_id: 'event-a', provider: 'codexoauth', model: 'gpt-5.6-sol', operation: 'responses', conversion_mode: 'codex_oauth_responses', conversion_duration_ms: 17, first_event_duration_ms: 82, duration_ms: 108, upstream_duration_ms: 7, http_status: 503, outcome: 'provider_unavailable'};
  c.request = async url => url.includes('/dashboard?')
    ? {summary: usage, daily: [{...usage, date: '2026-09-01'}], by_api_key: [event]}
    : {events: [event]};
  await c.loadUsage();
  assert.equal(elements.uCacheRate.textContent, '10%');
  assert.equal(elements.uCacheTokens.textContent, '100 / 30');
  assert.match(elements.uCacheRate.title, /100.*1,000.*30/);
  assert.match(elements.chartCacheRate.innerHTML, /2026-09-01: 10%/);
  assert.match(elements.chartCacheRate.innerHTML, /height="12.4"/); // 10% of the fixed 100% scale
  assert.match(elements.keyTable.innerHTML, /100 \/ 30/);
  assert.match(elements.keyTable.innerHTML, /class="usage-key-table"/);
  assert.match(elements.keyTable.innerHTML, />10%</);
  assert.match(elements.eventTable.innerHTML, /class="usage-event-table"/);
  assert.match(elements.eventTable.innerHTML, /title="codex_oauth_responses"/);
  assert.match(elements.eventTable.innerHTML, /title="provider_unavailable"/);
  assert.match(elements.eventTable.innerHTML, /title="in\/out\/total: 1,000\/20\/1,020"/);
  assert.match(elements.eventTable.innerHTML, />未发起<\/td>/);
  assert.match(elements.eventTable.innerHTML, /<th>耗时\(s\)<\/th>/);
  assert.match(elements.eventTable.innerHTML, /title="0.108">0.108<\/td>/);
  assert.match(elements.eventTable.innerHTML, />10%</);
  c.showUsageEvent(event);
  assert.match(elements.usageEventDetail.innerHTML, /缓存使用率<\/dt><dd>10%/);
  assert.match(elements.usageEventDetail.innerHTML, /转换耗时（秒）<\/dt><dd>0.017/);
  assert.match(elements.usageEventDetail.innerHTML, /首事件耗时（秒）<\/dt><dd>0.082/);
  assert.match(elements.usageEventDetail.innerHTML, /总耗时（秒）<\/dt><dd>0.108/);
  assert.match(elements.usageEventDetail.innerHTML, /上游响应头耗时（秒）<\/dt><dd>0.007/);
  c.renderCharts([]);
  assert.doesNotMatch(elements.chartCacheRate.innerHTML, /NaN|Infinity/);
  c.renderCharts([{date: '<unsafe>', input_tokens: 0, cache_hit_rate: 0}]);
  assert.doesNotMatch(elements.chartCacheRate.innerHTML, /<unsafe>/);
});

test('usage tables reserve stable widths and clip long cells', () => {
  assert.match(html, /\.usage-key-table\{min-width:920px\}/);
  assert.match(html, /\.usage-event-table\{min-width:1120px\}/);
  assert.match(html, /\.usage-key-table th,.usage-key-table td,.usage-event-table th,.usage-event-table td\{overflow:hidden;text-overflow:ellipsis;white-space:nowrap\}/);
  assert.match(html, /@media\(max-width:1100px\)\{\.usage-event-table\{min-width:880px\}/);
  assert.match(html, /@media\(max-width:760px\)\{\.usage-event-table,.usage-key-table\{min-width:0\}/);
});

test('upstream column distinguishes admission rejection from missing response', () => {
  const {context: c} = harness();
  assert.equal(c.usageUpstreamMeta({outcome: 'provider_unavailable'}).label, '未发起');
  assert.equal(c.usageUpstreamMeta({outcome: 'upstream_failed'}).label, '无响应');
  assert.equal(c.usageUpstreamMeta({upstream_status: 429, upstream_content_type: 'application/json'}).label, '429 · application/json');
});

test('usage durations are consistently displayed in seconds', () => {
  const {context: c} = harness();
  assert.equal(c.usageDurationSeconds(0), '0');
  assert.equal(c.usageDurationSeconds(82), '0.082');
  assert.equal(c.usageDurationSeconds(901316), '901.316');
  assert.equal(c.usageDurationSeconds(null), '—');
  assert.equal(c.usageDurationSeconds(-1), '—');
});
