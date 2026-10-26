'use strict';
// Explicitly owned local kind lab only. Installs isolated RBAC fixtures and
// real builtin authentication; no default context, plaintext auth exception or Git.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),http=require('node:http'),https=require('node:https'),net=require('node:net'),assert=require('node:assert/strict');
const {execFileSync,spawn}=require('node:child_process');
const action=process.argv[2],arg=process.argv.find(value=>value.startsWith('--state-dir='));
if(!['install','start','status'].includes(action)||!arg||!path.isAbsolute(arg.slice(12)))throw Error('Usage: builtin-rbac.cjs install|start|status --state-dir=/absolute/owned/lab');
const dir=arg.slice(12),stateFile=path.join(dir,'state.json'),privateDir=path.join(dir,'builtin-rbac'),root=path.resolve(__dirname,'../..');
let state=JSON.parse(fs.readFileSync(stateFile));
assert.equal(state.version,1);assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(state.clusters.length,2);
for(const c of state.clusters){assert.ok(['east','west'].includes(c.id));assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`)}
assert.equal(new Set(state.clusters.map(c=>c.id)).size,2);
process.umask(0o077);fs.mkdirSync(privateDir,{recursive:true,mode:0o700});
const write=(file,value)=>fs.writeFileSync(file,typeof value==='string'?value:JSON.stringify(value,null,2)+'\n',{mode:0o600});
const save=()=>write(stateFile,state);
const run=(cmd,args,options={})=>{try{return execFileSync(cmd,args,{encoding:'utf8',maxBuffer:16<<20,timeout:360000,...options})}catch(error){throw Error(`${cmd} failed (status ${error.status}); credentials and arguments omitted`)}};
const kube=(c,args,options)=>run('kubectl',['--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--context',`kind-${c.name}`,...args],options);
const get=(c,kind,name,ns)=>{const raw=kube(c,[...(ns?['-n',ns]:[]),'get',kind,name,'--ignore-not-found','-o','json']);return raw.trim()?JSON.parse(raw):null};
const wait=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const labels={'k8gb-ui.io/lab-owner':state.owner,'k8gb-ui.io/lab-scenario':'builtin-rbac'};
const object=(apiVersion,kind,ns,name,other={})=>({apiVersion,kind,metadata:{...(ns?{namespace:ns}:{}),name,labels},...other});
function create(c,kind,item){
 const old=get(c,kind,item.metadata.name,item.metadata.namespace);
 if(old){assert.equal(old.metadata.labels?.['k8gb-ui.io/lab-owner'],state.owner,`Refusing unowned ${kind}/${item.metadata.name}`);return}
 kube(c,['create','-f','-'],{input:JSON.stringify(item)});
}
function snapshot(c){
 const rows=[];
 for(const kind of ['gslbs.k8gb.io','ingresses.networking.k8s.io','ingressroutes.traefik.io'])for(const u of JSON.parse(kube(c,['-n','demo','get',kind,'-o','json'])).items){
  if(u.metadata.labels?.['k8gb-ui.io/lab-scenario']==='builtin-rbac')continue;
  rows.push({kind,name:u.metadata.name,uid:u.metadata.uid,spec:u.spec,annotations:u.metadata.annotations});
 }
 return crypto.createHash('sha256').update(JSON.stringify(rows.sort((a,b)=>(a.kind+a.name).localeCompare(b.kind+b.name)))).digest('hex');
}
function fixtures(c){
 create(c,'namespaces',object('v1','Namespace','','rbac-private'));
 const ns='rbac-private';
 create(c,'configmaps',object('v1','ConfigMap',ns,'k8gb-ui-audit',{metadata:{namespace:ns,name:'k8gb-ui-audit',labels:{...labels,'k8gb-ui.io/audit-journal':'true'}},data:{}}));
 create(c,'roles',object('rbac.authorization.k8s.io/v1','Role',ns,'k8gb-ui-agent-audit',{rules:[{apiGroups:[''],resources:['configmaps'],resourceNames:['k8gb-ui-audit'],verbs:['get','update']}]}));
 create(c,'rolebindings',object('rbac.authorization.k8s.io/v1','RoleBinding',ns,'k8gb-ui-agent-audit',{roleRef:{apiGroup:'rbac.authorization.k8s.io',kind:'Role',name:'k8gb-ui-agent-audit'},subjects:[{kind:'ServiceAccount',name:'k8gb-ui-agent',namespace:'k8gb-ui-system'}]}));
 create(c,'configmaps',object('v1','ConfigMap',ns,'rbac-web',{data:{'index.html':`<h1>private RBAC fixture on ${c.id}</h1>\n`}}));
 create(c,'deployments',object('apps/v1','Deployment',ns,'rbac-web',{spec:{replicas:1,selector:{matchLabels:{app:'rbac-web'}},template:{metadata:{labels:{app:'rbac-web'}},spec:{containers:[{name:'nginx',image:'nginx:1.28-alpine',imagePullPolicy:'Never',ports:[{containerPort:80}],volumeMounts:[{name:'html',mountPath:'/usr/share/nginx/html',readOnly:true}],resources:{requests:{cpu:'10m',memory:'8Mi'},limits:{cpu:'100m',memory:'32Mi'}}}],volumes:[{name:'html',configMap:{name:'rbac-web'}}]}}}}));
 create(c,'services',object('v1','Service',ns,'rbac-web',{spec:{selector:{app:'rbac-web'},ports:[{port:80,targetPort:80}]}}));
 for(const [namespace,name,service] of [['demo','rbac-demo','hello'],[ns,'rbac-private','rbac-web']]){
  create(c,'ingresses.networking.k8s.io',object('networking.k8s.io/v1','Ingress',namespace,name,{metadata:{namespace,name,labels,annotations:{'traefik.ingress.kubernetes.io/router.entrypoints':'websecure'}},spec:{ingressClassName:'traefik',rules:[{host:`${name}.apps.k8gb.test`,http:{paths:[{path:'/',pathType:'Prefix',backend:{service:{name:service,port:{number:80}}}}]}}]}}));
  create(c,'gslbs.k8gb.io',object('k8gb.io/v1beta1','Gslb',namespace,name,{spec:{ingress:{},resourceRef:{apiVersion:'networking.k8s.io/v1',kind:'Ingress',name},serviceHealthPolicy:'Any',strategy:{type:'roundRobin',dnsTtlSeconds:5}}}));
 }
 process.stdout.write(kube(c,['-n',ns,'rollout','status','deployment/rbac-web','--timeout=120s']));
 const deployment=get(c,'deployments','k8gb-ui-agent','k8gb-ui-system'),containers=structuredClone(deployment.spec.template.spec.containers),container=containers.find(row=>row.name==='k8gb-ui-agent');
 assert.ok(container.image.startsWith('k8gb-ui-lab-agent:'));
 const oldNamespaces=container.args.find(value=>value.startsWith('--allowed-namespaces='))?.split('=')[1];
 assert.ok(['demo','demo,rbac-private'].includes(oldNamespaces),'Unexpected agent namespace scope');
 container.args=container.args.filter(value=>!value.startsWith('--allowed-namespaces='));container.args.push('--allowed-namespaces=demo,rbac-private');
 if(JSON.stringify(containers)!==JSON.stringify(deployment.spec.template.spec.containers))kube(c,['-n','k8gb-ui-system','patch','deployment','k8gb-ui-agent','--type=json','-p',JSON.stringify([{op:'test',path:'/metadata/uid',value:deployment.metadata.uid},{op:'test',path:'/spec/template/spec/containers',value:deployment.spec.template.spec.containers},{op:'replace',path:'/spec/template/spec/containers',value:containers}])]);
 process.stdout.write(kube(c,['-n','k8gb-ui-system','rollout','status','deployment/k8gb-ui-agent','--timeout=180s']));
}
function credentials(){
 const ready=path.join(privateDir,'credentials.json');
 if(fs.existsSync(ready)){fs.accessSync(path.join(privateDir,'users.json'));fs.accessSync(path.join(privateDir,'session-key'));return}
 const accounts=[{username:'admin',groups:['platform-admins']},{username:'demo-reader',groups:['team-demo']}].map(account=>({...account,password:crypto.randomBytes(24).toString('base64url')}));
 const users=accounts.map(account=>({username:account.username,groups:account.groups,passwordHash:run(path.join(dir,'password-hash'),[],{input:account.password+'\n'}).trim()}));
 write(path.join(privateDir,'users.json'),{users});write(path.join(privateDir,'session-key'),crypto.randomBytes(32).toString('base64url'));
 write(ready,{accounts});write(path.join(privateDir,'credentials.txt'),`PRIVATE local lab credentials. Never commit or publish.\nURL: https://127.0.0.1:8792/?namespace=demo\n\n${accounts.map(account=>`Username: ${account.username}\nPassword: ${account.password}\nRole: ${account.username==='admin'?'admin in both configured namespaces/sites':'viewer, demo only'}\n`).join('\n')}`);
}
function certificates(){
 if(fs.existsSync(path.join(privateDir,'server.crt'))){for(const file of ['server.key','ca.crt'])fs.accessSync(path.join(privateDir,file));return}
 const openssl=args=>run('openssl',args,{stdio:['ignore','pipe','pipe']});
 openssl(['req','-x509','-newkey','rsa:2048','-nodes','-days','30','-keyout',path.join(privateDir,'ca.key'),'-out',path.join(privateDir,'ca.crt'),'-subj',`/CN=k8gb-ui-builtin-lab-${state.owner}`,'-addext','basicConstraints=critical,CA:TRUE']);
 openssl(['req','-new','-newkey','rsa:2048','-nodes','-keyout',path.join(privateDir,'server.key'),'-out',path.join(privateDir,'server.csr'),'-subj','/CN=localhost']);
 write(path.join(privateDir,'server.ext'),'basicConstraints=critical,CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost\n');
 openssl(['x509','-req','-in',path.join(privateDir,'server.csr'),'-CA',path.join(privateDir,'ca.crt'),'-CAkey',path.join(privateDir,'ca.key'),'-set_serial',`0x${crypto.randomBytes(8).toString('hex')}`,'-days','14','-extfile',path.join(privateDir,'server.ext'),'-out',path.join(privateDir,'server.crt')]);
}
async function free(port){await new Promise((resolve,reject)=>{const server=net.createServer();server.once('error',()=>reject(Error(`Required loopback port ${port} is occupied`)));server.listen(port,'127.0.0.1',()=>server.close(resolve))})}
function detached(binary,args,log){const fd=fs.openSync(log,'a'),child=spawn(binary,args,{detached:true,stdio:['ignore',fd,fd]});child.unref();fs.closeSync(fd);return child.pid}
const processCommand=pid=>{if(!Number.isInteger(pid)||pid<=0)return '';try{return run('ps',['-p',String(pid),'-o','command=']).trim()}catch{return ''}};
async function stopOwnedConsole(pid){
 if(!pid||!processCommand(pid))return;
 assert.equal(processCommand(pid),path.join(dir,'console')+' --config '+path.join(dir,'console.json'),'Refusing to stop another process');process.kill(pid,'SIGTERM');
 for(let i=0;i<100;i++){if(!processCommand(pid))return;await wait(100)}throw Error('Owned Console did not stop');
}
function request(url){return new Promise((resolve,reject)=>{
 const client=url.startsWith('https:')?https:http;
 const req=client.get(url,{...(client===https?{ca:fs.readFileSync(path.join(privateDir,'ca.crt'))}:{}),timeout:5000},response=>{response.resume();response.once('end',()=>resolve(response.statusCode))});req.on('error',reject);req.on('timeout',()=>req.destroy(Error('Local readiness timeout')));
})}
async function ready(url,status=200){for(let i=0;i<100;i++){try{if(await request(url)===status)return}catch{}await wait(100)}throw Error('Local authenticated endpoint did not become ready')}
async function start(){
 assert.ok(state.builtinRbac,'Install builtin RBAC first');
 const current=processCommand(state.consolePID),proxy=processCommand(state.builtinRbac.proxyPID);
 if(current&&proxy){
  assert.equal(current,path.join(dir,'console')+' --config '+path.join(dir,'console.json'));
  assert.equal(proxy,state.builtinRbac.proxyExecutable+' '+state.builtinRbac.proxyScript+' --state-dir='+dir);
  await ready(state.builtinRbac.publicURL+'/api/v1/session',401);status();return;
 }
 assert.ok(!current&&!proxy,'Only one RBAC component is running; inspect owned processes before restart');
 state.consolePID=detached(path.join(dir,'console'),['--config',path.join(dir,'console.json')],path.join(dir,'console.log'));save();
 await ready(`http://127.0.0.1:${state.builtinRbac.backendPort}/api/v1/session`,401);
 state.builtinRbac.proxyExecutable=process.execPath;
 state.builtinRbac.proxyPID=detached(process.execPath,[state.builtinRbac.proxyScript,`--state-dir=${dir}`],path.join(privateDir,'proxy.log'));save();
 await ready(state.builtinRbac.publicURL+'/api/v1/session',401);status();
}
function status(){const authMode=JSON.parse(fs.readFileSync(path.join(dir,'console.json'))).auth.mode;console.log(JSON.stringify({url:state.builtinRbac?.publicURL+'/?namespace=demo',authMode,credentialsFile:path.join(dir,authMode==='oidc'?'keycloak':'builtin-rbac','credentials.txt'),consolePID:state.consolePID,proxyPID:state.builtinRbac?.proxyPID},null,2))}
async function install(){
 if(state.builtinRbac){await start();return}
 assert.equal(state.consoleURL,'http://127.0.0.1:8791');
 for(const port of [8792,18891,19791,19792])await free(port);
 const before=Object.fromEntries(state.clusters.map(c=>[c.id,snapshot(c)]));
 const audit=await fetch(state.consoleURL+'/api/v1/audit?namespace=demo');assert.equal(audit.status,200);write(path.join(privateDir,'audit-before.json'),await audit.json());
 run('go',['build','-trimpath','-o',path.join(dir,'password-hash'),'./cmd/password-hash'],{cwd:root});credentials();certificates();
 run('go',['build','-trimpath','-o',path.join(dir,'console-builtin-next'),'./cmd/console'],{cwd:root});
 for(const c of state.clusters){fixtures(c);assert.equal(snapshot(c),before[c.id],`Existing application config changed on ${c.id}`)}
 const previous=JSON.parse(fs.readFileSync(path.join(dir,'console.json')));assert.equal(previous.auth.mode,'local');assert.equal(previous.listen,'127.0.0.1:8791');
 write(path.join(privateDir,'console-before.json'),previous);
 fs.copyFileSync(path.join(__dirname,'builtin-proxy.cjs'),path.join(privateDir,'proxy.cjs'));
 state.builtinRbac={publicURL:'https://127.0.0.1:8792',backendPort:18891,agentTLSPorts:{east:19791,west:19792},proxyScript:path.join(privateDir,'proxy.cjs'),privateNamespace:'rbac-private'};
 const config={...previous,listen:'127.0.0.1:18891',auth:{mode:'builtin',publicURL:state.builtinRbac.publicURL,cookieKeyFile:path.join(privateDir,'session-key'),usersFile:path.join(privateDir,'users.json'),bindings:[{groups:['platform-admins'],role:'admin',clusters:['*'],namespaces:['*']},{groups:['team-demo'],role:'viewer',clusters:['east','west'],namespaces:['demo']}]},clusters:previous.clusters.map(c=>({...c,url:`https://127.0.0.1:${state.builtinRbac.agentTLSPorts[c.id]}`,caFile:path.join(privateDir,'ca.crt'),namespaces:['demo','rbac-private']}))};
 const oldPID=state.consolePID;
 await stopOwnedConsole(oldPID);delete state.consolePID;
 fs.renameSync(path.join(dir,'console-builtin-next'),path.join(dir,'console'));write(path.join(dir,'console.json'),config);
 state.consoleURL=state.builtinRbac.publicURL;save();
 await start();
}
(async()=>{if(action==='install')await install();else if(action==='start')await start();else status()})().catch(error=>{console.error(error.message);process.exitCode=1});
