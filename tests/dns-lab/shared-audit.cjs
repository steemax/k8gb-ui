// Owned-lab acceptance: four agent replicas, shared local journals, then an
// east agent rollout. Creates audit plans only; never applies application edits.
'use strict';
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict'),net=require('node:net'),crypto=require('node:crypto');
const {execFileSync:run,spawn}=require('node:child_process');
if(!process.argv.includes('--run-plans'))throw Error('Explicit --run-plans required; writes audit records and restarts only the east lab agent');
const dir=process.argv.find(a=>a.startsWith('--state-dir='))?.slice(12);if(!dir||!path.isAbsolute(dir))throw Error('Explicit owned --state-dir required');
const state=JSON.parse(fs.readFileSync(path.join(dir,'state.json')));assert.match(state.owner,/^[a-f0-9]{8}$/);for(const c of state.clusters)assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);
const artifacts=path.resolve(process.env.ARTIFACTS_DIR||'artifacts/shared-audit');fs.mkdirSync(artifacts,{recursive:true});
const report={startedAt:new Date().toISOString(),status:'RUNNING',plans:[],replicas:[]},children=[];
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const canonical=events=>events.map(e=>JSON.stringify(e)).sort();
const digest=events=>crypto.createHash('sha256').update(JSON.stringify(canonical(events))).digest('hex');
const kube=(id,args)=>run('kubectl',['--kubeconfig',path.join(dir,id+'.kubeconfig'),'--context','kind-'+state.clusters.find(c=>c.id===id).name,...args],{encoding:'utf8',timeout:180000});
const freePort=()=>new Promise((resolve,reject)=>{const server=net.createServer();server.on('error',reject);server.listen(0,'127.0.0.1',()=>{const port=server.address().port;server.close(()=>resolve(port));});});
const get=async url=>{const r=await fetch(url);assert.equal(r.status,200);return r.json();};
const overview=()=>get('http://127.0.0.1:8791/api/v1/overview?namespace=demo');
const signature=o=>o.groups.map(g=>({id:g.id,instances:g.instances.map(i=>({cluster:i.clusterId,uid:i.resource.uid,policy:i.policy,traffic:i.traffic}))}));
function pods(id){const rows=JSON.parse(kube(id,['-n','k8gb-ui-system','get','pods','-l','app=k8gb-ui-agent','-o','json'])).items.filter(p=>!p.metadata.deletionTimestamp&&p.status.conditions?.some(c=>c.type==='Ready'&&c.status==='True'));assert.equal(rows.length,2,'Expected exactly two ready agent replicas');return rows;}
async function connect(id,pod){
  const port=await freePort(),child=spawn('kubectl',['--kubeconfig',path.join(dir,id+'.kubeconfig'),'--context','kind-'+state.clusters.find(c=>c.id===id).name,'-n','k8gb-ui-system','port-forward','--address','127.0.0.1','pod/'+pod.metadata.name,port+':8081'],{stdio:'ignore'});children.push(child);
  const base='http://127.0.0.1:'+port;
  for(let n=0;n<100;n++){if(child.exitCode!==null)throw Error('Agent port-forward stopped');try{const r=await fetch(base+'/healthz');if(r.status===200)break;}catch{}await wait(100);}
  const token=name=>fs.readFileSync(path.join(dir,id+'.'+name),'utf8').trim();
  return {id,pod:pod.metadata.name,uid:pod.metadata.uid,async request(endpoint,body){const r=await fetch(base+endpoint,{method:body?'POST':'GET',headers:{Authorization:'Bearer '+token(body?'write-token':'read-token'),...(body?{'Content-Type':'application/json'}:{})},...(body?{body:JSON.stringify(body)}:{})});return {status:r.status,body:await r.json()};}};
}
(async()=>{try{
  const before=await overview(),audit=await get('http://127.0.0.1:8791/api/v1/audit?namespace=demo');
  assert.equal(audit.partial,false);assert.ok(audit.sources.every(s=>s.retention?.storage==='configMap'));
  const connections=[];for(const c of state.clusters)for(const pod of pods(c.id))connections.push(await connect(c.id,pod));
  const marker='Shared audit replica acceptance '+report.startedAt;
  for(const replica of connections){
    const snapshot=await replica.request('/api/v1/snapshot?namespace=demo');assert.equal(snapshot.status,200);
    const object=snapshot.body.objects.find(o=>o.resource.name==='native-web'&&o.management==='native');assert.ok(object);
    const result=await replica.request('/api/v1/mutations/plan',{provider:'native',operation:'update',resource:object.resource,policy:object.policy,actor:'local-audit-acceptance',reason:marker+' / '+replica.id+' / '+replica.pod});
    report.plans.push({cluster:replica.id,pod:replica.pod,status:result.status,code:result.body.code});
    assert.ok(result.status===200||result.status===409,'Unexpected planning failure; no apply is attempted');
  }
  const saved={};
  for(const replica of connections){const result=await replica.request('/api/v1/audit?namespace=demo');assert.equal(result.status,200);assert.equal(result.body.partial||false,false);assert.equal(result.body.retention.storage,'configMap');assert.ok(result.body.retention.retainedBytes<=700*1024);if(saved[replica.id])assert.deepEqual(result.body,saved[replica.id]);else saved[replica.id]=result.body;report.replicas.push({cluster:replica.id,pod:replica.pod,hash:digest(result.body.events),journal:result.body.retention.configMap});}
  const merged=await get('http://127.0.0.1:8791/api/v1/audit?namespace=demo');assert.equal(merged.events.filter(e=>e.reason.startsWith(marker)).length,8,'Every plan lifecycle must be visible across all Console copies');
  const oldEast=pods('east').map(p=>p.metadata.uid);
  kube('east',['-n','k8gb-ui-system','rollout','restart','deployment/k8gb-ui-agent']);kube('east',['-n','k8gb-ui-system','rollout','status','deployment/k8gb-ui-agent','--timeout=120s']);
  for(const pod of pods('east')){assert.ok(!oldEast.includes(pod.metadata.uid));const replica=await connect('east',pod),result=await replica.request('/api/v1/audit?namespace=demo');assert.equal(result.status,200);assert.deepEqual(result.body,saved.east);}
  let final;
  // A Service connection opened before rollout can fail once while kube-proxy
  // removes its old endpoint. Wait for a complete read, never retry a mutation.
  for(let attempt=0;attempt<30;attempt++){final=await overview();if(final.sites.every(s=>s.available)&&final.groups.length===before.groups.length)break;await wait(500);}
  assert.ok(final.sites.every(s=>s.available),'Agent reads did not recover after rollout');
  assert.deepEqual(signature(final),signature(before),'Audit acceptance changed application settings');
  report.status='PASS';report.podRestartPreservedHistory=true;report.applicationConfigurationUnchanged=true;report.totalRetainedEvents=merged.events.length;
}catch(e){report.status='FAIL';report.error=e.message;process.exitCode=1;}finally{
  for(const child of children){if(child.exitCode===null){child.kill('SIGTERM');await Promise.race([new Promise(r=>child.once('exit',r)),wait(2000)]);if(child.exitCode===null)child.kill('SIGKILL');}}
  report.finishedAt=new Date().toISOString();fs.writeFileSync(path.join(artifacts,'acceptance.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report));
}})();
