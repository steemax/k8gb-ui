(function (root, factory) {
  'use strict';
  if (typeof module === 'object' && module.exports) module.exports = factory();
  else root.K8gbDNS = factory();
})(typeof window === 'object' ? window : globalThis, function () {
  'use strict';
  // Extracted from the original GSLB DNS visualizer; playback is illustrative,
  // Policy inputs may come from a reported snapshot or the unsaved editor form.
  const modes = {roundRobin:'Round robin', weightedRoundRobin:'Weighted round robin', failover:'Failover', geoip:'GeoIP'};
  const escape = value => String(value ?? '').replace(/[&<>"']/g,c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const icon = name => typeof window === 'object' && window.lucide ? window.lucide.createElement(window.lucide.icons[name === 'play' ? 'Play' : 'Pause'], {'aria-hidden':'true', width:16, height:16}).outerHTML : '';
  function normalize(input) {
    const tags = [...new Set((input.tags || []).filter(tag => typeof tag === 'string' && tag))];
    const excludedTags = tags.filter(tag => Array.isArray(input.excludedTags) && input.excludedTags.includes(tag));
    const value = {context:String(input.context || ''), hostname:String(input.hostname || ''), cod:String(input.cod || ''), tags, excludedTags,
      mode:input.mode, primary:String(input.primary || ''), ttl:String(input.ttl ?? '').trim(), draft:input.draft === true, weights:{}};
    for (const tag of tags) value.weights[tag] = String(Object.hasOwn(input.weights || {},tag) ? input.weights[tag] : '0').trim();
    let error = '';
    if (!Object.hasOwn(modes,value.mode)) error = 'This policy strategy has no DNS model.';
    else if (!tags.length) error = 'GeoTags were not reported for this group.';
    else if (value.mode === 'failover' && !tags.includes(value.primary)) error = 'The primary GeoTag is not present in this snapshot.';
    else if (value.mode === 'weightedRoundRobin' && (tags.some(tag => !/^\d+$/.test(value.weights[tag]) || Number(value.weights[tag]) > 2147483647) || Object.keys(input.weights || {}).some(tag => !tags.includes(tag)) || !tags.some(tag => Number(value.weights[tag]) > 0))) error = 'Reported weights must be nonnegative integers, with at least one positive weight and no missing GeoTags.';
    else if (value.ttl && (!/^\d+$/.test(value.ttl) || Number(value.ttl) < 1 || Number(value.ttl) > 2147483647)) error = 'The reported DNS TTL is outside the supported range.';
    return {...value,error};
  }
  const eligibleTags = config => config.tags.filter(tag => !config.excludedTags.includes(tag) && (config.mode !== 'weightedRoundRobin' || Number(config.weights[tag]) > 0));
  // A deterministic illustration of weighted ordering, not a DNS/traffic forecast.
  function nextWeighted(config, scores = []) {
    const weights = config.tags.map(tag => config.excludedTags.includes(tag) ? 0 : Number(config.weights[tag]) || 0), total = weights.reduce((sum,n) => sum+n,0);
    const next = weights.map((weight,i) => (scores[i] || 0)+weight);
    let first = -1;
    next.forEach((score,i) => { if (weights[i] > 0 && (first < 0 || score > next[first])) first = i; });
    if (first >= 0) next[first] -= total;
    return {first:first < 0 ? '' : config.tags[first],scores:next};
  }
  function frame(config, step, weightedFirst) {
    const c = config, rotate = tags => tags.map((_,i) => tags[(i+step)%tags.length]);
    if (c.error) return {phase:'invalid',title:'Model unavailable',copy:c.error,selected:[],answer:[],moving:false,duration:2200};
    const eligible = eligibleTags(c);
    if (!eligible.length) return {phase:'empty',title:'No eligible sites',copy:c.excludedTags.length === c.tags.length ? 'All sites are manually excluded. No new site records are modeled; existing DNS caches may still contain old records.' : 'The remaining sites have no positive weight. No site records are modeled.',selected:[],answer:[],moving:false,duration:3200};
    if (c.mode === 'failover') {
      const primary = c.primary, reserve = eligible.find(tag => tag !== primary);
      if (c.excludedTags.includes(primary)) return {phase:'primary-excluded',title:`Primary ${primary} is excluded`,copy:`The model uses ${reserve}. The primary remains excluded until it is explicitly included again; no simulated failback to it occurs.`,selected:[reserve],answer:[reserve],moving:true,duration:3200};
      if (!reserve) return {phase:'single',title:`Primary ${primary}`,copy:'Only one eligible GeoTag remains. No included failover destination is available in the model.',selected:[primary],answer:[primary],moving:true,duration:3200};
      const phases = [
        {phase:'healthy',title:`Primary ${primary} is healthy`,copy:`DNS returns only ${primary}. ${reserve} is on standby.`,selected:[primary],answer:[primary],duration:3200},
        {phase:'failure',title:`Simulated failure of ${primary}`,copy:`After detection, DNS returns ${reserve}; the resolver still caches ${primary}.`,selected:[reserve],answer:[primary],down:primary,cache:true,duration:3000},
        {phase:'cache',title:'Waiting for cache expiry',copy:`The old ${primary} record may remain until its TTL expires. Time is accelerated.`,selected:[reserve],answer:[primary],down:primary,cache:true,duration:3000},
        {phase:'reserve',title:`Failover to ${reserve}`,copy:'The resolver refreshes its cache with the standby site records.',selected:[reserve],answer:[reserve],down:primary,duration:3600},
        {phase:'recovery',title:`Primary ${primary} recovers`,copy:`DNS returns ${primary} again; the resolver still caches ${reserve}.`,selected:[primary],answer:[reserve],cache:true,duration:3000},
        {phase:'failback',title:`Failback to ${primary}`,copy:`The resolver receives ${primary} records. ${reserve} returns to standby.`,selected:[primary],answer:[primary],duration:3600}
      ];
      const phase = step%phases.length;
      return {...phases[phase],index:phase,moving:!phases[phase].cache};
    }
    if (c.mode === 'geoip') {
      const subnet = Math.floor(step/2)%(eligible.length+1), fallback = subnet === eligible.length;
      const selected = fallback ? rotate(eligible) : [eligible[subnet]];
      return {phase:fallback ? 'fallback' : 'subnet',title:fallback ? 'Subnet outside the GeoIP database' : `Example subnet to ${selected[0]}`,
        copy:fallback ? 'Fallback returns records for all eligible modeled sites.' : 'Illustrative subnet-to-GeoTag mapping, not the cluster GeoIP database.',
        client:fallback ? 'Unmatched subnet' : `Example subnet ${subnet+1}`,selected,answer:selected,moving:true,duration:2400};
    }
    const first = c.mode === 'weightedRoundRobin' ? eligible.includes(weightedFirst) ? weightedFirst : nextWeighted(c).first : eligible[step%eligible.length];
    const selected = [first,...eligible.filter(tag => tag !== first)];
    return {phase:'answer',title:`First site in the answer: ${first}`,copy:c.mode === 'weightedRoundRobin' ? 'Record ordering follows the policy weights of eligible sites only.' : 'Each answer contains all eligible modeled sites in rotating order.',
      selected,answer:selected,moving:true,duration:2200};
  }
  function html(id = 'k8gbPolicyDNS') {
    return `<aside class="k8gb-dns" id="${escape(id)}" aria-label="Illustrative DNS policy model"><div class="k8gb-dns-heading"><div><div class="k8gb-dns-eyebrow">DNS policy model <span class="k8gb-local">Simulation</span></div><h3 data-dns-hostname></h3></div><button type="button" class="k8gb-button k8gb-icon-button" data-dns-pause aria-label="Pause animation" title="Pause animation">${icon('pause')}</button></div>
      <div class="k8gb-dns-meta"><span data-dns-playback></span><span data-dns-mode></span><span data-dns-ttl></span><span>Accelerated time</span></div><div class="k8gb-dns-source" data-dns-source></div><div class="k8gb-dns-exclusions" data-dns-exclusions hidden></div>
      <div class="k8gb-dns-phase"><div><strong data-dns-title></strong><span data-dns-step></span></div><p data-dns-copy></p></div><div data-dns-stage></div>
      <div class="k8gb-dns-answer"><div class="k8gb-dns-answer-heading"><span data-dns-answer-source></span><span data-dns-answer-kind></span></div><div class="k8gb-dns-answer-tags" data-dns-answer></div></div>
      <div class="k8gb-dns-stats" data-dns-stats></div><p class="k8gb-dns-caption" data-dns-caption></p></aside>`;
  }
  function createView() {
    let host = null, config = null, signature = '', step = 0, progress = 0, scores = [], first = '', paintedStep = -1;
    let running = false, paused = false, visible = true, raf = 0, last = 0, controller, resize, intersection;
    const reduced = window.matchMedia('(prefers-reduced-motion: reduce)');
    const $ = selector => host.querySelector(selector);
    function stop() { cancelAnimationFrame(raf); running = false; last = 0; }
    function detach() { stop(); controller?.abort(); resize?.disconnect(); intersection?.disconnect(); host = null; }
    function unmount() { detach(); signature = ''; config = null; paused = false; }
    function draw() {
      const stage = $('[data-dns-stage]'), width = Math.max(160,stage.clientWidth), center = width/2;
      const columns = Math.min(config.tags.length || 1,Math.max(1,Math.min(4,Math.floor((width-16)/76))));
      const cell = (width-16)/columns, nodeWidth = cell-8;
      const maxChars = Math.max(3,Math.floor((nodeWidth-12)/7));
      const chunks = config.tags.map(tag => tag.match(new RegExp(`.{1,${maxChars}}`,'gu')) || ['']);
      const rowHeights = [];
      chunks.forEach((parts,i) => { const row = Math.floor(i/columns); rowHeights[row] = Math.max(rowHeights[row] || 0,parts.length*14+42); });
      const tops = [242]; rowHeights.forEach((height,i) => { tops[i+1] = tops[i]+height+26; });
      const nodes = config.tags.map((tag,i) => {
        const row = Math.floor(i/columns), count = Math.min(columns,config.tags.length-row*columns);
        return {tag,i,x:center+(i%columns-(count-1)/2)*cell,y:tops[row],height:rowHeights[row],row};
      });
      const height = config.tags.length ? tops[rowHeights.length]-16 : 242;
      stage.innerHTML = `<svg class="k8gb-dns-graphic" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="DNS model for ${escape(config.hostname || 'the selected GSLB')}"><title>Illustrative DNS answer model</title><desc>Modeled records are labeled by GeoTag, without invented IP addresses. This is not a cluster availability check or HTTP traffic.</desc>
        <path class="k8gb-dns-wire" data-dns-query d="M ${center} 64 L ${center} 116"></path>
        ${nodes.map(node => `<path class="k8gb-dns-wire k8gb-dns-series-${node.i%2}" data-dns-path="${node.i}" d="${node.row === 0 ? `M ${node.x} ${node.y} C ${node.x} 210 ${center} 217 ${center} 183` : `M ${node.x} ${node.y} C ${node.x} ${node.y-15} ${node.x < center ? 3 : width-3} ${node.y-15} ${node.x < center ? 3 : width-3} ${node.y-26} L ${node.x < center ? 3 : width-3} 211 Q ${center} 211 ${center} 183`}"></path>`).join('')}
        <rect class="k8gb-dns-node" x="${center-76}" y="16" width="152" height="48" rx="5"></rect><text x="${center}" y="36" text-anchor="middle">DNS resolver</text><text class="k8gb-dns-sub" data-dns-client x="${center}" y="53" text-anchor="middle"></text>
        <text class="k8gb-dns-sub" x="${center+9}" y="94">DNS</text><rect class="k8gb-dns-server" x="${center-Math.min(212,width-16)/2}" y="116" width="${Math.min(212,width-16)}" height="67" rx="6"></rect><text class="k8gb-dns-node-title" x="${center}" y="143" text-anchor="middle">k8gb / CoreDNS</text><text class="k8gb-dns-sub" data-dns-strategy x="${center}" y="164" text-anchor="middle"></text>
        ${nodes.map(node => `<rect class="k8gb-dns-site k8gb-dns-series-${node.i%2}" data-dns-node="${node.i}" x="${node.x-nodeWidth/2}" y="${node.y}" width="${nodeWidth}" height="${node.height}" rx="5"></rect><text class="k8gb-dns-tag" x="${node.x}" y="${node.y+22}" text-anchor="middle">${chunks[node.i].map((part,j) => `<tspan x="${node.x}" dy="${j ? 14 : 0}">${escape(part)}</tspan>`).join('')}</text><text class="k8gb-dns-sub" data-dns-health="${node.i}" x="${node.x}" y="${node.y+node.height-12}" text-anchor="middle"></text>`).join('')}
        <circle class="k8gb-dns-packet" data-dns-packet r="5" hidden></circle></svg>`;
      paintedStep = -1;
    }
    function status(s) {
      host.dataset.dnsPhase = s.phase; host.dataset.dnsStep = step;
      $('[data-dns-title]').textContent = s.title;
      $('[data-dns-title]').classList.toggle('k8gb-dns-warning',Boolean(s.down) || Boolean(config.error));
      $('[data-dns-copy]').textContent = s.copy;
      $('[data-dns-step]').textContent = s.index == null ? `#${step+1}` : `${s.index+1} / 6`;
      const cached = s.answer.join(', ');
      $('[data-dns-client]').textContent = s.cache ? 'Previous DNS cache' : s.client || `DNS query #${step+1}`;
      $('[data-dns-strategy]').textContent = modes[config.mode] || 'Unknown strategy';
      config.tags.forEach((tag,i) => {
        const path = $(`[data-dns-path="${i}"]`), node = $(`[data-dns-node="${i}"]`), health = $(`[data-dns-health="${i}"]`);
        const excluded = config.excludedTags.includes(tag), selected = !excluded && s.selected.includes(tag);
        path.classList.toggle('active',selected); path.classList.toggle('excluded',excluded); path.style.opacity = selected ? s.selected[0] === tag ? '1' : '.4' : excluded ? '.35' : '.2';
        node.classList.toggle('excluded',excluded); node.dataset.excluded = String(excluded);
        node.classList.toggle('down',!excluded && s.down === tag); health.classList.toggle('k8gb-dns-warning',!excluded && s.down === tag);
        health.textContent = excluded ? config.mode === 'failover' && tag === config.primary ? 'Primary / Excluded' : 'Excluded' : s.down === tag ? 'Model: down' : config.mode === 'failover' ? tag === config.primary ? 'Primary' : selected ? 'Active' : 'Standby' : selected ? 'In answer' : Number(config.weights[tag]) === 0 && config.mode === 'weightedRoundRobin' ? 'Weight 0' : 'Eligible';
      });
      $('[data-dns-answer-source]').textContent = s.cache ? 'Resolver cache' : 'Modeled answer / GeoTag';
      $('[data-dns-answer-kind]').textContent = s.cache ? 'Previous record' : s.answer.length > 1 ? 'Record ordering' : s.answer.length ? 'One site' : 'No modeled answer';
      $('[data-dns-answer]').innerHTML = s.answer.map((tag,i) => `<span class="k8gb-dns-answer-tag k8gb-dns-series-${config.tags.indexOf(tag)%2}"><span>${i+1}</span>${escape(tag)}</span>`).join('') || `<span class="k8gb-muted">${config.error ? 'Policy data unavailable' : 'No eligible site records'}</span>`;
      if (config.mode === 'failover') $('[data-dns-stats]').innerHTML = `<span class="k8gb-muted">Modeled DNS</span> <strong>${escape(s.selected.join(', ') || (config.error ? 'Not reported' : 'None'))}</strong>${s.cache ? `<small>Resolver cache: ${escape(cached)}</small>` : `<small>Primary ${escape(config.primary)}${config.excludedTags.includes(config.primary) ? ' / Excluded' : ''}</small>`}`;
      if (config.mode === 'geoip') $('[data-dns-stats]').textContent = 'Scenario: illustrative site subnets and fallback.';
    }
    function paint() {
      if (!host || !config) return;
      const s = frame(config,step,first);
      if (paintedStep !== step) { status(s); paintedStep = step; }
      const packet = $('[data-dns-packet]'), moving = running && s.moving && !reduced.matches;
      packet.toggleAttribute('hidden',!moving);
      if (!moving) return;
      const p = progress, index = config.tags.indexOf(s.selected[0]);
      const path = p < .28 || p >= .72 ? $('[data-dns-query]') : $(`[data-dns-path="${index}"]`);
      if (!path) { packet.toggleAttribute('hidden',true); return; }
      const t = p < .28 ? p/.28 : p < .72 ? (p-.28)/.44 : 1-(p-.72)/.28;
      const point = path.getPointAtLength(Math.max(0,Math.min(1,t))*path.getTotalLength());
      packet.setAttribute('cx',point.x); packet.setAttribute('cy',point.y);
      packet.classList.toggle('k8gb-dns-series-1',p >= .28 && index%2 === 1);
    }
    function playback() {
      if (!host) return;
      const empty = !eligibleTags(config).length;
      const button = $('[data-dns-pause]'), label = config.error ? 'Model unavailable' : empty ? 'No eligible sites' : reduced.matches ? 'Animation disabled by reduced-motion preference' : paused ? 'Resume animation' : 'Pause animation';
      button.disabled = Boolean(config.error) || empty || reduced.matches; button.setAttribute('aria-label',label); button.title = label;
      button.setAttribute('aria-pressed',String(paused || reduced.matches)); button.innerHTML = icon(paused || reduced.matches ? 'play' : 'pause');
      $('[data-dns-playback]').textContent = config.error ? 'Unavailable' : empty ? 'No eligible sites' : paused || reduced.matches ? 'Paused' : 'Autoplay';
      $('[data-dns-playback]').classList.toggle('paused',paused || empty || Boolean(config.error));
    }
    function tick(timestamp) {
      if (!running || !host?.isConnected) { stop(); return; }
      if (!last) last = timestamp;
      progress += Math.min(80,timestamp-last)/frame(config,step,first).duration; last = timestamp;
      if (progress >= 1) {
        progress -= 1; step++;
        if (config.mode === 'weightedRoundRobin' && !config.error) { const next = nextWeighted(config,scores); scores = next.scores; first = next.first; }
      }
      paint(); raf = requestAnimationFrame(tick);
    }
    function syncPlayback() {
      if (!host) return;
      const shouldRun = !paused && !reduced.matches && !config.error && eligibleTags(config).length > 0 && visible && !document.hidden;
      if (shouldRun && !running) { running = true; last = 0; raf = requestAnimationFrame(tick); }
      if (!shouldRun) stop();
      playback(); paint();
    }
    function update(nextHost, input) {
      if (!nextHost) { unmount(); return; }
      const next = normalize(input), nextSignature = JSON.stringify(next), changed = nextSignature !== signature;
      if (changed) {
        stop(); config = next; signature = nextSignature; step = 0; progress = 0; scores = []; first = ''; paused = false; paintedStep = -1;
        if (config.mode === 'weightedRoundRobin' && !config.error) { const next = nextWeighted(config); scores = next.scores; first = next.first; }
      }
      const rebound = host !== nextHost;
      if (rebound) {
        detach(); host = nextHost; visible = true; controller = new AbortController();
        host.addEventListener('click',event => { if (event.target.closest('[data-dns-pause]') && !config.error) { paused = !paused; syncPlayback(); } },{signal:controller.signal});
        document.addEventListener('visibilitychange',syncPlayback,{signal:controller.signal});
        reduced.addEventListener('change',syncPlayback,{signal:controller.signal});
        resize = new ResizeObserver(() => { if (host) { draw(); paint(); } }); resize.observe($('[data-dns-stage]'));
        intersection = new IntersectionObserver(entries => { if (entries[0].target === host) { visible = entries[0].isIntersecting; syncPlayback(); } }); intersection.observe(host);
      }
      if (changed || rebound) {
        host.dataset.dnsContext = config.context; host.dataset.dnsCod = config.cod; host.dataset.dnsExcluded = JSON.stringify(config.excludedTags);
        $('[data-dns-hostname]').innerHTML = config.hostname ? config.hostname.split('.').map(escape).join('.<wbr>') : 'Hostname not reported';
        $('[data-dns-mode]').textContent = modes[config.mode] || String(config.mode || 'Unknown strategy');
        $('[data-dns-ttl]').textContent = config.ttl ? `TTL ${config.ttl} s` : 'TTL not reported';
        $('[data-dns-source]').textContent = `${config.draft ? 'Unsaved form' : 'Reported policy'} / ${config.cod || 'cluster not reported'}`;
        $('[data-dns-exclusions]').hidden = !config.excludedTags.length;
        $('[data-dns-exclusions]').textContent = `Excluded from modeled answers: ${config.excludedTags.join(', ')}`;
        const eligible = eligibleTags(config), total = eligible.reduce((sum,tag) => sum+(Number(config.weights[tag]) || 0),0);
        $('[data-dns-stats]').innerHTML = config.mode === 'weightedRoundRobin' && !config.error ? `<span class="k8gb-muted">Modeled first-record share</span><div class="k8gb-dns-allocation" role="img" aria-label="Effective site weights excluding manual exclusions">${eligible.map(tag => `<span class="k8gb-dns-series-${config.tags.indexOf(tag)%2}" style="flex:${Number(config.weights[tag])/total}"></span>`).join('')}</div><div class="k8gb-dns-shares">${config.tags.map(tag => `<span>${escape(tag)} / ${escape(config.weights[tag])} / ${config.excludedTags.includes(tag) ? 'Excluded' : `${(total ? 100*Number(config.weights[tag])/total : 0).toLocaleString('en-GB',{maximumFractionDigits:1})}%`}</span>`).join('')}</div>` : config.mode === 'roundRobin' ? `${eligible.length} eligible / ${config.tags.length} sites` : '';
        $('[data-dns-caption]').textContent = config.mode === 'failover' ? 'Illustrative failure and recovery, not observed availability. Actual failover depends on health checks, DNS updates and resolver cache; standby selection is illustrative.' : config.mode === 'geoip' ? 'GeoIP relies on EDNS Client Subnet and a GeoIP database. Subnet mappings here are illustrative, not cluster data.' : config.mode === 'weightedRoundRobin' ? 'Illustrative ordering, not an HTTP traffic forecast. Zero-weight sites are excluded from the model.' : 'DNS model, not HTTP traffic. Availability and record ordering are illustrative, not live observations.';
        if (config.excludedTags.length) $('[data-dns-caption]').textContent += ' Manual exclusions are omitted from new modeled answers; actual DNS withdrawal and cache expiry are not confirmed.';
        draw();
      }
      syncPlayback();
    }
    return {html,update,unmount};
  }
  return {normalize,nextWeighted,frame,html,createView};
});
