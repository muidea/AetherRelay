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
    state: {locale: 'zh-CN', range: 'today', events: []}, URLSearchParams, AbortController, setTimeout, clearTimeout,
    $: id => elements[id] ||= {style: {}, classList:{toggle(){}}, showModal() {}},
    t: (s,params={}) => s.replace(/\{(\w+)\}/g,(_,key)=>String(params[key]??'')), apiURL: s => s,
    setLocalizedText: (el,s,params={}) => {el.textContent=s.replace(/\{(\w+)\}/g,(_,key)=>String(params[key]??''));},
    setLocalizedAttribute: (el,name,source) => {el[name]=typeof source==='function'?source():source;},
    formatDate: s => s,
    document: {querySelectorAll: () => []},
    toast: message => { throw new Error(message); },
  });
  vm.runInContext(source.match(/^const esc=.*$/m)[0], context);
  for (const name of ['formatNumber', 'compact', 'usageCardHint', 'usageCacheRate', 'usageCacheTokens', 'usageCacheHint', 'usageTokensUnavailable', 'usageTokenValue', 'usageDurationSeconds', 'svgBars', 'svgStacked', 'renderCharts', 'usageTPS', 'usageTPSHint', 'renderKeyTable', 'eventStatusMeta', 'usageUpstreamMeta', 'usageParams', 'loadUsageRuntime', 'loadUsage', 'renderUsageEventTable', 'renderUsageEvents', 'mergeUsageEvents', 'usageEventMatchesFilters', 'refreshUsageEvents', 'loadEvents', 'usageAccountLabel','formatUsageDate','usageInputISO','usageInputValue','usageCause','usageCauseLabel','usageEvidenceLabel','usageReplayLabel','setUsageTraceTab','renderUsageTrace','renderUsageEventFields', 'showUsageEvent']) {
    const match = source.match(new RegExp(`^(?:async )?function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function |\\n\\n// events)`, 'm'));
    assert.ok(match, `missing function ${name}`);
    // Some helpers share a line with other declarations; a fresh VM isolates tests.
    vm.runInContext(match[0], context);
  }
  context.loadUsageTrace=async()=>{};context.loadUsageDiagnostics=async()=>{};
  return {context, elements};
}

test('all embedded scripts parse', () => {
  for (const script of scripts) new vm.Script(script);
});

test('current concurrency reads a separate live endpoint independently of historical filters', async () => {
  const {context:c,elements}=harness();
  c.state.range='30d';c.state.apiKeyFilter='other';c.state.providerFilter='other';
  const urls=[];
  c.request=async (url,options)=>{urls.push(url);assert.ok(options.signal);return {scope:'process',inflight:12};};
  await c.loadUsageRuntime();
  assert.deepEqual(urls,['/api/usage/runtime']);
  assert.equal(elements.uInflight.textContent,'12');
  assert.equal(elements.uInflight.title,'当前并发：12');
  c.request=async()=>({scope:'process',inflight:0});
  await c.loadUsageRuntime();
  assert.equal(elements.uInflight.textContent,'0');
});

test('current concurrency clears stale values on failure or invalid data', async () => {
  const {context:c,elements}=harness();
  c.request=async()=>({scope:'process',inflight:5});
  await c.loadUsageRuntime();
  for(const value of [null,{}, {scope:'process',inflight:null}, {scope:'process',inflight:-1}, {scope:'process',inflight:1.5}, {scope:'process',inflight:'2'}]){
    c.request=async()=>value;
    await c.loadUsageRuntime();
    assert.equal(elements.uInflight.textContent,'—');
    assert.equal(elements.uInflight.title,'当前并发数据不可用');
    assert.equal(c.state.usageRuntimeBusy,false);
  }
  c.request=async()=>{throw new Error('offline')};
  await c.loadUsageRuntime();
  assert.equal(elements.uInflight.textContent,'—');
});

