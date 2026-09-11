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

function harness(){
  const requests=[],messages=[];
  const context=vm.createContext({
    state:{locale:'zh-CN',unified:{loading:false,updating:new Set()},cg:{accounts:[]},codex:{accounts:[]}},
    t:(text,values={})=>text.replace(/\{(\w+)\}/g,(_,key)=>values[key]??''),
    request:async(url,options)=>{requests.push({url,options});return {item:{}}},
    apiURL:path=>`/admin${path}`,
    authHeaders:headers=>headers,
    invalidateFeatureCatalog:()=>{},
    toast:(message,tone)=>messages.push({message,tone}),
    loadUnifiedAccounts:async()=>{},
    renderUnifiedAccounts:()=>{},
  });
  vm.runInContext(source.match(/^const esc=.*$/m)[0],context);
  for(const name of ['normalizedCodexFingerprintMode','codexFingerprintSummary','unifiedCredentialKey','unifiedCredentialEnabled','unifiedCredentialToggle','unifiedCredentialActionBlocked','setUnifiedCredentialEnabled']){
    vm.runInContext(functionSource(name),context);
  }
  return {context,requests,messages};
}

test('unified account summary hides inactive fingerprint mode',()=>{
  const {context:c}=harness();
  assert.equal(c.codexFingerprintSummary({fingerprint_mode:'off'}),'');
  assert.equal(c.codexFingerprintSummary({}),'');
  assert.match(c.codexFingerprintSummary({fingerprint_mode:'session'}),/>指纹 会话</);
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
