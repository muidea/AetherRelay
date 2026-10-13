// Run with: node --test web/admin/language.test.cjs (no npm dependencies).
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const html=fs.readFileSync(`${__dirname}/index.html`,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].at(-1)[1];

class Element {
  constructor(text='',protectedContent=false){this.nodeType=1;this.attributes={};this.protectedContent=protectedContent;this.writes=0;this.textContent=text;}
  get textContent(){return this.firstChild?.nodeValue||'';}
  set textContent(value){this.firstChild={nodeType:3,nodeValue:value,parentElement:this};}
  closest(selector){return this.protectedContent?this:null;}
  getAttribute(name){return Object.hasOwn(this.attributes,name)?this.attributes[name]:null;}
  setAttribute(name,value){this.writes++;this.attributes[name]=value;}
  hasAttribute(name){return Object.hasOwn(this.attributes,name);}
  querySelectorAll(){return [];}
}
function harness(){
  let observe,observer;
  const context=vm.createContext({
    state:{locale:'zh-CN'},Node:{ELEMENT_NODE:1,TEXT_NODE:3},NodeFilter:{SHOW_TEXT:4},
    document:{body:new Element(),createTreeWalker:root=>{
      const nodes=root.firstChild?[root.firstChild]:[];let index=0;
      return {nextNode(){this.currentNode=nodes[index++];return !!this.currentNode;}};
    }},
    MutationObserver:class {constructor(callback){observer=callback;}observe(root,options){observe=options;}},
  });
  vm.runInContext(source.slice(source.indexOf('const enUS='),source.indexOf('const state=')),context);
  vm.runInContext(source.match(/^const esc=.*$/m)[0],context);
  vm.runInContext(source.match(/^function t\(.*$/m)[0],context);
  vm.runInContext(source.slice(source.indexOf('// Keep source keys'),source.indexOf('function renderLocale')),context);
  vm.runInContext(source.slice(source.indexOf('const localeObserver='),source.indexOf('// boot')),context);
  return {context,observe,notify:records=>observer(records)};
}

test('new text nodes and textContent edits translate and restore their source',()=>{
  const {context:c,notify,observe}=harness();c.state.locale='en-US';
  const element=new Element('  联网搜索 ');
  notify([{type:'childList',addedNodes:[element.firstChild]}]);
  assert.equal(element.textContent,'  Web search ');
  c.state.locale='zh-CN';c.translateDOM(element);
  assert.equal(element.textContent,'  联网搜索 ');
  element.firstChild.nodeValue='选择或创建对话';c.state.locale='en-US';
  notify([{type:'characterData',target:element.firstChild}]);
  assert.equal(element.textContent,'Select or create a conversation');
  assert.equal(observe.characterData,true);
  assert.ok(observe.attributeFilter.includes('title'));
});

test('attribute updates retain new values and repeated renders avoid mutation loops',()=>{
  const {context:c,notify}=harness();const element=new Element();element.setAttribute('title','当前并发');
  c.state.locale='en-US';c.translateDOM(element);assert.equal(element.getAttribute('title'),'Current concurrency');
  element.setAttribute('title','输出 Token');notify([{type:'attributes',target:element}]);
  assert.equal(element.getAttribute('title'),'Output tokens');
  const writes=element.writes;c.translateDOM(element);c.translateDOM(element);assert.equal(element.writes,writes);
  c.state.locale='zh-CN';c.translateDOM(element);assert.equal(element.getAttribute('title'),'输出 Token');
  delete element.attributes.title;c.translateDOM(element);assert.equal(element.getAttribute('title'),null);
});

test('metric tooltips keep the latest sample across both language directions',()=>{
  const {context:c}=harness();const element=new Element();let inflight=2;
  const render=()=>c.t('当前并发')+'：'+inflight;
  c.state.locale='en-US';c.setLocalizedAttribute(element,'title',render);inflight=5;
  c.setLocalizedAttribute(element,'title',render);c.translateDOM(element);
  assert.equal(element.getAttribute('title'),'Current concurrency：5');
  c.state.locale='zh-CN';c.translateDOM(element);assert.equal(element.getAttribute('title'),'当前并发：5');
  c.state.locale='en-US';c.translateDOM(element);assert.equal(element.getAttribute('title'),'Current concurrency：5');
});

test('parameterized text created in English restores Chinese without changing its account label',()=>{
  const {context:c}=harness();const element=new Element();c.state.locale='en-US';
  c.setLocalizedText(element,'编辑 {name}',{name:'关闭'});assert.equal(element.textContent,'Edit 关闭');
  c.state.locale='zh-CN';c.translateDOM(element);assert.equal(element.textContent,'编辑 关闭');
  c.setLocalizedText(element,'编辑 {name}',{name:'second account'});c.state.locale='en-US';c.translateDOM(element);
  assert.equal(element.textContent,'Edit second account');
});

test('user content, unknown English labels and action meanings remain intact',()=>{
  const {context:c}=harness();c.state.locale='en-US';const content=new Element('关闭',true);content.setAttribute('title','关闭');
  c.translateDOM(content);assert.equal(content.textContent,'关闭');assert.equal(content.getAttribute('title'),'关闭');
  const business=new Element('Start time');c.translateDOM(business);c.state.locale='zh-CN';c.translateDOM(business);
  assert.equal(business.textContent,'Start time');c.state.locale='en-US';
  assert.equal(c.t('关闭'),'Close');assert.equal(c.t('关闭指纹收敛'),'Off');
});

function loginHarness(){
  const auth=fs.readFileSync(`${__dirname}/../../internal/modules/application/adminapi/service/admin/auth.go`,'utf8');
  const script=auth.match(/const tpl = `[\s\S]*?<script>([\s\S]*?)<\/script>/)[1].replace('BASE_PATH_JSON','"/admin"').replace('DEFAULT_LANGUAGE_JSON','"zh-CN"');
  const elements={};let tick;
  const context=vm.createContext({window:{},document:{cookie:'',documentElement:{},getElementById:id=>elements[id]||=(new Element()),querySelectorAll:()=>[],querySelector:()=>elements.languageLabel||=(new Element('语言 '))},location:{search:'',replace(){}},navigator:{languages:['zh-CN']},URLSearchParams,Set,Number,setInterval:fn=>{tick=fn;return 1;},clearInterval(){},fetch:async()=>({ok:false,status:401,json:async()=>({})})});
  vm.runInContext(script,context);
  const select=locale=>{elements.language.onchange({target:{value:locale}});};
  return {context,elements,select,tick:()=>tick()};
}

test('login credential and network errors switch languages after they are displayed',async()=>{
  const {context:c,elements,select}=loginHarness();await elements.f.onsubmit({preventDefault(){}});
  assert.equal(elements.err.textContent,'用户名或密码错误');select('en-US');assert.equal(elements.err.textContent,'Invalid username or password');
  select('zh-CN');c.fetch=async()=>{throw new Error('offline');};await elements.f.onsubmit({preventDefault(){}});
  assert.equal(elements.err.textContent,'网络错误，请稍后重试');select('en-US');assert.equal(elements.err.textContent,'Network error. Try again later.');
});

test('login rate-limit countdown preserves remaining seconds during language switches',async()=>{
  const {context:c,elements,select,tick}=loginHarness();c.fetch=async()=>({ok:false,status:429,json:async()=>({}),headers:{get:()=>3}});
  await elements.f.onsubmit({preventDefault(){}});assert.equal(elements.err.textContent,'尝试过多，请 3 秒后重试');
  select('en-US');assert.equal(elements.err.textContent,'Too many attempts. Try again in 3 seconds.');tick();assert.equal(elements.err.textContent,'Too many attempts. Try again in 2 seconds.');
  select('zh-CN');assert.equal(elements.err.textContent,'尝试过多，请 2 秒后重试');tick();tick();assert.equal(elements.err.textContent,'');assert.equal(elements.btn.disabled,false);
});

test('translation templates preserve unknown placeholders and match dictionary parameters',()=>{
  const {context:c}=harness();c.state.locale='en-US';
  assert.equal(c.t('server rejected {field}'),'server rejected {field}');
  const dictionary=vm.runInContext('enUS',c);
  const parameters=value=>[...String(value).matchAll(/\{(\w+)\}/g)].map(m=>m[1]).sort();
  for(const [key,value] of Object.entries(dictionary))assert.deepEqual(parameters(value),parameters(key),key);
});

test('source-bound HTML updates open controls and escapes account content',()=>{
  const {context:c}=harness();const element=new Element('Edit <账号>');
  element.setAttribute('data-i18n-source','编辑 {name}');element.setAttribute('data-i18n-params',JSON.stringify({name:'<账号>'}));
  c.translateDOM(element);assert.equal(element.textContent,'编辑 <账号>');c.state.locale='en-US';c.translateDOM(element);
  assert.equal(element.textContent,'Edit <账号>');
  const markup=c.localizedHTML('编辑 {name}',{name:'<img src=x onerror="alert(1)">'});
  assert.ok(!markup.includes('<img'));assert.ok(markup.includes('&lt;img'));assert.ok(markup.includes('&quot;'));
});

test('populated TPS diagnostics localize partial output and buffering explanations',()=>{
  const {context:c}=harness();c.state.locale='en-US';
  for(const name of ['usageTPS','usageTPSHint'])vm.runInContext(source.match(new RegExp(`^function ${name}\\(.*$`,'m'))[0],c);
  const single=c.usageTPSHint({tps:1,observed_tps:200,estimated:true,generation_partial:true,generation_buffered:true});
  assert.ok(!/[\u3400-\u9fff]/.test(single));assert.match(single,/Estimated tokens/);assert.match(single,/Partial output/);assert.match(single,/buffering reported/);
  const summary=c.usageTPSHint({tps:1,observed_tps:200,tps_samples:10,tps_partial_samples:2,observed_tps_buffered_samples:3});
  assert.ok(!/[\u3400-\u9fff]/.test(summary));assert.match(summary,/10 valid samples/);assert.match(summary,/buffered delivery/);
});

test('provider health card keeps failure details separate from later success in both languages',()=>{
  const {context:c}=harness();
  c.formatDate=value=>value;c.formatNumber=value=>String(value);
  for(const name of ['providerAvailabilityMeta','providerFailureHint','providerStatusSummary']){
    const match=source.match(new RegExp(`^function ${name}\\([\\s\\S]*?(?=\\n(?:async )?function )`,'m'));
    vm.runInContext(match[0],c);
  }
  const availability={status:'healthy',score:90,sample_count:21,success_rate:1,p95_ms:49154,last_status:200,last_outcome:'success',last_failure_status:502,last_failure_outcome:'upstream_failed',last_failure_at:'2026-10-13T07:02:00Z',last_success_at:'2026-10-13T07:05:00Z'};
  const zh=c.providerStatusSummary({availability});assert.match(zh,/最近失败 · HTTP 502 · upstream_failed/);assert.match(zh,/最近成功/);
  c.state.locale='en-US';const en=c.providerStatusSummary({availability});
  assert.match(en,/<div class="health-note">Last failure · HTTP 502 · upstream_failed/);
  // API timestamps have second precision; recovery within the same second still clears the alert.
  const sameSecond=c.providerStatusSummary({availability:{...availability,last_success_at:availability.last_failure_at}});
  assert.match(sameSecond,/<div class="health-note">Last failure/);
  const failedAgain=c.providerStatusSummary({availability:{...availability,last_success_at:availability.last_failure_at,last_outcome:'upstream_failed',consecutive_failures:1}});
  assert.match(failedAgain,/<div class="health-alert">Last failure/);assert.match(en,/Last success/);assert.ok(!en.includes('HTTP 200 · success'));
  assert.equal(c.providerFailureHint({...availability,last_failure_status:200,last_failure_outcome:'upstream_truncated'}),'Last failure · HTTP 200 · upstream_truncated · 2026-10-13T07:02:00Z');
  delete availability.last_failure_status;delete availability.last_failure_outcome;
  assert.equal(c.providerFailureHint(availability),'Last failure · 2026-10-13T07:02:00Z');
});
