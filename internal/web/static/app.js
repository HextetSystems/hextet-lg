(function(){try{document.getElementById('copy-link').onclick=function(){if(navigator.clipboard)navigator.clipboard.writeText(location.href)};let t=localStorage.getItem('theme');if(t)document.documentElement.dataset.theme=t;document.getElementById('theme').onclick=function(){let d=document.documentElement;let dark=d.dataset.theme?d.dataset.theme==='dark':matchMedia('(prefers-color-scheme: dark)').matches;d.dataset.theme=dark?'light':'dark';localStorage.setItem('theme',d.dataset.theme)}}catch(e){}})();
document.addEventListener("click",e=>{if(e.target.matches("[data-copy]")){let el=document.getElementById(e.target.dataset.copy);if(navigator.clipboard)navigator.clipboard.writeText(el.textContent)}else if(e.target.matches("[data-copy-link]")){if(navigator.clipboard)navigator.clipboard.writeText(location.href)}});
(function(){var f=document.getElementById('search-form');if(!f)return;
function sync(){var n=f.elements.neighbor,g=f.elements.group;if(n&&g){g.disabled=n.value.trim()!=='';n.disabled=!g.disabled&&g.value.trim()!==''}}
f.addEventListener('change',function(e){var t=e.target;if(t.type==='checkbox'&&t.checked){var grp=t.closest('[data-excl]').dataset.excl;f.querySelectorAll('[data-excl="'+grp+'"] input[type=checkbox]').forEach(function(o){if(o!==t)o.checked=false})}sync()});
f.addEventListener('input',sync);document.body.addEventListener('htmx:afterSettle',sync);sync()})();
document.body.addEventListener('htmx:beforeRequest',e=>{if(e.target.matches('form'))e.target.classList.add('loading')});document.body.addEventListener('htmx:afterRequest',e=>{if(e.target.matches('form'))e.target.classList.remove('loading')});

// Validation fragments are deliberately returned with HTTP 400.
document.body.addEventListener('htmx:beforeSwap',function(e){if(e.detail.xhr.status===400){e.detail.shouldSwap=true;e.detail.isError=false}});

// The form is outside the HTMX results target, so update its disclosure after each response.
document.body.addEventListener('htmx:afterRequest',function(e){if(e.target.id!=='search-form')return;var d=document.getElementById('advanced');if(e.detail.xhr.status===400){d.open=true;return}if(e.detail.xhr.status>=200&&e.detail.xhr.status<300){d.open=false;var n=0;var canonical=new URL(e.detail.xhr.getResponseHeader('HX-Push-Url')||location.href,location.href);d.querySelectorAll('input,select').forEach(function(x){if(x.name==='prefix'&&canonical.searchParams.get('type')==='prefix'&&canonical.searchParams.has('q'))return;if(!x.disabled&&((x.type==='checkbox'&&x.checked)||(x.type!=='checkbox'&&x.value.trim()!=='')))n++});document.getElementById('advanced-count').textContent=n?'('+n+' active)':''}});

// Status requests only read the server cache; the page request schedules DNS.
(function(){
 let generation=0;
 function update(){
  const current=++generation;
  let nodes=Array.from(document.querySelectorAll('[data-asn]')).filter(n=>!n.title && /^[0-9]{1,10}$/.test(n.dataset.asn));
  nodes.sort((a,b)=>(a.dataset.asnPriority==='0'?0:1)-(b.dataset.asnPriority==='0'?0:1));
  let ids=Array.from(new Set(nodes.map(n=>n.dataset.asn))).slice(0,256);
  if(!ids.length)return;
  let attempts=0;
  function poll(){
   if(current!==generation||!nodes.some(n=>n.isConnected))return;
   fetch('/asn-status?asn='+encodeURIComponent(ids.join(',')),{headers:{'Accept':'application/json'},cache:'no-store'}).then(r=>r.ok?r.json():{}).then(data=>{
    if(current!==generation)return;
    nodes.forEach(n=>{if(n.isConnected&&data[n.dataset.asn])n.title=data[n.dataset.asn]});
    if(++attempts<24 && nodes.some(n=>n.isConnected && !n.title))setTimeout(poll,1500);
   }).catch(()=>{});
  }
  setTimeout(poll,500);
 }
 document.addEventListener('DOMContentLoaded',update);
 document.body.addEventListener('htmx:afterSwap',update);
 window.addEventListener('pagehide',()=>{generation++});
})();
