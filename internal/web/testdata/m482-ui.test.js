const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
const app=fs.readFileSync('internal/web/static/app.js','utf8');
const settle=async()=>{for(let i=0;i<10;i++)await Promise.resolve()};

// ---- neighbor labels (PTR names replace the address in place) ----
const ptrSource=app.split('// Status requests only read the server cache; the page request schedules DNS.\n')[1];
function ptrPage(nodes){const events={},body={},timers=[],req=[];
 const document={querySelectorAll:s=>nodes.filter(n=>s==='[data-asn]'?n.dataset.asn:n.dataset.ptr),addEventListener:(n,f)=>events[n]=f,body:{addEventListener:(n,f)=>body[n]=f}};
 vm.runInNewContext(ptrSource,{document,window:{addEventListener(){}},fetch:url=>new Promise(r=>req.push({url,resolve:r})),setTimeout:f=>timers.push(f),encodeURIComponent,Promise});
 return {events,body,timers,req}}
const label=(ip,fmt)=>({dataset:{ptr:ip,ptrFormat:fmt},title:'',isConnected:true,textContent:ip});
const answer=(h,i,obj)=>h.req[i].resolve({ok:true,json:async()=>obj});

test('name format replaces the address with the name and keeps the address in the tooltip',async()=>{
 const n=label('198.51.100.1','name'),h=ptrPage([n]);h.events.DOMContentLoaded();h.timers.shift()();
 assert.equal(decodeURIComponent(h.req[0].url),'/ptr-status?ip=198.51.100.1');
 answer(h,0,{'198.51.100.1':'peer1.example.net'});await settle();
 assert.equal(n.textContent,'peer1.example.net');assert.equal(n.title,'peer1.example.net (198.51.100.1)');
});
test('both format shows name and address; IPv6 works',async()=>{
 const n=label('2001:db8::1','both'),h=ptrPage([n]);h.events.DOMContentLoaded();h.timers.shift()();
 answer(h,0,{'2001:db8::1':'v6.example.net'});await settle();
 assert.equal(n.textContent,'v6.example.net (2001:db8::1)');assert.equal(n.title,n.textContent);
});
test('unresolved and negative answers leave the address in place',async()=>{
 const n=label('198.51.100.1','name'),h=ptrPage([n]);h.events.DOMContentLoaded();h.timers.shift()();
 answer(h,0,{});await settle();
 assert.equal(n.textContent,'198.51.100.1');assert.equal(n.title,'');
});
test('next-hop nodes and neighbor labels share one request; only labels change text',async()=>{
 const nh={dataset:{ptr:'192.0.2.1'},title:'',isConnected:true,textContent:'192.0.2.1'},nb=label('192.0.2.1','name'),dup=label('192.0.2.1','both');
 const h=ptrPage([nh,nb,dup]);h.events.DOMContentLoaded();h.timers.shift()();
 assert.equal(h.req.length,1,'one status request for the shared address');
 assert.equal(decodeURIComponent(h.req[0].url),'/ptr-status?ip=192.0.2.1');
 answer(h,0,{'192.0.2.1':'nh1.example.net'});await settle();
 assert.equal(nh.textContent,'192.0.2.1');assert.equal(nh.title,'nh1.example.net');
 assert.equal(nb.textContent,'nh1.example.net');assert.equal(dup.textContent,'nh1.example.net (192.0.2.1)');
});
test('a stale or disconnected label is not rewritten',async()=>{
 const n=label('198.51.100.1','name'),h=ptrPage([n]);h.events.DOMContentLoaded();h.timers.shift()();
 n.isConnected=false;answer(h,0,{'198.51.100.1':'peer1.example.net'});await settle();
 assert.equal(n.textContent,'198.51.100.1');
});
test('a data-ptr-format on an ASN node is ignored',async()=>{
 const a={dataset:{asn:'13335',ptrFormat:'name'},title:'',isConnected:true,textContent:'AS13335'},h=ptrPage([a]);h.events.DOMContentLoaded();h.timers.shift()();
 answer(h,0,{'13335':'Cloudflare'});await settle();
 assert.equal(a.textContent,'AS13335');assert.equal(a.title,'Cloudflare');
});
test('history restore re-runs enrichment for the restored page',()=>{
 const n=label('198.51.100.1','name'),h=ptrPage([n]);
 assert.equal(typeof h.body['htmx:historyRestore'],'function');
 h.body['htmx:historyRestore']();assert.equal(h.timers.length,1);
});

