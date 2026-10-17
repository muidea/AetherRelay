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
  for(const name of ['codexSteeringView','codexSteeringControl','codexSteeringLabel','codexSteeringCheckHTML','renderCodexSteeringReport','closeCodexSteering','pollCodexSteering','openCodexSteering','startCodexSteering'])vm.runInContext(functionSource(name),c);
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
test('management diagnostics separate handshake, writes and upstream rejection and escape all text',()=>{
  const {c,elements}=harness();
  c.renderCodexSteeringReport({...report,done:true,state:'finished',execution_outcome:'upstream_error',error_code:'upstream_error',upstream_http_status:101,steer_sent:true,steer_write_completed:true,create_sent:true,create_write_completed:true,steering_state:'sent',upstream_error:{event_type:'error',type:'<type>',code:'<code>',param:'<input>',message:'<img src=x onerror=alert(1)>',status:400,truncated:true},timeline_truncated:true,timeline:[{sequence:1,elapsed_ms:0,direction:'send',type:'<response.steer>',state:'written'}]});
  const html=elements.codexSteeringResult.innerHTML;
  assert.match(html,/101/);assert.match(html,/400/);assert.match(html,/本地写入完成/);
  assert.match(html,/未观测到接受/);assert.match(html,/失败事件/);assert.match(html,/较早条目或过长类型已截断/);
  for(const value of ['type','code','input','response.steer'])assert.ok(html.includes(`&lt;${value}&gt;`));
  assert.match(html,/&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.doesNotMatch(html,/<img|<response\.steer>/);
  const exported=JSON.parse(elements.codexSteeringJSON.textContent);
  assert.equal(exported.upstream_error.code,'<code>');assert.equal(exported.steer_write_completed,true);
  assert.equal(exported.capability,'UNKNOWN');assert.equal(exported.timeline.length,1);
});
test('attempted write failures and old reports never claim successful upstream acceptance',()=>{
  const {c,elements}=harness();
  c.renderCodexSteeringReport({...report,done:true,steer_sent:true,steer_write_completed:false,steering_state:'write_failed'});
  assert.match(elements.codexSteeringResult.innerHTML,/可能已抵达上游/);
  assert.match(elements.codexSteeringResult.innerHTML,/写入未完成/);
  assert.doesNotMatch(elements.codexSteeringResult.innerHTML,/已接受/);
  c.renderCodexSteeringReport({...report,done:true,steer_sent:true,steering_state:'sent'});
  assert.match(elements.codexSteeringResult.innerHTML,/已尝试提交（旧报告）/);
  assert.doesNotMatch(elements.codexSteeringResult.innerHTML,/本地写入完成|未观测到接受/);
});

test('native inflight refusal shows profile-scoped unsupported without changing raw diagnostics',()=>{
  const {c,elements,calls}=harness();
  c.renderCodexSteeringReport({...report,done:true,state:'finished',capability:'UNSUPPORTED_FOR_PROFILE',execution_outcome:'unsupported_native_inflight_message',error_code:'unsupported_native_inflight_message',steering_state:'failed',steer_sent:true,steer_write_completed:true,upstream_error:{event_type:'error',type:'invalid_request_error',code:'unsupported_native_inflight_message',message:'Start a new independent response.create turn instead.',truncated:false}});
  const html=elements.codexSteeringResult.innerHTML;
  assert.match(html,/当前配置不支持/);
  assert.match(html,/上游当前通道不支持运行中更新/);
  assert.match(html,/本地写入完成/);
  assert.doesNotMatch(html,/证据不足|已接受/);
  const json=JSON.parse(elements.codexSteeringJSON.textContent);
  assert.equal(json.capability,'UNSUPPORTED_FOR_PROFILE');
  assert.equal(json.execution_outcome,'unsupported_native_inflight_message');
  assert.equal(json.upstream_error.code,json.error_code);
  assert.equal(calls.length,0);
});



test('fallback reports one final capability with escaped native diagnostics',()=>{
 const {c,elements,calls}=harness();
 const fallback={reason:'native_protocol_unsupported',error_code:'unsupported_native_inflight_message',upstream_error:{code:'<native-error>',message:'<script>'},timeline:[{type:'<response.steer>'}]};
 c.renderCodexSteeringReport({...report,done:true,state:'finished',capability:'SUPPORTED',invocation_mode:'local_queue_drain_create',execution_outcome:'completed',input_queued:true,continuation_sent:true,continuation_write_completed:true,fallback});
 const rendered=elements.codexSteeringResult.innerHTML;
 assert.match(rendered,/客户端式更新与续接/);assert.match(rendered,/协议支持/);assert.match(rendered,/原生尝试诊断/);assert.match(rendered,/已切换调用方式/);assert.match(rendered,/&lt;native-error&gt;/);assert.match(rendered,/&lt;script&gt;/);
 assert.doesNotMatch(rendered,/<script>|当前配置不支持|Steering 协议状态/);
 const json=JSON.parse(elements.codexSteeringJSON.textContent);
 assert.equal(json.capability,'SUPPORTED');assert.equal(json.native_steering,undefined);assert.equal(json.client_steering,undefined);assert.equal(json.fallback.capability,undefined);assert.equal(calls.length,0);
});
test('a switched probe keeps polling and prevents resubmission until its final result',async()=>{
 const ongoing={...report,invocation_mode:'local_queue_drain_create',fallback:{reason:'native_protocol_inconclusive'},input_queued:true};
 const {c,elements,timers,calls}=harness(()=>ongoing);c.codexSteeringView().slot='slot';
 await c.pollCodexSteering(c.codexSteeringView().epoch,'probe-one');
 assert.equal(elements.codexSteeringStart.disabled,true);assert.equal(elements.codexSteeringModel.disabled,true);assert.equal(elements.codexSteeringExport.disabled,true);assert.equal(timers.length,1);
 await c.startCodexSteering();assert.equal(calls.length,1);
 assert.match(elements.codexSteeringResult.innerHTML,/实际检测方式/);
});
