#!/usr/bin/env node
'use strict';

// Intentionally standalone: no Kubernetes writes, shell, npm packages or auto-run on import.
const fs = require('node:fs');
const http = require('node:http');
const net = require('node:net');
const path = require('node:path');
const { execFile } = require('node:child_process');
const { randomUUID } = require('node:crypto');
const { isDeepStrictEqual: equal } = require('node:util');
const { performance } = require('node:perf_hooks');

const API = 'k8gb-ui/v1alpha1';
const NS = 'demo';
const HOST = 'hello.apps.k8gb.test';
const IDS = ['east', 'west'];
const DNS = [{ clusterId: 'east', port: 15053 }, { clusterId: 'west', port: 15054 }];
const OVERRIDE = '203.0.113.77';
const FIELDS = {
  excluded: '/metadata/annotations/k8gb-ui.io~1site-excluded',
  ips: '/metadata/annotations/k8gb.io~1exposed-ip-addresses',
  hostnames: '/metadata/annotations/k8gb.io~1exposed-hostnames',
  snapshot: '/metadata/annotations/k8gb-ui.io~1ip-override-state',
};
const HELP = `Standalone local k8gb DNS acceptance probe (Node.js >=18 and dig).

Usage:
  K8GB_LAB_EAST_IP=<private-kind-node-ip> K8GB_LAB_WEST_IP=<private-kind-node-ip> \\
    node tests/dns-lab/probe.cjs [--run-mutations] [--report=/absolute/new-report.json]

Default: read-only baseline inspection, never plan or apply; status READ_ONLY, not PASS.
--run-mutations                  Explicitly permit the four east-site operations.
--console-origin=http://127.0.0.1:8791
                                 Only literal 127.0.0.1 or localhost HTTP origins.
--dns-timeout-ms=120000           Per-phase DNS and resource convergence deadline.
--poll-interval-ms=2000           Poll interval (100..10000 ms).
--plan-delay-ms=0                Review delay before applying the unchanged signed plan
                                 (0..240000 ms); e.g. 10000 exposes live RV churn.
--check-localtargets             Record optional localtargets.<hostname> diagnostics.
--report=/absolute/new-report.json
                                 Exclusive-create JSON artifact; never overwrite.
--help                           Print this help without contacting anything.

Fixed scope: clusters east/west only; namespace demo; hello.apps.k8gb.test;
DNS 127.0.0.1:15053/15054, UDP and TCP. No other target selection is supported.
Unknown apply outcomes are never retried or automatically undone.
`;