test('current concurrency polling avoids overlapping requests', async () => {
  const {context:c,elements}=harness();
  let finish,calls=0;
  c.request=()=>{calls++;return new Promise(resolve=>{finish=resolve})};
  const pending=c.loadUsageRuntime();
  await c.loadUsageRuntime();
  assert.equal(calls,1);
  finish({scope:'process',inflight:3});
  await pending;
  assert.equal(elements.uInflight.textContent,'3');
  assert.equal(c.state.usageRuntimeBusy,false);
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
  assert.match(elements.usageEventDetail.innerHTML,/上游 Content-Length<\/dt><dd[^>]*>0/);
  assert.match(elements.usageEventDetail.innerHTML,/转换等级<\/dt><dd[^>]*>2/);
  assert.match(elements.usageEventDetail.innerHTML,/转换降级<\/dt><dd[^>]*>false/);
  assert.match(elements.usageEventDetail.innerHTML,/转换耗时（秒）<\/dt><dd[^>]*>0/);
  c.showUsageEvent({upstream_status:200,upstream_content_length:0,upstream_content_length_known:false});
  assert.match(elements.usageEventDetail.innerHTML,/上游 Content-Length<\/dt><dd[^>]*>未知/);
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
  c.request = async url => url==='/api/usage/runtime' ? {scope:'process',inflight:4} : url.includes('/dashboard?')
    ? {summary: usage, daily: [{...usage, date: '2026-09-01'}], by_api_key: [event]}
    : {events: [event]};
  c.$('keyTable').className='loading';
  await c.loadUsage();
	assert.equal(elements.uInflight.textContent, '4');
  assert.equal(elements.keyTable.className, '');
  assert.equal(elements.uCacheRate.textContent, '10%');
  assert.equal(elements.uCacheTokens.textContent, '100 / 30');
  assert.equal(elements.uCacheRate.title, '缓存使用率：10%');
  assert.equal(elements.uCacheTokens.title, '缓存读取 Token：100\n缓存创建 Token：30');
  assert.match(elements.chartCacheRate.innerHTML, /2026-09-01 · 缓存使用率：10%/);
  assert.match(elements.chartCacheRate.innerHTML, /height="12.4"/); // 10% of the fixed 100% scale
  assert.match(elements.keyTable.innerHTML, /100 \/ 30/);
  assert.match(elements.keyTable.innerHTML, /class="usage-key-table"/);
  assert.match(elements.keyTable.innerHTML, />10%</);
  assert.match(elements.eventTable.innerHTML, /class="usage-event-table"/);
  assert.doesNotMatch(elements.eventTable.innerHTML, /codex_oauth_responses/);
  assert.match(elements.eventTable.innerHTML, /title="provider_unavailable"/);
  assert.match(elements.eventTable.innerHTML, /title="in\/out\/total: 1,000\/20\/1,020"/);
  assert.doesNotMatch(elements.eventTable.innerHTML, /<th[^>]*>上游响应头<\/th>/);
  assert.match(elements.eventTable.innerHTML, /<th scope="col">耗时\(s\)<\/th>/);
  assert.match(elements.eventTable.innerHTML, /title="0.108"><div[^>]*>0.108<\/div>/);
  assert.match(elements.eventTable.innerHTML, />10%</);
  c.showUsageEvent(event);
  assert.match(elements.usageEventDetail.innerHTML, /缓存使用率<\/dt><dd[^>]*>10%/);
  assert.match(elements.usageEventDetail.innerHTML, /转换耗时（秒）<\/dt><dd[^>]*>0.017/);
  assert.match(elements.usageEventDetail.innerHTML, /首事件耗时（秒）<\/dt><dd[^>]*>0.082/);
  assert.match(elements.usageEventDetail.innerHTML, /总耗时（秒）<\/dt><dd[^>]*>0.108/);
  assert.match(elements.usageEventDetail.innerHTML, /上游响应头耗时（秒）<\/dt><dd[^>]*>0.007/);
  assert.match(elements.usageEventDetail.innerHTML, /失败分类<\/dt><dd[^>]*>accounts_cooling/);
  assert.match(elements.usageEventDetail.innerHTML, /可重试<\/dt><dd[^>]*>是/);
  assert.match(elements.usageEventDetail.innerHTML, /建议等待（秒）<\/dt><dd[^>]*>5/);
  c.renderCharts([]);
  assert.doesNotMatch(elements.chartCacheRate.innerHTML, /NaN|Infinity/);
  c.renderCharts([{date: '<unsafe>', input_tokens: 0, cache_hit_rate: 0}]);
  assert.doesNotMatch(elements.chartCacheRate.innerHTML, /<unsafe>/);
});

test('usage event list keeps complete values and diagnostic fields in the dialog', () => {
  const {context:c,elements}=harness();
  const event={event_id:'0123456789abcdef',started_at:'2026-10-09T07:00:00Z',api_key_id:'a-very-long-key-name',model:'a-very-long-model-name',provider:'codexoauth',operation:'responses',input_tokens:202648,output_tokens:1231,total_tokens:203879,tps:123456.78,http_status:502,outcome:'upstream_failed',conversion_mode:'anthropic_to_codex_responses',upstream_status:200};
  const table=c.renderUsageEventTable([event]);
  const headers=[...table.matchAll(/<th scope="col"[^>]*>([^<]*)<\/th>/g)].map(m=>m[1]);
  assert.deepEqual(headers,['时间','Key','Model','Op','Tokens','缓存使用率','TPS','最终状态','异常原因','账号 / 尝试','耗时(s)']);
  assert.equal((table.match(/data-label=/g)||[]).length,11);
  assert.match(table,/<th scope="col" title="输入 Token \/ 输出 Token \/ 总 Token">Tokens<\/th>/);
  for(const value of [event.api_key_id,event.model,event.operation,'202,648','1,231','203,879','123456.78','upstream_failed'])assert.ok(table.includes(value),value);
  assert.doesNotMatch(table,/text-overflow:ellipsis|0123456789abcdef|anthropic_to_codex_responses/);
  assert.match(table,/tabindex="0"/);
  c.showUsageEvent(event);
  assert.match(elements.usageEventDetail.innerHTML,/Event ID<\/dt><dd[^>]*>0123456789abcdef/);
  assert.match(elements.usageEventDetail.innerHTML,/转换模式<\/dt><dd[^>]*>anthropic_to_codex_responses/);
  assert.match(elements.usageEventDetail.innerHTML,/上游 HTTP 状态<\/dt><dd[^>]*>200/);
  assert.ok(c.renderUsageEventTable([{...event,api_key_id:'<script>bad</script>'}]).includes('&lt;script&gt;bad&lt;/script&gt;'));
});

