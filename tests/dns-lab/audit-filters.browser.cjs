'use strict';
// Read-only audit acceptance against the owned local lab. No plan/apply calls.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const base=process.env.K8GB_UI_URL||'http://127.0.0.1:8791';
assert.equal(new URL(base).hostname,'127.0.0.1');
const artifacts=path.resolve(process.env.ARTIFACTS_DIR||'artifacts/audit-filters-live');
const get=async()=>{const r=await fetch(base+'/api/v1/audit?namespace=demo');assert.equal(r.status,200);return r.json()};
(async()=>{
 fs.mkdirSync(artifacts,{recursive:true});
 const before=await get();assert.ok(before.events.length);assert.equal(before.partial,false);
 const browser=await chromium.launch({headless:true});
 const report={viewports:[],errors:[],mutationRequests:0};
 try{
  for(const width of [1440,390])for(const theme of ['light','dark']){
   const context=await browser.newContext({viewport:{width,height:1000}});
   await context.addInitScript(theme=>localStorage.setItem('k8gb-ui-theme',theme),theme);
   const page=await context.newPage();page.on('pageerror',error=>report.errors.push(error.message));
   page.on('request',request=>{if(request.method()==='POST'&&request.url().includes('/api/v1/mutations/'))report.mutationRequests++});
   await page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});
   await page.getByRole('button',{name:'Audit',exact:true}).click();
   await page.getByRole('heading',{name:`${before.events.length} of ${before.events.length} events`,exact:true}).waitFor();
   const first=before.events[0];
   await page.locator('#audit-search').fill(first.resource.name);
   await page.locator('#audit-clusterId').selectOption(first.clusterId);
   const matched=before.events.filter(event=>event.clusterId===first.clusterId&&JSON.stringify(event).toLowerCase().includes(first.resource.name.toLowerCase()));
   assert.equal(await page.locator('#auditResultsHeading').innerText(),`${matched.length} of ${before.events.length} events`);
   await page.getByRole('button',{name:'Newest first',exact:true}).click();
   const oldest=matched.reduce((a,b)=>Date.parse(a.time)<Date.parse(b.time)?a:b);
   const formatted=await page.evaluate(time=>window.K8gbCore.formatTime(time),oldest.time);
   assert.equal(await page.locator('.k8gb-audit-time').first().innerText(),formatted);
   await page.getByRole('button',{name:'Refresh audit',exact:true}).click();
   await page.locator('#auditEvents[aria-busy="false"]').waitFor();
   assert.equal(await page.locator('#audit-search').inputValue(),first.resource.name);
   assert.equal(await page.locator('#audit-clusterId').inputValue(),first.clusterId);
   assert.equal(await page.getByRole('button',{name:'Oldest first',exact:true}).count(),1);
   await page.locator('#audit-search').fill('no-such-audit-record-0bd32c');
   assert.equal(await page.locator('#auditResultsHeading').innerText(),`0 of ${before.events.length} events`);
   assert.match(await page.locator('.k8gb-audit-sources').innerText(),/All sources responded \/ 2 of 2/);
   await page.getByRole('button',{name:'Reset filters',exact:true}).click();
   assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
   await page.screenshot({path:path.join(artifacts,`${theme}-${width}.png`)});
   report.viewports.push({width,theme,filtering:true,sorting:true,refreshPreservesFilters:true,sourceCoverageVisible:true});
   await context.close();
  }
  assert.deepEqual((await get()).events,before.events,'Audit review must not alter retained history');
  assert.deepEqual(report.errors,[]);assert.equal(report.mutationRequests,0);
  report.passed=true;report.retainedEvents=before.events.length;report.historyUnchanged=true;
  console.log('Live audit filters, sorting, refresh and layout passed in light/dark desktop/mobile; no mutations or history changes');
 }finally{await browser.close();fs.writeFileSync(path.join(artifacts,'acceptance.json'),JSON.stringify(report,null,2))}
})().catch(error=>{console.error(error.stack);process.exitCode=1});
