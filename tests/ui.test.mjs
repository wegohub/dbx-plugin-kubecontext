import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { Window } from 'happy-dom';

const html = await readFile(new URL('../ui/index.html', import.meta.url), 'utf8');
const controls = await readFile(new URL('../ui/controls.js', import.meta.url), 'utf8');
const monitorCode = await readFile(new URL('../ui/monitor.js', import.meta.url), 'utf8');
const code = html.split('<script type="module">')[1].split('</script>')[0];
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
async function setup(overrides = {}, context = { connectionId: 'c1', contextName: 'alpha' }) {
  const window = new Window({ url: 'https://plugin.test', settings: { disableJavaScriptEvaluation: true } });
  window.document.write(html.replace(/<script\b[\s\S]*?<\/script>/g, ''));
  const calls = [];
  let onEvent, onClose;
  window.Terminal = class {
    constructor() { this.output = ""; this.cols = 100; this.rows = 25; }
    loadAddon() {} open() {} onData(callback) { this.input = callback; }
    write(data) { this.output += data; } writeln(data) { this.output += data + "\r\n"; }
    focus() {} clear() { this.output = ""; } reset() { this.output = ""; } dispose() {}
  };
  window.FitAddon = { FitAddon: class { fit() {} } };
  window.ResizeObserver = class { observe() {} disconnect() {} };
  window.dbxPlugin = {
    ready: Promise.resolve(), context,
    workbench: { onClose: (callback) => { onClose = callback; } },
    fileTransfer: overrides.fileTransfer, capabilities: overrides.capabilities, storage: overrides.storage,
    onEvent: (listener) => { onEvent = listener; },
    invoke: async (method, params) => {
      calls.push({ method, params });
      if (overrides[method]) return overrides[method](params, (method, params) => onEvent({method, params}));
      if (method === 'kube/contexts') return { current: 'alpha', contexts: [{name:'alpha',current:true}, {name:'beta'}, {name:'gamma'}] };
      if (method === 'kube/use-context') return { current: params.context };
      if (method === 'kube/overview') return { context: params.context, version: {}, namespaces: [] };
      if (method === 'kube/permissions') return {};
      if (method === 'kube/list') return { rows: [{ name: '中文-pod', namespace: 'default' }] };
      if (method === 'kube/get') return { json: '{"kind":"Pod","spec":{"containers":[{"name":"api"}]}}' };
      return {};
    },
  };
  window.eval(controls);window.eval(monitorCode);
  const exports = window.eval(`(function(){ ${code}\n return {state, startLogs, stopLogs, selectRow, switchTab, loadTab, loadList, ask, enterContext, loadContexts, importFile, pickImportFile, startTerminal, terminals, activeTerminal, closeTerminal, enterPod, monitor, renderRows, renderActions}; })()`);
  await tick(); await tick();
  return { window, calls, ...exports, emit: (method, params) => onEvent({method, params}), closeWorkbench: () => onClose(), close: () => window.happyDOM.abort() };
}

test('detail passes resource kind, name, namespace and connection', async () => {
  const app = await setup();
  await app.selectRow(app.state.rows[0]);
  const request = app.calls.find((call) => call.method === 'kube/get');
  assert.deepEqual(JSON.parse(JSON.stringify(request.params)), { connectionId: 'c1', context: 'alpha', name: '中文-pod', namespace: 'default', kind: 'pods' });
  assert.match(app.window.document.getElementById('pane').textContent, /Pod/);
  await app.close();
});

test('events arriving before start response are rendered, UTF-8 survives and retention defaults to 100', async () => {
  const app = await setup({ 'kube/logs/start': async (params, emit) => {
    emit('kube/logChunk', { streamId: params.streamId, lines: Array.from({length: 130}, (_, i) => `中文日志 ${i}`) });
    emit('kube/logEnd', { streamId: params.streamId });
    return { streamId: params.streamId };
  }});
  await app.selectRow(app.state.rows[0]); app.switchTab('logs'); await tick();
  assert.equal(app.state.logLines.length, 100);
  assert.equal(app.state.logLines[0], '中文日志 30');
  assert.match(app.window.document.getElementById('pane').textContent, /中文日志 129/);
  assert.equal(app.window.document.getElementById('log-state').textContent, '已结束');
  const input = app.window.document.getElementById('max-lines'); input.value = '3'; input.dispatchEvent(new app.window.Event('change'));
  assert.equal(app.state.logLines.length, 3);
  await app.close();
});

