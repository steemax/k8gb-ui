'use strict';
// Real local users, HTTPS transport, real agents/Kubernetes. Changes only the
// two owned native RBAC fixtures and restores their DNS TTL before success.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),assert=require('node:assert/strict');
const {execFileSync}=require('node:child_process');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const arg=process.argv.find(value=>value.startsWith('--state-dir='));
if(!arg||!path.isAbsolute(arg.slice(12)))throw Error('Explicit --state-dir=/absolute/owned/lab required');
const dir=arg.slice(12),state=JSON.parse(fs.readFileSync(path.join(dir,'state.json'))),cfg=state.builtinRbac;
assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(cfg?.publicURL,'https://127.0.0.1:8792');
for(const c of state.clusters)assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);
const mode=process.env.AUTH_MODE||'builtin';assert.ok(['builtin','oidc'].includes(mode));
const accounts=JSON.parse(fs.readFileSync(path.join(dir,mode==='oidc'?'keycloak/credentials.json':'builtin-rbac/credentials.json'))).accounts;
const actor=mode==='oidc'?accounts.find(x=>x.username==='admin').id:'builtin:admin';
const base=cfg.publicURL,artifacts=path.resolve(process.env.ARTIFACTS_DIR||`artifacts/${mode}-rbac-live`);
const report={authMode:mode,identityProvider:mode==='oidc'?'Keycloak':false,publicURL:base,checks:[],planRefreshes:[],errors:[],passed:false};
const prefix=`Local ${mode} RBAC acceptance`;
const kube=(c,args)=>execFileSync('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,...args],{encoding:'utf8',timeout:30000,maxBuffer:16<<20});
function snapshot(includeFixtures=false){
 const rows=[];
 for(const c of state.clusters)for(const namespace of ['demo','rbac-private'])for(const kind of ['gslbs.k8gb.io','ingressroutes.traefik.io','ingresses.networking.k8s.io'])for(const u of JSON.parse(kube(c,['-n',namespace,'get',kind,'-o','json'])).items){
  if(!includeFixtures&&u.metadata.labels?.['k8gb-ui.io/lab-scenario']==='builtin-rbac')continue;
  rows.push({cluster:c.id,kind,namespace,name:u.metadata.name,uid:u.metadata.uid,spec:u.spec,annotations:u.metadata.annotations});
 }
 return crypto.createHash('sha256').update(JSON.stringify(rows.sort((a,b)=>JSON.stringify([a.cluster,a.kind,a.namespace,a.name]).localeCompare(JSON.stringify([b.cluster,b.kind,b.namespace,b.name]))))).digest('hex');
}
function ttl(namespace,name,value){for(const c of state.clusters){const u=JSON.parse(kube(c,['-n',namespace,'get','gslbs.k8gb.io',name,'-o','json']));assert.equal(u.metadata.labels?.['k8gb-ui.io/lab-owner'],state.owner);assert.equal(u.spec.strategy.dnsTtlSeconds,value)}}
const post=(page,endpoint,data,headers={})=>page.request.post(base+endpoint,{headers:{Origin:base,'X-K8gb-Intent':'mutation',...headers},data});
async function json(page,endpoint){const response=await page.request.get(base+endpoint);assert.equal(response.status(),200,endpoint);return response.json()}
async function login(browser,username){
 const account=accounts.find(row=>row.username===username);assert.ok(account);
 const context=await browser.newContext({ignoreHTTPSErrors:true,viewport:{width:1440,height:1000}}),page=await context.newPage();
 page.on('pageerror',error=>report.errors.push(error.message));
 await page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});
 if(mode==='builtin')await page.getByRole('heading',{name:'Sign in',exact:true}).waitFor();
 else assert.ok(page.url().startsWith(state.keycloak.url+'/realms/k8gb-ui-lab/'));
 await page.getByLabel(mode==='oidc'?'Username or email':'Username',{exact:true}).fill(username);await page.getByLabel('Password',{exact:true}).fill(account.password);
 await page.getByRole('button',{name:/^Sign in$/i}).click();await page.locator('#namespace:not([disabled])').waitFor();
 const session=await json(page,'/api/v1/session');assert.equal(session.subject,mode==='oidc'?account.id:'builtin:'+username);assert.equal(session.mode,mode);
 const cookie=(await context.cookies()).find(row=>row.name==='__Host-k8gb-session');assert.ok(cookie?.httpOnly&&cookie.secure&&cookie.sameSite==='Lax');
 return {context,page,session};
}
async function change(page,namespace,name,value){
 await page.goto(base+'/?namespace='+namespace,{waitUntil:'networkidle'});
 await page.locator('#namespace:not([disabled])').waitFor();await page.locator('#namespace').selectOption(namespace);
 await page.getByRole('link',{name,exact:true}).click();
 await page.getByRole('button',{name:'Edit GSLB policy',exact:true}).click();await page.locator('#mutationTTL').waitFor();
 assert.equal(await page.locator('#mutationScope').inputValue(),'all');
 await page.locator('#mutationTTL').fill(String(value));await page.locator('#mutationReason').fill(`${prefix}: ${namespace} TTL ${value}`);
 let planned;
 for(let attempt=0;attempt<3;attempt++){
  const response=page.waitForResponse(response=>response.url().endsWith('/api/v1/mutations/plan')&&response.request().method()==='POST');
  await page.getByRole('button',{name:'Preview changes',exact:true}).click();planned=await(await response).json();
  if(planned.canApply)break;
  const errors=planned.targets?.filter(row=>row.error).map(row=>({clusterId:row.clusterId,...row.error}));
  if(!errors?.length||errors.some(row=>!['stale_resource','stale_dependency'].includes(row.code)))break;
  // A failed planning call made no application writes. Only version conflicts
  // get a fresh UI preview; apply is never retried, including unknown outcomes.
  report.planRefreshes.push({namespace,value,errors});await page.waitForTimeout(1500);
 }
 assert.equal(planned.canApply,true,`${namespace} TTL ${value}: ${JSON.stringify(planned.targets?.map(row=>({clusterId:row.clusterId,error:row.error})))}`);assert.equal(planned.targets.length,2);
 for(const row of planned.targets){assert.ok(!row.error);assert.equal(row.plan.request.actor,actor);assert.equal(row.plan.request.resource.namespace,namespace)}
 await page.locator('#mutationConsent').check();
 const resultResponse=page.waitForResponse(response=>response.url().endsWith('/api/v1/mutations/apply')&&response.request().method()==='POST');
 await page.getByRole('button',{name:'Apply changes',exact:true}).click();const result=await(await resultResponse).json();
 assert.equal(result.targets.length,2);assert.ok(result.targets.every(row=>!row.error&&row.result?.status==='applied'));
 await page.getByRole('heading',{name:'Apply result',exact:true}).waitFor();ttl(namespace,name,value);
}
(async()=>{
 fs.mkdirSync(artifacts,{recursive:true});const baseline=snapshot();let browser,admin,reader,anonymous;
 try{
  browser=await chromium.launch({headless:true});
  anonymous=await browser.newContext({ignoreHTTPSErrors:true});
  for(const endpoint of ['/api/v1/session','/api/v1/clusters','/api/v1/namespaces','/api/v1/overview?namespace=demo','/api/v1/catalog?namespace=demo','/api/v1/audit?namespace=demo'])assert.equal((await anonymous.request.get(base+endpoint)).status(),401);
  assert.equal((await anonymous.request.post(base+'/api/v1/mutations/plan',{data:{targets:[]}})).status(),401);
  assert.equal((await anonymous.request.get('http://127.0.0.1:18891/api/v1/session')).status(),401,'Backend must also authenticate');
  const old=await anonymous.request.get('http://127.0.0.1:8791/api/v1/session',{maxRedirects:0});assert.equal(old.status(),302);assert.equal(old.headers().location,base+'/api/v1/session');
  assert.equal((await anonymous.request.post('http://127.0.0.1:8791/api/v1/mutations/apply',{data:{targets:[]}})).status(),403);
  report.checks.push('Unauthenticated API rejected; old anonymous admin URL removed; backend authenticated');
  admin=await login(browser,'admin');reader=await login(browser,'demo-reader');
  assert.equal(admin.session.readOnly,false);assert.equal(reader.session.readOnly,true);
  assert.deepEqual((await json(admin.page,'/api/v1/namespaces')).namespaces,['demo','rbac-private']);
  assert.deepEqual((await json(reader.page,'/api/v1/namespaces')).namespaces,['demo']);
  for(const namespace of ['demo','rbac-private']){
   const overview=await json(admin.page,'/api/v1/overview?namespace='+namespace);
   assert.equal(overview.capabilities.readOnly,false);assert.equal(overview.sites.length,2);assert.ok(overview.sites.every(site=>site.available&&!site.capabilities.readOnly));
  }
  const overview=await json(reader.page,'/api/v1/overview?namespace=demo');assert.equal(overview.capabilities.readOnly,true);assert.ok(overview.sites.every(site=>site.capabilities.readOnly));
  assert.deepEqual(await reader.page.locator('#namespace option').evaluateAll(rows=>rows.map(row=>row.value)),['demo']);
  assert.match(await reader.page.locator('#accessBadge').innerText(),/Read-only/);
  assert.equal(await reader.page.getByRole('button',{name:/Create native GSLB|Create GSLB from IngressRoute/}).count(),0);
  await reader.page.getByRole('link',{name:'rbac-demo',exact:true}).click();
  assert.equal(await reader.page.getByRole('button',{name:'Edit GSLB policy',exact:true}).count(),0);
  assert.equal(await reader.page.locator('[data-traffic-instance]').count(),0);
  report.checks.push('Admin sees both configured namespaces/sites; reader sees demo with read-only capabilities and no write controls');
  const denied=[];
  for(const namespace of ['rbac-private','kube-system'])for(const endpoint of ['overview','catalog','audit']){
   const response=await reader.page.request.get(`${base}/api/v1/${endpoint}?namespace=${namespace}`);assert.equal(response.status(),403);denied.push(`${endpoint}:${namespace}`);
  }
  const ownCatalog=await json(reader.page,'/api/v1/catalog?namespace=demo');assert.ok(ownCatalog.sites.every(site=>site.catalog?.capabilities.readOnly));
  const adminOverview=await json(admin.page,'/api/v1/overview?namespace=demo'),instance=adminOverview.groups.find(row=>row.hostname==='rbac-demo.apps.k8gb.test').instances[0];
  const request={provider:'native',operation:'update',resource:instance.resource,policy:{strategy:'roundRobin',dnsTtlSeconds:6},reason:prefix+': verify role boundaries',actor:'forged-admin'};
  const response=await post(admin.page,'/api/v1/mutations/plan',{targets:[{clusterId:instance.clusterId,request}]});assert.equal(response.status(),200);
  const signed=await response.json();assert.equal(signed.canApply,true);assert.equal(signed.targets[0].plan.request.actor,actor);
  const noChange=snapshot(true);
  for(const endpoint of ['plan','apply']){
   const payload=endpoint==='plan'?{targets:[{clusterId:instance.clusterId,request}]}:{targets:signed.targets,allowPartial:false};
   const rejected=await post(reader.page,'/api/v1/mutations/'+endpoint,payload,{'X-Forwarded-User':'admin','X-Auth-Request-Groups':'platform-admins'});
   assert.equal(rejected.status(),403);assert.equal((await rejected.json()).code,'write_forbidden');
  }
  assert.equal(snapshot(true),noChange,'Denied viewer requests changed application state');
  assert.equal((await reader.page.request.get('https://127.0.0.1:19791/api/v1/snapshot?namespace=demo')).status(),401,'Session cookie is not an agent bearer token');
  await reader.page.goto(base+'/?namespace=rbac-private',{waitUntil:'networkidle'});
  await reader.page.getByRole('link',{name:'rbac-demo',exact:true}).waitFor();
  assert.equal(await reader.page.locator('#namespace').inputValue(),'demo');
  assert.equal(await reader.page.getByRole('link',{name:'rbac-private',exact:true}).count(),0);
  report.checks.push('Foreign namespace overview/catalog/audit return 403; URL tampering, header impersonation and stolen admin plan do not bypass viewer role');
  report.deniedReads=denied;
  for(const [namespace,name] of [['demo','rbac-demo'],['rbac-private','rbac-private']]){
   ttl(namespace,name,5);await change(admin.page,namespace,name,6);await change(admin.page,namespace,name,5);
   const audit=await json(admin.page,'/api/v1/audit?namespace='+namespace);
   assert.equal(audit.partial,false);assert.ok(audit.events.some(row=>row.actor===actor&&row.status==='applied'&&row.reason.startsWith(prefix)));
  }
  report.checks.push('Admin performed and restored real Kubernetes policy changes in both namespaces on east and west; audit actor is the authenticated subject');
  await admin.page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});
  await admin.page.getByRole('link',{name:'mtls-lab',exact:true}).click();await admin.page.getByRole('button',{name:/Edit source IngressRoute/}).first().click();
  await admin.page.locator('#mutationCNs').waitFor();assert.equal(await admin.page.locator('#mutationCNs').isDisabled(),false);assert.equal(await admin.page.locator('#mutationCNs').inputValue(),'client.demo.k8gb.test');
  assert.equal(await admin.page.locator('#mutationMTLSEntrypoint').inputValue(),'secure-client');
  await reader.page.goto(base+'/?namespace=demo',{waitUntil:'networkidle'});await reader.page.getByRole('button',{name:'Audit',exact:true}).click();await reader.page.locator('#auditEvents[aria-busy="false"]').waitFor();
  assert.equal((await json(reader.page,'/api/v1/audit?namespace=demo')).namespace,'demo');
  for(const width of [1440,390])for(const theme of ['light','dark']){
   await reader.page.setViewportSize({width,height:1000});await reader.page.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
   assert.ok(await reader.page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await reader.page.screenshot({path:path.join(artifacts,`reader-${theme}-${width}.png`)});
  }
  await admin.page.screenshot({path:path.join(artifacts,'admin-settings.png')});
  const audit=await json(admin.page,'/api/v1/audit?namespace=demo'),oldAudit=JSON.parse(fs.readFileSync(path.join(dir,'builtin-rbac/audit-before.json'))),retained=new Set(audit.events.map(row=>JSON.stringify(row)));
  assert.ok(oldAudit.events.every(row=>retained.has(JSON.stringify(row))),'Preexisting history lost');
  assert.equal(snapshot(),baseline,'Existing application configuration changed');
  if(mode==='builtin'){await reader.page.getByRole('button',{name:'Sign out',exact:true}).click();await reader.page.getByRole('heading',{name:'Sign in',exact:true}).waitFor()}
  else assert.equal((await reader.page.request.post(base+'/auth/logout',{headers:{Origin:base},maxRedirects:0})).status(),303);
  assert.equal((await reader.page.request.get(base+'/api/v1/session')).status(),401);
  report.checks.push('certauthz settings and existing application config/history preserved; viewer audit remains namespace-scoped; sign-out invalidates browser access');
  if(mode==='oidc'){
   const context=await browser.newContext({ignoreHTTPSErrors:true}),page=await context.newPage(),account=accounts.find(x=>x.username==='realm-admin-only');
   await page.goto(base+'/',{waitUntil:'networkidle'});await page.getByLabel('Username or email',{exact:true}).fill(account.username);await page.getByLabel('Password',{exact:true}).fill(account.password);await page.getByRole('button',{name:/^Sign in$/i}).click();
   await page.waitForURL(url=>url.origin===base&&url.pathname==='/');
   assert.equal((await json(page,'/api/v1/session')).readOnly,true);assert.deepEqual((await json(page,'/api/v1/namespaces')).namespaces,[]);
   for(const ns of ['demo','rbac-private'])assert.equal((await page.request.get(base+'/api/v1/overview?namespace='+ns)).status(),403);
   assert.equal((await post(page,'/api/v1/mutations/plan',{targets:[]})).status(),403);await context.close();
   report.checks.push('Keycloak realm-admin without a bound group is authenticated but receives no namespace or write access');
  }
  report.originalAuditEventsPreserved=oldAudit.events.length;assert.deepEqual(report.errors,[]);report.passed=true;
  console.log(`Real ${mode} RBAC passed: HTTPS login, admin changes on both sites/namespaces, viewer read-only scope, direct API denials and restored fixture policies`);
 }catch(error){let message=error.stack||error.message;for(const account of accounts)message=message.replaceAll(account.password,'[redacted]');report.errors.push(message);console.error(message);process.exitCode=1}
 finally{await admin?.context.close();await reader?.context.close();await anonymous?.close();await browser?.close();fs.writeFileSync(path.join(artifacts,'acceptance.json'),JSON.stringify(report,null,2)+'\n')}
})();