test('usage events remain keyboard accessible after loading', async () => {
  const {context:c,elements}=harness();
  const row={dataset:{event:'0'},classList:{toggle(){}},setAttribute(){}};
  c.document.querySelectorAll=()=>[row];
  const event={event_id:'keyboard-event',http_status:200};
  c.request=async()=>({events:[event]});
  c.$('eventTable').className='loading';
  await c.loadEvents(true);
  assert.equal(elements.eventTable.className,'');
  let opened=0,prevented=0;
  elements.usageEventDialog={showModal(){opened++}};
  for(const key of ['Enter',' ','Escape'])row.onkeydown({key,preventDefault(){prevented++}});
  row.onclick();
  assert.equal(opened,3);
  assert.equal(prevented,2);
  assert.match(elements.usageEventDetail.innerHTML,/keyboard-event/);
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

test('final status reflects the outcome after HTTP headers were committed', () => {
 const {context:c}=harness();
 const failed=['upstream_truncated','upstream_failed','incomplete','client_canceled','client_write','request_timeout','first_event_timeout','idle_timeout','stream_lifetime_timeout','service_shutdown','conversion','protocol','error','unknown_future_failure','constructor','__proto__'];
 for(const outcome of failed){
  const result=c.eventStatusMeta({http_status:200,state:'completed',outcome});
  assert.doesNotMatch(result.label,/成功/);
  assert.notEqual(result.className,'status-2xx');
  assert.match(result.label,/^200 /);
 }
 assert.match(c.eventStatusMeta({http_status:200,outcome:'upstream_truncated'}).label,/输出中断/);
 assert.match(c.eventStatusMeta({http_status:200,outcome:'upstream_truncated'}).title,/不代表请求最终完成/);
 assert.match(c.eventStatusMeta({http_status:504,outcome:'request_timeout'}).label,/504 请求超时/);
 assert.match(c.eventStatusMeta({http_status:200,outcome:'service_shutdown'}).label,/服务停机中断/);
 assert.equal(c.usageCauseLabel('service_shutdown'),'服务停机中断');
 assert.match(c.eventStatusMeta({http_status:503,outcome:'provider_unavailable',failure_class:'accounts_cooling',retry_after_seconds:10}).title,/建议等待 10 秒/);
 assert.match(c.eventStatusMeta({http_status:200,outcome:'success'}).label,/成功/);
 assert.match(c.eventStatusMeta({http_status:200}).label,/成功/);
 assert.equal(c.eventStatusMeta({state:'started'}).label,'待完成');
 assert.doesNotMatch(c.eventStatusMeta({http_status:503,outcome:'success'}).label,/成功/);
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
 assert.match(elements.usageEventDetail.innerHTML,/输入 Token<\/dt><dd[^>]*>未取得/);
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
  assert.equal(elements.uTPS.title,'TPS：25.00 Token/s');
  assert.match(elements.keyTable.innerHTML,/>25\.00</);
  assert.match(elements.eventTable.innerHTML,/>30\.00</);
  assert.doesNotMatch(elements.eventTable.innerHTML,/>partial</);
  c.showUsageEvent(event);
  assert.match(elements.usageEventDetail.innerHTML,/TPS \(Token\/s\).*30\.00/);
  assert.match(elements.usageEventDetail.innerHTML,/观测输出阶段耗时（秒）<\/dt><dd[^>]*>30<\/dd>/);
});

test('TPS trend uses daily weighted values and distinguishes missing days from known zero', async () => {
  const {context:c,elements}=harness();
  assert.match(html,/TPS 趋势（Token\/s）.*id="chartTPS"/);
  const daily=[
    {date:'2026-10-08',tps:null,tps_samples:0,output_tokens:5000},
    {date:'2026-10-09',tps:25,tps_samples:2,tps_output_tokens:1000,tps_duration_ms:40000,tps_estimated_samples:1,tps_partial_samples:1,output_tokens:6000},
    {date:'2026-10-10',tps:0,tps_samples:1},
    {date:'2026-10-11',tps:null,tps_samples:0},
  ];
  c.request=async url=>url.includes('/dashboard?')?{summary:{},daily,by_api_key:[]}:{events:[]};
  c.state.apiKeyFilter='key-a';
  let requested='';
  const request=c.request;
  c.request=async url=>{requested+=url;return request(url)};
  await c.loadUsage();
  assert.match(requested,/api_key_id=key-a/);
  const svg=elements.chartTPS.innerHTML;
  assert.match(svg,/2026-10-09 · TPS：25\.00 Token\/s/);
  assert.match(svg,/<rect[^>]+height="124"/);
  assert.equal((svg.match(/<rect /g)||[]).length,1);
  assert.equal((svg.match(/<text /g)||[]).length,2);
  assert.match(svg,/<circle[^>]*><title>2026-10-10 · TPS：0\.00 Token\/s/);
  assert.match(svg,/2026-10-08 · TPS：—/);
  assert.doesNotMatch(svg,/NaN|Infinity/);
  c.renderCharts([{date:'<unsafe>',tps:Infinity},{date:'bad',tps:-1}]);
  assert.doesNotMatch(elements.chartTPS.innerHTML,/<unsafe>|NaN|Infinity|<rect /);
  c.renderCharts([]);
  assert.doesNotMatch(elements.chartTPS.innerHTML,/NaN|Infinity/);
});


test('cards keep concise TPS values while buffered diagnostics remain in events and keys', async () => {
 const {context:c,elements}=harness();
 const stats={tps:32.89,observed_tps:127784.6,tps_samples:2,observed_tps_buffered_samples:1};
 const event={event_id:'buffered',api_key_id:'key',outcome:'success',http_status:200,tps:32.89,observed_tps:127784.6,generation_buffered:true,generation_partial:false};
 c.request=async url=>url.includes('/dashboard?')?{summary:stats,by_api_key:[{...stats,api_key_id:'key'}],daily:[{...stats,date:'2026-10-09'}]}:{events:[event]};
 await c.loadUsage();
 assert.equal(elements.uTPS.textContent,'32.89');
 assert.equal(elements.uTPS.title,'TPS：32.89 Token/s');
 assert.match(elements.keyTable.innerHTML,/上游缓冲 1/);
 assert.match(elements.chartTPS.innerHTML,/2026-10-09 · TPS：32\.89 Token\/s/);
 assert.doesNotMatch(elements.eventTable.innerHTML,/>buffered</);
 assert.match(elements.eventTable.innerHTML,/上游明确缓冲/);
 c.showUsageEvent(event);
 assert.match(elements.usageEventDetail.innerHTML,/上游明确缓冲<\/dt><dd[^>]*>true/);
 assert.doesNotMatch(c.usageTPSHint({tps:10}),/上游明确缓冲/);
});


test('E2E and buffered observed output rates are distinct in list and detail', () => {
 const {context:c,elements}=harness();
 const event={event_id:'x600-041517',outcome:'success',http_status:200,output_tokens:536,duration_ms:16296,tps:536/16.296,observed_tps:536/.093580012,generation_duration_ms:93.580012,generation_buffered:true,output_tokens_known:true};
 const table=c.renderUsageEventTable([event]);
 assert.match(table,/>TPS</);
 assert.match(table,/>32\.89</);
 assert.doesNotMatch(table,/>5727\.72</);
 c.showUsageEvent(event);
 assert.match(elements.usageEventDetail.innerHTML,/TPS \(Token\/s\)<\/dt><dd[^>]*>32\.89/);
 assert.match(elements.usageEventDetail.innerHTML,/观测输出 TPS \(Token\/s\)<\/dt><dd[^>]*>5727\.72/);
 assert.match(elements.usageEventDetail.innerHTML,/观测输出阶段耗时（秒）<\/dt><dd[^>]*>0\.094/);
 assert.match(c.usageTPSHint(event),/请求总耗时，含首输出等待/);
 assert.equal(c.usageTPS({tps:null,observed_tps:5727.72}), '—');
});


test('known zero output on canceled requests remains observable', () => {
 const {context:c}=harness();
 const e={state:'completed',outcome:'client_canceled',input_tokens:0,output_tokens:0,total_tokens:0,cached_input_tokens_known:false,cache_creation_input_tokens_known:false,output_tokens_known:true,tps:0};
 assert.equal(c.usageTokensUnavailable(e),false);
 assert.equal(c.usageTokenValue(e,'output_tokens'),'0');
 assert.equal(c.usageTPS(e),'0.00');
 assert.match(c.usageTPSHint(e),/部分输出/);
});

// CP-OBS-013: actual target is visible in a single-event dialog and escaped.
test('event dialog shows provider and historical account snapshot safely', () => {
 const {context:c,elements}=harness();
 c.showUsageEvent({provider:'codexoauth',account_id:'slot-id',account_label:'operator<admin>@example.com'});
 const detail=elements.usageEventDetail.innerHTML;
 assert.match(detail, /<dt>实际 Provider<\/dt><dd[^>]*>codexoauth<\/dd>/);
 assert.match(detail, /operator&lt;admin&gt;@example.com/);
 assert.match(detail, /<dt>账号槽位 ID<\/dt><dd[^>]*>slot-id<\/dd>/);
 assert.ok(!detail.includes('operator<admin>'));
 c.showUsageEvent({provider:'chatgptweb'});
 assert.match(elements.usageEventDetail.innerHTML, /<dt>账号<\/dt><dd[^>]*>未记录<\/dd>/);
 c.showUsageEvent({provider:'custom-provider'});
 assert.match(elements.usageEventDetail.innerHTML, /<dt>账号<\/dt><dd[^>]*>不适用<\/dd>/);
 c.showUsageEvent({provider:'codexoauth',account_id:'unlabelled-slot'});
 assert.match(elements.usageEventDetail.innerHTML, /<dt>账号<\/dt><dd[^>]*>unlabelled-slot<\/dd>/);
});

test('localized event counts retain the load-more hint when a cursor is present',async()=>{
  const {context:c,elements}=harness();
  c.request=async()=>({events:[{event_id:'page-1',http_status:200,outcome:'success'}],next_cursor:'page-2'});
  await c.loadEvents(true);
  assert.equal(elements.eventMeta.textContent,'1 条（可加载更多）');assert.equal(elements.loadMoreEvents.disabled,false);
  c.request=async()=>({events:[{event_id:'page-2',http_status:200,outcome:'success'}]});
  await c.loadEvents(false);
  assert.equal(elements.eventMeta.textContent,'2 条');assert.equal(elements.loadMoreEvents.disabled,true);
});

test('Codex substitution shows response evidence and account while success stays successful',()=>{
  const {context:c,elements}=harness();
  const event={provider:'codexoauth',model:'gpt-6.1-sol',upstream_request_model:'gpt-6.1-sol',response_model:'gpt-6-luna<script>',model_substituted:true,account_label:'operator@example.com',account_id:'local-slot',outcome:'success',http_status:200};
  const table=c.renderUsageEventTable([event]);
  assert.match(table,/上游模型替换/);assert.match(table,/gpt-6-luna&lt;script&gt;/);assert.match(table,/200.*成功/);assert.doesNotMatch(table,/<script>/);
  const cells=[...table.matchAll(/<td\b[^>]*>([\s\S]*?)<\/td>/g)].map(match=>match[1]);
  assert.equal(cells.length,11);assert.doesNotMatch(cells[2],/上游模型替换|gpt-6-luna/);
  assert.match(cells[7],/200 成功/);assert.match(cells[7],/title="gpt-6.1-sol → gpt-6-luna&lt;script&gt;"/);
  assert.match(cells[7],/>上游模型替换<\/div>/);assert.doesNotMatch(cells[7].replace(/<[^>]*>/g,''),/gpt-6-luna|gpt-6.1-sol/);
  c.showUsageEvent(event);const detail=elements.usageEventDetail.innerHTML;
  assert.equal(elements.usageEventModelNotice.style.display,'block');assert.match(elements.usageEventModelNotice.textContent,/gpt-6.1-sol → gpt-6-luna<script>/);assert.match(elements.usageEventModelNotice.textContent,/operator@example.com（槽位：local-slot）/);assert.match(elements.usageEventModelNotice.textContent,/不代表账号健康异常/);
  assert.match(detail,/上游请求模型<\/dt><dd[^>]*>gpt-6.1-sol/);assert.match(detail,/响应模型<\/dt><dd[^>]*>gpt-6-luna&lt;script&gt;/);assert.match(detail,/operator@example.com/);assert.match(detail,/local-slot/);
  c.showUsageEvent({provider:'codexoauth'});assert.match(elements.usageEventDetail.innerHTML,/响应模型<\/dt><dd[^>]*>未记录/);
  c.showUsageEvent({provider:'chatgptweb'});assert.match(elements.usageEventDetail.innerHTML,/响应模型<\/dt><dd[^>]*>不适用/);
  assert.doesNotMatch(c.renderUsageEventTable([{...event,provider:'chatgptweb'}]),/上游模型替换/);
  for(const current of [{...event,model_substituted:false},{...event,provider:'chatgptweb'},{provider:'codexoauth'}]){c.showUsageEvent(current);assert.equal(elements.usageEventModelNotice.style.display,'none');assert.equal(elements.usageEventModelNotice.textContent,'');}
});

// CP-OBS-016: pending status refresh and local rejection diagnostics.
test('automatic refresh completes loaded pending rows and the open detail without resetting pagination',async()=>{
 const {context:c,elements}=harness();
 c.state.apiKeyFilter='thought-flow';
 const started={event_id:'pending',state:'started',api_key_id:'thought-flow',model:'',started_at:'2026-10-23T10:46:05Z'};
 const older={event_id:'older',state:'completed',api_key_id:'thought-flow',http_status:200,outcome:'success'};
 c.request=async()=>({events:[started,older],next_cursor:'older-page'});
 await c.loadEvents(true);c.showUsageEvent(started);elements.usageEventDialog.open=true;
 const completed={...started,state:'completed',model:'gpt-6',http_status:200,outcome:'success',duration_ms:28925,input_tokens:2940,output_tokens:1922,total_tokens:4862};
 const urls=[];c.request=async url=>{urls.push(url);return {events:[completed]}};
 await c.refreshUsageEvents();
 assert.deepEqual(Array.from(c.state.events,e=>e.event_id),['pending','older']);
 assert.equal(c.state.cursor,'older-page');assert.equal(c.state.apiKeyFilter,'thought-flow');
 assert.equal(c.state.events[0].model,'gpt-6');assert.match(elements.eventTable.innerHTML,/200 成功/);
 assert.doesNotMatch(elements.eventTable.innerHTML,/待完成/);assert.match(elements.usageEventDetail.innerHTML,/4,862|4862/);
 assert.equal(c.state.usageEvent.http_status,200);
 const params=new URLSearchParams(urls[0].split('?')[1]);
 assert.deepEqual(params.getAll('event_id'),['pending']);assert.equal(params.get('range'),'all');
 await c.refreshUsageEvents();assert.equal(urls.length,1);
});

test('automatic dashboard tick refreshes pending events even if the dashboard fails',async()=>{
 const {context:c,elements}=harness();
 const pending={event_id:'failed',state:'started'};
 c.request=async()=>({events:[pending],next_cursor:'keep'});await c.loadEvents(true);
 c.toast=()=>{};
 c.request=async url=>{
  if(url.includes('/runtime'))return {scope:'process',inflight:0};
  if(url.includes('/dashboard'))throw new Error('dashboard unavailable');
  return {events:[{...pending,state:'completed',http_status:400,outcome:'error',error_code:'conversion_unsupported',failure_class:'conversion_unsupported',unsupported_features:['response_format']}]};
 };
 await c.loadUsage(false);await new Promise(resolve=>setImmediate(resolve));
 assert.equal(c.state.events[0].state,'completed');assert.match(elements.eventTable.innerHTML,/400 调用失败/);assert.equal(c.state.cursor,'keep');
});

test('late refreshes cannot overwrite a new filter or reset and concurrent ticks do not overlap',async()=>{
 const {context:c}=harness();
 c.request=async()=>({events:[{event_id:'old',state:'started'}]});await c.loadEvents(true);
 let finish,calls=0;c.request=()=>{calls++;return new Promise(resolve=>finish=resolve)};
 const refreshing=c.refreshUsageEvents();await c.refreshUsageEvents();assert.equal(calls,1);
 c.state.apiKeyFilter='new-key';c.request=async()=>({events:[{event_id:'new',state:'completed',api_key_id:'new-key'}],next_cursor:'new-page'});
 await c.loadEvents(true);finish({events:[{event_id:'old',state:'completed',http_status:200}]});await refreshing;
 assert.deepEqual(Array.from(c.state.events,e=>e.event_id),['new']);assert.equal(c.state.cursor,'new-page');
 assert.equal(c.state.eventsRefreshBusy,false);
});

test('pagination deduplicates IDs and stale started snapshots do not regress completed rows',async()=>{
 const {context:c}=harness();
 c.request=async()=>({events:[{event_id:'same',state:'completed',http_status:200}],next_cursor:'next'});await c.loadEvents(true);
 c.request=async()=>({events:[{event_id:'same',state:'started'},{event_id:'older',state:'completed',http_status:400}]});await c.loadEvents(false);
 assert.equal(c.state.events.length,2);assert.equal(c.state.events[0].http_status,200);
});

test('old page and dashboard responses cannot replace a newer manual refresh',async()=>{
 const {context:c,elements}=harness();
 const finishes=[];c.request=()=>new Promise(resolve=>finishes.push(resolve));
 const old=c.loadEvents(true);const fresh=c.loadEvents(true);
 finishes[1]({events:[{event_id:'fresh',state:'completed'}]});await fresh;
 finishes[0]({events:[{event_id:'old',state:'started'}]});await old;
 assert.equal(c.state.events[0].event_id,'fresh');
 c.loadUsageRuntime=()=>{};c.loadEvents=async()=>{};
 const dashboards=[];c.request=()=>new Promise(resolve=>dashboards.push(resolve));
 const oldDashboard=c.loadUsage(),newDashboard=c.loadUsage();
 dashboards[1]({summary:{requests:20}});await newDashboard;
 dashboards[0]({summary:{requests:1}});await oldDashboard;
 assert.equal(elements.uRequests.textContent,'20');
});

test('pending refreshes use bounded batches, retain data on failure, and respect active result filters',async()=>{
 const {context:c}=harness();
 c.state.events=Array.from({length:105},(_,i)=>({event_id:`id-${i}`,state:'started',provider:'chatgptweb'}));
 c.state.eventsFilterKey=c.usageParams().toString();c.state.cursor='preserved';
 const urls=[];c.request=async url=>{urls.push(url);const ids=new URLSearchParams(url.split('?')[1]).getAll('event_id');return {events:ids.map(event_id=>({event_id,state:'completed',provider:'chatgptweb',http_status:200,outcome:'success'}))}};
 await c.refreshUsageEvents();assert.equal(urls.length,2);
 assert.equal(new URLSearchParams(urls[0].split('?')[1]).getAll('event_id').length,100);assert.equal(c.state.events.length,105);
 c.state.events=[{event_id:'still-pending',state:'started'}];c.request=async()=>{throw new Error('offline')};
 await c.refreshUsageEvents();assert.equal(c.state.events[0].state,'started');assert.equal(c.state.eventsRefreshBusy,false);
 c.state.modelFilter='auto';c.state.events[0].model='auto';c.state.eventsFilterKey=c.usageParams().toString();
 c.request=async()=>({events:[{event_id:'still-pending',model:'gpt-6',state:'completed',http_status:200}]});
 await c.refreshUsageEvents();assert.equal(c.state.events.length,0);assert.equal(c.state.cursor,'preserved');
});

test('Codex overload details retain the cause, partial outcome and unknown token usage',()=>{
  const {context:c,elements}=harness();
  const event={provider:'codexoauth',http_status:200,upstream_status:101,outcome:'upstream_truncated',error_code:'upstream',failure_class:'upstream_overloaded',retryable:false,generation_partial:true,output_tokens_known:false,cached_input_tokens_known:false,cache_creation_input_tokens_known:false,input_tokens:0,output_tokens:0,total_tokens:0,upstream_error_type:'service_unavailable_error',upstream_error_code:'server_is_overloaded',upstream_error_message:'Our servers are currently overloaded. Please try again later.'};
  c.showUsageEvent(event);
  const out=elements.usageEventDetail.innerHTML;
  for(const value of ['server_is_overloaded','service_unavailable_error','上游服务过载',event.upstream_error_message,'upstream_truncated','未取得'])assert.ok(out.includes(value),value);
  assert.match(out,/错误代码<\/dt><dd[^>]*>upstream<\/dd>/);
  assert.match(out,/可重试<\/dt><dd[^>]*>否<\/dd>/);
  assert.match(out,/部分输出<\/dt><dd[^>]*>true<\/dd>/);
  assert.match(c.eventStatusMeta(event).label,/200.*输出中断/);
  c.showUsageEvent({...event,upstream_error_code:'<unsafe>',upstream_error_message:'<img src=x onerror=alert(1)>'});
  assert.match(elements.usageEventDetail.innerHTML,/&lt;img/);
  assert.doesNotMatch(elements.usageEventDetail.innerHTML,/<img|上游服务过载/);
  c.showUsageEvent({error_code:'upstream',http_status:200,outcome:'upstream_truncated'});
  assert.match(elements.usageEventDetail.innerHTML,/上游错误代码<\/dt><dd[^>]*>—<\/dd>/);
  assert.doesNotMatch(elements.usageEventDetail.innerHTML,/上游服务过载/);
});

function installUsageFunctions(context,names){
 for(const name of names){const match=source.match(new RegExp(`^(?:async )?function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function |\\n\\n// events)`,'m'));assert.ok(match,name);vm.runInContext(match[0],context);}
}

test('diagnostic dates and custom ranges always use UTC+8 regardless of browser timezone',()=>{
 const {context:c}=harness();const old=process.env.TZ;
 try{for(const zone of ['UTC','America/Los_Angeles','Asia/Singapore']){process.env.TZ=zone;assert.equal(c.usageInputISO('2026-10-30T00:00'),'2026-10-29T16:00:00.000Z');assert.equal(c.usageInputValue('2026-10-29T16:00:00Z'),'2026-10-30T00:00');assert.equal(c.usageParams().get('timezone'),'Asia/Singapore');}}
 finally{if(old===undefined)delete process.env.TZ;else process.env.TZ=old;}
});

test('diagnostic rendering preserves missing observations and escapes evidence',()=>{
 const {context:c,elements}=harness();
 const event={event_id:'event',state:'completed',http_status:200,outcome:'upstream_truncated',generation_partial:true};
 const trace={total_attempts:1,context:{request_id:'request'},attempts:[{id:1,failed:true,finished:true,event_count:0,wire_bytes:0,upstream_error_message:'<img src=x onerror=alert(1)>'}]};
 c.renderUsageTrace({event,trace,evidence:[{field:'raw_error_body',state:'not_collected'}]});
 assert.doesNotMatch(elements.usageTraceOverview.innerHTML,/0 \/ 0/);assert.match(elements.usageTraceOverview.innerHTML,/未采集/);assert.match(elements.usageTraceOverview.innerHTML,/未取得/);
 assert.match(elements.usageTraceEvidence.innerHTML,/当时未采集/);assert.match(elements.usageTraceEvidence.innerHTML,/不等同于原始响应帧/);assert.match(elements.usageTraceEvidence.innerHTML,/&lt;img/);assert.doesNotMatch(elements.usageTraceEvidence.innerHTML,/<img/);
 trace.attempts[0].read_observed=true;c.renderUsageTrace({event,trace});assert.match(elements.usageTraceOverview.innerHTML,/0 \/ 0/);
 assert.match(c.eventStatusMeta({...event,outcome:'process_interrupted'}).label,/未知/);
 assert.match(c.eventStatusMeta({...event,outcome:'success',had_anomaly:true}).label,/曾有异常/);
});

test('trace requests cannot overwrite a newly opened event',async()=>{
 const {context:c,elements}=harness();installUsageFunctions(c,['loadUsageTrace']);
 const pending=[];c.request=()=>new Promise(resolve=>pending.push(resolve));
 const first={event_id:'first',state:'completed',outcome:'success'},last={event_id:'last',state:'completed',outcome:'error'};
 c.state.usageEvent=first;const one=c.loadUsageTrace(first);c.state.usageEvent=last;const two=c.loadUsageTrace(last);
 pending[1]({event:last,trace:null});await two;pending[0]({event:first,trace:null});await one;
 assert.equal(c.state.usageTrace.event.event_id,'last');assert.match(elements.usageTraceSummary.innerHTML,/last/);assert.doesNotMatch(elements.usageTraceSummary.innerHTML,/first/);
});

test('diagnostic aggregation excludes unknown requests and ignores stale scopes',async()=>{
 const {context:c,elements}=harness();installUsageFunctions(c,['loadUsageDiagnostics','renderUsageDiagnostics']);
 const pending=[];c.request=()=>new Promise(resolve=>pending.push(resolve));
 const one=c.loadUsageDiagnostics();c.state.searchFilter='new';const two=c.loadUsageDiagnostics();
 const current={summary:{completed:4,success:3,failed:1,unknown:12,affected:1,rate:.25},groups:[],daily:[]};pending[1](current);await two;
 pending[0]({summary:{completed:100,success:0}});await one;
 assert.equal(elements.uSuccess.textContent,'75.0%');assert.match(elements.usageDiagnosticsHint.textContent,/结果未知不计入成功率分母/);
 c.request=async()=>{throw Error('offline')};await c.loadUsageDiagnostics();assert.equal(elements.uSuccess.textContent,'—');assert.equal(elements.usageDiagnosticsSummary.innerHTML,'');
});

test('group drilldown distinguishes absent account from a literal unknown account',()=>{
 const {context:c}=harness();installUsageFunctions(c,['setUsageView','usageDrill']);c.setHash=()=>{};c.loadUsage=()=>{};
 c.usageDrill('account',{key:'',affected:1});assert.equal(c.state.accountFilter,'');assert.equal(c.state.missingDimension,'account');assert.equal(c.usageParams().get('missing_dimension'),'account');assert.equal(c.state.resultFilter,'anomaly');
 assert.equal(c.usageEventMatchesFilters({state:'completed',outcome:'error',account_id:''}),true);assert.equal(c.usageEventMatchesFilters({state:'completed',outcome:'error',account_id:'unknown'}),false);
 c.usageDrill('account',{key:'unknown',affected:1});assert.equal(c.state.missingDimension,'');assert.equal(c.state.accountFilter,'unknown');
});

test('CSV download uses the complete active diagnostic filter and JSON retains trace evidence',async()=>{
 const {context:c,elements}=harness();installUsageFunctions(c,['downloadUsageCSV','downloadUsageDiagnostic']);Object.assign(c.state,{range:'all',accountFilter:'slot',resultFilter:'partial',searchFilter:'request-id',causeFilter:'server_is_overloaded',transportFilter:'websocket',revisionFilter:'revision'});
 c.authHeaders=()=>({});c.saveUsageBlob=(name,blob)=>{c.saved={name,blob}};let url;c.fetch=async u=>{url=u;return {ok:true,blob:async()=>'csv'}};
 await c.downloadUsageCSV();const query=new URLSearchParams(url.split('?')[1]);for(const [k,v]of c.usageParams())assert.equal(query.get(k),v);assert.equal(elements.usageListCSV.disabled,false);assert.equal(c.saved.name,'usage-events.csv');
 c.Blob=Blob;c.state.usageTrace={event:{event_id:'id'},trace:{context:{request_id:'req'},attempts:[{failed:true}]}};c.downloadUsageDiagnostic();const diagnostic=JSON.parse(await c.saved.blob.text());assert.equal(diagnostic.trace.context.request_id,'req');assert.equal(diagnostic.trace.attempts[0].failed,true);
});

test('missing historical presence flags do not turn unavailable failure usage into zero tokens',()=>{
 const {context:c}=harness();assert.equal(c.usageTokensUnavailable({state:'completed',outcome:'upstream_truncated',input_tokens:0,output_tokens:0,total_tokens:0}),true);assert.equal(c.usageTokensUnavailable({state:'completed',outcome:'success',input_tokens:0,output_tokens:0,total_tokens:0,output_tokens_known:true}),false);
});
