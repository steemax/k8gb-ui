(function (root, factory) {
  if (typeof module === 'object' && module.exports) module.exports = factory(require('./k8gb-core.js'));
  else root.K8gbMutations = factory(root.K8gbCore);
})(typeof globalThis !== 'undefined' ? globalThis : this, function (C) {
  'use strict';
  const h = C.escape, clone = value => JSON.parse(JSON.stringify(value));
  const supported = {native:['create','update','excludeSite','includeSite'], traefik:['enable','update','deactivate','excludeSite','includeSite'], traffic:['overrideIPs','resetIPs']};
  const refKeys = ['apiVersion','kind','namespace','name','uid','resourceVersion'];
  function reference(value, observed = true) {
    if (!value || refKeys.slice(0, observed ? 6 : 4).some(key => typeof value[key] !== 'string' || !value[key])) throw new Error('An exact resource reference with UID and resource version is required');
    return Object.fromEntries(refKeys.filter(key => value[key]).map(key => [key, value[key]]));
  }
  function sameResource(a, b) {
    return !!a && !!b && refKeys.slice(0,4).every(key => a[key] === b[key]) && (!a.uid || a.uid === b.uid);
  }
  function sourceReference(value) {
    const source = reference(value, false);
    // Kubernetes ownerReferences contain UID, but never resourceVersion.
    // The fresh catalog, not the owner reference, pins the write version.
    if (!source.uid) throw new Error('An owning source UID is required');
    return source;
  }
  function allows(caps, provider, operation) {
    return caps?.readOnly === false && C.list(caps.providers).includes(provider) && C.list(caps.operations?.[provider]).includes(operation) && supported[provider]?.includes(operation) === true;
  }
  function can(context, clusterId, provider, operation, catalog) {
    const overview = context?.overview, site = C.list(overview?.sites).find(item => item.cluster?.id === clusterId);
    return !context?.demo && !context?.stale && overview?.namespace === context?.namespace && site?.available === true && allows(overview?.capabilities, provider, operation) && allows(site.capabilities, provider, operation) && (!catalog || allows(catalog.capabilities, provider, operation));
  }
  function hasEdits(context, operations = supported) {
    return C.list(context?.overview?.sites).some(site => Object.entries(operations).some(([provider, values]) => values.some(operation => can(context, site.cluster?.id, provider, operation))));
  }
  function nativeInstances(context, group) {
    const seen = new Set();
    return C.list(group?.instances).filter(instance => {
      const key = JSON.stringify([instance.clusterId, instance.resource]);
      if (C.management(instance).mode !== 'native' || instance.resource?.kind !== 'Gslb' || seen.has(key) || !can(context, instance.clusterId, 'native', 'update')) return false;
      seen.add(key); return true;
    });
  }
  function observedInstance(context, instance) {
    const resource = reference(instance?.resource);
    if (resource.kind !== 'Gslb') throw new Error('Select an observed Gslb resource');
    const row = C.list(context?.overview?.groups).flatMap(group => C.list(group.instances)).find(row => row.clusterId === instance.clusterId && JSON.stringify(reference(row.resource)) === JSON.stringify(resource));
    if (!row) throw new Error('Select an observed Gslb resource');
    return row;
  }
  const resettable = instance => instance?.traffic?.overrideManaged === true && instance.traffic.overrideConflict !== true;
  const exclusionOperation = (instance, candidate) => (typeof candidate?.traffic?.excluded === 'boolean' ? candidate.traffic.excluded : C.isExcluded(instance)) ? 'includeSite' : 'excludeSite';
  function siteCandidate(context, group, instance, candidate) {
    if (C.management(instance).mode === 'native') {
      const observedGroup = C.list(context?.overview?.groups).find(row => row.id === group?.id && row.hostname === group?.hostname);
      return instance.target?.kind === 'Ingress' && candidate?.provider === 'native' && candidate.enabled === true && candidate.siteTrafficEligible === true && candidate.resource?.namespace === context.namespace && candidate.clusterId === instance.clusterId && !!candidate.catalog && sameResource(instance.target,candidate.resource) && C.list(candidate.hostnames).includes(group?.hostname) && C.list(observedGroup?.instances).some(value => value.clusterId === instance.clusterId && sameResource(value.resource,instance.resource));
    }
    try { sourceReference(instance.source); reference(candidate?.resource); } catch (_) { return false; }
    if (candidate.resource.namespace !== context.namespace) return false;
    const observedGroup = C.list(context?.overview?.groups).find(row => row.id === group?.id && row.hostname === group?.hostname);
    return C.management(instance).mode === 'traefik' && !!group?.hostname && C.list(observedGroup?.instances).some(value => value.clusterId === instance.clusterId && sameResource(instance.resource,value.resource)) &&
      instance.source.kind === 'IngressRoute' && candidate.provider === 'traefik' && candidate.clusterId === instance.clusterId && !!candidate.catalog &&
      candidate.enabled === true && candidate.siteTrafficEligible === true && C.list(candidate.hostnames).includes(group.hostname) && sameResource(instance.source,candidate.resource);
  }
  function validateIPs(input) {
    const ips = typeof input === 'string' ? input.trim().split(/[\s,]+/).filter(Boolean) : input;
    if (!Array.isArray(ips) || !ips.length || ips.length > 16) throw new Error('Enter between 1 and 16 IPv4 unicast addresses');
    for (const ip of ips) {
      if (typeof ip !== 'string' || !/^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(ip)) throw new Error('Only IPv4 unicast addresses are allowed');
      const bytes = ip.split('.').map(Number);
      if (bytes.some(byte => byte > 255) || ip === '0.0.0.0' || ip === '255.255.255.255' || bytes[0] === 127 || bytes[0] >= 224 && bytes[0] <= 239 || bytes[0] === 169 && bytes[1] === 254) throw new Error('Only IPv4 unicast addresses are allowed (no loopback or link-local)');
    }
    if (new Set(ips).size !== ips.length) throw new Error('Duplicate IP addresses are not allowed');
    return [...ips].sort();
  }
  function validatePolicy(input) {
    if (!Object.hasOwn(C.strategies, input?.strategy)) throw new Error('Select a supported strategy');
    const policy = {strategy:input.strategy};
    if (input.dnsTtlSeconds !== '' && input.dnsTtlSeconds != null) {
      const ttl = Number(input.dnsTtlSeconds);
      if (!Number.isSafeInteger(ttl) || ttl < 0 || ttl > 2147483647) throw new Error('DNS TTL must be an integer between 0 and 2147483647');
      policy.dnsTtlSeconds = ttl;
    }
    if (input.strategy === 'failover') {
      if (!String(input.primaryGeoTag || '').trim()) throw new Error('Primary GeoTag is required for failover');
      policy.primaryGeoTag = input.primaryGeoTag.trim();
    }
    if (input.strategy === 'weightedRoundRobin') {
      const entries = Object.entries(input.weights || {});
      if (!entries.length || entries.some(([tag, value]) => !tag.trim() || value === '' || !Number.isSafeInteger(Number(value)) || Number(value) < 0) || !entries.some(([,value]) => Number(value) > 0)) throw new Error('Weights must be non-negative integers with at least one positive weight');
      policy.weights = Object.fromEntries(entries.map(([tag,value]) => [tag, Number(value)]));
    }
    return policy;
  }
  function buildTargets({context, mode, group, candidate, instances = [], name, policy, traefik, traffic, reason, dnsZone}) {
    reason = String(reason || '').trim();
    if (!reason) throw new Error('A reason is required');
    if (new TextEncoder().encode(reason).length > 1024 || /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/.test(reason)) throw new Error('Reason must be at most 1024 bytes without control characters');
    let targets;
    if (mode.startsWith('traffic-') || ['traefik-excludeSite','traefik-includeSite','native-excludeSite','native-includeSite'].includes(mode)) {
      if (instances.length !== 1) throw new Error('Select exactly one observed object and site');
      const instance = observedInstance(context, instances[0]), operation = mode.slice(mode.indexOf('-') + 1);
      let provider, resource;
      if (mode.startsWith('traffic-')) {
        provider = 'traffic'; resource = reference(instance.resource);
        if (!supported.traffic.includes(operation)) throw new Error('Unsupported traffic operation');
        if (operation === 'resetIPs' && !resettable(instance)) throw new Error('Only a managed IP snapshot without external changes can be reset. Inspect the live annotations first');
      } else {
        provider = mode.startsWith('native-') ? 'native' : 'traefik';
        if (!siteCandidate(context,group,instance,candidate)) throw new Error('Select an enabled, site-traffic eligible native Ingress or source-owned global Traefik route from the fresh catalog');
        if (provider === 'traefik') sourceReference(instance.source);
        if (operation !== exclusionOperation(instance,candidate)) throw new Error('The exclusion operation does not match the current catalog site state');
        resource = reference(candidate.resource);
      }
      const request = {provider, operation, resource, reason};
      if (provider === 'native') request.target = reference(instance.resource);
      if (operation === 'overrideIPs') request.traffic = {ips:validateIPs(traffic?.ips)};
      targets = [{clusterId:instance.clusterId, request}];
    } else if (mode === 'native-update') {
      if (!instances.length) throw new Error('Select at least one observed native instance');
      const seen = new Set();
      targets = instances.map(instance => {
        if (C.management(instance).mode !== 'native' || instance.resource?.kind !== 'Gslb' || !C.list(context.overview?.groups).some(group => C.list(group.instances).some(row => row.clusterId === instance.clusterId && C.management(row).mode === 'native' && JSON.stringify(reference(row.resource)) === JSON.stringify(reference(instance.resource))))) throw new Error('Only observed native GSLB instances may be updated');
        const resource = reference(instance.resource), key = JSON.stringify([instance.clusterId, resource]);
        if (seen.has(key)) throw new Error('Duplicate mutation target');
        seen.add(key);
        return {clusterId:instance.clusterId, request:{provider:'native', operation:'update', resource, policy:validatePolicy(policy), reason}};
      });
    } else {
      const provider = mode === 'native-create' ? 'native' : 'traefik', operation = mode === 'native-create' ? 'create' : mode.replace('traefik-', '');
      if (!candidate || candidate.provider !== provider || candidate.eligible !== true && operation !== 'deactivate' || !supported[provider]?.includes(operation)) throw new Error('Select an eligible catalog resource');
      const resource = reference(candidate.resource);
      if (provider === 'native' && (!['Ingress','HTTPRoute'].includes(resource.kind) || candidate.enabled)) throw new Error('Select an unused native Ingress or HTTPRoute');
      if (provider === 'traefik' && (resource.kind !== 'IngressRoute' || (operation === 'enable') === !!candidate.enabled)) throw new Error('The Traefik operation does not match the catalog state');
      const request = {provider, operation, resource, reason};
      if (provider === 'native') {
        if (!/^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$/.test(name || '') || name.length > 253 || name.split('.').some(label => label.length > 63 || !/^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(label))) throw new Error('Enter a valid GSLB name');
        request.target = resource;
        request.resource = {apiVersion:'k8gb.io/v1beta1', kind:'Gslb', namespace:resource.namespace, name};
      }
      if (operation !== 'deactivate') request.policy = validatePolicy(policy);
      if (provider === 'traefik' && operation !== 'deactivate') request.traefik = {hostname:String(traefik?.hostname || '').trim(), entrypoint:String(traefik?.entrypoint || '').trim(), mtls:traefik?.mtls === true, local:traefik?.local === true, commonNames:C.unique(traefik?.commonNames)};
      if (request.traefik) {
        const options = request.traefik, security = commonSecurity([candidate]);
        if (options.mtls) {
          options.mtlsEntrypoint = String(traefik?.mtlsEntrypoint || '').trim();
          const ref = traefik?.tlsOption;
          options.tlsOption = {namespace:String(ref?.namespace || '').trim(),name:String(ref?.name || '').trim()};
          if (!security.entrypoints.includes(options.mtlsEntrypoint)) throw new Error('Select an approved mTLS entrypoint available on all selected clusters');
          if (!security.tlsOptions.some(row => row.eligible && row.resource.namespace === options.tlsOption.namespace && row.resource.name === options.tlsOption.name)) throw new Error('Select an existing TLSOption with RequireAndVerifyClientCert and CA Secret references');
        }
        if (options.commonNames.length && (!options.mtls || !security.sanFilterConfigured)) throw new Error('DNS SAN filtering requires mTLS and a configured certauthz plugin alias on every selected cluster');
      }
      if (operation === 'create' || operation === 'enable') {
        if (candidate.catalog?.dnsZones || dnsZone != null) {
          if (!zonesFor(candidate).includes(dnsZone)) throw new Error('Select a DNS zone supported by every selected cluster');
          const hosts = provider === 'native' ? candidateHosts(candidate) : [request.traefik.hostname];
          if (!hosts.length || hosts.some(host => !inZone(dnsZone,host))) throw new Error('Every routing hostname must be below the selected DNS zone; native target hosts are not rewritten');
          request.dnsZone = dnsZone;
        }
        if (request.traefik) request.traefik.local = false;
      }
      targets = [{clusterId:candidate.clusterId, request}];
    }
    for (const target of targets) {
      if (target.request.resource.namespace !== context.namespace || target.request.target && target.request.target.namespace !== context.namespace || !can(context, target.clusterId, target.request.provider, target.request.operation, candidate?.catalog)) throw new Error('This operation is not authorized in the current namespace and cluster');
    }
    return clone(targets);
  }
  function commonSecurity(candidates) {
    const rows = C.list(candidates), catalogs = rows.map(candidate => candidate.catalog?.traefikSecurity);
    const result = {entrypoints:[],ordinaryEntrypoints:[],tlsOptions:[],sanFilterConfigured:false,pluginAlias:'',warnings:[]};
    if (!rows.length || catalogs.some(value => !value)) { result.warnings.push('Traefik security catalog is unavailable'); return result; }
    for (const field of ['entrypoints','ordinaryEntrypoints']) result[field] = C.unique(catalogs[0][field]).filter(name => catalogs.every(catalog => C.list(catalog[field]).includes(name)));
    const options = rows.map((candidate,index) => C.list(catalogs[index].tlsOptions).filter(row => row.resource?.apiVersion === candidate.resource.apiVersion));
    for (const row of options[0]) {
      const matches = options.map(values => values.find(value => value.resource?.namespace === row.resource.namespace && value.resource?.name === row.resource.name));
      if (matches.some(value => !value)) continue;
      const eligible = matches.every(value => value.eligible === true && value.clientAuthType === 'RequireAndVerifyClientCert' && C.list(value.caSecretNames).length > 0 && value.resource?.uid && value.resource?.resourceVersion);
      const sameCA = matches.every(value => JSON.stringify(C.unique(value.caSecretNames).sort()) === JSON.stringify(C.unique(row.caSecretNames).sort()));
      result.tlsOptions.push({...row,eligible:eligible && sameCA,reason:!sameCA ? 'CA Secret references differ between clusters' : matches.find(value => !value.eligible)?.reason || row.reason});
    }
    result.pluginAlias = catalogs[0].pluginAlias || '';
    result.sanFilterConfigured = !!result.pluginAlias && catalogs.every(catalog => catalog.sanFilterConfigured === true && catalog.pluginAlias === result.pluginAlias);
    result.warnings = C.unique(catalogs.flatMap(catalog => C.list(catalog.warnings)));
    return result;
  }
  function requestSignature(request) {
    const ref = value => value ? refKeys.map(key => value[key] || '') : null;
    const policy = request.policy ? [request.policy.strategy, request.policy.primaryGeoTag || '', request.policy.dnsTtlSeconds || 0, Object.entries(request.policy.weights || {}).sort(([a],[b]) => a.localeCompare(b))] : null;
    const traefik = request.traefik ? [request.traefik.hostname || '', request.traefik.entrypoint || '', !!request.traefik.mtls, !!request.traefik.local, C.list(request.traefik.commonNames),request.traefik.mtlsEntrypoint || '',request.traefik.tlsOption?.namespace || '',request.traefik.tlsOption?.name || ''] : null;
    const traffic = request.traffic ? [C.list(request.traffic.ips)] : null;
    return JSON.stringify([request.provider, request.operation, ref(request.resource), ref(request.target), policy, traefik, traffic, request.reason, request.dnsZone || '']);
  }
  const candidateHosts = candidate => C.unique(C.list(candidate?.hostnames).map(host => String(host).toLowerCase().replace(/\.$/,''))).sort();
  const validDNSName = value => typeof value === 'string' && value.length <= 253 && value.includes('.') && value.split('.').every(label => label.length <= 63 && /^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$/.test(label));
  const inZone = (zone,host) => validDNSName(zone) && validDNSName(host) && host.endsWith(`.${zone}`);
  function zonesFor(candidate) {
    const catalog = candidate?.catalog;
    if (catalog?.dnsZones?.available !== true) return [];
    const allowed = C.list(catalog.traefikDomains);
    return C.unique(C.list(catalog.dnsZones.zones).filter(zone => validDNSName(zone) && (candidate.provider !== 'traefik' || !allowed.length || allowed.some(domain => zone === domain || inZone(domain,zone))))).sort();
  }
  function commonZones(candidates) {
    return candidates.length ? zonesFor(candidates[0]).filter(zone => candidates.every(candidate => zonesFor(candidate).includes(zone))) : [];
  }
  function matchingCandidates(candidates, base) {
    return C.list(candidates).map((candidate,index) => ({candidate,index})).filter(({candidate}) => candidate.provider === base?.provider && candidate.resource.kind === base.resource.kind && candidate.resource.namespace === base.resource.namespace && candidate.resource.name === base.resource.name && JSON.stringify(candidateHosts(candidate)) === JSON.stringify(candidateHosts(base)));
  }
  function buildCatalogBatch(input, candidates) {
    if (!Array.isArray(candidates) || !candidates.length || candidates.length > 32) throw new Error('Select between 1 and 32 target clusters');
    if (input.mode.startsWith('traefik-') && input.traefik?.mtls) {
      const security=commonSecurity(candidates),ref=input.traefik.tlsOption;
      if (!security.entrypoints.includes(input.traefik.mtlsEntrypoint) || !security.tlsOptions.some(row=>row.eligible && row.resource.namespace===ref?.namespace && row.resource.name===ref?.name)) throw new Error('Selected mTLS configuration must be available and compatible on all selected clusters');
      if (C.list(input.traefik.commonNames).length && !security.sanFilterConfigured) throw new Error('The same certauthz plugin alias must be configured on all selected clusters');
    }
    const seen = new Set();
    const hosts = JSON.stringify(candidateHosts(candidates[0]));
    return candidates.flatMap(candidate => {
      if (seen.has(candidate.clusterId)) throw new Error('Select only one resource in each cluster');
      seen.add(candidate.clusterId);
      if (input.mode === 'native-create' && (!candidateHosts(candidate).length || JSON.stringify(candidateHosts(candidate)) !== hosts)) throw new Error('Native targets must declare the same DNS hostnames in all selected clusters');
      return buildTargets({...input,candidate});
    });
  }
  function draftDNSInput({group, sites, candidates, policy, hostname}) {
    const rows = C.list(group?.instances);
    const selectedSites = C.list(sites).filter(site => !candidates || candidates.some(candidate => candidate.clusterId === site.cluster?.id));
    return {context:`draft:${group?.id || 'create'}`,hostname:hostname || group?.hostname || candidateHosts(C.list(candidates)[0]).join(', '),cod:selectedSites.map(site => site.cluster.id).join(', '),
      tags:rows.length ? C.unique(rows.map(row => row.geoTag)) : C.unique(selectedSites.flatMap(site => C.list(site.cluster?.geoTags))),
      mode:policy.strategy,primary:policy.primaryGeoTag || '',ttl:policy.dnsTtlSeconds,weights:policy.weights || {},draft:true};
  }
  function refreshGslbTargets(targets, context, latest) {
    if (latest.namespace !== context.namespace) throw new Error('Fresh snapshot has a different namespace');
    const freshContext = {...context,overview:latest};
    const rows = overview => C.list(overview?.groups).flatMap(group => C.list(group.instances));
    return targets.map(target => {
      if (target.request.resource.kind !== 'Gslb' || target.request.operation === 'create') return target;
      const previous = rows(context.overview).find(row => row.clusterId === target.clusterId && sameResource(row.resource,target.request.resource));
      const current = rows(latest).find(row => row.clusterId === target.clusterId && sameResource(row.resource,target.request.resource));
      if (!previous || !current || !can(freshContext,target.clusterId,target.request.provider,target.request.operation)) throw new Error('Resource identity or authorization changed; refresh and reopen the form');
      if (C.management(previous).mode !== C.management(current).mode || C.policySignature(previous.policy) !== C.policySignature(current.policy) || JSON.stringify(previous.traffic) !== JSON.stringify(current.traffic) || !!previous.source !== !!current.source || previous.source && !sameResource(previous.source,current.source) || !!previous.target !== !!current.target || previous.target && !sameResource(previous.target,current.target)) throw new Error('Resource configuration changed; refresh and reopen the form');
      return {...target,request:{...target.request,resource:reference(current.resource)}};
    });
  }
  function diffRows(changes) {
    const value = input => input === undefined ? '(not present)' : JSON.stringify(input, null, 2);
    return C.list(changes).map(change => ({path:String(change.path || '/'), before:value(change.before), after:value(change.after)}));
  }
  const auditLabels = {
    provider:{native:'Direct Gslb',traefik:'Webhook / Traefik',traffic:'DNS overrides'},
    operation:{create:'Create GSLB',enable:'Create from IngressRoute',update:'Update settings',deactivate:'Disable GSLB',excludeSite:'Exclude site',includeSite:'Include site',overrideIPs:'Override IPs',resetIPs:'Reset IPs'},
    status:{planned:'Planned',applied:'Applied',noop:'No changes',plan_started:'Planning',apply_started:'Applying',plan_failed:'Plan failed',apply_failed:'Apply failed'}
  };
  const auditDefaults = () => ({search:'',clusterId:'',actor:'',provider:'',operation:'',status:'',period:'all',from:'',to:'',sort:'time',direction:'desc',page:1,pageSize:50});
  const auditCollator = new Intl.Collator(undefined,{numeric:true,sensitivity:'base'});
  function queryAudit(events, namespace, filters = {}, now = Date.now()) {
    const input = {...auditDefaults(),...filters}, all = C.list(events).filter(event => event?.resource?.namespace === namespace);
    let from = -Infinity, to = Infinity, error = '';
    const periods = {'1h':3600000,'24h':86400000,'7d':604800000};
    if (Object.hasOwn(periods,input.period)) { from = now - periods[input.period]; to = now; }
    if (input.period === 'custom') {
      if (input.from) from = Date.parse(input.from);
      if (input.to) to = Date.parse(input.to);
      if (!input.from && !input.to || Number.isNaN(from) || Number.isNaN(to) || from > to) error = 'Choose a valid time range: From must not be later than To.';
    }
    const terms = String(input.search).trim().toLowerCase().split(/\s+/).filter(Boolean);
    const rows = error ? [] : all.filter(event => {
      if (['clusterId','actor','provider','operation'].some(key => input[key] && event[key] !== input[key])) return false;
      if (input.status === '@outcomeUnknown' ? !event.outcomeUnknown : input.status && event.status !== input.status) return false;
      if (from !== -Infinity || to !== Infinity) { const time = Date.parse(event.time); if (!Number.isFinite(time) || time < from || time > to) return false; }
      const text = [event.id,event.time,event.clusterId,input.clusterNames?.[event.clusterId],event.actor,event.provider,event.operation,event.resource.kind,event.resource.namespace,event.resource.name,event.reason,event.status,auditLabels.provider[event.provider],auditLabels.operation[event.operation],auditLabels.status[event.status],event.outcomeUnknown ? 'Outcome unknown' : '',JSON.stringify(event.changes || [])].join(' ').toLowerCase();
      return terms.every(term => text.includes(term));
    });
    const field = ['time','clusterId','resource','actor','operation','status'].includes(input.sort) ? input.sort : 'time';
    const value = event => field === 'time' ? (Number.isFinite(Date.parse(event.time)) ? Date.parse(event.time) : null) : field === 'resource' ? `${event.resource.name || ''} ${event.resource.kind || ''}` : field === 'clusterId' ? input.clusterNames?.[event.clusterId] || event.clusterId : field === 'status' ? (event.outcomeUnknown ? 'Outcome unknown' : auditLabels.status[event.status] || event.status) : field === 'operation' ? auditLabels.operation[event.operation] || event.operation : event[field];
    rows.sort((a,b) => {
      const x = value(a), y = value(b), missing = v => v == null || v === '';
      if (missing(x) !== missing(y)) return missing(x) ? 1 : -1;
      const result = field === 'time' ? (x || 0) - (y || 0) : auditCollator.compare(String(x || ''),String(y || ''));
      if (result) return result * (input.direction === 'asc' ? 1 : -1);
      const timeA = Date.parse(a.time), timeB = Date.parse(b.time);
      if (Number.isFinite(timeA) && Number.isFinite(timeB) && timeA !== timeB) return timeB - timeA;
      const identity = event => JSON.stringify([event.clusterId,event.id,event.resource.name,event.actor,event.provider,event.operation,event.status,event.reason]);
      return auditCollator.compare(identity(a),identity(b));
    });
    const pageSize = [25,50,100].includes(Number(input.pageSize)) ? Number(input.pageSize) : 50;
    const pages = Math.max(1,Math.ceil(rows.length / pageSize)), page = Math.min(pages,Math.max(1,Math.floor(Number(input.page) || 1)));
    return {events:rows.slice((page - 1) * pageSize,page * pageSize),total:all.length,matched:rows.length,page,pages,pageSize,error};
  }
  async function postJSON(path, payload, {fetcher = globalThis.fetch, timeout = 45000} = {}) {
    if (!['/api/v1/mutations/plan','/api/v1/mutations/apply'].includes(path)) throw new Error('Unsupported mutation endpoint');
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), timeout);
    try {
      const response = await fetcher(path, {method:'POST', credentials:'same-origin', redirect:'error', cache:'no-store', signal:controller.signal, headers:{Accept:'application/json','Content-Type':'application/json','X-K8gb-Intent':'mutation'}, body:JSON.stringify(payload)});
      const body = await response.json();
      if (!response.ok) throw new Error(`HTTP ${response.status}: ${body.error || 'Mutation request failed'}`);
      return body;
    } catch (error) {
      if (path.endsWith('/apply')) { const unknown = new Error(`Outcome unknown. ${error.message || 'Connection lost'}. Check audit and live resource state before making another plan. Do not retry this apply.`); unknown.outcomeUnknown = true; throw unknown; }
      throw error;
    } finally { clearTimeout(timer); }
  }
  function createPlanner({post = postJSON, now = Date.now} = {}) {
    let generation = 0, state = {status:'idle', batch:null, expiresAt:0, message:'', result:null};
    return {
      get state() { return state; },
      invalidate() { if (['applying','unknown','finished'].includes(state.status)) return; generation++; state = {...state, status:'idle', batch:null, expiresAt:0, message:'', result:null}; },
      async plan(targets) {
        if (['applying','unknown','finished'].includes(state.status)) throw new Error('Start a new form after reviewing the result');
        const version = ++generation;
        state = {status:'planning', batch:null, expiresAt:0, message:'', result:null};
        try {
          const batch = await post('/api/v1/mutations/plan', {targets});
          if (version !== generation) return;
          if (!Array.isArray(batch?.targets) || batch.targets.length !== targets.length || batch.targets.some((row,index) => row.clusterId !== targets[index].clusterId || !row.error && (!row.plan || requestSignature(row.plan.request) !== requestSignature(targets[index].request)))) throw new Error('Plan response does not match the requested targets');
          let expiresAt = now() + 300000;
          for (const row of batch.targets) {
            if (row.error) continue;
            const plan = row.plan, expiry = Date.parse(plan?.expiresAt);
            if (!plan || plan.clusterId !== row.clusterId || !plan.token || !plan.digest || !Number.isFinite(expiry)) throw new Error('Invalid signed plan response');
            expiresAt = Math.min(expiresAt, expiry);
          }
          state = {status:'planned', batch:clone(batch), expiresAt, message:'', result:null};
        } catch (error) { if (version === generation) state = {...state, status:'idle', message:error.message}; }
      },
      canApply(consent, allowPartial = false, disableConsent = false, localConsent = false) {
        const batch = state.batch, rows = C.list(batch?.targets), errors = rows.some(row => row.error);
        return state.status === 'planned' && now() < state.expiresAt && consent === true && rows.some(row => row.plan && !row.plan.noop) && (!errors && batch.canApply === true || errors && allowPartial === true) && rows.every(row => row.error || (!['deactivate','excludeSite'].includes(row.plan?.request?.operation) || disableConsent === true) && (row.plan?.request?.provider !== 'native' || row.plan.request.operation !== 'excludeSite' || localConsent === true));
      },
      async apply(consent, allowPartial = false, disableConsent = false, localConsent = false) {
        if (!this.canApply(consent, allowPartial, disableConsent, localConsent)) throw new Error('A current plan and explicit consent are required');
        const targets = clone(state.batch.targets);
        state.status = 'applying';
        try {
          const result = await post('/api/v1/mutations/apply', {targets, allowPartial});
          if (!Array.isArray(result?.targets) || result.targets.length !== targets.length || result.targets.some((row,index) => row.clusterId !== targets[index].clusterId || !row.result && !row.error || row.result?.resource && !sameResource(targets[index].plan?.request.resource,row.result.resource))) throw new Error('Incomplete apply response');
          state = {...state, status:result.targets.some(row => row.error?.outcomeUnknown) ? 'unknown' : 'finished', result:clone(result), batch:null};
        } catch (error) { state = {...state, status:'unknown', batch:null, message:error.outcomeUnknown ? error.message : `Outcome unknown. ${error.message}. Check audit and live resource state; do not retry this apply.`}; }
      }
    };
  }
  function createView({getContext, icon, onApplied = () => {}, onTrafficReady = () => {}, onViewChange = () => {}}) {
    let workspace, audit, planner, editor, catalogRequest = C.createLatestRequest(), auditRequest = C.createLatestRequest(), trafficRequest = C.createLatestRequest(), trafficCatalog, timer, opener;
    let auditData, auditNamespace, auditFilters = auditDefaults();
    const $ = id => document.getElementById(id);
    const diagram = typeof window === 'object' && window.K8gbDNS ? window.K8gbDNS.createView() : null;
    const editing = () => !!workspace && !workspace.hidden;
    const auditing = () => !!audit && !audit.hidden;
    const applying = () => editing() && planner?.state.status === 'applying';
    function close() {
      if (applying()) return false;
      clearInterval(timer); catalogRequest.cancel(); auditRequest.cancel();
      diagram?.unmount();
      if (workspace) workspace.hidden = true;
      if (audit) audit.hidden = true;
      onViewChange(null);
      if (opener?.isConnected && !opener.closest('[hidden]')) opener.focus();
      else ($('detailHeading') || $('catalogHeading'))?.focus();
      return true;
    }
    const button = (action, label, symbol, classes = '') => `<button type="button" class="k8gb-button ${classes}" data-mutation-action="${action}">${icon(symbol)}${h(label)}</button>`;
    const clusterName = id => C.list(getContext().overview?.sites).find(site => site.cluster?.id === id)?.cluster?.name || id;
    const refLabel = (id, ref) => `${clusterName(id)} [${id}] / ${ref.kind} ${ref.namespace}/${ref.name} / ${ref.apiVersion}`;
    function changesHTML(changes) {
      const rows = diffRows(changes);
      return rows.length ? `<table class="k8gb-table k8gb-diff"><thead><tr><th>Field</th><th>Before</th><th>After</th></tr></thead><tbody>${rows.map(row => `<tr><td data-label="Field"><code>${h(row.path)}</code></td><td data-label="Before"><pre>${h(row.before)}</pre></td><td data-label="After"><pre>${h(row.after)}</pre></td></tr>`).join('')}</tbody></table>` : '<p class="k8gb-muted">No changes.</p>';
    }
    function mount() {
      if (workspace) return;
      workspace = document.createElement('section'); workspace.className = 'k8gb-editor'; workspace.id = 'mutationWorkspace'; workspace.hidden = true; workspace.setAttribute('aria-labelledby','mutationHeading'); $('main').append(workspace);
      workspace.addEventListener('click', async event => {
        const action = event.target.closest('[data-mutation-action]')?.dataset.mutationAction;
        if (action === 'close') close();
        if (action === 'preview') await preview();
        if (action === 'apply') {
          try {
            if (!authorizedPlan()) throw new Error('The planned operations are no longer authorized in this scope');
            const pending = planner.apply($('mutationConsent')?.checked, $('mutationPartial')?.checked === true, $('mutationDisableConsent')?.checked === true, $('mutationLocalConsent')?.checked === true);
            renderPreview(); await pending; renderPreview(); onApplied();
          } catch (error) { showError(error.message); }
        }
      });
      workspace.addEventListener('submit', event => event.preventDefault());
      workspace.addEventListener('input', event => {
        if (!event.target.closest('#mutationForm')) return;
        if (event.target.id === 'mutationHostLabel') updateCreationHostname();
        if (event.target.id === 'mutationMTLS' && event.target.checked) $('mutationCNDetails').open = true;
        planner.invalidate(); updateStrategy(); renderPreview();
      });
      workspace.addEventListener('change', event => {
        const id = event.target.id;
        if (event.target.closest('#mutationForm')) { planner.invalidate(); renderPreview(); }
        if (id === 'mutationCandidate') { editor.selected = Number(event.target.value); seed(); renderForm(); }
        if (id === 'mutationDNSZone') {
          editor.zone = event.target.value;
          if (editor.mode === 'native-create') {
            const index = editor.candidates.findIndex(candidate => selectableCandidate(candidate) && candidateHosts(candidate).length && candidateHosts(candidate).every(host => inZone(editor.zone,host)));
            if (index >= 0) { editor.selected = index; seed(); }
            renderForm();
          } else { updateCreationHostname(); updateDiagram(); }
        }
        if (id === 'mutationScope') { editor.scope = event.target.value; if (editor.scope !== 'all') { editor.base = Number(editor.scope); seed(); } renderForm(); }
        if (id === 'mutationBase') { editor.base = Number(event.target.value); seed(); renderForm(); }
        if (id === 'mutationOperation') { editor.operation = event.target.value; seed(); renderForm(); }
        if (event.target.dataset.targetCluster) {
          if (event.target.checked) editor.selectedClusters.add(event.target.dataset.targetCluster);
          else editor.selectedClusters.delete(event.target.dataset.targetCluster);
          renderTargetSummary(); renderZoneOptions(); renderTLSChoices(); updateDiagram();
        }
        if (event.target.dataset.targetChoice) {
          const id = event.target.dataset.targetChoice, index = Number(event.target.value);
          editor.targetIndices.set(id,index);
          if (index < 0) editor.selectedClusters.delete(id);
          renderTargetRows(); renderZoneOptions(); renderTLSChoices(); updateDiagram();
        }
        if (['mutationConsent','mutationPartial','mutationDisableConsent','mutationLocalConsent'].includes(id)) updateApply();
      });
    }
    function seed() {
      const source = editor.mode === 'native-update' ? editor.instances[editor.base] : editor.candidates[editor.selected];
      editor.policy = clone(source?.policy || {strategy:'roundRobin'}); editor.traefik = clone(source?.traefik || {});
      if (creating()) {
        const zones = zonesFor(source).filter(zone => editor.mode !== 'native-create' || candidateHosts(source).every(host => inZone(zone,host)));
        if (!zones.includes(editor.zone)) editor.zone = zones.filter(zone => candidateHosts(source).every(host => inZone(zone,host))).sort((a,b) => b.length-a.length)[0] || zones[0] || '';
      }
      if (editor.multi) {
        editor.targetIndices = new Map(); editor.selectedClusters = new Set();
        for (const site of C.list(getContext().overview?.sites)) {
          const matches = matchingCandidates(editor.candidates,source).filter(row => row.candidate.clusterId === site.cluster?.id && selectableCandidate(row.candidate) && (!creating() || zonesFor(row.candidate).includes(editor.zone)));
          if (matches.length === 1) { editor.targetIndices.set(site.cluster.id,matches[0].index); editor.selectedClusters.add(site.cluster.id); }
        }
      }
      if (creating()) {
        const zones = commonZones(selectedCandidates());
        if (!zones.includes(editor.zone)) editor.zone = zones.filter(zone => candidateHosts(source).every(host => inZone(zone,host))).sort((a,b) => b.length-a.length)[0] || zones[0] || '';
        editor.hostLabel = candidateHosts(source)[0]?.endsWith(`.${editor.zone}`) ? candidateHosts(source)[0].slice(0,-editor.zone.length-1) : source?.resource?.name || '';
      }
    }
    function creating() { return ['native-create','traefik-enable'].includes(editor?.mode); }
    function updateCreationHostname() {
      if (editor.mode !== 'traefik-enable' || !$('mutationHostname')) return;
      editor.hostLabel = $('mutationHostLabel')?.value.trim() || '';
      $('mutationHostname').value = editor.hostLabel && editor.zone ? `${editor.hostLabel}.${editor.zone}` : '';
    }
    function renderZoneOptions() {
      if (!$('mutationDNSZone')) return;
      const candidates = selectedCandidates(), zones = commonZones(candidates).filter(zone => editor.mode !== 'native-create' || candidates.every(target => editor.candidates.some(candidate => candidate.clusterId === target.clusterId && selectableCandidate(candidate) && candidateHosts(candidate).length && candidateHosts(candidate).every(host => inZone(zone,host)))));
      if (!zones.includes(editor.zone)) editor.zone = zones[0] || '';
      $('mutationDNSZone').innerHTML = '<option value="">Select DNS zone</option>' + zones.map(zone => `<option value="${h(zone)}"${zone === editor.zone ? ' selected' : ''}>${h(zone)}</option>`).join('');
      const unavailable = candidates.filter(candidate => candidate.catalog?.dnsZones?.available !== true);
      const status = $('mutationDNSZoneStatus');
      status.className = `k8gb-zone-status${!zones.length ? ' warning' : ''}`;
      status.innerHTML = `${icon(zones.length ? 'circle-check' : 'circle-alert')}<span>${h(!candidates.length ? 'No target clusters selected' : unavailable.length ? `Zone discovery unavailable: ${unavailable.map(candidate => clusterName(candidate.clusterId)).join(', ')}` : zones.length ? `Available on ${candidates.length} selected ${candidates.length === 1 ? 'cluster' : 'clusters'}` : 'No common DNS zone')}</span>`;
      if ($('mutationZoneSources')) $('mutationZoneSources').innerHTML = candidates.map(candidate => `<div><strong>${h(clusterName(candidate.clusterId))}</strong><code>${h(candidate.catalog?.dnsZones?.available ? candidate.catalog.dnsZones.source : candidate.catalog?.dnsZones?.error || 'DNS zones unavailable')}</code></div>`).join('');
      if (editor.mode === 'native-create') {
        for (const option of $('mutationCandidate').options) {
          const candidate = editor.candidates[Number(option.value)];
          option.hidden = !!editor.zone && candidateHosts(candidate).some(host => !inZone(editor.zone,host));
        }
        $('mutationDeclaredHosts').textContent = candidates.flatMap(candidate => candidateHosts(candidate)).filter((value,index,all) => all.indexOf(value) === index).join(', ');
      }
      updateCreationHostname();
    }
    function selectedCandidates() {
      return [...(editor.selectedClusters || [])].map(id => editor.candidates[editor.targetIndices.get(id)]).filter(Boolean);
    }
    function selectableCandidate(candidate) {
      const operation = editor.mode === 'native-create' ? 'create' : editor.mode === 'traefik-enable' ? 'enable' : editor.operation;
      return !!candidate && (candidate.eligible === true || operation === 'deactivate' && candidate.enabled === true) && can(getContext(),candidate.clusterId,candidate.provider,operation,candidate.catalog);
    }
    function renderTargetRows() {
      if (!$('mutationClusterTargets')) return;
      $('mutationClusterTargets').innerHTML = '<div class="k8gb-target-columns" aria-hidden="true"><span>Cluster</span><span>Routing resource</span><span>Placement</span></div>' + C.list(getContext().overview?.sites).map(site => {
        const id = site.cluster?.id, options = editor.candidates.map((candidate,index) => ({candidate,index})).filter(row => row.candidate.clusterId === id && (editor.mode !== 'native-create' || !editor.zone || candidateHosts(row.candidate).every(host => inZone(editor.zone,host))));
        const selected = editor.targetIndices.get(id), candidate = editor.candidates[selected];
        const allowed = selectableCandidate(candidate);
        const reason = candidate && !allowed ? candidate.reason || 'Operation unavailable' : !site.available ? 'Cluster read unavailable' : '';
        return `<div class="k8gb-target-row" data-target-row="${h(id)}"><label class="k8gb-check k8gb-target-cluster"><input type="checkbox" data-target-cluster="${h(id)}" aria-label="Target ${h(clusterName(id))}"${editor.selectedClusters.has(id) ? ' checked' : ''}${allowed ? '' : ' disabled'}>${icon('server')}<span><strong>${h(clusterName(id))}</strong><small>${h(C.list(site.cluster?.geoTags).join(', ') || id)}</small></span></label><div class="k8gb-target-resource"><span class="k8gb-select-control"><select aria-label="Resource on ${h(clusterName(id))}" data-target-choice="${h(id)}"${options.length ? '' : ' disabled'}><option value="-1"${selected == null || selected < 0 ? ' selected' : ''}>Select a resource</option>${options.map(({candidate,index}) => `<option value="${index}"${index === selected ? ' selected' : ''}${selectableCandidate(candidate) ? '' : ' disabled'}>${h(candidate.resource.kind)} / ${h(candidate.resource.name)}${selectableCandidate(candidate) ? '' : ` (${h(candidate.reason || 'Unavailable')})`}</option>`).join('')}</select>${icon('chevron-down')}</span><small class="${reason ? 'k8gb-target-warning' : 'k8gb-muted'}">${h(reason || (candidate ? candidateHosts(candidate).join(', ') : 'No routing resource selected'))}</small></div><span class="k8gb-target-status" data-target-status="${h(id)}"></span></div>`;
      }).join('');
      renderTargetSummary();
    }
    function renderTargetSummary() {
      if (!$('mutationTargetSummary')) return;
      const candidates = selectedCandidates();
      $('mutationTargetSummary').textContent = `${candidates.length} of ${C.list(getContext().overview?.sites).length} selected`;
      for (const row of workspace.querySelectorAll('[data-target-row]')) {
        const id = row.dataset.targetRow, selected = editor.selectedClusters.has(id), candidate = editor.candidates[editor.targetIndices.get(id)], allowed = selectableCandidate(candidate);
        row.dataset.selected = String(selected);
        const status = row.querySelector('[data-target-status]');
        status.className = `k8gb-target-status${selected ? ' selected' : !allowed ? ' unavailable' : ''}`;
        status.innerHTML = `${icon(selected ? 'circle-check' : !allowed ? 'circle-alert' : 'minus')}<span>${selected ? 'Selected' : !allowed ? 'Unavailable' : 'Not selected'}</span>`;
      }
    }
    function readPolicy() {
      return {strategy:$('mutationStrategy')?.value,dnsTtlSeconds:$('mutationTTL')?.value,primaryGeoTag:$('mutationPrimary')?.value,
        weights:Object.fromEntries([...workspace.querySelectorAll('[data-weight-tag]')].filter(input => input.value !== '').map(input => [input.dataset.weightTag,input.value]))};
    }
    function updateDiagram() {
      const host = $('k8gbMutationDNS');
      if (!host || editor.operational || editor.operation === 'deactivate') { diagram?.unmount(); if (host) host.hidden = true; return; }
      host.hidden = false;
      diagram?.update(host,draftDNSInput({group:editor.group,sites:getContext().overview?.sites,candidates:editor.multi ? selectedCandidates() : undefined,policy:readPolicy(),hostname:$('mutationHostname')?.value}));
    }
    function tags() {
      return C.unique([...C.list(getContext().overview?.sites).flatMap(site => C.list(site.cluster?.geoTags)), ...Object.keys(editor.policy.weights || {}), editor.policy.primaryGeoTag]);
    }
    function renderForm() {
      if (editor.operational) { renderTrafficForm(); return; }
      const candidate = editor.candidates?.[editor.selected], native = editor.mode.startsWith('native'), update = editor.mode === 'native-update', policy = editor.policy, options = editor.traefik;
      const eligible = candidate && candidate.eligible !== true ? `<p class="k8gb-notice">${h(candidate.reason || 'Resource is not eligible')}</p>` : '';
      const deactivate = editor.operation === 'deactivate';
      const select = html => `<span class="k8gb-select-control">${html}${icon('chevron-down')}</span>`;
      const field = (label,id,control,classes = '') => `<div class="k8gb-form-field ${classes}"><label class="k8gb-label" for="${id}">${label}</label>${control}</div>`;
      const section = (id,title,symbol,content) => `<section class="k8gb-form-section" aria-labelledby="${id}"><h3 id="${id}" class="k8gb-form-section-heading">${icon(symbol)}${title}</h3><div class="k8gb-form-section-body">${content}</div></section>`;
      const shortRef = (id,ref) => `${clusterName(id)} / ${ref.name}`;
      const scope = update ? `<div class="k8gb-form-grid">${field('Update policy on','mutationScope',select(`<select id="mutationScope"><option value="all"${editor.scope === 'all' ? ' selected' : ''}>All authorized clusters (${editor.instances.length})</option>${editor.instances.map((instance,index) => `<option value="${index}"${editor.scope === String(index) ? ' selected' : ''}>${h(shortRef(instance.clusterId,instance.resource))}</option>`).join('')}</select>`))}<div${editor.scope !== 'all' ? ' hidden' : ''}>${field('Policy source','mutationBase',select(`<select id="mutationBase">${editor.instances.map((instance,index) => `<option value="${index}"${editor.base === index ? ' selected' : ''}>${h(shortRef(instance.clusterId,instance.resource))}</option>`).join('')}</select>`))}</div></div>` : `<div class="k8gb-form-grid${native ? '' : ' single'}">${field(native ? 'Native target' : 'Traefik route','mutationCandidate',select(`<select id="mutationCandidate">${C.unique(editor.candidates.map(row => row.clusterId)).map(id => `<optgroup label="${h(clusterName(id))}">${editor.candidates.map((row,index) => row.clusterId === id ? `<option value="${index}" title="${h(refLabel(row.clusterId,row.resource))}"${editor.selected === index ? ' selected' : ''}>${h(row.resource.kind)} / ${h(row.resource.name)}${row.eligible ? '' : ' (unavailable)'}</option>` : '').join('')}</optgroup>`).join('')}</select>`))}${native ? field('GSLB name <span class="k8gb-required" aria-hidden="true">*</span>','mutationName','<input id="mutationName" required maxlength="253" autocomplete="off">') : ''}</div>${eligible}`;
      const source = update ? editor.instances[editor.base] : candidate;
      const referenceDetails = source ? `<details class="k8gb-form-technical"><summary>${icon('chevron-right')}Resource reference</summary><dl class="k8gb-reference"><dt>Cluster</dt><dd>${h(clusterName(source.clusterId))}</dd><dt>Resource</dt><dd>${h(source.resource.kind)} <code>${h(source.resource.namespace)}/${h(source.resource.name)}</code></dd><dt>API</dt><dd><code>${h(source.resource.apiVersion)}</code></dd><dt>UID</dt><dd><code>${h(source.resource.uid)}</code></dd><dt>Version</dt><dd><code>${h(source.resource.resourceVersion)}</code></dd></dl></details>` : '';
      const targets = editor.multi ? '<section class="k8gb-targets" aria-labelledby="mutationTargetsHeading"><div class="k8gb-target-heading"><h4 id="mutationTargetsHeading">Target clusters</h4><span id="mutationTargetSummary" role="status"></span></div><div id="mutationClusterTargets"></div></section>' : '';
      const operation = !native && candidate?.enabled && can(getContext(),candidate.clusterId,'traefik','deactivate',candidate.catalog) ? field('Operation','mutationOperation',select(`<select id="mutationOperation">${can(getContext(),candidate.clusterId,'traefik','update',candidate.catalog) ? `<option value="update"${!deactivate ? ' selected' : ''}>Update settings</option>` : ''}<option value="deactivate"${deactivate ? ' selected' : ''}>Disable GSLB...</option></select>`)) : '';
      const zone = field('DNS zone','mutationDNSZone',select('<select id="mutationDNSZone" required></select>'));
      const hostname = `<div class="k8gb-hostname-result">${icon('globe')}<div><span class="k8gb-label">${native ? 'Routing hostname' : 'GSLB hostname'}</span>${native ? '<span id="mutationDeclaredHosts" class="k8gb-mono"></span>' : `<input id="mutationHostname" readonly aria-label="GSLB hostname" value="${h(options.hostname || '')}">`}</div></div>`;
      const identity = creating() ? `<div class="k8gb-form-grid k8gb-dns-identity${native ? ' native' : ''}">${!native ? field('DNS name','mutationHostLabel',`<input id="mutationHostLabel" required autocomplete="off" value="${h(editor.hostLabel || '')}">`) : ''}${zone}</div>${hostname}<div class="k8gb-zone-discovery"><div id="mutationDNSZoneStatus" role="status"></div><details class="k8gb-form-technical"><summary>${icon('chevron-right')}Zone sources</summary><div id="mutationZoneSources"></div></details></div>` : !native ? field('Hostname','mutationHostname',`<input id="mutationHostname" value="${h(options.hostname || '')}">`) : '';
      const dnsPolicy = `<div class="k8gb-form-grid k8gb-policy-grid">${field('Strategy','mutationStrategy',select(`<select id="mutationStrategy">${Object.entries(C.strategies).map(([value,label]) => `<option value="${value}"${policy.strategy === value ? ' selected' : ''}>${h(label)}</option>`).join('')}${!Object.hasOwn(C.strategies,policy.strategy) ? '<option value="" selected>Select strategy</option>' : ''}</select>`))}${field('DNS TTL','mutationTTL',`<span class="k8gb-unit-control"><input id="mutationTTL" type="number" min="0" max="2147483647" step="1" value="${h(policy.dnsTtlSeconds ?? '')}"><span>sec</span></span>`)}</div><div id="mutationPrimaryField">${field('Primary GeoTag','mutationPrimary',`<input id="mutationPrimary" value="${h(policy.primaryGeoTag || '')}" list="mutationGeoTags"><datalist id="mutationGeoTags">${tags().map(tag => `<option value="${h(tag)}"></option>`).join('')}</datalist>`)}</div><fieldset id="mutationWeights"><legend>Weights by GeoTag</legend>${tags().map((tag,index) => `<label class="k8gb-weight-input">${h(tag)}<input type="number" min="0" step="1" data-weight-tag="${h(tag)}" aria-label="Weight ${h(tag)}" value="${h(policy.weights?.[tag] ?? '')}" id="mutationWeight${index}"></label>`).join('') || '<p class="k8gb-muted">No GeoTags reported.</p>'}</fieldset>`;
      const security = native ? '' : section('mutationSecurityHeading','Security','shield-check',`<div class="k8gb-security-row"><div class="k8gb-tls-setting"><label class="k8gb-switch"><input id="mutationMTLS" type="checkbox"${options.mtls ? ' checked' : ''}><span class="k8gb-switch-track"></span><strong>mTLS</strong></label><span id="mutationTLSStatus" class="k8gb-muted"></span></div>${field('Ordinary entrypoint','mutationEntrypoint',select(`<select id="mutationEntrypoint" data-initial="${h(options.entrypoint || '')}"></select>`))}</div><div id="mutationTLSFields" class="k8gb-mtls-fields"${!options.mtls ? ' hidden' : ''}><div class="k8gb-form-grid">${field('mTLS entrypoint','mutationMTLSEntrypoint',select(`<select id="mutationMTLSEntrypoint" data-initial="${h(options.mtlsEntrypoint || '')}"></select>`))}${field('TLSOption','mutationTLSOption',select(`<select id="mutationTLSOption" data-initial="${h(options.tlsOption ? `${options.tlsOption.namespace}/${options.tlsOption.name}` : '')}"></select>`))}</div><div id="mutationCAReferences" class="k8gb-ca-references" role="status"></div><p id="mutationTLSHint" class="k8gb-target-warning" hidden></p></div><details id="mutationCNDetails" class="k8gb-form-technical k8gb-cn-details"${options.mtls || C.list(options.commonNames).length ? ' open' : ''}><summary>${icon('chevron-right')}Allowed DNS SAN<span id="mutationCNCount"></span></summary>${field('DNS SAN patterns','mutationCNs',`<textarea id="mutationCNs" rows="2">${h(C.list(options.commonNames).join('\n'))}</textarea>`)}<p id="mutationSANStatus" class="k8gb-muted"></p></details><p id="mutationCNHint" class="k8gb-target-warning" hidden></p>${!creating() ? `<label class="k8gb-check"><input id="mutationLocal" type="checkbox"${options.local ? ' checked' : ''}>Local</label>` : ''}`);
      $('mutationForm').classList.add('structured');
      $('mutationForm').innerHTML = `${section('mutationRoutingHeading','Routing & placement','network',scope + targets + referenceDetails + operation)}<div id="mutationPolicyFields"${deactivate ? ' hidden' : ''}>${section('mutationDNSHeading','DNS policy','globe',identity + dnsPolicy)}${security}</div>${section('mutationChangeHeading','Change request','clipboard-pen-line',field('Reason <span class="k8gb-required" aria-hidden="true">*</span>','mutationReason','<textarea id="mutationReason" required rows="2" maxlength="1024"></textarea>') + `<div class="k8gb-form-actions">${button('preview','Preview changes','scan-eye','k8gb-primary')}</div>`)}`;
      renderTargetRows(); renderZoneOptions(); renderTLSChoices(); updateStrategy(); renderPreview();
    }
    function renderTLSChoices() {
      if (!$('mutationMTLSEntrypoint')) return;
      const candidates = editor.multi ? selectedCandidates() : [editor.candidates?.[editor.selected]].filter(Boolean), security = commonSecurity(candidates);
      const choose = (id,values,label) => {
        const node = $(id), current = node.dataset.initial ?? node.value; delete node.dataset.initial;
        node.innerHTML = `<option value="">${h(label)}</option>${values.map(value => `<option value="${h(value.value)}"${value.disabled ? ' disabled' : ''}>${h(value.label)}</option>`).join('')}${current && !values.some(value => value.value === current) ? `<option value="${h(current)}" disabled>${h(current)} (unavailable)</option>` : ''}`;
        node.value = current || ''; node.disabled = !values.length;
      };
      choose('mutationEntrypoint',security.ordinaryEntrypoints.map(value => ({value,label:value})),'Preserve original / installation default');
      choose('mutationMTLSEntrypoint',security.entrypoints.map(value => ({value,label:value})),'Select entrypoint');
      choose('mutationTLSOption',security.tlsOptions.map(row => ({value:`${row.resource.namespace}/${row.resource.name}`,label:`${row.resource.namespace}/${row.resource.name}${row.eligible ? '' : ` - ${row.reason || 'not eligible'}`}`,disabled:!row.eligible})),'Select TLSOption');
      editor.security = security; updateTLSDetails();
    }
    function updateTLSDetails() {
      if (!$('mutationMTLS')) return;
      const security = editor.security || commonSecurity([]), enabled = $('mutationMTLS').checked, value = $('mutationTLSOption').value;
      $('mutationTLSFields').hidden = !enabled;
      const option = security.tlsOptions.find(row => `${row.resource.namespace}/${row.resource.name}` === value);
      $('mutationCAReferences').innerHTML = option ? `<span class="k8gb-label">CA Secret references</span><div class="k8gb-row">${C.list(option.caSecretNames).map(name => `<code>${h(option.resource.namespace)}/${h(name)}</code>`).join('') || '<span class="k8gb-muted">Not configured</span>'}</div><span class="k8gb-muted">${h(option.clientAuthType || 'Client authentication not configured')}</span>` : '<span class="k8gb-muted">Select a TLSOption to inspect its CA references.</span>';
      const issue = !security.entrypoints.length ? 'No approved mTLS entrypoints are configured.' : !security.tlsOptions.some(row => row.eligible) ? 'No compatible TLSOption with RequireAndVerifyClientCert is available on all selected clusters.' : '';
      $('mutationTLSHint').hidden = !issue; $('mutationTLSHint').textContent = issue;
      const hasNames = $('mutationCNs').value.trim() !== '';
      $('mutationCNs').disabled = !security.sanFilterConfigured && !hasNames;
      $('mutationSANStatus').textContent = security.sanFilterConfigured ? `certauthz alias: ${security.pluginAlias}. Plugin runtime loading is not verified.` : 'DNS SAN filtering is unavailable: no common certauthz plugin alias is configured.';
    }
    function renderTrafficForm() {
      $('mutationForm').classList.remove('structured');
      const instance = editor.instance, operation = editor.mode.slice(editor.mode.indexOf('-') + 1), candidate = editor.candidates[editor.selected];
      const resource = candidate?.resource || instance.resource;
      const warning = operation === 'overrideIPs' ? 'This override affects all hosts of this Gslb, not only the displayed hostname. Configured exposed hostnames are automatically snapshotted and removed, then restored by Reset IPs. Health checks remain enabled. IPv4 unicast only, 1-16 addresses. DNS convergence is asynchronous and depends on TTL; API write success does not mean DNS has converged.' : operation === 'resetIPs' ? 'Restore the managed annotation snapshot, including the original exposed hostnames. This affects all hosts of this Gslb. Health checks remain enabled; DNS convergence is asynchronous and depends on TTL. API write success does not mean DNS has converged.' : operation === 'excludeSite' ? 'Exclude this site for all global hosts of this source route. This is NOT a graceful connection drain: the generated global Ingress backend deliberately becomes unavailable. Clients with cached DNS may see errors until TTL expires. The parent IngressRoute routing and local siblings remain unchanged. This does not delete GSLB. Include site reverses the exclusion. API write success does not mean DNS has converged.' : 'Reverse the exclusion and include this site in global GSLB traffic again. Parent IngressRoute routing and local siblings remain unchanged. DNS convergence is asynchronous and depends on TTL; API write success does not mean DNS has converged.';
      $('mutationForm').innerHTML = `<p class="k8gb-mono">${h(refLabel(instance.clusterId,resource))}</p><p class="k8gb-notice">${h(warning)}</p>${operation === 'overrideIPs' ? `<label class="k8gb-label" for="mutationIPs">IPv4 unicast addresses (1-16)</label><textarea id="mutationIPs" required rows="4" spellcheck="false" autocomplete="off">${h(C.list(instance.traffic?.ips).join('\n'))}</textarea>` : ''}<label class="k8gb-label" for="mutationReason">Reason (required)</label><textarea id="mutationReason" required rows="2" maxlength="1024"></textarea><div class="k8gb-row">${button('preview','Preview changes','scan-eye','k8gb-primary')}</div>`;
      if (editor.mode.startsWith('native-')) $('mutationForm').insertAdjacentHTML('afterbegin',`<p class="k8gb-error"><strong>${operation === 'excludeSite' ? 'LOCAL INGRESS ROUTING WILL STOP' : 'Restore local Ingress routing'}</strong>${operation === 'excludeSite' ? 'All hosts and HTTP paths of this native Ingress will point to an unavailable backend, including requests sent directly to this cluster. This is not DNS-only exclusion. Cached clients may fail until DNS TTL expires. Include site restores the managed backend snapshot; external edits block automatic restoration.' : 'The exact managed Ingress backend snapshot will be restored. Gslb policy and IP override annotations are not reset.'}</p>`);
      if (editor.mode.startsWith('native-')) $('mutationForm').querySelector('.k8gb-notice').textContent = 'The working native Ingress is changed, not a separate generated route. The existing Gslb, Service objects, balancing policy and IP override are preserved. DNS convergence is asynchronous; a successful API write is not confirmation of traffic withdrawal.';
      if (typeof candidate?.traffic?.excluded === 'boolean' && candidate.traffic.excluded !== C.isExcluded(instance)) $('mutationForm').insertAdjacentHTML('afterbegin', `<p class="k8gb-notice">Requested site state in the fresh source catalog: ${candidate.traffic.excluded ? 'Excluded' : 'Included'}; observed Gslb state: ${C.isExcluded(instance) ? 'Excluded' : 'Included'}. Reconciliation is pending. This operation uses the fresh source state.</p>`);
      renderPreview();
    }
    function updateStrategy() {
      if (!$('mutationStrategy')) return;
      $('mutationPrimaryField').hidden = $('mutationStrategy').value !== 'failover'; $('mutationWeights').hidden = $('mutationStrategy').value !== 'weightedRoundRobin';
      if ($('mutationMTLS')) {
        const enabled = $('mutationMTLS').checked, count = C.unique($('mutationCNs').value.split('\n').map(value => value.trim()).filter(Boolean)).length;
        $('mutationTLSStatus').textContent = enabled ? 'Enabled' : 'Not enabled';
        $('mutationCNCount').textContent = count ? String(count) : '';
        $('mutationCNHint').hidden = enabled || !count;
        $('mutationCNHint').textContent = enabled || !count ? '' : 'DNS SAN filtering requires mTLS.';
        updateTLSDetails();
      }
      updateDiagram();
    }
    function showError(message) { $('mutationError').textContent = message; $('mutationError').hidden = !message; }
    async function preview() {
      const currentEditor = editor, currentPlanner = planner, context = getContext();
      if (editor.referenceLoading || ['planning','applying','unknown','finished'].includes(planner.state.status)) return;
      try {
        showError('');
        const policy = editor.operational ? undefined : readPolicy();
        const candidate = editor.candidates?.[editor.selected];
        if (editor.multi && (selectedCandidates().length !== editor.selectedClusters.size || selectedCandidates().some(candidate => !selectableCandidate(candidate)))) throw new Error('Every selected cluster must have an available, authorized resource');
        const input = {context, group:editor.group, mode:editor.mode === 'traefik-settings' ? `traefik-${editor.operation}` : editor.mode, candidate, name:$('mutationName')?.value, instances:editor.operational ? [editor.instance] : editor.scope === 'all' ? editor.instances : [editor.instances?.[Number(editor.scope)]].filter(Boolean), policy, traffic:{ips:$('mutationIPs')?.value}, traefik:{entrypoint:$('mutationEntrypoint')?.value, hostname:$('mutationHostname')?.value, mtls:$('mutationMTLS')?.checked, local:$('mutationLocal')?.checked, commonNames:$('mutationCNs')?.value.split('\n').map(value => value.trim()).filter(Boolean)}, reason:$('mutationReason').value};
        input.traefik.mtlsEntrypoint = $('mutationMTLSEntrypoint')?.value;
        if ($('mutationTLSOption')?.value) {const [namespace,name] = $('mutationTLSOption').value.split('/'); input.traefik.tlsOption = {namespace,name};}
        if (creating()) input.dnsZone = $('mutationDNSZone')?.value || '';
        let targets = editor.multi ? buildCatalogBatch(input,selectedCandidates()) : buildTargets(input);
        if (targets.some(target => target.request.resource.kind === 'Gslb' && target.request.operation !== 'create')) {
          planner.invalidate(); editor.referenceLoading = true; renderPreview();
          const latest = C.validateOverview(await C.getJSON(`/api/v1/overview?${new URLSearchParams({namespace:context.namespace})}`));
          if (editor !== currentEditor || planner !== currentPlanner || !editing() || getContext().overview !== context.overview || getContext().namespace !== context.namespace || getContext().stale) return;
          targets = refreshGslbTargets(targets,context,latest);
          editor.referenceLoading = false;
        }
        const pending = planner.plan(targets); renderPreview(); await pending; renderPreview();
        if (editing() && editor === currentEditor && planner.state.batch) $('mutationPreview').scrollIntoView({block:'start'});
      } catch (error) { if (editor === currentEditor && editing()) { editor.referenceLoading = false; renderPreview(); showError(error.message); } }
      finally { if (editor === currentEditor && editor.referenceLoading) { editor.referenceLoading = false; if (editing()) renderPreview(); } }
    }
    function renderPreview() {
      if (!editing()) return;
      const state = planner.state, busy = editor.referenceLoading || ['planning','applying'].includes(state.status), done = ['unknown','finished'].includes(state.status);
      $('mutationForm').disabled = busy || done;
      workspace.querySelector('[data-mutation-action="close"]').disabled = state.status === 'applying';
      showError(state.message);
      const rows = C.list(state.batch?.targets), errors = rows.some(row => row.error), deactivate = rows.some(row => ['deactivate','excludeSite'].includes(row.plan?.request.operation));
      $('mutationPreview').innerHTML = `${busy ? `<p role="status">${state.status === 'applying' ? 'Applying once...' : 'Planning changes...'}</p>` : ''}${state.batch ? `<h3>Change preview</h3>${rows.map(row => `<section class="k8gb-plan-target"><h4>${h(clusterName(row.clusterId))} <code>${h(row.clusterId)}</code></h4>${row.error ? `<p class="k8gb-error">${h(row.error.code)}: ${h(row.error.error)}</p>` : `<p class="k8gb-mono">${h(refLabel(row.clusterId, row.plan.request.resource))}</p>${row.plan.noop ? '<p>No changes required.</p>' : ''}${C.list(row.plan.warnings).map(warning => `<p class="k8gb-notice">${h(warning)}</p>`).join('')}${changesHTML(row.plan.changes)}`}</section>`).join('')}<p id="mutationExpiry" role="status"></p>${errors ? '<label class="k8gb-check"><input id="mutationPartial" type="checkbox">Apply only successful plans; failed clusters remain unchanged</label>' : ''}${deactivate ? '<label class="k8gb-check k8gb-disable-consent"><input id="mutationDisableConsent" type="checkbox">I confirm disabling GSLB on the selected Traefik route</label>' : ''}<label class="k8gb-check"><input id="mutationConsent" type="checkbox">I reviewed the before/after changes and approve this plan</label>${button('apply','Apply changes','check','k8gb-primary')}` : ''}${state.result ? `<h3>Apply result</h3>${state.result.targets.map(row => `<section class="k8gb-plan-target"><h4>${h(clusterName(row.clusterId))}</h4>${row.error ? `<p class="k8gb-error">${h(row.error.code)}: ${h(row.error.error)}${row.error.outcomeUnknown ? ' Outcome unknown. Check audit and live resource state; do not retry this apply.' : ''}</p>` : `<p>${h(row.result.status)} / ${h(row.result.reconciliation)}</p>${C.list(row.result.warnings).map(warning => `<p class="k8gb-notice">${h(warning)}</p>`).join('')}`}</section>`).join('')}` : ''}`;
      if (state.status === 'unknown' && !state.message) showError('Outcome unknown. Check audit and live resource state; do not retry this apply.');
      if (rows.length > 1) $('mutationPreview').insertAdjacentHTML('afterbegin','<p class="k8gb-notice">Multi-cluster apply is not atomic. Review the result for every selected cluster; a failed cluster is not rolled back or retried automatically.</p>');
      if (rows.some(row => row.plan?.request.operation === 'excludeSite')) $('mutationDisableConsent').parentElement.lastChild.textContent = 'I confirm excluding this site from traffic, without deleting GSLB; Include site reverses this exclusion';
      if (rows.some(row => row.plan?.request.provider === 'native' && row.plan.request.operation === 'excludeSite')) $('mutationConsent').parentElement.insertAdjacentHTML('beforebegin','<label class="k8gb-check k8gb-disable-consent"><input id="mutationLocalConsent" type="checkbox">I understand that local routing through this native Ingress will stop for ALL hosts and HTTP paths, not only DNS traffic</label>');
      updateApply();
    }
    function updateApply() {
      if (!editing() || !planner) return;
      const expiry = $('mutationExpiry'), state = planner.state;
      if (expiry) { const seconds = Math.max(0, Math.ceil((state.expiresAt - Date.now()) / 1000)); expiry.textContent = seconds ? `Plan expires in ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2,'0')}` : 'Plan expired. Preview changes again.'; }
      const apply = workspace.querySelector('[data-mutation-action="apply"]');
      if (apply) apply.disabled = !planner.canApply($('mutationConsent')?.checked, $('mutationPartial')?.checked === true, $('mutationDisableConsent')?.checked === true, $('mutationLocalConsent')?.checked === true) || !authorizedPlan();
    }
    function authorizedPlan() {
      const context = getContext(), rows = C.list(planner?.state.batch?.targets);
      if (editor.operational) {
        try {
          for (const row of rows.filter(row => !row.error)) buildTargets({context, group:editor.group, mode:editor.mode, instances:[editor.instance], candidate:editor.candidates[editor.selected], traffic:row.plan?.request.traffic, reason:row.plan?.request.reason});
        } catch (_) { return false; }
      }
      if (editor.multi && (rows.length !== selectedCandidates().length || rows.some(row => !selectedCandidates().some(candidate => row.clusterId === candidate.clusterId && (row.error || sameResource(row.plan?.request.target || row.plan?.request.resource,candidate.resource)))))) return false;
      return rows.length > 0 && rows.every(row => row.error || row.plan?.request.resource.namespace === context.namespace && can(context,row.clusterId,row.plan.request.provider,row.plan.request.operation,(editor.multi ? selectedCandidates().find(candidate => candidate.clusterId === row.clusterId) : editor.candidates?.[editor.selected])?.catalog));
    }
    async function open(mode, group, source) {
      if (applying()) return;
      mount(); opener = document.activeElement; planner = createPlanner(); clearInterval(timer); catalogRequest.cancel(); diagram?.unmount();
      auditRequest.cancel(); if (audit) audit.hidden = true;
      const operational = mode.startsWith('traffic-') || ['traefik-excludeSite','traefik-includeSite','native-excludeSite','native-includeSite'].includes(mode);
      editor = {mode, group, operational, multi:['native-create','traefik-enable','traefik-settings'].includes(mode), instance:source, namespace:getContext().namespace, candidates:[], selected:0, instances:nativeInstances(getContext(),group), scope:'all', base:0, operation:'update'};
      const titles = {'traffic-overrideIPs':'Override IPs','traffic-resetIPs':'Reset IPs','traefik-excludeSite':'Exclude site','traefik-includeSite':'Include site','native-excludeSite':'Exclude native site / local routing impact','native-includeSite':'Include native site'};
      const title = titles[mode] || (mode === 'native-create' ? 'Create native GSLB' : mode === 'native-update' ? 'Edit GSLB policy' : mode === 'traefik-enable' ? 'Create GSLB from IngressRoute' : 'Source IngressRoute settings');
      workspace.innerHTML = `<div class="k8gb-editor-heading">${button('close','Close','arrow-left')}<div><p class="k8gb-eyebrow">${mode.startsWith('native') ? 'Direct Gslb' : mode.startsWith('traffic') ? 'Operational traffic' : 'Webhook / Traefik'} / ${h(editor.namespace)}</p><h2 id="mutationHeading" tabindex="-1">${title}</h2>${group?.hostname ? `<p class="k8gb-editor-host">${h(group.hostname)}</p>` : ''}</div></div><div id="mutationWarnings"></div><p id="mutationError" class="k8gb-error" role="alert" hidden></p><div class="k8gb-editor-layout${operational ? ' operational' : ''}"><fieldset id="mutationForm" class="k8gb-mutation-form"><p>Loading catalog...</p></fieldset>${!operational && diagram ? `<div class="k8gb-editor-diagram">${diagram.html('k8gbMutationDNS')}</div>` : ''}</div><div id="mutationPreview" aria-live="polite"></div>`;
      workspace.hidden = false; onViewChange('edit'); $('mutationHeading').focus(); timer = setInterval(updateApply, 1000);
      if (mode.startsWith('traffic-')) {
        try {
          const instance = observedInstance(getContext(),source), operation = mode.slice(8);
          if (!supported.traffic.includes(operation) || !can(getContext(),instance.clusterId,'traffic',operation)) throw new Error('This traffic operation is not authorized');
          if (operation === 'resetIPs' && !resettable(instance)) throw new Error('No conflict-free managed snapshot. Inspect the live annotations first');
          renderForm(); ($('mutationIPs') || $('mutationReason')).focus();
        } catch (error) { $('mutationForm').innerHTML = ''; showError(error.message); }
        return;
      }
      if (mode === 'native-update') { seed(); renderForm(); $('mutationScope').focus(); return; }
      await catalogRequest.run(async signal => {
        const data = await C.getJSON(`/api/v1/catalog?${new URLSearchParams({namespace:editor.namespace})}`, {signal});
        if (data?.namespace !== editor.namespace || !Array.isArray(data.sites)) throw new Error('Invalid catalog scope');
        return data;
      }, data => {
        editor.candidates = data.sites.flatMap(site => C.list(site.catalog?.candidates).map(candidate => ({...candidate, clusterId:site.clusterId, catalog:site.catalog}))).filter(candidate => {
          if (candidate.resource?.namespace !== editor.namespace) return false;
          if (operational && mode.startsWith('native-')) return source && siteCandidate(getContext(),group,source,candidate) && can(getContext(),candidate.clusterId,'native',exclusionOperation(source,candidate),candidate.catalog);
          if (mode === 'native-create') return candidate.provider === 'native' && ['Ingress','HTTPRoute'].includes(candidate.resource.kind) && !candidate.enabled && can(getContext(),candidate.clusterId,'native','create',candidate.catalog);
          if (candidate.provider !== 'traefik' || candidate.resource.kind !== 'IngressRoute') return false;
          if (mode === 'traefik-enable') return !candidate.enabled && can(getContext(),candidate.clusterId,'traefik','enable',candidate.catalog);
          if (operational) return source && siteCandidate(getContext(),group,source,candidate) && can(getContext(),candidate.clusterId,'traefik',exclusionOperation(source,candidate),candidate.catalog);
          return candidate.enabled && C.list(group?.instances).some(instance => C.management(instance).mode === 'traefik' && instance.clusterId === candidate.clusterId && sameResource(instance.source,candidate.resource)) && ['update','deactivate'].some(operation => can(getContext(),candidate.clusterId,'traefik',operation,candidate.catalog));
        });
        const warnings = data.sites.flatMap(site => [site.error ? `${site.clusterId}: ${site.error}` : '', ...C.list(site.catalog?.warnings)]).filter(Boolean);
        if (!editor.candidates.length) { $('mutationForm').innerHTML = '<p>No authorized catalog resources available.</p>'; showError(warnings.join('; ')); return; }
        const sourceIndex = source ? editor.candidates.findIndex(candidate => candidate.clusterId === source.clusterId && sameResource(source.source,candidate.resource)) : -1;
        editor.selected = sourceIndex >= 0 ? sourceIndex : Math.max(0,editor.candidates.findIndex(candidate => candidate.eligible));
        if (operational) { editor.mode = `${mode.startsWith('native-') ? 'native' : 'traefik'}-${exclusionOperation(source,editor.candidates[0])}`; $('mutationHeading').textContent = titles[editor.mode]; }
        if (mode === 'traefik-settings' && !can(getContext(),editor.candidates[0].clusterId,'traefik','update',editor.candidates[0].catalog)) editor.operation = 'deactivate';
        if (!operational) seed(); renderForm();
        $('mutationWarnings').innerHTML = warnings.length ? `<details class="k8gb-technical"><summary>Catalog warnings (${warnings.length})</summary>${warnings.map(warning => `<p class="k8gb-notice">${h(warning)}</p>`).join('')}</details>` : '';
        $(operational ? 'mutationReason' : 'mutationCandidate').focus();
      }, error => { $('mutationForm').innerHTML = ''; showError(error.message); });
    }
    function actions(group) {
      let html = '';
      if (nativeInstances(getContext(),group).length) html += button('native-update','Edit GSLB policy','file-pen-line');
      const seen = new Set();
      C.list(group.instances).forEach((instance,index) => {
        const key = JSON.stringify([instance.clusterId,instance.source]);
        if (C.management(instance).mode !== 'traefik' || instance.source?.kind !== 'IngressRoute' || seen.has(key) || !['update','deactivate'].some(operation => can(getContext(),instance.clusterId,'traefik',operation))) return;
        seen.add(key); html += `<button type="button" class="k8gb-button" data-traefik-instance="${index}">${icon('webhook')}Edit source IngressRoute / ${h(clusterName(instance.clusterId))}</button>`;
      });
      return html ? `<div class="k8gb-row k8gb-mutation-actions">${html}</div>` : '';
    }
    function prepareTraffic(group) {
      const context = getContext();
      if (context.demo || context.stale || !C.list(group.instances).some(instance => ['excludeSite','includeSite'].some(operation => can(context,instance.clusterId,C.management(instance).mode === 'native' ? 'native' : 'traefik',operation)))) return;
      if (trafficCatalog?.overview === context.overview && trafficCatalog.groupId === group.id && trafficCatalog.namespace === context.namespace) return;
      // Eligibility belongs to this snapshot; opening the workspace fetches a fresh source RV again.
      const scope = {overview:context.overview,namespace:context.namespace,groupId:group.id,candidates:[]}; trafficCatalog = scope;
      return trafficRequest.run(signal => C.getJSON(`/api/v1/catalog?${new URLSearchParams({namespace:context.namespace})}`,{signal}), data => {
        if (data?.namespace === scope.namespace && Array.isArray(data.sites)) scope.candidates = data.sites.flatMap(site => C.list(site.catalog?.candidates).map(candidate => ({...candidate,clusterId:site.clusterId,catalog:site.catalog})));
        onTrafficReady();
      }, () => onTrafficReady());
    }
    function trafficActions(instance, index, group) {
      try { reference(instance.resource); } catch (_) { return ''; }
      const context = getContext(), canReset = resettable(instance);
      const action = (mode, label, symbol, disabled = false, title = '') => `<button type="button" class="k8gb-button${mode.endsWith('-excludeSite') ? ' k8gb-danger' : ''}" data-traffic-instance="${index}" data-traffic-mode="${mode}"${disabled ? ' disabled' : ''}${title ? ` title="${h(title)}"` : ''}>${icon(symbol)}${h(label)}</button>`;
      let html = '';
      if (can(context,instance.clusterId,'traffic','overrideIPs')) html += action('traffic-overrideIPs','Override IPs','network');
      if (can(context,instance.clusterId,'traffic','resetIPs')) html += action('traffic-resetIPs','Reset IPs','rotate-ccw',!canReset,canReset ? '' : instance.traffic?.overrideConflict ? 'External changes detected. Inspect live annotations before reset.' : 'No managed snapshot to restore.');
      const candidate = trafficCatalog?.overview === context.overview && trafficCatalog.groupId === group?.id && trafficCatalog.namespace === context.namespace ? trafficCatalog.candidates.find(candidate => siteCandidate(context,group,instance,candidate)) : null;
      const operation = exclusionOperation(instance,candidate);
      const provider = C.management(instance).mode === 'native' ? 'native' : 'traefik';
      if (candidate && can(context,instance.clusterId,provider,operation,candidate.catalog)) html += action(`${provider}-${operation}`,operation === 'includeSite' ? 'Include site' : 'Exclude site','power',false,provider === 'native' ? 'Also changes local routing for all hosts and paths of the native Ingress' : 'Exclude only the generated global route');
      return html ? `<div class="k8gb-row k8gb-mutation-actions">${html}</div>` : '';
    }
    async function openAudit() {
      if (applying()) return;
      opener = document.activeElement; clearInterval(timer); catalogRequest.cancel(); diagram?.unmount(); if (workspace) workspace.hidden = true;
      if (auditNamespace !== getContext().namespace) auditFilters = auditDefaults();
      auditNamespace = getContext().namespace; auditData = null;
      if (!audit) {
        audit = document.createElement('section'); audit.className = 'k8gb-audit'; audit.id = 'auditWorkspace'; audit.hidden = true; audit.setAttribute('aria-labelledby','auditHeading'); $('main').append(audit);
        audit.addEventListener('click', event => {
          if (event.target.closest('[data-audit-close]')) close();
          if (event.target.closest('[data-audit-refresh]')) loadAudit();
          if (event.target.closest('[data-audit-reset]')) { auditFilters = auditDefaults(); populateAuditControls(); renderAuditEvents(); }
          if (event.target.closest('[data-audit-direction]')) { auditFilters.direction = auditFilters.direction === 'desc' ? 'asc' : 'desc'; auditFilters.page = 1; renderAuditEvents(); }
          const page = event.target.closest('[data-audit-page]');
          if (page) { auditFilters.page += Number(page.dataset.auditPage); renderAuditEvents(); $('auditResultsHeading').focus(); }
        });
        const filterChanged = event => {
          const key = event.target.dataset.auditFilter;
          if (!key || event.type === 'input' && event.target.tagName === 'SELECT' || event.type === 'change' && event.target.tagName !== 'SELECT') return;
          auditFilters[key] = event.target.value; auditFilters.page = 1;
          if (key === 'sort') auditFilters.direction = event.target.value === 'time' ? 'desc' : 'asc';
          renderAuditEvents();
        };
        audit.addEventListener('input',filterChanged); audit.addEventListener('change',filterChanged);
      }
      const field = (key,label,options) => `<label class="k8gb-label">${label}<select id="audit-${key}" data-audit-filter="${key}">${options || ''}</select></label>`;
      audit.innerHTML = `<div class="k8gb-editor-heading"><button class="k8gb-button" data-audit-close>${icon('arrow-left')}Close</button><div><p class="k8gb-eyebrow">${h(getContext().namespace)} / Authorized cluster sources</p><h2 id="auditHeading" tabindex="-1">Recent mutation audit</h2></div><span class="k8gb-spacer"></span><button class="k8gb-button k8gb-icon-button" data-audit-refresh title="Refresh audit" aria-label="Refresh audit">${icon('refresh-cw')}</button></div><div id="auditSources"></div><section class="k8gb-audit-controls" aria-label="Audit filters"><div class="k8gb-audit-search-row"><label class="k8gb-label">Search audit<span class="k8gb-audit-search">${icon('search')}<input id="audit-search" type="search" data-audit-filter="search" placeholder="Object, user, reason, changed field..." autocomplete="off"></span></label>${field('period','Time range','<option value="all">All retained history</option><option value="1h">Last hour</option><option value="24h">Last 24 hours</option><option value="7d">Last 7 days</option><option value="custom">Custom range</option>')}<div class="k8gb-audit-sort">${field('sort','Sort by','<option value="time">Time</option><option value="resource">Object</option><option value="clusterId">Cluster</option><option value="actor">User</option><option value="operation">Operation</option><option value="status">Result</option>')}<button class="k8gb-button k8gb-icon-button" data-audit-direction></button></div></div><div class="k8gb-audit-filter-row">${field('clusterId','Cluster')}${field('actor','User')}${field('provider','Management')}${field('operation','Operation')}${field('status','Result')}</div><div id="auditCustomRange" class="k8gb-audit-range" hidden><label class="k8gb-label">From (local time)<input id="audit-from" type="datetime-local" data-audit-filter="from"></label><label class="k8gb-label">To (local time)<input id="audit-to" type="datetime-local" data-audit-filter="to"></label></div><div class="k8gb-row k8gb-audit-results"><h3 id="auditResultsHeading" tabindex="-1" aria-live="polite"></h3><span class="k8gb-spacer"></span><button class="k8gb-button" data-audit-reset>${icon('filter-x')}Reset filters</button></div></section><p id="auditQueryError" class="k8gb-error" role="alert" hidden></p><div id="auditEvents" aria-busy="true"></div><nav id="auditPagination" class="k8gb-audit-pagination" aria-label="Audit pages" hidden><label class="k8gb-label">Per page<select id="audit-pageSize" data-audit-filter="pageSize"><option value="25">25</option><option value="50">50</option><option value="100">100</option></select></label><span id="auditPageSummary" class="k8gb-muted"></span><span class="k8gb-spacer"></span><button class="k8gb-button k8gb-icon-button" data-audit-page="-1" title="Previous page" aria-label="Previous audit page">${icon('chevron-left')}</button><button class="k8gb-button k8gb-icon-button" data-audit-page="1" title="Next page" aria-label="Next audit page">${icon('chevron-right')}</button></nav>`;
      populateAuditControls(); renderAuditEvents();
      audit.hidden = false; onViewChange('audit'); $('auditHeading').focus(); await loadAudit();
    }
    async function loadAudit() {
      const namespace = getContext().namespace; auditData = null;
      $('auditSources').textContent = ''; $('auditQueryError').hidden = true;
      $('auditEvents').textContent = 'Loading audit...'; $('auditEvents').setAttribute('aria-busy','true'); $('auditPagination').hidden = true; $('auditResultsHeading').textContent = 'Loading audit...';
      await auditRequest.run(signal => C.getJSON(`/api/v1/audit?${new URLSearchParams({namespace})}`, {signal}), data => {
        $('auditEvents').setAttribute('aria-busy','false');
        if (!data || !Array.isArray(data.events) && data.events != null || data.namespace && data.namespace !== namespace) { $('auditEvents').textContent = 'Invalid audit response'; $('auditResultsHeading').textContent = 'Audit unavailable'; return; }
        const sources = C.list(data.sources), available = sources.filter(source => source.available).length;
        const coverage = sources.length ? `<section class="k8gb-audit-sources" aria-label="Audit sources"><div class="k8gb-row"><strong class="${data.partial ? 'k8gb-error' : ''}">${data.partial ? 'Partial audit' : 'All sources responded'} / ${available} of ${sources.length}</strong><span class="k8gb-spacer"></span><span class="k8gb-muted">${h(C.formatTime(data.collectedAt))}</span></div><div class="k8gb-table-scroll"><table><thead><tr><th>Cluster</th><th>Source</th><th>Records</th><th>Retained history</th></tr></thead><tbody>${sources.map(auditSourceHTML).join('')}</tbody></table></div></section>` : '';
        const memory = sources.some(source => source.retention?.storage === 'memory') ? '<p class="k8gb-notice neutral">Replica-local memory is not durable. Agent restarts clear history; multiple replicas behind one agent URL can return different retained events.</p>' : '';
        const unknownRetention = sources.some(source => source.available && !source.retention) ? '<p class="k8gb-notice neutral">Some agents do not report retention. Shared or durable history is not verified for those sources.</p>' : '';
        auditData = {...data,events:C.list(data.events).filter(event => event?.resource?.namespace === namespace)};
        $('auditSources').innerHTML = `${coverage}${memory}${unknownRetention}${C.list(data.warnings).map(warning => `<p class="k8gb-notice neutral">${h(warning)}</p>`).join('')}`;
        populateAuditControls(); renderAuditEvents();
      }, error => { $('auditEvents').textContent = `Audit unavailable: ${error.message}`; $('auditEvents').setAttribute('aria-busy','false'); $('auditResultsHeading').textContent = 'Audit unavailable'; });
    }
    function populateAuditControls() {
      const events = C.list(auditData?.events);
      for (const key of ['clusterId','actor','provider','operation','status']) {
        const values = C.unique([...events.map(event => event[key]),...(key === 'clusterId' ? C.list(auditData?.sources).map(source => source.clusterId) : [])]).filter(Boolean).sort(auditCollator.compare);
        if (auditFilters[key] && !values.includes(auditFilters[key]) && auditFilters[key] !== '@outcomeUnknown') values.push(auditFilters[key]);
        const labels = {clusterId:'All clusters',actor:'All users',provider:'All management',operation:'All operations',status:'All results'};
        const name = value => key === 'clusterId' ? clusterName(value) : auditLabels[key]?.[value] || value;
        $(`audit-${key}`).innerHTML = `<option value="">${labels[key]}</option>${key === 'status' ? '<option value="@outcomeUnknown">Outcome unknown</option>' : ''}${values.map(value => `<option value="${h(value)}">${h(name(value))}</option>`).join('')}`;
      }
      for (const [key,value] of Object.entries(auditFilters)) if ($(`audit-${key}`)) $(`audit-${key}`).value = value;
    }
    function renderAuditEvents() {
      $('auditCustomRange').hidden = auditFilters.period !== 'custom';
      const direction = audit.querySelector('[data-audit-direction]'), label = auditFilters.sort === 'time' ? auditFilters.direction === 'desc' ? 'Newest first' : 'Oldest first' : auditFilters.direction === 'asc' ? 'Ascending' : 'Descending';
      direction.innerHTML = icon(auditFilters.direction === 'desc' ? 'arrow-down' : 'arrow-up'); direction.title = label; direction.setAttribute('aria-label',label);
      if (!auditData) return;
      const clusterNames = Object.fromEntries(C.list(getContext().overview?.sites).map(site => [site.cluster.id,site.cluster.name || site.cluster.id]));
      const result = queryAudit(auditData.events,auditNamespace,{...auditFilters,clusterNames});
      auditFilters.page = result.page;
      $('auditQueryError').hidden = !result.error; $('auditQueryError').textContent = result.error;
      $('auditResultsHeading').textContent = `${result.matched} of ${result.total} events`;
      $('auditEvents').innerHTML = result.events.map(auditEventHTML).join('') || (result.error ? '' : `<p class="k8gb-muted">${result.total ? 'No events match these filters.' : auditData.partial ? 'No retained events from responding sources. The audit is incomplete.' : 'No recent events in this namespace.'}</p>`);
      $('auditPagination').hidden = !result.matched;
      $('auditPageSummary').textContent = `${(result.page - 1) * result.pageSize + 1}-${Math.min(result.page * result.pageSize,result.matched)} / ${result.matched} | Page ${result.page} of ${result.pages}`;
      audit.querySelector('[data-audit-page="-1"]').disabled = result.page <= 1;
      audit.querySelector('[data-audit-page="1"]').disabled = result.page >= result.pages;
    }
    function auditSourceHTML(source) {
      const retention=source.retention, status=!source.available ? 'Unavailable' : source.partial ? 'Degraded' : 'Available';
      const details=!source.available ? h(source.error || 'Source unavailable') : !retention ? 'Retention not reported' : [
        retention.storage === 'configMap' ? `Shared ConfigMap / ${h(retention.configMap)}` : `${h(retention.storage)} / ${h(retention.scope)}`,
        Number.isFinite(retention.retainedBytes) && retention.limitBytes>0 ? `${h(Math.ceil(retention.retainedBytes/1024))} / ${h(Math.ceil(retention.limitBytes/1024))} KiB` : '',
        retention.startedAt ? `since ${h(C.formatTime(retention.startedAt))}` : '',
        retention.evictedEvents ? `${h(retention.evictedEvents)} evicted` : '',
        retention.pendingEvents ? `${h(retention.pendingEvents)} pending` : '',
        retention.droppedEvents ? `${h(retention.droppedEvents)} dropped` : ''
      ].filter(Boolean).join(' / ');
      return `<tr><td>${h(clusterName(source.clusterId))}</td><td><span class="k8gb-state ${source.available && !source.partial ? 'consistent' : 'warning'}">${status}</span></td><td>${source.available ? h(source.events) : '-'}</td><td>${details}</td></tr>`;
    }
    function auditEventHTML(event) {
      const status = String(event.status || 'Not reported');
      const tone = event.outcomeUnknown || status.endsWith('_failed') || ['failed','error','denied'].includes(status) ? 'warning' : ['applied','noop'].includes(status) ? 'consistent' : 'unknown';
      return `<details class="k8gb-plan-target k8gb-audit-event" data-audit-status="${h(status)}"${event.outcomeUnknown ? ' open' : ''}><summary><span class="k8gb-audit-time">${h(C.formatTime(event.time))}</span><span><strong>${h(event.provider)} / ${h(event.operation)}</strong><span class="k8gb-audit-ref">${h(clusterName(event.clusterId))} / ${h(event.resource.kind)} ${h(event.resource.name)}</span><span class="k8gb-audit-reason">${h(event.reason)}</span></span><span class="k8gb-audit-actor">${h(event.actor || 'Not reported')}</span><span class="k8gb-state ${tone}">${h(auditLabels.status[status] || status)}${event.outcomeUnknown ? ' / Outcome unknown' : ''}</span></summary><p class="k8gb-mono">${h(refLabel(event.clusterId,event.resource))}</p>${changesHTML(event.changes)}</details>`;
    }
    return {open, openAudit, close, actions, trafficActions, prepareTraffic, get active() { return editing() ? 'edit' : auditing() ? 'audit' : null; }, get applying() { return applying(); }, contextChanged() { if (editing() && !['applying','unknown','finished'].includes(planner?.state.status)) { planner?.invalidate(); if (editor.namespace !== getContext().namespace || !hasEdits(getContext())) close(); else renderPreview(); } if (auditing()) close(); }, destroy() { clearInterval(timer); catalogRequest.cancel(); auditRequest.cancel(); trafficRequest.cancel(); diagram?.unmount(); }};
  }
  return {allows, can, hasEdits, nativeInstances, reference, sameResource, validateIPs, validatePolicy, commonSecurity, buildTargets, matchingCandidates, buildCatalogBatch, zonesFor, commonZones, inZone, draftDNSInput, refreshGslbTargets, requestSignature, diffRows, queryAudit, postJSON, createPlanner, createView};
});
