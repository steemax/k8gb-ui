// Disposable proof that an invalid routing reference is not DNS exclusion.
'use strict';
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto'),assert=require('node:assert/strict');
const {execFileSync:run}=require('node:child_process');
const dir=process.argv.find(arg=>arg.startsWith('--state-dir='))?.slice(12);
if(!dir || !path.isAbsolute(dir))throw Error('Use --state-dir=/absolute/owned-lab-runtime');
const state=JSON.parse(fs.readFileSync(path.join(dir,'state.json'))),cluster=state.clusters.find(c=>c.id==='east');
assert.match(state.owner,/^[a-f0-9]{8}$/);assert.equal(cluster.name,`k8gb-ui-lab-${state.owner}-east`);
const args=['--kubeconfig',path.join(dir,'east.kubeconfig'),'--context','kind-'+cluster.name,'-n','demo'];
const kubectl=(argv,input)=>run('kubectl',[...args,...argv],{input:input&&JSON.stringify(input),encoding:'utf8',timeout:45000});
const name='reference-probe-'+crypto.randomBytes(4).toString('hex'),hostname=name+'.apps.k8gb.test';
const report={hostname,startedAt:new Date().toISOString(),resources:[],status:'RUNNING'};
const created=[];
const wait=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const get=(kind)=>JSON.parse(kubectl(['get',kind,name,'-o','json']));
const dns=()=>[false,true].map(tcp=>({protocol:tcp?'tcp':'udp',raw:run('dig',['@127.0.0.1','-p',String(cluster.dnsPort),hostname,'A','+noall','+answer','+time=2','+tries=1',...(tcp?['+tcp']:[])],{encoding:'utf8'})}));
const ingress={apiVersion:'networking.k8s.io/v1',kind:'Ingress',metadata:{name,namespace:'demo',labels:{'k8gb-ui.io/lab-owner':state.owner}},spec:{ingressClassName:'traefik',rules:[{host:hostname,http:{paths:[{path:'/',pathType:'Prefix',backend:{service:{name:'hello',port:{number:80}}}}]}}]}};
const gslb={apiVersion:'k8gb.io/v1beta1',kind:'Gslb',metadata:{name,namespace:'demo',labels:{'k8gb-ui.io/lab-owner':state.owner}},spec:{resourceRef:{apiVersion:'networking.k8s.io/v1',kind:'Ingress',name},strategy:{type:'roundRobin',dnsTtlSeconds:5}}};
(async()=>{
  try {
    for(const object of [ingress,gslb]){const current=JSON.parse(kubectl(['create','-f','-','-o','json'],object));created.push(current);}
    let endpoint;
    for(let n=0;n<50;n++){try{endpoint=get('dnsendpoints.externaldns.k8s.io');if(dns().every(sample=>sample.raw.includes(cluster.ip)))break;}catch{}await wait(1000);}
    assert.ok(endpoint);report.before=dns();assert.ok(report.before.every(sample=>sample.raw.includes(cluster.ip)));
    const live=get('gslbs.k8gb.io');
    kubectl(['patch','gslbs.k8gb.io',name,'--type=json','-p',JSON.stringify([{op:'test',path:'/metadata/uid',value:live.metadata.uid},{op:'test',path:'/spec/resourceRef/name',value:name},{op:'replace',path:'/spec/resourceRef/name',value:name+'-absent'}])]);
    await wait(20000);
    const after=get('dnsendpoints.externaldns.k8s.io');report.after=dns();
    report.events=JSON.parse(kubectl(['get','events','--field-selector','involvedObject.name='+name,'-o','json'])).items.map(event=>({reason:event.reason,message:event.message}));
    assert.deepEqual(after.spec,endpoint.spec,'Unexpected DNSEndpoint change');
    assert.ok(report.after.every(sample=>sample.raw.includes(cluster.ip)),'Old authoritative A answer was not preserved');
    assert.ok(report.events.some(event=>event.reason==='ReconcileError' && /resolving references/.test(event.message)),'Reference failure not observed');
    report.status='PASS';report.finding='Missing Ingress stops reconciliation; old DNSEndpoint and authoritative DNS remain after four TTL periods. It is not Exclude site.';
  }catch(error){report.status='FAIL';report.error=error.message;process.exitCode=1;}
  finally {
    report.cleanup=[];
    for(const object of created.reverse()) {
      try {
        const group=object.kind==='Gslb'?'k8gb.io/v1beta1':'networking.k8s.io/v1';
        const resource=object.kind==='Gslb'?'gslbs':'ingresses';
        kubectl(['delete','--raw',`/apis/${group}/namespaces/demo/${resource}/${name}`,'-f','-'],{apiVersion:'v1',kind:'DeleteOptions',preconditions:{uid:object.metadata.uid},propagationPolicy:'Background'});
        report.cleanup.push({kind:object.kind,name,status:'deleted'});
      }catch(error){report.cleanup.push({kind:object.kind,name,error:error.message});process.exitCode=1;}
    }
    report.finishedAt=new Date().toISOString();
    fs.writeFileSync(path.join(dir,'missing-reference-report.json'),JSON.stringify(report,null,2),{mode:0o600});
    console.log(JSON.stringify(report,null,2));
  }
})();
