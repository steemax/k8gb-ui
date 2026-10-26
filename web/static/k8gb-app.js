(function () {
  'use strict';
  const C = window.K8gbCore, M = window.K8gbMutations, h = C.escape, $ = id => document.getElementById(id);
  const state = {clusters:[], namespaces:[], namespace:null, overview:null, demo:false, clusterError:'', namespaceError:'', overviewError:'', loading:false, metadataLoading:true, view:'cards', selected:C.groupFromHash(location.hash), tab:'overview', policyIndex:0};
  const requests = {clusters:C.createLatestRequest(), namespaces:C.createLatestRequest(), overview:C.createLatestRequest()};
  const dns = window.K8gbDNS.createView();
  const mutationContext = () => ({overview:state.overview, namespace:state.namespace, demo:state.demo, stale:state.loading || state.metadataLoading || !!state.overviewError || !!state.clusterError});
  const mutations = M.createView({getContext:mutationContext, icon, onApplied:() => loadOverview(state.namespace), onTrafficReady:() => { if (state.selected) renderDetail(); }, onViewChange:syncView});
  let returnGroup = '', refreshSequence = 0;
  try { if (localStorage.getItem('k8gb-ui-view') === 'rows') state.view = 'rows'; } catch (_) { /* Optional preference. */ }
  function icon(name) {
    const key = name.replace(/(^|-)(\w)/g, (_, __, char) => char.toUpperCase());
    return window.lucide?.icons[key] ? window.lucide.createElement(window.lucide.icons[key], {'aria-hidden':'true', width:16, height:16}).outerHTML : '';
  }
  function badge(label, tone = 'unknown') {
    const symbol = {consistent:'circle-check', warning:'triangle-alert', info:'info', unknown:'circle-help'}[tone] || 'circle-help';
    return `<span class="k8gb-state ${tone}">${icon(symbol)}${h(label)}</span>`;
  }
  function managementBadge(value) {
    return `<span class="k8gb-management ${h(value.mode)}" aria-label="Management: ${h(value.label)}">${icon(value.icon)}${h(value.label)}</span>`;
  }
  function managementSummary(group) {
    return `<div class="k8gb-management-list">${C.groupManagement(group).map(managementBadge).join('')}</div>`;
  }
  function managementDetails(instance) {
    const mode = C.management(instance), ref = mode.mode === 'native' ? instance.resource : instance.source;
    return `<div class="k8gb-management-detail">${managementBadge(mode)}<span class="k8gb-muted">${mode.mode === 'external' ? 'Source' : 'Write target'}</span>${ref ? `<code>${h(ref.kind)} ${h(ref.namespace)}/${h(ref.name)}</code>` : '<span class="k8gb-muted">External controller</span>'}<span class="k8gb-muted">${mode.mode === 'native' ? 'Gslb spec' : mode.mode === 'traefik' ? 'IngressRoute annotations' : 'Not managed by this adapter'}</span></div>`;
  }
  function cluster(id) { return state.overview?.sites.find(site => site.cluster?.id === id)?.cluster || state.clusters.find(item => item.id === id) || {id, name:id}; }
  function site(id) { return state.overview?.sites.find(item => item.cluster?.id === id); }
  function clusterLabel(id) { const item = cluster(id); return item.name || item.id; }
  function effectiveHealth(instance) { return site(instance.clusterId)?.available === true ? C.instanceHealth(instance) : C.health(); }
  function statusFor(group) {
    const status = C.groupStatus(group);
    if (status.tone !== 'warning' && C.list(group.instances).some(instance => site(instance.clusterId)?.available !== true)) return {tone:'unknown', label:'Unknown'};
    return status;
  }
  function names(group) { return C.unique(C.list(group.instances).map(instance => C.management(instance).mode === 'traefik' ? instance.source?.name : instance.resource?.name)).join(' / ') || group.hostname || group.id; }
  function syncView() {
    $('catalog').hidden = Boolean(state.selected) || !!mutations.active;
    $('detail').hidden = !state.selected || !!mutations.active;
    $('mutationActions').hidden = !!mutations.active;
    $('snapshotScope').hidden = !!mutations.active;
    $('audit').setAttribute('aria-pressed', String(mutations.active === 'audit'));
    if (mutations.active) dns.unmount();
    else if (state.selected) renderDetail();
  }
  function targetsHTML(instance) {
    if (instance.dnsObserved !== true) return '<span class="k8gb-muted">DNS not observed</span>';
    return C.unique(instance.dnsTargets).map(target => `<code>${h(target)}</code>`).join('') || '<span class="k8gb-muted">Observed empty answer</span>';
  }
  function allTargetsHTML(group) {
    const observed = C.list(group.instances).filter(instance => instance.dnsObserved === true);
    const values = C.unique(observed.flatMap(instance => C.list(instance.dnsTargets)));
    return values.map(target => `<code>${h(target)}</code>`).join('') || `<span class="k8gb-muted">${observed.length ? 'Observed empty answer' : 'DNS not observed'}</span>`;
  }
  function signalsHTML(group, full = false) {
    const signals = C.list(group.signals);
    const shown = full ? signals : signals.slice(0, 2);
    return shown.length ? `<ul class="k8gb-signals${full ? ' k8gb-signals-full' : ''}">${shown.map(signal => {
      const tone = C.warningSignal(signal) ? 'warning' : String(signal.severity).toLowerCase() === 'info' ? 'info' : 'unknown';
      return `<li class="k8gb-signal ${tone}" data-signal-code="${h(signal.code || '')}"><span class="k8gb-signal-label">${icon(tone === 'warning' ? 'triangle-alert' : tone === 'info' ? 'info' : 'circle-help')}<strong>${h(C.signalLabel(signal.code || signal.severity))}</strong></span><span class="k8gb-signal-message">${h(signal.message)}</span></li>`;
    }).join('')}${!full && signals.length > shown.length ? `<li class="k8gb-signal">+${signals.length - shown.length} signals</li>` : ''}</ul>` : '';
  }
  function chipsHTML(group) {
    return C.list(group.instances).map(instance => { const health = effectiveHealth(instance); return `<span class="k8gb-site-chip ${health.tone}" title="${h(instance.clusterId)} / application ${h(health.label)}"><strong>${h(clusterLabel(instance.clusterId))}</strong>${badge(health.label, health.tone)}</span>`; }).join('') || '<span class="k8gb-muted">No instances reported</span>';
  }
  function endpointLabel(group) {
    const summary = C.endpointSummary(C.list(group.instances).flatMap(instance => C.list(instance.backends)));
    return summary.known ? `${summary.ready}/${summary.total} endpoints${summary.known < summary.count ? ' (partial)' : ''}` : 'Endpoints unknown';
  }
  function policySummary(group) {
    const instances = C.list(group.instances);
    return {strategy:C.unique(instances.map(instance => C.policyLabel(instance.policy))).join(' / ') || 'Unknown', ttl:C.unique(instances.map(instance => C.ttlLabel(instance.policy))).join(' / ') || 'Not reported'};
  }
  function renderTheme() {
    const dark = document.documentElement.dataset.theme === 'dark', label = dark ? 'Switch to light theme' : 'Switch to dark theme';
    $('theme').innerHTML = icon(dark ? 'sun' : 'moon'); $('theme').title = label; $('theme').setAttribute('aria-label', label);
  }
  function renderSites() {
    const opened = new Set([...$('sites').querySelectorAll('details[open]')].map(element => element.dataset.clusterId));
    const all = new Map(state.clusters.map(item => [item.id, item]));
    for (const item of C.list(state.overview?.sites)) if (item.cluster?.id) all.set(item.cluster.id, item.cluster);
    $('sites').setAttribute('aria-busy', String(state.metadataLoading));
    $('siteCount').textContent = C.countLabel(all.size, 'cluster');
    $('sites').innerHTML = [...all.values()].map(item => {
      const current = site(item.id), available = current?.available === true, inventory = C.inventoryStatus(current);
      const access = current ? available ? 'Read available' : 'Read unavailable' : 'Read unknown';
      const tone = current ? available ? 'consistent' : 'warning' : 'unknown';
      const instances = C.list(state.overview?.groups).flatMap(group => C.list(group.instances)).filter(instance => instance.clusterId === item.id);
      const appHealth = available ? C.aggregateHealth(instances) : C.health();
      return `<details class="k8gb-source" data-cluster-id="${h(item.id)}"${opened.has(item.id) ? ' open' : ''}><summary>${icon('server')}<strong>${h(item.name || item.id)}</strong>${badge(access, tone)}${available && current.objectsComplete === false ? badge(inventory.label, inventory.tone) : ''}${icon('chevron-down')}</summary><div class="k8gb-source-detail"><dl class="k8gb-reference"><dt>Cluster ID</dt><dd><code>${h(item.id)}</code></dd><dt>GeoTags</dt><dd>${h(C.unique(item.geoTags).join(', ') || 'Not reported')}</dd><dt>Zones</dt><dd>${h(C.unique(item.zones).join(', ') || 'Not reported')}</dd><dt>Snapshot</dt><dd>${h(C.formatTime(current?.collectedAt))}</dd><dt>Primary GSLB inventory</dt><dd>${badge(inventory.label, inventory.tone)}</dd><dt>Application health</dt><dd>${badge(appHealth.label, appHealth.tone)}</dd></dl>${current?.error ? `<p class="k8gb-source-issues">${h(current.error)}</p>` : ''}${warningList(current?.warnings)}</div></details>`;
    }).join('') || `<span class="k8gb-muted">${state.metadataLoading ? 'Loading clusters...' : 'No clusters reported'}</span>`;
  }
  function warningList(warnings) { return C.list(warnings).length ? `<ul class="k8gb-issues">${C.list(warnings).map(warning => `<li>${h(warning)}</li>`).join('')}</ul>` : ''; }
  function renderNamespaces() {
    const values = C.unique([...state.namespaces, state.namespace]).sort((a,b) => a.localeCompare(b));
    $('namespace').innerHTML = values.length ? values.map(value => `<option value="${h(value)}">${h(value)}${!state.namespaces.includes(value) ? ' (last selected)' : ''}</option>`).join('') : `<option value="">${state.namespaceError ? 'Namespaces unavailable' : 'No namespaces reported'}</option>`;
    $('namespace').value = state.namespace || ''; $('namespace').disabled = !values.length;
  }
  function renderNotices() {
    const notices = [];
    const old = state.overview;
    if (state.loading) notices.push(`<div class="k8gb-notice neutral" role="status">${icon('loader-circle')}Loading ${h(state.namespace || 'namespaces')}...${old ? ` Showing the last snapshot for ${h(old.namespace || 'all namespaces')}.` : ''}</div>`);
    if (state.overviewError) notices.push(`<div class="k8gb-error" role="alert">${icon('triangle-alert')}<strong>${old ? 'Stale snapshot.' : 'Snapshot unavailable.'}</strong><span>${h(state.overviewError)}${old ? ` Last successful snapshot: ${h(C.formatTime(old.collectedAt))}, namespace ${h(old.namespace || 'all namespaces')}.` : ''}</span></div>`);
    if (!state.namespaceError && state.namespace === null && !state.metadataLoading) notices.push(`<div class="k8gb-notice neutral">${icon('info')}No accessible namespaces.${old ? ` Showing the last snapshot for ${h(old.namespace)}.` : ''}</div>`);
    for (const [name, message] of [['Clusters', state.clusterError], ['Namespaces', state.namespaceError]]) if (message) notices.push(`<div class="k8gb-notice">${icon('triangle-alert')}<strong>${name} unavailable.</strong><span>${h(message)}${old ? ' Displayed metadata may be stale.' : ''}</span></div>`);
    if (state.namespaceWarnings?.length) notices.push(`<div class="k8gb-notice">${icon('triangle-alert')}<div>${warningList(state.namespaceWarnings)}</div></div>`);
    const unavailable = C.list(old?.sites).filter(item => !item.available);
    if (unavailable.length) notices.push(`<div class="k8gb-notice">${icon('triangle-alert')}<strong>Partial snapshot.</strong><span>Read unavailable: ${unavailable.map(item => h(item.cluster?.name || item.cluster?.id)).join(', ')}.</span></div>`);
    $('notices').innerHTML = notices.join('');
    $('snapshotTime').textContent = old ? `${state.overviewError ? 'Stale / ' : ''}${C.formatTime(old.collectedAt)}` : 'No snapshot';
    $('snapshotScope').textContent = old ? `Snapshot namespace: ${old.namespace || 'all namespaces'}` : '';
    $('demoBadge').hidden = !state.demo;
    $('refresh').setAttribute('aria-busy', String(state.loading || state.metadataLoading));
    const editable = M.hasEdits(mutationContext());
    $('accessBadge').innerHTML = `${icon(editable ? 'shield-check' : 'lock-keyhole')}${editable ? 'Admin edits' : 'Read-only'}`;
    $('mutationActions').innerHTML = `${M.hasEdits(mutationContext(),{native:['create']}) ? `<button type="button" class="k8gb-button" data-create-native>${icon('plus')}Create native GSLB</button>` : ''}${M.hasEdits(mutationContext(),{traefik:['enable']}) ? `<button type="button" class="k8gb-button" data-enable-traefik>${icon('webhook')}Create GSLB from IngressRoute</button>` : ''}`;
    $('audit').disabled = !state.namespace || state.namespace !== state.overview?.namespace || state.loading;
  }
  function renderCatalog() {
    const groups = C.list(state.overview?.groups), visible = C.filterGroups(groups, {query:$('search').value, warnings:$('warningsFilter').checked, dns:$('dnsFilter').checked});
    $('groupCount').textContent = `${visible.length} / ${groups.length}`;
    $('signalCounts').innerHTML = `${badge(C.countLabel(groups.filter(C.hasWarnings).length, 'warning'), 'warning')}${badge(`${groups.filter(group => statusFor(group).tone === 'unknown').length} unknown`, 'unknown')}${badge(C.countLabel(groups.filter(group => statusFor(group).tone === 'info').length, 'DNS difference'), 'info')}`;
    document.querySelectorAll('[data-view]').forEach(button => button.setAttribute('aria-pressed', String(button.dataset.view === state.view)));
    const container = $('resources'); container.className = state.view === 'rows' ? 'k8gb-row-view' : 'k8gb-grid';
    container.setAttribute('aria-busy', String(state.loading));
    if (!visible.length) {
      const message = !state.overview ? state.loading || state.metadataLoading ? 'Loading snapshots...' : state.namespace === null && !state.namespaceError ? 'No accessible namespaces' : 'No snapshot available' : groups.length ? 'No resources match the selected filters' : C.list(state.overview.sites).some(item => !item.available) ? 'No resources received. Cluster reads are incomplete.' : 'No GSLB resources in this namespace';
      container.innerHTML = `<p class="k8gb-empty">${message}</p>`; return;
    }
    container.innerHTML = state.view === 'rows' ? `<table class="k8gb-table k8gb-object-table"><caption class="k8gb-sr-only">GSLB resources</caption><thead><tr><th>GSLB / URL</th><th>Management</th><th>State</th><th>Applications</th><th>Policy / TTL</th><th>DNS targets</th></tr></thead><tbody>${visible.map(group => {
      const status = statusFor(group), policy = policySummary(group);
      return `<tr class="k8gb-object-row ${status.tone}"><td data-label="GSLB / URL">${groupLink(group)}<small>${h(group.hostname || 'Hostname not reported')}</small><small>${h(group.namespace)}</small></td><td data-label="Management">${managementSummary(group)}</td><td data-label="State">${badge(status.label, status.tone)}${signalsHTML(group)}</td><td data-label="Applications"><div class="k8gb-row-sites">${chipsHTML(group)}</div></td><td data-label="Policy / TTL">${h(policy.strategy)}<small>TTL ${h(policy.ttl)}</small></td><td data-label="DNS targets"><div class="k8gb-dns-targets">${allTargetsHTML(group)}</div></td></tr>`;
    }).join('')}</tbody></table>` : visible.map(group => {
      const status = statusFor(group), policy = policySummary(group);
      return `<article class="k8gb-tile ${status.tone}"><div class="k8gb-tile-main"><div class="k8gb-tile-status">${badge(status.label, status.tone)}<span class="k8gb-backend-count">${icon('boxes')}${h(endpointLabel(group))}</span></div>${groupLink(group)}<div class="k8gb-host">${h(group.hostname || 'Hostname not reported')}</div>${managementSummary(group)}${signalsHTML(group)}<div class="k8gb-row-sites">${chipsHTML(group)}</div><div class="k8gb-tile-config"><span>${icon('git-branch')}${h(policy.strategy)}</span><span>${icon('timer')}TTL ${h(policy.ttl)}</span></div><div class="k8gb-dns-targets"><span class="k8gb-muted">DNS</span>${allTargetsHTML(group)}</div></div><div class="k8gb-tile-footer"><span>${h(group.namespace)}${group.zone ? ` / ${h(group.zone)}` : ''}</span><a class="k8gb-detail-link" href="${h(C.groupHash(group.id))}" title="Open resource details" aria-label="Details for ${h(names(group))}">${icon('chevron-right')}</a></div></article>`;
    }).join('');
  }
  function groupLink(group) { return `<a class="k8gb-name" data-group-id="${h(group.id)}" href="${h(C.groupHash(group.id))}">${h(names(group))}</a>`; }
  function referenceHTML(reference, label) {
    if (!reference) return `<div class="k8gb-reference-block"><h4>${label}</h4><p class="k8gb-muted">Not reported</p></div>`;
    return `<div class="k8gb-reference-block"><h4>${label}</h4><dl class="k8gb-reference">${[['API version', reference.apiVersion], ['Kind', reference.kind], ['Namespace', reference.namespace], ['Name', reference.name], ['UID', reference.uid], ['Resource version', reference.resourceVersion]].map(([key,value]) => `<dt>${key}</dt><dd><code>${h(value || 'Not reported')}</code></dd>`).join('')}</dl></div>`;
  }
  function policyHTML(policy) {
    return `<dl class="k8gb-reference"><dt>Strategy</dt><dd>${h(C.policyLabel(policy))}</dd><dt>DNS TTL</dt><dd>${h(C.ttlLabel(policy))}</dd><dt>Primary GeoTag</dt><dd>${h(policy?.primaryGeoTag || 'Not reported')}</dd><dt>Weights</dt><dd>${Object.entries(policy?.weights || {}).map(([tag,weight]) => `<span class="k8gb-weight"><code>${h(tag)}</code>: ${h(weight)}</span>`).join('') || 'Not reported'}</dd></dl>`;
  }
  function trafficHTML(instance, index, group) {
    const traffic = instance.traffic;
    const actions = mutations.trafficActions(instance,index,group);
    if (!traffic && !C.isExcluded(instance) && !actions) return '';
    return `<div class="k8gb-traffic"><h4>Operational traffic</h4><dl class="k8gb-reference"><dt>Site traffic</dt><dd>${badge(C.isExcluded(instance) ? 'Excluded' : 'Not excluded', C.isExcluded(instance) ? 'info' : 'consistent')}</dd><dt>Configured IPs</dt><dd>${C.list(traffic?.ips).map(ip => `<code>${h(ip)}</code>`).join(', ') || 'None configured'}</dd><dt>Exposed hostnames</dt><dd>${C.list(traffic?.hostnames).map(hostname => `<code>${h(hostname)}</code>`).join(', ') || 'None configured'}</dd><dt>IP snapshot</dt><dd>${traffic?.overrideConflict ? 'Conflict: external annotation changes' : traffic?.overrideManaged ? 'Managed snapshot available' : 'No managed snapshot'}</dd></dl>${traffic?.overrideConflict ? '<p class="k8gb-notice">External annotation changes detected. Reset is disabled. Inspect the live exposed-IP and exposed-hostnames annotations and managed snapshot before refreshing.</p>' : ''}${actions}</div>`;
  }
  function overviewHTML(group) {
    return `${signalsHTML(group, true)}${C.list(group.instances).map((instance,index) => {
      const health = effectiveHealth(instance), currentSite = site(instance.clusterId), backends = C.list(instance.backends);
      return `<section class="k8gb-section"><div class="k8gb-instance-heading"><h3>${h(clusterLabel(instance.clusterId))}</h3>${badge(health.label, health.tone)}<span class="k8gb-muted">GeoTag: ${h(instance.geoTag || 'Not reported')}</span></div>${managementDetails(instance)}<div class="k8gb-instance-policy"><span>${h(C.policyLabel(instance.policy))}</span><span>TTL ${h(C.ttlLabel(instance.policy))}</span><div class="k8gb-dns-targets"><span class="k8gb-muted">DNS</span>${targetsHTML(instance)}</div></div>${trafficHTML(instance,index,group)}${warningList(instance.warnings)}<h4>Backends</h4>${backends.length ? `<table class="k8gb-table k8gb-backend-table"><thead><tr><th>Service</th><th>Health</th><th>Ready / total endpoints</th></tr></thead><tbody>${backends.map(backend => { const current = C.health(backend.health); return `<tr><td data-label="Service"><code>${h(backend.namespace)}/${h(backend.service)}${backend.port ? `:${h(backend.port)}` : ''}</code></td><td data-label="Health">${badge(current.label,current.tone)}</td><td data-label="Endpoints">${backend.endpointDataAvailable === true ? `${h(backend.readyEndpoints)} / ${h(backend.totalEndpoints)}` : '<span class="k8gb-muted">Unknown / not available</span>'}</td></tr>`; }).join('')}</tbody></table>` : '<p class="k8gb-muted">Backends not reported</p>'}<details class="k8gb-technical"><summary>Resource references / snapshot ${h(C.formatTime(currentSite?.collectedAt))}</summary><div class="k8gb-resource-references">${referenceHTML(instance.resource, 'GSLB resource')}${referenceHTML(instance.target, 'Target reference')}${instance.source ? referenceHTML(instance.source, 'Source IngressRoute') : ''}</div></details></section>`;
    }).join('') || '<p class="k8gb-empty">No instances reported</p>'}`;
  }
  function renderDetail() {
    const group = C.list(state.overview?.groups).find(item => item.id === state.selected);
    if (!group) { dns.unmount(); $('detail').innerHTML = `<a class="k8gb-back" href="#">${icon('arrow-left')}Resources</a><h2 id="detailHeading" tabindex="-1">${state.loading || state.metadataLoading ? 'Loading resource...' : 'Resource not in this snapshot'}</h2><p class="k8gb-muted k8gb-mono">${h(state.selected)}</p>`; return; }
    const status = statusFor(group), instances = C.list(group.instances);
    state.policyIndex = Math.max(0, Math.min(state.policyIndex, instances.length - 1));
    $('detail').innerHTML = `<a class="k8gb-back" href="#">${icon('arrow-left')}Resources</a><div class="k8gb-detail-heading"><div><h2 id="detailHeading" tabindex="-1">${h(names(group))}</h2><p>${h(group.hostname || 'Hostname not reported')} / ${h(group.namespace)}</p></div><div class="k8gb-detail-state">${managementSummary(group)}${badge(status.label,status.tone)}</div></div><div class="k8gb-tabs" role="tablist" aria-label="Resource details"><button class="k8gb-tab" type="button" id="overviewTab" data-tab="overview" role="tab" aria-selected="${state.tab === 'overview'}" aria-controls="detailPanel" tabindex="${state.tab === 'overview' ? 0 : -1}">Overview</button><button class="k8gb-tab" type="button" id="policyTab" data-tab="policy" role="tab" aria-selected="${state.tab === 'policy'}" aria-controls="detailPanel" tabindex="${state.tab === 'policy' ? 0 : -1}">DNS Policy</button></div><div id="detailPanel" role="tabpanel" aria-labelledby="${state.tab === 'overview' ? 'overviewTab' : 'policyTab'}" tabindex="0">${state.tab === 'overview' ? overviewHTML(group) : instances.length ? `<div class="k8gb-policy-layout"><section class="k8gb-section"><label for="policyCluster" class="k8gb-label">Reported policy</label><select id="policyCluster">${instances.map((instance,index) => `<option value="${index}"${index === state.policyIndex ? ' selected' : ''}>${h(clusterLabel(instance.clusterId))} / ${h(instance.resource?.name || instance.clusterId)}</option>`).join('')}</select><h3 class="k8gb-policy-heading">${h(instances[state.policyIndex].resource?.name || 'Policy')}</h3>${policyHTML(instances[state.policyIndex].policy)}<h4>Observed DNS targets</h4><div class="k8gb-dns-targets">${targetsHTML(instances[state.policyIndex])}</div></section>${dns.html()}</div>` : '<p class="k8gb-empty">No policy reported</p>'}</div>`;
    if (state.tab === 'policy' && instances.length) dns.update($('k8gbPolicyDNS'), C.dnsInput(group, instances[state.policyIndex]));
    else dns.unmount();
    $('detailPanel').insertAdjacentHTML('afterbegin', mutations.actions(group));
    mutations.prepareTraffic(group);
  }
  function render() {
    renderSites(); renderNotices(); renderCatalog();
    syncView();
    if (!state.selected) dns.unmount();
  }
  async function loadOverview(namespace) {
    if (!namespace) return;
    state.loading = true; state.overviewError = ''; mutations.contextChanged(); render();
    await requests.overview.run(async signal => {
      const data = C.validateOverview(await C.getJSON(`/api/v1/overview?${new URLSearchParams({namespace})}`, {signal}));
      if (data.namespace !== namespace) throw new Error('Snapshot namespace does not match the requested namespace');
      return data;
    }, data => { state.overview = data; state.overviewError = ''; state.loading = false; render(); }, error => { state.overviewError = error.message || 'Unable to read overview'; state.loading = false; render(); });
  }
  async function loadClusters() {
    await requests.clusters.run(async signal => {
      const data = await C.getJSON('/api/v1/clusters', {signal});
      if (!data || typeof data.readOnly !== 'boolean' || !Array.isArray(data.clusters) && data.clusters != null) throw new Error('Invalid clusters response');
      return data;
    }, data => { state.clusters = C.list(data.clusters); state.demo = data.demo === true; state.clusterError = ''; $('logout').hidden = !['oidc','builtin'].includes(data.authMode); render(); }, error => { state.clusterError = error.message || 'Unable to read clusters'; render(); });
  }
  async function loadNamespaces() {
    await requests.namespaces.run(async signal => {
      const data = await C.getJSON('/api/v1/namespaces', {signal});
      if (!data || !Array.isArray(data.namespaces) && data.namespaces != null) throw new Error('Invalid namespaces response');
      return data;
    }, data => {
      state.namespaces = C.unique(data.namespaces).sort((a,b) => a.localeCompare(b)); state.namespaceWarnings = C.list(data.warnings); state.namespaceError = '';
      if (state.namespace === null) { state.namespace = C.initialNamespace(state.namespaces); if (state.namespace !== null) loadOverview(state.namespace); }
      renderNamespaces(); render();
    }, error => { state.namespaceError = error.message || 'Unable to read namespaces'; renderNamespaces(); render(); });
  }
  async function refresh() {
    const sequence = ++refreshSequence;
    state.metadataLoading = true; render();
    const pending = [loadClusters(), loadNamespaces()];
    if (state.namespace !== null) pending.push(loadOverview(state.namespace));
    await Promise.all(pending);
    if (sequence === refreshSequence) { state.metadataLoading = false; render(); }
  }
  $('refresh').addEventListener('click', refresh);
  $('audit').addEventListener('click', () => mutations.openAudit());
  $('namespace').addEventListener('change', event => { state.namespace = event.target.value; loadOverview(state.namespace); });
  $('search').addEventListener('input', renderCatalog);
  $('warningsFilter').addEventListener('change', renderCatalog); $('dnsFilter').addEventListener('change', renderCatalog);
  $('theme').addEventListener('click', () => {
    const theme = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = theme;
    try { localStorage.setItem('k8gb-ui-theme', theme); } catch (_) { /* Optional preference. */ }
    renderTheme();
  });
  document.addEventListener('click', event => {
    if (event.target.closest('[data-create-native]')) mutations.open('native-create');
    if (event.target.closest('[data-enable-traefik]')) mutations.open('traefik-enable');
    const group = C.list(state.overview?.groups).find(item => item.id === state.selected);
    if (event.target.closest('[data-mutation-action="native-update"]') && group) mutations.open('native-update',group);
    const source = event.target.closest('[data-traefik-instance]');
    if (source && group) mutations.open('traefik-settings',group,C.list(group.instances)[Number(source.dataset.traefikInstance)]);
    const traffic = event.target.closest('[data-traffic-instance]');
    if (traffic && group && !traffic.disabled) mutations.open(traffic.dataset.trafficMode,group,C.list(group.instances)[Number(traffic.dataset.trafficInstance)]);
    const view = event.target.closest('[data-view]');
    if (view) { state.view = view.dataset.view; try { localStorage.setItem('k8gb-ui-view', state.view); } catch (_) { /* Optional preference. */ } renderCatalog(); }
    const tab = event.target.closest('[data-tab]');
    if (tab) { state.tab = tab.dataset.tab; renderDetail(); $(state.tab === 'overview' ? 'overviewTab' : 'policyTab').focus(); }
  });
  $('detail').addEventListener('change', event => { if (event.target.id === 'policyCluster') { state.policyIndex = Number(event.target.value); renderDetail(); $('policyCluster').focus(); } });
  $('detail').addEventListener('keydown', event => {
    if (event.target.matches('[role="tab"]') && ['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) {
      event.preventDefault(); state.tab = event.key === 'Home' ? 'overview' : event.key === 'End' ? 'policy' : state.tab === 'overview' ? 'policy' : 'overview';
      renderDetail(); $(state.tab === 'overview' ? 'overviewTab' : 'policyTab').focus();
    }
  });
  window.addEventListener('hashchange', () => {
    if (mutations.active && !mutations.close()) return;
    const next = C.groupFromHash(location.hash);
    if (next !== state.selected) { returnGroup = state.selected || next; state.tab = 'overview'; state.policyIndex = 0; }
    state.selected = next; render();
    if (next) $('detailHeading')?.focus();
    else [...document.querySelectorAll('[data-group-id]')].find(element => element.dataset.groupId === returnGroup)?.focus();
  });
  window.addEventListener('pagehide', () => { Object.values(requests).forEach(request => request.cancel()); dns.unmount(); mutations.destroy(); });
  window.addEventListener('pageshow', event => { if (event.persisted) refresh(); });
  window.lucide?.createIcons(); renderTheme(); refresh();
})();
