// Opt-in live lab acceptance: create both management modes on east + west,
// exclude/restore only the new native demo, preserving the original user objects.
'use strict';
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {execFileSync:run}=require('node:child_process');
const http=require('node:http');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
if(!process.argv.includes('--run-mutations'))throw Error('Explicit --run-mutations required; creates persistent demo objects');
const stateDir=process.argv.find(arg=>arg.startsWith('--state-dir='))?.slice(12);
if(!stateDir||!path.isAbsolute(stateDir))throw Error('Use --state-dir=/absolute/owned-lab-runtime');
const state=JSON.parse(fs.readFileSync(path.join(stateDir,'state.json')));
assert.match(state.owner,/^[a-f0-9]{8}$/);
for(const c of state.clusters)assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);
const base='http://127.0.0.1:8791',artifacts=path.resolve(process.env.ARTIFACTS_DIR||'artifacts/multisite-review');
fs.mkdirSync(artifacts,{recursive:true});
const resume=process.argv.includes('--resume-site-test');
const report=resume?JSON.parse(fs.readFileSync(path.join(artifacts,'acceptance.json'))):{startedAt:new Date().toISOString(),operations:[],errors:[],status:'RUNNING'};
const resumeAfterExclude=resume&&report.operations.length===3;
if(resume){assert.equal(report.status,'FAIL');assert.ok([2,3].includes(report.operations.length),'Resume is only allowed after confirmed creation or exclusion');assert.ok(report.operations.slice(0,2).every(o=>o.result.targets.length===2&&o.result.targets.every(t=>t.result?.status==='applied')));if(resumeAfterExclude){const last=report.operations[2];assert.equal(last.provider,'native');assert.equal(last.operation,'excludeSite');assert.equal(last.result.targets.length,1);assert.equal(last.result.targets[0].clusterId,'east');assert.equal(last.result.targets[0].result?.status,'applied');}report.resumedAt=new Date().toISOString();report.status='RUNNING';delete report.error;delete report.warning;}
const save=()=>fs.writeFileSync(path.join(artifacts,'acceptance.json'),JSON.stringify(report,null,2)+'\n');
const wait=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const get=async endpoint=>{const r=await fetch(base+endpoint);assert.equal(r.status,200);return r.json();};
const overview=()=>get('/api/v1/overview?namespace=demo');
const signature=group=>group.instances.map(row=>({cluster:row.clusterId,uid:row.resource.uid,policy:row.policy,traffic:row.traffic}));
const nativeIngress=()=>{const c=state.clusters.find(c=>c.id==='east');return JSON.parse(run('kubectl',['--kubeconfig',path.join(stateDir,'east.kubeconfig'),'--context','kind-'+c.name,'-n','demo','get','ingress','native-new','-o','json'],{encoding:'utf8'}));};
const localHTTP=(port,hostname)=>new Promise((resolve,reject)=>{const req=http.get({host:'127.0.0.1',port,path:'/',headers:{Host:hostname}},res=>{res.resume();res.on('end',()=>resolve(res.statusCode));});req.setTimeout(5000,()=>req.destroy(Error('HTTP timeout')));req.on('error',reject);});
async function dns(hostname,ips){
  let samples,consecutive=0;
  for(let attempt=0;attempt<90;attempt++){
    samples=state.clusters.flatMap(c=>[false,true].map(tcp=>{
      const raw=run('dig',['@127.0.0.1','-p',String(c.dnsPort),hostname,'A','+noall','+answer','+time=2','+tries=1',...(tcp?['+tcp']:[])],{encoding:'utf8'});
      const addresses=raw.trim().split('\n').map(line=>line.split(/\s+/)).filter(parts=>parts[0]===hostname+'.'&&parts[3]==='A').map(parts=>parts[4]).sort();
      return {site:c.id,protocol:tcp?'tcp':'udp',raw,addresses};
    }));
    if(samples.every(sample=>JSON.stringify(sample.addresses)===JSON.stringify([...ips].sort())))consecutive++;else consecutive=0;
    if(consecutive===2)return samples;
    await wait(1000);
  }
  throw Error('DNS did not converge: '+JSON.stringify(samples));
}
(async()=>{
  let browser;
  try{
    const initial=await overview(),catalog=await get('/api/v1/catalog?namespace=demo');
    const original=initial.groups.filter(g=>['native.apps.k8gb.test','hello.apps.k8gb.test'].includes(g.hostname));
    assert.equal(original.length,2);
    if(!resume)assert.ok(!initial.groups.some(g=>['native-new.apps.k8gb.test','hello-new.apps.k8gb.test'].includes(g.hostname)),'Demo targets were already used; not resetting them');
    else for(const operation of report.operations.slice(0,2))for(const target of operation.result.targets){
      const group=initial.groups.find(g=>g.hostname===(operation.provider==='native'?'native-new.apps.k8gb.test':'hello-new.apps.k8gb.test'));
      const instance=group?.instances.find(i=>i.clusterId===target.clusterId),resource=operation.provider==='native'?instance?.resource:instance?.source;
      assert.equal(resource?.uid,target.result.resource.uid,'Previously created object was replaced; refusing resume');assert.equal(instance?.traffic.excluded,resumeAfterExclude&&operation.provider==='native'&&target.clusterId==='east');
    }
    for(const site of catalog.sites){
      assert.ok(site.catalog.candidates.some(c=>c.provider==='native'&&c.resource.name==='native-new'&&(resume?c.siteTrafficEligible&&c.enabled:c.eligible&&!c.enabled)));
      assert.ok(site.catalog.candidates.some(c=>c.provider==='traefik'&&c.resource.name==='hello-new'&&c.eligible&&c.enabled===resume));
    }
    const before=nativeIngress();
    if(resumeAfterExclude){
      assert.equal(before.metadata.uid,report.operations[2].result.targets[0].result.resource.uid);
      const snapshot=JSON.parse(before.metadata.annotations['k8gb-ui.io/native-site-state']);assert.equal(snapshot.ingressUID,before.metadata.uid);assert.equal(snapshot.version,1);assert.ok(Array.isArray(snapshot.rules));
      before.spec.rules=snapshot.rules;delete before.metadata.annotations['k8gb-ui.io/native-site-state'];
      if(snapshot.excluded===null)delete before.metadata.annotations['k8gb-ui.io/site-excluded'];else before.metadata.annotations['k8gb-ui.io/site-excluded']=snapshot.excluded;
    }
    browser=await chromium.launch({headless:true});
    const page=await browser.newPage({viewport:{width:1440,height:1100}});
    page.on('pageerror',error=>report.errors.push(error.message));
    const navigate=async group=>{await page.goto(base+'/?namespace=demo'+(group?'#group='+group.id:''),{waitUntil:'networkidle'});await page.waitForFunction(()=>document.getElementById('refresh').getAttribute('aria-busy')==='false');};
    async function apply(label,targets,provider,operation){
      await page.locator('#mutationReason').fill('Multisite live acceptance '+report.startedAt+': '+label);
      const planned=page.waitForResponse(r=>r.url().endsWith('/mutations/plan')&&r.request().method()==='POST');
      await page.getByRole('button',{name:'Preview changes',exact:true}).click();
      const batch=await(await planned).json();
      assert.equal(batch.canApply,true,JSON.stringify(batch.targets.map(row=>row.error)));assert.equal(batch.targets.length,targets);
      assert.ok(batch.targets.every(row=>row.plan.request.provider===provider&&row.plan.request.operation===operation));
      await page.locator('#mutationConsent').check();
      if(operation==='excludeSite'){
        await page.locator('#mutationDisableConsent').check();
        assert.equal(await page.getByRole('button',{name:'Apply changes',exact:true}).isDisabled(),true);
        await page.locator('#mutationLocalConsent').check();
      }
      const applied=page.waitForResponse(r=>r.url().endsWith('/mutations/apply')&&r.request().method()==='POST');
      await page.getByRole('button',{name:'Apply changes',exact:true}).click();
      const result=await(await applied).json();report.operations.push({label,provider,operation,result});save();
      assert.equal(result.targets.length,targets);assert.ok(result.targets.every(row=>row.result?.status==='applied'),JSON.stringify(result));
      await page.getByRole('heading',{name:'Apply result',exact:true}).waitFor();
      await page.getByRole('button',{name:'Close',exact:true}).click();
    }
    for(const mode of resume?[]:['native','traefik']){
      await navigate();
      await page.getByRole('button',{name:mode==='native'?'Create native GSLB':'Create GSLB from IngressRoute',exact:true}).click();
      await page.locator('#mutationCandidate').waitFor();
      const option=(await page.locator('#mutationCandidate option').evaluateAll(nodes=>nodes.map(n=>({value:n.value,text:n.textContent})))).find(row=>row.text.includes(mode==='native'?'native-new':'hello-new'));
      assert.ok(option);await page.locator('#mutationCandidate').selectOption(option.value);
      assert.equal(await page.locator('#mutationClusterTargets input:checked').count(),2);
      if(mode==='native')await page.locator('#mutationName').fill('native-multisite');
      await page.locator('#mutationTTL').fill('5');
      await page.locator('#k8gbMutationDNS .k8gb-dns-graphic').waitFor();
      await page.screenshot({path:path.join(artifacts,mode+'-create.png'),fullPage:true});
      await apply('Create '+mode+' on east and west',2,mode,mode==='native'?'create':'enable');
      report.operations.at(-1).dns=await dns(mode==='native'?'native-new.apps.k8gb.test':'hello-new.apps.k8gb.test',state.clusters.map(c=>c.ip));save();
    }
    let current=await overview(),native=current.groups.find(g=>g.hostname==='native-new.apps.k8gb.test');
    assert.equal(native.instances.length,2);
    if(!resumeAfterExclude){
      await navigate(native);
      await page.locator('[data-traffic-mode="native-excludeSite"][data-traffic-instance="0"]').click();await page.locator('#mutationReason').waitFor();
      assert.match(await page.locator('#mutationForm').innerText(),/LOCAL INGRESS ROUTING WILL STOP/);
      await page.screenshot({path:path.join(artifacts,'native-local-warning.png'),fullPage:true});
      await apply('Exclude east native route with explicit local impact',1,'native','excludeSite');
    }
    report.operations.at(-1).dns=await dns(native.hostname,[state.clusters.find(c=>c.id==='west').ip]);
    const local=await localHTTP(18091,native.hostname),remote=await localHTTP(18092,native.hostname);
    report.operations.at(-1).http={east:local,west:remote};save();
    assert.ok(local>=400);assert.equal(remote,200);
    current=await overview();native=current.groups.find(g=>g.id===native.id);
    assert.equal(native.instances.find(row=>row.clusterId==='east').traffic.excluded,true);
    assert.ok(native.instances.find(row=>row.clusterId==='east').backends.every(row=>row.health==='Excluded'));
    await navigate(native);
    await page.locator('[data-traffic-mode="native-includeSite"][data-traffic-instance="0"]').click();await page.locator('#mutationReason').waitFor();
    await apply('Restore exact east native Ingress backend snapshot',1,'native','includeSite');
    report.operations.at(-1).dns=await dns(native.hostname,state.clusters.map(c=>c.ip));save();
    const after=nativeIngress();assert.equal(after.metadata.uid,before.metadata.uid);assert.deepEqual(after.spec,before.spec);assert.deepEqual(after.metadata.annotations,before.metadata.annotations);
    const restored=await localHTTP(18091,native.hostname);assert.equal(restored,200);
    const final=await overview();for(const group of original)assert.deepEqual(signature(final.groups.find(g=>g.id===group.id)),signature(group),'Original user configuration changed');
    await navigate();await page.screenshot({path:path.join(artifacts,'resources.png'),fullPage:true});
    assert.deepEqual(report.errors,[]);report.status='PASS';report.created=['native-multisite on east and west','hello-new IngressRoute enabled on east and west'];
  }catch(error){report.status='FAIL';report.error=error.message;report.warning='No blind retry, cleanup or restore after unknown apply. Inspect recorded results before manual recovery.';process.exitCode=1;}
  finally{report.finishedAt=new Date().toISOString();save();await browser?.close();console.log(JSON.stringify({status:report.status,error:report.error,operations:report.operations.length,artifacts}));}
})();
