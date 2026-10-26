'use strict';
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
const {execFileSync,spawn}=require('node:child_process');
const args=Object.fromEntries(process.argv.slice(3).map(v=>{const i=v.indexOf('=');return [v.slice(0,i),v.slice(i+1)]}));
const action=process.argv[2], dir=args['--state-dir'];
if(!dir||!path.isAbsolute(dir)||!['init','install','fixtures','console','status','stop'].includes(action)) throw new Error('Usage: node lab.cjs init|install|fixtures|console|status|stop --state-dir=/absolute/path [--upstream=/path/to/k8gb-v1.0.0] [--traefik-crds=/path/to/dynamic-configuration] [--docker-context=desktop-linux]');
const root=path.resolve(__dirname,'../..'), statePath=path.join(dir,'state.json');
fs.mkdirSync(dir,{recursive:true,mode:0o700});
const write=(name,value)=>fs.writeFileSync(path.join(dir,name),typeof value==='string'?value:JSON.stringify(value,null,2),{mode:0o600});
let state=fs.existsSync(statePath)?JSON.parse(fs.readFileSync(statePath)):null;
const save=()=>write('state.json',state);
const command=(cmd,a,options={})=>execFileSync(cmd,a,{encoding:'utf8',maxBuffer:32<<20,timeout:360000,env:{...process.env,DOCKER_CONTEXT:state?.dockerContext||args['--docker-context'],KIND_EXPERIMENTAL_PROVIDER:'docker'},...options});
const docker=(a,o)=>command('docker',['--context',state?.dockerContext||args['--docker-context']||'desktop-linux',...a],o);
const kubectl=(id,a,o)=>command('kubectl',['--kubeconfig',path.join(dir,`${id}.kubeconfig`),'--context','kind-'+state.clusters.find(c=>c.id===id).name,...a],o);
const helmEnv=()=>({...process.env,HELM_CACHE_HOME:path.join(dir,'helm-cache'),HELM_CONFIG_HOME:path.join(dir,'helm-config'),HELM_DATA_HOME:path.join(dir,'helm-data')});
const apply=(id,items)=>kubectl(id,['apply','-f','-'],{input:JSON.stringify({apiVersion:'v1',kind:'List',items})});
const object=(apiVersion,kind,namespace,name,other={})=>({apiVersion,kind,metadata:{...(namespace?{namespace}:{}),name},...other});
const deploy=(namespace,name,container,spec={})=>object('apps/v1','Deployment',namespace,name,{spec:{replicas:1,selector:{matchLabels:{app:name}},template:{metadata:{labels:{app:name}},spec:{containers:[{name,...container}],...spec}}}});
const service=(namespace,name,ports,extra={})=>object('v1','Service',namespace,name,{spec:{selector:{app:name},ports,...extra}});
const binding=(name,role,ns,sa)=>object('rbac.authorization.k8s.io/v1','ClusterRoleBinding','',name,{roleRef:{apiGroup:'rbac.authorization.k8s.io',kind:'ClusterRole',name:role},subjects:[{kind:'ServiceAccount',namespace:ns,name:sa}]});
const images={operator:'registry.k8gb.io/k8gb-io/k8gb:v1.0.0',dns:'registry.k8gb.io/k8gb-io/k8s_crd:v0.6.0',traefik:'traefik:v3.6.10',backend:'nginx:1.28-alpine',agent:'k8gb-ui-lab-agent:dev',webhook:'k8gb-ui-lab-webhook:dev'};
function validateState(){
  if(!state||state.version!==1||!/^[a-f0-9]{8}$/.test(state.owner)||state.clusters?.length!==2)throw new Error('Not an owned lab state');
  for(const c of state.clusters)if(!['east','west'].includes(c.id)||c.name!==`k8gb-ui-lab-${state.owner}-${c.id}`)throw new Error('Invalid owned cluster');
}
function init(){
  if(state)throw new Error('State already exists; use install or console, never silently reset live edits');
  const upstream=args['--upstream'],crds=args['--traefik-crds'];
  if(!upstream||!crds||!fs.existsSync(path.join(upstream,'chart/k8gb/Chart.yaml')))throw new Error('Explicit public k8gb v1.0.0 source and Traefik CRD directory required');
  const version=fs.readFileSync(path.join(upstream,'chart/k8gb/Chart.yaml'),'utf8');if(!/^appVersion: v1\.0\.0$/m.test(version))throw new Error('Pin the upstream v1.0.0 chart');
  const owner=crypto.randomBytes(4).toString('hex');
  state={version:1,owner,upstream:path.resolve(upstream),traefikCRDs:path.resolve(crds),dockerContext:args['--docker-context']||'desktop-linux',consoleURL:'http://127.0.0.1:8791',images,clusters:['east','west'].map((id,i)=>({id,name:`k8gb-ui-lab-${owner}-${id}`,dnsPort:15053+i,httpPort:18091+i,agentPort:18791+i}))};save();
  for(const c of state.clusters){
    const ports=[[53,c.dnsPort,'TCP'],[53,c.dnsPort,'UDP'],[80,c.httpPort,'TCP'],[31081,c.agentPort,'TCP']];
    write(`${c.id}.kind.json`,{apiVersion:'kind.x-k8s.io/v1alpha4',kind:'Cluster',nodes:[{role:'control-plane',extraPortMappings:ports.map(([containerPort,hostPort,protocol])=>({containerPort,hostPort,protocol,listenAddress:'127.0.0.1'}))}]});
    console.log(`Creating isolated ${c.id}`);
    command('kind',['create','cluster','--name',c.name,'--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--config',path.join(dir,`${c.id}.kind.json`),'--image','kindest/node:v1.36.1','--wait','120s'],{stdio:'inherit'});
    c.ip=docker(['inspect','--format','{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}',`${c.name}-control-plane`]).trim();save();
  }
  const [east,west]=state.clusters;
  write('parent.zone',`$ORIGIN k8gb.test.\n$TTL 5\n@ IN SOA edge.k8gb.test. hostmaster.k8gb.test. (1 60 30 3600 5)\n@ IN NS edge.k8gb.test.\nedge IN A ${east.ip}\napps IN NS gslb-ns-east-apps.k8gb.test.\napps IN NS gslb-ns-west-apps.k8gb.test.\ngslb-ns-east-apps IN A ${east.ip}\ngslb-ns-west-apps IN A ${west.ip}\n`);
  write('Corefile',`k8gb.test:53 {\n  errors\n  file /config/parent.zone k8gb.test\n}\n`);
  for(const file of ['Corefile','parent.zone'])fs.chmodSync(path.join(dir,file),0o644);
  state.edgeContainer=`k8gb-ui-edge-${state.owner}`;save();
  docker(['run','-d','--name',state.edgeContainer,'--network','kind','--label',`k8gb-ui.lab-owner=${state.owner}`,'-p','127.0.0.1:15353:53/tcp','-p','127.0.0.1:15353:53/udp','-v',`${dir}/Corefile:/config/Corefile:ro`,'-v',`${dir}/parent.zone:/config/parent.zone:ro`,images.dns,'-conf','/config/Corefile'],{stdio:'inherit'});
  state.edgeIP=docker(['inspect','--format','{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}',state.edgeContainer]).trim();save();
  write('parent.zone',fs.readFileSync(path.join(dir,'parent.zone'),'utf8').replace(`edge IN A ${east.ip}`,`edge IN A ${state.edgeIP}`));
  fs.chmodSync(path.join(dir,'parent.zone'),0o644);
  docker(['restart',state.edgeContainer]);
  fs.mkdirSync(path.join(dir,'bin'),{recursive:true});
  const arch=docker(['info','--format','{{.Architecture}}']).trim()==='aarch64'?'arm64':'amd64';
  for(const component of ['agent','webhook']){
    command('go',['build','-trimpath','-o',path.join(dir,'bin',component),`./cmd/${component}`],{cwd:root,env:{...process.env,GOOS:'linux',GOARCH:arch,CGO_ENABLED:'0'}});
    docker(['build','--build-arg',`COMPONENT=${component}`,'-f',path.join(__dirname,'Dockerfile'),'-t',images[component],path.join(dir,'bin')],{stdio:'inherit'});
  }
  command('go',['build','-trimpath','-o',path.join(dir,'console'),'./cmd/console'],{cwd:root});
  for(const c of state.clusters){
    for(const name of ['read-token','write-token','plan-key'])write(`${c.id}.${name}`,crypto.randomBytes(32).toString('base64url'));
    command('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-days','30','-keyout',path.join(dir,`${c.id}.tls.key`),'-out',path.join(dir,`${c.id}.tls.crt`),'-subj','/CN=k8gb-ui-traefik.k8gb-ui-system.svc','-addext','subjectAltName=DNS:k8gb-ui-traefik.k8gb-ui-system.svc,DNS:k8gb-ui-traefik.k8gb-ui-system.svc.cluster.local','-addext','basicConstraints=critical,CA:TRUE'],{stdio:['ignore','ignore','ignore']});
  }
  console.log('Initialized. Run install with the same --state-dir.');
}
function install(){
  validateState();
  if(state.builtinRbac)throw new Error('Authenticated RBAC lab is installed; refusing baseline fixture/auth reset');
  for(const c of state.clusters){
    console.log(`Installing real controllers in ${c.id}`);
    command('kind',['load','docker-image',...Object.values(images),'--name',c.name],{stdio:'inherit'});
    write(`${c.id}.values.json`,{k8gb:{imageTag:'v1.0.0',clusterGeoTag:c.id,clusterExposedIPs:[c.ip],extGslbClustersGeoTags:state.clusters.filter(o=>o.id!==c.id).map(o=>o.id).join(','),dnsZones:[{loadBalancedZone:'apps.k8gb.test',parentZone:'k8gb.test',dnsZoneNegTTL:5}],edgeDNSServers:[state.edgeIP],reconcileRequeueSeconds:3,zoneDelegationReconcileRequeueSeconds:10,zoneDelegationRecoverySeconds:5,nsRecordTTL:5,resources:{limits:{cpu:'500m',memory:'256Mi'}}},coredns:{serviceType:'LoadBalancer',replicaCount:1},extdns:{enabled:false},gatewayapi:{enabled:false},istio:{enabled:false}});
    command('helm',['upgrade','--install','k8gb',path.join(state.upstream,'chart/k8gb'),'--namespace','k8gb','--create-namespace','--kubeconfig',path.join(dir,`${c.id}.kubeconfig`),'--kube-context','kind-'+c.name,'-f',path.join(dir,`${c.id}.values.json`)],{env:helmEnv(),stdio:'inherit'});
    const dnsDeploy=JSON.parse(kubectl(c.id,['-n','k8gb','get','deployment','k8gb-coredns','-o','json']));
    const ports=dnsDeploy.spec.template.spec.containers[0].ports;
    const patch=ports.flatMap((p,i)=>p.containerPort===5353?[{op:'add',path:`/spec/template/spec/containers/0/ports/${i}/hostPort`,value:53}]:[]);
    if(patch.length!==2)throw new Error('Unexpected CoreDNS chart ports');
    kubectl(c.id,['-n','k8gb','patch','deployment','k8gb-coredns','--type=json','-p',JSON.stringify(patch)]);
    kubectl(c.id,['-n','k8gb','patch','service','k8gb-coredns','--subresource=status','--type=merge','-p',JSON.stringify({status:{loadBalancer:{ingress:[{ip:c.ip}]}}})]);
    const crdFiles=fs.readdirSync(state.traefikCRDs).filter(n=>/^traefik\.io_.*\.yaml$/.test(n));
    for(const file of crdFiles)kubectl(c.id,['apply','-f',path.join(state.traefikCRDs,file)]);
    const ns='k8gb-ui-system';
    console.log(apply(c.id,[object('v1','Namespace','',ns),{...object('v1','Namespace','','demo'),metadata:{name:'demo',labels:{'k8gb-ui.io/traefik-enabled':'true'}}}]));
    // Install the published read/write boundaries; service account belongs only to this lab cluster.
    for(const file of ['agent-rbac.yaml','agent-write-rbac.yaml','agent-dns-zones-rbac.yaml','traefik-reconciler-rbac.yaml'])kubectl(c.id,['apply','-f',path.join(root,'deploy',file)]);
    for(const bundle of ['agent-native-site-rbac.yaml','agent-audit-configmap.yaml']){
      const overlay=path.join(dir,bundle.replace('.yaml',''));fs.mkdirSync(overlay,{recursive:true});
      fs.copyFileSync(path.join(root,'deploy',bundle),path.join(overlay,'resources.yaml'));
      fs.writeFileSync(path.join(overlay,'Kustomization'),JSON.stringify({apiVersion:'kustomize.config.k8s.io/v1beta1',kind:'Kustomization',namespace:'demo',resources:['resources.yaml']}));
      kubectl(c.id,['apply','-k',overlay]);
    }
    const read=name=>fs.readFileSync(path.join(dir,`${c.id}.${name}`),'utf8');
    const appItems=[
      object('v1','ServiceAccount',ns,'k8gb-ui-traefik'),object('v1','ServiceAccount',ns,'traefik'),
      object('networking.k8s.io/v1','IngressClass','','traefik',{spec:{controller:'traefik.io/ingress-controller'}}),
      object('rbac.authorization.k8s.io/v1','ClusterRole','','k8gb-ui-lab-traefik',{rules:[{apiGroups:[''],resources:['services','secrets','nodes','namespaces','configmaps'],verbs:['get','list','watch']},{apiGroups:['discovery.k8s.io'],resources:['endpointslices'],verbs:['get','list','watch']},{apiGroups:['networking.k8s.io'],resources:['ingresses','ingressclasses'],verbs:['get','list','watch']},{apiGroups:['networking.k8s.io'],resources:['ingresses/status'],verbs:['update']},{apiGroups:['traefik.io'],resources:crdFiles.map(n=>n.replace('traefik.io_','').replace('.yaml','')),verbs:['get','list','watch']}]}),
      binding('k8gb-ui-lab-traefik','k8gb-ui-lab-traefik',ns,'traefik'),
      deploy(ns,'traefik',{image:images.traefik,imagePullPolicy:'Never',args:['--providers.kubernetescrd','--providers.kubernetesingress','--providers.kubernetesingress.ingressendpoint.ip='+c.ip,'--entrypoints.websecure.address=:8080','--log.level=WARN','--api.dashboard=false'],ports:[{name:'http',containerPort:8080,hostPort:80}],resources:{requests:{cpu:'50m',memory:'64Mi'},limits:{cpu:'500m',memory:'256Mi'}},readinessProbe:{tcpSocket:{port:'http'}}},{serviceAccountName:'traefik'}),
      object('v1','ConfigMap','demo','hello',{data:{'index.html':`<h1>hello from ${c.id}</h1>\n`}}),
      deploy('demo','hello',{image:images.backend,imagePullPolicy:'Never',ports:[{name:'http',containerPort:80}],readinessProbe:{httpGet:{path:'/',port:'http'}},volumeMounts:[{name:'body',mountPath:'/usr/share/nginx/html',readOnly:true}],resources:{requests:{cpu:'20m',memory:'16Mi'},limits:{cpu:'200m',memory:'64Mi'}}},{volumes:[{name:'body',configMap:{name:'hello'}}]}),
      service('demo','hello',[{name:'http',port:80,targetPort:'http'}]),
      object('v1','Secret',ns,'agent-keys',{type:'Opaque',stringData:{'read-token':read('read-token'),'write-token':read('write-token'),'plan-key':read('plan-key')}}),
      object('v1','Secret',ns,'webhook-tls',{type:'kubernetes.io/tls',stringData:{'tls.crt':read('tls.crt'),'tls.key':read('tls.key')}}),
      deploy(ns,'k8gb-ui-traefik',{image:images.webhook,imagePullPolicy:'Never',args:['--listen=:8443','--tls-cert-file=/tls/tls.crt','--tls-private-key-file=/tls/tls.key','--reconcile','--reconcile-interval=2s','--allowed-namespaces=demo','--allowed-geo-tags=east,west','--domain=apps.k8gb.test','--local-domain=local.k8gb.test','--ingress-class=traefik'],ports:[{name:'https',containerPort:8443}],volumeMounts:[{name:'tls',mountPath:'/tls',readOnly:true}],readinessProbe:{httpGet:{path:'/readyz',port:'https',scheme:'HTTPS'}},resources:{requests:{cpu:'50m',memory:'32Mi'},limits:{cpu:'500m',memory:'192Mi'}}},{serviceAccountName:'k8gb-ui-traefik',volumes:[{name:'tls',secret:{secretName:'webhook-tls'}}]}),
      service(ns,'k8gb-ui-traefik',[{name:'https',port:443,targetPort:'https'}]),
      deploy(ns,'k8gb-ui-agent',{image:images.agent,imagePullPolicy:'Never',args:['--listen=:8081','--cluster-id='+c.id,'--allowed-namespaces=demo','--token-file=/run/keys/read-token','--enable-writes','--enable-native-site-traffic','--audit-configmap=k8gb-ui-audit','--write-token-file=/run/keys/write-token','--plan-key-file=/run/keys/plan-key','--geotags=east,west','--traefik-adapter','--traefik-domain=apps.k8gb.test','--traefik-local-domain=local.k8gb.test'],ports:[{name:'http',containerPort:8081}],volumeMounts:[{name:'keys',mountPath:'/run/keys',readOnly:true}],readinessProbe:{httpGet:{path:'/healthz',port:'http'}},resources:{requests:{cpu:'50m',memory:'32Mi'},limits:{cpu:'500m',memory:'192Mi'}}},{serviceAccountName:'k8gb-ui-agent',volumes:[{name:'keys',secret:{secretName:'agent-keys'}}]}),
      service(ns,'k8gb-ui-agent',[{name:'http',port:8081,targetPort:'http',nodePort:31081}],{type:'NodePort'})
    ];
    appItems.find(item=>item.kind==='Deployment'&&item.metadata.name==='k8gb-ui-agent').spec.replicas=2;
    console.log(apply(c.id,appItems));
    for(const [namespace,name] of [[ns,'k8gb-ui-traefik'],[ns,'k8gb-ui-agent'],[ns,'traefik'],['demo','hello'],['k8gb','k8gb-coredns'],['k8gb','k8gb']])console.log(kubectl(c.id,['-n',namespace,'rollout','status','deployment/'+name,'--timeout=180s']));
    const admission=object('admissionregistration.k8s.io/v1','MutatingWebhookConfiguration','','k8gb-ui-traefik',{webhooks:[{name:'ingressroutes.traefik.k8gb-ui.io',admissionReviewVersions:['v1'],sideEffects:'None',failurePolicy:'Fail',reinvocationPolicy:'IfNeeded',timeoutSeconds:10,namespaceSelector:{matchLabels:{'k8gb-ui.io/traefik-enabled':'true'}},clientConfig:{service:{namespace:ns,name:'k8gb-ui-traefik',path:'/mutate',port:443},caBundle:Buffer.from(read('tls.crt')).toString('base64')},rules:[{operations:['CREATE','UPDATE'],apiGroups:['traefik.io'],apiVersions:['v1alpha1'],resources:['ingressroutes'],scope:'Namespaced'}]}]});
    console.log(apply(c.id,[admission]));
    // Source creation happens only during installation, not on console restart.
    const route=object('traefik.io/v1alpha1','IngressRoute','demo','hello',{spec:{entryPoints:['websecure'],routes:[{match:'Host(`hello.local.k8gb.test`)',kind:'Rule',services:[{name:'hello',port:80}]}]}});
    route.metadata.annotations={gslb:'true',gslb_hostname:'hello.apps.k8gb.test',gslb_strategy:'roundRobin',gslb_dnsttl:'5',gslb_entrypoint:'websecure'};
    console.log(apply(c.id,[route]));
  }
  fixtures();
  state.installed=true;state.sharedAudit=true;state.agentReplicas=2;save();console.log('Installed. Run console with the same --state-dir.');
}
function fixtures(){
  validateState();if(state.stopped)throw new Error('Lab has been removed');
  const ingress=(name,host)=>object('networking.k8s.io/v1','Ingress','demo',name,{spec:{ingressClassName:'traefik',rules:[{host,http:{paths:[{path:'/',pathType:'Prefix',backend:{service:{name:'hello',port:{number:80}}}}]}}]}});
  for(const c of state.clusters){
    const native=ingress('native-web','native.apps.k8gb.test');
    native.metadata.annotations={'traefik.ingress.kubernetes.io/router.entrypoints':'websecure'};
    const unused=ingress('native-new','native-new.apps.k8gb.test');
    unused.metadata.annotations={'traefik.ingress.kubernetes.io/router.entrypoints':'websecure'};
    const gslb=object('k8gb.io/v1beta1','Gslb','demo','native-web',{spec:{resourceRef:{apiVersion:'networking.k8s.io/v1',kind:'Ingress',name:'native-web'},strategy:{type:'roundRobin',dnsTtlSeconds:5}}});
    const route=object('traefik.io/v1alpha1','IngressRoute','demo','hello-new',{spec:{entryPoints:['websecure'],routes:[{match:'Host(`hello-new.local.k8gb.test`)',kind:'Rule',services:[{name:'hello',port:80}]}]}});
    route.metadata.annotations={gslb:'false',gslb_hostname:'hello-new.apps.k8gb.test',gslb_entrypoint:'websecure'};
    const unusedNext=structuredClone(unused);unusedNext.metadata.name='native-next';unusedNext.spec.rules[0].host='native-next.apps.k8gb.test';
    const routeNext=structuredClone(route);routeNext.metadata.name='hello-next';routeNext.metadata.annotations.gslb_hostname='hello-next.apps.k8gb.test';routeNext.spec.routes[0].match='Host(`hello-next.local.k8gb.test`)';
    for(const resource of [native,unused,gslb,route,unusedNext,routeNext]){
      resource.metadata.labels={'k8gb-ui.io/lab-owner':state.owner};
      const type={Ingress:'ingresses.networking.k8s.io',Gslb:'gslbs.k8gb.io',IngressRoute:'ingressroutes.traefik.io'}[resource.kind];
      const existing=kubectl(c.id,['-n','demo','get',type,resource.metadata.name,'--ignore-not-found','-o','json']).trim();
      if(existing){
        if(JSON.parse(existing).metadata.labels?.['k8gb-ui.io/lab-owner']!==state.owner)throw new Error('Unowned lab fixture collision: '+c.id+'/'+resource.kind+'/'+resource.metadata.name);
        console.log(`Preserving ${c.id} ${resource.kind}/${resource.metadata.name}`);continue;
      }
      console.log(kubectl(c.id,['create','-f','-'],{input:JSON.stringify(resource)}));
    }
  }
  state.nativeFixtures=true;save();
}
function consoleStart(){
  validateState();if(!state.installed||state.stopped)throw new Error('Install a running lab first');
  if(state.builtinRbac)throw new Error('Use builtin-rbac.cjs start; refusing to replace authenticated access with anonymous localAdmin');
  if(state.consolePID){let cmd='';try{cmd=command('ps',['-p',String(state.consolePID),'-o','command=']).trim()}catch{}if(cmd.startsWith(path.join(dir,'console')+' '))throw new Error('Lab Console is already running; use status');}
  write('console.json',{listen:'127.0.0.1:8791',writesEnabled:true,auth:{mode:'local',localAdmin:true},clusters:state.clusters.map(c=>({id:c.id,name:c.id.toUpperCase()+' local kind',url:`http://127.0.0.1:${c.agentPort}`,namespaces:['demo'],geoTags:[c.id],zones:['apps.k8gb.test'],bearerTokenFile:path.join(dir,`${c.id}.read-token`),writeTokenFile:path.join(dir,`${c.id}.write-token`)}))});
  const fd=fs.openSync(path.join(dir,'console.log'),'a');
  const child=spawn(path.join(dir,'console'),['--config',path.join(dir,'console.json')],{detached:true,stdio:['ignore',fd,fd]});child.unref();fs.closeSync(fd);state.consolePID=child.pid;save();status();
}
function status(){validateState();console.log(JSON.stringify({url:state.consoleURL,authMode:state.builtinRbac?JSON.parse(fs.readFileSync(path.join(dir,'console.json'))).auth.mode:'local',stateDir:dir,consolePID:state.consolePID,edgeDNSPort:15353,clusters:state.clusters},null,2));}
function stop(){validateState();if(state.consolePID){let cmd='';try{cmd=command('ps',['-p',String(state.consolePID),'-o','command=']).trim()}catch{}if(cmd.startsWith(path.join(dir,'console')+' '))process.kill(state.consolePID,'SIGTERM')}
  if(state.builtinRbac?.proxyPID){let cmd='';try{cmd=command('ps',['-p',String(state.builtinRbac.proxyPID),'-o','command=']).trim()}catch{}const expected=state.builtinRbac.proxyExecutable+' '+state.builtinRbac.proxyScript+' --state-dir='+dir;if(cmd===expected)process.kill(state.builtinRbac.proxyPID,'SIGTERM')}
  if(state.keycloak){
    let cmd='';try{cmd=command('ps',['-p',String(state.keycloak.proxyPID),'-o','command=']).trim()}catch{}
    if(cmd===state.keycloak.proxyExecutable+' '+state.keycloak.proxyScript+' --state-dir='+dir)process.kill(state.keycloak.proxyPID,'SIGTERM');
    const owner=docker(['inspect','--format','{{index .Config.Labels "k8gb-ui.io/lab-owner"}}',state.keycloak.container]).trim();
    if(owner!==state.owner)throw new Error('Keycloak owner mismatch');
    docker(['stop',state.keycloak.container]); // Keep the owned dev database volume and private credentials.
  }
  for(const c of state.clusters)command('kind',['delete','cluster','--name',c.name],{stdio:'inherit'});
  if(state.edgeContainer){const owner=docker(['inspect','--format','{{index .Config.Labels "k8gb-ui.lab-owner"}}',state.edgeContainer]).trim();if(owner!==state.owner)throw new Error('Edge owner mismatch');docker(['rm','-f',state.edgeContainer],{stdio:'inherit'})}state.stopped=true;save();
}
try{({init,install,fixtures,console:consoleStart,status,stop}[action])()}catch(error){console.error(error.message);process.exitCode=1}
