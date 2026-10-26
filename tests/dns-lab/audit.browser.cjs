// Read-only local-lab acceptance. Temporary Console peers are always stopped;
// agent processes and their retained histories are never restarted or changed.
'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path'),http=require('node:http'),net=require('node:net'),crypto=require('node:crypto');
const {spawn}=require('node:child_process');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const dir=process.argv.find(arg=>arg.startsWith('--state-dir='))?.slice(12);
if(!dir||!path.isAbsolute(dir))throw Error('Explicit --state-dir=/absolute/owned-lab-runtime required');
const state=JSON.parse(fs.readFileSync(path.join(dir,'state.json')));
assert.match(state.owner,/^[a-f0-9]{8}$/);for(const c of state.clusters)assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);
assert.equal(state.consoleURL,'http://127.0.0.1:8791');
const artifacts=path.resolve(process.env.ARTIFACTS_DIR||'artifacts/federated-audit');fs.mkdirSync(artifacts,{recursive:true});
const report={startedAt:new Date().toISOString(),errors:[],status:'RUNNING'},children=[];
const wait=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const get=async base=>{const r=await fetch(base+'/api/v1/audit?namespace=demo');assert.equal(r.status,200);return r.json();};
const canonical=events=>events.map(event=>JSON.stringify(event)).sort();
const freePort=()=>new Promise((resolve,reject)=>{const server=net.createServer();server.on('error',reject);server.listen(0,'127.0.0.1',()=>{const port=server.address().port;server.close(()=>resolve(port));});});
(async()=>{
  let browser,unavailable;
  try{
    const main=await get(state.consoleURL);
    assert.equal(main.partial,false);assert.equal(main.sources.length,2);assert.ok(main.sources.every(source=>source.available));
    const beforeFile=path.join(dir,'audit-before-console-update.json');
    if(fs.existsSync(beforeFile)){const retained=new Set(canonical(main.events));for(const old of canonical(JSON.parse(fs.readFileSync(beforeFile)).events))assert.ok(retained.has(old),'Console update lost retained agent history');}
    const cfg=JSON.parse(fs.readFileSync(path.join(dir,'console.json')));cfg.writesEnabled=false;cfg.auth.localAdmin=false;cfg.clusters.reverse();
    async function peer(config,suffix){
      config.listen='127.0.0.1:'+await freePort();const file=path.join(dir,'audit-peer-'+suffix+'.json');fs.writeFileSync(file,JSON.stringify(config),{mode:0o600});
      const child=spawn(path.join(dir,'console'),['--config',file],{stdio:'ignore'});children.push(child);
      const base='http://'+config.listen;
      for(let i=0;i<100;i++){if(child.exitCode!==null)throw Error('Temporary Console exited before ready');try{const r=await fetch(base+'/healthz');if(r.status===200)return base;}catch{}await wait(50);}
      throw Error('Temporary Console did not become ready');
    }
    const replica=await peer(structuredClone(cfg),'all-sources'),other=await get(replica);
    assert.deepEqual(other.events,main.events,'Different Console copies returned different event order or contents');assert.deepEqual(other.sources,main.sources);
    unavailable=http.createServer((req,res)=>res.writeHead(503).end());await new Promise(resolve=>unavailable.listen(0,'127.0.0.1',resolve));
    const partialConfig=structuredClone(cfg);partialConfig.clusters.find(c=>c.id==='west').url='http://127.0.0.1:'+unavailable.address().port;
    const partialBase=await peer(partialConfig,'unavailable-source'),partial=await get(partialBase);
    assert.equal(partial.partial,true);assert.equal(partial.sources.find(s=>s.clusterId==='west').available,false);assert.ok(partial.events.every(e=>e.clusterId==='east'));
    browser=await chromium.launch({headless:true});const page=await browser.newPage({viewport:{width:1440,height:1000}});page.on('pageerror',e=>report.errors.push(e.message));
    async function show(base){await page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});await page.getByRole('button',{name:'Audit',exact:true}).click();await page.locator('.k8gb-audit-sources').waitFor();assert.equal(await page.locator('dialog').count(),0);}
    await show(state.consoleURL);assert.match(await page.locator('.k8gb-audit-sources').innerText(),/All sources responded \/ 2 of 2/);await page.screenshot({path:path.join(artifacts,'all-sources-light.png'),fullPage:true});
    await show(partialBase);assert.match(await page.locator('.k8gb-audit-sources').innerText(),/Partial audit \/ 1 of 2/);await page.screenshot({path:path.join(artifacts,'partial-light.png'),fullPage:true});
    await page.evaluate(()=>{document.documentElement.dataset.theme='dark';});await page.screenshot({path:path.join(artifacts,'partial-dark.png'),fullPage:true});
    await page.setViewportSize({width:390,height:844});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await page.screenshot({path:path.join(artifacts,'partial-mobile.png'),fullPage:true});
    const final=await get(state.consoleURL);assert.deepEqual(final.events,main.events,'Read-only audit review mutated history');assert.deepEqual(report.errors,[]);
    report.status='PASS';report.retainedEvents=main.events.length;report.sameEventsAcrossTwoConsoles=true;report.originalHistoryPreserved=true;report.partialSourceVisible=true;report.eventHash=crypto.createHash('sha256').update(JSON.stringify(canonical(main.events))).digest('hex');
  }catch(error){report.status='FAIL';report.error=error.message;process.exitCode=1;}
  finally{await browser?.close();for(const child of children){if(child.exitCode===null){child.kill('SIGTERM');await Promise.race([new Promise(resolve=>child.once('exit',resolve)),wait(3000)]);if(child.exitCode===null)child.kill('SIGKILL');}}if(unavailable)await new Promise(resolve=>unavailable.close(resolve));report.finishedAt=new Date().toISOString();fs.writeFileSync(path.join(artifacts,'acceptance.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report));}
})();
