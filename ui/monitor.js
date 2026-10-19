(() => {
  const finite = value => typeof value === 'number' && Number.isFinite(value);
  const number = value => finite(value) ? new Intl.NumberFormat('zh-CN',{maximumFractionDigits:2}).format(value) : '—';
  const cpu = value => finite(value) ? number(value) + ' m' : '—';
  function bytes(value) {
    if (!finite(value)) return '—';
    const units=['B','KiB','MiB','GiB','TiB']; let index=0;
    while (value >= 1024 && index<units.length-1) { value/=1024; index++; }
    return number(value)+' '+units[index];
  }
  function networkRate(previous, current) {
    if (!previous || !current) return null;
    const seconds=(Date.parse(current.timestamp)-Date.parse(previous.timestamp))/1000;
    if (!(seconds>0) || !['rxBytes','txBytes'].every(key=>finite(current[key])&&finite(previous[key])&&current[key]>=previous[key])) return null;
    return {rx:(current.rxBytes-previous.rxBytes)/seconds,tx:(current.txBytes-previous.txBytes)/seconds};
  }
  function create({root, invoke}) {
    let revision=0,timer=null,selected=null,paused=false,busy=false,history={},previousNetwork=null,lastRate=null,uid=null,lastData=null;
    const el=(tag,cls,text)=>{const node=document.createElement(tag);if(cls)node.className=cls;if(text!==undefined)node.textContent=text;return node;};
    const toolbar=el('div','monitor-toolbar'), label=el('span','muted','每'), interval=el('select'); interval.id='monitor-interval';interval.setAttribute('aria-label','监控刷新间隔');
    [5,10,30].forEach(value=>{const option=el('option','',value+' 秒');option.value=value;interval.append(option);});
    const pause=el('button','btn small','暂停'), refresh=el('button','btn small','刷新'), status=el('div','monitor-status muted','');
    toolbar.append(label,interval,pause,refresh);
    const grid=el('div','metric-grid'),warnings=el('div','monitor-warnings'),containers=el('div','monitor-containers');
    root.append(toolbar,status,grid,warnings,containers);
    function chart(points, series, format) {
      const ns='http://www.w3.org/2000/svg',svg=document.createElementNS(ns,'svg');svg.setAttribute('viewBox','0 0 360 108');svg.setAttribute('role','img');svg.setAttribute('aria-label','最近 60 个采样点，'+series.map(s=>s.label).join(' / '));
      const all=points.flatMap(p=>series.map(s=>p[s.key])).filter(finite),max=Math.max(...all,1),minTime=points[0]?.time||0,span=Math.max(1,(points.at(-1)?.time||0)-minTime);
      const line=document.createElementNS(ns,'path');line.setAttribute('d','M6 8V86H354');line.setAttribute('class','chart-axis');svg.append(line);
      for (const [i,item] of series.entries()) {
        let path='',open=false;
        for (const point of points) {
          if (!finite(point[item.key])) {open=false;continue;}
          const x=6+(point.time-minTime)/span*348,y=86-point[item.key]/max*72;
          path+=(open?'L':'M')+x.toFixed(1)+' '+y.toFixed(1);open=true;
          const dot=document.createElementNS(ns,'circle');dot.setAttribute('cx',x);dot.setAttribute('cy',y);dot.setAttribute('r','2');dot.setAttribute('class','chart-series series-'+i);
          const title=document.createElementNS(ns,'title');title.textContent=new Date(point.time).toLocaleTimeString()+' · '+item.label+' '+format(point[item.key]);dot.append(title);svg.append(dot);
        }
        const curve=document.createElementNS(ns,'path');curve.setAttribute('d',path);curve.setAttribute('class','chart-series series-'+i);svg.append(curve);
      }
      for(const [x,y,text] of [[8,12,format(max)],[6,103,points.length?new Date(minTime).toLocaleTimeString(): ''],[285,103,points.length?new Date(points.at(-1).time).toLocaleTimeString():'']]) {
        const t=document.createElementNS(ns,'text');t.setAttribute('x',x);t.setAttribute('y',y);t.textContent=text;svg.append(t);
      }
      return svg;
    }
    function sample(key,time,values) {
      const points=history[key] ||= [];
      if (points.at(-1)?.time!==time) {points.push({time,...values});if(points.length>60)points.shift();}
      return points;
    }
    function render(data) {
      const now=Date.now(), budget=data.budget||{}, network=data.network;
      if (uid!==data.podUid) {history={};previousNetwork=null;lastRate=null;uid=data.podUid;}
      if (network) {
        if(network.timestamp!==previousNetwork?.timestamp) {lastRate=networkRate(previousNetwork,network);previousNetwork=network;}
      } else {previousNetwork=null;lastRate=null;}
      const timestamp=Date.parse(data.metricsTimestamp)||now, diskTime=Date.parse(data.diskTimestamp)||now, networkTime=Date.parse(network?.timestamp)||now;
      const cards=[
        {key:'cpu',label:'CPU',value:data.cpuMillicores,format:cpu,time:timestamp,source:data.sources?.cpuMemory,request:budget.cpuRequestMilli,limit:budget.cpuLimitMilli,partial:budget.cpuLimitPartial},
        {key:'memory',label:'内存 · Working set',value:data.memoryBytes,format:bytes,time:timestamp,source:data.sources?.cpuMemory,request:budget.memoryRequestBytes,limit:budget.memoryLimitBytes,partial:budget.memoryLimitPartial},
        {key:'disk',label:'临时磁盘 · 已使用',value:data.diskUsedBytes,format:bytes,time:diskTime,source:data.sources?.disk},
        {key:'network',label:'网络 · 接收 / 发送',value:lastRate?.rx,format:v=>finite(v)?bytes(v)+'/s':'—',time:networkTime,source:data.sources?.network},
      ];
      grid.replaceChildren();
      for (const card of cards) {
        const box=el('section','metric-card');box.dataset.metric=card.key;
        box.append(el('div','metric-label',card.label),el('strong','metric-value',card.key==='network'&&lastRate ? bytes(lastRate.rx)+'/s ↓ · '+bytes(lastRate.tx)+'/s ↑' : card.format(card.value)));
        if(card.key==='cpu'||card.key==='memory') {
          const limits='Request '+(finite(card.request)?card.format(card.request):'未设置')+' · Limit '+(card.partial?'部分未设置':finite(card.limit)?card.format(card.limit):'未设置');
          box.append(el('div','metric-budget muted',limits));
          if(finite(card.limit)&&card.limit>0&&finite(card.value)) {
            const meter=el('div','metric-meter'),fill=el('i');fill.style.width=Math.min(100,card.value/card.limit*100)+'%';meter.append(fill);meter.setAttribute('data-tooltip','用量 / Limit：'+number(card.value/card.limit*100)+'%');box.append(meter);
          }
        }
        const values=card.key==='network'?{rx:lastRate?.rx??null,tx:lastRate?.tx??null}:{value:card.value??null};
        const points=sample(card.key,card.time,values),series=card.key==='network'?[{key:'rx',label:'接收 ↓'},{key:'tx',label:'发送 ↑'}]:[{key:'value',label:card.label}];
        box.append(chart(points,series,card.format));
        const available=finite(card.value), stale=now-card.time>120000;
        box.append(el('div','metric-source muted',available?(stale?'数据较旧 · ':'')+(card.source||'')+' · '+new Date(card.time).toLocaleTimeString(): card.key==='network'&&network?'等待下一次有效采样；接收 ↓ / 发送 ↑':'指标不可用，详见下方原因'));
        grid.append(box);
      }
      status.textContent='最近刷新 '+new Date().toLocaleTimeString()+' · 保留最近 60 个采样点'+(data.metricsWindow?' · CPU 统计窗口 '+data.metricsWindow:'');
      warnings.replaceChildren(...(data.warnings||[]).map(w=>el('div','metric-warning',w.scope+'：'+w.message)));
      warnings.append(el('div','muted','临时磁盘包含容器可写层、日志和 emptyDir；挂载卷的使用量单独列出。网络由相邻采样的累计字节计算，首个采样和计数重置不显示速率。'));
      containers.replaceChildren(el('strong','','容器用量与资源配置'));
      const table=el('table'),head=el('thead'),tr=el('tr');['容器','CPU 用量','Request / Limit','内存用量','Request / Limit'].forEach(t=>tr.append(el('th','',t)));head.append(tr);table.append(head);
      const body=el('tbody');for(const config of budget.containers||[]) {
        const usage=(data.containers||[]).find(c=>c.name===config.name)||{},row=el('tr');
        [config.name+(config.kind==='container'?'':' · '+config.kind),cpu(usage.cpuMillicores),(finite(config.cpuRequestMilli)?cpu(config.cpuRequestMilli):'未设置')+' / '+(finite(config.cpuLimitMilli)?cpu(config.cpuLimitMilli):'未设置'),bytes(usage.memoryBytes),(finite(config.memoryRequestBytes)?bytes(config.memoryRequestBytes):'未设置')+' / '+(finite(config.memoryLimitBytes)?bytes(config.memoryLimitBytes):'未设置')].forEach(t=>{const td=el('td','',t);td.dataset.tooltip=t;row.append(td);});body.append(row);
      }table.append(body);containers.append(table);
      if(data.volumes?.length) {containers.append(el('strong','','挂载卷'));for(const v of data.volumes)containers.append(el('div','muted',v.name+' · 已用 '+bytes(v.usedBytes)+' / 容量 '+bytes(v.capacityBytes)));}
    }
    function stop() {revision++;clearTimeout(timer);timer=null;selected=null;busy=false;}
    async function poll() {
      if(!selected||busy)return;const current=revision;busy=true;refresh.disabled=true;
      status.textContent='正在读取指标…';
      try {
        const data=await invoke('kube/pod-metrics',selected);
        if(current!==revision)return;lastData=data;render(data);
      } catch(error) {
        if(current!==revision)return;
        // Do not leave old readings looking like current live data.
        grid.replaceChildren();containers.replaceChildren();warnings.replaceChildren(el('div','metric-warning',error.message||String(error)));status.textContent='读取失败 · '+new Date().toLocaleTimeString();previousNetwork=null;lastRate=null;
      } finally {
        if(current===revision) {busy=false;refresh.disabled=false;if(!paused)timer=setTimeout(poll,Number(interval.value||5)*1000);}
      }
    }
    function start(params) {stop();selected={...params};paused=false;pause.textContent='暂停';history={};uid=null;lastData=null;previousNetwork=null;grid.replaceChildren();warnings.replaceChildren();containers.replaceChildren();poll();}
    pause.onclick=()=>{paused=!paused;pause.textContent=paused?'继续':'暂停';clearTimeout(timer);if(!paused)poll();};
    refresh.onclick=()=>{clearTimeout(timer);poll();}; interval.onchange=()=>{clearTimeout(timer);if(!paused&&!busy)poll();};
    return {start,stop,refresh:()=>{clearTimeout(timer);return poll();},get history(){return history;},get data(){return lastData;}};
  }
  window.KubeMonitor={create,cpu,bytes,networkRate};
})();
