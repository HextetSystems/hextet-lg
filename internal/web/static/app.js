(function(){try{function bind(){var b=document.getElementById('theme');if(!b)return;
// The visitor's choice is kept in localStorage under "theme" ("light" or "dark") and overrides the configured default in this browser only. Remove the key (or clear site data) to return to the default.
b.onclick=function(){var d=document.documentElement,cur=d.dataset.theme;if(cur!=='light'&&cur!=='dark'){var def=d.dataset.themeDefault;cur=def==='light'||def==='dark'?def:(matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light')}var next=cur==='dark'?'light':'dark';d.dataset.theme=next;try{localStorage.setItem('theme',next)}catch(e){}}}
bind();
// Browser Back/Forward re-renders the page body from the server, which replaces the button.
document.addEventListener('htmx:historyRestore',bind)}catch(e){}})();
// Copy buttons confirm in their own label and in the #copy-status live region (when present), then restore the label.
document.addEventListener("click",e=>{const t=e.target.closest&&e.target.closest("[data-copy],[data-copy-link]");if(!t)return;
 const isLink=t.matches("[data-copy-link]"),src=isLink?null:document.getElementById(t.dataset.copy),text=isLink?location.href:src&&src.textContent;
 const label=t.dataset.label||t.textContent;t.dataset.label=label;const status=document.getElementById('copy-status');
 const say=(button,live)=>{t.textContent=button;if(status)status.textContent=live;clearTimeout(t._copyTimer);t._copyTimer=setTimeout(()=>{t.textContent=label;if(status)status.textContent=''},1800)};
 if(typeof text!=='string'||!navigator.clipboard){say('Copy unavailable','Copying is not available in this browser');return}
 navigator.clipboard.writeText(text).then(()=>say('Copied',isLink?'Link copied':'Text copied'),()=>say('Copy failed','Copy failed'))});
// Search form behavior is delegated from the document: browser Back/Forward re-renders the form from the server (see the history note in README), so handlers must not be bound to one form element.
(function(){function form(){return document.getElementById('search-form')}
function sync(){var f=form();if(!f)return;var n=f.elements.neighbor,g=f.elements.group;if(n&&g){g.disabled=n.value.trim()!=='';n.disabled=!g.disabled&&g.value.trim()!==''}}
document.addEventListener('change',function(e){var t=e.target,f=form();if(!f||!f.contains(t))return;if(t.type==='checkbox'&&t.checked){var grp=t.closest('[data-excl]').dataset.excl;f.querySelectorAll('[data-excl="'+grp+'"] input[type=checkbox]').forEach(function(o){if(o!==t)o.checked=false})}sync()});
document.addEventListener('input',function(e){var f=form();if(f&&f.contains(e.target))sync()});
document.body.addEventListener('htmx:afterSettle',sync);document.body.addEventListener('htmx:historyRestore',sync);sync()})();
document.body.addEventListener('htmx:beforeRequest',e=>{if(e.target.matches('form'))e.target.classList.add('loading')});document.body.addEventListener('htmx:afterRequest',e=>{if(e.target.matches('form'))e.target.classList.remove('loading')});

// Validation fragments are deliberately returned with HTTP 400.
document.body.addEventListener('htmx:beforeSwap',function(e){if(e.detail.xhr.status===400){e.detail.shouldSwap=true;e.detail.isError=false}});

// HTMX 2 swaps a history-restore response only for status 200-399 and otherwise fires this event, leaving the old page under the already-changed URL. A rejected search (HTTP 400) is a legitimate history entry, so fall back to an ordinary navigation of the current URL: the server still answers 400, and the browser shows that document exactly as a reload does. A full navigation sends no history-restore request, so this cannot loop.
document.body.addEventListener('htmx:historyCacheMissLoadError',function(){location.reload()});

// The form is outside the HTMX results target, so update its disclosure after each response.
document.body.addEventListener('htmx:afterRequest',function(e){if(e.target.id!=='search-form')return;var d=document.getElementById('advanced');if(e.detail.xhr.status===400){d.open=true;return}if(e.detail.xhr.status>=200&&e.detail.xhr.status<300){d.open=false;var n=0;var canonical=new URL(e.detail.xhr.getResponseHeader('HX-Push-Url')||location.href,location.href);d.querySelectorAll('input,select').forEach(function(x){if(x.name==='prefix'&&canonical.searchParams.get('type')==='prefix'&&canonical.searchParams.has('q'))return;if(!x.disabled&&((x.type==='checkbox'&&x.checked)||(x.type!=='checkbox'&&x.value.trim()!=='')))n++});document.getElementById('advanced-count').textContent=n?'('+n+' active)':''}});

// Status requests only read the server cache; the page request schedules DNS.
(function(){
 let generation=0;
 function update(){
  const current=++generation;
  let asns=Array.from(document.querySelectorAll('[data-asn]')).filter(n=>!n.title && /^[0-9]{1,10}$/.test(n.dataset.asn||''));
  asns.sort((a,b)=>(a.dataset.asnPriority==='0'?0:1)-(b.dataset.asnPriority==='0'?0:1));
  let ptrs=Array.from(document.querySelectorAll('[data-ptr]')).filter(n=>!n.title && typeof n.dataset.ptr==='string' && n.dataset.ptr.length<=45);
  ptrs.sort((a,b)=>(a.dataset.ptrPriority==='0'?0:1)-(b.dataset.ptrPriority==='0'?0:1));
  let groups=[{nodes:asns,key:'asn',url:'/asn-status?asn='},{nodes:ptrs,key:'ptr',url:'/ptr-status?ip='}];
  groups.forEach(g=>g.ids=Array.from(new Set(g.nodes.map(n=>n.dataset[g.key]))).slice(0,256));
  if(!groups.some(g=>g.ids.length))return;
  let attempts=0;
  function poll(){
   if(current!==generation||!groups.some(g=>g.nodes.some(n=>n.isConnected && !n.title)))return;
   let work=groups.filter(g=>g.ids.length && g.nodes.some(n=>n.isConnected && !n.title)).map(g=>fetch(g.url+encodeURIComponent(g.ids.join(',')),{headers:{'Accept':'application/json'},cache:'no-store'}).then(r=>r.ok?r.json():{}).then(data=>{
    if(current!==generation)return;
    g.nodes.forEach(n=>{const id=n.dataset[g.key],name=data&&data[id];if(!n.isConnected||typeof name!=='string'||!name)return;
    // A neighbor label (data-ptr-format) shows the name in place; its tooltip always carries name and address. Everything else only gets a tooltip.
    const fmt=g.key==='ptr'?n.dataset.ptrFormat:'';
    if(fmt==='name'||fmt==='both'){n.title=name+' ('+id+')';n.textContent=fmt==='both'?n.title:name}else n.title=name});
   }).catch(()=>{}));
   Promise.all(work).then(()=>{if(current===generation && ++attempts<24 && groups.some(g=>g.nodes.some(n=>n.isConnected && !n.title)))setTimeout(poll,1500)});
  }
  setTimeout(poll,500);
 }
 document.addEventListener('DOMContentLoaded',update);
 document.body.addEventListener('htmx:afterSwap',update);
 document.body.addEventListener('htmx:historyRestore',update);
 window.addEventListener('pagehide',()=>{generation++});
 window.addEventListener('pageshow',e=>{if(e.persisted)update()});
})();
