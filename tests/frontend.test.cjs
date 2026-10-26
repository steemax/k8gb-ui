'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const C = require('../web/static/k8gb-core.js');
const DNS = require('../web/static/k8gb-dns.js');
const root = path.resolve(__dirname, '..');
const instance = (overrides = {}) => ({clusterId:'east', resource:{apiVersion:'k8gb.absa.oss/v1beta1', kind:'Gslb', namespace:'default', name:'catalog-gslb'}, hostname:'catalog.example.org', geoTag:'east', health:'Healthy', dnsObserved:true, dnsTargets:['192.0.2.1'], backends:[], warnings:[], policy:{strategy:'roundRobin', dnsTtlSeconds:30}, ...overrides});
const group = (overrides = {}) => ({id:'default/catalog.example.org', namespace:'default', hostname:'catalog.example.org', zone:'example.org', state:'consistent', signals:[], instances:[instance(), instance({clusterId:'west', geoTag:'west', dnsTargets:['198.51.100.1']})], ...overrides});

test('escaping API data protects text and quoted attributes', () => {
  assert.equal(C.escape('<img src=x onerror="x">&\''), '&lt;img src=x onerror=&quot;x&quot;&gt;&amp;&#39;');
  assert.equal(C.escape(null), '');
});
test('management separates native, webhook, external and mixed site ownership', () => {
  const direct = instance({management:'native'}), route = instance({management:'traefik',source:{kind:'IngressRoute',name:'parent'}});
  assert.equal(C.management(direct).mode,'native');
  assert.equal(C.management(route).mode,'traefik');
  for (const value of [instance({management:'external'}),instance({management:'future-provider'}),instance({management:'native',source:route.source}),instance({management:'traefik'})]) assert.equal(C.management(value).mode,'external');
  assert.deepEqual(C.groupManagement(group({instances:[direct,route,direct]})).map(value => value.mode),['native','traefik']);
  assert.equal(C.management(instance()).mode,'native');
  assert.equal(C.management(instance({source:route.source})).mode,'traefik');
  assert.equal(C.filterGroups([group({instances:[route]})],{query:'webhook parent'}).length,1);
});
test('signal names and summary counts are human-readable', () => {
  assert.equal(C.signalLabel('dns_target_count'), 'DNS target count');
  assert.equal(C.signalLabel('missing_object'), 'Missing GSLB');
  assert.equal(C.signalLabel('new_signal'), 'New signal');
  assert.equal(C.countLabel(1,'warning'), '1 warning');
  assert.equal(C.countLabel(2,'DNS difference'), '2 DNS differences');
});
test('equal local DNS counts are consistent even when IP sets differ', () => {
  assert.equal(C.dnsDifference(group()), false);
  assert.equal(C.groupStatus(group()).tone, 'consistent');
  assert.equal(C.dnsDifference(group({instances:[instance({dnsTargets:['192.0.2.1','192.0.2.2']}), instance({dnsTargets:['198.51.100.1','198.51.100.2']})]})), false);
});
test('DNS target count differences are informational, not warnings', () => {
  const data = group({instances:[instance(), instance({dnsTargets:['198.51.100.1','198.51.100.2']})]});
  assert.equal(C.dnsDifference(data), true);
  assert.equal(C.hasWarnings(data), false);
  assert.deepEqual(C.groupStatus(data), {tone:'info', label:'DNS differences'});
});
test('server dns_target_count signal drives DNS filtering', () => {
  const data = group({signals:[{code:'dns_target_count', severity:'info', message:'Observed target counts differ'}]});
  assert.equal(C.dnsDifference(data), true);
  assert.deepEqual(C.filterGroups([data, group()], {dns:true}), [data]);
  assert.equal(C.dnsSignal({code:'dns_target_count', severity:'unknown'}), false);
});
test('unobserved DNS is unknown, not an empty answer or DNS difference', () => {
  const data = group({instances:[instance({dnsObserved:false, dnsTargets:[]}), instance()]});
  assert.equal(C.dnsDifference(data), false);
  assert.equal(C.hasWarnings(data), false);
  assert.equal(C.groupStatus(data).tone, 'unknown');
});
test('an observed empty DNS answer participates in count comparison', () => {
  assert.equal(C.dnsDifference(group({instances:[instance({dnsTargets:[]}), instance()]})), true);
});
test('optional API warnings and unknown health never imply warning severity', () => {
  const data = group({state:'unknown', instances:[instance({health:'Unknown', warnings:['Optional EndpointSlice API is unavailable']})], signals:[{code:'endpoint_data', severity:'unknown', message:'Not accessible'}]});
  assert.equal(C.hasWarnings(data), false);
  assert.equal(C.filterGroups([data], {warnings:true}).length, 0);
  assert.equal(C.groupStatus(data).tone, 'unknown');
});
test('server warning/error states and severity are authoritative', () => {
  for (const state of ['warning','error']) assert.equal(C.hasWarnings(group({state})), true);
  for (const severity of ['warning','error']) assert.equal(C.hasWarnings(group({signals:[{code:'policy', severity}]})), true);
  assert.equal(C.hasWarnings(group({signals:[{severity:'info'}]})), false);
});
test('unknown collection health takes precedence over DNS informational state', () => {
  const dns = {code:'dns_target_count', severity:'info'};
  assert.equal(C.groupStatus(group({state:'unknown', signals:[dns]})).tone, 'unknown');
  assert.equal(C.groupStatus(group({signals:[dns, {code:'collection_stale', severity:'unknown'}]})).tone, 'unknown');
  assert.equal(C.groupStatus(group({state:'warning', signals:[dns, {severity:'unknown'}]})).tone, 'warning');
});
test('health preserves unknown and endpoint zero is not missing data', () => {
  assert.equal(C.health('Healthy').tone, 'consistent');
  assert.equal(C.health('unhealthy').tone, 'warning');
  assert.equal(C.health('not reported').tone, 'unknown');
  assert.equal(C.aggregateHealth([]).tone, 'unknown');
  assert.equal(C.aggregateHealth([instance(), instance({health:'Unknown'})]).tone, 'unknown');
  assert.deepEqual(C.endpointSummary([{endpointDataAvailable:true, readyEndpoints:0, totalEndpoints:0}, {endpointDataAvailable:false, readyEndpoints:99, totalEndpoints:99}]), {ready:0, total:0, known:1, count:2});
});
test('native backend failure statuses are warnings with specific labels', () => {
  for (const [value,label] of [['NotFound','Not found'],['NoEndpoints','No endpoints'],['NotReady','Not ready']]) {
    assert.deepEqual(C.health(value), {tone:'warning', label});
    assert.equal(C.health(value.toLowerCase()).tone, 'warning');
    assert.equal(C.aggregateHealth([instance(), instance({health:value})]).tone, 'warning');
  }
  for (const value of [undefined, '', 'Unknown', 'OptionalDataUnavailable']) assert.deepEqual(C.health(value), {tone:'unknown', label:'Unknown'});
});
test('primary inventory completeness is independent from read availability and optional warnings', () => {
  assert.deepEqual(C.inventoryStatus({available:true, objectsComplete:false}), {tone:'unknown', label:'Partial GSLB inventory'});
  assert.deepEqual(C.inventoryStatus({available:true, objectsComplete:true, warnings:['Optional endpoint API unavailable']}), {tone:'consistent', label:'Complete'});
  assert.deepEqual(C.inventoryStatus({available:false, objectsComplete:true}), {tone:'unknown', label:'Unknown'});
  assert.deepEqual(C.inventoryStatus({available:true}), {tone:'unknown', label:'Not reported'});
  assert.deepEqual(C.inventoryStatus(undefined), {tone:'unknown', label:'Unknown'});
});
test('source_partial remains unknown and does not become a warning or DNS info status', () => {
  const data = group({signals:[{code:'source_partial', severity:'unknown'}, {code:'dns_target_count', severity:'info'}]});
  assert.equal(C.signalLabel('source_partial'), 'Partial GSLB inventory');
  assert.equal(C.hasWarnings(data), false);
  assert.equal(C.groupStatus(data).tone, 'unknown');
  assert.equal(C.filterGroups([data], {warnings:true}).length, 0);
});
test('search covers exact resource, target, URL, service and IPv6 fields', () => {
  const data = group({instances:[instance({target:{namespace:'apps', name:'catalog-route'}, dnsTargets:['2001:db8::1'], backends:[{namespace:'apps', service:'catalog-service', port:'http'}]})]});
  for (const query of ['CATALOG-GSLB', 'https://catalog.example.org/', 'catalog-route', 'apps/catalog-service', '2001:db8::1', 'catalog service']) assert.equal(C.filterGroups([data], {query}).length, 1, query);
  assert.equal(C.filterGroups([data], {query:'absent'}).length, 0);
});
test('filters compose without conflating warnings and DNS information', () => {
  const info = group({id:'info', signals:[{code:'dns_target_count', severity:'info'}]});
  const warn = group({id:'warn', state:'warning'});
  const both = group({id:'both', state:'warning', signals:info.signals});
  assert.deepEqual(C.filterGroups([info,warn,both], {warnings:true}).map(item => item.id), ['warn','both']);
  assert.deepEqual(C.filterGroups([info,warn,both], {warnings:true,dns:true}).map(item => item.id), ['both']);
});
test('group identity and deep links are IDs, not duplicate resource names', () => {
  const first = group({id:'scope/a?x="quoted"&y=1'}), second = group({id:'scope/b'});
  assert.equal(C.groupFromHash(C.groupHash(first.id)), first.id);
  const valid = C.validateOverview({namespace:'default', sites:[], groups:[first,second], capabilities:{readOnly:true}});
  assert.equal(valid.groups.find(item => item.id === second.id), second);
  assert.throws(() => C.validateOverview({...valid, groups:[first,first]}), /duplicate group ID/);
});
test('overview validation allows read-only and mutable capabilities and Go nil collections', () => {
  assert.deepEqual(C.validateOverview({namespace:'default', groups:null, sites:null, capabilities:{readOnly:true}}).groups, []);
  assert.equal(C.validateOverview({namespace:'default', capabilities:{readOnly:false, operations:{native:['create']}}}).capabilities.readOnly, false);
  assert.throws(() => C.validateOverview({namespace:'default', capabilities:{readOnly:'false'}}), /overview/);
  assert.throws(() => C.validateOverview({namespace:'default', groups:{}, capabilities:{readOnly:true}}), /overview/);
});
test('namespace selection has an explicit empty scope, never an empty overview request', () => {
  assert.equal(C.initialNamespace([]), null);
  assert.equal(C.initialNamespace(null), null);
  assert.equal(C.initialNamespace(['z','a']), 'a');
  assert.equal(C.initialNamespace(['z','default','a']), 'default');
});
test('omitted TTL and unsupported strategies are not invented', () => {
  assert.equal(C.ttlLabel({}), 'Not reported');
  assert.equal(C.ttlLabel({dnsTtlSeconds:0}), 'Not reported');
  assert.equal(C.policyLabel({strategy:'custom'}), 'custom');
  assert.equal(C.policyLabel({strategy:'failover',primaryGeoTag:'west'}), 'Failover / primary west');
  assert.equal(C.policyLabel({strategy:'failover'}), 'Failover / primary not reported');
  assert.equal(C.formatTime('0001-01-01T00:00:00Z'), 'Not reported');
  assert.equal(C.formatTime('invalid'), 'Not reported');
});
test('DNS model is fed only reported policy and actual GeoTags', () => {
  const data = group(); const input = C.dnsInput(data, data.instances[0]);
  assert.equal(input.context, data.id); assert.equal(input.cod, 'east');
  assert.deepEqual(input.tags, ['east','west']); assert.equal(input.mode, 'roundRobin');
  assert.equal(C.dnsInput(data, instance({policy:{}})).ttl, '');
  assert.equal(DNS.normalize(input).error, '');
});
test('original DNS round-robin playback rotates ordered answers', () => {
  const config = DNS.normalize({tags:['east','west'], mode:'roundRobin'});
  assert.deepEqual(DNS.frame(config, 0).answer, ['east','west']);
  assert.deepEqual(DNS.frame(config, 1).answer, ['west','east']);
});
test('original weighted DNS playback respects weights and excludes zero weight', () => {
  const config = DNS.normalize({tags:['east','west','zero'], mode:'weightedRoundRobin', weights:{east:3,west:1,zero:0}});
  assert.equal(config.error, '');
  const counts = {east:0,west:0,zero:0}; let scores = [];
  for (let step = 0; step < 40; step++) { const next = DNS.nextWeighted(config, scores); scores = next.scores; counts[next.first]++; assert.equal(DNS.frame(config, step, next.first).answer.includes('zero'), false); }
  assert.deepEqual(counts, {east:30,west:10,zero:0});
});
test('original failover playback retains resolver cache then fails back', () => {
  const config = DNS.normalize({tags:['east','west'], mode:'failover', primary:'east', ttl:30});
  assert.equal(DNS.frame(config, 1).cache, true);
  assert.deepEqual(DNS.frame(config, 1).selected, ['west']);
  assert.deepEqual(DNS.frame(config, 1).answer, ['east']);
  assert.deepEqual(DNS.frame(config, 3).answer, ['west']);
  assert.deepEqual(DNS.frame(config, 5).answer, ['east']);
});
test('original GeoIP scenario has an illustrative fallback', () => {
  const config = DNS.normalize({tags:['east','west'], mode:'geoip'});
  assert.equal(DNS.frame(config, 4).phase, 'fallback');
  assert.equal(DNS.frame(config, 4).answer.length, 2);
});
test('missing/invalid policy input is unavailable rather than a fake simulation', () => {
  for (const input of [{tags:[],mode:'roundRobin'}, {tags:['east'],mode:'other'}, {tags:['east'],mode:'failover',primary:'absent'}, {tags:['east'],mode:'weightedRoundRobin',weights:{east:0}}, {tags:['east'],mode:'roundRobin',ttl:-1}]) {
    const config = DNS.normalize(input); assert.ok(config.error); assert.equal(DNS.frame(config,0).moving, false);
  }
});
test('latest request guard discards slow successes and aborts superseded fetch', async () => {
  const request = C.createLatestRequest(); let first, oldSignal; const commits = [];
  const pending = request.run(signal => { oldSignal = signal; return new Promise(resolve => { first = resolve; }); }, value => commits.push(value), assert.fail);
  await request.run(async () => 'new', value => commits.push(value), assert.fail);
  assert.equal(oldSignal.aborted, true); first('old'); await pending;
  assert.deepEqual(commits, ['new']);
});
test('late failure cannot replace the current namespace state', async () => {
  const request = C.createLatestRequest(); let reject; const errors = [];
  const pending = request.run(() => new Promise((_, fail) => { reject = fail; }), assert.fail, error => errors.push(error));
  await request.run(async () => 'current', () => {}, assert.fail);
  reject(new Error('old namespace failed')); await pending; assert.deepEqual(errors, []);
});
test('cancellation prevents callbacks after teardown', async () => {
  const request = C.createLatestRequest(); let complete;
  const pending = request.run(() => new Promise(resolve => { complete = resolve; }), assert.fail, assert.fail);
  request.cancel(); complete('done'); await pending;
});
test('network client sends only same-origin read-only GETs', async () => {
  let options;
  const data = await C.getJSON('/api/v1/overview?namespace=default', {fetcher:async (url, input) => { assert.equal(url, '/api/v1/overview?namespace=default'); options = input; return {ok:true, json:async () => ({ok:true})}; }});
  assert.deepEqual(data, {ok:true}); assert.equal(options.method, 'GET'); assert.equal(options.credentials, 'same-origin'); assert.equal(options.redirect, 'error');
  await assert.rejects(C.getJSON('https://example.org/api/v1/clusters'), /Unsupported/);
  await assert.rejects(C.getJSON('/api/v1/mutations'), /Unsupported/);
});
test('network errors include the backend status and message', async () => {
  await assert.rejects(C.getJSON('/api/v1/namespaces', {fetcher:async () => ({ok:false, status:403, json:async () => ({error:'Access denied'})})}), /HTTP 403: Access denied/);
});
test('request timeout and external cancellation abort the fetch', async () => {
  const fetcher = (_, {signal}) => new Promise((_, reject) => { if (signal.aborted) reject(new DOMException('Aborted','AbortError')); else signal.addEventListener('abort', () => reject(new DOMException('Aborted','AbortError')), {once:true}); });
  await assert.rejects(C.getJSON('/api/v1/clusters', {timeout:10,fetcher}), /timed out/);
  const controller = new AbortController(); const pending = C.getJSON('/api/v1/clusters', {signal:controller.signal,fetcher}); controller.abort();
  await assert.rejects(pending, {name:'AbortError'});
});
test('theme defaults to light and honors only an explicitly saved dark theme', () => {
  const code = fs.readFileSync(path.join(root, 'web/static/k8gb-theme.js'), 'utf8');
  for (const [stored, expected] of [[null,'light'],['dark','dark'],['light','light'],['invalid','light']]) {
    const document = {documentElement:{dataset:{}}}; vm.runInNewContext(code, {document,localStorage:{getItem:() => stored}}); assert.equal(document.documentElement.dataset.theme, expected);
  }
  const document = {documentElement:{dataset:{}}}; vm.runInNewContext(code, {document,localStorage:{getItem:() => { throw new Error('Denied'); }}}); assert.equal(document.documentElement.dataset.theme, 'light');
});
test('all referenced runtime assets are local and embedded, including Lucide license', () => {
  const html = fs.readFileSync(path.join(root, 'web/index.html'), 'utf8');
  for (const match of html.matchAll(/(?:src|href)="(\/static\/[^\"]+)"/g)) assert.equal(fs.existsSync(path.join(root,'web',match[1])), true, match[1]);
  assert.equal(/(?:src|href)="https?:/.test(html), false);
  assert.equal(fs.existsSync(path.join(root,'web/static/vendor/lucide.LICENSE')), true);
  assert.match(fs.readFileSync(path.join(root,'web/embed.go'),'utf8'), /go:embed index\.html static\/\*/);
  assert.match(fs.readFileSync(path.join(root,'web/static/k8gb-dns.js'),'utf8'), /root\.K8gbDNS = factory/);
  for (const file of ['k8gb-app.js','k8gb-dns.js','k8gb.css','k8gb-core.js']) assert.doesNotMatch(fs.readFileSync(path.join(root,'web/static',file),'utf8'), /DiagGslb|window\.Diag|wholeDiag|fontawesome|cdnjs/);
});
