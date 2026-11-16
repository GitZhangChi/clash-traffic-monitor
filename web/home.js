'use strict';
const $ = id => document.getElementById(id);
const state = {range:'today', device:'', application:'', offset:0, showInactive:false, data:null, request:0, controller:null};
const fmt = bytes => {
  if (!Number.isFinite(bytes)) return '—';
  const units = ['B','KiB','MiB','GiB','TiB'];
  const n = bytes > 0 ? Math.min(Math.floor(Math.log(bytes)/Math.log(1024)),units.length-1) : 0;
  return `${(bytes/1024**n).toLocaleString('zh-CN',{maximumFractionDigits:n ? 2 : 0})} ${units[n]}`;
};
const dateLabel = ms => new Date(ms).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false});
const node = (tag, text, className) => {const e=document.createElement(tag);if(text!==undefined)e.textContent=text;if(className)e.className=className;return e;};
const dayValue = d => `${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')}`;
function timeRange() {
  const now=new Date(), start=new Date(now.getFullYear(),now.getMonth(),now.getDate()), end=new Date(start);
  end.setDate(end.getDate()+1);
  if(state.range==='hour') return [now.getTime()-3600000,now.getTime()+1000];
  if(state.range==='yesterday'){end.setTime(start.getTime());start.setDate(start.getDate()-1);}
  if(state.range==='week')start.setDate(start.getDate()-6);
  if(state.range==='custom'){
    const a=new Date(`${$('startDate').value}T00:00:00`),b=new Date(`${$('endDate').value}T00:00:00`);
    b.setDate(b.getDate()+1);
    return [a.getTime(),b.getTime()];
  }
  return [start.getTime(),end.getTime()];
}
async function refresh() {
  const serial=++state.request;
  const [start,end]=timeRange();
  if(!Number.isFinite(start)||!Number.isFinite(end)||end<=start||end-start>90*86400000){showNotice('请选择不超过 90 天的有效日期范围。');return;}
  const params=new URLSearchParams({start:String(start),end:String(end)});
  if(state.device)params.set('device',state.device);
  if(state.application)params.set('application',state.application);
  params.set('offset',String(state.offset));params.set('limit','50');
  state.controller?.abort();const controller=new AbortController();state.controller=controller;
  const timeout=setTimeout(()=>controller.abort(),15000);
  $('domainPrevious').disabled=$('domainNext').disabled=true;
  try{
    const response=await fetch(`/api/home/summary?${params}`,{cache:'no-store',signal:controller.signal});
    if(response.status===401){location.assign('/login');return;}
    if(!response.ok)throw new Error('暂时无法读取统计，请稍后刷新。');
    const data=await response.json();
    if(serial!==state.request)return;
    if(state.offset>0&&state.offset>=data.domainCount){state.offset=0;refresh();return;}
    state.data=data;render(data);document.body.dataset.renderedRequest=String(serial);
  }catch(error){if(serial===state.request){showNotice(error.name==='AbortError'?'读取统计超时，请稍后刷新。':error.message||'暂时无法读取统计。');$('status').textContent='连接中断';$('status').classList.add('stale');}}
  finally{clearTimeout(timeout);if(state.controller===controller)state.controller=null;}
}
function showNotice(text){$('notice').textContent=text;$('notice').hidden=!text;}
function deviceSymbol(name){const n=name.toLowerCase();if(n.includes('tv')||n.includes('电视'))return '▣';if(n.includes('watch'))return '◷';if(n.includes('phone'))return '▯';if(n.includes('ipad'))return '▯';if(n.includes('camera'))return '◉';if(n.includes('mac'))return '▱';if(n.includes('router')||n.includes('路由器'))return '⌘';return '◇';}
function render(data) {
  const fresh=data.lastSample&&Date.now()-data.lastSample<20000&&!data.collectionError;
  $('status').textContent=fresh?'正在采集':'采集延迟';$('status').classList.toggle('stale',!fresh);
  $('collectionSince').textContent=data.collectedSince?`${dateLabel(data.collectedSince)} 开始记录 · ${data.lastSample?dateLabel(data.lastSample)+' 更新':'等待采样'}`:'首次采样后开始记录';
  const selected=data.devices.find(d=>d.id===state.device);
  $('selectedName').textContent=selected?`/ ${selected.name}`:'';
  $('total').textContent=fmt(data.totals.total);$('download').textContent=fmt(data.totals.download);$('upload').textContent=fmt(data.totals.upload);
  const knownDevices=data.devices.filter(d=>d.mac);
  const unresolved=data.devices.filter(d=>d.id.startsWith('ip:')).length;
  $('activeCount').textContent=knownDevices.filter(d=>d.total>0).length;
  $('inventoryCount').textContent=`已发现 ${knownDevices.length} 台设备${unresolved?' · '+unresolved+' 个地址待归属':' · IPv4 / IPv6 合并'}`;
  $('downloadDetail').textContent=state.device?'所选设备的采样用量':`当前全屋下行 ${fmt(fresh?data.downloadRate:0)}/s`;
  $('uploadDetail').textContent=state.device?'所选设备的采样用量':`当前全屋上行 ${fmt(fresh?data.uploadRate:0)}/s`;
  $('totalDetail').textContent=state.device?'设备用量按连接采样统计':'总量含未归属的短连接用量';
  const unknown=data.applications.find(a=>a.id==='unknown')?.total||0;
  const percent=bytes=>data.totals.total?`${(bytes/data.totals.total*100).toFixed(2)}%`:'0%';
  $('classification').textContent=`未识别服务 ${fmt(unknown)}（${percent(unknown)}）${state.device?'':` · 未采样到连接明细 ${fmt(data.unattributed.total)}（${percent(data.unattributed.total)}）`}`;
  showNotice([data.collectionError,data.inventoryError].filter(Boolean).join('；'));
  renderDevices(data);
  renderApplications(data);renderDomains(data);renderChart(data);
  $('coverage').textContent=`统计包含 sing-box 接管的直连和代理流量，保留近 ${data.retentionDays} 天记录。${!state.device&&data.unattributed.total>0?'本时段另有 '+fmt(data.unattributed.total)+' 未归属到设备或域名（包括采样间隔内结束的短连接）。':''}`;
}
function renderDevices(data){
  const knownDevices=data.devices.filter(d=>d.mac);
  const inactive=data.devices.filter(d=>d.total===0&&d.id!==state.device).length;
  $('inactiveCount').textContent=String(inactive);
  const body=$('devices');body.replaceChildren();
  const visible=data.devices.filter(d=>state.showInactive||d.total>0||d.id===state.device);
  for(const d of visible){
    const row=node('tr');row.classList.toggle('selected',d.id===state.device);
    const identity=node('td'),wrap=node('div',undefined,'device-name'),text=node('div');
    wrap.append(node('span',deviceSymbol(d.name),'device-symbol'));
    const button=node('button',d.name,'device-button');button.type='button';button.title=d.name;button.addEventListener('click',()=>selectDevice(d.id));
    const ipv4=d.addresses.find(a=>!a.includes(':'));
    const ipv6=d.addresses.filter(a=>a.includes(':')&&!a.startsWith('fe80:')).length;
    const address=node('span',`${ipv4||d.mac||d.addresses[0]||'地址待更新'}${ipv6?' · IPv6 × '+ipv6:''}`,'address');address.title=d.addresses.join('\n');
    text.append(button,address);wrap.append(text);identity.append(wrap);
    const du=node('td',undefined,'du');du.append(node('div',`↓ ${fmt(d.download)}`),node('div',`↑ ${fmt(d.upload)}`,'up'));
    row.append(identity,du,node('td',fmt(d.total)));body.append(row);
  }
  if(!visible.length){const row=node('tr'),cell=node('td','此时段没有设备流量，可勾选显示零用量设备','empty');cell.colSpan=3;row.append(cell);body.append(row);}
  $('deviceBadge').textContent=String(knownDevices.length);
}
function selectDevice(id){state.device=id;state.application='';state.offset=0;refresh();}
function renderApplications(data){
  const container=$('applications');container.replaceChildren();
  $('applicationBadge').textContent=String(data.applications.length);
  const maximum=Math.max(1,...data.applications.map(a=>a.total));
  for(const app of data.applications){
    const button=node('button',undefined,'app-button');button.type='button';button.dataset.application=app.id;button.classList.toggle('selected',app.id===state.application);button.setAttribute('aria-pressed',String(app.id===state.application));
    const row=node('div',undefined,'app-row');row.append(node('span',app.name,'app-name'),node('span',fmt(app.total),'app-volume'));
    const track=node('div',undefined,'app-track'),fill=node('div',undefined,'app-fill');fill.style.width=`${Math.max(0.5,app.total/maximum*100)}%`;track.append(fill);
    button.append(row,track);
    if(app.tunnel)button.append(node('p','若设备自行代理，隧道内的具体 App 无法识别','app-caption'));
    if(app.description)button.append(node('p',app.description,'app-caption'));
    button.addEventListener('click',()=>{state.application=state.application===app.id?'':app.id;state.offset=0;refresh();});
    container.append(button);
  }
  if(!data.applications.length)container.append(node('p','此时间范围尚无流量记录。首次采集前的历史无法追溯。','empty'));
}
function renderDomains(data){
  const app=data.applications.find(a=>a.id===state.application);
  $('domainTitle').textContent=state.application?`目标域名 · ${app?.name||state.application}`:'目标域名';$('clearApp').hidden=!state.application;
  const body=$('domains');body.replaceChildren();
  const domains=data.domains;
  for(const d of domains){const row=node('tr'),service=node('td',d.application);if(d.description)service.title=d.description;row.append(node('td',d.host),node('td',d.deviceName),service,node('td',fmt(d.total)));body.append(row);}
  if(!domains.length){const row=node('tr'),cell=node('td','此时间范围没有对应的域名记录','empty');cell.colSpan=4;row.append(cell);body.append(row);}
  $('domainPage').textContent=`${domains.length?data.domainOffset+1:0}–${data.domainOffset+domains.length} / ${data.domainCount} 条 · 筛选合计 ${fmt(data.domainUsage.total)}`;
  $('domainPrevious').disabled=data.domainOffset===0;
  $('domainNext').disabled=data.domainOffset+domains.length>=data.domainCount;
}
function renderChart(data){
  const container=$('chart');container.replaceChildren();
  const step=data.end-data.start>2*86400000?3600000:300000;
  const start=Math.max(data.start,Math.min(data.collectedSince||data.start,data.end-step));
  const end=Math.max(start+step,Math.min(data.end,Date.now()+step));
  const from=Math.floor(start/step)*step,to=Math.ceil(end/step)*step;
  const pointMap=new Map(data.timeline.map(p=>[p.timestamp,p]));
  const points=[];for(let t=from;t<to;t+=step)points.push(pointMap.get(t)||{timestamp:t,upload:0,download:0,total:0});
  const maximum=Math.max(1,...points.map(p=>p.total));
  const ns='http://www.w3.org/2000/svg',svg=document.createElementNS(ns,'svg');svg.setAttribute('viewBox','0 0 1000 160');svg.setAttribute('preserveAspectRatio','none');
  for(const y of [20,65,110,155]){const line=document.createElementNS(ns,'line');for(const [key,value]of Object.entries({x1:0,x2:1000,y1:y,y2:y,stroke:'#edf1f6','stroke-width':1}))line.setAttribute(key,String(value));svg.append(line);}
  const width=1000/points.length;
  points.forEach((p,i)=>{
    let base=155;
    for(const [bytes,color]of [[p.download,'#6d9ce5'],[p.upload,'#edb660']]){
      const height=bytes/maximum*128;if(height<=0)continue;
      const rect=document.createElementNS(ns,'rect');
      for(const [key,value]of Object.entries({x:i*width+Math.min(2,width*.1),y:base-height,width:Math.max(1,width-Math.min(4,width*.2)),height,rx:Math.min(2,width/8),fill:color}))rect.setAttribute(key,String(value));
      const title=document.createElementNS(ns,'title');title.textContent=`${dateLabel(p.timestamp)} · 下载 ${fmt(p.download)} / 上传 ${fmt(p.upload)}`;rect.append(title);svg.append(rect);base-=height;
    }
  });
  container.append(svg);
  if(!data.timeline.length)container.append(node('div','采集后逐步积累流量趋势','chart-empty'));
  $('chartStart').textContent=dateLabel(from);$('chartMiddle').textContent=dateLabel(from+(to-from)/2);$('chartEnd').textContent=dateLabel(to);
  $('chartCaption').textContent=`每${step===3600000?'小时':'5 分钟'}用量 · ${state.device?'所选设备':'全屋总量'}`;
}
document.querySelectorAll('[data-range]').forEach(button=>button.addEventListener('click',()=>{
  state.range=button.dataset.range;document.querySelectorAll('[data-range]').forEach(b=>b.classList.toggle('active',b===button));
  $('customRange').hidden=state.range!=='custom';state.application='';state.offset=0;if(state.range!=='custom')refresh();
}));
$('customRange').addEventListener('submit',event=>{event.preventDefault();state.offset=0;refresh();});
$('allDevices').addEventListener('click',()=>selectDevice(''));
$('clearApp').addEventListener('click',()=>{state.application='';state.offset=0;refresh();});
$('showInactive').addEventListener('change',event=>{state.showInactive=event.target.checked;if(state.data)renderDevices(state.data);});
$('domainPrevious').addEventListener('click',()=>{state.offset=Math.max(0,state.offset-50);refresh();});
$('domainNext').addEventListener('click',()=>{state.offset+=50;refresh();});
$('refresh').addEventListener('click',refresh);
$('startDate').value=$('endDate').value=dayValue(new Date());
document.addEventListener('visibilitychange',()=>{if(!document.hidden)refresh();});
setInterval(()=>{if(!document.hidden&&!state.controller)refresh();},5000);
refresh();
