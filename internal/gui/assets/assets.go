package assets

var IndexHTML = []byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Remote Assist GUI</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
/* 滚动条：浏览器默认的浅色宽条在深色界面里非常跳。Firefox 用 scrollbar-*，
   Chromium/Edge 用 ::-webkit-*，两套都给。滑块用 background-clip:content-box +
   透明边框留出内缩，视觉上更细且不贴边。 */
*{scrollbar-width:thin;scrollbar-color:#45475a transparent}
::-webkit-scrollbar{width:10px;height:10px}
::-webkit-scrollbar-track{background:transparent}
::-webkit-scrollbar-thumb{background:#45475a;border-radius:5px;border:2px solid transparent;background-clip:content-box}
::-webkit-scrollbar-thumb:hover{background:#585b70;background-clip:content-box}
::-webkit-scrollbar-corner{background:transparent}
/* 拖动分隔条时全局禁选：否则横向拖拽会把界面文字一路选中（蓝底） */
body.resizing{user-select:none;cursor:col-resize}
html,body{height:100%;font-family:Consolas,"Microsoft YaHei",monospace;font-size:13px;background:#1e1e2e;color:#cdd6f4}
#app{display:flex;flex-direction:column;height:100%}
header{padding:5px 12px;background:#181825;border-bottom:1px solid #313244;display:flex;gap:6px;align-items:center}
header input{background:#313244;border:1px solid #45475a;color:#cdd6f4;padding:3px 8px;border-radius:3px}
header input.code{width:130px}
header input.server{width:160px}
button{background:#89b4fa;color:#11111b;border:none;padding:3px 10px;border-radius:3px;cursor:pointer;font-weight:bold;font-size:12px}
button:hover{background:#74a0f0}
button.sec{background:#f38ba8}
button.sm{padding:1px 6px;font-weight:normal;font-size:11px}
#status{margin-left:auto;padding:2px 9px;border-radius:8px;background:#45475a;color:#cdd6f4;font-size:11px;font-weight:normal}
#status.ok{background:#a6e3a1;color:#11111b}
#status.bad{background:#f38ba8;color:#11111b}
#status:disabled{cursor:default;opacity:1}
#conn-info{display:none;position:fixed;right:12px;top:39px;width:min(340px,calc(100vw - 24px));z-index:1000;background:#181825;border:1px solid #45475a;border-radius:4px;box-shadow:0 8px 24px rgba(0,0,0,.55);padding:7px 10px}
#conn-info .conn-title{display:flex;align-items:center;justify-content:space-between;padding-bottom:5px;margin-bottom:3px;border-bottom:1px solid #313244;font-weight:bold}
#conn-info .conn-row{display:grid;grid-template-columns:78px minmax(0,1fr);gap:8px;padding:3px 0;font-size:11px}
#conn-info .conn-label{color:#9399b2}
#conn-info .conn-value{color:#cdd6f4;overflow-wrap:anywhere;text-align:right}
#conn-info .transport-p2p{color:#a6e3a1}
#upgrade-notice{display:none;min-height:32px;padding:5px 12px;background:#332d1f;border-bottom:1px solid #665c3a;color:#f9e2af;align-items:center;gap:8px}
#upgrade-notice .msg{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
#upgrade-notice.success{background:#203326;border-color:#3f684a;color:#a6e3a1}
main{flex:1;display:flex;min-height:0}
#tree-panel{width:260px;min-width:120px;display:flex;flex-direction:column;flex-shrink:0}
#resizer{width:5px;background:#313244;cursor:col-resize;flex-shrink:0;transition:background .1s;user-select:none;touch-action:none}
#resizer:hover,#resizer.dragging{background:#89b4fa}
#tree-hdr{padding:3px 8px;background:#181825;border-bottom:1px solid #313244;font-size:11px;color:#9399b2;display:flex;align-items:center;gap:4px}
#tree-hdr input{flex:1;background:#313244;border:1px solid #45475a;color:#cdd6f4;padding:1px 5px;border-radius:2px;font-size:11px}
#tree{flex:1;overflow:auto;padding:3px}
.tdir{padding:1px 3px;cursor:pointer;white-space:nowrap;border-radius:2px;font-size:12px;display:flex;align-items:center;gap:2px;user-select:none}
.tdir:hover{background:#313244}
.tdir .arr{display:inline-block;width:12px;text-align:center;color:#6c7086;font-size:10px;flex-shrink:0}
.tdir .arr.open::after{content:"\25BE"}
.tdir .arr.closed::after{content:"\25B8"}
.tfile{padding:1px 3px;cursor:pointer;white-space:nowrap;border-radius:2px;font-size:12px;display:flex;align-items:center;gap:2px}
.tfile:hover{background:#313244}
.tfile .ico{font-size:11px;flex-shrink:0}
.tfile .sz{color:#6c7086;font-size:10px;margin-left:auto;padding-left:6px}
.tchildren{margin-left:14px}
.tloading{color:#6c7086;font-size:11px;padding:2px 4px}
#right{flex:1;display:flex;flex-direction:column;min-width:0}
.tabs{display:flex;background:#181825;border-bottom:1px solid #313244;overflow-x:auto;min-height:30px}
.tabs .tab{padding:4px 12px;font-size:12px;color:#9399b2;cursor:pointer;border-right:1px solid #313244;display:flex;align-items:center;gap:4px;white-space:nowrap;user-select:none}
.tabs .tab:hover{background:#313244}
.tabs .tab.active{color:#cdd6f4;background:#1e1e2e;border-bottom:1px solid #1e1e2e;margin-bottom:-1px}
.tabs .tab .close{font-size:10px;color:#6c7086;margin-left:4px;cursor:pointer}
.tabs .tab .close:hover{color:#f38ba8}
.tabcontent{flex:1;display:flex;flex-direction:column;min-height:0;overflow:hidden}
/* 各面板与其输出区都要 min-height:0：flex 子项默认 min-height:auto，意思是"不许缩到比
   内容还矮"，于是输出区不滚而是把自己撑高，再被 .tabcontent 的 overflow:hidden 裁掉
   ——表现就是没滚动条 + 内容被截断。overflow:auto 只有配上 min-height:0 才生效。 */
#term-box{display:none;flex-direction:column;flex:1;min-height:0}
#term-box .input-row{display:flex;align-items:center;padding:4px 8px;background:#11111b;gap:4px}
#term-box .prompt{color:#a6e3a1;font-weight:bold;user-select:none}
#termInput{flex:1;background:transparent;border:none;color:#cdd6f4;font-family:inherit;font-size:13px;outline:none}
#term-output{flex:1;min-height:0;overflow:auto;padding:6px 10px;white-space:pre-wrap;word-break:break-all;font-size:12px;line-height:1.4}
#search-box{display:none;flex-direction:column;flex:1;min-height:0}
#search-box .sp{padding:6px 10px;background:#181825;border-bottom:1px solid #313244;display:flex;gap:6px;align-items:center}
#search-box .sp label{color:#9399b2;font-size:11px}
#search-box .sp input{background:#313244;border:1px solid #45475a;color:#cdd6f4;padding:2px 6px;border-radius:2px;font-size:12px;flex:1}
#search-output{flex:1;min-height:0;overflow:auto;padding:6px 10px;font-size:12px}
#file-box{display:none;flex-direction:column;flex:1;min-height:0}
#file-hdr{padding:3px 10px;background:#181825;border-bottom:1px solid #313244;display:flex;align-items:center;gap:6px;font-size:11px}
#file-hdr .path{color:#89b4fa;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
#file-hdr .info{color:#6c7086}
#file-hdr .image-tool{display:none;width:25px;height:23px;padding:0;align-items:center;justify-content:center;font-size:12px}
#file-hdr #fZoomFit{width:36px}
#file-content{flex:1;min-height:0;overflow:auto;padding:6px 10px;white-space:pre-wrap;word-break:break-all;font-size:12px;line-height:1.5}
#file-content.image-preview{padding:0;display:block;position:relative;white-space:normal;background:#11111b;overscroll-behavior:contain}
#file-content.image-preview .image-stage{position:relative;min-width:100%;min-height:100%}
#file-content.image-preview img{display:block;position:absolute;max-width:none;max-height:none;object-fit:contain;user-select:none;-webkit-user-drag:none}
#file-editor{display:none;flex:1;min-height:0}
#file-editor textarea{width:100%;height:100%;background:#1e1e2e;border:none;color:#cdd6f4;font-family:inherit;font-size:12px;padding:6px 10px;resize:none;outline:none;line-height:1.5}
#proc-box{display:none;flex-direction:column;flex:1;min-height:0}
#proc-output{flex:1;min-height:0;overflow:auto;padding:6px 10px;font-size:12px;white-space:pre-wrap}
.err{color:#f38ba8}.meta{color:#9399b2}.ok{color:#a6e3a1}.cmd{color:#a6e3a1}
/* 右键菜单 */
#ctxmenu{display:none;position:fixed;background:#181825;border:1px solid #45475a;border-radius:4px;padding:4px 0;z-index:999;min-width:140px;box-shadow:0 4px 12px rgba(0,0,0,.5)}
#ctxmenu .ci{padding:4px 14px;font-size:12px;cursor:pointer;color:#cdd6f4;white-space:nowrap}
#ctxmenu .ci:hover{background:#313244}
#ctxmenu .ci.danger{color:#f38ba8}
</style>
</head>
<body>
<div id="app">
  <header>
    <span class="meta">Code:</span>
    <input class="code" id="code" placeholder="ABCD-EFGHIJ">
    <span class="meta">Relay:</span>
    <input class="server" id="server" placeholder="host:port">
    <label class="meta"><input type="checkbox" id="noauth"> NoAuth</label>
    <button id="connect">连接</button>
    <button class="sm" id="btnProc">进程</button>
    <button id="status" title="连接详情" disabled>未连接</button>
  </header>
  <div id="upgrade-notice"><span class="msg" id="upgrade-msg"></span><button class="sm" id="upgrade-select">选择升级包</button><input type="file" id="upgrade-file" style="display:none"></div>
  <main>
    <div id="tree-panel">
      <div id="tree-hdr"><span>远端</span><input id="treePath" placeholder="路径回车跳转"></div>
      <div id="tree"></div>
    </div>
    <div id="resizer" title="拖动调整目录树宽度"></div>
    <div id="right">
      <div class="tabs" id="tabbar"></div>
      <div class="tabcontent">
        <div id="term-box"><div class="input-row"><span class="prompt">$</span><input id="termInput" placeholder="输入命令" autocomplete="off"></div><div id="term-output"></div></div>
        <div id="search-box"><div class="sp"><label>正则:</label><input id="grepPattern" placeholder="TODO"><label>目录:</label><input id="grepRoot" placeholder="C:\\"><button class="sm" id="grepRun">搜索</button></div><div id="search-output"></div></div>
        <div id="proc-box"><div id="proc-output"></div></div>
        <div id="file-box"><div id="file-hdr"><span class="path" id="fpath"></span><span class="info" id="finfo"></span><button class="sm image-tool" id="fZoomOut" title="缩小" aria-label="缩小">−</button><button class="sm image-tool" id="fZoomFit" title="适合窗口">适配</button><button class="sm image-tool" id="fZoomIn" title="放大" aria-label="放大">+</button><button class="sm image-tool" id="fDownload" title="另存为" aria-label="另存为">⇩</button><button class="sm" id="fEdit" style="display:none">编辑</button><button class="sm" id="fSave" style="display:none;background:#a6e3a1">保存</button><button class="sm" id="fCancel" style="display:none">取消</button></div><div id="file-content"></div><div id="file-editor"><textarea id="fEditor"></textarea></div></div>
      </div>
    </div>
  </main>
</div>
<div id="conn-info"></div>
<div id="ctxmenu"></div>
<script>
const $=s=>document.querySelector(s),status=$('#status');
// 访问令牌从启动 URL 的 query 里取，之后每次调 API 都带 X-Auth-Token 头。
// 用自定义头是刻意的：自定义头必然触发 CORS 预检，跨站页面连预检都过不去，
// 从根上挡掉「任意网页偷偷调本地 GUI 去操控远端机器」。EventSource 和下载用的 <a>
// 设不了头，只能退回 query 参数（令牌本身即凭据，跨站方拿不到，同样安全）。
const TOKEN=new URLSearchParams(location.search).get('token')||'';
function withToken(path){return path+(path.includes('?')?'&':'?')+'token='+encodeURIComponent(TOKEN)}
let connected=false,cmdHistory=[],histIdx=-1,currentFilePath='',peerVersion='',helpVersion='';
let tabs=[],activeTab=null;

function setStatus(ok,text){connected=ok;status.textContent=text;status.className=ok?'ok':(text==='未连接'?'':'bad');status.disabled=!ok;if(!ok)$('#conn-info').style.display='none';renderConnBtn()}
// 连接与断开共用一个按钮：连上了它就是「断开」，没连就是「连接」。
function renderConnBtn(){const b=$('#connect');if(!b)return;b.textContent=connected?'断开':'连接';b.className=connected?'sec':''}

const es=new EventSource(withToken('/api/events')); // EventSource 设不了自定义头，只能走 query
// 后端广播的全是具名事件（event: xxx，不带 data 字段），而 onmessage 只对带 data 的帧
// 触发——所以原先那条 onmessage→底部日志框的线永远不会有内容，日志框一直是空的。
// 日志框已移除：状态归右上角状态灯，值得留痕的（重连/丢失）直接说给终端。
es.addEventListener('connected',()=>{setStatus(true,'已连接');loadDrives()});
es.addEventListener('disconnected',()=>{setStatus(false,'已断开')});
es.addEventListener('reconnecting',()=>{setStatus(false,'重连中...');termLog('<span class="meta">连接断开，正在自动重连...</span>')});
es.addEventListener('lost',()=>{setStatus(false,'丢失');termLog('<span class="err">自动重连失败，请重新点击连接</span>')});
es.addEventListener('upgrade-progress',e=>{const n=$('#upgrade-notice');n.style.display='flex';n.className='';$('#upgrade-msg').textContent=e.data});
es.onerror=()=>{};

async function api(path,body){const ctrl=new AbortController();const t=setTimeout(()=>ctrl.abort(),30000);try{const r=await fetch(path,{method:'POST',headers:{'Content-Type':'application/json','X-Auth-Token':TOKEN},body:JSON.stringify(body||{}),signal:ctrl.signal});return await r.json()}catch(e){return{ok:false,error:String(e)}}finally{clearTimeout(t)}}

function formatConnectedTime(seconds){
  seconds=Math.max(0,Number(seconds)||0);if(seconds<60)return seconds+' 秒';if(seconds<3600)return Math.floor(seconds/60)+' 分钟';return Math.floor(seconds/3600)+' 小时 '+Math.floor(seconds%3600/60)+' 分钟';
}
function connectionRow(label,value,cls){return'<div class="conn-row"><span class="conn-label">'+esc(label)+'</span><span class="conn-value '+(cls||'')+'">'+esc(value||'—')+'</span></div>'}
async function showConnectionInfo(){
  if(!connected)return;const panel=$('#conn-info');panel.style.display='block';panel.innerHTML='<div class="conn-title"><span>连接状态</span><span class="meta">测量中...</span></div>';
  const r=await api('/api/status',{});if(!r.connected){panel.innerHTML='<div class="conn-title"><span>连接状态</span></div>'+connectionRow('状态','已断开');return}
  const mode=r.p2p?'P2P 直连':'Relay 中转',latency=r.latency_ms!==undefined?r.latency_ms+' ms':'不可用';
  panel.innerHTML='<div class="conn-title"><span>连接状态</span><span class="ok">'+esc(mode)+'</span></div>'+
    connectionRow('服务器',r.server)+connectionRow('传输',mode,r.p2p?'transport-p2p':'')+connectionRow('往返延迟',latency)+
    connectionRow('远端主机',r.peer_host)+connectionRow('远端版本',r.peer_version)+connectionRow('Help 版本',r.help_version)+
    connectionRow('会话 ID',r.session_id)+connectionRow('已连接',formatConnectedTime(r.connected_seconds));
}
status.onclick=e=>{e.stopPropagation();const panel=$('#conn-info');if(panel.style.display==='block')panel.style.display='none';else showConnectionInfo()};
$('#conn-info').onclick=e=>e.stopPropagation();
document.addEventListener('click',()=>{$('#conn-info').style.display='none'});

async function doConnect(){
  const code=$('#code').value.trim(),server=$('#server').value.trim(),noauth=$('#noauth').checked;
  resetRemoteState(); // 可能换了一台远端，旧缓存一律作废
  setStatus(false,'连接中...');
  const r=await api('/api/connect',{code,server,no_auth:noauth});
  if(r.ok){const d=parseMCP(r.result);setStatus(true,'已连接');setUpgradeInfo(d);loadDrives();openTerm()}
  else{setStatus(false,'失败');termLog('<span class="err">'+esc(r.error)+'</span>')}
}
async function doDisconnect(){
  await api('/api/disconnect',{});
  setStatus(false,'未连接');
  resetRemoteState();
  $('#tree').innerHTML='';closeAllTabs();openTerm(); // 终端是常驻页，断开后仍留着
}
// resetRemoteState 丢掉上一台远端的一切缓存。必须清：目录树缓存会让「连 A→断→连 B」
// 显示成 A 的目录；而 remoteOS 更要命——它决定 joinRemote 用 '\' 还是 '/'，残留下来会
// 让新远端的路径全拼错。
function resetRemoteState(){
  driveLoadGeneration++;
  for(const k in treeCache)delete treeCache[k];
  remoteOS='';
  $('#treePath').value='';
  peerVersion='';helpVersion='';$('#upgrade-notice').style.display='none';$('#upgrade-notice').className='';
}
$('#connect').onclick=async()=>{
  const b=$('#connect');b.disabled=true;
  try{connected?await doDisconnect():await doConnect()}finally{b.disabled=false;renderConnBtn()}
};

function versionParts(s){
  const m=String(s||'').match(/(?:^|\s)v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?/i);
  return m?[Number(m[1]),Number(m[2]),Number(m[3]),Number(m[4]||0)]:null;
}
function isVersionOlder(current,latest){
  const a=versionParts(current),b=versionParts(latest);if(!a||!b)return false;
  for(let i=0;i<a.length;i++){if(a[i]!==b[i])return a[i]<b[i]}
  return false;
}
function setUpgradeInfo(meta){
  peerVersion=meta.peer_version||'';helpVersion=meta.help_version||'';
  const n=$('#upgrade-notice');n.className='';
  if(!isVersionOlder(peerVersion,helpVersion)){n.style.display='none';return}
  $('#upgrade-msg').textContent='远端 share '+peerVersion+'，当前 help '+helpVersion;
  n.style.display='flex';$('#upgrade-select').disabled=false;$('#upgrade-select').style.display='inline-block';
}
$('#upgrade-select').onclick=()=>$('#upgrade-file').click();
$('#upgrade-file').onchange=async()=>{
  const input=$('#upgrade-file'),file=input.files&&input.files[0];if(!file)return;
  input.value='';
  if(!confirm('将 '+file.name+' 传到远端，验证后以先建后断方式升级 share。继续吗？'))return;
  const btn=$('#upgrade-select'),connBtn=$('#connect');btn.disabled=true;connBtn.disabled=true;
  $('#upgrade-notice').className='';$('#upgrade-notice').style.display='flex';
  $('#upgrade-msg').textContent='正在准备升级...';
  const form=new FormData();form.append('binary',file,file.name);
  try{
    const resp=await fetch('/api/upgrade',{method:'POST',headers:{'X-Auth-Token':TOKEN},body:form});
    const r=await resp.json();
    if(!r.ok){$('#upgrade-msg').textContent=r.error||('升级失败: HTTP '+resp.status);btn.disabled=false;return}
    const newVersion=r.peer_version||r.new_version||peerVersion;
    btn.style.display='none';resetRemoteState();$('#upgrade-notice').style.display='flex';$('#upgrade-notice').className='success';
    peerVersion=newVersion;$('#upgrade-msg').textContent='share 已升级到 '+newVersion+(r.warning?'；'+r.warning:'');loadDrives();
  }catch(e){$('#upgrade-msg').textContent='升级请求失败: '+String(e);btn.disabled=false}
  finally{connBtn.disabled=false;renderConnBtn()}
};

// ========== Tabs ==========
// tab \u7684 id \u53ea\u662f\u5185\u90e8\u6807\u8bc6\uff08\u53ef\u80fd\u542b\u8def\u5f84\u91cc\u7684 : \ /\uff09\uff0c\u4e0d\u662f DOM \u5143\u7d20 id\u2014\u2014
// \u6bcf\u4e2a tab \u663e\u5f0f\u8bb0\u5f55\u81ea\u5df1\u8be5\u663e\u793a\u54ea\u4e2a box\uff0c\u5426\u5219 $('#'+id) \u4f1a\u62ff\u8def\u5f84\u53bb\u5f53\u9009\u62e9\u5668\u7528\u3002
const BOXES=['term-box','search-box','proc-box','file-box'];
// fixed=true 的页常驻、不给关闭叉。终端就是这种：顶部的「终端」按钮已移除，
// 若还能把它关掉就再也开不出来了。
function openTab(id,title,boxId,showFn,fixed){
  let t=tabs.find(x=>x.id===id);
  if(!t){t={id,title,boxId,showFn,fixed};tabs.push(t)}
  else{t.title=title;t.boxId=boxId;t.showFn=showFn}
  activateTab(id);
}
function activateTab(id){
  const t=tabs.find(x=>x.id===id);
  if(!t)return;
  activeTab=id;
  BOXES.forEach(x=>$('#'+x).style.display='none');
  $('#'+t.boxId).style.display='flex';
  renderTabs();
  if(t.showFn)t.showFn(t);
}
function closeTab(id){
  const t=tabs.find(x=>x.id===id);
  if(t&&t.fixed)return; // 常驻页（终端）关不掉
  if(t&&t.imageObjectURL)URL.revokeObjectURL(t.imageObjectURL);
  if(t&&t.imageRequest)t.imageRequest.abort();
  tabs=tabs.filter(x=>x.id!==id);
  if(activeTab===id){activeTab=tabs.length?tabs[tabs.length-1].id:null;if(activeTab)activateTab(activeTab);else openTerm()}
  renderTabs();
}
function closeAllTabs(){for(const t of tabs){if(t.imageObjectURL)URL.revokeObjectURL(t.imageObjectURL);if(t.imageRequest)t.imageRequest.abort()}tabs=[];activeTab=null;$('#tabbar').innerHTML='';BOXES.forEach(x=>$('#'+x).style.display='none')}
function renderTabs(){
  const bar=$('#tabbar');bar.innerHTML='';
  for(const t of tabs){
    const d=document.createElement('div');d.className='tab'+(t.id===activeTab?' active':'');
    const label=document.createElement('span');label.textContent=t.title;d.appendChild(label);
    if(!t.fixed){
      const x=document.createElement('span');x.className='close';x.textContent='\u00d7';
      x.onclick=e=>{e.stopPropagation();closeTab(t.id)};
      d.appendChild(x);
    }
    d.onclick=()=>activateTab(t.id);
    bar.appendChild(d);
  }
}

// ========== exec 终端 ==========
function termLog(s){$('#term-output').innerHTML+=s+'\n';$('#term-output').scrollTop=$('#term-output').scrollHeight}
// termAppend 追加一段流式输出。用文本节点而不是 termLog 的 innerHTML 拼接：命令可以吐
// 半行（进度条、没换行的提示符），按行拼会把半行强行变成整行；textContent 也天然免转义。
function termAppend(text,cls){
  if(!text)return;
  const out=$('#term-output'),n=document.createElement('span');
  if(cls)n.className=cls;
  n.textContent=text;
  out.appendChild(n);
  out.scrollTop=out.scrollHeight;
}
function b64bytes(b64){const s=atob(b64),a=new Uint8Array(s.length);for(let i=0;i<s.length;i++)a[i]=s.charCodeAt(i);return a}
// scanUTF8 扫一段字节：complete = 合法且完整的前缀长度；ok=false 表示撞到了非法字节。
// 末尾"被块边界切断的多字节字符"不算非法（ok 仍为 true），它只是没走完，留给下一块拼。
// 这是区分"这不是 UTF-8"与"UTF-8 还没读完"的关键，也是不能直接用 fatal TextDecoder 的
// 原因：它会把切断的半个字符吞进内部缓冲，等下一块抛错时那几个字节已经找不回来了。
function scanUTF8(b){
  let i=0;
  while(i<b.length){
    const c=b[i];
    let need;
    if(c<0x80)need=1;
    else if(c>=0xC2&&c<=0xDF)need=2;
    else if(c>=0xE0&&c<=0xEF)need=3;
    else if(c>=0xF0&&c<=0xF4)need=4;
    else return {ok:false,complete:i};
    if(i+need>b.length)return {ok:true,complete:i}; // 尾巴被切断，下一块再说
    for(let j=1;j<need;j++)if((b[i+j]&0xC0)!==0x80)return {ok:false,complete:i};
    i+=need;
  }
  return {ok:true,complete:i};
}
// makeDecoder 造一个"先 UTF-8、不行就 GBK"的流式解码器。
// 远端控制台不一定说 UTF-8：中文 Windows 上 ping/dir 这些内置命令吐的是 GBK(CP936)，
// 硬按 UTF-8 解就是"���� Ping"。这里自己攒未解完的字节并用 scanUTF8 判断，一旦确认有
// 非法字节就把整条流改判 GBK 并保持（一条流的编码不会中途变）。
// 自己攒而不是让 fatal 解码器攒，是为了在改判 GBK 时把之前"被切断的半个字符"原样补回去
// ——否则那几个字节会被 UTF-8 解码器吞掉，中文首字直接丢。
// 局限：只覆盖 UTF-8 与 GBK；日文 CP932、俄文 CP1251 之类仍会乱码。
function makeDecoder(){
  const utf8=new TextDecoder('utf-8'),gbk=new TextDecoder('gbk');
  let gbkMode=false,pending=new Uint8Array(0);
  return (bytes,final=false)=>{
    let buf;
    if(pending.length){buf=new Uint8Array(pending.length+bytes.length);buf.set(pending);buf.set(bytes,pending.length)}
    else buf=bytes;
    if(!gbkMode&&!scanUTF8(buf).ok)gbkMode=true;
    if(gbkMode){
      pending=new Uint8Array(0); // 交给 gbk 解码器自己处理跨块的半个字符
      return gbk.decode(buf,{stream:!final});
    }
    const {complete}=scanUTF8(buf);
    pending=buf.slice(complete);
    const text=utf8.decode(buf.subarray(0,complete));
    if(!final)return text;
    const tail=utf8.decode(pending);pending=new Uint8Array(0);
    return text+tail;
  };
}
// 文件试读与终端输出共用 UTF-8/GBK 回退；文件额外识别 UTF-16 BOM。这只在用户主动点击
// "按文本查看" 后使用，默认预览仍拒绝非 UTF-8 内容，避免误编辑二进制文件。
function makeFileDecoder(){
  const fallback=makeDecoder();let decoder=null;
  return (bytes,final=false)=>{
    if(decoder)return decoder.decode(bytes,{stream:!final});
    if(bytes.length>=2&&bytes[0]===0xFF&&bytes[1]===0xFE){decoder=new TextDecoder('utf-16le');return decoder.decode(bytes.subarray(2),{stream:!final})}
    if(bytes.length>=2&&bytes[0]===0xFE&&bytes[1]===0xFF){decoder=new TextDecoder('utf-16be');return decoder.decode(bytes.subarray(2),{stream:!final})}
    return fallback(bytes,final);
  };
}
function openTerm(){openTab('term','终端','term-box',()=>{$('#termInput').focus()},true)}
$('#termInput').addEventListener('keydown',e=>{
  if(e.key==='Enter'){e.preventDefault();const v=$('#termInput').value;if(v.trim())execCmd(v);$('#termInput').value=''}
  else if(e.key==='ArrowUp'){e.preventDefault();if(cmdHistory.length){histIdx=Math.max(0,histIdx+1);$('#termInput').value=cmdHistory[cmdHistory.length-1-histIdx]||''}}
  else if(e.key==='ArrowDown'){e.preventDefault();histIdx=Math.min(cmdHistory.length,histIdx+1);$('#termInput').value=histIdx>=cmdHistory.length?'':cmdHistory[cmdHistory.length-1-histIdx]||''}
});
// remote 的 exec 不走 shell，直接敲 pwd/ls/dir/cd/管道 会因“找不到可执行文件”失败。
// 终端是给人用的，所以整行交给远端 shell 执行。远端是 Windows 还是 Unix 探测一次即知。
let remoteOS=''; // 'windows' | 'unix' | ''(未探测)
async function detectOS(){
  if(remoteOS)return remoteOS;
  const r=await api('/api/call',{tool:'exec',args:{argv:['cmd','/c','ver'],timeout_ms:8000}});
  if(!r.ok)return 'unix'; // 探测本身失败（网络抖动/超时）：这次先按 unix 猜，但**不缓存**
  const d=parseMCP(r.result);
  // 只有拿到确定答案才落缓存。否则 Windows 远端上偶发失败一次，就会把 remoteOS 永久
  // 钉死成 unix：之后所有命令都用 sh -c 发给 Windows，全部失败且不会自愈（只能刷页面）。
  remoteOS=/Windows/i.test(d.stdout||'')?'windows':'unix';
  return remoteOS;
}
function shellArgv(os,line){
  return os==='windows'?['powershell','-NoProfile','-Command',line]:['sh','-c',line];
}
async function execCmd(line){
  if(!connected||!line.trim())return;
  // clear/cls 是清「本地终端显示」，远端 shell 的 clear 清的是远端屏幕、传不回来，
  // 所以在前端直接拦截清空 DOM，不发给远端。
  const lc=line.trim().toLowerCase();
  if(lc==='clear'||lc==='cls'){$('#term-output').innerHTML='';return}
  if(!cmdHistory.length||cmdHistory[cmdHistory.length-1]!==line)cmdHistory.push(line);histIdx=-1;
  termLog('<span class="cmd">$ '+esc(line)+'</span>');
  try{
    const os=await detectOS();
    await execStream(shellArgv(os,line));
  }catch(e){termLog('<span class="err">'+esc(String(e))+'</span>')}
}

// execStream 边跑边渲染一条命令的输出：构建、tail、跑测试这类长命令不必等跑完才出字。
// 不走 api()——它有 30s abort，长命令必被腰斩；这里读 NDJSON 流直到远端收尾。
async function execStream(argv){
  const resp=await fetch('/api/exec/stream',{
    method:'POST',headers:{'Content-Type':'application/json','X-Auth-Token':TOKEN},body:JSON.stringify({argv})
  });
  if(!resp.ok||!resp.body){
    let msg='HTTP '+resp.status;
    try{const j=await resp.json();if(j&&j.error)msg=j.error}catch{}
    termLog('<span class="err">'+esc(msg)+'</span>');return;
  }
  // stdout / stderr 各一个 decoder：块边界可能切断多字节字符（中文日志里很常见），
  // 两条流交错共用一个 decoder 会互相污染那半个字符的状态。
  const decs={stdout:makeDecoder(),stderr:makeDecoder()};
  const reader=resp.body.getReader(),td=new TextDecoder();
  let buf='';
  for(;;){
    const {done,value}=await reader.read();
    if(done)break;
    buf+=td.decode(value,{stream:true});
    let i;
    while((i=buf.indexOf('\n'))>=0){
      const line=buf.slice(0,i);buf=buf.slice(i+1);
      if(line.trim())execEvent(JSON.parse(line),decs);
    }
  }
}

function execEvent(ev,decs){
  if(ev.type==='chunk'){
    const dec=decs[ev.stream]||decs.stdout;
    termAppend(dec(b64bytes(ev.data||'')),ev.stream==='stderr'?'err':'');
    return;
  }
  if(ev.type==='error'){termLog('<span class="err">'+esc(ev.error)+'</span>');return}
  if(ev.type==='result'){
    // 0.0.5 不会发流式块，而是把输出放在最终响应里；新版流式结果不带这两项，故兼容
    // 渲染不会重复输出。
    const d=parseMCP(ev.result);
    if(d.stdout)termAppend(d.stdout);
    if(d.stderr)termAppend(d.stderr,'err');
    if(d.error)termLog('<span class="err">无法执行: '+esc(d.error)+'</span>'); // 命令没能启动的原因
    if(d.exit_code)termLog('<span class="meta">exit: '+esc(d.exit_code)+'</span>');
  }
}

// ========== grep 搜索 ==========
function openSearch(root){
  if(root!==undefined)$('#grepRoot').value=root;
  openTab('search','搜索','search-box',()=>{$('#grepPattern').focus()});
}
$('#grepRun').onclick=async()=>{
  if(!connected)return;const pattern=$('#grepPattern').value.trim(),root=$('#grepRoot').value.trim();
  if(!pattern)return;
  const out=$('#search-output');out.innerHTML='<span class="meta">搜索中...</span>';
  const r=await api('/api/call',{tool:'grep',args:{pattern,root:root||undefined,max_matches:200}});
  if(!r.ok){out.innerHTML='<span class="err">'+esc(r.error)+'</span>';return}
  const d=parseMCP(r.result);
  let h='';
  if(d&&Array.isArray(d.matches)){h='<span class="meta">'+d.matches.length+' 条匹配'+(d.truncated?' (已截断)':'')+'</span>\n';for(const m of d.matches){h+=esc(m.file||'')+':'+esc(String(m.line||''))+'\n'}}
  else h=esc(JSON.stringify(d,null,2));
  out.innerHTML=h;out.scrollTop=out.scrollHeight;
};

// ========== 进程 ==========
function openProc(){openTab('proc','进程','proc-box',async()=>{$('#proc-output').innerHTML='<span class="meta">加载中...</span>';await loadProc()})}
$('#btnProc').onclick=()=>openProc();
async function loadProc(){
  if(!connected)return;const out=$('#proc-output');
  const r=await api('/api/call',{tool:'process_list',args:{max_count:100}});
  if(!r.ok){out.innerHTML='<span class="err">'+esc(r.error)+'</span>';return}
  const d=parseMCP(r.result);
  // 字段名以 ProcessListResult/ProcInfo 为准：procs（不是 processes），且没有 cpu 字段
  if(!Array.isArray(d.procs)){out.textContent=JSON.stringify(d,null,2);return}
  let h='<span class="meta">'+d.procs.length+' 个进程'+(d.truncated?'（共 '+esc(d.total)+'，已截断；可调 max_count）':'')+'</span>\n';
  h+='<span class="meta">'+'PID'.padStart(8)+'  '+'NAME'.padEnd(28)+' USER</span>\n';
  for(const p of d.procs){
    h+=esc(String(p.pid||'').padStart(8))+'  '+esc(String(p.name||'').padEnd(28))+' '+esc(p.user||'')+'\n';
  }
  out.innerHTML=h;out.scrollTop=0;
}

// ========== 文件 ==========
// read_file 单次默认只给 64 KiB 并用 bytes_len/eof 描述本次实际返回的内容，
// 所以这里必须按协议 offset += bytes_len 续读到 eof，否则大文件只能看到开头一截。
// 文件可能很大，一次性全拉会卡浏览器，所以分块“加载更多”：先给第一块，底部按钮
// 按需续读到 eof。编辑只在完整加载(eof)后开放——否则保存会用不完整内容覆盖、截断
// 远端文件。read_file 的 bytes_len/eof 描述本次实际返回的内容，据此 offset 续读。
const FILE_CHUNK=512*1024; // 每次加载的块大小（read_file 单次上限 1 MiB）
// loadMoreFile 读下一段。返回 true=本段成功，false=失败/已到底。
// 返回值不是可有可无：调用方「加载全部」要靠它决定是否继续，失败时若只看 t.eof
// （失败路径不会置位）就会变成永不退出的紧密循环——断开连接后每次请求立刻失败，
// 循环便退化成对后端的请求洪水，页面还没法停。
async function loadMoreFile(t){
  if(t.loadingMore||t.eof)return false;
  t.loadingMore=true;
  try{
    const r=await api('/api/call',{tool:'read_file',args:{path:t.path,offset:t.offset,length:FILE_CHUNK,as_text:!!t.asText}});
    if(!r.ok){$('#file-content').innerHTML='<span class="err">'+esc(r.error)+'</span>';return false}
    const d=parseMCP(r.result);
    if(d.binary){t.binary=true;t.eof=true;renderFileTab(t);return true}
    if(d.bytes_b64!==undefined){
      if(!t.decoder)t.decoder=makeFileDecoder();
      t.content+=t.decoder(b64bytes(d.bytes_b64),!!d.eof);
    }else t.content+=(d.text||'');
    const n=d.bytes_len!==undefined?d.bytes_len:new TextEncoder().encode(d.text||'').length;
    t.offset+=n;
    if(d.eof||n===0)t.eof=true;
    renderFileTab(t);
    return true;
  }finally{t.loadingMore=false}
}
async function showFile(path,entry){
  const id='file:'+path;
  openTab(id,path.split(/[\\/]/).pop()||path,'file-box',async t=>{
    currentFilePath=path;
    if(t.loaded){t.image?renderImagePreview(t):renderFileTab(t);return} // 切回已打开的文件：用缓存，不重读、不丢未保存的编辑
    t.loaded=true;t.path=path;t.image=isPreviewableImage(path);t.offset=0;t.content='';t.eof=false;t.binary=false;t.asText=false;t.decoder=null;t.editing=false;
    $('#fpath').textContent=path;
    resetFileView();
    if(t.image){renderImagePreview(t);return}
    $('#file-content').textContent='读取中...';
    await loadMoreFile(t);
  });
}
function isPreviewableImage(path){return /\.(png|jpe?g|gif|webp|bmp)$/i.test(String(path))}
function imageFitScale(t){
  const c=t.imageContainer,img=t.imageElement;if(!c||!img||!img.naturalWidth)return 1;
  return Math.min(1,Math.max(.05,Math.min((c.clientWidth-24)/img.naturalWidth,(c.clientHeight-24)/img.naturalHeight)));
}
function applyImageScale(t,next,anchor){
  const c=t.imageContainer,stage=t.imageStage,img=t.imageElement;if(!c||!stage||!img||!img.naturalWidth)return;
  const oldScale=t.imageScale||1,viewX=anchor?anchor.clientX-c.getBoundingClientRect().left:c.clientWidth/2,viewY=anchor?anchor.clientY-c.getBoundingClientRect().top:c.clientHeight/2;
  const oldW=img.naturalWidth*oldScale,oldH=img.naturalHeight*oldScale,oldStageW=Math.max(c.clientWidth,oldW+24),oldStageH=Math.max(c.clientHeight,oldH+24);
  const imageX=(c.scrollLeft+viewX-(oldStageW-oldW)/2)/oldScale,imageY=(c.scrollTop+viewY-(oldStageH-oldH)/2)/oldScale;
  const scale=Math.max(.05,Math.min(8,next)),width=img.naturalWidth*scale,height=img.naturalHeight*scale,stageW=Math.max(c.clientWidth,width+24),stageH=Math.max(c.clientHeight,height+24);
  t.imageScale=scale;stage.style.width=stageW+'px';stage.style.height=stageH+'px';img.style.width=width+'px';img.style.height=height+'px';img.style.left=(stageW-width)/2+'px';img.style.top=(stageH-height)/2+'px';
  c.scrollLeft=(stageW-width)/2+imageX*scale-viewX;c.scrollTop=(stageH-height)/2+imageY*scale-viewY;
  if(activeTab===t.id)$('#finfo').textContent='图片 '+img.naturalWidth+' x '+img.naturalHeight+' · '+Math.round(scale*100)+'%';
}
function downloadPreview(path,objectURL){
  const a=document.createElement('a');a.href=objectURL||withToken('/api/preview?download=1&path='+encodeURIComponent(path));a.download=path.split(/[\\/]/).pop()||'image';document.body.appendChild(a);a.click();a.remove();
}
async function renderImagePreview(t){
  resetFileView();
  $('#fpath').textContent=t.path;$('#finfo').textContent='图片';
  const c=$('#file-content');c.innerHTML='';c.classList.add('image-preview');
  const stage=document.createElement('div');stage.className='image-stage';c.appendChild(stage);
  const img=document.createElement('img');img.alt=t.path.split(/[\\/]/).pop()||'图片预览';
  t.imageContainer=c;t.imageStage=stage;t.imageElement=img;
  for(const id of ['fZoomOut','fZoomFit','fZoomIn','fDownload'])$('#'+id).style.display='inline-flex';
  $('#fDownload').disabled=true;
  $('#fZoomOut').onclick=()=>applyImageScale(t,(t.imageScale||1)/1.2);
  $('#fZoomIn').onclick=()=>applyImageScale(t,(t.imageScale||1)*1.2);
  $('#fZoomFit').onclick=()=>applyImageScale(t,imageFitScale(t));
  $('#fDownload').onclick=()=>downloadPreview(t.path,t.imageObjectURL);
  c.onwheel=e=>{if(!img.naturalWidth)return;e.preventDefault();applyImageScale(t,(t.imageScale||1)*(e.deltaY<0?1.15:1/1.15),e)};
  img.onload=()=>{if(!img.naturalWidth||activeTab!==t.id)return;if(!t.imageScale)t.imageScale=imageFitScale(t);applyImageScale(t,t.imageScale)};
  img.onerror=()=>{
    if(activeTab!==t.id)return;
    c.classList.remove('image-preview');c.textContent='图片预览失败。';
    const b=document.createElement('button');b.className='sm';b.textContent='另存为';b.onclick=()=>downloadPreview(t.path,t.imageObjectURL);c.appendChild(b);
  };
  stage.appendChild(img);
  if(t.imageRequest)t.imageRequest.abort();const request=new AbortController();t.imageRequest=request;
  try{
    const response=await fetch(withToken('/api/preview?path='+encodeURIComponent(t.path)),{signal:request.signal});
    if(!response.ok)throw new Error((await response.text())||('HTTP '+response.status));
    const blob=await response.blob();if(request.signal.aborted)return;
    if(t.imageObjectURL)URL.revokeObjectURL(t.imageObjectURL);t.imageObjectURL=URL.createObjectURL(blob);img.src=t.imageObjectURL;$('#fDownload').disabled=false;
  }catch(e){if(request.signal.aborted)return;img.onerror();if(activeTab===t.id)c.insertAdjacentHTML('afterbegin','<span class="err">'+esc(String(e))+'</span>')}
}
function resetFileView(){
  $('#file-content').style.display='';$('#file-content').classList.remove('image-preview');$('#file-content').onwheel=null;$('#file-editor').style.display='none';
  for(const id of ['fZoomOut','fZoomFit','fZoomIn','fDownload'])$('#'+id).style.display='none';
  $('#fEdit').style.display='none';$('#fSave').style.display='none';$('#fCancel').style.display='none';
}
function updateFileInfo(t){
  $('#finfo').textContent=t.binary?'二进制':((t.asText?'文本试读 ': '')+formatSize(t.offset)+(t.eof?'':' / 未完'));
}
function renderFileTab(t){
  resetFileView();
  updateFileInfo(t);
  if(t.binary){
    const c=$('#file-content');c.innerHTML='<span class="meta">二进制文件，不支持在线查看/编辑。右键树节点可下载到本地。</span> ';
    const b=document.createElement('button');b.className='sm';b.textContent='按文本查看';b.onclick=()=>viewFileAsText(t);c.appendChild(b);return;
  }
  if(t.editing){startEdit(t);return}
  const c=$('#file-content');
  c.textContent=t.content;
  if(!t.eof){
    // 底部“加载更多”条，类似 GitHub review 的折叠展开：按需续读，不一次拉爆
    const bar=document.createElement('div');
    bar.style.cssText='padding:8px 0;margin-top:6px;border-top:1px dashed #45475a;display:flex;gap:8px;align-items:center';
    const info=document.createElement('span');info.className='meta';info.textContent='已加载 '+formatSize(t.offset)+'，还有更多';bar.appendChild(info);
    const more=document.createElement('button');more.className='sm';more.textContent='加载下一段';
    more.onclick=()=>{more.disabled=true;more.textContent='加载中...';loadMoreFile(t)};bar.appendChild(more);
    const all=document.createElement('button');all.className='sm';all.textContent='加载全部';
    // 靠 loadMoreFile 的返回值退出，不能只看 t.eof：读失败时 eof 不会置位，
    // 只看 eof 会变成不停重试的请求洪水（断开连接时必现）。
    all.onclick=async()=>{all.disabled=true;more.disabled=true;while(!t.eof){if(!await loadMoreFile(t))break}};bar.appendChild(all);
    c.appendChild(bar);
  }else if(!t.asText){
    $('#fEdit').style.display='inline-block';$('#fEdit').onclick=()=>{t.editing=true;startEdit(t)};
  }
}
async function viewFileAsText(t){
  if(t.loadingMore)return;
  t.asText=true;t.binary=false;t.offset=0;t.content='';t.eof=false;t.decoder=makeFileDecoder();
  $('#file-content').textContent='按文本读取中...';
  await loadMoreFile(t);
}
function startEdit(t){
  $('#file-content').style.display='none';$('#file-editor').style.display='flex';
  $('#fEditor').value=t.draft!==undefined?t.draft:t.content;
  $('#fEditor').oninput=()=>{t.draft=$('#fEditor').value};
  $('#fEdit').style.display='none';$('#fSave').style.display='inline-block';$('#fCancel').style.display='inline-block';
  $('#fEditor').focus();
}
$('#fCancel').onclick=()=>{const t=tabs.find(x=>x.id===activeTab);if(!t)return;t.editing=false;t.draft=undefined;renderFileTab(t)};
$('#fSave').onclick=async()=>{
  const t=tabs.find(x=>x.id===activeTab);if(!t)return;
  const content=$('#fEditor').value;
  const r=await api('/api/call',{tool:'write_file',args:{path:currentFilePath,content:b64FromText(content),create:true}});
  if(r.ok){t.content=content;t.draft=undefined;termLog('<span class="ok">已保存: '+esc(currentFilePath)+'</span>')}
  else termLog('<span class="err">保存失败: '+esc(r.error)+'</span>');
};

// ========== 目录树 + 右键菜单 ==========
const treeCache={};
let driveLoadGeneration=0;
// joinRemote 按**远端**的分隔符拼路径。不能写死反斜杠：Linux 上 '\' 是合法文件名字符，
// 远端不会报错也不会纠正——/etc + '\' + hosts 得到 "/etc\hosts"（一个不存在的文件），
// 上传更阴，/tmp + '\' + a.txt 会在根目录建出一个名为 "tmp\a.txt" 的文件，静默传错地方。
function joinRemote(dir,name){
  const base=dir.replace(/[\\\/]$/,'');
  return base+(remoteOS==='windows'?'\\':'/')+name;
}
async function fetchDirResult(path){
  const k=path.replace(/[\\\/]$/,'')||path;if(Object.prototype.hasOwnProperty.call(treeCache,k))return{ok:true,entries:treeCache[k]};
  const r=await api('/api/call',{tool:'list_dir',args:{path,max_entries:200}});if(!r.ok)return{ok:false,entries:[],error:r.error||'list_dir failed'};
  try{const d=parseMCP(r.result);if(d&&Array.isArray(d.entries)){treeCache[k]=d.entries;return{ok:true,entries:d.entries}}}catch{}
  return{ok:false,entries:[],error:'invalid list_dir response'};
}
async function fetchDir(path){return(await fetchDirResult(path)).entries}
function unixStartPathFromProbe(stdout){
  const lines=String(stdout||'').replace(/\r/g,'').split('\n'),cwd=lines[0]&&lines[0][0]==='/'?lines[0]:'/';
  if(lines[1])try{
    const raw=Uint8Array.from(atob(lines[1].trim()),c=>c.charCodeAt(0));
    const argv=new TextDecoder('utf-8').decode(raw).replace(/\0+$/,'').split('\0');let root='';
    for(let i=1;i<argv.length;i++){const a=argv[i];if(a==='--root'&&i+1<argv.length)root=argv[++i];else if(a.startsWith('--root='))root=a.slice(7)}
    if(root)return root[0]==='/'?root:cwd.replace(/\/$/,'')+'/'+root;
  }catch{}
  return '/';
}
async function probeUnixStartPath(){
  const script="pwd; if command -v base64 >/dev/null 2>&1 && [ -r \"/proc/$PPID/cmdline\" ]; then base64 \"/proc/$PPID/cmdline\" | tr -d '\\n'; fi; printf '\\n'";
  const r=await api('/api/call',{tool:'exec',args:{argv:['sh','-c',script],timeout_ms:8000}});
  if(!r.ok)return'/';const d=parseMCP(r.result);return unixStartPathFromProbe(d.stdout||'');
}
async function loadDrives(){
  if(!connected)return;const generation=++driveLoadGeneration,tree=$('#tree');tree.innerHTML='<div class="tloading">加载中...</div>';
  const r=await api('/api/call',{tool:'exec',args:{argv:['powershell','-c','Get-PSDrive -PSProvider FileSystem | Select -ExpandProperty Name']}});
  if(generation!==driveLoadGeneration)return;tree.innerHTML='';
  if(r.ok){const d=parseMCP(r.result);const lines=(d.stdout||'').split(/\r?\n/).map(s=>s.trim()).filter(s=>s&&/^[A-Z]$/i.test(s));
    if(lines.length>0){remoteOS='windows';for(const l of lines)addDirNode(tree,l.toUpperCase()+':\\',null,false);return}}
  remoteOS='unix';
  const preferred=await probeUnixStartPath(),candidates=[preferred,'/','.'].filter((p,i,a)=>p&&a.indexOf(p)===i);
  let startPath='',lastError='';
  for(const candidate of candidates){const result=await fetchDirResult(candidate);if(result.ok){startPath=candidate;break}lastError=result.error||lastError}
  if(generation!==driveLoadGeneration)return;tree.innerHTML='';
  if(!startPath){tree.innerHTML='<div class="tloading">默认目录不可访问'+(lastError?'：'+esc(lastError):'')+'</div>';return}
  $('#treePath').value=startPath;addDirNode(tree,startPath,true,true);
}
function addDirNode(parent,path,isRoot,isExpanded){
  const name=path.replace(/[\\\/]$/,'');
  const nd=document.createElement('div');nd.className='tdir';nd.dataset.path=path;nd.dataset.kind='dir';
  const arrow=document.createElement('span');arrow.className='arr closed';nd.appendChild(arrow);
  const label=name.split(/[\\/]/).pop()||path;nd.appendChild(document.createTextNode((isRoot?' 💾':' 📁')+' '+label));
  parent.appendChild(nd);
  const ch=document.createElement('div');ch.className='tchildren';ch.style.display='none';parent.appendChild(ch);
  let loaded=false;
  const toggle=async()=>{
    if(ch.style.display==='none'){
      if(!loaded){ch.innerHTML='<div class="tloading">加载...</div>';ch.style.display='';arrow.className='arr open';const entries=await fetchDir(path);ch.innerHTML='';entries.sort((a,b)=>(b.kind==='dir'?1:0)-(a.kind==='dir'?1:0));for(const e of entries){const full=joinRemote(path,e.name);if(e.kind==='dir')addDirNode(ch,full,false,false);else addFileNode(ch,full,e)}loaded=true}
      else{ch.style.display='';arrow.className='arr open'}
    }else{ch.style.display='none';arrow.className='arr closed'}
  };
  nd.onclick=async(ev)=>{ev.stopPropagation();await toggle()};
  nd.oncontextmenu=ev=>{ev.preventDefault();showCtxMenu(ev,path,'dir')};
  if(isExpanded)void toggle();
}
function addFileNode(parent,path,entry){
  const nd=document.createElement('div');nd.className='tfile';nd.dataset.path=path;nd.dataset.kind='file';
  nd.innerHTML='<span class="ico">📄</span>'+esc(entry.name)+'<span class="sz">'+formatSize(entry.size)+'</span>';
  nd.onclick=ev=>{ev.stopPropagation();showFile(path,entry)};
  nd.oncontextmenu=ev=>{ev.preventDefault();showCtxMenu(ev,path,'file')};
  parent.appendChild(nd);
}
$('#treePath').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();const p=$('#treePath').value.trim();if(p){$('#tree').innerHTML='';addDirNode($('#tree'),p,true,false)}}});

// 右键菜单：用闭包传路径，不拼 onclick 字符串。路径一旦进了 JS 字符串字面量，
// Windows 里极常见的 C:\temp / C:\node 就会被解释成 <TAB>emp / <换行>ode；而且
// esc() 产生的 &#39; 在 HTML 属性里会被解码回单引号，反而能闭合字符串。
const ctx=$('#ctxmenu');
function showCtxMenu(ev,path,kind){
  const items=kind==='file'
    ?[['📖 读取',()=>showFile(path,null)],['⬇ 下载到本地',()=>ctxDownload(path)],['📜 Tail 日志',()=>ctxTail(path)]]
    :[['📂 列出内容',()=>ctxListDir(path)],['🔎 在当前目录搜索',()=>openSearch(path)],['⬆ 上传到此目录',()=>ctxUpload(path)]];
  ctx.innerHTML='';
  for(const [label,fn] of items){
    const d=document.createElement('div');d.className='ci';d.textContent=label;
    d.onclick=e=>{e.stopPropagation();ctx.style.display='none';fn()};
    ctx.appendChild(d);
  }
  ctx.style.display='block';ctx.style.left=ev.clientX+'px';ctx.style.top=ev.clientY+'px';
}
document.addEventListener('click',()=>ctx.style.display='none');

async function ctxTail(p){
  openTerm();
  termLog('<span class="cmd">tail '+esc(p)+'</span>');
  const r=await api('/api/call',{tool:'tail_log',args:{path:p,lines:100}});
  if(!r.ok){termLog('<span class="err">'+esc(r.error)+'</span>');return}
  const d=parseMCP(r.result);
  termLog(esc(d.lines||''));
  if(d.lines_truncated)termLog('<span class="meta">(内容过长，已保留最新部分；可减少 lines)</span>');
}
async function ctxListDir(p){
  openTab('dir:'+p,(p.split(/[\\/]/).filter(Boolean).pop()||p)+'/','file-box',async()=>{
    currentFilePath='';
    $('#fpath').textContent=p;$('#finfo').textContent='';
    resetFileView();
    $('#file-content').textContent='加载中...';
    const r=await api('/api/call',{tool:'list_dir',args:{path:p}});
    if(!r.ok){$('#file-content').innerHTML='<span class="err">'+esc(r.error)+'</span>';return}
    const d=parseMCP(r.result);
    if(!Array.isArray(d.entries)){$('#file-content').textContent=JSON.stringify(d,null,2);return}
    let h='<span class="meta">'+d.entries.length+' 条'+(d.truncated?'（已截断，共 '+esc(d.total)+' 条，可调 max_entries）':'')+'</span>\n';
    for(const e of d.entries)h+=(e.kind==='dir'?'📁':'📄')+' '+esc(e.name)+(e.kind==='dir'?'':' <span class="meta">'+formatSize(e.size)+'</span>')+'\n';
    $('#file-content').innerHTML=h;
  });
}
function ctxUpload(p){
  const input=document.createElement('input');input.type='file';
  input.onchange=async()=>{
    const file=input.files[0];if(!file)return;
    const remotePath=joinRemote(p,file.name);
    termLog('<span class="meta">上传中: '+esc(file.name)+' ('+formatSize(file.size)+')</span>');
    // 走 arrayBuffer 而不是 file.text()：后者按 UTF-8 解码，二进制文件必坏。
    const content=b64FromBytes(new Uint8Array(await file.arrayBuffer()));
    const r=await api('/api/call',{tool:'write_file',args:{path:remotePath,content,create:true}});
    if(r.ok){termLog('<span class="ok">上传完成: '+esc(remotePath)+'</span>');delete treeCache[p.replace(/[\\\/]$/,'')]}
    else termLog('<span class="err">上传失败: '+esc(r.error)+'</span>');
  };
  input.click();
}
// 下载走后端 /api/download：read_file 经 humanize 后只剩文本，拿不到二进制原始字节，
// 而后端可以用 download_file（走裸 bridge）拿到完整字节再流给浏览器。
function ctxDownload(p){
  const a=document.createElement('a');
  a.href=withToken('/api/download?path='+encodeURIComponent(p)); // <a> 设不了自定义头，走 query
  a.download=p.split(/[\\/]/).pop()||'download';
  document.body.appendChild(a);a.click();a.remove();
  termLog('<span class="meta">已开始下载: '+esc(p)+'</span>');
}

// ========== parseMCP ==========
function parseMCP(res){if(!res)return{};let d=typeof res==='string'?JSON.parse(res):res;if(d&&d.content&&Array.isArray(d.content)&&d.content[0]&&d.content[0].text!==undefined){try{d=JSON.parse(d.content[0].text)}catch{}}return d||{}}

// ========== 工具函数 ==========
function formatSize(n){if(!n)return'';if(n<1024)return n+'B';if(n<1024*1024)return(n/1024).toFixed(1)+'K';return(n/1024/1024).toFixed(1)+'M'}
function esc(s){return String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
// write_file 的 content 是 base64（Go 端为 []byte）。发明文会被当 base64 解码：多数会
// 报错，但恰好落在 base64 字母表且长度为 4 倍数的短内容会被静默解成垃圾字节写进文件。
// btoa 只吃 latin1，所以必须先用 TextEncoder 转成 UTF-8 字节，中文才不会坏。
function b64FromBytes(bytes){
  let bin='';const CH=0x8000; // 分块，避免 apply 参数过多爆栈
  for(let i=0;i<bytes.length;i+=CH)bin+=String.fromCharCode.apply(null,bytes.subarray(i,i+CH));
  return btoa(bin);
}
function b64FromText(s){return b64FromBytes(new TextEncoder().encode(s))}

// ========== 目录树宽度拖拽 ==========
// 之前这里只有 CSS（col-resize 光标 + 一个没人加过的 .dragging 样式），根本没有 JS，
// 所以分隔条拖不动；而按住横拖时浏览器就按默认行为走——把界面文字一路选成蓝底。
// 用 pointer 事件 + setPointerCapture：捕获后即使指针跑出细条（拖快时必然发生）事件
// 也还在，不用往 document 上挂全局 listener 再记得摘掉。
(function(){
  const rz=$('#resizer'),panel=$('#tree-panel');
  if(!rz||!panel)return;
  let dragging=false;
  rz.addEventListener('pointerdown',e=>{
    dragging=true;
    rz.classList.add('dragging');
    document.body.classList.add('resizing'); // 拖动期间全局禁选
    if(rz.setPointerCapture)rz.setPointerCapture(e.pointerId);
    e.preventDefault(); // 阻止浏览器起手就开始选字
  });
  rz.addEventListener('pointermove',e=>{
    if(!dragging)return;
    const w=e.clientX-panel.getBoundingClientRect().left;
    // 夹住范围：太窄看不到树，太宽把右侧挤没
    panel.style.width=Math.max(120,Math.min(w,window.innerWidth-240))+'px';
  });
  const stop=e=>{
    if(!dragging)return;
    dragging=false;
    rz.classList.remove('dragging');
    document.body.classList.remove('resizing');
    if(rz.releasePointerCapture&&e.pointerId!==undefined){try{rz.releasePointerCapture(e.pointerId)}catch(_){}}
  };
  rz.addEventListener('pointerup',stop);
  rz.addEventListener('pointercancel',stop);
})();

openTerm();
</script>
</body>
</html>`)
