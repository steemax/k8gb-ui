'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const M = require('../web/static/k8gb-mutations.js');
const C = require('../web/static/k8gb-core.js');
const caps = () => ({readOnly:false, providers:['native','traefik'], operations:{native:['create','update'], traefik:['enable','update','deactivate']}});
const ref = (kind = 'Gslb', name = 'web', uid = 'uid-east', rv = '12') => ({apiVersion:kind === 'Gslb' ? 'k8gb.io/v1beta1' : kind === 'IngressRoute' ? 'traefik.io/v1alpha1' : 'networking.k8s.io/v1', kind, namespace:'app', name, uid, resourceVersion:rv});
const policy = {strategy:'roundRobin', dnsTtlSeconds:30};
const instance = (clusterId = 'east') => ({clusterId, resource:ref('Gslb','web',`uid-${clusterId}`), policy, target:ref('Ingress',`route-${clusterId}`)});
const context = () => ({namespace:'app', demo:false, stale:false, overview:{namespace:'app', capabilities:caps(), sites:['east','west'].map(id => ({cluster:{id}, available:true, capabilities:caps()})), groups:[{id:'app/web.example.test', instances:[instance(),instance('west')]}]}});
const security = () => ({entrypoints:['secure-client','websecure'],ordinaryEntrypoints:['websecure','grpc'],pluginAlias:'certauthz',sanFilterConfigured:true,tlsOptions:[{resource:{...ref('IngressRoute','client-auth'),kind:'TLSOption'},clientAuthType:'RequireAndVerifyClientCert',caSecretNames:['client-ca'],eligible:true}],warnings:[]});
const candidate = (provider = 'native') => ({clusterId:'east', catalog:{capabilities:caps(),traefikSecurity:security()}, provider, eligible:true, enabled:provider === 'traefik', resource:ref(provider === 'native' ? 'Ingress' : 'IngressRoute','route'), policy, traefik:{entrypoint:'websecure',mtlsEntrypoint:'secure-client',tlsOption:{namespace:'app',name:'client-auth'}, mtls:true, commonNames:['client.example.test'], local:true, hostname:'web.example.test'}});
const nativeInput = () => ({context:context(),mode:'native-update',instances:[instance()],policy,reason:'Change routing'});
const planFor = (targets, expiresAt = Date.now() + 300000) => ({canApply:true, targets:targets.map(target => ({clusterId:target.clusterId, plan:{clusterId:target.clusterId, request:target.request, changes:[{path:'/spec/strategy/dnsTtlSeconds',before:30,after:60}], expiresAt:new Date(expiresAt).toISOString(), token:'signed-token', digest:'sha256:digest'}}))});

