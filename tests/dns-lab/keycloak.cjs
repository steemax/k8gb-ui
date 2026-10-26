'use strict';
// Explicit local kind lab only. Generated secrets stay outside the source tree.
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),https=require('node:https'),assert=require('node:assert/strict');
const {execFileSync,spawn}=require('node:child_process');
const action=process.argv[2],arg=process.argv.find(x=>x.startsWith('--state-dir='));
assert.ok(['install','oidc','builtin','status'].includes(action)&&arg&&path.isAbsolute(arg.slice(12)),'Usage: keycloak.cjs install|oidc|builtin|status --state-dir=/absolute/owned/lab');
const dir=arg.slice(12),stateFile=path.join(dir,'state.json'),state=JSON.parse(fs.readFileSync(stateFile)),priv=path.join(dir,'keycloak'),root=path.resolve(__dirname,'../..');
assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(state.builtinRbac?.publicURL,'https://127.0.0.1:8792');
for(const c of state.clusters)assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);
process.umask(0o077);fs.mkdirSync(priv,{recursive:true,mode:0o700});
const write=(file,value)=>fs.writeFileSync(file,typeof value==='string'?value:JSON.stringify(value,null,2)+'\n',{mode:0o600});
const save=()=>write(stateFile,state),wait=ms=>new Promise(r=>setTimeout(r,ms));
function run(cmd,args,opts={}){try{return execFileSync(cmd,args,{encoding:'utf8',timeout:180000,maxBuffer:8<<20,...opts})}catch(e){throw Error(`${cmd} failed (${e.status}); arguments/output omitted to protect credentials`)}}
const docker=args=>run('docker',['--context','desktop-linux',...args]);
function command(pid){try{return pid?run('ps',['-p',String(pid),'-o','command=']).trim():''}catch{return ''}}
async function stop(pid,expected){if(!command(pid))return;assert.equal(command(pid),expected,'Refusing unrelated process');process.kill(pid,'SIGTERM');for(let i=0;i<100;i++){if(!command(pid))return;await wait(100)}throw Error('Owned process did not stop')}
function detached(bin,args,log){const fd=fs.openSync(log,'a'),child=spawn(bin,args,{detached:true,stdio:['ignore',fd,fd]});child.unref();fs.closeSync(fd);return child.pid}
const ca=fs.readFileSync(path.join(dir,'builtin-rbac/ca.crt'));
function get(url){return new Promise((resolve,reject)=>{const r=https.get(url,{ca,timeout:3000},s=>{let body='';s.on('data',x=>body+=x);s.on('end',()=>resolve({status:s.statusCode,body}))});r.on('error',reject);r.on('timeout',()=>r.destroy())})}
async function ready(url,status){for(let i=0;i<180;i++){try{const r=await get(url);if(r.status===status)return r}catch{}await wait(500)}throw Error('Owned endpoint readiness timeout')}
function status(){const mode=JSON.parse(fs.readFileSync(path.join(dir,'console.json'))).auth.mode;console.log(JSON.stringify({mode,ui:state.consoleURL+'/?namespace=demo',keycloak:state.keycloak?.url,credentialsFile:path.join(dir,mode==='builtin'?'builtin-rbac':'keycloak','credentials.txt')},null,2))}
async function install(){
 if(state.keycloak)return;
 const current=JSON.parse(fs.readFileSync(path.join(dir,'console.json')));assert.equal(current.auth.mode,'builtin');write(path.join(priv,'console.builtin.json'),current);
 const accounts=[{username:'admin',groups:['platform-admins']},{username:'demo-reader',groups:['team-demo']},{username:'realm-admin-only',groups:[]}].map(u=>({...u,id:crypto.randomUUID(),password:crypto.randomBytes(24).toString('base64url')}));
 const bootstrap={username:'lab-keycloak-admin',password:crypto.randomBytes(24).toString('base64url')},secret=crypto.randomBytes(32).toString('base64url');
 write(path.join(priv,'credentials.json'),{accounts,bootstrap});write(path.join(priv,'credentials.txt'),accounts.map(x=>`${x.username}: ${x.password}`).join('\n')+`\nKeycloak admin console ${bootstrap.username}: ${bootstrap.password}\n`);
 write(path.join(priv,'client-secret'),secret);fs.writeFileSync(path.join(priv,'session-key'),crypto.randomBytes(48),{mode:0o600});
 const realm={realm:'k8gb-ui-lab',enabled:true,sslRequired:'external',registrationAllowed:false,resetPasswordAllowed:false,groups:[{name:'platform-admins'},{name:'team-demo'}],clients:[{clientId:'k8gb-ui',enabled:true,publicClient:false,secret,standardFlowEnabled:true,directAccessGrantsEnabled:false,serviceAccountsEnabled:false,redirectUris:[current.auth.publicURL+'/auth/callback'],webOrigins:[current.auth.publicURL],attributes:{'pkce.code.challenge.method':'S256'},protocolMappers:[{name:'groups',protocol:'openid-connect',protocolMapper:'oidc-group-membership-mapper',consentRequired:false,config:{'full.path':'false','claim.name':'groups','id.token.claim':'true','access.token.claim':'false','userinfo.token.claim':'false'}}]}],users:accounts.map(x=>({id:x.id,username:x.username,enabled:true,emailVerified:true,firstName:'Lab',lastName:x.username,email:x.username+'@example.test',groups:x.groups,credentials:[{type:'password',value:x.password,temporary:false}],...(x.username==='realm-admin-only'?{clientRoles:{'realm-management':['realm-admin']}}:{})}))};
 write(path.join(priv,'realm.json'),realm);write(path.join(priv,'keycloak.env'),`KC_BOOTSTRAP_ADMIN_USERNAME=${bootstrap.username}\nKC_BOOTSTRAP_ADMIN_PASSWORD=${bootstrap.password}\n`);
 state.keycloak={url:'https://127.0.0.1:8844',backendPort:18880,container:`k8gb-ui-keycloak-${state.owner}`,volume:`k8gb-ui-keycloak-${state.owner}`,image:'quay.io/keycloak/keycloak:26.5.2',proxyScript:path.join(priv,'proxy.cjs'),proxyExecutable:process.execPath};
 fs.copyFileSync(path.join(__dirname,'keycloak-proxy.cjs'),state.keycloak.proxyScript);
 const cfg={...current,auth:{mode:'oidc',issuerURL:state.keycloak.url+'/realms/k8gb-ui-lab',issuerCAFile:path.join(dir,'builtin-rbac/ca.crt'),clientID:'k8gb-ui',clientSecretFile:path.join(priv,'client-secret'),redirectURL:current.auth.publicURL+'/auth/callback',cookieKeyFile:path.join(priv,'session-key'),groupsClaim:'groups',bindings:current.auth.bindings}};
 write(path.join(priv,'console.oidc.json'),cfg);save();
}
async function ensureKeycloak(){
 const cfg=state.keycloak;assert.ok(cfg,'Run install first');
 let existing;try{existing=JSON.parse(docker(['container','inspect',cfg.container]))[0]}catch{}
 if(existing){assert.equal(existing.Config.Labels?.['k8gb-ui.io/lab-owner'],state.owner);if(!existing.State.Running)docker(['start',cfg.container])}
 else{
  docker(['volume','create','--label',`k8gb-ui.io/lab-owner=${state.owner}`,cfg.volume]);
  // Root is confined to this disposable dev container, with no host/docker socket.
  // It allows a 0600 host realm import; no permissive host secret file modes.
  docker(['run','-d','--name',cfg.container,'--label',`k8gb-ui.io/lab-owner=${state.owner}`,'--user','0','--cap-drop=ALL','--security-opt','no-new-privileges','--memory=1g','--cpus=2','-p','127.0.0.1:18880:8080','--env-file',path.join(priv,'keycloak.env'),'-v',`${cfg.volume}:/opt/keycloak/data`,'-v',`${path.join(priv,'realm.json')}:/opt/keycloak/data/import/k8gb-ui-lab-realm.json:ro`,cfg.image,'start-dev','--import-realm','--hostname='+cfg.url,'--proxy-headers=xforwarded']);
 }
 if(command(cfg.proxyPID))assert.equal(command(cfg.proxyPID),cfg.proxyExecutable+' '+cfg.proxyScript+' --state-dir='+dir);
 else{cfg.proxyPID=detached(cfg.proxyExecutable,[cfg.proxyScript,'--state-dir='+dir],path.join(priv,'proxy.log'));save()}
 const r=await ready(cfg.url+'/realms/k8gb-ui-lab/.well-known/openid-configuration',200),discovery=JSON.parse(r.body);assert.equal(discovery.issuer,cfg.url+'/realms/k8gb-ui-lab');assert.ok(discovery.token_endpoint.startsWith(cfg.url+'/'));
}
async function mode(mode){
 if(mode==='oidc')await ensureKeycloak();
 run('go',['build','-trimpath','-o',path.join(dir,'console-auth-next'),'./cmd/console'],{cwd:root});
 const cfg=JSON.parse(fs.readFileSync(path.join(priv,'console.'+mode+'.json')));assert.equal(cfg.auth.mode,mode);assert.equal(cfg.listen,'127.0.0.1:18891');
 await stop(state.consolePID,path.join(dir,'console')+' --config '+path.join(dir,'console.json'));
 fs.renameSync(path.join(dir,'console-auth-next'),path.join(dir,'console'));write(path.join(dir,'console.json'),cfg);
 state.consolePID=detached(path.join(dir,'console'),['--config',path.join(dir,'console.json')],path.join(dir,'console.log'));save();
 await ready(state.consoleURL+'/api/v1/session',401);status();
}
(async()=>{if(action==='status')status();else{if(action==='install')await install();await mode(action==='builtin'?'builtin':'oidc')}})().catch(e=>{console.error(e.message);process.exitCode=1});
