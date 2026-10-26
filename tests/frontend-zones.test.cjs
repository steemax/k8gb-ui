'use strict';
const {test}=require('node:test');const assert=require('node:assert/strict');
const M=require('../web/static/k8gb-mutations.js');
const row=(id,zones,provider='native')=>({clusterId:id,provider,catalog:{dnsZones:{available:true,zones}}});
test('zones are intersected per selected cluster, not taken from display registry',()=>{
 assert.deepEqual(M.commonZones([row('east',['apps.example.test','east.example.test']),row('west',['apps.example.test','west.example.test'])]),['apps.example.test']);
 assert.deepEqual(M.commonZones([row('east',['apps.example.test']),{clusterId:'west',catalog:{dnsZones:{available:false,zones:['apps.example.test']}}}]),[]);
 assert.deepEqual(M.commonZones([]),[]);
});
test('Traefik allowlist can narrow controller zones and DNS containment uses label boundaries',()=>{
 const candidate=row('east',['apps.example.test','private.example.test'],'traefik');candidate.catalog.traefikDomains=['apps.example.test'];
 assert.deepEqual(M.zonesFor(candidate),['apps.example.test']);
 assert.equal(M.inZone('apps.example.test','web.apps.example.test'),true);
 for(const host of ['apps.example.test','webapps.example.test','*.apps.example.test','bad name.apps.example.test']) assert.equal(M.inZone('apps.example.test',host),false);
});
test('signed request identity includes the selected DNS zone',()=>{
 const req={provider:'native',operation:'create',resource:{},reason:'Create',dnsZone:'apps.example.test'};
 assert.notEqual(M.requestSignature(req),M.requestSignature({...req,dnsZone:'other.example.test'}));
});
