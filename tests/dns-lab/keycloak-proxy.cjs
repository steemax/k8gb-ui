'use strict';
// Owned loopback IdP only; overwrite forwarded headers, never trust the caller.
const fs=require('node:fs'),path=require('node:path'),https=require('node:https'),http=require('node:http'),assert=require('node:assert/strict');
const arg=process.argv.find(x=>x.startsWith('--state-dir='));assert.ok(arg&&path.isAbsolute(arg.slice(12)));
const dir=arg.slice(12),state=JSON.parse(fs.readFileSync(path.join(dir,'state.json'))),cfg=state.keycloak;
assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(cfg?.url,'https://127.0.0.1:8844');assert.equal(cfg.backendPort,18880);
const server=https.createServer({key:fs.readFileSync(path.join(dir,'builtin-rbac/server.key')),cert:fs.readFileSync(path.join(dir,'builtin-rbac/server.crt')),minVersion:'TLSv1.2'},(req,res)=>{
 if(req.headers.host!=='127.0.0.1:8844'||!req.url.startsWith('/')||req.url.startsWith('//'))return res.writeHead(400).end();
 const headers={...req.headers};for(const key of Object.keys(headers))if(key.startsWith('x-forwarded-')||['forwarded','connection','proxy-authorization','upgrade'].includes(key))delete headers[key];
 Object.assign(headers,{'x-forwarded-proto':'https','x-forwarded-host':'127.0.0.1:8844','x-forwarded-port':'8844','x-forwarded-for':'127.0.0.1'});
 const remote=http.request({hostname:'127.0.0.1',port:18880,path:req.url,method:req.method,headers},upstream=>{res.writeHead(upstream.statusCode,upstream.headers);upstream.pipe(res)});
 remote.on('error',()=>{if(!res.headersSent)res.writeHead(502).end();else res.destroy()});remote.setTimeout(30000,()=>remote.destroy());req.on('aborted',()=>remote.destroy());req.pipe(remote);
});
server.listen(8844,'127.0.0.1',()=>console.log('Owned Keycloak HTTPS proxy ready'));
for(const signal of ['SIGTERM','SIGINT'])process.on(signal,()=>{server.close();server.closeAllConnections();setTimeout(()=>process.exit(0),100).unref()});
