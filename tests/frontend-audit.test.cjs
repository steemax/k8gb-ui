'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {queryAudit} = require('../web/static/k8gb-mutations.js');
const now = Date.parse('2026-10-22T12:00:00Z');
const event = (id, overrides = {}) => ({id,time:'2026-10-22T11:30:00Z',clusterId:'east',actor:'alice',provider:'native',operation:'update',status:'applied',reason:'Adjust failover primary',resource:{kind:'Gslb',namespace:'demo',name:'web'},changes:[{path:'/spec/strategy/primaryGeoTag',before:'east',after:'west'}],...overrides});
const ids = result => result.events.map(row => row.id);

test('audit filters never escape the current namespace or alter the input history', () => {
  const rows = [event('a'),event('private',{resource:{kind:'Gslb',namespace:'private',name:'secret'}}),null,{}];
  const before = structuredClone(rows);
  const result = queryAudit(rows,'demo',{},now);
  assert.deepEqual(ids(result),['a']); assert.equal(result.total,1); assert.deepEqual(rows,before);
  assert.equal(queryAudit(rows,'other').total,0);
  assert.equal(queryAudit(null,'demo').total,0);
});
test('audit search combines words across object, actor, reason, changed fields and operation labels', () => {
  const rows = [event('a'),event('b',{actor:'bob',operation:'excludeSite',provider:'traefik',reason:'Maintenance',changes:[]})];
  for (const search of ['ALICE web','primaryGeoTag WEST','Adjust failover','Direct Gslb','Update settings','applied']) assert.deepEqual(ids(queryAudit(rows,'demo',{search},now)),search === 'applied' ? ['a','b'] : ['a']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{search:'maintenance Exclude site'},now)),['b']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{search:'not-found'},now)),[]);
  const xss = event('xss',{actor:'<admin>',reason:'<script>alert(1)</script>'});
  assert.deepEqual(ids(queryAudit([xss],'demo',{search:'<script>'},now)),['xss']);
});
test('audit exact facets compose and outcome unknown is independent from raw result status', () => {
  const rows = [event('a'),event('b',{clusterId:'west'}),event('c',{actor:'bob'}),event('d',{provider:'traefik'}),event('e',{operation:'create'}),event('f',{status:'apply_failed',outcomeUnknown:true}),event('g',{status:'apply_failed'})];
  assert.deepEqual(ids(queryAudit(rows,'demo',{clusterId:'east',actor:'alice',provider:'native',operation:'update',status:'applied'},now)),['a']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{status:'@outcomeUnknown'},now)),['f']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{status:'apply_failed'},now)),['f','g']);
  assert.equal(queryAudit(rows,'demo',{clusterId:'unavailable'},now).matched,0);
});
test('relative audit periods are bounded by now, include their lower boundary and reject invalid times', () => {
  const rows = [event('recent'),event('boundary',{time:'2026-10-22T11:00:00Z'}),event('older',{time:'2026-10-21T12:00:00Z'}),event('week',{time:'2026-10-15T12:00:00Z'}),event('future',{time:'2026-10-23T00:00:00Z'}),event('invalid',{time:'not-a-date'})];
  assert.deepEqual(ids(queryAudit(rows,'demo',{period:'1h'},now)),['recent','boundary']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{period:'24h'},now)),['recent','boundary','older']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{period:'7d'},now)),['recent','boundary','older','week']);
  assert.equal(queryAudit(rows,'demo',{},now).total,6);
});
test('custom audit range supports one-sided bounds and exposes invalid ranges instead of silently ignoring them', () => {
  const rows = [event('a'),event('b',{time:'2026-10-20T12:00:00Z'})];
  assert.deepEqual(ids(queryAudit(rows,'demo',{period:'custom',from:'2026-10-22T00:00:00Z'},now)),['a']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{period:'custom',to:'2026-10-20T12:00:00Z'},now)),['b']);
  for (const range of [{},{from:'bad'},{to:'bad'},{from:'2026-10-23T00:00:00Z',to:'2026-10-21T00:00:00Z'}]) {
    const result = queryAudit(rows,'demo',{period:'custom',...range},now);
    assert.match(result.error,/valid time range/); assert.equal(result.matched,0); assert.equal(result.total,2);
  }
});
test('audit sorting is natural, reversible, deterministic on equal timestamps and missing values stay last', () => {
  const rows = [event('ten',{resource:{namespace:'demo',kind:'Gslb',name:'web10'},actor:'zoe'}),event('two',{resource:{namespace:'demo',kind:'Gslb',name:'web2'},actor:'bob',time:'2026-10-22T10:00:00Z'}),event('missing',{actor:'',time:''})];
  assert.deepEqual(ids(queryAudit(rows,'demo',{sort:'resource',direction:'asc'},now)),['missing','two','ten']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{sort:'actor',direction:'asc'},now)),['two','ten','missing']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{sort:'actor',direction:'desc'},now)),['ten','two','missing']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{sort:'time',direction:'desc'},now)),['ten','two','missing']);
  assert.deepEqual(ids(queryAudit(rows,'demo',{sort:'time',direction:'asc'},now)),['two','ten','missing']);
  const tied = [event('same',{clusterId:'west'}),event('same',{clusterId:'east'})];
  assert.deepEqual(queryAudit(tied,'demo',{},now).events.map(row=>row.clusterId),['east','west']);
  assert.deepEqual(queryAudit(tied.reverse(),'demo',{},now).events.map(row=>row.clusterId),['east','west']);
  assert.equal(queryAudit(tied,'demo',{},now).matched,2,'Client filtering must not deduplicate different cluster events');
  const clusterNames = {east:'Zulu',west:'Alpha'};
  assert.deepEqual(queryAudit(tied,'demo',{clusterNames,sort:'clusterId',direction:'asc'},now).events.map(row=>row.clusterId),['west','east']);
  assert.deepEqual(queryAudit(tied,'demo',{clusterNames,search:'Alpha'},now).events.map(row=>row.clusterId),['west']);
});
test('audit pagination follows filtering and sorting, clamps stale pages and validates page size', () => {
  const rows = Array.from({length:130},(_,i)=>event(String(i),{actor:i<10 ? 'bob' : 'alice',resource:{namespace:'demo',kind:'Gslb',name:`web${i}`}}));
  let result = queryAudit(rows,'demo',{sort:'resource',direction:'asc',pageSize:50,page:2},now);
  assert.equal(result.matched,130); assert.equal(result.pages,3); assert.equal(result.events.length,50); assert.equal(result.events[0].id,'50');
  result = queryAudit(rows,'demo',{actor:'bob',page:99,pageSize:25},now);
  assert.equal(result.page,1); assert.equal(result.events.length,10); assert.equal(result.total,130);
  assert.equal(queryAudit(rows,'demo',{pageSize:999},now).pageSize,50);
  assert.equal(queryAudit(rows,'demo',{page:-10},now).page,1);
});
