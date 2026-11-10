const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs'),vm=require('node:vm');
const app=fs.readFileSync('internal/web/static/app.js','utf8');

// The history fallback is the statement that follows its explanatory comment.
const marker="document.body.addEventListener('htmx:historyCacheMissLoadError',";
function load(){
 const i=app.indexOf(marker);assert.ok(i>=0,'history error handler is present in app.js');
 const src=app.slice(i,app.indexOf('\n',i));
 const handlers={},calls={reload:0};
 vm.runInNewContext(src,{document:{body:{addEventListener:(n,f)=>handlers[n]=f}},location:{reload:()=>calls.reload++}});
 return {handlers,calls};
}

test('a failed history restore falls back to an ordinary navigation of the current URL',()=>{
 const {handlers,calls}=load();
 assert.equal(typeof handlers['htmx:historyCacheMissLoadError'],'function');
 handlers['htmx:historyCacheMissLoadError']({detail:{path:'/?q=x',xhr:{status:400}}});
 assert.equal(calls.reload,1);
});

test('the handler is registered once and only for the history error event',()=>{
 assert.equal(app.split(marker).length-1,1);
 assert.ok(!/historyCacheSize\s*[:=]\s*[1-9]/.test(app),'app.js must not re-enable history snapshots');
});

test('snapshots stay disabled in the page configuration',()=>{
 const tpl=fs.readFileSync('internal/web/templates/index.html','utf8');
 assert.match(tpl,/"historyCacheSize":0/);
});
