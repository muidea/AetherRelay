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
    state:{locale:'zh-CN',unified:{loading:false,updating:new Set()},cg:{accounts:[]},codex:{accounts:[],busy:false}},
    t:(text,values={})=>text.replace(/\{(\w+)\}/g,(_,key)=>values[key]??''),
    request:async(url,requestOptions)=>{requests.push({url,options:requestOptions});if(options.request)return options.request(url,requestOptions);return {item:{}}},
    apiURL:path=>`/admin${path}`,
    authHeaders:headers=>headers,
    invalidateFeatureCatalog:()=>{},
    toast:(message,tone)=>messages.push({message,tone}),
    loadUnifiedAccounts:async()=>{},
    loadCodexAccounts:async()=>{reloads.codex++},
    renderCodexAccounts:()=>{},
    renderUnifiedAccounts:()=>{},
  });
  vm.runInContext(source.match(/^const esc=.*$/m)[0],context);
  for(const name of ['normalizedCodexFingerprintMode','unifiedCredentialKey','codexFingerprintControl','codexConcurrencyControl','unifiedCredentialEnabled','unifiedCredentialToggle','unifiedCredentialActionBlocked','setUnifiedCredentialEnabled','updateCodexFingerprintMode','updateCodexMaxConcurrency']){
    vm.runInContext(functionSource(name),context);
  }
  return {context,requests,messages,reloads};
}

test('unified account fingerprint control exposes off and scoped modes',()=>{
  const {context:c}=harness();
  const off=c.codexFingerprintControl({id:'codex-1',fingerprint_mode:'off'});
  assert.match(off,/data-codex-fingerprint="codex-1"/);
  assert.match(off,/<option value="off" selected>/);
  assert.match(c.codexFingerprintControl({id:'codex-1'}),/<option value="scoped" selected>/);
  c.state.unified.updating.add('codex:codex-1');
  assert.match(c.codexFingerprintControl({id:'codex-1',fingerprint_mode:'off'}),/ disabled>/);
});

test('account concurrency control defaults to two and patches the selected account',async()=>{
  const {context:c,requests,messages}=harness({request:async()=>({item:{id:'codex/id',max_concurrency:3}})});
  assert.match(c.codexConcurrencyControl({id:'codex/id'}),/value="2"/);
  c.state.codex.accounts=[{id:'codex/id',max_concurrency:2}];
  const input={dataset:{codexConcurrency:'codex/id'},value:'3',disabled:false};
  await c.updateCodexMaxConcurrency(input);
  assert.equal(requests[0].url,'/admin/api/codex/accounts/codex%2Fid');
  assert.deepEqual(JSON.parse(requests[0].options.body),{max_concurrency:3});
  assert.equal(c.state.codex.accounts[0].max_concurrency,3);
  assert.equal(c.state.unified.updating.size,0);
  assert.deepEqual(messages.map(item=>item.message),['Codex 账号最大并发已设为 3']);
});

test('account concurrency control rejects values outside one to thirty-two',async()=>{
  const {context:c,requests,messages,reloads}=harness();
  await c.updateCodexMaxConcurrency({dataset:{codexConcurrency:'codex-1'},value:'0',disabled:false});
  assert.equal(requests.length,0);
  assert.equal(reloads.codex,1);
  assert.deepEqual(messages,[{message:'最大并发必须是 1 到 32 之间的整数',tone:'error'}]);
});

test('unified account fingerprint control patches mode and clears busy state',async()=>{
  const {context:c,requests,messages}=harness({request:async()=>({item:{id:'codex/id',fingerprint_mode:'scoped'}})});
  c.state.codex.accounts=[{id:'codex/id',fingerprint_mode:'off'}];
  const select={dataset:{codexFingerprint:'codex/id'},value:'scoped',disabled:false};
  await c.updateCodexFingerprintMode(select);
  assert.equal(requests[0].url,'/admin/api/codex/accounts/codex%2Fid');
  assert.deepEqual(JSON.parse(requests[0].options.body),{fingerprint_mode:'scoped'});
  assert.equal(c.state.codex.accounts[0].fingerprint_mode,'scoped');
  assert.equal(c.state.unified.updating.size,0);
  assert.equal(select.disabled,false);
  assert.deepEqual(messages.map(item=>item.message),['Codex 指纹收敛已设为 scoped']);
});

test('failed fingerprint update reloads authoritative account state',async()=>{
  const {context:c,messages,reloads}=harness({request:async()=>{throw new Error('更新失败')}});
  const select={dataset:{codexFingerprint:'codex-1'},value:'scoped',disabled:false};
  await c.updateCodexFingerprintMode(select);
  assert.equal(reloads.codex,1);
  assert.equal(c.state.unified.updating.size,0);
  assert.equal(select.disabled,false);
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