class ProbeError extends Error {
  constructor(code, message, outcomeUnknown = false) {
    super(message);
    this.code = code;
    this.outcomeUnknown = outcomeUnknown;
  }
}
function check(condition, message, code = 'safety_check') {
  if (!condition) throw new ProbeError(code, message);
}
function errorJSON(error) {
  return { code: error.code || 'probe_error', message: error.message, outcomeUnknown: error.outcomeUnknown === true };
}
function canonicalHost(value) { return String(value).toLowerCase().replace(/\.$/, ''); }
function sorted(values) { return [...new Set(values)].sort(); }
function sameIPs(actual, expected) { return equal(sorted(actual), sorted(expected)); }
function privateIPv4(value) {
  if (typeof value !== 'string' || !net.isIPv4(value)) return false;
  const [a, b] = value.split('.').map(Number);
  return a === 10 || a === 172 && b >= 16 && b <= 31 || a === 192 && b === 168;
}
function optionsFrom(argv, env) {
  const options = { runMutations: false, origin: 'http://127.0.0.1:8791', timeoutMs: 120000, intervalMs: 2000, planDelayMs: 0, localtargets: false };
  const seen = new Set();
  for (const arg of argv) {
    const key = arg.split('=')[0];
    check(!seen.has(key), `Duplicate argument: ${key}`, 'invalid_arguments');
    seen.add(key);
    if (arg === '--help') options.help = true;
    else if (arg === '--run-mutations') options.runMutations = true;
    else if (arg === '--check-localtargets') options.localtargets = true;
    else if (arg.startsWith('--console-origin=')) options.origin = arg.slice('--console-origin='.length);
    else if (arg.startsWith('--report=')) options.report = arg.slice('--report='.length);
    else if (arg.startsWith('--dns-timeout-ms=')) options.timeoutMs = Number(arg.slice('--dns-timeout-ms='.length));
    else if (arg.startsWith('--poll-interval-ms=')) options.intervalMs = Number(arg.slice('--poll-interval-ms='.length));
    else if (arg.startsWith('--plan-delay-ms=')) options.planDelayMs = Number(arg.slice('--plan-delay-ms='.length));
    else throw new ProbeError('invalid_arguments', `Unsupported argument: ${arg}`);
  }
  check(/^http:\/\/(127\.0\.0\.1|localhost):[1-9][0-9]{0,4}$/.test(options.origin), 'Console must be an explicit HTTP loopback origin without credentials, path, query or fragment', 'invalid_arguments');
  const url = new URL(options.origin);
  check(url.origin === options.origin && Number(url.port) <= 65535, 'Console origin must be canonical and include its non-default port', 'invalid_arguments');
  check(Number.isInteger(options.timeoutMs) && options.timeoutMs >= 1000 && options.timeoutMs <= 600000, 'DNS timeout must be 1000..600000 ms', 'invalid_arguments');
  check(Number.isInteger(options.intervalMs) && options.intervalMs >= 100 && options.intervalMs <= 10000, 'Poll interval must be 100..10000 ms', 'invalid_arguments');
  check(Number.isInteger(options.planDelayMs) && options.planDelayMs >= 0 && options.planDelayMs <= 240000, 'Plan review delay must be 0..240000 ms', 'invalid_arguments');
  if (options.report !== undefined) check(path.isAbsolute(options.report), '--report must be an absolute new file path', 'invalid_arguments');
  if (!options.help) {
    options.baseline = { east: env.K8GB_LAB_EAST_IP, west: env.K8GB_LAB_WEST_IP };
    for (const id of IDS) check(privateIPv4(options.baseline[id]), `K8GB_LAB_${id.toUpperCase()}_IP must be a canonical RFC1918 IPv4 kind node address`, 'invalid_baseline');
    check(options.baseline.east !== options.baseline.west, 'The two kind nodes must have distinct baseline IPs', 'invalid_baseline');
  }
  return options;
}
function sameRef(a, b) {
  return !!a && !!b && ['apiVersion', 'kind', 'namespace', 'name', 'uid'].every(key => a[key] === b[key]);
}
function pin(ref, kind) {
  check(ref && ref.kind === kind && ref.namespace === NS && typeof ref.name === 'string' && /^[a-z0-9][a-z0-9.-]*$/.test(ref.name), `Invalid ${kind} target scope`);
  check(typeof ref.uid === 'string' && ref.uid.length > 0 && typeof ref.resourceVersion === 'string' && ref.resourceVersion.length > 0, `${kind} must have a fresh UID and resourceVersion`);
  if (kind === 'Gslb') check(ref.apiVersion === 'k8gb.io/v1beta1', 'Only the canonical generated Gslb is in lab scope');
  if (kind === 'IngressRoute') check(['traefik.io/v1alpha1', 'traefik.containo.us/v1alpha1'].includes(ref.apiVersion), 'Unsupported Source API');
  if (kind === 'Ingress') check(ref.apiVersion === 'networking.k8s.io/v1', 'Unsupported generated Ingress API');
  return ref;
}
function exactIDs(values, label) {
  check(Array.isArray(values) && values.length === 2 && equal([...values].sort(), IDS), `${label} must contain exactly east and west, once each`);
}
function policy(policyValue) {
  check(policyValue?.strategy === 'roundRobin' && !policyValue.primaryGeoTag && Object.keys(policyValue.weights || {}).length === 0, 'Only unweighted roundRobin baseline is allowed');
}
function traffic(value, label) {
  check(value && ['excluded', 'overrideManaged', 'overrideConflict'].every(key => typeof value[key] === 'boolean') && Array.isArray(value.ips) && Array.isArray(value.hostnames), `${label}: incomplete traffic state`);
  check(!value.overrideConflict, `${label}: external/malformed override snapshot conflict`);
}
function hasUnknown(value) {
  if (!value || typeof value !== 'object') return false;
  return Object.entries(value).some(([key, child]) => (key === 'outcomeUnknown' || key === 'OutcomeUnknown') && child === true || hasUnknown(child));
}

// Parse only real dig response RRs, not +short text, comments, or an expected-value fixture.
function parseDig(stdout, hostname) {
  const header = stdout.match(/;; ->>HEADER<<-.*status:\s*([A-Z0-9]+),/);
  const flagLine = stdout.match(/;; flags:\s*([^;]*);/);
  check(header && flagLine, 'dig did not return a DNS response header', 'invalid_dns_response');
  const flags = flagLine[1].trim().split(/\s+/);
  const records = [];
  for (const raw of stdout.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith(';')) continue;
    const rr = line.match(/^(\S+)\s+(\d+)\s+IN\s+(\S+)\s+(.+)$/);
    check(rr, `Unparseable dig answer: ${line}`, 'invalid_dns_response');
    const record = { name: canonicalHost(rr[1]), ttl: Number(rr[2]), type: rr[3], data: rr[4].trim() };
    if (record.type === 'A') check(net.isIPv4(record.data), 'Invalid IPv4 A RDATA returned by dig', 'invalid_dns_response');
    records.push(record);
  }
  return {
    rcode: header[1], flags, records,
    valid: header[1] === 'NOERROR' && flags.includes('qr') && !flags.includes('tc'),
    actualIPs: sorted(records.filter(rr => rr.type === 'A' && rr.name === canonicalHost(hostname)).map(rr => rr.data)),
  };
}
function queryDNS(site, protocol, hostname = HOST, budgetMs = 4000) {
  const startedAt = new Date().toISOString(), start = performance.now();
  const args = ['@127.0.0.1', '-p', String(site.port), `${hostname}.`, 'A', '+time=2', '+tries=1', '+nosearch', '+nocmd', '+noquestion', '+nostats', '+comments', '+answer', '+noauthority', '+noadditional', ...(protocol === 'tcp' ? ['+tcp'] : ['+notcp', '+ignore'])];
  return new Promise(resolve => {
    execFile('dig', args, { timeout: Math.max(1, Math.min(4000, budgetMs)), maxBuffer: 256 * 1024, encoding: 'utf8', env: { ...process.env, HOME: '/dev/null', LC_ALL: 'C', LANG: 'C' } }, (error, stdout, stderr) => {
      const sample = { clusterId: site.clusterId, server: '127.0.0.1', port: site.port, protocol, hostname, startedAt, finishedAt: new Date().toISOString(), durationMs: Math.round(performance.now() - start), args, stdout, stderr, valid: false, actualIPs: [] };
      if (error) sample.error = { code: String(error.code || 'dig_failed'), signal: error.signal || null, message: error.message };
      try { Object.assign(sample, parseDig(stdout, hostname)); }
      catch (parseError) { sample.parseError = errorJSON(parseError); }
      if (error) sample.valid = false;
      resolve(sample);
    });
  });
}
function pause(ms) { return new Promise(resolve => setTimeout(resolve, ms)); }

