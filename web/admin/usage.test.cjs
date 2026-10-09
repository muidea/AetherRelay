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
  for (const name of ['formatNumber', 'compact', 'usageCacheRate', 'usageCacheTokens', 'usageCacheHint', 'usageTokensUnavailable', 'usageTokenValue', 'usageDurationSeconds', 'svgBars', 'svgStacked', 'renderCharts', 'usageTPS', 'usageTPSHint', 'renderKeyTable', 'eventStatusMeta', 'usageUpstreamMeta', 'usageParams', 'loadUsage', 'loadEvents', 'showUsageEvent']) {
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

test('observation presence preserves unknown, known zero and partial totals', () => {
  const {context:c,elements}=harness();
  assert.equal(c.usageCacheRate({input_tokens:100,cache_hit_rate:0,cached_input_tokens_known:false}), '—');
  assert.equal(c.usageCacheRate({input_tokens:100,cache_hit_rate:0,cached_input_tokens_known:true}), '0%');
  assert.equal(c.usageCacheRate({cache_input_tokens:0,input_tokens:100,cache_hit_rate:0,cached_input_tokens_known:true}), '—');
  assert.equal(c.usageCacheTokens({cached_input_tokens:0,cached_input_tokens_known:false},'cached_input_tokens'), '未提供');
  assert.equal(c.usageCacheTokens({cached_input_tokens:0,cached_input_tokens_known:true},'cached_input_tokens'), '0');
  assert.match(c.usageCacheTokens({cached_input_tokens:40,cached_input_tokens_known:false},'cached_input_tokens'), /40.*不完整/);
  assert.match(c.usageCacheHint({cache_input_tokens:100,input_tokens:125,cached_input_tokens:80,cached_input_tokens_known:true,cache_creation_input_tokens:0,cache_creation_input_tokens_known:true}), /输入 Token: 100/);
  c.showUsageEvent({upstream_status:200,upstream_content_length:0,upstream_content_length_known:true,conversion_level:2,conversion_duration_ms:0,conversion_degraded:false});
  assert.match(elements.usageEventDetail.innerHTML,/上游 Content-Length<\/dt><dd>0/);
  assert.match(elements.usageEventDetail.innerHTML,/转换等级<\/dt><dd>2/);
  assert.match(elements.usageEventDetail.innerHTML,/转换降级<\/dt><dd>false/);
  assert.match(elements.usageEventDetail.innerHTML,/转换耗时（秒）<\/dt><dd>0/);
  c.showUsageEvent({upstream_status:200,upstream_content_length:0,upstream_content_length_known:false});
  assert.match(elements.usageEventDetail.innerHTML,/上游 Content-Length<\/dt><dd>未知/);
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
  const event = {...usage, api_key_id: 'key-a', event_id: 'event-a', provider: 'codexoauth', model: 'gpt-5.6-sol', operation: 'responses', conversion_mode: 'codex_oauth_responses', conversion_duration_ms: 17, first_event_duration_ms: 82, duration_ms: 108, upstream_duration_ms: 7, http_status: 503, outcome: 'provider_unavailable', failure_class: 'accounts_cooling', retryable: true, retry_after_seconds: 5};
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
  assert.match(elements.usageEventDetail.innerHTML, /失败分类<\/dt><dd>accounts_cooling/);
  assert.match(elements.usageEventDetail.innerHTML, /可重试<\/dt><dd>是/);
  assert.match(elements.usageEventDetail.innerHTML, /建议等待（秒）<\/dt><dd>5/);
  c.renderCharts([]);
  assert.doesNotMatch(elements.chartCacheRate.innerHTML, /NaN|Infinity/);
  c.renderCharts([{date: '<unsafe>', input_tokens: 0, cache_hit_rate: 0}]);
  assert.doesNotMatch(elements.chartCacheRate.innerHTML, /<unsafe>/);
});

test('usage tables reserve stable widths and clip long cells', () => {
  assert.match(html, /\.usage-key-table\{min-width:1020px\}/);
  assert.match(html, /\.usage-event-table\{min-width:1220px\}/);
  assert.match(html, /\.usage-key-table th,.usage-key-table td,.usage-event-table th,.usage-event-table td\{overflow:hidden;text-overflow:ellipsis;white-space:nowrap\}/);
  assert.match(html, /@media\(max-width:1100px\)\{\.usage-event-table\{min-width:880px\}/);
  assert.match(html, /@media\(max-width:760px\)\{\.usage-event-table,.usage-key-table\{min-width:0\}/);
});

test('upstream column distinguishes admission rejection from missing response', () => {
  const {context: c} = harness();
  assert.equal(c.usageUpstreamMeta({outcome: 'provider_unavailable'}).label, '未发起');
  assert.equal(c.usageUpstreamMeta({outcome: 'provider_unavailable', failure_class: 'accounts_cooling', retry_after_seconds: 5}).title, '请求在路由或账号池准入阶段结束，未发起上游 HTTP 请求 · accounts_cooling · 建议等待 5 秒');
  assert.equal(c.usageUpstreamMeta({outcome: 'upstream_failed'}).label, '无响应');
  assert.equal(c.usageUpstreamMeta({outcome: 'first_event_timeout'}).label, '无响应');
  assert.equal(c.usageUpstreamMeta({outcome: 'first_event_timeout',upstream_status:200}).label, '200');
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

test('proxy cancellation and absent failed usage are not shown as successful zero usage', () => {
 const {context:c,elements}=harness();
 const e={http_status:499,outcome:'client_canceled',upstream_status:200,input_tokens:0,output_tokens:0,total_tokens:0,cached_input_tokens_known:false,cache_creation_input_tokens_known:false};
 assert.match(c.eventStatusMeta(e).label,/499.*下游连接取消/);
 assert.match(c.eventStatusMeta(e).title,/反向代理超时/);
 assert.match(c.usageUpstreamMeta(e).title,/不代表请求最终完成/);
 assert.equal(c.usageTokenValue(e,'input_tokens'),'未取得');
 c.showUsageEvent(e);
 assert.match(elements.usageEventDetail.innerHTML,/输入 Token<\/dt><dd>未取得/);
 assert.equal(c.usageTokenValue({...e,outcome:'success'},'input_tokens'),'0');
 assert.equal(c.usageTokenValue({...e,input_tokens:10},'input_tokens'),'10');
 assert.equal(c.usageTokenValue({...e,cached_input_tokens_known:true},'input_tokens'),'0');
});

test('TPS renders weighted server values, unknown and known zero with sample provenance', async () => {
  const {context:c,elements}=harness();
  assert.equal(c.usageTPS({}), '—');
  assert.equal(c.usageTPS({tps:null}), '—');
  assert.equal(c.usageTPS({tps:0}), '0.00');
  assert.equal(c.usageTPS({tps:25}), '25.00');
  assert.equal(c.usageTPS({tps:Infinity}), '—');
  const summary={tps:25,tps_samples:2,tps_estimated_samples:1,tps_partial_samples:1};
  const event={tps:30,generation_partial:true,estimated:true,generation_duration_ms:30000,first_output_at:'2026-10-09T00:00:00Z',event_id:'b',api_key_id:'key',state:'completed',outcome:'success'};
  c.request=async url=>url.includes('/dashboard?')?{summary,daily:[],by_api_key:[{...summary,api_key_id:'key'}]}:{events:[event]};
  await c.loadUsage();
  assert.equal(elements.uTPS.textContent,'25.00');
  assert.match(elements.uTPS.title,/有效样本 2.*估算 1.*部分输出 1/);
  assert.match(elements.keyTable.innerHTML,/>25\.00</);
  assert.match(elements.eventTable.innerHTML,/30\.00.*partial/);
  c.showUsageEvent(event);
  assert.match(elements.usageEventDetail.innerHTML,/TPS \(Token\/s\).*30\.00/);
  assert.match(elements.usageEventDetail.innerHTML,/生成耗时（秒）<\/dt><dd>30<\/dd>/);
});
