'use strict';
// Install only a separate, owned Helm release into the two existing lab clusters.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),assert=require('node:assert/strict');
const {execFileSync}=require('node:child_process');
const arg=process.argv.find(x=>x.startsWith('--state-dir='));assert.ok(arg&&path.isAbsolute(arg.slice(12)));
const dir=arg.slice(12),root=path.resolve(__dirname,'../..'),state=JSON.parse(fs.readFileSync(path.join(dir,'state.json'))),priv=path.join(dir,'helm-review');
assert.match(state.owner,/^[a-f0-9]{8}$/);process.umask(0o077);fs.mkdirSync(priv,{recursive:true,mode:0o700});
const run=(cmd,args,options={})=>{try{return execFileSync(cmd,args,{encoding:'utf8',timeout:240000,maxBuffer:16<<20,...options})}catch(e){throw Error(`${cmd} failed (${e.status}); arguments/output omitted`)}};
const docker=args=>run('docker',['--context','desktop-linux',...args]);
const kube=(c,args,options)=>run('kubectl',['--kubeconfig',path.join(dir,c.id+'.kubeconfig'),'--context','kind-'+c.name,...args],options);
function allowed(c,verb,resource,namespace){
 const args=['--kubeconfig',path.join(dir,c.id+'.kubeconfig'),'--context','kind-'+c.name,'auth','can-i',verb,resource,'-n',namespace,'--as=system:serviceaccount:'+ns+':'+fullname+'-agent'];
 try{return execFileSync('kubectl',args,{encoding:'utf8',timeout:30000}).trim()}catch(e){if(e.status===1&&e.stdout?.trim()==='no')return 'no';throw Error('RBAC permission check failed')}
}
const ns='k8gb-ui-chart-review',appNS='k8gb-ui-chart-app',release='review',fullname='k8gb-ui-review';
const labels={'k8gb-ui.io/lab-owner':state.owner};
const write=(file,value)=>fs.writeFileSync(file,typeof value==='string'?value:JSON.stringify(value,null,2)+'\n',{mode:0o600});
function create(c,obj){
 const args=[...(obj.metadata.namespace?['-n',obj.metadata.namespace]:[]),'get',obj.kind,obj.metadata.name,'--ignore-not-found','-o','json'];
 const raw=kube(c,args);if(raw.trim()){assert.equal(JSON.parse(raw).metadata.labels?.['k8gb-ui.io/lab-owner'],state.owner);return}
 obj.metadata.labels={...obj.metadata.labels,...labels};kube(c,['create','-f','-'],{input:JSON.stringify(obj)});
}
function snapshot(c){
 const rows=[];for(const namespace of ['demo','rbac-private'])for(const kind of ['gslbs.k8gb.io','ingresses.networking.k8s.io','ingressroutes.traefik.io'])for(const obj of JSON.parse(kube(c,['-n',namespace,'get',kind,'-o','json'])).items)rows.push({kind,namespace,name:obj.metadata.name,spec:obj.spec,annotations:obj.metadata.annotations});
 return crypto.createHash('sha256').update(JSON.stringify(rows)).digest('hex');
}
const arch=docker(['info','--format','{{.Architecture}}']).trim()==='aarch64'?'arm64':'amd64';
for(const component of ['console','agent','webhook']){
 run('go',['build','-trimpath','-o',path.join(dir,'bin',component),'./cmd/'+component],{cwd:root,env:{...process.env,GOOS:'linux',GOARCH:arch,CGO_ENABLED:'0'}});
 docker(['build','--build-arg','COMPONENT='+component,'-f',path.join(__dirname,'Dockerfile'),'-t',`k8gb-ui-lab-${component}:helm-${state.owner}`,path.join(dir,'bin')]);
}
const report={passed:false,clusters:[],profiles:['builtin Console','read-only native agent','writer + native site traffic + audit + optional webhook'],existingApplicationsPreserved:false};
for(const c of state.clusters){
 assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);const before=snapshot(c);
 run('kind',['load','docker-image',...['console','agent','webhook'].map(component=>`k8gb-ui-lab-${component}:helm-${state.owner}`),'--name',c.name],{env:{...process.env,DOCKER_CONTEXT:'desktop-linux',KIND_EXPERIMENTAL_PROVIDER:'docker'}});
 for(const name of [ns,appNS])create(c,{apiVersion:'v1',kind:'Namespace',metadata:{name}});
 const crt=path.join(priv,'webhook.crt'),key=path.join(priv,'webhook.key'),svc=fullname+'-webhook.'+ns+'.svc';
 if(!fs.existsSync(crt))run('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-days','14','-keyout',key,'-out',crt,'-subj','/CN='+svc,'-addext','subjectAltName=DNS:'+svc,'-addext','basicConstraints=critical,CA:TRUE'],{stdio:['ignore','pipe','pipe']});
 const secrets=[{name:'agent-auth',data:{token:crypto.randomBytes(32).toString('base64url'),'write-token':crypto.randomBytes(32).toString('base64url'),'plan-key':crypto.randomBytes(48).toString('base64url')}},{name:'ui-auth',data:{'users.yaml':fs.readFileSync(path.join(dir,'builtin-rbac/users.json'),'utf8'),'session-key':crypto.randomBytes(48).toString('base64url')}},{name:'connection',data:{token:crypto.randomBytes(32).toString('base64url')}},{name:'webhook-tls',data:{'tls.crt':fs.readFileSync(crt,'utf8'),'tls.key':fs.readFileSync(key,'utf8')}}];
 for(const s of secrets)create(c,{apiVersion:'v1',kind:'Secret',metadata:{namespace:ns,name:s.name},type:s.name==='webhook-tls'?'kubernetes.io/tls':'Opaque',stringData:s.data});
 create(c,{apiVersion:'v1',kind:'ConfigMap',metadata:{namespace:appNS,name:'k8gb-ui-audit',labels:{'k8gb-ui.io/audit-journal':'true'}},data:{}});
 // This checks Console readiness/config, not public agent Ingress connectivity.
 const readonly={fullnameOverride:fullname,console:{authSecretName:'ui-auth',image:{repository:'k8gb-ui-lab-console',tag:'helm-'+state.owner,pullPolicy:'Never'},auth:{publicURL:'https://ui.example.test',bindings:[{role:'viewer',groups:['team-demo'],clusters:[c.id],namespaces:[appNS]}]},secretMounts:[{name:'site',secretName:'connection',mountPath:'/run/site'}],clusters:[{id:c.id,url:'https://agent.example.test',zones:['apps.k8gb.test'],bearerTokenFile:'/run/site/token'}]},agent:{enabled:true,clusterID:c.id,namespaces:[appNS],authSecretName:'agent-auth',geoTags:['east','west'],image:{repository:'k8gb-ui-lab-agent',tag:'helm-'+state.owner,pullPolicy:'Never'}}};
 const values=path.join(priv,c.id+'.json');write(values,readonly);
 const helmArgs=['upgrade','--install',release,path.join(root,'charts/k8gb-ui'),'--namespace',ns,'--kubeconfig',path.join(dir,c.id+'.kubeconfig'),'--kube-context','kind-'+c.name,'-f',values,'--wait','--timeout','180s'];
 run('helm',helmArgs);
 const can=verb=>allowed(c,verb,'gslbs.k8gb.io',appNS);
 assert.equal(can('get'),'yes');assert.equal(can('patch'),'no');
 const writable=structuredClone(readonly);Object.assign(writable.agent,{writesEnabled:true,nativeSiteTraffic:true,audit:{enabled:true},replicas:2});
 writable.traefik={enabled:true,domains:['apps.k8gb.test'],ordinaryEntrypoints:['websecure'],mtlsEntrypoints:['secure-client'],tlsOptionNamespaces:[appNS],certauthzAlias:'certauthz'};
 writable.webhook={enabled:true,namespaces:[appNS],geoTags:['east','west'],tlsSecretName:'webhook-tls',caBundle:fs.readFileSync(crt).toString('base64'),image:{repository:'k8gb-ui-lab-webhook',tag:'helm-'+state.owner,pullPolicy:'Never'}};
 write(values,writable);run('helm',helmArgs);assert.equal(can('patch'),'yes');
 assert.equal(allowed(c,'patch','gslbs.k8gb.io','demo'),'no');
 assert.equal(allowed(c,'get','secrets',appNS),'no');
 // Real API-server dry-run invokes the chart's admission service without
 // reconciling persisted parents or producing DNS records.
 const route={apiVersion:'traefik.io/v1alpha1',kind:'IngressRoute',metadata:{namespace:appNS,name:'chart-check',annotations:{gslb:'true',gslb_hostname:'chart-check.apps.k8gb.test',gslb_strategy:'roundRobin'}},spec:{entryPoints:['websecure'],routes:[{kind:'Rule',match:'Host(`chart-check.local.k8gb.test`)',services:[{name:'unused',port:80}]}]}};
 let dry;
 for(let attempt=0;attempt<15;attempt++){
  try{dry=JSON.parse(execFileSync('kubectl',['--kubeconfig',path.join(dir,c.id+'.kubeconfig'),'--context','kind-'+c.name,'create','--dry-run=server','-f','-','-o','json'],{input:JSON.stringify(route),encoding:'utf8',timeout:20000,stdio:['pipe','pipe','pipe']}));break}
  catch(e){
   // Side-effect-free admission only: wait for Service routing propagation.
   // Persisted mutations and non-transport admission errors are never retried.
   if(attempt===14||!(/failed calling webhook/.test(e.stderr||'')&&/connection refused|no endpoints|i\/o timeout/.test(e.stderr||'')))throw Error('Chart admission dry-run failed');
   Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,1000);
  }
 }
 assert.equal(dry.metadata.annotations.gslb,'true');
 const cmBefore=JSON.parse(kube(c,['-n',appNS,'get','cm','k8gb-ui-audit','-o','json']));
 run('helm',helmArgs);
 const cmAfter=JSON.parse(kube(c,['-n',appNS,'get','cm','k8gb-ui-audit','-o','json']));assert.deepEqual(cmAfter.data,cmBefore.data);assert.equal(cmAfter.metadata.uid,cmBefore.metadata.uid);
 assert.equal(snapshot(c),before,'Existing app config changed');
 report.clusters.push({id:c.id,readOnlyDeniedWrites:true,namespaceWriteScope:true,secretsDenied:true,deploymentsReady:true,admissionDryRun:true,upgradePreservedJournal:true});
}
report.existingApplicationsPreserved=true;report.passed=true;
const out=path.join(root,'artifacts/helm-live');fs.mkdirSync(out,{recursive:true});fs.writeFileSync(path.join(out,'acceptance.json'),JSON.stringify(report,null,2)+'\n');
console.log('Helm local install/upgrade passed on east and west; existing services unchanged');