test('a late start is stopped after leaving logs and cannot replace detail', async () => {
  let respond;
  const app = await setup({ 'kube/logs/start': (params) => new Promise((resolve) => { respond = () => resolve({streamId: params.streamId}); }) });
  await app.selectRow(app.state.rows[0]); app.switchTab('logs'); await tick();
  const streamId = app.state.logStream;
  app.switchTab('detail'); await tick(); respond(); await tick();
  app.emit('kube/logChunk', {streamId, lines:['stale log']});
  assert.match(app.window.document.getElementById('pane').textContent, /Pod/);
  assert.equal(app.state.logStream, null);
  assert.ok(app.calls.filter((c) => c.method === 'kube/logs/stop').length >= 2);
  await app.close();
});

test('late detail responses cannot overwrite the active logs view', async () => {
  let respond;
  const app = await setup({ 'kube/get': () => new Promise((resolve) => { respond = resolve; }) });
  const selecting = app.selectRow(app.state.rows[0]); await tick();
  app.switchTab('logs'); await tick();
  app.emit('kube/logChunk', {streamId: app.state.logStream, lines:['live']});
  respond({json:'{"stale":true}'}); await selecting;
  assert.equal(app.window.document.getElementById('pane').textContent, 'live');
  await app.close();
});

test('import modal closes by button, backdrop and Escape; confirmation also closes', async () => {
  const app = await setup(); const doc=app.window.document;
  const open=() => doc.getElementById('ov-import').click();
  const closed=() => doc.getElementById('dropzone').classList.contains('hidden');
  open(); assert.equal(closed(),false); doc.getElementById('dz-close').click(); assert.equal(closed(),true);
  open(); doc.getElementById('dropzone').click(); assert.equal(closed(),true);
  open(); doc.dispatchEvent(new app.window.KeyboardEvent('keydown',{key:'Escape'})); assert.equal(closed(),true);
  const answer=app.ask('确定？'); doc.getElementById('confirm-dialog').click(); assert.equal(await answer,null);
  assert.ok(doc.getElementById('confirm-dialog').classList.contains('hidden'));
  await app.close();
});

test('log events from another tab are ignored and closing detail hides it', async () => {
  const app = await setup(); await app.selectRow(app.state.rows[0]); app.switchTab('logs'); await tick();
  app.emit('kube/logChunk',{streamId:'foreign',lines:['other cluster']}); assert.equal(app.state.logLines.length,0);
  app.window.document.getElementById('close-detail').click();
  assert.ok(app.window.document.getElementById('detail').classList.contains('hidden'));
  await app.close();
});


test('rapid namespace requests render only the newest result', async () => {
  let oldResponse;
  const app = await setup();
  app.window.dbxPlugin.invoke = async (method, params) => {
    if (method !== 'kube/list') return {};
    if (params.namespace === 'old') return new Promise((resolve) => { oldResponse = resolve; });
    return {rows: [{name: 'new-pod', namespace:'new'}]};
  };
  app.state.namespace = 'old'; const old = app.loadList();
  app.state.namespace = 'new'; await app.loadList();
  oldResponse({rows:[{name:'stale-pod',namespace:'old'}]}); await old;
  assert.equal(app.state.rows[0].name, 'new-pod');
  assert.doesNotMatch(app.window.document.getElementById('rows').textContent, /stale/);
  await app.close();
});


