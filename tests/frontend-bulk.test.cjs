'use strict';
const {test}=require('node:test');
const assert=require('node:assert/strict');
const M=require('../web/static/k8gb-mutations.js');
const caps=()=>({readOnly:false,providers:['native','traefik'],operations:{native:['excludeSite','includeSite'],traefik:['excludeSite','includeSite']}});
const ref=(kind,name,cluster)=>({apiVersion:kind==='Gslb'?'k8gb.io/v1beta1':kind==='Ingress'?'networking.k8s.io/v1':'traefik.io/v1alpha1',kind,namespace:'app',name,uid:cluster+'-'+kind+'-'+name,resourceVersion:'20'});
function fixture(count=2){
  const groups=[],candidates=[];
  for(let i=0;i<count;i++){
    const provider=i%2?'traefik':'native',name='web-'+i,hostname=name+'.example.test';
    const instances=['east','west'].map(clusterId=>{
      const resource=ref('Gslb',name,clusterId),routing=ref(provider==='native'?'Ingress':'IngressRoute',name,clusterId);
      const instance={clusterId,resource,hostname,traffic:{excluded:false},...(provider==='native'?{target:routing}:{source:{...routing}})};
      if(instance.source)delete instance.source.resourceVersion;
      candidates.push({provider,clusterId,resource:{...routing,resourceVersion:'99'},enabled:true,eligible:true,siteTrafficEligible:true,hostnames:[hostname],traffic:{excluded:false},catalog:{capabilities:caps()}});
      return instance;
    });
    groups.push({id:name,hostname,namespace:'app',instances});
  }
  const context={namespace:'app',overview:{namespace:'app',groups,capabilities:caps(),sites:['east','west'].map(id=>({cluster:{id},available:true,capabilities:caps()}))}};
  return {context,groups,candidates,clusterIds:['east','west'],operation:'excludeSite',reason:'Planned maintenance'};
}
test('bulk exclude and include are symmetric for mixed native/webhook resources and pin fresh route RVs',()=>{
  const f=fixture();
  let batch=M.buildSiteBatch(f);assert.equal(batch.targets.length,4);assert.equal(batch.skipped.length,0);
  assert.deepEqual(batch.targets.map(t=>t.request.provider),['native','native','traefik','traefik']);
  assert.ok(batch.targets.every(t=>t.request.resource.resourceVersion==='99'&&t.request.operation==='excludeSite'&&!t.request.policy&&!t.request.traefik));
  assert.ok(batch.targets.filter(t=>t.request.provider==='native').every(t=>t.request.target.kind==='Gslb'));
  assert.ok(batch.targets.filter(t=>t.request.provider==='traefik').every(t=>!t.request.target));
  f.candidates.forEach(c=>c.traffic.excluded=true);f.operation='includeSite';batch=M.buildSiteBatch(f);
  assert.equal(batch.targets.length,4);assert.ok(batch.targets.every(t=>t.request.operation==='includeSite'));
});
test('bulk uses catalog requested state, skips already-in-state resources, and reports every skip',()=>{
  const f=fixture();f.candidates[0].traffic.excluded=true;f.groups[0].instances[1].traffic.excluded=true;
  let batch=M.buildSiteBatch(f);assert.equal(batch.targets.length,3);assert.equal(batch.skipped[0].reason,'Already excluded');
  batch=M.buildSiteBatch({...f,operation:'includeSite'});assert.equal(batch.targets.length,1);assert.equal(batch.skipped.length,3);assert.ok(batch.skipped.every(r=>r.reason==='Already included'));
  delete f.candidates[0].traffic;batch=M.buildSiteBatch(f);assert.match(batch.skipped[0].reason,/state is unavailable/);
});
test('bulk skips unauthorized, unavailable, cross-namespace, missing and ambiguous resources',()=>{
  for(const change of [f=>{f.context.overview.sites[0].available=false;},f=>{f.context.overview.sites[0].capabilities.readOnly=true;},f=>{f.candidates[0].resource.namespace='other';},f=>{f.candidates[0].catalog.capabilities.readOnly=true;},f=>{f.candidates[0].siteTrafficEligible=false;},f=>{f.candidates.push(structuredClone(f.candidates[0]));}]){
    const f=fixture();change(f);const batch=M.buildSiteBatch(f);assert.ok(batch.targets.length<4);assert.ok(batch.skipped.length>0);assert.ok(batch.targets.every(t=>t.request.resource.namespace==='app'));
  }
  for(const change of [f=>{f.context.stale=true;},f=>{f.context.demo=true;},f=>{f.context.overview.capabilities.readOnly=true;},f=>{f.context.namespace='other';}]){const f=fixture();change(f);assert.equal(M.buildSiteBatch(f).targets.length,0);assert.equal(M.selectableGroup(f.context,f.groups[0]),false);}
});
test('bulk does not silently target a replacement resource or changed owner',()=>{
  for(const change of [f=>{f.context.overview.groups[0].instances[0].resource.uid='replacement';},f=>{f.context.overview.groups[1].instances[0].source.uid='replacement';},f=>{f.context.overview.groups[0].instances[0].target.uid='replacement';}]){
    const f=fixture();f.groups=structuredClone(f.groups);change(f);const batch=M.buildSiteBatch(f);assert.ok(batch.targets.length<4);assert.ok(batch.skipped.some(r=>/identity/.test(r.reason)));
  }
});
test('bulk deduplicates shared multi-host Traefik routes and affects them only once',()=>{
  const f=fixture(),duplicate=structuredClone(f.groups[1]);duplicate.id='another-host';duplicate.hostname='other.example.test';duplicate.instances.forEach(i=>i.hostname=duplicate.hostname);f.groups.push(duplicate);
  f.candidates.filter(c=>c.provider==='traefik').forEach(c=>c.hostnames.push(duplicate.hostname));
  const batch=M.buildSiteBatch(f);assert.equal(batch.targets.length,4);assert.equal(batch.skipped.filter(r=>/Shared routing/.test(r.reason)).length,2);
});
test('bulk bounds requests to 32 routing targets and never truncates or splits automatically',()=>{
  assert.equal(M.buildSiteBatch(fixture(16)).targets.length,32);
  assert.throws(()=>M.buildSiteBatch(fixture(17)),/at most 32/);
  assert.throws(()=>M.buildSiteBatch({...fixture(),operation:'deactivate'}),/exclusion or inclusion/);
  for(const reason of ['',' ','x'.repeat(1025),'invalid\u0000'])assert.throws(()=>M.buildSiteBatch({...fixture(),reason}),/reason|Reason/);
  assert.throws(()=>M.buildSiteBatch({...fixture(),clusterIds:[]}),/Select/);
});
test('bulk native exclusion retains mandatory local routing and exclusion confirmations',async()=>{
  const targets=M.buildSiteBatch(fixture()).targets;
  const planner=M.createPlanner({post:async()=>({canApply:true,targets:targets.map(t=>({clusterId:t.clusterId,plan:{clusterId:t.clusterId,request:t.request,token:'signed',digest:'digest',expiresAt:new Date(Date.now()+300000).toISOString(),changes:[{}]}}))})});
  await planner.plan(targets);assert.equal(planner.canApply(true,false,true,false),false);assert.equal(planner.canApply(true,false,false,true),false);assert.equal(planner.canApply(true,false,true,true),true);
});