// ---- search form handlers survive history restore (the form element is replaced) ----
const formSource=app.split('// Search form behavior is delegated')[1].split("document.body.addEventListener('htmx:beforeRequest'")[0];
function formPage(){
 const doc={},body={};let form=null;
 const mkForm=(fields)=>{const f={elements:fields,contains:e=>e&&e._form===f,listeners:[],boxes:[]};f.querySelectorAll=sel=>sel.includes('data-excl')?f.boxes.filter(b=>sel.includes('"'+b.group+'"')):[];return f};
 const document={getElementById:id=>id==='search-form'?form:null,addEventListener:(n,fn)=>doc[n]=fn,body:{addEventListener:(n,fn)=>body[n]=fn}};
 vm.runInNewContext('//'+formSource,{document});
 return {doc,body,setForm:f=>{form=f},mkForm}
}
const field=(value='')=>({value,disabled:false});
test('exclusive checkboxes and neighbor/group exclusion work on the restored form',()=>{
 const h=formPage();
 for(let round=0;round<2;round++){ // round 0: initial page; round 1: a new form element after history restore
  const neighbor=field(),group=field(),f=h.mkForm({neighbor,group});h.setForm(f);
  const best={type:'checkbox',checked:true,_form:f,closest:()=>({dataset:{excl:'state'}}),group:'state'},leaked={type:'checkbox',checked:true,_form:f,closest:()=>({dataset:{excl:'state'}}),group:'state'};
  f.boxes=[best,leaked];
  h.body['htmx:historyRestore']();                       // syncs the (new) form
  h.doc.change({target:best});
  assert.equal(leaked.checked,false,'round '+round+': the other box in the group is cleared');assert.equal(best.checked,true);
  neighbor.value='192.0.2.1';neighbor._form=f;h.doc.input({target:neighbor});
  assert.equal(group.disabled,true,'round '+round+': group disabled while neighbor is set');assert.equal(neighbor.disabled,false);
 }
});
test('events from outside the search form are ignored',()=>{
 const h=formPage(),f=h.mkForm({neighbor:field(),group:field()});h.setForm(f);
 const outside={type:'checkbox',checked:true,_form:null,closest:()=>{throw Error('must not be inspected')}};
 assert.doesNotThrow(()=>h.doc.change({target:outside}));assert.doesNotThrow(()=>h.doc.input({target:outside}));
});
test('pages without a search form do not throw',()=>{const h=formPage();assert.doesNotThrow(()=>{h.body['htmx:afterSettle']();h.doc.change({target:{}})})});

// ---- theme toggle is rebound after the body is re-rendered ----
test('theme button works again after history restore replaces it',()=>{
 const head=app.split('\ndocument.addEventListener("click"')[0];
 const root={dataset:{themeDefault:'light'}},events={},store=new Map();let button={onclick:null};
 const document={documentElement:root,getElementById:id=>id==='theme'?button:null,addEventListener:(n,f)=>events[n]=f};
 vm.runInNewContext(head,{document,localStorage:{setItem:(k,v)=>store.set(k,v)},matchMedia:()=>({matches:false})});
 button.onclick();assert.equal(root.dataset.theme,'dark');
 const replaced={onclick:null};button=replaced;      // server-rendered page body replaced the button
 assert.equal(replaced.onclick,null);events['htmx:historyRestore']();
 replaced.onclick();assert.equal(root.dataset.theme,'light');assert.equal(store.get('theme'),'light');
});
