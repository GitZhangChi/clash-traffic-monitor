'use strict';
const $ = id => document.getElementById(id);
const state = {range:'today', device:'', application:'', data:null, request:0};
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
  try{
    const response=await fetch(`/api/home/summary?${params}`,{cache:'no-store',signal:AbortSignal.timeout(15000)});
    if(response.status===401){location.assign('/login');return;}
    if(!response.ok)throw new Error('暂时无法读取统计，请稍后刷新。');
    const data=await response.json();
    if(serial!==state.request)return;
    state.data=data;render(data);
  }catch(error){if(serial===state.request){showNotice(error.message||'暂时无法读取统计。');$('status').textContent='连接中断';$('status').classList.add('stale');}}
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
  showNotice([data.collectionError,data.inventoryError].filter(Boolean).join('；'));
  const body=$('devices');body.replaceChildren();
  for(const d of data.devices){
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
  if(!data.devices.length){const row=node('tr'),cell=node('td','等待设备信息与流量采样','empty');cell.colSpan=3;row.append(cell);body.append(row);}
  $('deviceBadge').textContent=String(knownDevices.length);
  renderApplications(data);renderDomains(data);renderChart(data);
  $('coverage').textContent=`统计包含 sing-box 接管的直连和代理流量，保留近 ${data.retentionDays} 天记录。${!state.device&&data.unattributed.total>0?'本时段另有 '+fmt(data.unattributed.total)+' 未归属到设备或域名（包括采样间隔内结束的短连接）。':''}`;
}
function selectDevice(id){state.device=id;state.application='';refresh();}
function renderApplications(data){
  const container=$('applications');container.replaceChildren();
  $('applicationBadge').textContent=String(data.applications.length);
  const maximum=Math.max(1,...data.applications.map(a=>a.total));
  for(const app of data.applications){
    const button=node('button',undefined,'app-button');button.type='button';button.classList.toggle('selected',app.name===state.application);
    const row=node('div',undefined,'app-row');row.append(node('span',app.name,'app-name'),node('span',fmt(app.total),'app-volume'));
    const track=node('div',undefined,'app-track'),fill=node('div',undefined,'app-fill');fill.style.width=`${Math.max(0.5,app.total/maximum*100)}%`;track.append(fill);
    button.append(row,track);
    if(app.tunnel)button.append(node('p','若设备自行代理，隧道内的具体 App 无法识别','app-caption'));
    button.addEventListener('click',()=>{state.application=state.application===app.name?'':app.name;renderApplications(state.data);renderDomains(state.data);});
    container.append(button);
  }
  if(!data.applications.length)container.append(node('p','此时间范围尚无流量记录。首次采集前的历史无法追溯。','empty'));
}
function renderDomains(data){
  $('domainTitle').textContent=state.application?`目标域名 · ${state.application}`:'目标域名';$('clearApp').hidden=!state.application;
  const body=$('domains');body.replaceChildren();
  const domains=data.domains.filter(d=>!state.application||d.application===state.application);
  for(const d of domains){const row=node('tr');row.append(node('td',d.host),node('td',d.deviceName),node('td',d.application),node('td',fmt(d.total)));body.append(row);}
  if(!domains.length){const row=node('tr'),cell=node('td','此时间范围没有对应的域名记录','empty');cell.colSpan=4;row.append(cell);body.append(row);}
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
  $('customRange').hidden=state.range!=='custom';state.application='';if(state.range!=='custom')refresh();
}));
$('customRange').addEventListener('submit',event=>{event.preventDefault();refresh();});
$('allDevices').addEventListener('click',()=>selectDevice(''));
$('clearApp').addEventListener('click',()=>{state.application='';renderApplications(state.data);renderDomains(state.data);});
$('refresh').addEventListener('click',refresh);
$('startDate').value=$('endDate').value=dayValue(new Date());
document.addEventListener('visibilitychange',()=>{if(!document.hidden)refresh();});
setInterval(()=>{if(!document.hidden)refresh();},5000);
refresh();
