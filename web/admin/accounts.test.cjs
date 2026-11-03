// Run with: node --test web/admin/accounts.test.cjs (no build or npm dependencies).
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

const html=fs.readFileSync(`${__dirname}/index.html`,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];

function functionSource(name){
  const match=source.match(new RegExp(`^(?:async )?function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function )`,'m'));
  assert.ok(match,`missing function ${name}`);
  return match[0];
}

function harness(options={}){
  const requests=[],messages=[],reloads={codex:0};
  const context=vm.createContext({
    TextEncoder,
    state:{locale:'zh-CN',unified:{loading:false,updating:new Set()},cg:{accounts:[]},codex:{accounts:[],settingsDrafts:new Map(),busy:false}},
    t:(text,values={})=>text.replace(/\{(\w+)\}/g,(_,key)=>values[key]??''),
    request:async(url,requestOptions)=>{requests.push({url,options:requestOptions});if(options.request)return options.request(url,requestOptions);return {item:{}}},
    apiURL:path=>`/admin${path}`,
    authHeaders:headers=>headers,
    invalidateFeatureCatalog:()=>{},
    toast:(message,tone,params={})=>messages.push({message:message.replace(/\{(\w+)\}/g,(_,key)=>params[key]??''),tone}),
    loadUnifiedAccounts:async()=>{},
    loadCodexAccounts:async()=>{reloads.codex++},
    renderCodexAccounts:()=>{},
    renderUnifiedAccounts:()=>{},
    document:{querySelectorAll:()=>[]},
  });
  vm.runInContext(source.match(/^const esc=.*$/m)[0],context);
  for(const name of ['normalizedCodexBlockedModels','validCodexImportBlockedModels','codexBlockedModelsControl','updateCodexBlockedModels','normalizedCodexFingerprintMode','unifiedCredentialKey','codexSettingsValue','codexSettingsDirty','codexFingerprintControl','codexConcurrencyControl','codexSettingsActions','refreshCodexSettingsDraftUI','unifiedCredentialEnabled','unifiedCredentialToggle','unifiedCredentialActionBlocked','setUnifiedCredentialEnabled','updateCodexFingerprintMode','updateCodexMaxConcurrency','cancelCodexAccountSettings','saveCodexAccountSettings']){
    vm.runInContext(functionSource(name),context);
  }
  return {context,requests,messages,reloads};
}

test('Web device identity failure stays visible without changing healthy account badges',()=>{
  const {context:c}=harness();
  vm.runInContext(functionSource('webDeviceIdentityBadge'),c);
  assert.equal(c.webDeviceIdentityBadge({id:'healthy'}),'');
  const rendered=c.webDeviceIdentityBadge({device_identity_error:'invalid_device_identity'});
  assert.match(rendered,/设备身份异常 · 已停止调度/);
  assert.match(rendered,/更新凭据或启用账号不会解除此限制/);
  assert.match(source,/webDeviceIdentityBadge\(account\)/);
  assert.match(source,/webDeviceIdentityBadge\(a\)/);
});

test('unified account fingerprint control exposes off and scoped modes',()=>{
  const {context:c}=harness();
  const off=c.codexFingerprintControl({id:'codex-1',fingerprint_mode:'off'});
  assert.match(off,/data-codex-fingerprint="codex-1"/);
  assert.match(off,/<option value="off" selected>/);
  assert.match(c.codexFingerprintControl({id:'codex-1'}),/<option value="scoped" selected>/);
  c.state.unified.updating.add('codex:codex-1');
  assert.match(c.codexFingerprintControl({id:'codex-1',fingerprint_mode:'off'}),/ disabled>/);
});

