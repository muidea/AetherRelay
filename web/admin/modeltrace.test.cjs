const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(`${__dirname}/index.html`,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];
function functionSource(name){const m=source.match(new RegExp(`^(?:async )?function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function )`,'m'));assert.ok(m,`missing ${name}`);return m[0]}
function harness(reply=()=>({})){const elements={},calls=[],timers=[];const c=vm.createContext({state:{codex:{accounts:[{id:'slot',model_snapshot:{models:[{id:'gpt-test'}]}}]}},$:id=>elements[id]??={value:id==='codexModelTraceModel'?'gpt-test':id==='codexModelTraceReasoning'?'low':'',disabled:false,textContent:'',innerHTML:'',showModal(){}},t:(s,params={})=>s.replace(/\{(\w+)\}/g,(_,key)=>params[key]??`{${key}}`),esc:s=>String(s).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;'),apiURL:p=>p,authHeaders:h=>h,newUUID:()=>`key-${calls.length}`,request:async(url,options)=>{calls.push({url,options});return reply(url,options)},renderCodexAccounts(){},renderUnifiedAccounts(){},setTimeout:fn=>{timers.push(fn);return timers.length},clearTimeout(){}});for(const name of ['codexModelTraceView','codexModelTraceControl','codexModelTraceLabel','renderCodexModelTraceReport','closeCodexModelTrace','pollCodexModelTrace','openCodexModelTrace','startCodexModelTrace','cancelCodexModelTrace'])vm.runInContext(functionSource(name),c);return {c,elements,calls,timers}}
const pending={probe_id:'one',target_slot_id:'slot',model:'gpt-test',state:'running',done:false,samples:[]};
test('opening only reads latest and polling does not resubmit',async()=>{const{c,calls,timers}=harness(()=>pending);await c.openCodexModelTrace('slot');assert.equal(calls.length,1);assert.match(calls[0].url,/modeltrace-probes\/latest$/);assert.equal(calls[0].options,undefined);assert.equal(timers.length,1);await c.startCodexModelTrace();assert.equal(calls.length,1)});
test('submit freezes model and reasoning and active cancel uses protected DELETE',async()=>{const{c,calls,elements}=harness(()=>pending);c.codexModelTraceView().slot='slot';await c.startCodexModelTrace();assert.deepEqual(JSON.parse(calls[0].options.body),{model:'gpt-test',reasoning:'low'});assert.ok(calls[0].options.headers['Idempotency-Key']);assert.equal(elements.codexModelTraceReasoning.disabled,true);await c.cancelCodexModelTrace();assert.equal(calls[1].options.method,'DELETE');assert.match(calls[1].url,/modeltrace-probes\/one$/)});
test('closed-set evidence is escaped and incomplete sampling remains visible',()=>{const{c,elements}=harness();c.renderCodexModelTraceReport({...pending,done:true,historical:true,samples:[{state:'completed',expected_count:300},{state:'failed',error_code:'<script>'},{state:'not_started'}],result:{requested_model_in_bank:false,snapshot:{bank_sha256:'digest'},analysis:{prediction:'<img>',used_outputs:1,diagnostics:[{accepted:true,parsed_numbers:300}],results:[{model:'<img>',probability:0.99,profile_similarity:0.8}]}}});const text=elements.codexModelTraceResult.innerHTML;assert.match(text,/1 \/ 3/);assert.match(text,/未收录/);assert.match(text,/库内相对权重/);assert.match(text,/&lt;script&gt;/);assert.doesNotMatch(text,/<script>|<img>/);assert.equal(elements.codexModelTraceExport.disabled,false);assert.equal(elements.codexModelTraceCancel.disabled,true)});
test('retired detection controls and API are absent',()=>{assert.match(html,/el\.dataset\.codexModeltrace/);assert.doesNotMatch(html,/codexSteering|steering-probes|response\.steer|检测上游能力/);new vm.Script(source)});

test('primary view omits internal metadata while JSON preserves the entire report',()=>{
 const {c,elements}=harness();
 const report={...pending,probe_id:'internal-probe-id',target_slot_id:'internal-slot-id',reasoning:'upstream_default',done:true,bank:{bank_sha256:'internal-bank-digest'},samples:[{state:'completed',usage:{input_tokens:1234567},output_text:'complete raw output'}]};
 c.renderCodexModelTraceReport(report);
 const text=elements.codexModelTraceResult.innerHTML;
 assert.doesNotMatch(text,/internal-probe-id|internal-slot-id|internal-bank-digest|input_tokens|1234567|complete raw output|准入原因|错误代码|upstream_default/);
 assert.match(text,/上游默认/);
 assert.deepEqual(JSON.parse(elements.codexModelTraceJSON.textContent),report);
});
test('all candidates remain available and unknown metrics are not shown as zero',()=>{
 const {c,elements}=harness();
 const results=Array.from({length:18},(_,i)=>({model:`candidate-${i}`,probability:i===0?0:null,profile_similarity:i===0?0:undefined}));
 c.renderCodexModelTraceReport({...pending,done:true,result:{requested_model_in_bank:true,analysis:{prediction:'candidate-0',used_outputs:0,results}}});
 const text=elements.codexModelTraceResult.innerHTML;
 assert.match(text,/其余候选模型（15）/);
 for(const item of results)assert.ok(text.includes(`<td>${item.model}</td>`));
 assert.match(text,/<td>candidate-0<\/td><td>0.00%<\/td><td>0.00%<\/td>/);
 assert.match(text,/<td>candidate-1<\/td><td>—<\/td><td>—<\/td>/);
 assert.doesNotMatch(text,/NaN|未收录|请求模型已收录|家族分布/);
});
test('failure reasons and sample positions survive compact rendering',()=>{
 const {c,elements}=harness();
 c.renderCodexModelTraceReport({...pending,done:true,error_code:'deadline_<task>',admission_reason:'limited_<account>',parsed_counts:[0,0,312],samples:[{state:'failed',error_code:'upstream_<error>'},{state:'not_started'},{state:'completed',expected_count:312}]});
 const text=elements.codexModelTraceResult.innerHTML;
 for(const value of ['deadline_&lt;task&gt;','limited_&lt;account&gt;','upstream_&lt;error&gt;','挑战 1','挑战 2','挑战 3','未执行','解析数量：312'])assert.ok(text.includes(value),value);
 assert.doesNotMatch(text,/行为指纹最接近|候选模型|<table|计入样本|<p>未计入/);
});
test('opening shows the account email without duplicating the slot identifier',async()=>{
 const {c,elements}=harness(()=>pending);
 c.state.codex.accounts[0].email='muidea@gmail.com';
 await c.openCodexModelTrace('slot');
 assert.equal(elements.codexModelTraceAccount.textContent,'muidea@gmail.com');
});