test('editing needs explicit supported operations in overview, site and catalog, never a readOnly toggle', () => {
  const data = context();
  assert.equal(M.can(data,'east','native','create'),true);
  data.overview.capabilities.operations = undefined;
  assert.equal(M.hasEdits(data),false);
  data.overview.capabilities = caps(); data.overview.sites[0].capabilities.operations.native = ['update'];
  assert.equal(M.can(data,'east','native','create'),false);
  assert.equal(M.can(data,'east','native','update'),true);
  assert.equal(M.can(data,'east','native','delete'),false);
  assert.equal(M.can(data,'east','native','update',{capabilities:{...caps(),readOnly:true}}),false);
});
test('demo, stale scope, unavailable sites and mismatched namespace are always mutation-free', () => {
  for (const overrides of [{demo:true},{stale:true},{namespace:'other'}]) assert.equal(M.hasEdits({...context(),...overrides}),false);
  const data = context(); data.overview.sites.forEach(site => { site.available = false; }); assert.equal(M.hasEdits(data),false);
  data.overview.sites.forEach(site => { site.available = true; site.capabilities = {readOnly:false}; }); assert.equal(M.hasEdits(data),false);
});
test('native update requires a nonempty reason and sends no actor or copied target refs', () => {
  assert.throws(() => M.buildTargets({...nativeInput(),reason:' \n '}),/reason/);
  for (const reason of ['x'.repeat(1025),'\u00e9'.repeat(513),'bad\u0000reason']) assert.throws(() => M.buildTargets({...nativeInput(),reason}),/Reason/);
  const targets = M.buildTargets({...nativeInput(),reason:'  Change routing  ',instances:[instance(),instance('west')]});
  assert.equal(targets.length,2);
  assert.equal(targets[0].request.reason,'Change routing');
  assert.deepEqual(targets.map(target => target.request.resource.uid),['uid-east','uid-west']);
  assert.ok(targets.every(target => !Object.hasOwn(target.request,'target') && !Object.hasOwn(target.request,'actor')));
  assert.equal(targets[0].request.resource.resourceVersion,'12');
});
test('native updates reject unobserved, derived, missing UID/RV and cross-namespace resources', () => {
  const base = nativeInput();
  for (const row of [{...instance(),source:ref('IngressRoute')},{...instance(),resource:ref('Gslb','not-observed')},{...instance(),resource:{...ref(),uid:''}},{...instance(),resource:{...ref(),resourceVersion:''}},{...instance(),resource:{...ref(),namespace:'other'}}]) assert.throws(() => M.buildTargets({...base,instances:[row]}));
  const data = context(); data.overview.groups[0].instances[0].source = ref('IngressRoute');
  assert.deepEqual(M.nativeInstances(data,data.overview.groups[0]).map(row => row.clusterId),['west']);
});
test('native creation uses a catalog native target with its full reference and an independent new Gslb resource', () => {
  const row = candidate();
  const [target] = M.buildTargets({context:context(),mode:'native-create',candidate:row,name:'new-gslb',policy,reason:'Create route'});
  assert.deepEqual(target.request.resource,{apiVersion:'k8gb.io/v1beta1',kind:'Gslb',namespace:'app',name:'new-gslb'});
  assert.deepEqual(target.request.target,row.resource);
  assert.equal(Object.hasOwn(target.request,'resourceRef'),false);
  for (const changed of [{...row,provider:'traefik'},{...row,enabled:true},{...row,eligible:false},{...row,resource:{...row.resource,namespace:'other'}},{...row,resource:ref('Service')}]) assert.throws(() => M.buildTargets({context:context(),mode:'native-create',candidate:changed,name:'new-gslb',policy,reason:'Create'}));
});
test('reported external management never becomes a native edit action', () => {
  const data=context(), row=data.overview.groups[0].instances[0];row.management='external';
  assert.deepEqual(M.nativeInstances(data,data.overview.groups[0]).map(value=>value.clusterId),['west']);
  assert.throws(()=>M.buildTargets({context:data,mode:'native-update',instances:[row],policy,reason:'Not a direct target'}),/native/);
});
test('catalog batches preserve independent cluster UID/RV and require explicit compatible native DNS targets', () => {
  const east = {...candidate(),hostnames:['web.example.test']};
  const west = {...candidate(),clusterId:'west',resource:ref('Ingress','route','uid-west','99'),hostnames:['WEB.EXAMPLE.TEST.']};
  const input = {context:context(),mode:'native-create',name:'shared-gslb',policy,reason:'Create on both sites'};
  const targets = M.buildCatalogBatch(input,[east,west]);
  assert.deepEqual(targets.map(row=>[row.clusterId,row.request.target.uid,row.request.target.resourceVersion]),[['east','uid-east','12'],['west','uid-west','99']]);
  assert.ok(targets.every(row=>row.request.resource.name==='shared-gslb'));
  assert.throws(()=>M.buildCatalogBatch(input,[]),/Select/);
  assert.throws(()=>M.buildCatalogBatch(input,[east,east]),/one resource/);
  assert.throws(()=>M.buildCatalogBatch(input,[east,{...west,hostnames:['other.example.test']}]),/same DNS/);
  const denied = context(); denied.overview.sites[1].capabilities.readOnly=true;
  assert.throws(()=>M.buildCatalogBatch({...input,context:denied},[east,west]),/authorized/);
  const matches=M.matchingCandidates([east,west,{...west,resource:ref('Ingress','unrelated')}],east);
  assert.deepEqual(matches.map(row=>row.index),[0,1]);
});
test('draft DNS model uses the live form and selected creation sites, not the saved policy', () => {
  const data = {sites:[{cluster:{id:'east',geoTags:['east']}},{cluster:{id:'west',geoTags:['west']}}],candidates:[{...candidate(),hostnames:['web.example.test']}],policy:{strategy:'failover',primaryGeoTag:'east',dnsTtlSeconds:'15',weights:{east:'3',west:'1'}}};
  const result=M.draftDNSInput(data);
  assert.equal(result.hostname,'web.example.test'); assert.equal(result.mode,'failover'); assert.equal(result.ttl,'15'); assert.equal(result.draft,true);
  assert.deepEqual(result.tags,['east']); assert.deepEqual(result.weights,{east:'3',west:'1'});
  assert.deepEqual(M.draftDNSInput({...data,group:{id:'g',hostname:'saved.example.test',instances:[{geoTag:'east'},{geoTag:'west'}]},hostname:'draft.example.test'}).tags,['east','west']);
});
test('native site exclusion pins Ingress plus owning Gslb and requires separate local-impact consent', async () => {
  const data=context(),group=data.overview.groups[0],row=group.instances[0];group.hostname='web.example.test';
  for(const caps of [data.overview.capabilities,...data.overview.sites.map(site=>site.capabilities)]) caps.operations.native.push('excludeSite','includeSite');
  const entry={...candidate(),resource:row.target,enabled:true,siteTrafficEligible:true,hostnames:[group.hostname],traffic:{excluded:false}};
  entry.catalog.capabilities=data.overview.capabilities;
  const input={context:data,mode:'native-excludeSite',group,instances:[row],candidate:entry,reason:'Maintenance with local route impact'};
  const targets=M.buildTargets(input);
  assert.deepEqual(targets[0].request.resource,row.target);assert.deepEqual(targets[0].request.target,row.resource);
  assert.equal(targets[0].request.provider,'native');assert.equal(targets[0].request.policy,undefined);
  const planner=M.createPlanner({post:async()=>planFor(targets)});await planner.plan(targets);
  assert.equal(planner.canApply(true,false,true),false);
  assert.equal(planner.canApply(true,false,true,true),true);
  assert.equal(planner.canApply(true,false,false,true),false);
  assert.throws(()=>M.buildTargets({...input,candidate:{...entry,siteTrafficEligible:false}}),/eligible/);
  assert.throws(()=>M.buildTargets({...input,candidate:{...entry,resource:{...entry.resource,name:'other'}}}),/eligible/);
  row.management='traefik';
  assert.throws(()=>M.buildTargets(input),/eligible/);
});
test('pre-preview refresh advances only observed Gslb versions, never identity, policy or management', () => {
  const data=context(), targets=M.buildTargets({...nativeInput(),context:data});
  const latest=structuredClone(data.overview);latest.groups[0].instances[0].resource.resourceVersion='new-status-rv';
  const updated=M.refreshGslbTargets(targets,data,latest);
  assert.equal(updated[0].request.resource.resourceVersion,'new-status-rv');assert.equal(targets[0].request.resource.resourceVersion,'12');
  assert.deepEqual(updated[0].request.policy,targets[0].request.policy);
  for(const modify of [overview=>{overview.namespace='other';},overview=>{overview.groups=[];},overview=>{overview.groups[0].instances[0].resource.uid='replacement';},overview=>{overview.groups[0].instances[0].management='external';},overview=>{overview.groups[0].instances[0].policy.dnsTtlSeconds=99;},overview=>{overview.groups[0].instances[0].traffic={overrideManaged:true};},overview=>{overview.sites[0].available=false;}]){
    const changed=structuredClone(latest);modify(changed);assert.throws(()=>M.refreshGslbTargets(targets,data,changed));
  }
});
test('Traefik update uses catalog route UID/RV and preserves all supported options', () => {
  const row = candidate('traefik');
  const [target] = M.buildTargets({context:context(),mode:'traefik-update',candidate:row,policy:row.policy,traefik:row.traefik,reason:'Update policy'});
  assert.equal(target.request.provider,'traefik'); assert.deepEqual(target.request.resource,row.resource);
  assert.deepEqual(target.request.traefik,row.traefik); assert.equal(target.request.target,undefined);
  assert.equal(M.sameResource({...row.resource,resourceVersion:'old'},row.resource),true);
  assert.equal(M.sameResource({...row.resource,uid:'replaced'},row.resource),false);
});
test('shared mTLS config requires an approved entrypoint, strict TLSOption and configured SAN plugin', () => {
  for (const change of [row => {row.traefik.mtlsEntrypoint='missing';},row=>{row.traefik.tlsOption.name='missing';},row=>{row.catalog.traefikSecurity.tlsOptions[0].eligible=false;},row=>{row.catalog.traefikSecurity.tlsOptions[0].clientAuthType='RequireAnyClientCert';},row=>{row.catalog.traefikSecurity.sanFilterConfigured=false;},row=>{row.catalog.traefikSecurity=undefined;}]) {
    const row=candidate('traefik');change(row);assert.throws(()=>M.buildTargets({context:context(),mode:'traefik-update',candidate:row,policy,traefik:row.traefik,reason:'Secure TLS'}));
  }
  const east=candidate('traefik'),west=structuredClone(east);west.clusterId='west';
  west.catalog.traefikSecurity.tlsOptions[0].resource.uid='west-tls-uid';
  assert.equal(M.commonSecurity([east,west]).tlsOptions[0].eligible,true);
  west.catalog.traefikSecurity.tlsOptions[0].caSecretNames=['different-ca'];
  assert.equal(M.commonSecurity([east,west]).tlsOptions[0].eligible,false);
  west.catalog.traefikSecurity.entrypoints=['secure-client'];
  assert.deepEqual(M.commonSecurity([east,west]).entrypoints,['secure-client']);
  const input={provider:'traefik',operation:'update',resource:east.resource,traefik:east.traefik};
  assert.notEqual(M.requestSignature(input),M.requestSignature({...input,traefik:{...east.traefik,mtlsEntrypoint:'websecure'}}));
  assert.notEqual(M.requestSignature(input),M.requestSignature({...input,traefik:{...east.traefik,tlsOption:{namespace:'app',name:'other'}}}));
});
test('Traefik enable and deactivate respect current catalog state; deactivate contains no policy', () => {
  const row = candidate('traefik');
  assert.throws(() => M.buildTargets({context:context(),mode:'traefik-enable',candidate:row,policy,reason:'Enable'}),/state/);
  row.enabled = false;
  assert.equal(M.buildTargets({context:context(),mode:'traefik-enable',candidate:row,policy,reason:'Enable'})[0].request.operation,'enable');
  assert.throws(() => M.buildTargets({context:context(),mode:'traefik-deactivate',candidate:row,reason:'Disable'}));
  row.enabled = true; row.eligible = false;
  assert.deepEqual(Object.keys(M.buildTargets({context:context(),mode:'traefik-deactivate',candidate:row,reason:'Disable'})[0].request).sort(),['operation','provider','reason','resource']);
});
test('policy validation uses typed fields and rejects invalid weights and TTL', () => {
  assert.throws(() => M.validatePolicy({strategy:'custom'}));
  assert.throws(() => M.validatePolicy({strategy:'failover'}));
  for (const dnsTtlSeconds of [-1,1.5,'invalid',2147483648,Number.MAX_SAFE_INTEGER+1]) assert.throws(() => M.validatePolicy({...policy,dnsTtlSeconds}));
  for (const weights of [{},{east:0},{east:-1},{east:'bad'},{east:1.1},{'':1}]) assert.throws(() => M.validatePolicy({strategy:'weightedRoundRobin',weights}));
  assert.deepEqual(M.validatePolicy({strategy:'weightedRoundRobin',weights:{east:'1',west:'0'}}),{strategy:'weightedRoundRobin',weights:{east:1,west:0}});
});
test('diff generation retains real structured before/after values, null and absence', () => {
  assert.deepEqual(M.diffRows([{path:'/spec/strategy',before:{type:'roundRobin'},after:{type:'failover'}},{path:'/new',before:null,after:['east','west']},{path:'/missing',after:false}]),[{path:'/spec/strategy',before:'{\n  "type": "roundRobin"\n}',after:'{\n  "type": "failover"\n}'},{path:'/new',before:'null',after:'[\n  "east",\n  "west"\n]'},{path:'/missing',before:'(not present)',after:'false'}]);
});
test('mutation transport sends required same-origin intent headers and never sets actor or Origin', async () => {
  let options;
  await M.postJSON('/api/v1/mutations/plan',{targets:[]},{fetcher:async (url,input) => { options = input; return {ok:true,json:async () => ({targets:[]})}; }});
  assert.equal(options.method,'POST'); assert.equal(options.credentials,'same-origin'); assert.equal(options.redirect,'error');
  assert.equal(options.headers['Content-Type'],'application/json'); assert.equal(options.headers['X-K8gb-Intent'],'mutation'); assert.equal(options.headers.Origin,undefined);
  await assert.rejects(M.postJSON('https://example.test/api/v1/mutations/apply',{}),/Unsupported/);
  await assert.rejects(M.postJSON('/api/v1/mutations/delete',{}),/Unsupported/);
});
test('GETs allow catalog/audit/session but never a mutation endpoint or external destination', async () => {
  for (const path of ['/api/v1/catalog?namespace=app','/api/v1/audit?namespace=app','/api/v1/session']) await C.getJSON(path,{fetcher:async (_,options) => { assert.equal(options.method,'GET'); return {ok:true,json:async () => ({})}; }});
  await assert.rejects(C.getJSON('/api/v1/mutations/plan'),/Unsupported/);
});
test('plan is signed, consent-gated, capped at five minutes, and consumed after one apply', async () => {
  let time = Date.now(), calls = [];
  const targets = M.buildTargets(nativeInput());
  const planner = M.createPlanner({now:() => time, post:async (path,payload) => { calls.push([path,payload]); return path.endsWith('/plan') ? planFor(targets,time+600000) : {targets:[{clusterId:'east',result:{status:'applied'}}]}; }});
  await planner.plan(targets); assert.equal(planner.state.expiresAt,time+300000);
  assert.equal(planner.canApply(false),false); assert.equal(planner.canApply(true),true);
  await planner.apply(true); assert.equal(planner.state.status,'finished'); assert.equal(calls.length,2);
  assert.deepEqual(calls[1][1],{targets:planFor(targets,time+600000).targets,allowPartial:false});
  await assert.rejects(planner.apply(true),/current plan/); assert.equal(calls.length,2);
});
test('expired plans cannot apply even with consent and never trigger a network call', async () => {
  let time = Date.now(), calls = 0; const targets = M.buildTargets(nativeInput());
  const planner = M.createPlanner({now:() => time,post:async () => { calls++; return planFor(targets,time+1000); }});
  await planner.plan(targets); time += 1000;
  assert.equal(planner.canApply(true),false); await assert.rejects(planner.apply(true)); assert.equal(calls,1);
});
test('any form edit invalidates a plan, including late plan responses', async () => {
  const targets = M.buildTargets(nativeInput()); let complete;
  const planner = M.createPlanner({post:() => new Promise(resolve => { complete = resolve; })});
  const pending = planner.plan(targets); planner.invalidate(); complete(planFor(targets)); await pending;
  assert.equal(planner.state.batch,null); assert.equal(planner.canApply(true),false);
  const fresh = M.createPlanner({post:async () => planFor(targets)}); await fresh.plan(targets); fresh.invalidate(); assert.equal(fresh.canApply(true),false);
});
test('mismatched cluster, UID, resource version or policy in plan is not accepted', async () => {
  const targets = M.buildTargets(nativeInput());
  for (const mutate of [batch => { batch.targets[0].clusterId = 'west'; },batch => { batch.targets[0].plan.request.resource.uid = 'other'; },batch => { batch.targets[0].plan.request.resource.resourceVersion = '999'; },batch => { batch.targets[0].plan.request.policy.dnsTtlSeconds = 999; }]) {
    const planner = M.createPlanner({post:async () => { const batch = structuredClone(planFor(targets)); mutate(batch); return batch; }});
    await planner.plan(targets); assert.equal(planner.canApply(true),false); assert.match(planner.state.message,/match/);
  }
});
test('cluster planning failures require separate partial consent and remain visible in apply payload', async () => {
  const targets = M.buildTargets({...nativeInput(),instances:[instance(),instance('west')]}); const batch = planFor(targets);
  batch.canApply = false; batch.targets[1] = {clusterId:'west',error:{code:'conflict',error:'Resource changed'}};
  let applied; const planner = M.createPlanner({post:async (path,payload) => { if (path.endsWith('/plan')) return batch; applied = payload; return {targets:[{clusterId:'east',result:{status:'applied'}},{clusterId:'west',error:{code:'not_planned',error:'Skipped'}}]}; }});
  await planner.plan(targets); assert.equal(planner.canApply(true),false); assert.equal(planner.canApply(true,true),true);
  await planner.apply(true,true); assert.deepEqual(applied,{targets:batch.targets,allowPartial:true});
});
test('Traefik deactivation requires its own consent in addition to reviewed-plan consent', async () => {
  const targets = M.buildTargets({context:context(),mode:'traefik-deactivate',candidate:candidate('traefik'),reason:'Disable'});
  const planner = M.createPlanner({post:async () => planFor(targets)}); await planner.plan(targets);
  assert.equal(planner.canApply(true,false,false),false); assert.equal(planner.canApply(true,false,true),true);
});
test('unknown apply outcomes are not retried and cannot be re-planned in the same form', async () => {
  const targets = M.buildTargets(nativeInput()); let calls = 0;
  const planner = M.createPlanner({post:async path => { calls++; if (path.endsWith('/plan')) return planFor(targets); throw new Error('Network lost'); }});
  await planner.plan(targets); await planner.apply(true);
  assert.equal(planner.state.status,'unknown'); assert.match(planner.state.message,/Outcome unknown/); assert.equal(calls,2);
  planner.invalidate(); assert.equal(planner.state.status,'unknown');
  await assert.rejects(planner.apply(true)); await assert.rejects(planner.plan(targets)); assert.equal(calls,2);
});
test('per-cluster unknown results, malformed responses and network failures retain unknown state', async () => {
  const targets = M.buildTargets(nativeInput());
  for (const result of [{targets:[{clusterId:'east',error:{outcomeUnknown:true,error:'Timeout'}}]},{targets:[]},{}]) {
    const planner = M.createPlanner({post:async path => path.endsWith('/plan') ? planFor(targets) : result}); await planner.plan(targets); await planner.apply(true); assert.equal(planner.state.status,'unknown');
  }
  let requests = 0;
  await assert.rejects(M.postJSON('/api/v1/mutations/apply',{}, {fetcher:async () => { requests++; throw new Error('Disconnected'); }}),error => error.outcomeUnknown === true);
  assert.equal(requests,1);
});
test('multiple resources in one cluster retain ordered plans and independent results', async () => {
  const data = context(), second = {...instance(),resource:ref('Gslb','second','uid-second')}; data.overview.groups[0].instances.push(second);
  const targets = M.buildTargets({...nativeInput(),context:data,instances:[instance(),second]});
  const planner = M.createPlanner({post:async path => path.endsWith('/plan') ? planFor(targets) : {targets:targets.map(target => ({clusterId:'east',result:{resource:target.request.resource,status:'applied'}}))}});
  await planner.plan(targets); assert.equal(planner.canApply(true),true); await planner.apply(true); assert.equal(planner.state.status,'finished');
});
test('a no-op or malformed signed plan is never applicable', async () => {
  const targets = M.buildTargets(nativeInput());
  for (const mutate of [batch => { batch.targets[0].plan.noop = true; },batch => { batch.targets[0].plan.token = ''; },batch => { batch.targets[0].plan.digest = ''; },batch => { batch.targets[0].plan.expiresAt = 'invalid'; }]) {
    const planner = M.createPlanner({post:async () => { const batch = structuredClone(planFor(targets)); mutate(batch); return batch; }}); await planner.plan(targets); assert.equal(planner.canApply(true),false);
  }
});
test('apply timeout is a single unknown outcome with no automatic retry', async () => {
  let calls = 0;
  await assert.rejects(M.postJSON('/api/v1/mutations/apply',{}, {timeout:5,fetcher:(_,options) => { calls++; return new Promise((resolve,reject) => options.signal.addEventListener('abort',() => reject(new Error('Aborted')),{once:true})); }}),error => error.outcomeUnknown === true);
  assert.equal(calls,1);
});