test('imports in another connection draft refresh the dropdown while viewing resources', async () => {
  const app = await setup({'kube/contexts': async () => ({contexts:[{name:'new-context',server:'https://test'}]})});
  app.state.mode='context';
  app.emit('kube/contextsChanged',{connectionId:'c1'}); await tick();
  assert.equal(app.state.contexts[0].name,'new-context');
  assert.match(app.window.document.getElementById('context-select').textContent,/new-context/);
  await app.close();
});

test('the context selector precedes namespace and changes context in the same workbench', async () => {
  const app = await setup(); const doc = app.window.document;
  const select = doc.getElementById('context-select');
  assert.ok(select.compareDocumentPosition(doc.getElementById('namespace')) & app.window.Node.DOCUMENT_POSITION_FOLLOWING);
  assert.equal(select.value, 'alpha');
  select.value = 'beta'; select.dispatchEvent(new app.window.Event('change'));
  await tick(); await tick();
  assert.equal(app.state.contextName, 'beta');
  assert.match(select.selectedOptions[0].textContent, /kubectl 默认/);
  assert.equal(app.calls.filter(c => c.method === 'kube/overview').at(-1).params.context, 'beta');
  assert.equal(app.window.dbxPlugin.openWorkbench, undefined);
  await app.close();
});

test('rapid context selections serialize activation and discard stale resource responses and logs', async () => {
  let oldOverview, firstActivation;
  const app = await setup();
  await app.selectRow(app.state.rows[0]); app.switchTab('logs'); await tick();
  const oldStream = app.state.logStream;
  app.window.dbxPlugin.invoke = async (method, params) => {
    app.calls.push({method,params});
    if (method === 'kube/use-context' && params.context === 'beta') return new Promise(resolve => { firstActivation = resolve; });
    if (method === 'kube/overview' && params.context === 'beta') return new Promise(resolve => { oldOverview = resolve; });
    if (method === 'kube/overview') return {context:params.context,namespaces:['latest']};
    if (method === 'kube/list') return {rows:[{name:params.context+'-pod'}]};
    return {};
  };
  const beta = app.enterContext('beta'); await tick();
  const gamma = app.enterContext('gamma'); await tick();
  assert.equal(app.calls.filter(c => c.method === 'kube/use-context').at(-1).params.context, 'beta');
  firstActivation({current:'beta'}); await Promise.all([beta,gamma]);
  assert.equal(app.state.contextName,'gamma');
  assert.equal(app.state.rows[0].name,'gamma-pod');
  assert.equal(oldOverview,undefined);
  app.emit('kube/logChunk',{streamId:oldStream,lines:['old-cluster']});
  assert.equal(app.state.logLines.length,0);
  assert.equal(docValue(app,'context-select'),'gamma');
  await app.close();
});

const docValue = (app,id) => app.window.document.getElementById(id).value;

test('a stale overview cannot overwrite the namespace and permissions of a later context', async () => {
  let oldOverview;
  const app = await setup({
    'kube/overview': async params => params.context === 'beta' ? new Promise(resolve => {oldOverview=resolve;}) : {context:params.context,namespaces:[params.context]},
    'kube/list': async params => ({rows:[{name:params.context+'-pod'}]})
  });
  const old = app.enterContext('beta'); await tick();
  await app.enterContext('gamma');
  oldOverview({context:'beta',namespaces:['stale-namespace']}); await old;
  assert.equal(app.state.contextName,'gamma');
  assert.equal(app.state.rows[0].name,'gamma-pod');
  assert.doesNotMatch(app.window.document.getElementById('namespace').textContent,/stale/);
  await app.close();
});

test('failed activation clears the resource controls instead of showing data from the previous cluster', async () => {
  const app = await setup({'kube/use-context': async params => {if(params.context==='beta') throw new Error('read-only kubeconfig'); return {};}});
  await app.enterContext('beta');
  assert.equal(app.state.contextName,'');
  assert.equal(app.state.rows.length,0);
  assert.ok(app.window.document.getElementById('context-controls').classList.contains('hidden'));
  assert.match(app.window.document.getElementById('banner').textContent,/read-only/);
  await app.close();
});

