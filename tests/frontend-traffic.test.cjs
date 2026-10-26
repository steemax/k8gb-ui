'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const M = require('../web/static/k8gb-mutations.js');
const C = require('../web/static/k8gb-core.js');
const caps = () => ({readOnly:false,providers:['native','traefik','traffic'],operations:{native:['update'],traefik:['excludeSite','includeSite'],traffic:['overrideIPs','resetIPs']}});
const ref = (kind = 'Gslb', rv = '12') => ({apiVersion:kind === 'Gslb' ? 'k8gb.io/v1beta1' : 'traefik.io/v1alpha1',kind,namespace:'app',name:kind === 'Gslb' ? 'web' : 'route',uid:`uid-${kind}`,resourceVersion:rv});
function fixture(derived = false) {
  const instance = {clusterId:'east',resource:ref(),...(derived ? {source:ref('IngressRoute')} : {}),health:'Healthy',traffic:{excluded:false,ips:['192.0.2.1'],hostnames:['origin.example.test'],overrideManaged:true,overrideConflict:false}};
  if (instance.source) delete instance.source.resourceVersion;
  const group = {id:'app/web',hostname:'web.example.test',instances:[instance]};
  const context = {namespace:'app',overview:{namespace:'app',capabilities:caps(),sites:[{cluster:{id:'east'},available:true,capabilities:caps()}],groups:[group]}};
  const candidate = {clusterId:'east',provider:'traefik',enabled:true,eligible:true,siteTrafficEligible:true,hostnames:['web.example.test'],resource:ref('IngressRoute','99'),catalog:{capabilities:caps()},traffic:{excluded:false}};
  const input = {context,group,instances:[instance],candidate,mode:'traffic-overrideIPs',traffic:{ips:['198.51.100.2','192.0.2.1']},reason:'Maintenance'};
  return {instance,context,candidate,input};
}
const batchFor = targets => ({canApply:true,targets:targets.map(target => ({clusterId:target.clusterId,plan:{clusterId:target.clusterId,request:structuredClone(target.request),token:'signed',digest:'digest',expiresAt:new Date(Date.now()+300000).toISOString(),changes:[{path:'/metadata/annotations',before:{},after:{ips:['192.0.2.1']}}]}}))});

