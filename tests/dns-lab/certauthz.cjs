'use strict';
// Opt-in writes to the explicitly owned local lab only. No default kube-context,
// image publication or PKI export. Existing application policies are preserved.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),net=require('node:net'),assert=require('node:assert/strict');
const {execFileSync,spawn,spawnSync}=require('node:child_process');
const action=process.argv[2],arg=process.argv.find(value=>value.startsWith('--state-dir='));
if(!['install','verify','probe'].includes(action)||!arg)throw Error('Usage: node tests/dns-lab/certauthz.cjs install|verify|probe --state-dir=/absolute/owned/lab');
const dir=arg.slice('--state-dir='.length),root=path.resolve(__dirname,'../..'),statePath=path.join(dir,'state.json');
if(!path.isAbsolute(dir))throw Error('Absolute state directory required');
const state=JSON.parse(fs.readFileSync(statePath,'utf8'));
assert.equal(state.version,1);assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(state.clusters?.length,2);
for(const c of state.clusters){assert.ok(['east','west'].includes(c.id));assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`)}
assert.equal(new Set(state.clusters.map(c=>c.id)).size,2);assert.ok(['http://127.0.0.1:8791','https://127.0.0.1:8792'].includes(state.consoleURL));
if(state.builtinRbac&&action==='verify')throw Error('The lab now requires builtin sign-in; use an authenticated browser for mutations, or probe for read-only SAN checks');
assert.equal(state.mtls?.source,'mtls-lab');
process.umask(0o077);
const version='v0.1.2',moduleName='github.com/famedly/traefik-certauthz',alias='certauthz',allowedSAN='client.demo.k8gb.test';
const pki=path.join(dir,'mtls-pki'),reportDir=path.join(root,'artifacts/certauthz-live');
const run=(cmd,args,options={})=>{try{return execFileSync(cmd,args,{encoding:'utf8',maxBuffer:16<<20,timeout:360000,...options})}catch(error){throw Error(`${cmd} failed (status ${error.status}); arguments/credentials omitted`)}};
const kube=(c,args,options)=>run('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,...args],options);
const get=(c,kind,name,namespace='demo')=>JSON.parse(kube(c,['-n',namespace,'get',kind,name,'-o','json']));
const wait=ms=>new Promise(resolve=>setTimeout(resolve,ms));
function snapshot(c){
 const data=[];
 for(const kind of ['ingressroutes.traefik.io','gslbs.k8gb.io','ingresses.networking.k8s.io'])for(const item of JSON.parse(kube(c,['-n','demo','get',kind,'-o','json'])).items){
  if(item.metadata.name==='mtls-lab'||item.metadata.ownerReferences?.some(ref=>ref.kind==='IngressRoute'&&ref.name==='mtls-lab'))continue;
  data.push({kind,name:item.metadata.name,uid:item.metadata.uid,spec:item.spec,annotations:item.metadata.annotations});
 }
 return crypto.createHash('sha256').update(JSON.stringify(data.sort((a,b)=>(a.kind+a.name).localeCompare(b.kind+b.name)))).digest('hex');
}
function patch(c,name,flags){
 const current=get(c,'deployment',name,'k8gb-ui-system'),containers=structuredClone(current.spec.template.spec.containers),container=containers.find(row=>row.name===name);
 assert.ok(container,'Unexpected lab container');
 assert.ok(name==='traefik' ? container.image==='traefik:v3.6.10' : container.image.startsWith('k8gb-ui-lab-'));
 for(const flag of flags){const prefix=flag.split('=')[0]+'=';container.args=(container.args||[]).filter(value=>!value.toLowerCase().startsWith(prefix.toLowerCase()));container.args.push(flag)}
 if(JSON.stringify(containers)!==JSON.stringify(current.spec.template.spec.containers))kube(c,['-n','k8gb-ui-system','patch','deployment',name,'--type=json','-p',JSON.stringify([{op:'test',path:'/metadata/uid',value:current.metadata.uid},{op:'test',path:'/spec/template/spec/containers',value:current.spec.template.spec.containers},{op:'replace',path:'/spec/template/spec/containers',value:containers}])]);
 process.stdout.write(kube(c,['-n','k8gb-ui-system','rollout','status',`deployment/${name}`,'--timeout=180s']));
}
function fixture(c){
 const source=get(c,'ingressroute','mtls-lab');assert.equal(source.metadata.labels?.['k8gb-ui.lab-owner'],state.owner);
 const policy=get(c,'tlsoption','client-auth');assert.equal(policy.spec.clientAuth.clientAuthType,'RequireAndVerifyClientCert');
 assert.deepEqual(policy.spec.clientAuth.secretNames,['mtls-lab-client-ca']);return source;
}
function issueClients(){
 for(const file of ['ca.key','ca.crt','client.key','client.crt','untrusted.key','untrusted.crt'])fs.accessSync(path.join(pki,file));
 for(const [name,san] of [['other-san','other.demo.k8gb.test'],['san-suffix',allowedSAN+'.attacker.test'],['cn-only',null]]){
  const cert=path.join(pki,`${name}.crt`),key=path.join(pki,`${name}.key`);
  if(!fs.existsSync(cert)){
   run('openssl',['req','-new','-newkey','rsa:2048','-nodes','-keyout',key,'-out',path.join(pki,`${name}.csr`),'-subj',`/CN=${allowedSAN}`],{stdio:['ignore','pipe','pipe']});
   fs.writeFileSync(path.join(pki,`${name}.ext`),`basicConstraints=critical,CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\n${san ? `subjectAltName=DNS:${san}\n` : ''}`);
   run('openssl',['x509','-req','-in',path.join(pki,`${name}.csr`),'-CA',path.join(pki,'ca.crt'),'-CAkey',path.join(pki,'ca.key'),'-set_serial',`0x${crypto.randomBytes(8).toString('hex')}`,'-days','14','-extfile',path.join(pki,`${name}.ext`),'-out',cert],{stdio:['ignore','pipe','pipe']});
  }
  const parsed=new crypto.X509Certificate(fs.readFileSync(cert));
  assert.equal(parsed.subject,`CN=${allowedSAN}`);assert.equal(parsed.subjectAltName,san ? `DNS:${san}` : undefined);
  run('openssl',['verify','-CAfile',path.join(pki,'ca.crt'),cert],{stdio:['ignore','pipe','pipe']});
 }
}
function install(){
 issueClients();const before=Object.fromEntries(state.clusters.map(c=>[c.id,snapshot(c)]));
 for(const c of state.clusters){
  fixture(c);
  patch(c,'traefik',[`--experimental.plugins.${alias}.modulename=${moduleName}`,`--experimental.plugins.${alias}.version=${version}`,'--log.level=INFO']);
  patch(c,'k8gb-ui-traefik',[`--cn-plugin=${alias}`]);
  patch(c,'k8gb-ui-agent',[`--traefik-cn-plugin=${alias}`]);
  assert.equal(snapshot(c),before[c.id],`Existing policies changed on ${c.id}`);
  console.log(`${c.id}: certauthz ${version} configured on Traefik, webhook and agent`);
 }
 const fresh=JSON.parse(fs.readFileSync(statePath));assert.equal(fresh.owner,state.owner);
 fresh.certauthz={moduleName,version,alias};fs.writeFileSync(statePath,JSON.stringify(fresh,null,2)+'\n');
}
async function api(url,body){
 const response=await fetch(state.consoleURL+url,{method:body?'POST':'GET',headers:body?{'Content-Type':'application/json','Origin':state.consoleURL,'X-K8gb-Intent':'mutation'}:{},body:body?JSON.stringify(body):undefined,signal:AbortSignal.timeout(60000)});
 const value=await response.json();if(!response.ok)throw Error(`Console API returned ${response.status}`);return value;
}
async function update(names,entry){
 const targets=state.clusters.map(c=>{const u=fixture(c);return {clusterId:c.id,request:{provider:'traefik',operation:'update',resource:{apiVersion:u.apiVersion,kind:u.kind,namespace:'demo',name:u.metadata.name,uid:u.metadata.uid,resourceVersion:u.metadata.resourceVersion},policy:{strategy:'roundRobin',dnsTtlSeconds:5},traefik:{hostname:'mtls-lab.apps.k8gb.test',entrypoint:'websecure',mtls:true,mtlsEntrypoint:entry,tlsOption:{namespace:'demo',name:'client-auth'},commonNames:names,local:false},reason:`Local DNS SAN acceptance: ${names.length ? 'certauthz filtering' : 'trusted-client baseline'} on ${entry}`}}});
 const planned=await api('/api/v1/mutations/plan',{targets});
 if(!planned.canApply||planned.targets.some(row=>row.error))throw Error(`SAN plan rejected: ${planned.targets.map(row=>row.error?.error||'ok').join('; ')}`);
 const applied=await api('/api/v1/mutations/apply',{targets:planned.targets,allowPartial:false});
 if(applied.targets.some(row=>row.error))throw Error('SAN apply did not succeed on every site; not retrying');
}
async function withListener(c,port,task){
 const server=net.createServer();await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));const local=server.address().port;await new Promise(resolve=>server.close(resolve));
 const child=spawn('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,'-n','k8gb-ui-system','port-forward','deployment/traefik',`${local}:${port}`,'--address=127.0.0.1'],{stdio:'ignore'});
 try{return await task(local)}finally{child.kill('SIGTERM');await new Promise(resolve=>{if(child.exitCode!==null)return resolve();child.once('exit',resolve)})}
}
function request(local,name){
 // HTTP/1.1 avoids LibreSSL/curl hiding a TLS alert as a generic HTTP/2
 // framing error. Negative trust checks must identify a TLS error, not any exit.
 const result=spawnSync('curl',['--http1.1','--silent','--show-error','--connect-timeout','2','--max-time','5','--noproxy','*','--cacert',path.join(pki,'ca.crt'),'--connect-to',`mtls-lab.apps.k8gb.test:443:127.0.0.1:${local}`,...(name ? ['--cert',path.join(pki,`${name}.crt`),'--key',path.join(pki,`${name}.key`)] : []),'--write-out','\n%{http_code}','https://mtls-lab.apps.k8gb.test/'],{encoding:'utf8',timeout:8000});
 const output=result.stdout||'',line=output.lastIndexOf('\n');
 return {exit:result.status,error:String(result.stderr||''),code:Number(output.slice(line+1)),body:output.slice(0,line)};
}
async function eventually(probe,accept,label){
 let result;for(let i=0;i<60;i++){result=probe();if(accept(result))return result;await wait(500)}throw Error(`${label}: ${JSON.stringify({exit:result?.exit,httpCode:result?.code,error:result?.error})}`);
}
async function verify(){
 issueClients();const before=Object.fromEntries(state.clusters.map(c=>[c.id,snapshot(c)])),auditBefore=await api('/api/v1/audit?namespace=demo');
 const catalog=await api('/api/v1/catalog?namespace=demo');
 for(const site of catalog.sites){assert.equal(site.catalog?.traefikSecurity?.pluginAlias,alias);assert.equal(site.catalog?.traefikSecurity?.sanFilterConfigured,true)}
 const report={plugin:{moduleName,version,alias},results:[],finalEntrypoint:'secure-client',allowedSAN};
 // Prove every negative SAN certificate passes the same strict CA policy before
 // enabling certauthz. A later 403 is authorization, not a TLS handshake failure.
 await update([],'secure-client');
 for(const c of state.clusters)await withListener(c,8443,async local=>{
  for(const identity of ['client','other-san','san-suffix','cn-only'])await eventually(()=>request(local,identity),r=>r.exit===0&&r.code===200&&r.body.includes(`hello from ${c.id}`),`${identity}: baseline mTLS failed on ${c.id}`);
 });
 for(const [entry,port] of [['secure-client',8443],['websecure',8080]]){
  await update([allowedSAN],entry);
  for(const c of state.clusters){
   await withListener(c,port,async local=>{
    await eventually(()=>request(local,'client'),r=>r.exit===0&&r.code===200&&r.body.includes(`hello from ${c.id}`),`Allowed DNS SAN failed on ${c.id}`);
    for(const identity of ['other-san','san-suffix','cn-only'])await eventually(()=>request(local,identity),r=>r.exit===0&&r.code===403&&r.body.includes('No matching DNSNames'),`${identity}: expected certauthz 403 on ${c.id}`);
   });
   // A TLS rejection can reset the forwarded stream and stop kubectl itself.
   // Use a fresh tunnel per negative identity, never count a closed port as TLS.
   for(const identity of [null,'untrusted'])await withListener(c,port,async local=>{
    await eventually(()=>request(local,'client'),r=>r.exit===0&&r.code===200&&r.body.includes(`hello from ${c.id}`),`TLS probe listener unavailable on ${c.id}`);
    const r=request(local,identity);
    const rejected=[35,56].includes(r.exit)&&r.code===0&&(identity ? /alert (unknown ca|bad certificate|certificate unknown)/i : /certificate required|reason\(1116\)/i).test(r.error);
    assert.ok(rejected,`Expected explicit TLS rejection of ${identity||'no certificate'} on ${c.id}/${entry}: ${JSON.stringify({exit:r.exit,httpCode:r.code,error:r.error})}`);
   });
   report.results.push({cluster:c.id,entrypoint:entry,allowedSANStatus:200,otherSANStatus:403,matchingCNWithoutAllowedSANStatus:403,allowedSANSuffixStatus:403,noSANStatus:403,noCertificateRejectedByTLS:true,untrustedCARejectedByTLS:true});
  }
 }
 await update([allowedSAN],'secure-client');
 for(const c of state.clusters){
  await withListener(c,8443,async local=>{
   await eventually(()=>request(local,'client'),r=>r.exit===0&&r.code===200&&r.body.includes(`hello from ${c.id}`),`Final allowed route unavailable on ${c.id}`);
   await eventually(()=>request(local,'other-san'),r=>r.exit===0&&r.code===403&&r.body.includes('No matching DNSNames'),`Final SAN filter unavailable on ${c.id}`);
  });
  assert.equal(snapshot(c),before[c.id],`Preexisting policies changed on ${c.id}`);
  const middleware=JSON.parse(kube(c,['-n','demo','get','middlewares.traefik.io','-o','json'])).items.find(row=>row.metadata.ownerReferences?.some(ref=>ref.name==='mtls-lab'&&ref.kind==='IngressRoute'));
  assert.deepEqual(middleware?.spec.plugin,{[alias]:{domains:[allowedSAN]}});
 }
 const auditAfter=await api('/api/v1/audit?namespace=demo'),retained=new Set(auditAfter.events.map(row=>JSON.stringify(row)));
 assert.ok(auditBefore.events.every(row=>retained.has(JSON.stringify(row))),'Prior audit history lost');
 report.trustedNegativeClientsPassedBeforeSANFilter=true;report.preexistingConfigsPreserved=true;report.priorAuditPreserved=true;report.passed=true;
 fs.mkdirSync(reportDir,{recursive:true});fs.writeFileSync(path.join(reportDir,'acceptance.json'),JSON.stringify(report,null,2)+'\n');
 console.log(JSON.stringify(report,null,2));
}
async function probe(){
 const results=[];
 for(const c of state.clusters){
  fixture(c);
  await withListener(c,8443,async local=>{
   await eventually(()=>request(local,'client'),r=>r.exit===0&&r.code===200&&r.body.includes(`hello from ${c.id}`),`Allowed SAN failed on ${c.id}`);
   for(const identity of ['other-san','san-suffix','cn-only'])await eventually(()=>request(local,identity),r=>r.exit===0&&r.code===403&&r.body.includes('No matching DNSNames'),`Expected ${identity} rejection on ${c.id}`);
   results.push({cluster:c.id,allowedSANStatus:200,otherSANStatus:403,noSANStatus:403,allowedSANSuffixStatus:403});
  });
 }
 const report={results,readOnly:true,passed:true};
 fs.mkdirSync(reportDir,{recursive:true});fs.writeFileSync(path.join(reportDir,'current.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report,null,2));
}
(async()=>{if(action==='install')install();else if(action==='probe')await probe();else await verify()})().catch(error=>{console.error(error.message);process.exitCode=1});
