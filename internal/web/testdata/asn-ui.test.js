const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('internal/web/static/app.js','utf8').split('// Status requests only read the server cache; the page request schedules DNS.\n')[1];
function harness(nodes=[]) {
 const docEvents={},bodyEvents={},winEvents={},timers=[], requests=[];
 const document={querySelectorAll:()=>nodes,addEventListener:(n,f)=>docEvents[n]=f,body:{addEventListener:(n,f)=>bodyEvents[n]=f}};
 const window={addEventListener:(n,f)=>winEvents[n]=f};
 const fetch=(url)=>new Promise((resolve,reject)=>requests.push({url,resolve,reject}));
 vm.runInNewContext(source,{document,window,fetch,setTimeout:f=>timers.push(f),encodeURIComponent});
 return {docEvents,bodyEvents,winEvents,timers,requests,setNodes:n=>{nodes=n},tick(){assert.ok(timers.length);timers.shift()()}};
}
function node(asn,priority) {return {dataset:{asn,asnPriority:priority},title:'',isConnected:true,textContent:asn,href:'/?type=asn&q='+asn}}
const settle = async()=>{for(let i=0;i<6;i++)await Promise.resolve()};
test('batch updates duplicate ASNs while preserving text and links',async()=>{
 const a=node('13335','0'),b=node('13335'),c=node('174');const h=harness([c,a,b]);h.docEvents.DOMContentLoaded();h.tick();
 assert.equal(decodeURIComponent(h.requests[0].url),'/asn-status?asn=13335,174');
 h.requests[0].resolve({ok:true,json:async()=>({'13335':'Cloudflare','174':'Cogent'})});await settle();
 assert.equal(a.title,'Cloudflare');assert.equal(b.title,'Cloudflare');assert.equal(c.title,'Cogent');assert.equal(a.textContent,'13335');assert.equal(a.href,'/?type=asn&q=13335');
});
test('HTMX navigation ignores old responses and detached elements',async()=>{
 const old=node('13335');const h=harness([old]);h.docEvents.DOMContentLoaded();h.tick();const fresh=node('15169');old.isConnected=false;h.setNodes([fresh]);h.bodyEvents['htmx:afterSwap']();h.tick();
 h.requests[0].resolve({ok:true,json:async()=>({'13335':'stale'})});h.requests[1].resolve({ok:true,json:async()=>({'15169':'new'})});await settle();assert.equal(old.title,'');assert.equal(fresh.title,'new');
});
test('back-forward cache resumes updates and invalidates prior requests',async()=>{
 const a=node('13335');const h=harness([a]);h.docEvents.DOMContentLoaded();h.tick();h.winEvents.pagehide();h.winEvents.pageshow({persisted:true});h.tick();
 h.requests[0].resolve({ok:true,json:async()=>({'13335':'stale'})});h.requests[1].resolve({ok:true,json:async()=>({'13335':'fresh'})});await settle();assert.equal(a.title,'fresh');
});
test('disabled, unsuccessful, malformed and failed responses leave numbers alone',async()=>{
 const off=harness();off.docEvents.DOMContentLoaded();assert.equal(off.timers.length,0);
 for(const response of [{ok:false},{ok:true,json:async()=>null},{ok:true,json:async()=>({'13335':{bad:true}})},{ok:true,json:async()=>{throw Error('bad JSON')}}]) {
  const a=node('13335');const h=harness([a]);h.docEvents.DOMContentLoaded();h.tick();h.requests[0].resolve(response);await settle();assert.equal(a.title,'');assert.equal(a.textContent,'13335');
 }
 const a=node('13335');const h=harness([a]);h.docEvents.DOMContentLoaded();h.tick();h.requests[0].reject(Error('offline'));await settle();assert.equal(a.title,'');
});
