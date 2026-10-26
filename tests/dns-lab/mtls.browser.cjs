'use strict';
// Read-only UI acceptance against the owned mTLS fixture. Never plans/applies.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const base=process.env.K8GB_UI_URL||'http://127.0.0.1:8791';
const dir=path.resolve(process.env.ARTIFACTS_DIR||'artifacts/mtls-live-ui');
(async()=>{
 fs.mkdirSync(dir,{recursive:true});const browser=await chromium.launch({headless:true});
 const report={viewports:[],errors:[],mutationRequests:0};
 try{
  for(const width of [1440,390])for(const theme of ['light','dark']){
   const context=await browser.newContext({viewport:{width,height:1000}});
   await context.addInitScript(theme=>{localStorage.setItem('k8gb-ui-theme',theme)},theme);
   const page=await context.newPage();page.on('pageerror',error=>report.errors.push(error.message));
   page.on('request',request=>{if(request.method()==='POST'&&request.url().includes('/api/v1/mutations/'))report.mutationRequests++});
   await page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});
   await page.getByRole('link',{name:'mtls-lab',exact:true}).click();
   const response=page.waitForResponse(response=>new URL(response.url()).pathname==='/api/v1/catalog'&&response.status()===200);
   await page.getByRole('button',{name:/Edit source IngressRoute/}).first().click();
   const catalog=await(await response).json();
   const sanConfigured=catalog.sites.length>0&&catalog.sites.every(site=>site.catalog?.traefikSecurity?.sanFilterConfigured&&site.catalog.traefikSecurity.pluginAlias==='certauthz');
   await page.locator('#mutationTLSOption').waitFor();
   assert.equal(await page.locator('#mutationMTLS').isChecked(),true);
   assert.equal(await page.locator('#mutationMTLSEntrypoint').inputValue(),'secure-client');
   assert.equal(await page.locator('#mutationTLSOption').inputValue(),'demo/client-auth');
   assert.equal(await page.locator('#mutationTLSOption option[value="demo/insecure-client-auth"]').isDisabled(),true);
   assert.match(await page.locator('#mutationCAReferences').innerText(),/demo\/mtls-lab-client-ca/);
   const currentSANs=await page.locator('#mutationCNs').inputValue();
   assert.equal(await page.locator('#mutationCNs').isDisabled(),!sanConfigured&&!currentSANs.trim());
   if(sanConfigured){
    assert.match(await page.locator('#mutationSANStatus').innerText(),/certauthz alias: certauthz/);
    const expected=catalog.sites[0].catalog.candidates.find(row=>row.resource.name==='mtls-lab').traefik.commonNames||[];
    assert.deepEqual(currentSANs.split('\n').filter(Boolean),expected);
   }
   assert.equal(await page.locator('#mutationMTLSEntrypoint').count(),1);
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
   await page.screenshot({path:path.join(dir,`${theme}-${width}.png`),fullPage:true});
   await page.locator('#mutationMTLSEntrypoint').selectOption('websecure');
   assert.equal(await page.locator('#mutationMTLSEntrypoint').inputValue(),'websecure');
   report.viewports.push({width,theme,currentConfigLoaded:true,weakPolicyDisabled:true,SANFilterConfigured:sanConfigured,SANsLoaded:currentSANs.split('\n').filter(Boolean)});
   await context.close();
  }
  assert.deepEqual(report.errors,[]);assert.equal(report.mutationRequests,0);report.passed=true;
  console.log('Live shared mTLS UI: current values, CA references, weak-policy/SAN guards and desktop/mobile light/dark passed; no mutations');
 }finally{await browser.close();fs.writeFileSync(path.join(dir,'acceptance.json'),JSON.stringify(report,null,2))}
})().catch(error=>{console.error(error.stack);process.exitCode=1});
