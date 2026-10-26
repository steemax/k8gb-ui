(function (root, factory) {
  'use strict';
  if (typeof module === 'object' && module.exports) module.exports = factory();
  else root.K8gbCore = factory();
})(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  const list = value => Array.isArray(value) ? value : [];
  const unique = values => [...new Set(list(values).filter(value => typeof value === 'string' && value))];
  const escape = value => String(value ?? '').replace(/[&<>"']/g, char => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[char]));
  const strategies = {roundRobin:'Round robin', weightedRoundRobin:'Weighted round robin', failover:'Failover', geoip:'GeoIP'};
  const signalLabels = {site_excluded:'Site excluded', ip_override:'IP override', ip_override_conflict:'IP override conflict', dns_target_count:'DNS target count', missing_object:'Missing GSLB', source_partial:'Partial GSLB inventory', policy_mismatch:'Policy mismatch', policy_drift:'Policy mismatch', health:'Application health', backend_health:'Backend health', collection_stale:'Stale collection', collection_health:'Collection health', native_api_collection:'Native API collection', endpoint_data:'Endpoint data', cluster_unavailable:'Cluster read unavailable', namespace_unavailable:'Namespace unavailable'};
  function signalLabel(code) {
    return signalLabels[code] || String(code || 'Unknown').replace(/[_-]+/g, ' ').replace(/^\w/, char => char.toUpperCase());
  }
  const countLabel = (count, singular, plural = `${singular}s`) => `${count} ${count === 1 ? singular : plural}`;
  function health(value) {
    switch (String(value || '').trim().toLowerCase()) {
      case 'healthy': return {tone:'consistent', label:'Healthy'};
      case 'unhealthy': return {tone:'warning', label:'Unhealthy'};
      case 'degraded': return {tone:'warning', label:'Degraded'};
      case 'notfound': return {tone:'warning', label:'Not found'};
      case 'noendpoints': return {tone:'warning', label:'No endpoints'};
      case 'notready': return {tone:'warning', label:'Not ready'};
      case 'excluded': return {tone:'info', label:'Excluded'};
      default: return {tone:'unknown', label:'Unknown'};
    }
  }
  const isExcluded = instance => instance?.traffic?.excluded === true || String(instance?.health || '').toLowerCase() === 'excluded';
  const instanceHealth = instance => health(isExcluded(instance) ? 'Excluded' : instance?.health);
  function management(instance) {
    const reported = instance?.management;
    const mode = reported || (instance?.source ? instance.source.kind === 'IngressRoute' ? 'traefik' : 'external' : 'native');
    if (mode === 'native' && !instance?.source) return {mode:'native', label:'Direct Gslb', icon:'file-pen-line'};
    if (mode === 'traefik' && instance?.source?.kind === 'IngressRoute') return {mode:'traefik', label:'Webhook / Traefik', icon:'webhook'};
    return {mode:'external', label:'External management', icon:'lock-keyhole'};
  }
  const groupManagement = group => [...new Map(list(group?.instances).map(instance => { const value = management(instance); return [value.mode, value]; })).values()];
  function inventoryStatus(site) {
    if (site?.available !== true) return {tone:'unknown', label:'Unknown'};
    if (site.objectsComplete === false) return {tone:'unknown', label:'Partial GSLB inventory'};
    if (site.objectsComplete === true) return {tone:'consistent', label:'Complete'};
    return {tone:'unknown', label:'Not reported'};
  }
  function aggregateHealth(instances) {
    const states = list(instances).map(instanceHealth);
    return states.find(state => state.tone === 'warning') || states.find(state => state.tone === 'unknown') || states[0] || health();
  }
  const warningSignal = signal => ['warning','error'].includes(String(signal?.severity).toLowerCase());
  const dnsSignal = signal => signal?.code === 'dns_target_count' && String(signal.severity).toLowerCase() === 'info';
  function hasWarnings(group) {
    return ['warning','error'].includes(String(group?.state).toLowerCase()) || list(group?.signals).some(warningSignal);
  }
  function dnsDifference(group) {
    if (list(group?.signals).some(dnsSignal)) return true;
    const observed = list(group?.instances).filter(instance => !isExcluded(instance) && instance.dnsObserved === true);
    // Site-local addresses naturally differ. Only answer cardinality is compared.
    return new Set(observed.map(instance => list(instance.dnsTargets).length)).size > 1;
  }
  function groupStatus(group) {
    if (hasWarnings(group)) return {tone:'warning', label:'Warnings'};
    const all = list(group?.instances), instances = all.filter(instance => !isExcluded(instance));
    const unknownSignals = list(group?.signals).some(signal => String(signal.severity).toLowerCase() === 'unknown');
    if (unknownSignals || ['unknown','partial','unavailable'].includes(String(group?.state).toLowerCase())) return {tone:'unknown', label:'Unknown'};
    if (all.length && !instances.length) return {tone:'info', label:'Excluded'};
    if (!instances.length || instances.some(instance => health(instance.health).tone === 'unknown' || instance.dnsObserved !== true)) return {tone:'unknown', label:'Unknown'};
    if (all.some(isExcluded)) return {tone:'info', label:'Partially excluded'};
    if (dnsDifference(group)) return {tone:'info', label:'DNS differences'};
    return {tone:'consistent', label:'Consistent'};
  }
  function searchText(group) {
    return [group?.id, group?.namespace, group?.hostname, group?.zone, ...list(group?.instances).flatMap(instance => [
      instance.clusterId, instance.hostname, instance.geoTag, instance.resource?.name, instance.resource?.namespace,
      instance.target?.name, instance.target?.namespace, instance.source?.name, management(instance).label, ...list(instance.dnsTargets),
      ...list(instance.backends).flatMap(backend => [backend.namespace, backend.service, `${backend.namespace}/${backend.service}`, backend.port])
    ])].filter(Boolean).join(' ').toLowerCase();
  }
  function filterGroups(groups, {query = '', warnings = false, dns = false} = {}) {
    const terms = query.trim().toLowerCase().replace(/https?:\/\//g, '').split(/\s+/).filter(Boolean).map(term => term.replace(/\/$/, ''));
    return list(groups).filter(group => (!warnings || hasWarnings(group)) && (!dns || dnsDifference(group)) && terms.every(term => searchText(group).includes(term)));
  }
  function endpointSummary(backends) {
    const all = list(backends), known = all.filter(backend => backend.endpointDataAvailable === true);
    return {ready:known.reduce((sum, backend) => sum + (Number(backend.readyEndpoints) || 0), 0), total:known.reduce((sum, backend) => sum + (Number(backend.totalEndpoints) || 0), 0), known:known.length, count:all.length};
  }
  function policyLabel(policy) {
    const label = strategies[policy?.strategy] || policy?.strategy || 'Unknown';
    return policy?.strategy === 'failover' ? `${label} / primary ${policy.primaryGeoTag || 'not reported'}` : label;
  }
  function ttlLabel(policy) { return Number(policy?.dnsTtlSeconds) > 0 ? `${policy.dnsTtlSeconds} s` : 'Not reported'; }
  function policySignature(policy) {
    return JSON.stringify([policy?.strategy || '', policy?.primaryGeoTag || '', Number(policy?.dnsTtlSeconds) || 0, Object.entries(policy?.weights || {}).sort(([a],[b]) => a.localeCompare(b))]);
  }
  function dnsInput(group, instance) {
    return {context:group.id, hostname:instance.hostname || group.hostname, cod:instance.clusterId,
      tags:unique(list(group.instances).map(row => row.geoTag)), mode:instance.policy?.strategy,
      primary:instance.policy?.primaryGeoTag || '', ttl:Number(instance.policy?.dnsTtlSeconds) > 0 ? instance.policy.dnsTtlSeconds : '', weights:instance.policy?.weights || {}};
  }
  function validateOverview(data) {
    if (!data || typeof data.namespace !== 'string' || !Array.isArray(data.sites) && data.sites != null || !Array.isArray(data.groups) && data.groups != null || !data.capabilities || typeof data.capabilities.readOnly !== 'boolean') throw new Error('Invalid overview response');
    const groups = list(data.groups), seen = new Set();
    for (const group of groups) {
      if (!group || typeof group.id !== 'string' || !group.id || seen.has(group.id) || !Array.isArray(group.instances) && group.instances != null) throw new Error('Invalid or duplicate group ID in overview');
      seen.add(group.id);
    }
    return {...data, sites:list(data.sites), groups};
  }
  function formatTime(value) {
    const date = new Date(value);
    return value && Number.isFinite(date.getTime()) && date.getUTCFullYear() > 1 ? date.toLocaleString('en-GB', {hour12:false, timeZoneName:'short'}) : 'Not reported';
  }
  function groupFromHash(hash) {
    return new URLSearchParams(String(hash || '').replace(/^#/, '')).get('group') || '';
  }
  const groupHash = id => `#${new URLSearchParams({group:id})}`;
  function initialNamespace(namespaces) {
    const values = unique(namespaces).sort((a,b) => a.localeCompare(b));
    return values.includes('default') ? 'default' : values[0] || null;
  }
  function createLatestRequest() {
    let sequence = 0, controller;
    return {
      async run(task, success, failure) {
        const version = ++sequence;
        controller?.abort();
        controller = new AbortController();
        try {
          const data = await task(controller.signal);
          if (version === sequence) success(data);
        } catch (error) { if (version === sequence) failure(error); }
      },
      cancel() { sequence++; controller?.abort(); }
    };
  }
  async function getJSON(path, {signal, timeout = 12000, fetcher = globalThis.fetch} = {}) {
    if (!/^\/api\/v1\/(clusters|namespaces|overview|catalog|audit|session)(\?[^#]*)?$/.test(path)) throw new Error('Unsupported read-only endpoint');
    const controller = new AbortController();
    const cancel = () => controller.abort();
    signal?.addEventListener('abort', cancel, {once:true});
    if (signal?.aborted) cancel();
    let timedOut = false;
    const timer = setTimeout(() => { timedOut = true; controller.abort(); }, timeout);
    try {
      const response = await fetcher(path, {method:'GET', signal:controller.signal, cache:'no-store', credentials:'same-origin', redirect:'error', headers:{Accept:'application/json'}});
      if (!response.ok) {
        let message = `HTTP ${response.status}`;
        try { const body = await response.json(); if (typeof body.error === 'string') message += `: ${body.error}`; } catch (_) { /* The status remains useful for non-JSON errors. */ }
        throw new Error(message);
      }
      return await response.json();
    } catch (error) {
      if (timedOut) throw new Error(`Request timed out after ${timeout / 1000} s`);
      throw error;
    } finally { clearTimeout(timer); signal?.removeEventListener('abort', cancel); }
  }
  return {list, unique, escape, strategies, signalLabel, countLabel, health, isExcluded, instanceHealth, management, groupManagement, inventoryStatus, aggregateHealth, warningSignal, dnsSignal, hasWarnings, dnsDifference, groupStatus, searchText, filterGroups, endpointSummary, policyLabel, ttlLabel, policySignature, dnsInput, validateOverview, formatTime, groupFromHash, groupHash, initialNamespace, createLatestRequest, getJSON};
});
