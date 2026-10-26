'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const base = 'http://127.0.0.1:8791', namespace = 'demo';
const args = process.argv.slice(2), mutate = args.includes('--run-mutations');
const reportPath = args.find(value => value.startsWith('--report='))?.slice(9);
if (args.some(arg => arg !== '--run-mutations' && !arg.startsWith('--report=')) || !reportPath || !path.isAbsolute(reportPath)) throw Error('Use [--run-mutations] --report=/absolute/new-report.json');
const fd = fs.openSync(reportPath,'wx',0o600);
const report = {status:'RUNNING',startedAt:new Date().toISOString(),mutations:mutate,operations:[],errors:[]};
const save = () => { const data=JSON.stringify(report,null,2)+'\n';fs.ftruncateSync(fd,0);fs.writeSync(fd,data,0,'utf8'); };
const overview = async () => {const response=await fetch(base+'/api/v1/overview?namespace='+namespace);assert.equal(response.status,200);return response.json();};
const sleep = ms => new Promise(resolve=>setTimeout(resolve,ms));
async function dns(host,ips,ttls) {
  let last,consecutive=0;const deadline=Date.now()+90000,start=Date.now();
  do {
    last=[];
    for(const [index,port] of [15053,15054].entries())for(const tcp of [false,true]){
      const raw=execFileSync('dig',['@127.0.0.1','-p',String(port),host,'A','+noall','+answer','+time=2','+tries=1',...(tcp?['+tcp']:[])],{encoding:'utf8',timeout:4000});
      const records=raw.trim().split('\n').map(line=>line.trim().split(/\s+/)).filter(parts=>parts[0]===host+'.' && parts[2]==='IN' && parts[3]==='A').map(parts=>({ip:parts[4],ttl:Number(parts[1])}));
      last.push({port,protocol:tcp?'tcp':'udp',records,raw});
      if(JSON.stringify(records.map(r=>r.ip).sort())!==JSON.stringify([...ips].sort()) || records.some(r=>r.ttl!==ttls[index]))consecutive=-1;
    }
    consecutive=consecutive<0?0:consecutive+1;
    if(consecutive===2)return {durationMs:Date.now()-start,samples:last};
    await sleep(2000);
  }while(Date.now()<deadline);
  throw Error('DNS did not converge: '+JSON.stringify(last));
}
(async()=>{
  let browser;
  try{
    const initial=await overview();
    const groups={native:initial.groups.find(g=>g.hostname==='native.apps.k8gb.test'),traefik:initial.groups.find(g=>g.hostname==='hello.apps.k8gb.test')};
    const ips=[...new Set(groups.traefik?.instances.flatMap(i=>i.dnsTargets))];
    if(mutate)assert.equal(ips.length,2);
    for(const [mode,group] of Object.entries(groups)){
      assert.equal(group?.instances.length,2);
      for(const instance of group.instances){assert.equal(instance.management,mode);if(mutate){assert.equal(instance.policy.strategy,'roundRobin');assert.equal(instance.policy.dnsTtlSeconds,5);assert.equal(instance.traffic.excluded,false);assert.equal(instance.traffic.overrideManaged,false);assert.equal(instance.traffic.overrideConflict,false);}}
    }
    browser=await chromium.launch({headless:true});
    const page=await browser.newPage({viewport:{width:1440,height:1000}});
    page.on('pageerror',error=>report.errors.push(error.message));
    async function open(mode){
      await page.goto(base+'/?namespace=demo#group='+groups[mode].id,{waitUntil:'networkidle'});
      await page.locator('.k8gb-management-detail').first().waitFor();
      const direct=page.getByRole('button',{name:'Edit GSLB policy',exact:true});
      assert.equal(await direct.count(),mode==='native'?1:0);
      assert.equal(await page.locator('[data-traefik-instance]').count(),mode==='traefik'?2:0);
      assert.match(await page.locator('.k8gb-management-detail').first().innerText(),mode==='native'?/Direct Gslb[\s\S]*Gslb demo\/native-web/:/Webhook[\s\S]*IngressRoute demo\/hello/);
    }
    async function edit(mode,strategy,ttl){
      await open(mode);
      if(mode==='native'){await page.getByRole('button',{name:'Edit GSLB policy',exact:true}).click();await page.locator('#mutationScope').selectOption('all');}
      else {await page.locator('[data-traefik-instance="0"]').click();await page.locator('#mutationTTL').waitFor();}
      await page.locator('#mutationStrategy').selectOption(strategy);
      if(strategy==='failover')await page.locator('#mutationPrimary').fill('west');
      await page.locator('#mutationTTL').fill(String(ttl));
      await page.locator('#mutationReason').fill('Live management acceptance: '+mode+' '+strategy+' TTL '+ttl);
      const planning=page.waitForResponse(r=>r.url().endsWith('/mutations/plan')&&r.request().method()==='POST');
      await page.getByRole('button',{name:'Preview changes',exact:true}).click();
      const plan=await(await planning).json();assert.equal(plan.canApply,true,JSON.stringify(plan.targets.map(t=>({cluster:t.clusterId,error:t.error}))));
      assert.equal(plan.targets.length,mode==='native'?2:1);
      for(const target of plan.targets){assert.equal(target.plan.request.provider,mode==='native'?'native':'traefik');assert.equal(target.plan.request.resource.kind,mode==='native'?'Gslb':'IngressRoute');}
      await page.locator('#mutationConsent').check();await sleep(10000);
      const applying=page.waitForResponse(r=>r.url().endsWith('/mutations/apply')&&r.request().method()==='POST');
      await page.getByRole('button',{name:'Apply changes',exact:true}).click();
      const result=await(await applying).json();
      for(const target of result.targets)assert.equal(target.result?.status,'applied',JSON.stringify(result));
      const operation={mode,strategy,ttl,apply:result};report.operations.push(operation);save();
      await page.locator('#mutationWorkspace').getByRole('button',{name:'Close',exact:true}).click();
      const expected=strategy==='failover'?[groups.native.instances.find(i=>i.clusterId==='west').dnsTargets.find(ip=>ip===process.env.K8GB_LAB_WEST_IP)]:ips;
      if(strategy==='failover')assert.ok(expected[0],'Set K8GB_LAB_WEST_IP to the known west node IP');
      operation.dns=await dns(groups[mode].hostname,expected,mode==='native'?[ttl,ttl]:[ttl,5]);
      operation.status='PASS';save();
    }
    await open('native');await open('traefik');
    if(mutate){
      assert.ok(ips.includes(process.env.K8GB_LAB_WEST_IP),'Explicit known west node IP required before mutations');
      await edit('native','failover',5);
      await edit('native','roundRobin',5);
      await edit('traefik','roundRobin',9);
      await edit('traefik','roundRobin',5);
    }
    await page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});
    assert.equal(await page.locator('.k8gb-tile .k8gb-management.native').count(),1);
    assert.equal(await page.locator('.k8gb-tile .k8gb-management.traefik').count(),1);
    for(const [name,width,height] of [['desktop',1440,1000],['mobile',390,844]]){
      await page.setViewportSize({width,height});await page.screenshot({path:path.join(path.dirname(reportPath),'management-'+name+'.png'),fullPage:true});
      assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'Horizontal overflow: '+name);
    }
    assert.deepEqual(report.errors,[]);report.status='PASS';
  }catch(error){report.status='FAIL';report.error=error.message;report.warning='No automatic mutation retry or restoration was attempted; inspect resources before retrying.';process.exitCode=1;}
  finally{report.finishedAt=new Date().toISOString();save();fs.closeSync(fd);if(browser)await browser.close();console.log(JSON.stringify({status:report.status,error:report.error,operations:report.operations.map(o=>({mode:o.mode,strategy:o.strategy,ttl:o.ttl,status:o.status})),reportPath},null,2));}
})();
