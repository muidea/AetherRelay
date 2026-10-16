// Run with node --test web/admin/steering.test.cjs; no build dependencies.
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(`${__dirname}/index.html`,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];
function functionSource(name){return source.match(new RegExp(`^(?:async )?function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function )`,'m'))[0]}
function harness(request){
  const elements={},calls=[],timers=[];
  const c=vm.createContext({
    state:{codex:{accounts:[{id:'slot',email:'probe@example.com',model_snapshot:{models:[{id:'gpt-test'}]}}]}},
    $:id=>elements[id]??={value:id==='codexSteeringModel'?'gpt-test':'',disabled:false,textContent:'',innerHTML:'',showModal(){},close(){}},
    request:async(url,options)=>{calls.push({url,options});return request?.(url,options)},
    apiURL:path=>'/admin'+path,authHeaders:h=>h,t:v=>v,newUUID:()=>`key-${calls.length}`,
    setTimeout:fn=>{timers.push(fn);return timers.length},clearTimeout(){},
    renderCodexAccounts(){},renderUnifiedAccounts(){},
  });
  vm.runInContext(source.match(/^const esc=.*$/m)[0],c);
  for(const name of ['codexSteeringView','codexSteeringControl','codexSteeringLabel','renderCodexSteeringReport','closeCodexSteering','pollCodexSteering','openCodexSteering','startCodexSteering'])vm.runInContext(functionSource(name),c);
  return {c,elements,calls,timers};
}
const report={probe_id:'probe-one',target_slot_id:'slot',model:'gpt-test',capability:'UNKNOWN',execution_outcome:'running',state:'running',done:false};
test('opening the manager only queries history and never starts a probe',async()=>{
  const {c,calls}=harness(()=>{const e=new Error('missing');e.status=404;throw e});
  await c.openCodexSteering('slot');
  assert.equal(calls.length,1);assert.equal(calls[0].options,undefined);
  assert.match(calls[0].url,/slot\/steering-probes\/latest$/);
  assert.equal(c.codexSteeringView().report,null);
});
test('double click does not duplicate a pending submission and only model is sent',async()=>{
  let resolve;const pending=new Promise(r=>resolve=r);
  const {c,calls}=harness(()=>pending);c.codexSteeringView().slot='slot';
  const first=c.startCodexSteering();await c.startCodexSteering();
  assert.equal(calls.length,1);assert.deepEqual(JSON.parse(calls[0].options.body),{model:'gpt-test'});
  assert.ok(calls[0].options.headers['Idempotency-Key']);
  resolve(report);await first;
  assert.equal(c.codexSteeringView().report.probe_id,'probe-one');
});
test('uncertain network failure preserves the same idempotency key for retry',async()=>{
  let failures=1;const {c,calls}=harness(()=>{if(failures-- >0)throw new Error('offline');return report});
  c.codexSteeringView().slot='slot';await c.startCodexSteering();await c.startCodexSteering();
  assert.equal(calls.length,2);
  assert.equal(calls[0].options.headers['Idempotency-Key'],calls[1].options.headers['Idempotency-Key']);
});
test('closing ignores late results while the server task remains recoverable',async()=>{
  let resolve;const pending=new Promise(r=>resolve=r);
  const {c}=harness(()=>pending);c.codexSteeringView().slot='slot';
  const start=c.startCodexSteering();c.closeCodexSteering();resolve(report);await start;
  assert.equal(c.codexSteeringView().report,null);
});
test('reports separate local refusal from health and escape account/model data',()=>{
  const {c,elements}=harness();
  c.renderCodexSteeringReport({...report,done:true,state:'finished',admission_state:'rejected',admission_reason:'model_blocked',target_slot_id:'<slot>',model:'<script>',historical:true});
  assert.match(elements.codexSteeringResult.innerHTML,/本地准入拒绝，未请求上游/);
  assert.match(elements.codexSteeringResult.innerHTML,/历史观测/);
  assert.match(elements.codexSteeringResult.innerHTML,/&lt;script&gt;/);
  assert.doesNotMatch(elements.codexSteeringResult.innerHTML,/<script>/);
  assert.match(elements.codexSteeringJSON.textContent,/"admission_reason": "model_blocked"/);
});
test('account cards query the report on open instead of retaining a stale capability badge',()=>{
  const {c}=harness();
  c.renderCodexSteeringReport({...report,done:true,capability:'SUPPORTED'});
  const card=c.codexSteeringControl({id:'slot'});
  assert.match(card,/data-codex-steering="slot"/);
  assert.doesNotMatch(card,/SUPPORTED|协议支持/);
});
test('admission recheck refusal preserves credential refresh without claiming zero upstream requests',()=>{
  const {c,elements}=harness();
  c.renderCodexSteeringReport({...report,done:true,state:'finished',admission_state:'rejected',admission_reason:'usage_limit',upstream_request_sent:true,credential_refresh_performed:true});
  assert.match(elements.codexSteeringResult.innerHTML,/已执行凭据刷新，未发起 WS 探测/);
  assert.doesNotMatch(elements.codexSteeringResult.innerHTML,/未请求上游/);
  assert.match(elements.codexSteeringResult.innerHTML,/账号额度受限/);
  assert.match(elements.codexSteeringJSON.textContent,/"upstream_request_sent": true/);
});