test('merge refreshes the dropdown without changing the selected context', async () => {
  const app = await setup({'kube/merge': async params => {
    assert.equal(params.kubeconfig,'test-config');
    assert.equal(params.sourcePath,'/tmp/new.yaml');
    return {contexts:[{name:'alpha'},{name:'new-context'}], current:'alpha',path:'/tmp/aggregate',added:['context/new-context']};
  }});
  await app.importFile({name:'kube-config.yaml',size:11,sourcePath:'/tmp/new.yaml',text:async () => 'test-config'});
  assert.equal(docValue(app,'context-select'),'alpha');
  assert.match(app.window.document.getElementById('context-select').textContent,/new-context/);
  assert.equal(app.state.contextName,'alpha');
  assert.equal(app.calls.find(c=>c.method==='kube/merge').params.connectionId,'c1');
  assert.ok(app.window.document.getElementById('dropzone').classList.contains('hidden'));
  await app.close();
});

test('each context retains its own terminal, including early output, and all are stopped on close', async () => {
  const app = await setup({'terminal/start': async (params,emit) => {emit('terminal/output',{streamId:params.streamId,data:params.context+' early output'}); return {};}});
  await app.startTerminal(); const alpha = [...app.terminals.values()].find(e=>e.context==='alpha');
  assert.match(alpha.terminal.output,/alpha early output/);
  await app.enterContext('beta'); await app.startTerminal(); const beta = [...app.terminals.values()].find(e=>e.context==='beta');
  assert.notEqual(alpha.streamId,beta.streamId);
  app.emit('terminal/output',{streamId:alpha.streamId,data:' background-alpha'});
  assert.doesNotMatch(beta.terminal.output,/background-alpha/);
  await app.enterContext('alpha'); await app.startTerminal();
  assert.equal(app.calls.filter(c=>c.method==='terminal/start').length,2);
  assert.match([...app.terminals.values()].find(e=>e.context==='alpha').terminal.output,/background-alpha/);
  alpha.terminal.input('kubectl get pod\r'); await tick();
  const input = app.calls.find(c=>c.method==='terminal/input');
  assert.equal(input.params.context,'alpha'); assert.equal(input.params.streamId,alpha.streamId);
  await app.closeWorkbench();
  assert.equal(app.calls.filter(c=>c.method==='terminal/stop').length,2);
  assert.equal(app.terminals.size,0);
  await app.close();
});

test('native file selection streams UTF-8 content into merge and closes the read handle', async () => {
  const closed=[];
  const config='配置: 中文';
  const app=await setup({
    capabilities:{fileTransfer:{pick:true,read:true}},
    fileTransfer:{
      pick:async options => {assert.equal(options.multiple,false);return {files:[{handleId:'picked',name:'kube-config.yaml',size:64}]};},
      read:async (handle,offset) => {assert.equal(handle,'picked');assert.equal(offset,0);return {dataBase64:Buffer.from(config).toString('base64'),eof:true};},
      cancel:async handle => closed.push(handle),
    },
    'kube/merge':async params => {assert.equal(params.kubeconfig,config);return {contexts:[{name:'alpha'},{name:'from-picker'}],current:'alpha',path:'/tmp/aggregate'};},
  });
  await app.pickImportFile();
  assert.deepEqual(closed,['picked']);
  assert.match(app.window.document.getElementById('context-select').textContent,/from-picker/);
  await app.close();
});

test('native OS file drops merge into the same connection and refresh context options', async () => {
  let onDrop;
  const closed=[];
  const app=await setup({
    fileTransfer:{
      onDrop:listener => {onDrop=listener;},
      read:async () => ({dataBase64:Buffer.from('dropped-config').toString('base64'),eof:true}),
      cancel:async handle => closed.push(handle),
    },
    'kube/merge':async params => {assert.equal(params.connectionId,'c1');assert.equal(params.kubeconfig,'dropped-config');return {contexts:[{name:'alpha'},{name:'from-drop'}],current:'alpha',path:'/tmp/aggregate'};},
  });
  onDrop([{handleId:'dropped',name:'kube-config.yaml',size:14}]);await tick();await tick();
  assert.match(app.window.document.getElementById('context-select').textContent,/from-drop/);
  assert.deepEqual(closed,['dropped']);
  assert.equal(app.state.contextName,'alpha');
  await app.close();
});

