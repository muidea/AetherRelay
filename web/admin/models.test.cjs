// Run: node --test web/admin/models.test.cjs
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(`${__dirname}/index.html`,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];
function harness(){
  const c=vm.createContext({Date,t:(s,values={})=>s.replace(/\{(\w+)\}/g,(_,key)=>values[key]??key),formatAccountDate:s=>s});
  vm.runInContext(source.match(/^const esc=.*$/m)[0],c);
  vm.runInContext(source.match(/^function codexAccountModelList\([\s\S]*?(?=\nfunction )/m)[0],c);
  return c;
}
test('CP-CAP-010 full account model list with explicit unavailable reasons',()=>{
  const c=harness();
  const account={model_snapshot:{models:[{id:'gpt-5.5'},{id:'gpt-6-astra'},{id:'gpt-5.6-sol'},{id:'gpt-5.6-terra'}]},model_availability:[
    {model:'gpt-5.5',available:false,reason:'model_not_found',until:'2026-09-08T10:00:00Z'},
    ...['gpt-6-astra','gpt-5.6-sol','gpt-5.6-terra'].map(model=>({model,available:true}))]};
  const out=c.codexAccountModelList(account);
  assert.match(out,/可用模型 3\/4/);
  for(const model of account.model_snapshot.models)assert.ok(out.includes(model.id));
  assert.match(out,/上游模型不可用/);assert.match(out,/恢复于/);assert.match(out,/<details/);
  assert.match(source,/\$\{codexAccountModelList\(account\)\}.*\$\{codexRefreshBadge/);
});
test('CP-CAP-010 unknown, empty, expired snapshots and untrusted IDs',()=>{
  const c=harness();
  assert.match(c.codexAccountModelList({}),/等待模型同步/);
  assert.match(c.codexAccountModelList({model_snapshot:{models:[]}}),/未发现模型/);
  const account={model_snapshot:{models:[{id:'<img src=x onerror=alert(1)>'}]}};
  let out=c.codexAccountModelList(account);
  assert.match(out,/可用性未知/);assert.match(out,/&lt;img/);assert.doesNotMatch(out,/<img/);
  account.model_snapshot.expires_at='2000-01-01T00:00:00Z';
  account.model_availability=[{model:account.model_snapshot.models[0].id,available:true}];
  out=c.codexAccountModelList(account);assert.match(out,/可用模型 0\/1/);assert.match(out,/模型快照已过期/);
});

test('quota model reason shows the exhausted weekly window and upstream reset, not cache expiry',()=>{
  const c=harness();
  const account={model_snapshot:{models:[{id:'gpt-6.1-sol'},{id:'blocked'},{id:'cooling'}]},model_availability:[
    {model:'gpt-6.1-sol',reason:'usage_limit',usage_limit_scope:'standard',usage_limit_window:'standard-secondary',usage_limit_window_seconds:604800,usage_limit_reset_at:'2026-11-04T06:59:27Z',until:'cache-expiry'},
    {model:'blocked',reason:'model_blocked'},
    {model:'cooling',reason:'network',until:'network-retry'},
  ]};
  const out=c.codexAccountModelList(account);
  assert.match(out,/周额度耗尽 · 预计重置于 2026-11-04T06:59:27Z/);
  assert.doesNotMatch(out,/cache-expiry/);
  assert.match(out,/管理员已禁止/);
  assert.match(out,/网络冷却 · 恢复于 network-retry/);
  account.model_snapshot.expires_at='2000-01-01T00:00:00Z';
  assert.match(c.codexAccountModelList(account),/管理员已禁止/);
});
test('unexplained group restriction invents no reset; independent review quota is labeled and escaped',()=>{
  const c=harness();
  const account={model_snapshot:{models:[{id:'gpt-6.1-sol'},{id:'codex-auto-review'}]},model_availability:[
    {model:'gpt-6.1-sol',reason:'usage_limit'},
    {model:'codex-auto-review',reason:'usage_limit',usage_limit_scope:'code-review',usage_limit_window:'review-primary',usage_limit_window_seconds:18000,usage_limit_reset_at:'<unsafe>'},
  ]};
  const out=c.codexAccountModelList(account);
  assert.match(out,/额度受限（未明确耗尽窗口）/);
  assert.match(out,/代码审查 · 5 小时额度耗尽 · 预计重置于 &lt;unsafe&gt;/);
  assert.doesNotMatch(out,/<unsafe>/);
});
