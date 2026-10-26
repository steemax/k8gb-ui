// DNS-zone creation review against the owned lab. Plans only; never applies resources.
'use strict';
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const base='http://127.0.0.1:8791',dir=path.resolve(process.env.ARTIFACTS_DIR || 'artifacts/dns-zones');
fs.mkdirSync(dir,{recursive:true});
const report={startedAt:new Date().toISOString(),applies:0,errors:[],plans:[],forms:[]};
const get=async endpoint=>{const r=await fetch(base+endpoint);assert.equal(r.status,200);return r.json()};
(async()=>{
 let browser;
 try {
  const before=await get('/api/v1/overview?namespace=demo');
  const signature=value=>value.groups.map(group=>({id:group.id,instances:group.instances.map(row=>({cluster:row.clusterId,uid:row.resource.uid,policy:row.policy,traffic:row.traffic}))}));
  const catalog=await get('/api/v1/catalog?namespace=demo');
  for(const site of catalog.sites){assert.deepEqual(site.catalog.dnsZones.zones,['apps.k8gb.test']);assert.equal(site.catalog.dnsZones.available,true);assert.deepEqual(site.catalog.warnings,[]);}
  const targets=catalog.sites.map(site=>{const candidate=site.catalog.candidates.find(c=>c.provider==='native' && c.resource.name==='native-next');assert.ok(candidate?.eligible && !candidate.enabled);return {clusterId:site.clusterId,request:{provider:'native',operation:'create',resource:{apiVersion:'k8gb.io/v1beta1',kind:'Gslb',namespace:'demo',name:'zone-preview-only'},target:candidate.resource,policy:{strategy:'roundRobin',dnsTtlSeconds:5},dnsZone:'apps.k8gb.test',reason:'DNS zone acceptance: plan only'}};});
  for(const invalid of [false,true]){
   const payload=structuredClone(targets);if(invalid)for(const row of payload)row.request.dnsZone='unserved.example.test';
   const response=await fetch(base+'/api/v1/mutations/plan',{method:'POST',headers:{Origin:base,'Content-Type':'application/json','X-K8gb-Intent':'mutation'},body:JSON.stringify({targets:payload})});assert.equal(response.status,200);
   const body=await response.json();report.plans.push({invalid,canApply:body.canApply,targets:body.targets.map(row=>({clusterId:row.clusterId,error:row.error,dependencies:row.plan?.dependencies}))});
   if(invalid){assert.equal(body.canApply,false);assert.ok(body.targets.every(row=>row.error?.code==='invalid_dns_zone'));}
   else {assert.equal(body.canApply,true,JSON.stringify(body.targets));assert.ok(body.targets.every(row=>row.plan.dependencies.some(dep=>dep.kind==='Deployment'&&dep.namespace==='k8gb'&&dep.name==='k8gb'&&dep.uid&&dep.resourceVersion)));}
  }
  browser=await chromium.launch({headless:true});
  for(const theme of ['light','dark'])for(const width of [1440,390]){
   const page=await browser.newPage({viewport:{width,height:1000}});page.on('pageerror',error=>report.errors.push(error.message));
   page.on('request',request=>{if(request.url().endsWith('/mutations/apply'))report.applies++});
   await page.addInitScript(theme=>localStorage.setItem('k8gb-ui-theme',theme),theme);
   await page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});
   await page.getByRole('button',{name:'Create native GSLB',exact:true}).click();await page.locator('#mutationDNSZone').waitFor();
   async function checkForm(mode) {
    const dimensions=await page.locator('#mutationForm').evaluate(form=>{
     const controls=[...form.querySelectorAll('select,input:not([type=checkbox]):not([readonly])')].filter(node=>node.getBoundingClientRect().height>0);
     return {controls:controls.map(node=>({id:node.id||node.dataset.targetChoice,height:node.getBoundingClientRect().height})),width:form.getBoundingClientRect().width,overflow:form.scrollWidth>form.clientWidth,sections:[...form.querySelectorAll('.k8gb-form-section>h3')].map(node=>node.textContent)};
    });
    assert.ok(!dimensions.overflow,`${mode} form overflow`);
    assert.ok(dimensions.controls.every(control=>control.height===40),`Inconsistent control heights: ${JSON.stringify(dimensions)}`);
    assert.ok((await page.locator('#mutationDNSZoneStatus').innerText()).includes('Available on 2 selected clusters'));
    assert.equal(await page.locator('#mutationZoneSources').isVisible(),false);
    assert.deepEqual(await page.locator('[data-target-status]').allTextContents(),['Selected','Selected']);
    report.forms.push({theme,viewport:width,mode,...dimensions});
   }
   assert.equal(await page.locator('#mutationDNSZone').inputValue(),'apps.k8gb.test');
   assert.match(await page.locator('#mutationDeclaredHosts').innerText(),/native-next.apps.k8gb.test/);
   assert.equal(await page.locator('#mutationClusterTargets input:checked').count(),2);
   await checkForm('native');
   await page.screenshot({path:path.join(dir,`${theme}-${width}-native.png`),fullPage:true});
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'Native form overflow');
   await page.getByRole('button',{name:'Close',exact:true}).click();
   await page.getByRole('button',{name:'Create GSLB from IngressRoute',exact:true}).click();await page.locator('#mutationHostLabel').waitFor();
   assert.equal(await page.locator('#mutationLocal').count(),0);assert.equal(await page.locator('#mutationClusterTargets input:checked').count(),2);
   await checkForm('traefik');
   assert.equal(await page.locator('#mutationCNDetails').getAttribute('open'),null);
   await page.locator('#mutationHostLabel').fill('zone-preview');
   assert.equal(await page.locator('#mutationHostname').inputValue(),'zone-preview.apps.k8gb.test');
   assert.equal(await page.locator('#k8gbMutationDNS [data-dns-hostname]').innerText(),'zone-preview.apps.k8gb.test');
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'Traefik form overflow');
   await page.screenshot({path:path.join(dir,`${theme}-${width}-traefik.png`),fullPage:true});
   await page.locator('[data-target-cluster="west"]').uncheck();assert.equal(await page.locator('[data-target-row="west"]').getAttribute('data-selected'),'false');
   assert.equal(await page.locator('[data-target-status="west"]').innerText(),'Not selected');
   await page.locator('[data-target-cluster="west"]').check();
   await page.locator('#mutationMTLS').check();assert.equal(await page.locator('#mutationCNs').isVisible(),true);
   if(await page.locator('#mutationCNs').isEnabled()){
    await page.locator('#mutationCNs').fill('client.example.test');
    await page.locator('#mutationMTLS').uncheck();assert.equal(await page.locator('#mutationCNs').inputValue(),'client.example.test');
    assert.equal(await page.locator('#mutationCNHint').isVisible(),true);
   }else{
    assert.match(await page.locator('#mutationSANStatus').innerText(),/unavailable/);
    await page.locator('#mutationMTLS').uncheck();
   }
   await page.locator('#mutationCNDetails>summary').click();await page.locator('#mutationReason').fill('Visual review only');
   assert.equal(await page.locator('#mutationCNs').isVisible(),false,'Typing elsewhere must not reopen technical details');
   await page.close();
  }
  assert.deepEqual(signature(await get('/api/v1/overview?namespace=demo')),signature(before));
  assert.equal(report.applies,0);assert.deepEqual(report.errors,[]);report.configurationUnchanged=true;report.passed=true;
 }catch(error){report.failure=error.stack;process.exitCode=1;console.error(error)}finally{if(browser)await browser.close();fs.writeFileSync(path.join(dir,'acceptance.json'),JSON.stringify(report,null,2)+'\n');}
 if(report.passed)console.log('DNS zones: controller discovery, cross-namespace dependency pins, server rejection, native and Traefik forms, responsive light/dark passed; no applies.');
})();