test('each context selects its concrete namespace and sends it to resource and permission requests', async () => {
  const namespaces = {'hkg-prod-backendhub':'backendhub','hkg-prod-scanx':'scanx','hkg-test':'backendhub','ht-wego':'wego','without-namespace':'default'};
  const app = await setup({
    'kube/contexts':async () => ({contexts:Object.entries(namespaces).map(([name,namespace])=>({name,namespace:name==='without-namespace'?undefined:namespace}))}),
    // Simulate a restricted user who cannot enumerate namespaces.
    'kube/overview':async params => ({context:params.context,namespace:namespaces[params.context],namespaces:[]}),
  }, {connectionId:'c1'});
  const select=app.window.document.getElementById('namespace');
  for (const [context,namespace] of Object.entries(namespaces)) {
    await app.enterContext(context);
    assert.equal(select.value,namespace);
    assert.equal(select.selectedOptions[0].textContent,namespace);
    assert.equal(app.state.namespace,namespace);
    assert.equal(app.calls.filter(c=>c.method==='kube/list').at(-1).params.namespace,namespace);
    assert.equal(app.calls.filter(c=>c.method==='kube/permissions').at(-1).params.namespace,namespace);
    // A manual override on one context must not carry into the next context.
    const option=app.window.document.createElement('option');option.value='other';option.textContent='other';select.append(option);
    select.value='other';select.dispatchEvent(new app.window.Event('change'));await tick();
  }
  await app.close();
});

test('the context namespace is retained even if namespace enumeration omits it and duplicates others', async () => {
  const app=await setup({
    'kube/overview':async params=>({context:params.context,namespace:'backendhub',namespaces:['default','scanx','scanx']}),
  });
  const select=app.window.document.getElementById('namespace');
  assert.equal(select.value,'backendhub');
  assert.deepEqual([...select.options].map(option=>option.value),['backendhub','default','scanx','*']);
  await app.close();
});

test('resource detail actions never expose delete, scale or restart for any kind', async () => {
  const app=await setup();
  for(const kind of ['pods','deployments','statefulsets','daemonsets','services','configmaps','secrets','nodes','namespaces']) {
    app.state.kind=kind;app.state.selected={name:'example',namespace:'default',kind};app.state.readOnly=false;
    app.renderActions();
    assert.doesNotMatch(app.window.document.getElementById('actions').textContent,/删除|重启|扩缩容/);
  }
  await app.close();
});

test('custom context menu supports keyboard selection, search and Escape without native popup', async () => {
  const app=await setup({'kube/contexts':async()=>({contexts:[{name:'alpha'},...Array.from({length:10},(_,i)=>({name:`dev-${i}`}))]})});
  const doc=app.window.document, trigger=doc.getElementById('context-select-trigger');
  assert.ok(doc.getElementById('context-select').hidden);
  trigger.click();assert.equal(trigger.getAttribute('aria-expanded'),'true');
  const search=doc.querySelector('.select-popover:not(.hidden) input');
  search.value='dev-8';search.dispatchEvent(new app.window.Event('input'));
  assert.equal(doc.querySelectorAll('#context-select-options [role="option"]').length,1);
  search.dispatchEvent(new app.window.KeyboardEvent('keydown',{key:'Enter',bubbles:true}));await tick();await tick();
  assert.equal(app.state.contextName,'dev-8');assert.equal(trigger.getAttribute('aria-expanded'),'false');
  trigger.click();search.dispatchEvent(new app.window.KeyboardEvent('keydown',{key:'Escape',bubbles:true}));
  assert.equal(trigger.getAttribute('aria-expanded'),'false');assert.equal(doc.activeElement,trigger);
  await app.close();
});

