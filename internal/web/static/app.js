(function(){try{document.getElementById('copy-link').onclick=function(){if(navigator.clipboard)navigator.clipboard.writeText(location.href)};let t=localStorage.getItem('theme');if(t)document.documentElement.dataset.theme=t;document.getElementById('theme').onclick=function(){let d=document.documentElement;let dark=d.dataset.theme?d.dataset.theme==='dark':matchMedia('(prefers-color-scheme: dark)').matches;d.dataset.theme=dark?'light':'dark';localStorage.setItem('theme',d.dataset.theme)}}catch(e){}})();
document.addEventListener("click",e=>{if(e.target.matches("[data-copy]")){let el=document.getElementById(e.target.dataset.copy);if(navigator.clipboard)navigator.clipboard.writeText(el.textContent)}else if(e.target.matches("[data-copy-link]")){if(navigator.clipboard)navigator.clipboard.writeText(location.href)}});
(function(){var f=document.getElementById('search-form');if(!f)return;
function endpoint(){var s=f.elements.endpoint;return s?s.value:'rib'}
function sync(){var ep=endpoint();
f.querySelectorAll('[data-endpoints]').forEach(function(l){var eps=l.dataset.endpoints;if(!eps)return;var ok=eps.split(' ').indexOf(ep)>=0;l.hidden=!ok;l.querySelectorAll('input,select').forEach(function(i){i.disabled=!ok})});
var n=f.elements.neighbor,g=f.elements.group;if(n&&g){g.disabled=n.value.trim()!==''||!!g.closest('[hidden]');n.disabled=!g.disabled&&g.value.trim()!==''}}
f.addEventListener('change',function(e){var t=e.target;if(t.type==='checkbox'&&t.checked){var grp=t.closest('[data-excl]').dataset.excl;f.querySelectorAll('[data-excl="'+grp+'"] input[type=checkbox]').forEach(function(o){if(o!==t)o.checked=false})}sync()});
f.addEventListener('input',sync);document.body.addEventListener('htmx:afterSettle',sync);sync()})();
document.body.addEventListener('htmx:beforeRequest',e=>{if(e.target.matches('form'))e.target.classList.add('loading')});document.body.addEventListener('htmx:afterRequest',e=>{if(e.target.matches('form'))e.target.classList.remove('loading')});

// Validation fragments are deliberately returned with HTTP 400.
document.body.addEventListener('htmx:beforeSwap',function(e){if(e.detail.xhr.status===400){e.detail.shouldSwap=true;e.detail.isError=false}});
