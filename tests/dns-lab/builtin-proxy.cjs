'use strict';
// Loopback-only TLS termination for the owned authentication lab. No user,
// group or agent token is injected by this proxy; Console enforces identity.
const fs=require('node:fs'),path=require('node:path'),http=require('node:http'),https=require('node:https'),assert=require('node:assert/strict');
const arg=process.argv.find(value=>value.startsWith('--state-dir='));
if(!arg||!path.isAbsolute(arg.slice(12)))throw Error('Explicit absolute --state-dir required');
const dir=arg.slice(12),state=JSON.parse(fs.readFileSync(path.join(dir,'state.json'))),cfg=state.builtinRbac;
assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(cfg?.publicURL,'https://127.0.0.1:8792');
for(const c of state.clusters)assert.equal(c.name,`k8gb-ui-lab-${state.owner}-${c.id}`);
const tls={key:fs.readFileSync(path.join(dir,'builtin-rbac/server.key')),cert:fs.readFileSync(path.join(dir,'builtin-rbac/server.crt')),minVersion:'TLSv1.2'};
const servers=[];
function forward(port,upstream){
 assert.ok(Number.isInteger(upstream)&&upstream>1024&&upstream<65536);
 const server=https.createServer(tls,(request,response)=>{
  if(request.headers.host!==`127.0.0.1:${port}`||!request.url.startsWith('/')||request.url.startsWith('//')){response.writeHead(400).end();return}
  const headers={...request.headers};for(const key of ['connection','proxy-authorization','proxy-connection','upgrade'])delete headers[key];
  const upstreamRequest=http.request({hostname:'127.0.0.1',port:upstream,path:request.url,method:request.method,headers},remote=>{
   response.writeHead(remote.statusCode,remote.headers);remote.pipe(response);
   remote.on('error',()=>response.destroy());
  });
  upstreamRequest.setTimeout(60000,()=>upstreamRequest.destroy());
  upstreamRequest.on('error',()=>{if(!response.headersSent)response.writeHead(502,{'Content-Type':'text/plain'}).end('Local lab upstream unavailable');else response.destroy()});
  request.on('aborted',()=>upstreamRequest.destroy());request.pipe(upstreamRequest);
 });
 server.requestTimeout=90000;server.headersTimeout=10000;return [server,port];
}
const redirect=http.createServer((request,response)=>{
 if(!['GET','HEAD'].includes(request.method)||!request.url.startsWith('/')||request.url.startsWith('//')){response.writeHead(403).end('Use the authenticated HTTPS lab');return}
 response.writeHead(302,{Location:cfg.publicURL+request.url,'Cache-Control':'no-store'}).end();
});
const listeners=[[redirect,8791],forward(8792,cfg.backendPort),...state.clusters.map(c=>forward(cfg.agentTLSPorts[c.id],c.agentPort))];
(async()=>{
 for(const [server,port] of listeners){await new Promise((resolve,reject)=>{server.once('error',reject);server.listen(port,'127.0.0.1',resolve)});servers.push(server)}
 fs.writeFileSync(path.join(dir,'builtin-rbac/proxy-ready.json'),JSON.stringify({owner:state.owner,pid:process.pid,publicURL:cfg.publicURL}),{mode:0o600});
 console.log('Owned builtin RBAC TLS proxy ready on loopback');
})().catch(()=>{console.error('Cannot start owned builtin RBAC TLS proxy');for(const server of servers)server.close();process.exitCode=1});
let stopping=false;
function stop(){if(stopping)return;stopping=true;for(const server of servers){server.close();server.closeAllConnections()}setTimeout(()=>process.exit(0),100).unref()}
process.on('SIGTERM',stop);process.on('SIGINT',stop);
