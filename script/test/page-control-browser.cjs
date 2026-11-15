// Optional actual-Chrome DOM check, independent of a running Outpost daemon.
// node script/test/page-control-browser.cjs [chrome executable] [pagecontrol/page.js]
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const {spawnSync} = require('node:child_process');
const chrome = process.argv[2] || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const runtime = process.argv[3] || path.resolve(__dirname, '../../../yoke/pkg/pagecontrol/page.js');
const ui = path.resolve(__dirname, '../../internal/agent/adminui/ui');
const html = fs.readFileSync(path.join(ui,'index.html'),'utf8');
const start = html.indexOf("  const netForm = el('form'");
const end = html.indexOf('  // --- Cluster LLM',start);
const styles = [...html.matchAll(/<style>([\s\S]*?)<\/style>/g)].map(match => match[0]).join("\n");
const fixture = `<!doctype html><html><head>${styles}</head><body>
<div id="page-content"><input id="existing-readonly" readonly><div id="networking"></div></div>
<button id="shape-button" hidden>Shape</button><dialog id="shape-dialog"><button id="shape-toggle">Toggle</button><button id="shape-close">Close</button><select id="shape-navigation"><option value="pair">Pair</option></select></dialog>
<pre id="result">pending</pre>
<script>${fs.readFileSync(runtime,'utf8')}</script>
<script>${fs.readFileSync(path.join(ui,'page-ui.js'),'utf8')}</script>
<script>
const calls=[];
const cfg={defaults:{},local_addr:'127.0.0.1:0'};
function el(tag, attrs={}, ...kids) {const n=document.createElement(tag);for(const [k,v] of Object.entries(attrs)){n.setAttribute(k,v);if(k==='value')n.value=v} n.append(...kids);return n;}
async function api(method,url,payload){calls.push({method,url,payload});return {restart_pending:true};}
function msg(){} function noteRestartPending(){}
OutpostPage.begin();
${html.slice(start,end)}
document.getElementById('networking').append(netForm,netBtn);
(async()=>{
 function check(value,message){if(!value)throw new Error(message);}
 await BashyPage.invoke('outpost.networking.draft',{admin_addr:'127.0.0.1:17778'});
 check(adminAddrIn.value==='127.0.0.1:17778','real field draft');
 check(calls.length===0,'draft must not persist');
 await BashyPage.invoke('outpost.shape-only',true);
 check(document.getElementById('page-content').inert,'ordinary region inert');
 check(adminAddrIn.readOnly,'field readonly');
 check(getComputedStyle(netBtn).visibility==='hidden','ordinary buttons hidden');
 check(!document.getElementById('shape-button').hidden,'Shape remains available');
 document.getElementById('shape-button').click();await Promise.resolve();
 check(document.getElementById('shape-dialog').open,'Shape button opens dialog');
 document.getElementById('shape-toggle').click();await Promise.resolve();
 check(!adminAddrIn.readOnly,'original writable field restored');
 check(getComputedStyle(netBtn).visibility!=='hidden','ordinary buttons restored');
 check(document.getElementById('existing-readonly').readOnly,'original readonly preserved');
 document.getElementById('shape-close').click();await Promise.resolve();
 check(!document.getElementById('shape-dialog').open,'close uses registered action');
 await BashyPage.invoke('outpost.networking.submit');
 netBtn.click();await new Promise(resolve=>setTimeout(resolve,0));
 check(calls.length===2 && JSON.stringify(calls[0])===JSON.stringify(calls[1]),'human/agent submit parity');
 check(calls[0].payload.admin_addr==='127.0.0.1:17778','submitted draft');
 await BashyPage.invoke('outpost.shape-only',true);OutpostPage.reset();
 check(!document.getElementById('page-content').inert && !adminAddrIn.readOnly,'login cleanup');
 check(BashyPage.describe().length===0,'registrations removed');
 document.getElementById('result').textContent='PASSED';
})().catch(e=>{document.getElementById('result').textContent='FAILED '+e.stack});
</script></body></html>`;
const temp=fs.mkdtempSync(path.join(os.tmpdir(),'outpost-page-browser-'));
try {
 const file=path.join(temp,'fixture.html');fs.writeFileSync(file,fixture);
 const result=spawnSync(chrome,['--headless','--no-first-run','--no-default-browser-check','--disable-gpu','--user-data-dir='+path.join(temp,'profile'),'--dump-dom','--virtual-time-budget=2000','file://'+file],{encoding:'utf8',timeout:30000,maxBuffer:4*1024*1024});
 if(result.error)throw result.error;
 const match=result.stdout.match(/<pre id="result">([\s\S]*?)<\/pre>/);
 if(result.status!==0 || !match || match[1]!=='PASSED')throw new Error(match?.[1] || result.stderr.slice(-2000));
 console.log('Outpost Chrome DOM tests passed');
} finally {fs.rmSync(temp,{recursive:true,force:true});}