class Probe {
  constructor(options, report, checkpoint) {
    this.options = options;
    this.report = report;
    this.checkpoint = checkpoint;
    this.owned = { exclusion: null, override: null };
    this.unknown = false;
    this.interrupted = null;
    this.cleaning = false;
  }
  guard() {
    check(!this.unknown, 'An apply outcome is unknown; automatic mutation/cleanup is forbidden', 'manual_attention');
    if (this.interrupted && !this.cleaning) throw new ProbeError('interrupted', `Received ${this.interrupted}; stopping test operations`);
  }
  async api(endpoint, payload) {
    const applying = endpoint === '/api/v1/mutations/apply';
    if (payload !== undefined) {
      this.guard();
      check(this.options.runMutations, 'Writes require --run-mutations');
      check(['/api/v1/mutations/plan', '/api/v1/mutations/apply'].includes(endpoint), 'Unsupported write endpoint');
    }
    const start = performance.now();
    const entry = { endpoint, method: payload === undefined ? 'GET' : 'POST', startedAt: new Date().toISOString() };
    this.report.http.push(entry);
    try {
      const response = await new Promise((resolve, reject) => {
        const body = payload === undefined ? null : JSON.stringify(payload);
        const url = new URL(endpoint, this.options.origin);
        const headers = { Accept: 'application/json', 'Cache-Control': 'no-store' };
        if (body !== null) Object.assign(headers, { 'Content-Type': 'application/json', Origin: this.options.origin, 'X-K8gb-Intent': 'mutation', 'Content-Length': Buffer.byteLength(body) });
        // Force localhost to literal loopback, independently of DNS/hosts/proxy settings.
        const request = http.request(url, { method: entry.method, headers, agent: false, family: 4, lookup: (_name, opts, callback) => opts.all ? callback(null, [{ address: '127.0.0.1', family: 4 }]) : callback(null, '127.0.0.1', 4) }, res => {
          const chunks = [];
          let length = 0;
          res.on('data', chunk => {
            length += chunk.length;
            if (length > 8 * 1024 * 1024) request.destroy(new Error('Console response exceeds 8 MiB'));
            else chunks.push(chunk);
          });
          res.on('error', reject);
          res.on('aborted', () => reject(new Error('Console response aborted')));
          res.on('end', () => resolve({ status: res.statusCode, type: res.headers['content-type'], body: Buffer.concat(chunks).toString('utf8') }));
        });
        const timer = setTimeout(() => request.destroy(new Error('Console request deadline exceeded')), 45000);
        request.on('error', reject);
        request.on('close', () => clearTimeout(timer));
        request.end(body);
      });
      entry.status = response.status;
      if (response.status < 200 || response.status >= 300) {
        let unknown = false;
        try { unknown = hasUnknown(JSON.parse(response.body)); } catch { /* Retain HTTP classification. */ }
        throw new ProbeError('console_http', `${endpoint}: HTTP ${response.status}: ${response.body.slice(0, 1000)}`, applying && (response.status >= 500 || unknown || ![400, 401, 403, 404, 405, 409, 415, 422].includes(response.status)));
      }
      check(/^application\/json(?:;|$)/i.test(response.type || ''), 'Console returned a non-JSON response', 'invalid_console_response');
      let data;
      try { data = JSON.parse(response.body); }
      catch { throw new ProbeError('invalid_console_response', 'Console returned malformed JSON', applying); }
      check(data && typeof data === 'object', 'Console returned an invalid JSON document', 'invalid_console_response');
      return data;
    } catch (error) {
      // Transport and response-shape failures during apply cannot establish write outcome.
      const problem = error instanceof ProbeError ? error : new ProbeError('console_transport', error.message, applying);
      if (applying && problem.code === 'invalid_console_response') problem.outcomeUnknown = true;
      entry.error = errorJSON(problem);
      throw problem;
    } finally {
      entry.finishedAt = new Date().toISOString();
      entry.durationMs = Math.round(performance.now() - start);
    }
  }
  async identity() {
    const session = await this.api('/api/v1/session');
    const clusters = await this.api('/api/v1/clusters');
    check(session.mode === 'local' && typeof session.subject === 'string' && session.subject.trim(), 'Console must authenticate a local session');
    check(clusters.apiVersion === API && clusters.demo === false, 'Demo/unknown Console is not a real acceptance target');
    exactIDs(clusters.clusters?.map(cluster => cluster.id), 'Console cluster inventory');
    if (this.options.runMutations) check(session.readOnly === false && clusters.readOnly === false, 'Console local-admin writes must be enabled');
    this.subject = session.subject;
    this.report.session = session;
    this.report.clusters = clusters;
  }
  async model(label) {
    const startedAt = new Date().toISOString();
    const catalog = await this.api(`/api/v1/catalog?namespace=${NS}`);
    const overview = await this.api(`/api/v1/overview?namespace=${NS}`);
    for (const [name, value] of [['catalog', catalog], ['overview', overview]]) {
      check(value.apiVersion === API && value.namespace === NS, `${name}: unexpected API or namespace`);
    }
    exactIDs(catalog.sites?.map(site => site.clusterId), 'Catalog sites');
    exactIDs(overview.sites?.map(site => site.cluster?.id), 'Overview sites');
    check(Array.isArray(overview.groups), 'Overview groups are missing');
    const groups = overview.groups.filter(group => canonicalHost(group.hostname) === HOST);
    check(groups.length === 1 && groups[0].namespace === NS && groups[0].zone, 'Missing or ambiguous global hostname group');
    const group = groups[0];
    exactIDs(group.instances?.map(instance => instance.clusterId), 'Hostname instances');
    const sites = {};
    for (const id of IDS) {
      const site = overview.sites.find(item => item.cluster.id === id);
      check(site.available === true && site.objectsComplete === true && !site.error, `${id}: incomplete/unavailable overview inventory`);
      const collected = Date.parse(site.collectedAt);
      check(Number.isFinite(collected) && Date.now() - collected < 120000 && collected - Date.now() < 30000, `${id}: stale/invalid overview timestamp`);
      const instance = group.instances.find(item => item.clusterId === id);
      check(canonicalHost(instance.hostname) === HOST, `${id}: instance hostname is outside lab scope`);
      pin(instance.resource, 'Gslb');
      pin(instance.target, 'Ingress');
      check(instance.source && instance.source.namespace === NS && instance.source.uid, `${id}: Gslb must have an owning Source`);
      const catalogSite = catalog.sites.find(item => item.clusterId === id);
      check(!catalogSite.error && catalogSite.catalog?.clusterId === id && catalogSite.catalog.namespace === NS && catalogSite.catalog.apiVersion === API, `${id}: invalid/unavailable fresh catalog`);
      check(Array.isArray(catalogSite.catalog.candidates), `${id}: catalog candidates missing`);
      const candidates = catalogSite.catalog.candidates.filter(candidate => Array.isArray(candidate.hostnames) && candidate.hostnames.some(host => canonicalHost(host) === HOST));
      check(candidates.length === 1, `${id}: ambiguous catalog candidates for hostname`);
      const source = candidates[0];
      pin(source.resource, 'IngressRoute');
      check(sameRef(instance.source, source.resource), `${id}: catalog Source does not exactly match overview owner UID/API/kind/name`);
      check(source.provider === 'traefik' && source.enabled === true && source.eligible === true && typeof source.siteTrafficEligible === 'boolean', `${id}: Source is not an enabled Traefik site candidate`);
      check(source.hostnames.length === 1 && source.traefik?.local === false && canonicalHost(source.traefik.hostname) === HOST, `${id}: Source has additional/local hosts outside lab scope`);
      for (const other of overview.groups) {
        for (const sibling of other.instances || []) {
          if (sibling.clusterId === id && sameRef(sibling.resource, instance.resource)) check(other === group && sibling === instance, `${id}: Gslb also affects another/duplicate hostname`);
          if (sibling.clusterId === id && sameRef(sibling.source, source.resource)) check(other === group && sibling === instance, `${id}: Source also owns another global/local object outside test scope`);
        }
      }
      policy(instance.policy);
      policy(source.policy);
      traffic(instance.traffic, `${id} Gslb`);
      traffic(source.traffic, `${id} Source`);
      check(!source.traffic.overrideManaged && source.traffic.ips.length === 0 && source.traffic.hostnames.length === 0, `${id}: Source has unexpected override metadata`);
      if (this.initial) {
        const original = this.initial.sites[id];
        check(sameRef(original.instance.resource, instance.resource) && sameRef(original.instance.target, instance.target) && sameRef(original.source.resource, source.resource), `${id}: an owned resource was replaced`);
        check(equal(instance.policy, original.instance.policy) && equal(source.policy, original.source.policy), `${id}: policy changed during probe`);
      }
      if (this.options.runMutations) {
        const caps = catalogSite.catalog.capabilities;
        check(caps?.readOnly === false, `${id}: catalog is read-only`);
        for (const [provider, operations] of Object.entries({ traefik: ['excludeSite', 'includeSite'], traffic: ['overrideIPs', 'resetIPs'] })) {
          check(operations.every(operation => caps.operations?.[provider]?.includes(operation)), `${id}: required ${provider} operations unavailable`);
        }
      }
      sites[id] = { instance, source, collectedAt: site.collectedAt, warnings: { site: site.warnings, catalog: catalogSite.catalog.warnings } };
    }
    const model = { label, startedAt, observedAt: new Date().toISOString(), sites };
    this.report.resources.push(model);
    return model;
  }
  checkState(model, expected, allowPendingExclusion = false) {
    for (const id of IDS) {
      const { instance, source } = model.sites[id];
      const excluded = id === 'east' && expected.excluded;
      const overridden = id === 'east' && expected.override;
      if (!allowPendingExclusion) check(source.siteTrafficEligible === true, `${id}: owned global site pair has not reconciled: ${source.siteTrafficReason || 'ineligible'}`, 'not_converged');
      check(source.traffic.excluded === excluded, `${id}: Source exclusion differs from this probe's known state`, 'unexpected_state');
      if (!allowPendingExclusion) check(instance.traffic.excluded === excluded, `${id}: generated exclusion has not converged`, 'not_converged');
      check(instance.traffic.overrideManaged === overridden, `${id}: managed override differs from this probe's known state`, 'unexpected_state');
      check(instance.traffic.hostnames.length === 0, `${id}: external exposed-hostnames override is not baseline`, 'unexpected_state');
      const baselineIPs = this.initial ? this.initial.sites[id].instance.traffic.ips : instance.traffic.ips;
      if (!this.initial) check(baselineIPs.length === 0 || equal(baselineIPs, [this.options.baseline[id]]), `${id}: externally configured IPs do not match the published kind-node baseline`, 'unsafe_initial_state');
      check(equal(instance.traffic.ips, overridden ? [OVERRIDE] : baselineIPs), `${id}: unexpected installed IP override`, 'unexpected_state');
      if (!allowPendingExclusion) check(String(instance.health).toLowerCase() === (excluded ? 'excluded' : 'healthy'), `${id}: application health is ${instance.health}`, 'not_converged');
    }
  }
  async resources(label, expected, record) {
    const start = performance.now(), deadline = start + this.options.timeoutMs;
    record.resourcePolls = [];
    while (true) {
      if (!this.cleaning && this.interrupted) this.guard();
      const model = await this.model(label);
      try {
        this.checkState(model, expected);
        record.resourcePolls.push({ at: model.observedAt, converged: true });
        record.resourceDurationMs = Math.round(performance.now() - start);
        return model;
      } catch (error) {
        record.resourcePolls.push({ at: model.observedAt, converged: false, error: errorJSON(error) });
        // Expected asynchronous exclusion changes may lag; foreign IP/policy changes may not.
        if (!['not_converged', 'unexpected_state'].includes(error.code)) throw error;
        if (error.code === 'unexpected_state') {
          this.checkState(model, expected, true);
        }
        if (performance.now() >= deadline) throw new ProbeError('resource_timeout', `${label}: resource state did not converge within ${this.options.timeoutMs} ms: ${error.message}`);
        await pause(Math.min(this.options.intervalMs, Math.max(1, deadline - performance.now())));
      }
    }
  }
  async answers(label, expected, record) {
    const start = performance.now(), deadline = start + this.options.timeoutMs;
    record.dns = { expectedIPs: sorted(expected), startedAt: new Date().toISOString(), rounds: [], requiredConsecutiveRounds: 2 };
    let consecutive = 0;
    do {
      if (!this.cleaning && this.interrupted) this.guard();
      const samples = await Promise.all(DNS.flatMap(site => ['udp', 'tcp'].map(protocol => queryDNS(site, protocol, HOST, Math.max(1, deadline - performance.now())))));
      const matches = samples.every(sample => sample.valid && sameIPs(sample.actualIPs, expected));
      record.dns.rounds.push({ observedAt: new Date().toISOString(), matches, samples });
      consecutive = matches ? consecutive + 1 : 0;
      this.checkpoint();
      if (consecutive === 2) {
        record.dns.finishedAt = new Date().toISOString();
        record.dns.durationMs = Math.round(performance.now() - start);
        record.dns.converged = true;
        return;
      }
      if (samples.some(sample => sample.error?.code === 'ENOENT')) throw new ProbeError('dig_missing', 'dig executable is required');
      if (performance.now() >= deadline) break;
      await pause(Math.min(this.options.intervalMs, Math.max(1, deadline - performance.now())));
    } while (performance.now() < deadline);
    record.dns.finishedAt = new Date().toISOString();
    record.dns.durationMs = Math.round(performance.now() - start);
    record.dns.converged = false;
    throw new ProbeError('dns_timeout', `${label}: real UDP/TCP A answers at both authorities did not converge to ${sorted(expected).join(', ')} within ${this.options.timeoutMs} ms`);
  }
  async localtargets(label) {
    if (this.options.localtargets) this.report.localtargets.push({ label, mandatory: false, naming: 'modern localtargets.<hostname>; legacy localtargets- is not required', samples: await Promise.all(DNS.map(site => queryDNS(site, 'udp', `localtargets.${HOST}`))) });
  }
  validateChanges(plan, operation) {
    check(Array.isArray(plan.changes) && plan.changes.length > 0 && new Set(plan.changes.map(change => change.path)).size === plan.changes.length, 'Plan must contain a nonempty, unambiguous diff');
    for (const change of plan.changes) check(Object.hasOwn(change, 'before') && Object.hasOwn(change, 'after'), 'Incomplete before/after diff');
    if (operation === 'includeSite' || operation === 'resetIPs') {
      const owned = this.owned[operation === 'includeSite' ? 'exclusion' : 'override'];
      check(owned, `${operation}: refusing to undo an operation not owned by this run`);
      const inverse = owned.plan.changes.map(change => ({ path: change.path, before: change.after, after: change.before }));
      const order = changes => [...changes].sort((a, b) => a.path.localeCompare(b.path));
      check(equal(order(plan.changes), order(inverse)), `${operation}: fresh reverse plan does not exactly restore this run's original annotation values`, 'cleanup_conflict');
    } else if (operation === 'excludeSite') {
      check(equal(plan.changes, [{ path: FIELDS.excluded, before: null, after: 'true' }]), 'Exclude must only add an absent site-excluded marker; preexisting annotations are not adopted');
    } else {
      check(plan.changes.every(change => [FIELDS.ips, FIELDS.hostnames, FIELDS.snapshot].includes(change.path)), 'Override plan changes fields outside the operational annotation allowlist');
      const ips = plan.changes.find(change => change.path === FIELDS.ips);
      const snapshot = plan.changes.find(change => change.path === FIELDS.snapshot);
      check(ips && ips.after === OVERRIDE && (ips.before === null || ips.before === this.options.baseline.east), 'Override plan contains nonbaseline original IP annotation');
      check(snapshot?.before === null && typeof snapshot.after === 'string', 'Override must create its own previously absent managed snapshot');
      let value;
      try { value = JSON.parse(snapshot.after); } catch { throw new ProbeError('invalid_plan', 'Invalid managed snapshot in override plan'); }
      check(equal(value, { version: 1, ips: ips.before, hostnames: null, applied: OVERRIDE }), 'Managed snapshot does not preserve the exact original baseline');
      check(!plan.changes.some(change => change.path === FIELDS.hostnames), 'Preexisting exposed-hostnames must not be adopted');
    }
  }
  validatePlan(batch, request, model) {
    check(batch.apiVersion === API && batch.canApply === true && Array.isArray(batch.targets) && batch.targets.length === 1 && !hasUnknown(batch), 'Signed batch is incomplete, cannot apply, or has an unknown outcome', 'invalid_plan');
    const row = batch.targets[0], plan = row.plan;
    check(row.clusterId === 'east' && !row.error && plan?.apiVersion === API && plan.clusterId === 'east', 'Plan target identity mismatch', 'invalid_plan');
    const { actor, ...echo } = plan.request || {};
    check(actor === this.subject && equal(echo, request), 'Signed request differs from the exact pinned request or authenticated session actor', 'invalid_plan');
    check(typeof plan.token === 'string' && plan.token.length > 0 && /^[a-f0-9]{64}$/.test(plan.digest), 'Plan is missing its signed receipt/digest', 'invalid_plan');
    const expiry = Date.parse(plan.expiresAt), planned = Date.parse(plan.plannedAt);
    check(Number.isFinite(planned) && planned <= Date.now() + 30000 && Date.now() - planned < 120000 && expiry > Date.now() + 2000 && expiry - planned <= 301000, 'Plan is stale, expired or has an invalid lifetime', 'invalid_plan');
    check(plan.noop === false, 'Expected a real transition; refusing an already-changed/noop plan', 'invalid_plan');
    this.validateChanges(plan, request.operation);
    if (request.provider === 'traefik') {
      check(Array.isArray(plan.dependencies), 'Site plan dependencies are missing', 'invalid_plan');
      for (const ref of [model.sites.east.instance.resource, model.sites.east.instance.target]) {
        check(plan.dependencies.some(dep => sameRef(dep, ref) && typeof dep.resourceVersion === 'string' && dep.resourceVersion), 'Site plan does not pin the owned global Gslb/Ingress pair', 'invalid_plan');
      }
    }
    return plan;
  }
  async audit(record) {
    try {
      const audit = await this.api(`/api/v1/audit?namespace=${NS}`);
      check(audit.apiVersion === API && Array.isArray(audit.events), 'Invalid audit response');
      const events = audit.events.filter(event => event.reason === record.reason);
      record.audit = { available: true, warnings: audit.warnings, events, confirmed: events.some(event => event.clusterId === 'east' && event.actor === this.subject && event.provider === record.provider && event.operation === record.operation && sameRef(event.resource, record.request.resource) && event.digest === record.plan.digest && ['applied', 'noop'].includes(event.status) && !event.outcomeUnknown) };
      if (hasUnknown(events)) throw new ProbeError('audit_unknown', 'Audit reports an unknown outcome for this run', true);
      if (!record.audit.confirmed) this.report.warnings.push(`${record.operation}: recent audit did not confirm this exact digest; inspect recorded events/warnings (history may be replica-local).`);
    } catch (error) {
      if (error.outcomeUnknown) throw error;
      record.audit = { available: false, confirmed: false, error: errorJSON(error) };
      this.report.warnings.push(`${record.operation}: optional recent audit unavailable: ${error.message}`);
    }
  }
  async mutation(operation, beforeState, afterState, expectedIPs, cleanup = false) {
    this.guard();
    const provider = ['excludeSite', 'includeSite'].includes(operation) ? 'traefik' : 'traffic';
    const record = { operation, provider, cleanup, startedAt: new Date().toISOString(), status: 'RUNNING', reason: `dns-lab:${this.report.runId}:${cleanup ? 'cleanup:' : ''}${operation}:${randomUUID()}` };
    (cleanup ? this.report.cleanup.operations : this.report.operations).push(record);
    const start = performance.now();
    try {
      const expectedBefore = beforeState.excluded ? [this.options.baseline.west] : beforeState.override ? [OVERRIDE, this.options.baseline.west] : Object.values(this.options.baseline);
      const beforeSamples = await Promise.all(DNS.flatMap(site => ['udp', 'tcp'].map(protocol => queryDNS(site, protocol))));
      record.beforeDNS = { observedAt: new Date().toISOString(), expectedIPs: sorted(expectedBefore), matches: beforeSamples.every(sample => sample.valid && sameIPs(sample.actualIPs, expectedBefore)), samples: beforeSamples };
      const model = await this.model(`${cleanup ? 'cleanup:' : ''}before:${operation}`);
      record.before = model;
      this.checkState(model, beforeState, cleanup);
      const resource = provider === 'traefik' ? model.sites.east.source.resource : model.sites.east.instance.resource;
      const request = { provider, operation, resource, reason: record.reason };
      if (operation === 'overrideIPs') request.traffic = { ips: [OVERRIDE] };
      record.request = request;
      const batch = await this.api('/api/v1/mutations/plan', { targets: [{ clusterId: 'east', request }] });
      const plan = this.validatePlan(batch, request, model);
      // Keep the opaque receipt intact for apply, but do not publish its bearer token.
      const { token, ...publicPlan } = plan;
      record.plan = { ...publicPlan, signedReceiptPresent: Boolean(token), canApply: batch.canApply };
      this.checkpoint();
      const reviewStart = performance.now();
      record.reviewDelay = { requestedMs: this.options.planDelayMs, startedAt: new Date().toISOString() };
      // Preserve the exact receipt/pins: drift must fail, not trigger a replan/retry.
      if (this.options.planDelayMs > 0) await pause(this.options.planDelayMs);
      record.reviewDelay.finishedAt = new Date().toISOString();
      record.reviewDelay.durationMs = Math.round(performance.now() - reviewStart);
      this.guard();
      check(Date.parse(plan.expiresAt) > Date.now(), 'Signed plan expired during review; no apply or automatic replan was attempted', 'plan_expired');
      record.applyStartedAt = new Date().toISOString();
      record.plannedToApplyMs = Date.parse(record.applyStartedAt) - Date.parse(plan.plannedAt);
      let result;
      try {
        result = await this.api('/api/v1/mutations/apply', { targets: batch.targets, allowPartial: false });
        record.apply = result;
        if (hasUnknown(result)) throw new ProbeError('outcome_unknown', 'Apply reports outcomeUnknown; do not retry or auto-clean up', true);
        check(result.apiVersion === API && Array.isArray(result.targets) && result.targets.length === 1 && result.targets[0]?.clusterId === 'east', 'Apply returned an incomplete/mismatched batch', 'invalid_apply_response');
        const row = result.targets[0];
        if (row.error) {
          check(!row.result && row.error.outcomeUnknown === false, 'Apply error has no trustworthy outcome classification', 'invalid_apply_response');
          throw new ProbeError('apply_rejected', `${operation}: ${row.error.code}: ${row.error.error}`);
        }
        const applied = row.result;
        check(applied?.apiVersion === API && applied.clusterId === 'east' && applied.digest === plan.digest && sameRef(applied.resource, request.resource) && typeof applied.resource.resourceVersion === 'string' && applied.resource.resourceVersion && ['applied', 'noop'].includes(applied.status), 'Apply status/identity is not applied/noop; write outcome is unsafe to infer', 'invalid_apply_response');
        check(applied.status !== 'applied' || applied.resource.resourceVersion !== resource.resourceVersion, 'Applied result did not advance resourceVersion', 'invalid_apply_response');
      } catch (error) {
        if (!(error instanceof ProbeError)) error = new ProbeError('invalid_apply_response', error.message, true);
        if (error.code === 'invalid_apply_response') error.outcomeUnknown = true;
        if (error.outcomeUnknown) {
          this.unknown = true;
          this.report.manualAttention = true;
          this.report.unknownApply = { operation, reason: record.reason, resource, error: errorJSON(error) };
        }
        throw error;
      }
      const key = provider === 'traefik' ? 'exclusion' : 'override';
      if (['excludeSite', 'overrideIPs'].includes(operation)) {
        // A noop does not establish ownership of an already-installed configuration.
        check(result.targets[0].result.status === 'applied', 'Forward noop cannot establish ownership for automatic restoration', 'ownership_not_established');
        this.owned[key] = { plan: publicPlan, reason: record.reason, result: result.targets[0].result };
      } else this.owned[key] = null;
      record.applyFinishedAt = new Date().toISOString();
      this.report.outstandingOwnedChanges = this.owned;
      this.checkpoint();
      await this.audit(record);
      record.after = await this.resources(`${cleanup ? 'cleanup:' : ''}after:${operation}`, afterState, record);
      await this.answers(`${cleanup ? 'cleanup:' : ''}after:${operation}`, expectedIPs, record);
      record.status = 'PASS';
    } catch (error) {
      if (error.outcomeUnknown) {
        this.unknown = true;
        this.report.manualAttention = true;
      }
      record.status = 'FAIL';
      record.error = errorJSON(error);
      throw error;
    } finally {
      record.finishedAt = new Date().toISOString();
      record.durationMs = Math.round(performance.now() - start);
      this.checkpoint();
    }
  }
  async cleanup() {
    this.cleaning = true;
    const cleanup = this.report.cleanup;
    cleanup.startedAt = new Date().toISOString();
    try {
      if (this.unknown) {
        cleanup.status = 'SKIPPED_UNKNOWN_OUTCOME';
        cleanup.error = { message: 'No automatic retry or cleanup after an unknown apply. Inspect fresh Source/Gslb, DNS and audit manually.' };
        this.report.manualAttention = true;
        return;
      }
      if (!this.owned.override && !this.owned.exclusion) {
        cleanup.status = 'NO_OWNED_CHANGES';
        return;
      }
      // Each reverse write uses fresh pins and must exactly invert our recorded signed diff.
      if (this.owned.override) await this.mutation('resetIPs', { excluded: !!this.owned.exclusion, override: true }, { excluded: !!this.owned.exclusion, override: false }, this.owned.exclusion ? [this.options.baseline.west] : Object.values(this.options.baseline), true);
      if (this.owned.exclusion) await this.mutation('includeSite', { excluded: true, override: false }, { excluded: false, override: false }, Object.values(this.options.baseline), true);
      cleanup.status = 'RESTORED_OWN_CHANGES';
    } catch (error) {
      cleanup.status = 'FAIL';
      cleanup.error = errorJSON(error);
      this.report.manualAttention = true;
    } finally {
      cleanup.finishedAt = new Date().toISOString();
      this.report.outstandingOwnedChanges = this.owned;
      this.checkpoint();
      this.cleaning = false;
    }
  }
  async run() {
    await this.identity();
    const initial = await this.model('baseline');
    this.report.initial = initial;
    this.checkState(initial, { excluded: false, override: false });
    this.initial = initial;
    this.report.baseline = { expectedIPs: this.options.baseline, acceptedInitialAnnotationIPs: Object.fromEntries(IDS.map(id => [id, initial.sites[id].instance.traffic.ips])), note: 'Empty annotations or exact published node IPs only; no managed snapshots or hostname overrides are adopted.' };
    this.report.initialDNS = {};
    await this.answers('baseline', Object.values(this.options.baseline), this.report.initialDNS);
    await this.localtargets('baseline');
    if (!this.options.runMutations) {
      this.report.status = 'READ_ONLY';
      this.report.baselineVerified = true;
      this.report.mutationsNotRun = 'Pass --run-mutations explicitly to execute operational acceptance.';
      return;
    }
    await this.mutation('excludeSite', { excluded: false, override: false }, { excluded: true, override: false }, [this.options.baseline.west]);
    await this.mutation('includeSite', { excluded: true, override: false }, { excluded: false, override: false }, Object.values(this.options.baseline));
    await this.mutation('overrideIPs', { excluded: false, override: false }, { excluded: false, override: true }, [OVERRIDE, this.options.baseline.west]);
    await this.mutation('resetIPs', { excluded: false, override: true }, { excluded: false, override: false }, Object.values(this.options.baseline));
    this.report.final = await this.model('final');
    this.checkState(this.report.final, { excluded: false, override: false });
    await this.localtargets('final');
    check(this.report.operations.length === 4 && this.report.operations.every(operation => operation.status === 'PASS') && !this.owned.override && !this.owned.exclusion && !this.unknown, 'All four operations must pass and restore baseline');
    this.guard();
    this.report.status = 'PASS';
  }
}