test('partial quota refresh leaves expired windows visibly stale',()=>{
  const {context:c}=harness();
  c.formatAccountDate=value=>value;
  for(const name of ['codexUsageWindowLabel','codexUsageSnapshotBadge'])vm.runInContext(functionSource(name),c);
  const fresh=new Date(Date.now()+60000).toISOString(),old=new Date(Date.now()-60000).toISOString();
  const rendered=c.codexUsageSnapshotBadge({usage_refresh_error:'timeout',usage_snapshot:{expires_at:fresh,windows:[
    {id:'standard-primary',used_percent_known:true,used_percent:20,expires_at:fresh,observed_at:fresh},
    {id:'standard-secondary',used_percent_known:true,used_percent:100,limit_reached:true,expires_at:old,observed_at:old},
  ]}});
  assert.equal((rendered.match(/缓存已过期/g)||[]).length,1);
  assert.match(rendered,/剩余 80%/);
  assert.match(rendered,/已达上限/);
  assert.match(rendered,/用量刷新失败：timeout/);
  assert.ok(rendered.includes(old));
});

test('model blacklist uses a draft, normalizes exact IDs and supports explicit clear',async()=>{
  const {context:c,requests}=harness({request:async()=>({item:{id:'slot',blocked_models:['future','gpt-sol']}})});
  const account={id:'slot',blocked_models:[],model_snapshot:{models:[{id:'gpt-sol'},{id:'<unsafe>'}]}};
  c.state.codex.accounts=[account];
  c.updateCodexBlockedModels({dataset:{codexBlockedModel:'slot',model:'gpt-sol'},checked:true});
  c.updateCodexBlockedModels({dataset:{codexBlockedModels:'slot'},value:' gpt-sol \nfuture\ngpt-sol'});
  assert.equal(requests.length,0);
  assert.match(c.codexBlockedModelsControl(account),/data-model="gpt-sol" checked/);
  assert.match(c.codexBlockedModelsControl(account),/&lt;unsafe&gt;/);
  await c.saveCodexAccountSettings('slot');
  assert.deepEqual(JSON.parse(requests[0].options.body),{blocked_models:['future','gpt-sol']});
  c.updateCodexBlockedModels({dataset:{codexBlockedModels:'slot'},value:''});
  await c.saveCodexAccountSettings('slot');
  assert.deepEqual(JSON.parse(requests[1].options.body),{blocked_models:[]});
});

test('cancelled or invalid blacklist edits do not issue a PATCH',async()=>{
  const {context:c,requests,messages}=harness();
  c.state.codex.accounts=[{id:'slot',blocked_models:['gpt-sol']}];
  c.updateCodexBlockedModels({dataset:{codexBlockedModel:'slot',model:'gpt-sol'},checked:false});
  c.cancelCodexAccountSettings('slot');
  await c.saveCodexAccountSettings('slot');
  assert.equal(requests.length,0);
  assert.equal(c.codexSettingsDirty(c.state.codex.accounts[0]),false);
  c.updateCodexBlockedModels({dataset:{codexBlockedModels:'slot'},value:'bad\x00model'});
  assert.equal(messages.at(-1).tone,'error');
  assert.equal(c.validCodexImportBlockedModels(null),false);
  assert.equal(c.validCodexImportBlockedModels(undefined),true);
  assert.equal(c.validCodexImportBlockedModels(['字'.repeat(86)]),false);
});

test('account settings remain local until one explicit save',async()=>{
  const {context:c,requests,messages}=harness({request:async()=>({item:{id:'codex/id',max_concurrency:3}})});
  assert.match(c.codexConcurrencyControl({id:'codex/id'}),/value="2"/);
  c.state.codex.accounts=[{id:'codex/id',fingerprint_mode:'scoped',max_concurrency:2}];
  const input={dataset:{codexConcurrency:'codex/id'},value:'3',disabled:false};
  c.updateCodexMaxConcurrency(input);
  c.updateCodexFingerprintMode({dataset:{codexFingerprint:'codex/id'},value:'off'});
  assert.equal(requests.length,0);
  assert.match(c.codexSettingsActions(c.state.codex.accounts[0]),/data-codex-settings-save="codex\/id"/);
  await c.saveCodexAccountSettings('codex/id');
  assert.equal(requests[0].url,'/admin/api/codex/accounts/codex%2Fid');
  assert.deepEqual(JSON.parse(requests[0].options.body),{fingerprint_mode:'off',max_concurrency:3});
  assert.equal(c.state.codex.accounts[0].max_concurrency,3);
  assert.equal(c.state.unified.updating.size,0);
  assert.equal(c.state.codex.settingsDrafts.size,0);
  assert.deepEqual(messages.map(item=>item.message),['Codex 账号设置已保存']);
});

