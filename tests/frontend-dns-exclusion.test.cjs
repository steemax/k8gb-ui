'use strict';
const {test}=require('node:test');
const assert=require('node:assert/strict');
const C=require('../web/static/k8gb-core.js');
const M=require('../web/static/k8gb-mutations.js');
const DNS=require('../web/static/k8gb-dns.js');
const modes=['roundRobin','weightedRoundRobin','failover','geoip'];
const input=mode=>({tags:['east','west','central'],excludedTags:['east'],mode,primary:'east',ttl:30,weights:{east:100,west:3,central:1}});
test('DNS input preserves excluded nodes without treating backend health failures as manual exclusions',()=>{
  const instances=[{clusterId:'a',geoTag:'east',traffic:{excluded:true}},{clusterId:'b',geoTag:'west',health:'Excluded'},{clusterId:'c',geoTag:'central',health:'Unhealthy'}];
  const data=C.dnsInput({id:'g',instances},{hostname:'web.example.test',clusterId:'a',policy:{strategy:'roundRobin'}});
  assert.deepEqual(data.tags,['east','west','central']);assert.deepEqual(data.excludedTags,['east','west']);
  assert.deepEqual(C.dnsExcludedTags([{geoTag:'east',traffic:{excluded:true}},{geoTag:'east',traffic:{excluded:false}}]),[]);
  assert.deepEqual(C.dnsExcludedTags([{geoTag:'east',traffic:{excluded:true}},{geoTag:'east',health:'Excluded'}]),['east']);
});
test('excluded GeoTags never appear in any modeled answer or path, in any strategy',()=>{
  for(const mode of modes){
    const config=DNS.normalize(input(mode));assert.equal(config.error,'');assert.deepEqual(config.tags,['east','west','central']);
    for(let step=0;step<36;step++)for(const first of [undefined,'east']){
      const frame=DNS.frame(config,step,first);assert.ok(!frame.selected.includes('east'));assert.ok(!frame.answer.includes('east'));assert.ok(frame.selected.every(tag=>config.tags.includes(tag)));assert.ok(!frame.selected.includes(''));
    }
  }
});
test('weighted ordering renormalizes surviving weights and excludes even a high-weight site',()=>{
  const config=DNS.normalize(input('weightedRoundRobin')),counts={east:0,west:0,central:0};let scores=[];
  for(let i=0;i<40;i++){const next=DNS.nextWeighted(config,scores);scores=next.scores;counts[next.first]++;}
  assert.deepEqual(counts,{east:0,west:30,central:10});
});
test('excluded primary stays excluded until inclusion, while excluded standbys are never selected',()=>{
  const primary=DNS.normalize(input('failover'));
  for(let step=0;step<18;step++){const frame=DNS.frame(primary,step);assert.equal(frame.phase,'primary-excluded');assert.deepEqual(frame.answer,['west']);assert.equal(frame.cache,undefined);}
  const standby=DNS.normalize({...input('failover'),excludedTags:['west']});assert.deepEqual(DNS.frame(standby,3).answer,['central']);
  const onlyPrimary=DNS.normalize({...input('failover'),excludedTags:['west','central']});assert.equal(DNS.frame(onlyPrimary,3).phase,'single');assert.deepEqual(DNS.frame(onlyPrimary,3).answer,['east']);
});
test('all manually excluded sites yield an explicit static empty model, not invalid policy data',()=>{
  for(const mode of modes){const config=DNS.normalize({...input(mode),excludedTags:['east','west','central']});assert.equal(config.error,'');const frame=DNS.frame(config,0);assert.equal(frame.phase,'empty');assert.match(frame.copy,/All sites are manually excluded/);assert.deepEqual(frame.answer,[]);assert.deepEqual(frame.selected,[]);assert.equal(frame.moving,false);}
  const config=DNS.normalize({tags:['east','west'],excludedTags:['east'],mode:'weightedRoundRobin',weights:{east:1,west:0}});assert.equal(config.error,'');assert.equal(DNS.nextWeighted(config).first,'');assert.equal(DNS.frame(config,0).phase,'empty');
});
test('including a site restores it to DNS rotation, and unknown exclusion tags do not create nodes',()=>{
  const config=DNS.normalize({...input('roundRobin'),excludedTags:['absent','east','east']});assert.deepEqual(config.excludedTags,['east']);
  const restored=DNS.normalize({...input('roundRobin'),excludedTags:[]});assert.deepEqual(DNS.frame(restored,0).answer,['east','west','central']);
});
test('draft policy changes preserve exclusions across modes, weights, primary, TTL and target scope',()=>{
  const group={id:'g',hostname:'web.example.test',instances:[{clusterId:'a',geoTag:'east',traffic:{excluded:true}},{clusterId:'b',geoTag:'west',traffic:{excluded:false}}]};
  const sites=[{cluster:{id:'a',geoTags:['east']}},{cluster:{id:'b',geoTags:['west']}}];
  for(const mode of modes){const draft=M.draftDNSInput({group,sites,candidates:[{clusterId:'b'}],policy:{strategy:mode,primaryGeoTag:'east',dnsTtlSeconds:42,weights:{east:5,west:1}}});assert.deepEqual(draft.tags,['east','west']);assert.deepEqual(draft.excludedTags,['east']);assert.ok(!DNS.frame(DNS.normalize(draft),0).answer.includes('east'));}
});
test('matching fresh catalog exclusion state overrides a lagging child but unrelated routes do not',()=>{
  const ref={apiVersion:'traefik.io/v1alpha1',kind:'IngressRoute',namespace:'app',name:'route',uid:'route-uid'};
  const group={id:'g',instances:[{clusterId:'a',geoTag:'east',source:ref,traffic:{excluded:true},health:'Excluded'}]};
  const policy={strategy:'roundRobin'},sites=[{cluster:{id:'a',geoTags:['east']}}];
  for(const excluded of [true,false]){
    const candidate={clusterId:'a',resource:{...ref,resourceVersion:'20'},traffic:{excluded}};
    assert.deepEqual(M.draftDNSInput({group,sites,candidates:[candidate],policy}).excludedTags,excluded?['east']:[]);
  }
  const unrelated={clusterId:'a',resource:{...ref,uid:'other-uid'},traffic:{excluded:false}};
  assert.deepEqual(M.draftDNSInput({group,sites,candidates:[unrelated],policy}).excludedTags,['east']);
});
test('creation previews preserve existing routing exclusions on selected clusters only',()=>{
  const sites=[{cluster:{id:'a',geoTags:['east']}},{cluster:{id:'b',geoTags:['west']}}],candidates=[{clusterId:'a',traffic:{excluded:true}}];
  const draft=M.draftDNSInput({sites,candidates,policy:{strategy:'roundRobin'}});assert.deepEqual(draft.tags,['east']);assert.deepEqual(draft.excludedTags,['east']);assert.equal(DNS.frame(DNS.normalize(draft),0).phase,'empty');
});