async function main(argv = process.argv.slice(2), env = process.env) {
  let options;
  try { options = optionsFrom(argv, env); }
  catch (error) {
    const failure = { status: 'FAIL', mutationsRun: false, error: errorJSON(error) };
    const reportArgs = argv.filter(arg => arg.startsWith('--report='));
    if (reportArgs.length === 1 && path.isAbsolute(reportArgs[0].slice('--report='.length))) {
      failure.reportPath = reportArgs[0].slice('--report='.length);
      try { fs.writeFileSync(failure.reportPath, `${JSON.stringify(failure, null, 2)}\n`, { flag: 'wx', mode: 0o600 }); }
      catch (reportError) { failure.reportError = errorJSON(reportError); }
    }
    process.stdout.write(`${JSON.stringify(failure, null, 2)}\n`);
    return 1;
  }
  if (options.help) { process.stdout.write(HELP); return 0; }
  const report = { schemaVersion: 1, runId: randomUUID(), status: 'RUNNING', startedAt: new Date().toISOString(), mode: options.runMutations ? 'operational-acceptance' : 'read-only', scope: { consoleOrigin: options.origin, namespace: NS, hostname: HOST, clusterIds: IDS, dns: DNS, overrideIP: OVERRIDE }, settings: { dnsTimeoutMs: options.timeoutMs, pollIntervalMs: options.intervalMs, planDelayMs: options.planDelayMs }, reportPath: options.report || null, http: [], resources: [], operations: [], localtargets: [], warnings: [], cleanup: { status: 'NOT_NEEDED', operations: [] }, manualAttention: false };
  let reportFD;
  let checkpointError;
  const checkpoint = () => {
    if (reportFD === undefined || checkpointError) return;
    try {
      const bytes = Buffer.from(`${JSON.stringify(report, null, 2)}\n`);
      let offset = 0;
      while (offset < bytes.length) offset += fs.writeSync(reportFD, bytes, offset, bytes.length - offset, offset);
      fs.ftruncateSync(reportFD, bytes.length);
    } catch (error) { checkpointError = error; }
  };
  const probe = new Probe(options, report, () => {
    checkpoint();
    if (checkpointError && !probe.cleaning) throw new ProbeError('report_write_failed', checkpointError.message);
  });
  const sigint = () => { probe.interrupted = 'SIGINT'; };
  const sigterm = () => { probe.interrupted = 'SIGTERM'; };
  const start = performance.now();
  try {
    if (options.report) reportFD = fs.openSync(options.report, 'wx', 0o600);
    checkpoint();
    if (checkpointError) throw checkpointError;
    process.on('SIGINT', sigint);
    process.on('SIGTERM', sigterm);
    await probe.run();
  } catch (error) {
    report.status = 'FAIL';
    report.error = errorJSON(error);
    await probe.cleanup();
  } finally {
    process.removeListener('SIGINT', sigint);
    process.removeListener('SIGTERM', sigterm);
    report.finishedAt = new Date().toISOString();
    report.durationMs = Math.round(performance.now() - start);
    checkpoint();
    if (checkpointError) {
      report.status = 'FAIL';
      report.reportError = errorJSON(checkpointError);
    }
    if (reportFD !== undefined) fs.closeSync(reportFD);
  }
  process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  return report.status === 'FAIL' ? 1 : 0;
}

module.exports = { optionsFrom, privateIPv4, parseDig, sameIPs, sameRef, hasUnknown, Probe, main };
if (require.main === module) main().then(code => { process.exitCode = code; }).catch(error => {
  process.stderr.write(`${JSON.stringify({ status: 'FAIL', error: errorJSON(error) })}\n`);
  process.exitCode = 1;
});