test('account concurrency control rejects values outside one to thirty-two',()=>{
  const {context:c,requests,messages,reloads}=harness();
  c.updateCodexMaxConcurrency({dataset:{codexConcurrency:'codex-1'},value:'0',disabled:false});
  assert.equal(requests.length,0);
  assert.equal(reloads.codex,0);
  assert.deepEqual(messages,[{message:'最大并发必须是 1 到 32 之间的整数',tone:'error'}]);
});

test('cancel discards staged account settings without a request',()=>{
  const {context:c,requests,messages}=harness({request:async()=>({item:{id:'codex/id',fingerprint_mode:'scoped'}})});
  c.state.codex.accounts=[{id:'codex/id',fingerprint_mode:'off'}];
  const select={dataset:{codexFingerprint:'codex/id'},value:'scoped',disabled:false};
  c.updateCodexFingerprintMode(select);
  c.cancelCodexAccountSettings('codex/id');
  assert.equal(requests.length,0);
  assert.equal(c.state.codex.settingsDrafts.size,0);
  assert.deepEqual(messages,[]);
});

test('failed settings save preserves the draft for retry',async()=>{
  const {context:c,messages,reloads}=harness({request:async()=>{throw new Error('更新失败')}});
  c.state.codex.accounts=[{id:'codex-1',fingerprint_mode:'off',max_concurrency:2}];
  const select={dataset:{codexFingerprint:'codex-1'},value:'scoped',disabled:false};
  c.updateCodexFingerprintMode(select);
  await c.saveCodexAccountSettings('codex-1');
  assert.equal(reloads.codex,0);
  assert.equal(c.state.codex.settingsDrafts.size,1);
  assert.equal(c.state.unified.updating.size,0);
  assert.deepEqual(messages,[{message:'更新失败',tone:'error'}]);
});

test('unified account slots render independent credential switches',()=>{
  const {context:c}=harness();
  const web=c.unifiedCredentialToggle('web',{id:'web-1',status:'正常'});
  assert.match(web,/data-ua-web-enabled="web-1"/);
  assert.match(web,/checked/);
  const codex=c.unifiedCredentialToggle('codex',{id:'codex-1',status:'disabled'});
  assert.match(codex,/data-ua-codex-enabled="codex-1"/);
  assert.doesNotMatch(codex,/checked/);
  c.state.unified.updating.add('web:web-1');
  assert.match(c.unifiedCredentialToggle('web',{id:'web-1',status:'normal'}),/disabled/);
});

test('disabled credentials keep quota and usage maintenance available',()=>{
  const {context:c}=harness();
  const web={id:'web-1',status:'禁用'};
  const codex={id:'codex-1',status:'disabled'};
  assert.equal(c.unifiedCredentialActionBlocked('web',web,'quota'),false);
  assert.equal(c.unifiedCredentialActionBlocked('codex',codex,'usage'),false);
  assert.equal(c.unifiedCredentialActionBlocked('codex',codex,'credential'),false);
  assert.equal(c.unifiedCredentialActionBlocked('codex',codex,'discovery'),true);
  c.state.unified.updating.add('codex:codex-1');
  assert.equal(c.unifiedCredentialActionBlocked('codex',codex,'usage'),true);
});

test('completed quota and usage refreshes reload authoritative account statistics',()=>{
  assert.match(functionSource('pollRefreshProgress'),/if\(done\)[\s\S]*?await loadChatGPTAccounts\(\)/);
  assert.match(functionSource('beginCodexUsagePolling'),/if\(current\.done\)[\s\S]*?await loadCodexAccounts\(\)/);
  assert.match(functionSource('codexRefreshBadge'),/permanent_auth_failure/);
});