test('traffic override pins native and generated Gslbs, never policies, route options or target refs', () => {
  for (const derived of [false,true]) {
    const {input,instance} = fixture(derived);
    const [target] = M.buildTargets({...input,policy:{strategy:'failover'},traefik:{local:true}});
    assert.deepEqual(target,{clusterId:'east',request:{provider:'traffic',operation:'overrideIPs',resource:instance.resource,reason:'Maintenance',traffic:{ips:['192.0.2.1','198.51.100.2']}}});
    assert.notEqual(target.request.resource,instance.resource);
  }
});
test('traffic authorization needs overview and site capabilities, current namespace and availability', () => {
  for (const mutate of [f => { f.context.demo = true; },f => { f.context.stale = true; },f => { f.context.namespace = 'other'; },f => { f.context.overview.capabilities.readOnly = true; },f => { f.context.overview.capabilities.operations.traffic = []; },f => { f.context.overview.sites[0].capabilities.operations.traffic = []; },f => { f.context.overview.sites[0].available = false; }]) {
    const f = fixture(); mutate(f); assert.throws(() => M.buildTargets(f.input),/authorized/);
    assert.equal(M.createView({getContext:() => f.context,icon:() => ''}).trafficActions(f.instance,0).includes('data-traffic-mode="traffic-overrideIPs"'),false);
  }
});
test('traffic targets reject unobserved, replaced, stale, cross-namespace, non-Gslb and multiple resources', () => {
  const {input,instance} = fixture();
  for (const resource of [{...ref(),uid:'replaced'},{...ref(),resourceVersion:'old'},{...ref(),namespace:'other'},{...ref(),uid:''},{...ref(),resourceVersion:''},ref('IngressRoute')]) assert.throws(() => M.buildTargets({...input,instances:[{...instance,resource}]}));
  for (const instances of [[],[instance,instance]]) assert.throws(() => M.buildTargets({...input,instances}),/exactly one/);
});
test('IP validation accepts explicit private/documentation unicast, rejects non-unicast, IPv6, duplicates and more than 16', () => {
  assert.deepEqual(M.validateIPs('192.0.2.1, 10.0.0.1\n198.51.100.2'),['10.0.0.1','192.0.2.1','198.51.100.2']);
  for (const ips of [undefined,[],['::1'],['::ffff:192.0.2.1'],['example.test'],['192.0.2.1:80'],['192.0.2.1/32'],['01.2.3.4'],['256.2.3.4'],['0.0.0.0'],['127.0.0.1'],['169.254.1.1'],['224.0.0.1'],['239.1.1.1'],['255.255.255.255'],['192.0.2.1','192.0.2.1'],Array.from({length:17},(_,i) => `192.0.2.${i+1}`)]) assert.throws(() => M.validateIPs(ips));
  assert.equal(M.validateIPs(Array.from({length:16},(_,i) => `10.0.0.${i+1}`)).length,16);
});
test('all traffic and exclusion operations require a reason', () => {
  const {input,candidate} = fixture(true);
  for (const mode of ['traffic-overrideIPs','traffic-resetIPs','traefik-excludeSite','traefik-includeSite']) {
    candidate.traffic.excluded = mode.endsWith('includeSite');
    for (const reason of ['', ' \n ', 'bad\u0000reason','x'.repeat(1025)]) assert.throws(() => M.buildTargets({...input,mode,reason}),/reason|Reason/);
  }
});
test('reset restores only a managed conflict-free snapshot and sends no traffic options', () => {
  const {input,instance,context} = fixture();
  assert.deepEqual(M.buildTargets({...input,mode:'traffic-resetIPs'})[0].request,{provider:'traffic',operation:'resetIPs',resource:ref(),reason:'Maintenance'});
  const view = M.createView({getContext:() => context,icon:() => ''});
  assert.doesNotMatch(view.trafficActions(instance,0),/traffic-resetIPs" disabled/);
  for (const traffic of [{overrideManaged:false},{overrideManaged:true,overrideConflict:true},undefined]) {
    instance.traffic = traffic;
    assert.throws(() => M.buildTargets({...input,mode:'traffic-resetIPs'}),/managed IP snapshot/);
    assert.match(view.trafficActions(instance,0),/traffic-resetIPs" disabled/);
  }
});
test('exclusion pins fresh source IngressRoute, requires enabled eligible catalog and never changes native backends', () => {
  const {input,instance,candidate,context} = fixture(true);
  assert.deepEqual(M.buildTargets({...input,mode:'traefik-excludeSite'})[0].request,{provider:'traefik',operation:'excludeSite',resource:ref('IngressRoute','99'),reason:'Maintenance'});
  for (const changed of [{...candidate,enabled:false},{...candidate,siteTrafficEligible:false},{...candidate,siteTrafficEligible:undefined},{...candidate,hostnames:['local.example.test']},{...candidate,provider:'native'},{...candidate,clusterId:'west'},{...candidate,catalog:undefined},{...candidate,catalog:{capabilities:{...caps(),readOnly:true}}},{...candidate,resource:{...candidate.resource,uid:'replaced'}},{...candidate,resource:{...candidate.resource,resourceVersion:''}}]) assert.throws(() => M.buildTargets({...input,mode:'traefik-excludeSite',candidate:changed}));
  assert.throws(() => M.buildTargets({...input,group:undefined,mode:'traefik-excludeSite'}),/global/);
  const local = {...input.group,id:'app/local',hostname:'local.example.test'}; context.overview.groups.push(local);
  assert.throws(() => M.buildTargets({...input,group:local,mode:'traefik-excludeSite'}),/global/);
  delete instance.source;
  assert.throws(() => M.buildTargets({...input,mode:'traefik-excludeSite'}),/source-owned/);
  assert.doesNotMatch(M.createView({getContext:() => context,icon:() => ''}).trafficActions(instance,0),/excludeSite|includeSite/);
  assert.equal(M.can(context,'east','native','excludeSite'),false);
});
test('include/exclude follows fresh catalog requested state when the observed child lags', () => {
  const {input,instance,candidate} = fixture(true);
  candidate.traffic.excluded = true;
  assert.equal(M.buildTargets({...input,mode:'traefik-includeSite'})[0].request.operation,'includeSite');
  assert.throws(() => M.buildTargets({...input,mode:'traefik-excludeSite'}),/catalog site state/);
  candidate.traffic.excluded = false; instance.traffic.excluded = true;
  assert.equal(M.buildTargets({...input,mode:'traefik-excludeSite'})[0].request.operation,'excludeSite');
  delete candidate.traffic;
  assert.equal(M.buildTargets({...input,mode:'traefik-includeSite'})[0].request.operation,'includeSite');
});
test('owner source has no resourceVersion; exclusion pins only the fresh catalog version', () => {
  const {input,instance,candidate} = fixture(true);
  assert.equal(instance.source.resourceVersion,undefined);
  assert.equal(M.buildTargets({...input,mode:'traefik-excludeSite'})[0].request.resource.resourceVersion,candidate.resource.resourceVersion);
  delete instance.source.uid;
  assert.throws(()=>M.buildTargets({...input,mode:'traefik-excludeSite'}),/source-owned/);
});
test('signed traffic plans bind the IPs, need consent, and reject injected options or a changed payload', async () => {
  const {input} = fixture(), targets = M.buildTargets(input);
  for (const mutate of [batch => { batch.targets[0].plan.request.traffic.ips = ['192.0.2.99']; },batch => { batch.targets[0].plan.request.policy = {strategy:'roundRobin'}; },batch => { batch.targets[0].plan.request.target = ref('IngressRoute'); },batch => { batch.targets[0].plan.request.traefik = {}; }]) {
    const planner = M.createPlanner({post:async () => { const batch = batchFor(targets); mutate(batch); return batch; }});
    await planner.plan(targets); assert.equal(planner.canApply(true),false); assert.match(planner.state.message,/match/);
  }
  const planner = M.createPlanner({post:async () => batchFor(targets)}); await planner.plan(targets);
  assert.equal(planner.canApply(false),false); assert.equal(planner.canApply(true),true);
});
test('exclusion needs destructive confirmation; inclusion only needs reviewed-plan consent', async () => {
  const {input,candidate} = fixture(true);
  for (const operation of ['excludeSite','includeSite']) {
    candidate.traffic.excluded = operation === 'includeSite';
    const targets = M.buildTargets({...input,mode:`traefik-${operation}`}), planner = M.createPlanner({post:async () => batchFor(targets)});
    await planner.plan(targets);
    assert.equal(planner.canApply(false,false,true),false);
    assert.equal(planner.canApply(true),operation === 'includeSite');
    assert.equal(planner.canApply(true,false,true),true);
  }
});
test('traffic plans retain expiry, invalidation and single-apply unknown-outcome protection', async () => {
  const {input} = fixture(), targets = M.buildTargets(input); let time = Date.now(), applied = 0;
  const planner = M.createPlanner({now:() => time,post:async path => { if (path.endsWith('/plan')) return batchFor(targets); applied++; throw new Error('Connection lost'); }});
  await planner.plan(targets); time += 300001; assert.equal(planner.canApply(true),false);
  time = Date.now(); planner.invalidate(); assert.equal(planner.canApply(true),false);
  await planner.plan(targets); await planner.apply(true); assert.equal(planner.state.status,'unknown');
  await assert.rejects(planner.apply(true)); await assert.rejects(planner.plan(targets)); assert.equal(applied,1);
});
test('Excluded is informative, DNS absence is intentional, unrelated backend failures remain warnings', () => {
  const excluded = {health:'Excluded',traffic:{excluded:true},dnsObserved:false,dnsTargets:[]};
  assert.deepEqual(C.health('Excluded'),{tone:'info',label:'Excluded'});
  assert.deepEqual(C.instanceHealth(excluded),{tone:'info',label:'Excluded'});
  assert.deepEqual(C.aggregateHealth([excluded]),{tone:'info',label:'Excluded'});
  assert.deepEqual(C.groupStatus({instances:[excluded],signals:[{code:'site_excluded',severity:'info'}]}),{tone:'info',label:'Excluded'});
  assert.equal(C.groupStatus({instances:[excluded],signals:[{code:'collection_stale',severity:'unknown'}]}).tone,'unknown');
  const included = {health:'Healthy',dnsObserved:true,dnsTargets:['192.0.2.1']};
  assert.equal(C.dnsDifference({instances:[excluded,included]}),false);
  assert.deepEqual(C.groupStatus({instances:[excluded,included]}),{tone:'info',label:'Partially excluded'});
  assert.equal(C.groupStatus({instances:[excluded,included],signals:[{code:'backend_health',severity:'warning'}]}).tone,'warning');
  assert.equal(C.health('NotFound').tone,'warning');
  assert.equal(C.signalLabel('site_excluded'),'Site excluded'); assert.equal(C.signalLabel('ip_override_conflict'),'IP override conflict');
});
test('preflight hides older/ineligible catalogs and local siblings before offering site controls', async () => {
  const original = C.getJSON;
  try {
    for (const change of [{},{siteTrafficEligible:false},{siteTrafficEligible:undefined},{hostnames:['local.example.test']},{resource:{...ref('IngressRoute'),uid:'replaced'}},{resource:{...ref('IngressRoute'),resourceVersion:''}}]) {
      const {context,instance,input,candidate} = fixture(true), row = {...candidate,...change};
      C.getJSON = async () => ({namespace:'app',sites:[{clusterId:'east',catalog:{capabilities:caps(),candidates:[row]}}]});
      const view = M.createView({getContext:() => context,icon:() => ''});
      assert.doesNotMatch(view.trafficActions(instance,0,input.group),/traefik-excludeSite/);
      await view.prepareTraffic(input.group);
      assert.equal(view.trafficActions(instance,0,input.group).includes('traefik-excludeSite'),Object.keys(change).length === 0);
      view.destroy();
    }
  } finally { C.getJSON = original; }
});