test('detail drawer drag clamps width, supports keyboard and saves the chosen width', async () => {
  const saved=[];
  const app=await setup({capabilities:{storage:true},storage:{get:async key=>key==='detail-width'?640:100,set:async(key,value)=>saved.push({key,value})}});
  const doc=app.window.document, detail=doc.getElementById('detail'), handle=doc.getElementById('detail-resizer');
  doc.getElementById('context-view').getBoundingClientRect=()=>({width:1000});
  await app.selectRow(app.state.rows[0]);assert.equal(detail.style.width,'640px');
  handle.dispatchEvent(new app.window.PointerEvent('pointerdown',{button:0,clientX:500,pointerId:1,bubbles:true}));
  app.window.dispatchEvent(new app.window.PointerEvent('pointermove',{clientX:100,pointerId:1}));
  assert.equal(detail.style.width,'780px');
  app.window.dispatchEvent(new app.window.PointerEvent('pointerup',{pointerId:1}));
  assert.ok(saved.some(item=>item.key==='detail-width'&&item.value===780));
  handle.dispatchEvent(new app.window.KeyboardEvent('keydown',{key:'Home',bubbles:true}));assert.equal(detail.style.width,'320px');
  handle.dispatchEvent(new app.window.KeyboardEvent('keydown',{key:'ArrowLeft',shiftKey:true,bubbles:true}));assert.equal(detail.style.width,'368px');
  handle.dispatchEvent(new app.window.MouseEvent('dblclick'));assert.equal(detail.style.width,'520px');
  assert.equal(doc.body.classList.contains('resizing'),false);
  await app.close();
});

test('Pod list renders rich columns and can filter by IP, owner, node and image', async () => {
  const app=await setup();const doc=app.window.document;
  app.state.rows=[{name:'api-1',namespace:'default',status:'CrashLoopBackOff',ready:'0/1',fields:{restarts:'7',node:'node-a',podIP:'10.0.0.8',owner:'ReplicaSet/api-123',images:'api:v4',reason:'back-off restarting'},age:'2d'}];
  app.renderRows();
  assert.match(doc.getElementById('resource-columns').textContent,/重启.*节点.*Pod IP.*所属对象.*镜像/);
  assert.match(doc.getElementById('rows').textContent,/7.*node-a.*10.0.0.8.*ReplicaSet\/api-123.*api:v4/);
  assert.equal(doc.querySelector('#rows .pill').title,'back-off restarting');
  for (const value of ['10.0.0.8','api-123','node-a','api:v4']) {doc.getElementById('search').value=value;app.renderRows();assert.equal(doc.querySelectorAll('#rows tr').length,1);}
  app.state.kind='services';app.state.rows=[{name:'web',fields:{ports:'80/TCP',clusterIP:'10.96.0.8'}}];doc.getElementById('search').value='';app.renderRows();
  assert.match(doc.getElementById('resource-columns').textContent,/Cluster IP.*外部地址.*端口/);
  await app.close();
});