test('credential switches patch only their matching account-pool owner',async()=>{
  const {context:c,requests,messages}=harness();
  await c.setUnifiedCredentialEnabled('web','web/id',false);
  await c.setUnifiedCredentialEnabled('codex','codex/id',true);
  assert.equal(requests[0].url,'/admin/api/chatgpt/accounts/web%2Fid');
  assert.deepEqual(JSON.parse(requests[0].options.body),{status:'禁用'});
  assert.equal(requests[1].url,'/admin/api/codex/accounts/codex%2Fid');
  assert.deepEqual(JSON.parse(requests[1].options.body),{status:'normal'});
  assert.deepEqual(messages.map(item=>item.message),['ChatGPT Web 凭证已禁用','Codex CLI 凭证已启用']);
  assert.equal(c.state.unified.updating.size,0);
});

test('known five-hour and weekly utilization takes priority over legacy copied group flags',()=>{
  const {context:c}=harness();c.formatAccountDate=value=>value;
  for(const name of ['codexUsageWindowLabel','codexUsageSnapshotBadge'])vm.runInContext(functionSource(name),c);
  const expires_at=new Date(Date.now()+60000).toISOString();
  const out=c.codexUsageSnapshotBadge({usage_snapshot:{expires_at,windows:[
    {id:'standard-primary',window_seconds:18000,used_percent_known:true,used_percent:0,allowed_known:true,allowed:false,limit_reached:true},
    {id:'standard-secondary',window_seconds:604800,used_percent_known:true,used_percent:100,allowed_known:true,allowed:false,limit_reached:true,reset_at:'weekly-reset'},
  ]}});
  assert.match(out,/5 小时/);assert.match(out,/剩余 100%/);
  assert.match(out,/剩余 0% · 已达上限/);
  assert.match(out,/预计重置于 weekly-reset/);
  assert.equal((out.match(/已达上限/g)||[]).length,1);
});
test('unknown group restriction preserves unknown percentages and its own freshness',()=>{
  const {context:c}=harness();c.formatAccountDate=value=>value;
  for(const name of ['codexUsageWindowLabel','codexUsageSnapshotBadge'])vm.runInContext(functionSource(name),c);
  const fresh=new Date(Date.now()+60000).toISOString(),old=new Date(Date.now()-60000).toISOString();
  const out=c.codexUsageSnapshotBadge({usage_snapshot:{expires_at:fresh,windows:[
    {id:'standard-primary',group_id:'standard',used_percent_known:false,reset_at:'metadata-only'},
    {id:'additional-unknown',used_percent_known:false,limit_reached:true},
  ],groups:[{id:'standard',scope:'standard',allowed_known:true,allowed:false,expires_at:old},{id:'extra',scope:'additional',model:'<unsafe>',limit_reached_known:true,limit_reached:true,expires_at:fresh}]}});
  assert.match(out,/用量未知/);assert.match(out,/主窗口/);
  assert.doesNotMatch(out,/剩余 [01]00?%|width:0%/);
  assert.match(out,/普通模型 · 额度受限（未明确耗尽窗口） · 缓存已过期/);
  assert.match(out,/&lt;unsafe&gt;/);assert.doesNotMatch(out,/<unsafe>/);
});

test('partial headers preserve the original group window explanation in the usage view',()=>{
  const {context:c}=harness();c.formatAccountDate=value=>value;
  for(const name of ['codexUsageWindowLabel','codexUsageSnapshotBadge'])vm.runInContext(functionSource(name),c);
  const expires_at=new Date(Date.now()+60000).toISOString();
  const out=c.codexUsageSnapshotBadge({usage_snapshot:{expires_at,windows:[
    {id:'standard-secondary',used_percent_known:true,used_percent:20,observed_at:'new-header'},
  ],groups:[{id:'standard',scope:'standard',allowed_known:true,allowed:false,observed_at:'full-query',expires_at,exhausted_window_id:'standard-secondary',exhausted_window_seconds:604800,reset_at:'original-weekly-reset'}]}});
  assert.match(out,/剩余 80%/);
  assert.match(out,/普通模型 · 周 · 已达上限 · 预计重置于 original-weekly-reset/);
  assert.match(out,/上次观测 full-query/);
  assert.doesNotMatch(out,/未明确耗尽窗口/);
});

