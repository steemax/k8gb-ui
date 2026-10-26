// Real local lab UI review. Optional webhook TTL change is explicitly restored.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const base = 'http://127.0.0.1:8791';
const mutate = process.argv.includes('--run-webhook');
const dir = path.resolve(process.env.ARTIFACTS_DIR || 'artifacts/workspace-review');
const report = {startedAt:new Date().toISOString(),webhookMutationRequested:mutate,operations:[],errors:[]};
const get = async endpoint => { const response = await fetch(base+endpoint); assert.equal(response.status,200); return response.json(); };
const overview = () => get('/api/v1/overview?namespace=demo');
const signature = group => group.instances.map(row=>({cluster:row.clusterId,uid:row.resource.uid,policy:row.policy,traffic:row.traffic}));
const sleep = ms => new Promise(resolve=>setTimeout(resolve,ms));
const save = () => fs.writeFileSync(path.join(dir,'acceptance.json'),JSON.stringify(report,null,2)+'\n');
fs.mkdirSync(dir,{recursive:true});
(async()=>{
  let browser;
  try {
    const initial = await overview();
    const native = initial.groups.find(group=>group.hostname==='native.apps.k8gb.test');
    const webhook = initial.groups.find(group=>group.hostname==='hello.apps.k8gb.test');
    assert.equal(native?.instances.length,2); assert.equal(webhook?.instances.length,2);
    const catalog = await get('/api/v1/catalog?namespace=demo');
    for (const site of catalog.sites) { assert.ok(!site.error); assert.deepEqual(site.catalog.warnings,[]); assert.ok(site.catalog.candidates.some(row=>row.provider==='traefik' && row.enabled && row.eligible)); }
    browser = await chromium.launch({headless:true});
    const page = await browser.newPage({viewport:{width:1440,height:1000}});
    page.on('pageerror',error=>report.errors.push(error.message));
    const navigate = async group => { await page.goto(base+'/?namespace=demo'+(group?'#group='+group.id:''),{waitUntil:'networkidle'}); await page.waitForFunction(()=>document.getElementById('refresh').getAttribute('aria-busy')==='false'); };
    const close = () => page.getByRole('button',{name:'Close',exact:true}).click();
    async function webhookForm() {
      await navigate(webhook);
      await page.getByRole('button',{name:'Edit source IngressRoute / EAST local kind',exact:true}).click();
      await page.locator('#mutationTTL').waitFor();
      if (mutate) await page.locator('[data-target-cluster="west"]').uncheck();
      assert.equal(await page.locator('#mutationWorkspace').isVisible(),true);
      assert.equal(await page.locator('#mutationHostname').inputValue(),webhook.hostname);
      assert.equal(await page.locator('#mutationError').isVisible(),false);
      assert.equal(await page.locator('dialog').count(),0);
    }
    async function changeTTL(ttl) {
      await webhookForm();
      const before = Number(await page.locator('#mutationTTL').inputValue());
      await page.locator('#mutationTTL').fill(String(ttl));
      await page.locator('#mutationReason').fill(`Workspace acceptance ${report.startedAt}: webhook TTL ${before} to ${ttl}`);
      const planned = page.waitForResponse(response=>response.url().endsWith('/mutations/plan') && response.request().method()==='POST');
      await page.getByRole('button',{name:'Preview changes',exact:true}).click();
      const batch = await (await planned).json();
      assert.equal(batch.canApply,true,JSON.stringify(batch.targets.map(row=>row.error)));
      assert.equal(batch.targets.length,1);
      assert.equal(batch.targets[0].clusterId,'east');
      assert.equal(batch.targets[0].plan.request.resource.kind,'IngressRoute');
      await page.locator('#mutationConsent').check();
      const applied = page.waitForResponse(response=>response.url().endsWith('/mutations/apply') && response.request().method()==='POST');
      await page.getByRole('button',{name:'Apply changes',exact:true}).click();
      const result = await (await applied).json();
      const operation = {before,ttl,result}; report.operations.push(operation); save();
      assert.equal(result.targets[0]?.result?.status,'applied',JSON.stringify(result));
      let converged = false;
      for(let attempt=0;attempt<35;attempt++) {
        const sample = await overview();
        const current = sample.groups.find(group=>group.id===webhook.id);
        if(current?.instances.find(row=>row.clusterId==='east')?.policy.dnsTtlSeconds===ttl) { converged=true; break; }
        await sleep(1000);
      }
      assert.ok(converged,'Webhook child policy did not converge');
      let dns;
      for(let attempt=0;attempt<30;attempt++) {
        dns = [false,true].map(tcp=>{
          const raw = execFileSync('dig',['@127.0.0.1','-p','15053',webhook.hostname,'A','+noall','+answer','+time=2','+tries=1',...(tcp?['+tcp']:[])],{encoding:'utf8'});
          const records = raw.trim().split('\n').map(line=>line.split(/\s+/)).filter(parts=>parts[0]===webhook.hostname+'.' && parts[3]==='A');
          return {protocol:tcp?'tcp':'udp',raw,ttls:records.map(parts=>Number(parts[1]))};
        });
        if(dns.every(sample=>sample.ttls.length && sample.ttls.every(value=>value===ttl))) break;
        await sleep(1000);
      }
      operation.dns=dns; save();
      assert.ok(dns.every(sample=>sample.ttls.length && sample.ttls.every(value=>value===ttl)),'Authoritative DNS TTL did not converge');
      await page.getByRole('heading',{name:'Apply result',exact:true}).waitFor(); await close();
    }
    if(mutate) {
      const source = catalog.sites.find(site=>site.clusterId==='east').catalog.candidates.find(row=>row.resource.name==='hello' && row.provider==='traefik');
      assert.ok(source.enabled && source.eligible && !source.traffic.excluded && !source.traffic.overrideManaged && !source.traffic.overrideConflict);
      const ttl = source.policy.dnsTtlSeconds;
      assert.ok(Number.isInteger(ttl) && ttl>=0 && ttl<10000);
      await changeTTL(ttl+1);
      await changeTTL(ttl);
    }
    for(const theme of ['light','dark']) {
      await navigate();
      await page.evaluate(theme=>{localStorage.setItem('k8gb-ui-theme',theme);document.documentElement.dataset.theme=theme;},theme);
      await page.reload({waitUntil:'networkidle'});
      await page.locator('.k8gb-tile').first().waitFor();
      if(native.instances.every(row=>row.policy.strategy==='failover')) assert.match(await page.locator('.k8gb-tile').filter({hasText:'native-web'}).innerText(),new RegExp('Failover / primary '+native.instances[0].policy.primaryGeoTag));
      await page.screenshot({path:path.join(dir,theme+'-resources.png'),fullPage:true});
      await navigate(native);
      await page.getByRole('button',{name:'Edit GSLB policy',exact:true}).click();
      assert.equal(await page.locator('#mutationPrimary').inputValue(),native.instances[0].policy.primaryGeoTag || '');
      assert.equal(await page.locator('#detail').isVisible(),false);
      await page.screenshot({path:path.join(dir,theme+'-native-edit.png'),fullPage:true}); await close();
      await webhookForm(); await page.screenshot({path:path.join(dir,theme+'-traefik-edit.png'),fullPage:true}); await close();
      await page.getByRole('button',{name:'Audit',exact:true}).click();
      await page.waitForFunction(()=>!document.getElementById('auditEvents').textContent.includes('Loading audit'));
      assert.equal(await page.locator('#auditWorkspace').isVisible(),true);
      await page.screenshot({path:path.join(dir,theme+'-audit.png'),fullPage:true}); await close();
    }
    await page.setViewportSize({width:390,height:844});
    await navigate(); assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    await page.screenshot({path:path.join(dir,'mobile-resources.png'),fullPage:true});
    await webhookForm(); assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    await page.screenshot({path:path.join(dir,'mobile-traefik-edit.png'),fullPage:true}); await close();
    const final = await overview();
    assert.deepEqual(signature(final.groups.find(group=>group.id===native.id)),signature(native),'Native user configuration changed');
    assert.deepEqual(signature(final.groups.find(group=>group.id===webhook.id)),signature(webhook),'Webhook baseline was not restored');
    assert.deepEqual(report.errors,[]); report.status='PASS';
  } catch(error) {
    report.status='FAIL'; report.error=error.message;
    report.warning='No automatic retry after a failed or unknown apply. Inspect recorded operations before restoring manually.';
    process.exitCode=1;
  } finally {
    report.finishedAt=new Date().toISOString(); save(); await browser?.close();
    console.log(JSON.stringify({status:report.status,error:report.error,operations:report.operations.length,artifacts:dir}));
  }
})();