test('full text tooltip renders safe text and closes on Escape and scrolling', async()=>{
 const app=await setup(),doc=app.window.document;app.state.rows=[{name:'a-very-long-pod',namespace:'default',fields:{images:'registry/'+('<img onerror=alert(1)>').repeat(20)}}];app.renderRows();
 const target=[...doc.querySelectorAll('#rows td')].find(td=>td.dataset.tooltip?.includes('registry/'));
 target.dispatchEvent(new app.window.Event('pointerover',{bubbles:true}));const tip=doc.getElementById('kc-tooltip');assert.equal(tip.textContent,target.dataset.tooltip);assert.equal(tip.querySelector('img'),null);assert.ok(!tip.classList.contains('hidden'));
 doc.dispatchEvent(new app.window.KeyboardEvent('keydown',{key:'Escape'}));assert.ok(tip.classList.contains('hidden'));await app.close();
});
test('pod usage joins by namespace, preserves zero and distinguishes requests and limits',async()=>{
 const app=await setup({'kube/pod-usage':async()=>({rows:[{name:'中文-pod',namespace:'default',cpuMillicores:0,memoryBytes:1048576,timestamp:'now'},{name:'中文-pod',namespace:'foreign',cpuMillicores:999}]})});await tick();
 const text=app.window.document.getElementById('rows').textContent;assert.match(text,/0 m/);assert.match(text,/1 MiB/);assert.doesNotMatch(text,/999/);
 const heads=app.window.document.getElementById('resource-columns').textContent;for(const value of ['CPU 用量','CPU Request','CPU Limit','内存用量','内存 Request','内存 Limit','操作'])assert.ok(heads.includes(value));await app.close();
});
test('monitor shows partial data as unavailable, computes rates and discards stale responses',async()=>{
 let calls=0,late;const data={podUid:'one',cpuMillicores:12,memoryBytes:1048576,metricsTimestamp:'2026-10-19T12:00:00Z',sources:{cpuMemory:'metrics-server'},budget:{containers:[]},warnings:[{scope:'磁盘 / 网络',message:'403 nodes/proxy forbidden'}]};
 const app=await setup({'kube/pod-metrics':async()=>{calls++;if(calls===2)return new Promise(resolve=>late=resolve);return data;}});
 await app.selectRow(app.state.rows[0]);app.switchTab('monitor');await tick();const doc=app.window.document;
 assert.match(doc.querySelector('[data-metric="cpu"] .metric-value').textContent,/12 m/);assert.equal(doc.querySelector('[data-metric="disk"] .metric-value').textContent,'—');assert.match(doc.getElementById('monitor').textContent,/403/);assert.equal(doc.querySelectorAll('#monitor svg').length,4);
 const pending=app.monitor.refresh();await tick();app.switchTab('detail');late({...data,cpuMillicores:999});await pending;assert.ok(doc.getElementById('monitor').classList.contains('hidden'));assert.doesNotMatch(doc.getElementById('monitor').textContent,/999/);
 const rate=app.window.KubeMonitor.networkRate({timestamp:'2026-10-19T12:00:00Z',rxBytes:100,txBytes:200},{timestamp:'2026-10-19T12:00:05Z',rxBytes:150,txBytes:300});assert.equal(rate.rx,10);assert.equal(rate.tx,20);
 assert.equal(app.window.KubeMonitor.networkRate({timestamp:'2026-10-19T12:00:00Z',rxBytes:100,txBytes:200},{timestamp:'2026-10-19T12:00:05Z',rxBytes:1,txBytes:3}),null);
 await app.closeWorkbench();await app.close();
});
test('enter container selects running containers, creates independent tabs and closes just one',async()=>{
 const app=await setup({'kube/get':async()=>({json:JSON.stringify({status:{containerStatuses:[{name:'app',state:{running:{}}},{name:'proxy',state:{running:{}}},{name:'stopped',state:{terminated:{}}}]}})})}),doc=app.window.document;
 await app.startTerminal();const local=app.activeTerminal();await app.enterPod(app.state.rows[0]);assert.equal(doc.getElementById('exec-container').options.length,2);doc.getElementById('exec-container').value='proxy';doc.getElementById('exec-form').dispatchEvent(new app.window.Event('submit',{bubbles:true,cancelable:true}));await tick();
 const remote=app.activeTerminal();assert.notEqual(remote.id,local.id);assert.equal(app.terminals.size,2);const start=app.calls.filter(c=>c.method==='terminal/start').at(-1);assert.equal(start.params.pod,'中文-pod');assert.equal(start.params.container,'proxy');assert.equal(start.params.shell,'/bin/sh');
 app.emit('terminal/output',{streamId:local.streamId,data:'local-only'});assert.doesNotMatch(remote.terminal.output,/local-only/);await app.closeTerminal(remote);assert.equal(app.terminals.size,1);assert.equal(app.activeTerminal(),local);assert.ok(local.streamId);await app.closeWorkbench();await app.close();
});