function usageSummaryHarness(){
  const {context:c}=harness();c.formatAccountDate=value=>value;
  for(const name of ['codexUsageWindowLabel','codexStandardUsageBadge','unifiedCodexSummary','codexAccountModelList'])vm.runInContext(functionSource(name),c);
  for(const name of ['codexRefreshBadge','accountRefreshScheduleBadge','modelDiscoveryBadge'])c[name]=()=>'';
  return c;
}

test('unified Codex summary shows weekly exhaustion even with a full five-hour window and available review model',()=>{
  const c=usageSummaryHarness(),expires_at=new Date(Date.now()+60000).toISOString(),reset_at=new Date(Date.now()+2*86400000).toISOString();
  const account={usage_snapshot:{expires_at,windows:[
    {id:'standard-primary',scope:'standard',window_seconds:18000,used_percent_known:true,used_percent:0},
    {id:'standard-secondary',scope:'standard',window_seconds:604800,used_percent_known:true,used_percent:100,reset_at},
    {id:'code-review-primary',scope:'code-review',used_percent_known:true,used_percent:0},
  ]},model_snapshot:{expires_at,models:[{id:'gpt-6.1-sol'},{id:'codex-auto-review'}]},model_availability:[
    {model:'gpt-6.1-sol',available:false,reason:'usage_limit',usage_limit_scope:'standard',usage_limit_window:'standard-secondary',usage_limit_window_seconds:604800,usage_limit_reset_at:reset_at},
    {model:'codex-auto-review',available:true},
  ]};
  const out=c.unifiedCodexSummary(account);
  assert.match(out,/普通模型/);assert.match(out,/剩余 0%/);assert.match(out,/周额度耗尽/);assert.match(out,new RegExp('预计重置于 '+reset_at.replaceAll('.','\\.')));
  assert.match(out,/unified-usage-meter exhausted/);assert.doesNotMatch(out,/剩余 100%|width:100%/);
  assert.match(out,/可用模型 1\/2/);assert.match(out,/tag tag-purple">codex-auto-review/);
});

test('summary uses the tightest standard window independently of array order and other quota scopes',()=>{
  const c=usageSummaryHarness(),expires_at=new Date(Date.now()+60000).toISOString();
  const windows=[
    {id:'code-review-primary',scope:'code-review',used_percent_known:true,used_percent:100},
    {id:'additional-reserve-primary',scope:'additional',used_percent_known:true,used_percent:100},
    {id:'standard-primary',scope:'standard',window_seconds:18000,used_percent_known:true,used_percent:75},
    {id:'standard-secondary',scope:'standard',window_seconds:604800,used_percent_known:true,used_percent:20},
  ];
  for(const ordered of [windows,[...windows].reverse()]){
    const out=c.codexStandardUsageBadge({usage_snapshot:{expires_at,windows:ordered}});
    assert.match(out,/剩余 25%/);assert.match(out,/5 小时/);assert.match(out,/unified-usage-meter warning/);
    assert.doesNotMatch(out,/额度耗尽|剩余 0%/);
  }
  const onlyReview=c.codexStandardUsageBadge({usage_snapshot:{expires_at,windows:windows.slice(0,2)}});
  assert.match(onlyReview,/用量未知/);assert.doesNotMatch(onlyReview,/剩余|unified-usage-meter/);
});

test('unknown standard utilization and group denial cannot appear as healthy remaining quota',()=>{
  const c=usageSummaryHarness(),expires_at=new Date(Date.now()+60000).toISOString();
  const snapshot={expires_at,windows:[
    {id:'standard-primary',used_percent_known:true,used_percent:0},
    {id:'standard-secondary',used_percent_known:false},
  ]};
  let out=c.codexStandardUsageBadge({usage_snapshot:snapshot});
  assert.match(out,/用量未知/);assert.doesNotMatch(out,/剩余|unified-usage-meter/);
  snapshot.windows.pop();snapshot.groups=[{id:'standard',scope:'standard',allowed_known:true,allowed:false}];
  out=c.codexStandardUsageBadge({usage_snapshot:snapshot});
  assert.match(out,/用量未知/);assert.match(out,/额度受限（未明确耗尽窗口）/);assert.doesNotMatch(out,/剩余|预计重置/);
  snapshot.groups[0].exhausted_window_id='standard-secondary';snapshot.groups[0].exhausted_window_seconds=604800;snapshot.groups[0].reset_at=new Date(Date.now()+3600000).toISOString();
  out=c.codexStandardUsageBadge({usage_snapshot:snapshot});
  assert.match(out,/周额度耗尽/);assert.match(out,/预计重置于/);assert.match(out,/用量未知/);assert.doesNotMatch(out,/剩余|unified-usage-meter/);
});

test('summary labels stale observations, ignores expired group denial, and supports a single weekly window',()=>{
  const c=usageSummaryHarness(),fresh=new Date(Date.now()+60000).toISOString(),old=new Date(Date.now()-60000).toISOString();
  const snapshot={expires_at:fresh,windows:[{id:'standard-primary',window_seconds:604800,used_percent_known:true,used_percent:40}],groups:[{id:'standard',scope:'standard',allowed_known:true,allowed:false,expires_at:old}]};
  let out=c.codexStandardUsageBadge({usage_snapshot:snapshot});
  assert.match(out,/剩余 60%/);assert.match(out,/周/);assert.doesNotMatch(out,/额度受限|缓存已过期/);
  snapshot.windows[0].expires_at=old;out=c.codexStandardUsageBadge({usage_snapshot:snapshot});
  assert.match(out,/缓存已过期/);
  assert.match(c.codexStandardUsageBadge({}),/用量未同步/);
});

test('simultaneously exhausted windows report the last reset and keep an unknown reset unknown',()=>{
  const c=usageSummaryHarness(),expires_at=new Date(Date.now()+60000).toISOString();
  const earlier=new Date(Date.now()+3600000).toISOString(),later=new Date(Date.now()+86400000).toISOString();
  const windows=[
    {id:'standard-primary',window_seconds:18000,used_percent_known:true,used_percent:100,reset_at:earlier},
    {id:'standard-secondary',window_seconds:604800,used_percent_known:true,used_percent:100,reset_at:later},
  ];
  for(const ordered of [windows,[...windows].reverse()]){
    const out=c.codexStandardUsageBadge({usage_snapshot:{expires_at,windows:ordered}});
    assert.match(out,/剩余 0%/);assert.match(out,/周额度耗尽/);assert.ok(out.includes(later));assert.ok(!out.includes(earlier));
  }
  windows[0].reset_at='';
  const out=c.codexStandardUsageBadge({usage_snapshot:{expires_at,windows}});
  assert.match(out,/5 小时额度耗尽/);assert.doesNotMatch(out,/预计重置于/);
});

test('known percentages override legacy copied denial flags and unknown exhaustion never invents a percentage',()=>{
  const c=usageSummaryHarness(),expires_at=new Date(Date.now()+60000).toISOString();
  const snapshot={expires_at,windows:[{id:'primary',used_percent_known:true,used_percent:0,allowed_known:true,allowed:false,limit_reached:true}]};
  assert.match(c.codexStandardUsageBadge({usage_snapshot:snapshot}),/剩余 100%/);
  snapshot.windows.push({id:'secondary',window_seconds:604800,used_percent_known:false,limit_reached:true});
  const out=c.codexStandardUsageBadge({usage_snapshot:snapshot});
  assert.match(out,/周额度耗尽/);assert.match(out,/用量未知/);assert.doesNotMatch(out,/剩余|unified-usage-meter/);
  snapshot.windows[1]={id:'secondary',used_percent_known:true,used_percent:100,expires_at:new Date(Date.now()-60000).toISOString(),reset_at:'<unsafe>'};
  assert.match(c.codexStandardUsageBadge({usage_snapshot:snapshot}),/&lt;unsafe&gt;/);
  assert.doesNotMatch(c.codexStandardUsageBadge({usage_snapshot:snapshot}),/<unsafe>/);
});
