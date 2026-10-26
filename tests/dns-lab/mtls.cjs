'use strict';
// Explicitly owned LOCAL lab only. Builds local images; never pushes or resets
// existing GSLB policies. Private PKI stays under the supplied runtime directory.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),net=require('node:net');
const {execFileSync,spawn,spawnSync}=require('node:child_process');
const action=process.argv[2],arg=process.argv.find(value=>value.startsWith('--state-dir='));
if(!['install','verify'].includes(action)||!arg)throw Error('Usage: node tests/dns-lab/mtls.cjs install|verify --state-dir=/absolute/owned/lab');
const dir=arg.slice('--state-dir='.length),root=path.resolve(__dirname,'../..'),statePath=path.join(dir,'state.json');
if(!path.isAbsolute(dir))throw Error('Absolute state directory required');
const state=JSON.parse(fs.readFileSync(statePath,'utf8'));
if(state.version!==1||!/^[a-f0-9]{8}$/.test(state.owner)||state.clusters?.length!==2||state.clusters.some(c=>c.name!==`k8gb-ui-lab-${state.owner}-${c.id}`||!['east','west'].includes(c.id)))throw Error('Not an owned two-cluster lab');
process.umask(0o077);
const pki=path.join(dir,'mtls-pki');fs.mkdirSync(pki,{recursive:true,mode:0o700});
const run=(cmd,args,options={})=>{try{return execFileSync(cmd,args,{encoding:'utf8',maxBuffer:32<<20,timeout:360000,...options});}catch(error){throw Error(`${cmd} failed (status ${error.status}); arguments/credentials omitted`);}};
const docker=args=>run('docker',['--context',state.dockerContext,...args]);
const kube=(c,args,options)=>run('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,...args],options);
const get=(c,kind,name,namespace='demo')=>JSON.parse(kube(c,['-n',namespace,'get',kind,name,'-o','json']));
const owner={'k8gb-ui.lab-owner':state.owner,'k8gb-ui.lab-scenario':'mtls'};
const obj=(apiVersion,kind,namespace,name,other={})=>({apiVersion,kind,metadata:{namespace,name,labels:owner},...other});
const apply=(c,items)=>kube(c,['apply','-f','-'],{input:JSON.stringify({apiVersion:'v1',kind:'List',items})});
const wait=ms=>new Promise(resolve=>setTimeout(resolve,ms));
function guard(c,kind,name,namespace='demo'){
  const text=kube(c,['-n',namespace,'get',kind,name,'--ignore-not-found','-o','json']);
  if(text.trim()&&JSON.parse(text).metadata.labels?.['k8gb-ui.lab-owner']!==state.owner)throw Error(`Refusing to adopt unowned ${kind}/${name}`);
}
function snapshot(c){
  const data=[];
  for(const kind of ['ingressroutes.traefik.io','gslbs.k8gb.io']){
    for(const item of JSON.parse(kube(c,['-n','demo','get',kind,'-o','json'])).items){
      if(item.metadata.labels?.['k8gb-ui.lab-scenario']==='mtls'||item.metadata.name==='mtls-lab')continue;
      // Skip generated children of only this fixture, retaining every preexisting service.
      if(item.metadata.ownerReferences?.some(ref=>ref.kind==='IngressRoute'&&ref.name==='mtls-lab'))continue;
      data.push({kind,name:item.metadata.name,uid:item.metadata.uid,spec:item.spec,annotations:item.metadata.annotations});
    }
  }
  return crypto.createHash('sha256').update(JSON.stringify(data.sort((a,b)=>(a.kind+a.name).localeCompare(b.kind+b.name)))).digest('hex');
}
function certs(){
  if(fs.existsSync(path.join(pki,'ready')))return;
  const openssl=args=>run('openssl',args,{stdio:['ignore','pipe','pipe']});
  openssl(['req','-x509','-newkey','rsa:2048','-nodes','-days','14','-keyout',path.join(pki,'ca.key'),'-out',path.join(pki,'ca.crt'),'-subj',`/CN=k8gb-ui-lab-${state.owner}`,'-addext','basicConstraints=critical,CA:TRUE']);
  for(const [name,usage,san] of [['server','serverAuth','DNS:*.apps.k8gb.test,DNS:*.local.k8gb.test'],['client','clientAuth','DNS:client.demo.k8gb.test']]){
    openssl(['req','-new','-newkey','rsa:2048','-nodes','-keyout',path.join(pki,`${name}.key`),'-out',path.join(pki,`${name}.csr`),'-subj',`/CN=lab-${name}`]);
    fs.writeFileSync(path.join(pki,`${name}.ext`),`basicConstraints=critical,CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=${usage}\nsubjectAltName=${san}\n`);
    openssl(['x509','-req','-in',path.join(pki,`${name}.csr`),'-CA',path.join(pki,'ca.crt'),'-CAkey',path.join(pki,'ca.key'),'-set_serial',`0x${crypto.randomBytes(8).toString('hex')}`,'-days','14','-extfile',path.join(pki,`${name}.ext`),'-out',path.join(pki,`${name}.crt`)]);
  }
  openssl(['req','-x509','-newkey','rsa:2048','-nodes','-days','14','-keyout',path.join(pki,'untrusted.key'),'-out',path.join(pki,'untrusted.crt'),'-subj','/CN=untrusted-client','-addext','extendedKeyUsage=clientAuth','-addext','subjectAltName=DNS:client.demo.k8gb.test']);
  fs.writeFileSync(path.join(pki,'ready'),'private lab PKI');
}
function patchDeployment(c,name,image,addArgs,transform){
  const current=get(c,'deployment',name,'k8gb-ui-system'),containers=structuredClone(current.spec.template.spec.containers),container=containers.find(value=>value.name===name);
  if(!container)throw Error('Unexpected Deployment container');
  if(image&&!container.image.startsWith(`k8gb-ui-lab-${name==='k8gb-ui-agent'?'agent':'webhook'}:`))throw Error('Unexpected component image');
  for(const flag of addArgs){const key=flag.split('=')[0]+'=';container.args=(container.args||[]).filter(value=>!value.startsWith(key));container.args.push(flag);}
  if(image)container.image=image;
  if(transform)transform(container);
  const patches=[{op:'test',path:'/metadata/uid',value:current.metadata.uid},{op:'test',path:'/spec/template/spec/containers',value:current.spec.template.spec.containers},{op:'replace',path:'/spec/template/spec/containers',value:containers}];
  if(name==='traefik')patches.push({op:'replace',path:'/spec/strategy',value:{type:'Recreate'}});
  kube(c,['-n','k8gb-ui-system','patch','deployment',name,'--type=json','-p',JSON.stringify(patches)]);
  process.stdout.write(kube(c,['-n','k8gb-ui-system','rollout','status',`deployment/${name}`,'--timeout=180s']));
}
function install(){
  certs();const arch=/arm64|aarch64/.test(docker(['info','--format','{{.Architecture}}']))?'arm64':'amd64';
  const images={agent:`k8gb-ui-lab-agent:mtls-${state.owner}`,webhook:`k8gb-ui-lab-webhook:mtls-${state.owner}`};
  for(const component of ['agent','webhook']){
    run('go',['build','-trimpath','-o',path.join(dir,'bin',component),`./cmd/${component}`],{cwd:root,env:{...process.env,GOOS:'linux',GOARCH:arch,CGO_ENABLED:'0'}});
    docker(['build','--build-arg',`COMPONENT=${component}`,'-f',path.join(__dirname,'Dockerfile'),'-t',images[component],path.join(dir,'bin')]);
  }
  for(const c of state.clusters){
    const before=snapshot(c);
    for(const image of Object.values(images))run('kind',['load','docker-image',image,'--name',c.name],{env:{...process.env,DOCKER_CONTEXT:state.dockerContext,KIND_EXPERIMENTAL_PROVIDER:'docker'}});
    for(const [kind,name,ns] of [['Role','k8gb-ui-traefik-security','demo'],['RoleBinding','k8gb-ui-traefik-security','demo'],['Secret','mtls-lab-client-ca','demo'],['Secret','mtls-lab-server-tls','k8gb-ui-system'],['TLSOption','client-auth','demo'],['TLSOption','insecure-client-auth','demo'],['TLSStore','default','k8gb-ui-system'],['IngressRoute','mtls-lab','demo']])guard(c,kind,name,ns);
    const read=name=>fs.readFileSync(path.join(pki,name),'utf8');
    apply(c,[
      obj('rbac.authorization.k8s.io/v1','Role','demo','k8gb-ui-traefik-security',{rules:[{apiGroups:['traefik.io','traefik.containo.us'],resources:['tlsoptions'],verbs:['get','list']}]}),
      obj('rbac.authorization.k8s.io/v1','RoleBinding','demo','k8gb-ui-traefik-security',{roleRef:{apiGroup:'rbac.authorization.k8s.io',kind:'Role',name:'k8gb-ui-traefik-security'},subjects:['k8gb-ui-agent','k8gb-ui-traefik'].map(name=>({kind:'ServiceAccount',name,namespace:'k8gb-ui-system'}))}),
      obj('v1','Secret','demo','mtls-lab-client-ca',{type:'Opaque',stringData:{'ca.crt':read('ca.crt')}}),
      obj('v1','Secret','k8gb-ui-system','mtls-lab-server-tls',{type:'kubernetes.io/tls',stringData:{'tls.crt':read('server.crt'),'tls.key':read('server.key')}}),
      obj('traefik.io/v1alpha1','TLSOption','demo','client-auth',{spec:{clientAuth:{clientAuthType:'RequireAndVerifyClientCert',secretNames:['mtls-lab-client-ca']}}}),
      obj('traefik.io/v1alpha1','TLSOption','demo','insecure-client-auth',{spec:{clientAuth:{clientAuthType:'RequireAnyClientCert'}}}),
      obj('traefik.io/v1alpha1','TLSStore','k8gb-ui-system','default',{spec:{defaultCertificate:{secretName:'mtls-lab-server-tls'}}})
    ]);
    patchDeployment(c,'traefik',null,['--entrypoints.secure-client.address=:8443'],container=>{if(!container.ports.some(value=>value.containerPort===8443))container.ports.push({name:'mtls',containerPort:8443});});
    patchDeployment(c,'k8gb-ui-traefik',images.webhook,['--mtls-entrypoints=websecure,secure-client','--ordinary-entrypoints=websecure','--tls-option-namespaces=demo']);
    patchDeployment(c,'k8gb-ui-agent',images.agent,['--traefik-mtls-entrypoints=websecure,secure-client','--traefik-ordinary-entrypoints=websecure','--traefik-tls-option-namespaces=demo']);
    const source=kube(c,['-n','demo','get','ingressroute','mtls-lab','--ignore-not-found','-o','json']);
    if(!source.trim())apply(c,[obj('traefik.io/v1alpha1','IngressRoute','demo','mtls-lab',{metadata:{namespace:'demo',name:'mtls-lab',labels:owner,annotations:{gslb:'false',gslb_hostname:'mtls-lab.apps.k8gb.test',gslb_entrypoint:'websecure'}},spec:{entryPoints:['websecure'],routes:[{kind:'Rule',match:'Host(`mtls-lab.local.k8gb.test`)',services:[{name:'hello',port:80}]}]}})]);
    if(snapshot(c)!==before)throw Error(`Preexisting GSLB config changed on ${c.id}`);
    const check=spawnSync('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,'auth','can-i','get','secrets','--as=system:serviceaccount:k8gb-ui-system:k8gb-ui-agent','-n','demo'],{encoding:'utf8'});
    if(check.error||![0,1].includes(check.status)||check.stdout.trim()!=='no')throw Error('Agent unexpectedly has Secret read permission, or authorization check failed');
    console.log(`${c.id}: mTLS catalog/admission installed; existing GSLB configs preserved`);
  }
  const fresh=JSON.parse(fs.readFileSync(statePath,'utf8'));fresh.images={...fresh.images,...images};fresh.mtls={source:'mtls-lab',hostname:'mtls-lab.apps.k8gb.test',pkiDirectory:pki};fs.writeFileSync(statePath,JSON.stringify(fresh,null,2));
}
async function api(url,body){
  const response=await fetch(state.consoleURL+url,{method:body?'POST':'GET',headers:body?{'Content-Type':'application/json','Origin':state.consoleURL,'X-K8gb-Intent':'mutation'}:{},body:body?JSON.stringify(body):undefined,signal:AbortSignal.timeout(60000)});
  const value=await response.json();if(!response.ok)throw Error(`Console API returned ${response.status}`);return value;
}
async function update(enabled,entry,commonNames=[]){
  const targets=state.clusters.map(c=>{const u=get(c,'ingressroute','mtls-lab');return {clusterId:c.id,request:{provider:'traefik',operation:u.metadata.annotations.gslb==='true'||u.metadata.annotations['k8gb-ui.io/gslb']==='true'?'update':'enable',resource:{apiVersion:u.apiVersion,kind:u.kind,namespace:'demo',name:u.metadata.name,uid:u.metadata.uid,resourceVersion:u.metadata.resourceVersion},policy:{strategy:'roundRobin',dnsTtlSeconds:5},traefik:{hostname:'mtls-lab.apps.k8gb.test',entrypoint:'websecure',mtls:enabled,mtlsEntrypoint:enabled?entry:undefined,tlsOption:enabled?{namespace:'demo',name:'client-auth'}:undefined,commonNames:enabled?commonNames:[],local:false},reason:`Local mTLS acceptance: ${enabled?entry:'restore ordinary routing'}`,dnsZone:u.metadata.annotations.gslb==='true'||u.metadata.annotations['k8gb-ui.io/gslb']==='true'?undefined:'apps.k8gb.test'}};});
  const planned=await api('/api/v1/mutations/plan',{targets});
  if(!planned.canApply||planned.targets.some(row=>row.error))throw Error(`mTLS plan rejected: ${planned.targets.map(row=>row.error?.error||'ok').join('; ')}`);
  const applied=await api('/api/v1/mutations/apply',{targets:planned.targets,allowPartial:false});
  if(applied.targets.some(row=>row.error))throw Error('mTLS apply did not succeed on every site');
}
async function tlsProbe(c,remotePort){
 for(const identity of ['client','none','untrusted']){
  const free=net.createServer();await new Promise(resolve=>free.listen(0,'127.0.0.1',resolve));const port=free.address().port;await new Promise(resolve=>free.close(resolve));
  const child=spawn('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,'-n','k8gb-ui-system','port-forward','deployment/traefik',`${port}:${remotePort}`,'--address=127.0.0.1'],{stdio:['ignore','ignore','ignore']});
  try{
    const base=['--http1.1','--silent','--show-error','--fail','--connect-timeout','2','--max-time','4','--noproxy','*','--cacert',path.join(pki,'ca.crt'),'--connect-to',`mtls-lab.apps.k8gb.test:443:127.0.0.1:${port}`];
    let body='';for(let i=0;i<50;i++){try{body=execFileSync('curl',[...base,'--cert',path.join(pki,'client.crt'),'--key',path.join(pki,'client.key'),'https://mtls-lab.apps.k8gb.test/'],{encoding:'utf8',stdio:['ignore','pipe','pipe']});if(body.includes(`hello from ${c.id}`))break;}catch{}await wait(500);}
    if(!body.includes(`hello from ${c.id}`))throw Error(`Trusted client handshake/routing failed on ${c.id}`);
    if(identity!=='client'){
      let failed=false;try{execFileSync('curl',[...base,...(identity==='none'?[]:['--cert',path.join(pki,'untrusted.crt'),'--key',path.join(pki,'untrusted.key')]),'https://mtls-lab.apps.k8gb.test/'],{stdio:['ignore','pipe','pipe']});}catch(error){failed=[35,56].includes(error.status)&&(identity==='none'?/certificate required|reason\(1116\)/i:/alert (unknown ca|bad certificate|certificate unknown)/i).test(String(error.stderr));}
      if(!failed)throw Error(`${identity} client unexpectedly accepted on ${c.id}`);
    }
  }finally{child.kill('SIGTERM');await new Promise(resolve=>{if(child.exitCode!==null)return resolve();child.once('exit',resolve);});}
 }
 return {cluster:c.id,trustedClient:true,noCertificateRejected:true,untrustedCARejected:true,serverCertificateVerified:true};
}
async function verify(){
  const results=[];const before=Object.fromEntries(state.clusters.map(c=>[c.id,snapshot(c)]));
  const catalog=await api('/api/v1/catalog?namespace=demo');
  const lists=state.clusters.map(c=>{
    const candidate=catalog.sites.find(site=>site.clusterId===c.id)?.catalog?.candidates?.find(row=>row.resource.name==='mtls-lab');
    if(!candidate?.traefik)throw Error(`Fixture security catalog unavailable on ${c.id}; refusing to overwrite DNS SAN settings`);
    return candidate.traefik.commonNames||[];
  });
  if(lists.some(names=>JSON.stringify(names)!==JSON.stringify(lists[0])))throw Error('Fixture DNS SAN policies differ across sites; refusing to overwrite them');
  for(const [entry,port] of [['secure-client',8443],['websecure',8080]]){
    await update(true,entry,lists[0]);
    for(const c of state.clusters)results.push({entrypoint:entry,...await tlsProbe(c,port)});
  }
  await update(false);
  for(const c of state.clusters){
    const u=get(c,'ingressroute','mtls-lab');
    if(JSON.stringify(u.spec.entryPoints)!=='["websecure"]'||u.spec.tls!==undefined||u.metadata.annotations['k8gb-ui.io/original-tls-options']||u.metadata.annotations['k8gb-ui.io/original-entrypoints'])throw Error('Original source routing/TLS not restored');
    let ok=false;for(let i=0;i<40;i++){try{ok=run('curl',['--silent','--fail','--noproxy','*','-H','Host: mtls-lab.apps.k8gb.test',`http://127.0.0.1:${c.httpPort}`]).includes(`hello from ${c.id}`);if(ok)break;}catch{}await wait(500)}if(!ok)throw Error('Ordinary routing not restored');
  }
  await update(true,'secure-client',lists[0]);
  for(const c of state.clusters){if(snapshot(c)!==before[c.id])throw Error('Preexisting GSLB config changed during acceptance');}
  const out=path.join(root,'artifacts/mtls-live');fs.mkdirSync(out,{recursive:true});fs.writeFileSync(path.join(out,'acceptance.json'),JSON.stringify({results,ordinaryRoutingRestored:true,finalEntrypoint:'secure-client',preexistingConfigsPreserved:true,certauthzRuntimeTested:false},null,2));
  console.log(JSON.stringify({url:state.consoleURL+'/?namespace=demo',hostname:'mtls-lab.apps.k8gb.test',results,ordinaryRoutingRestored:true,finalEntrypoint:'secure-client',certauthzRuntimeTested:false},null,2));
}
(async()=>{if(action==='install')install();else await verify();})().catch(error=>{console.error(error.message);process.exitCode=1});
