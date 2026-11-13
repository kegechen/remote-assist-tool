package assets

var IndexHTML = []byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Remote Assist GUI</title>
<style>
/* ===== 设计 token：扁平玻璃 + 三套主题（浅色/灰色/深色），由 html[data-theme] 切换 ===== */
:root{
  --ui:"Segoe UI Variable Text","Segoe UI","Microsoft YaHei UI","Microsoft YaHei","PingFang SC","Noto Sans CJK SC",system-ui,sans-serif;
  --mono:"Cascadia Mono","JetBrains Mono",Consolas,"Microsoft YaHei UI","Microsoft YaHei",monospace;
  --blur:blur(18px) saturate(140%);
  --r-lg:16px;--r-md:10px;--r-sm:8px;--gap:12px;
  /* 深色（默认，JS 未执行时的兜底） */
  color-scheme:dark;
  --bg:#101113;
  --glow1:rgba(255,255,255,.055);--glow2:rgba(255,255,255,.035);--glow3:rgba(255,255,255,.03);
  --text:#e6e7ea;--muted:#b4b8bf;--dim:#8d9199;
  --accent:#b9c0cc;--accent-ink:#121316;--accent-rgb:185,192,204;
  --ok:#7fd18b;--ok-rgb:127,209,139;--err:#f0869a;--err-rgb:240,134,154;--warn:#e8c56d;--warn-rgb:232,197,109;--ok-ink:#0b1a10;
  --glass:rgba(34,35,39,.62);--pop:rgba(40,41,46,.88);--glass-solid:#1f2023;--pop-solid:#26272b;
  --fill:rgba(255,255,255,.06);--fill-hover:rgba(255,255,255,.11);--field:rgba(0,0,0,.28);--well:#0b0b0c;
  --line:rgba(255,255,255,.09);--line-hi:rgba(255,255,255,.2);--hl:rgba(255,255,255,.07);
  --shadow:0 10px 32px rgba(0,0,0,.4);
  --brand1:#c9cfda;--brand2:#7e8794;--drv:#b7a5df;--toast-bg:#2b2024;
  --term-bg:#0c0d0f;--term-fg:#e2e4e8;--term-dim:#8d9199;--term-green:#7fd18b;--term-red:#f0869a;--term-yellow:#e8c56d;--term-blue:#b9c0cc;
}
:root[data-theme=gray]{
  color-scheme:dark;
  --bg:#2b2d30;
  --glow1:rgba(255,255,255,.05);--glow2:rgba(255,255,255,.035);--glow3:rgba(255,255,255,.03);
  --text:#e8e9eb;--muted:#bcbfc4;--dim:#a3a7ae;
  --accent:#c9cdd4;--accent-ink:#1f2124;--accent-rgb:201,205,212;
  --ok:#86d493;--ok-rgb:134,212,147;--err:#f28fa0;--err-rgb:242,143,160;--warn:#ebcb7a;--warn-rgb:235,203,122;--ok-ink:#0b1a10;
  --glass:rgba(60,62,66,.66);--pop:rgba(66,68,73,.92);--glass-solid:#3a3c40;--pop-solid:#424449;
  --fill:rgba(255,255,255,.07);--fill-hover:rgba(255,255,255,.12);--field:rgba(0,0,0,.2);--well:#1f2022;
  --line:rgba(255,255,255,.12);--line-hi:rgba(255,255,255,.24);--hl:rgba(255,255,255,.09);
  --shadow:0 10px 28px rgba(0,0,0,.3);
  --brand1:#d3d7de;--brand2:#8b929d;--drv:#c0b0e6;--toast-bg:#40303a;
  --term-bg:#1f2022;--term-fg:#e1e3e6;--term-dim:#a3a7ae;--term-green:#86d493;--term-red:#f28fa0;--term-yellow:#ebcb7a;--term-blue:#c9cdd4;
}
:root[data-theme=light]{
  color-scheme:light;
  --bg:#f3f4f6;
  --glow1:rgba(255,255,255,.95);--glow2:rgba(255,255,255,.6);--glow3:rgba(205,210,220,.3);
  --text:#1d2025;--muted:#484d55;--dim:#656b75;
  --accent:#3f4a5a;--accent-ink:#fff;--accent-rgb:63,74,90;
  --ok:#17803a;--ok-rgb:23,128,58;--err:#c42b47;--err-rgb:196,43,71;--warn:#8a5a00;--warn-rgb:138,90,0;--ok-ink:#fff;
  --glass:rgba(255,255,255,.72);--pop:rgba(255,255,255,.95);--glass-solid:#fbfbfc;--pop-solid:#fff;
  --fill:rgba(20,25,35,.05);--fill-hover:rgba(20,25,35,.09);--field:rgba(255,255,255,.78);--well:#c4c8ce;
  --line:rgba(20,25,35,.12);--line-hi:rgba(20,25,35,.26);--hl:rgba(255,255,255,.9);
  --shadow:0 10px 28px rgba(30,35,45,.12);
  --brand1:#4b5668;--brand2:#8a93a3;--drv:#6b4fb0;--toast-bg:#fff0f2;
  --term-bg:#fbfbfc;--term-fg:#1d2025;--term-dim:#656b75;--term-green:#17803a;--term-red:#c42b47;--term-yellow:#8a5a00;--term-blue:#3f4a5a;
}
:root{--accent-soft:rgba(var(--accent-rgb),.14);--accent-line:rgba(var(--accent-rgb),.38)}
/* 不支持 backdrop-filter 时用不透明底色兜底，保证可读 */
@supports not ((-webkit-backdrop-filter:blur(1px)) or (backdrop-filter:blur(1px))){
  :root:root:root{--glass:var(--glass-solid);--pop:var(--pop-solid)}
}
/* 切换主题时短暂打开过渡，平滑变色（由 JS 加类、150ms 后移除） */
html.theme-anim *,html.theme-anim *::before,html.theme-anim *::after{transition:background-color .15s ease,color .15s ease,border-color .15s ease,box-shadow .15s ease,fill .15s ease,stroke .15s ease!important}
*{box-sizing:border-box;margin:0;padding:0}
*{scrollbar-width:thin;scrollbar-color:var(--line-hi) transparent}
::-webkit-scrollbar{width:10px;height:10px}
::-webkit-scrollbar-track{background:transparent}
::-webkit-scrollbar-thumb{background:var(--line-hi);border-radius:5px;border:2px solid transparent;background-clip:content-box}
::-webkit-scrollbar-thumb:hover{background:var(--dim);background-clip:content-box}
::-webkit-scrollbar-corner{background:transparent}
/* 拖动分隔条时全局禁选：否则横向拖拽会把界面文字一路选中 */
body.resizing{user-select:none;cursor:col-resize}
html,body{height:100%;font-family:var(--ui);font-size:13px;color:var(--text);background-color:var(--bg);
  background-image:radial-gradient(900px 600px at 8% -8%,var(--glow1),transparent 60%),radial-gradient(800px 560px at 100% 0%,var(--glow2),transparent 60%),radial-gradient(900px 600px at 70% 112%,var(--glow3),transparent 60%);
  background-attachment:fixed}
#app{display:flex;flex-direction:column;height:100%;padding:var(--gap);gap:var(--gap);min-width:0}
/* 玻璃面板：半透明底 + 细描边 + 顶部高光 + 磨砂 */
header,#tree-panel,#conn-info,#ctxmenu,#upgrade-notice{background:var(--glass);border:1px solid var(--line);box-shadow:var(--shadow),inset 0 1px 0 var(--hl)}
header,#tree-panel,#conn-info,#ctxmenu{-webkit-backdrop-filter:var(--blur);backdrop-filter:var(--blur)}
header{position:relative;z-index:5;padding:9px 12px;border-radius:var(--r-lg);display:flex;flex-wrap:wrap;gap:8px 10px;align-items:center}
.brand{display:flex;align-items:center;gap:8px;font-weight:600;letter-spacing:.01em;margin-right:4px}
.brand svg{width:26px;height:26px;flex:none}
.hgrp{display:flex;align-items:center;gap:7px}
.lbl{color:var(--dim);font-size:12px;user-select:none;white-space:nowrap}
.hacts{gap:6px}
input,select,textarea{font-family:inherit}
header input.server,#treePath,#grepPattern,#grepRoot{background:var(--field);border:1px solid var(--line);color:var(--text);border-radius:var(--r-md);height:32px;padding:0 11px;font-size:13px;outline:0;transition:border-color .15s,box-shadow .15s}
header input.server{width:170px}
header input.server:focus,#treePath:focus,#grepPattern:focus,#grepRoot:focus{border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-soft)}
::placeholder{color:var(--dim);opacity:1}
.codebox{display:inline-flex;align-items:center;gap:3px;background:var(--field);border:1px solid var(--line);border-radius:var(--r-md);padding:0 10px;height:32px;transition:border-color .15s,box-shadow .15s,opacity .15s}
.codebox:focus-within{border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-soft)}
.codebox i{color:var(--dim);font-style:normal;user-select:none}
header .codebox input.cseg{width:5.2ch;background:transparent;border:0;outline:0;padding:0;text-align:center;color:var(--text);font-family:var(--mono);font-size:13px;letter-spacing:.08em}
header .codebox input.cseg::placeholder{color:var(--dim);letter-spacing:.08em}
.codebox.off,.codebox.off:focus-within{opacity:.4;border-color:var(--line);box-shadow:none}
.codebox.off input.cseg{cursor:not-allowed}
/* 复选框做成小开关 */
.chk{display:inline-flex;align-items:center;gap:6px;color:var(--muted);font-size:12px;cursor:pointer;user-select:none;white-space:nowrap}
.chk input{appearance:none;-webkit-appearance:none;position:relative;width:26px;height:15px;border-radius:8px;background:var(--fill-hover);border:1px solid var(--line);margin:0;cursor:pointer;flex:none;transition:background .15s}
.chk input::after{content:"";position:absolute;left:2px;top:2px;width:9px;height:9px;border-radius:50%;background:var(--muted);transition:transform .15s,background .15s}
.chk input:checked{background:var(--accent);border-color:transparent}
.chk input:checked::after{transform:translateX(11px);background:var(--accent-ink)}
.chk input:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
/* 按钮：主按钮为实色强调色；.sm 为幽灵玻璃按钮；.sec 为危险态 */
button{font-family:inherit;background:var(--accent);color:var(--accent-ink);border:1px solid transparent;height:32px;padding:0 16px;border-radius:var(--r-md);cursor:pointer;font-weight:600;font-size:13px;transition:filter .15s,background .15s,border-color .15s,transform .1s}
button:hover{filter:brightness(1.08)}
button:active{transform:translateY(1px)}
button:disabled{opacity:.6;cursor:default}
button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
button.sec{background:rgba(var(--err-rgb),.16);color:var(--err);border-color:rgba(var(--err-rgb),.4)}
button.sm{background:var(--fill);color:var(--text);border-color:var(--line);height:28px;padding:0 11px;font-weight:500;font-size:12px}
button.sm:hover{background:var(--fill-hover);border-color:var(--line-hi);filter:none}
button.ghost{height:32px;padding:0 13px}
#btnDesk{color:var(--accent);border-color:var(--accent-line);background:var(--accent-soft)}
#themeBtn{width:30px;height:30px;padding:0;display:inline-flex;align-items:center;justify-content:center;background:var(--fill);color:var(--text);border-color:var(--line)}
#themeBtn:hover{background:var(--fill-hover);border-color:var(--line-hi);filter:none}
#themeBtn svg{width:16px;height:16px;fill:none;stroke:currentColor;stroke-width:1.6;stroke-linecap:round;stroke-linejoin:round}
#themeMenu{display:none;position:fixed;z-index:1001;min-width:140px;padding:5px;border-radius:12px;background:var(--pop);border:1px solid var(--line);box-shadow:var(--shadow),inset 0 1px 0 var(--hl);-webkit-backdrop-filter:var(--blur);backdrop-filter:var(--blur)}
#themeMenu .ti{display:flex;align-items:center;gap:8px;height:30px;padding:0 12px;border-radius:8px;font-size:12.5px;color:var(--text);cursor:pointer;white-space:nowrap;user-select:none}
#themeMenu .ti:hover,#themeMenu .ti:focus-visible{background:var(--fill-hover);outline:0}
#themeMenu .ti .ck{width:12px;color:var(--accent);font-weight:700;visibility:hidden}
#themeMenu .ti[aria-checked=true]{font-weight:600}
#themeMenu .ti[aria-checked=true] .ck{visibility:visible}
#fSave{background:var(--ok);color:var(--ok-ink);border-color:transparent}
#compat{margin-left:auto;display:inline-flex;align-items:center;gap:6px;color:var(--warn);font-size:12px;font-weight:500;white-space:nowrap}
#compat[hidden]{display:none}
#compat:not([hidden])+#status{margin-left:0}
#status{margin-left:auto;display:inline-flex;align-items:center;gap:7px;height:28px;padding:0 13px;border-radius:14px;background:var(--fill);border:1px solid var(--line);color:var(--muted);font-size:12px;font-weight:500}
#status::before{content:"";width:7px;height:7px;border-radius:50%;background:var(--dim);flex:none}
#status.ok{background:rgba(var(--ok-rgb),.14);border-color:rgba(var(--ok-rgb),.38);color:var(--ok)}
#status.ok::before{background:var(--ok);box-shadow:0 0 0 3px rgba(var(--ok-rgb),.2)}
#status.bad{background:rgba(var(--err-rgb),.14);border-color:rgba(var(--err-rgb),.4);color:var(--err)}
#status.bad::before{background:var(--err)}
#status:disabled{cursor:default;opacity:1}
#status:not(:disabled){cursor:pointer}
#status:not(:disabled):hover{filter:brightness(1.15)}
#conn-info{display:none;position:fixed;right:var(--gap);top:62px;width:min(340px,calc(100vw - 24px));z-index:1000;background:var(--pop);border-radius:14px;padding:10px 14px}
#conn-info .conn-title{display:flex;align-items:center;justify-content:space-between;padding-bottom:8px;margin-bottom:4px;border-bottom:1px solid var(--line);font-weight:600}
#conn-info .conn-row{display:grid;grid-template-columns:78px minmax(0,1fr);gap:8px;padding:4px 0;font-size:12px}
#conn-info .conn-label{color:var(--dim)}
#conn-info .conn-value{color:var(--text);overflow-wrap:anywhere;text-align:right}
#conn-info .transport-p2p{color:var(--ok)}
#upgrade-notice{display:none;min-height:36px;padding:6px 14px;border-radius:var(--r-md);background:rgba(var(--warn-rgb),.12);border-color:rgba(var(--warn-rgb),.38);color:var(--warn);align-items:center;gap:10px}
#upgrade-notice .msg{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
#upgrade-notice.success{background:rgba(var(--ok-rgb),.12);border-color:rgba(var(--ok-rgb),.38);color:var(--ok)}
main{flex:1;display:flex;min-height:0;min-width:0}
#tree-panel{width:260px;min-width:120px;display:flex;flex-direction:column;flex-shrink:0;border-radius:var(--r-lg);overflow:hidden}
/* 分隔条：12px 透明热区，中间一条细线，悬停/拖动时变强调色 */
#resizer{width:var(--gap);flex-shrink:0;cursor:col-resize;position:relative;user-select:none;touch-action:none}
#resizer::after{content:"";position:absolute;left:50%;top:50%;width:3px;height:36px;margin:-18px 0 0 -1.5px;border-radius:2px;background:var(--line-hi);transition:background .15s,height .15s,margin .15s}
#resizer:hover::after,#resizer.dragging::after{background:var(--accent);height:56px;margin-top:-28px}
#tree-hdr{padding:10px 12px;border-bottom:1px solid var(--line);font-size:12px;color:var(--dim);display:flex;align-items:center;gap:8px}
#tree-hdr span{font-weight:600;color:var(--muted);letter-spacing:.04em}
#treePath{flex:1;min-width:0;height:28px;font-size:12px;border-radius:8px}
#tree{flex:1;overflow:auto;padding:8px}
.tdir,.tfile{padding:0 8px;height:28px;cursor:pointer;white-space:nowrap;border-radius:8px;font-size:12.5px;display:flex;align-items:center;gap:6px;transition:background .12s}
.tdir{user-select:none}
.tdir:hover,.tfile:hover{background:var(--fill-hover)}
.tdir .arr{display:inline-block;width:12px;text-align:center;color:var(--dim);font-size:10px;flex-shrink:0}
.tdir .arr.open::after{content:"\25BE"}
.tdir .arr.closed::after{content:"\25B8"}
.ico{width:16px;height:16px;flex:none;display:inline-flex;align-items:center;justify-content:center}
.ico svg{width:16px;height:16px;fill:none;stroke:currentColor;stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round}
.ico.dir{color:var(--accent)}.ico.drv{color:var(--drv)}.ico.fil{color:var(--dim)}
.tfile{padding-left:26px}
.tfile .sz{color:var(--dim);font-size:11px;margin-left:auto;padding-left:8px}
.tchildren{margin-left:14px;padding-left:4px;border-left:1px solid var(--line)}
.tloading{color:var(--dim);font-size:12px;padding:4px 8px}
/* 右侧主卡片：内含 canvas，所以这里刻意不加 backdrop-filter，避免远程桌面每帧触发大面积模糊重绘 */
#right{flex:1;display:flex;flex-direction:column;min-width:0;border-radius:var(--r-lg);overflow:hidden;background:var(--glass);border:1px solid var(--line);box-shadow:var(--shadow),inset 0 1px 0 var(--hl)}
.tabs{display:flex;gap:4px;padding:8px 10px 0;border-bottom:1px solid var(--line);overflow-x:auto;min-height:42px;align-items:flex-end}
.tabs .tab{padding:0 14px;height:33px;font-size:12.5px;color:var(--muted);cursor:pointer;border-radius:10px 10px 0 0;display:flex;align-items:center;gap:6px;white-space:nowrap;user-select:none;position:relative;transition:background .12s,color .12s}
.tabs .tab:hover{background:var(--fill);color:var(--text)}
.tabs .tab.active{color:var(--text);background:var(--fill-hover);font-weight:600}
.tabs .tab.active::after{content:"";position:absolute;left:12px;right:12px;bottom:0;height:2px;border-radius:2px;background:var(--accent)}
.tabs .tab .close{font-size:14px;line-height:1;color:var(--dim);margin-left:2px;width:18px;height:18px;border-radius:50%;display:inline-flex;align-items:center;justify-content:center;cursor:pointer}
.tabs .tab .close:hover{color:var(--err);background:rgba(var(--err-rgb),.15)}
.tabcontent{flex:1;display:flex;flex-direction:column;min-height:0;overflow:hidden}
/* 各面板与其输出区都要 min-height:0：flex 子项默认 min-height:auto，会把自己撑高而不是滚动 */
#term-box{display:none;flex-direction:column;flex:1;min-height:0}
#term-box .input-row{display:flex;align-items:baseline;gap:6px;margin-top:2px}
#term-box .prompt{color:var(--term-green);font-weight:bold;user-select:none;font-size:12.5px;line-height:1.5}
#termInput{flex:1;min-width:0;background:transparent;border:none;color:var(--term-fg);font-family:var(--mono);font-size:12.5px;line-height:1.5;height:auto;padding:0;outline:none;caret-color:var(--term-green)}
#term-output,#search-output,#proc-output,#file-content{font-family:var(--mono);font-size:12.5px;line-height:1.5}
#term-output{flex:1;min-height:0;overflow:auto;padding:14px 18px;background:var(--term-bg);color:var(--term-fg);cursor:text}
#term-output .err{color:var(--term-red)}#term-output .meta{color:var(--term-dim)}#term-output .ok,#term-output .cmd{color:var(--term-green)}
#term-log{white-space:pre-wrap;word-break:break-all}
#term-status{flex:none;display:flex;align-items:center;gap:10px;padding:4px 18px;font-size:12px;border-top:1px solid var(--line);color:var(--muted)}
#term-status .spacer{flex:1}
#termStop{display:none;height:22px;padding:0 10px;font-size:12px}
#search-box{display:none;flex-direction:column;flex:1;min-height:0}
#search-box .sp{padding:10px 14px;border-bottom:1px solid var(--line);display:flex;flex-wrap:wrap;gap:8px;align-items:center}
#search-box .sp label{color:var(--dim);font-size:12px}
#grepPattern,#grepRoot{flex:1;min-width:120px;height:30px;font-size:12.5px;font-family:var(--mono)}
#search-output{flex:1;min-height:0;overflow:auto;padding:12px 18px;background:var(--field);white-space:pre-wrap}
#file-box{display:none;flex-direction:column;flex:1;min-height:0}
#file-hdr{padding:8px 14px;border-bottom:1px solid var(--line);display:flex;align-items:center;gap:8px;font-size:12px;min-height:46px}
#file-hdr .path{color:var(--accent);flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-family:var(--mono)}
#file-hdr .info{color:var(--dim);white-space:nowrap}
#file-hdr .image-tool{display:none;width:30px;padding:0;align-items:center;justify-content:center;font-size:13px}
#file-hdr #fZoomFit{width:44px}
#file-content{flex:1;min-height:0;overflow:auto;padding:12px 18px;white-space:pre-wrap;word-break:break-all;background:var(--field);line-height:1.55}
#file-content.image-preview{padding:0;display:block;position:relative;white-space:normal;background:var(--well);overscroll-behavior:contain}
#file-content.image-preview .image-stage{position:relative;min-width:100%;min-height:100%}
#file-content.image-preview img{display:block;position:absolute;max-width:none;max-height:none;object-fit:contain;user-select:none;-webkit-user-drag:none}
#file-editor{display:none;flex:1;min-height:0}
#file-editor textarea{width:100%;height:100%;background:var(--field);border:none;color:var(--text);font-family:var(--mono);font-size:12.5px;padding:12px 18px;resize:none;outline:none;line-height:1.55}
#proc-box{display:none;flex-direction:column;flex:1;min-height:0}
#proc-output{flex:1;min-height:0;overflow:auto;padding:12px 18px;background:var(--field);white-space:pre-wrap}
.err{color:var(--err)}.meta{color:var(--muted)}.ok{color:var(--ok)}.cmd{color:var(--ok)}
/* 右键菜单 */
#ctxmenu{display:none;position:fixed;background:var(--pop);border-radius:12px;padding:5px;z-index:999;min-width:150px}
#ctxmenu .ci{padding:0 12px;height:30px;display:flex;align-items:center;font-size:12.5px;cursor:pointer;color:var(--text);white-space:nowrap;border-radius:8px}
#ctxmenu .ci:hover{background:var(--fill-hover)}
#ctxmenu .ci.danger{color:var(--err)}
/* ===== 远程桌面页 ===== */
#desk-box{display:none;flex-direction:column;flex:1;min-height:0}
#desk-bar{display:flex;gap:8px 14px;align-items:center;padding:9px 14px;border-bottom:1px solid var(--line);font-size:12px;color:var(--muted);flex-wrap:wrap}
#desk-bar .dk-grp{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
#desk-bar .dk-acts{margin-left:auto}
#desk-bar .dk-sep{width:1px;height:16px;background:var(--line-hi)}
#desk-bar .dk-lbl{color:var(--dim);font-size:12px;user-select:none}
.dk-num{display:inline-block;text-align:right;font-variant-numeric:tabular-nums}
.dk-pill{font-variant-numeric:tabular-nums;display:inline-flex;align-items:center;gap:7px;height:26px;padding:0 12px;border-radius:13px;background:var(--fill);border:1px solid var(--line);color:var(--muted);font-size:12px;white-space:nowrap;cursor:default}
.dk-dot{width:7px;height:7px;border-radius:50%;background:var(--dim);flex:none}
.dk-pill[data-s=loading] .dk-dot{background:var(--warn);animation:dkPulse 1.2s ease-in-out infinite}
.dk-pill[data-s=live] .dk-dot{background:var(--ok);box-shadow:0 0 0 3px rgba(var(--ok-rgb),.2)}
.dk-pill[data-s=live]{color:var(--ok)}
.dk-pill[data-s=error] .dk-dot{background:var(--err)}
.dk-pill[data-s=error]{color:var(--err)}
@keyframes dkPulse{0%,100%{opacity:1}50%{opacity:.35}}
.dk-seg{display:inline-flex;background:var(--field);border:1px solid var(--line);border-radius:var(--r-md);padding:2px;gap:2px}
#desk-bar .dk-seg button{background:transparent;color:var(--muted);border:0;border-radius:8px;height:24px;min-width:30px;padding:0 10px;font-size:12px;font-weight:500;transition:background .15s,color .15s}
#desk-bar .dk-seg button:hover{background:var(--fill-hover);color:var(--text);filter:none}
#desk-bar .dk-seg button[aria-pressed=true]{background:var(--accent-soft);color:var(--accent);font-weight:600;box-shadow:inset 0 0 0 1px var(--accent-line)}
#desk-bar .dk-btn{display:inline-flex;align-items:center;gap:6px;height:30px;padding:0 12px;background:var(--fill);color:var(--text);border:1px solid var(--line);border-radius:var(--r-md);font-size:12px;font-weight:500}
#desk-bar .dk-btn:hover{background:var(--fill-hover);border-color:var(--line-hi);filter:none}
#desk-bar .dk-btn[aria-pressed=true]{color:var(--accent);border-color:var(--accent-line);background:var(--accent-soft)}
#desk-bar .dk-btn svg,.dk-ov button svg{width:14px;height:14px;flex:none;fill:none;stroke:currentColor;stroke-width:1.6;stroke-linecap:round;stroke-linejoin:round}
#desk-bar select{appearance:none;-webkit-appearance:none;background:var(--field) url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='6'%3E%3Cpath d='M1 1l4 4 4-4' fill='none' stroke='%238a9099' stroke-width='1.4' stroke-linecap='round' stroke-linejoin='round'/%3E%3C/svg%3E") no-repeat right 10px center;color:var(--text);border:1px solid var(--line);border-radius:var(--r-md);height:30px;padding:0 28px 0 11px;font-size:12px;cursor:pointer;transition:border-color .15s}
#desk-bar select:hover{border-color:var(--line-hi)}
#desk-bar .dk-hide{display:none}
#desk-bar button:focus-visible,#desk-bar select:focus-visible,#deskCtl:focus-visible,.dk-ov button:focus-visible{outline:2px solid var(--accent);outline-offset:1px}
.dk-sw{display:inline-flex;align-items:center;gap:8px;cursor:pointer;color:var(--muted);user-select:none}
#deskCtl{appearance:none;-webkit-appearance:none;position:relative;width:30px;height:17px;border-radius:9px;background:var(--fill-hover);border:1px solid var(--line);margin:0;cursor:pointer;transition:background .15s;flex:none}
#deskCtl::after{content:"";position:absolute;left:2px;top:2px;width:11px;height:11px;border-radius:50%;background:var(--muted);transition:transform .15s,background .15s}
#deskCtl:checked{background:var(--accent);border-color:transparent}
#deskCtl:checked::after{transform:translateX(13px);background:var(--accent-ink)}
/* 画面区：纯色底，不叠任何 blur，保证 canvas 刷新不被拖慢 */
#desk-view{position:relative;flex:1;min-height:0;overflow:auto;background:var(--well);display:flex;padding:16px}
#deskCv{display:block;margin:auto;flex:none;border-radius:10px;background:#000;box-shadow:0 0 0 1px var(--line-hi),0 12px 32px rgba(0,0,0,.45);cursor:default;transition:box-shadow .15s}
#desk-view.ro #deskCv{cursor:default}
#desk-view:focus-within #deskCv{box-shadow:0 0 0 2px var(--accent),0 0 22px rgba(var(--accent-rgb),.28),0 12px 32px rgba(0,0,0,.45)}
#desk-view.ro:focus-within #deskCv{box-shadow:0 0 0 2px var(--dim),0 12px 32px rgba(0,0,0,.45)}
#desk-view:not([data-s=live]) #deskCv{opacity:0;pointer-events:none}
#desk-view[data-s=loading] #deskCv{opacity:.3}
#deskKb{position:absolute;left:0;top:0;width:1px;height:1px;opacity:0;border:0;padding:0;resize:none}
.dk-chip{position:absolute;display:inline-flex;align-items:center;gap:6px;height:22px;padding:0 10px;border-radius:11px;background:rgba(18,19,22,.78);border:1px solid rgba(255,255,255,.12);color:#d3d6dc;font-size:11px;pointer-events:none;white-space:nowrap;z-index:1}
#deskHint{left:24px;top:24px}
#deskHint span{display:none;align-items:center;gap:6px}
#desk-view .h-off{display:inline-flex}
#desk-view:focus-within .h-off{display:none}
#desk-view:focus-within .h-on{display:inline-flex;color:#a6e3a1}
#desk-view.ro .h-off,#desk-view.ro .h-on{display:none}
#desk-view.ro .h-ro{display:inline-flex;color:#f0d58a}
#desk-view:not([data-s=live]) #deskHint{display:none}
.dk-stat{display:inline-flex;align-items:center;gap:4px;font-size:12px;color:var(--dim);font-variant-numeric:tabular-nums;white-space:nowrap;padding:0 4px}
.dk-stat:empty{display:none}
.dk-ov{position:absolute;left:0;top:0;right:0;bottom:0;display:none;flex-direction:column;align-items:center;justify-content:center;gap:12px;padding:24px;text-align:center;z-index:2;color:var(--text)}
#desk-view:not([data-s=live]) .dk-ov{display:flex}
.dk-ic{width:52px;height:52px;border-radius:16px;background:var(--fill);border:1px solid var(--line);box-shadow:inset 0 1px 0 var(--hl);display:flex;align-items:center;justify-content:center;color:var(--muted)}
.dk-ic svg{display:none;width:22px;height:22px;fill:none;stroke:currentColor;stroke-width:1.5;stroke-linecap:round;stroke-linejoin:round}
#desk-view[data-s=idle] .ic-n,#desk-view[data-s=error] .ic-e{display:block}
#desk-view[data-s=loading] .sp{display:block;animation:dkSpin 1s linear infinite}
#desk-view[data-s=error] .dk-ic{color:var(--err);border-color:rgba(var(--err-rgb),.4)}
#deskMsg{max-width:420px;line-height:1.6;font-size:13px;color:var(--muted)}
#deskMsg.err{color:var(--err)}
.dk-ov button{display:none;align-items:center;gap:6px;height:32px;padding:0 16px;font-size:12px;font-weight:500;background:var(--fill);color:var(--text);border:1px solid var(--line);border-radius:var(--r-md)}
.dk-ov button:hover{background:var(--fill-hover);filter:none}
#desk-view[data-s=error] .dk-ov button{display:inline-flex}
#deskToast{position:absolute;left:50%;top:16px;transform:translateX(-50%);max-width:80%;padding:7px 14px;border-radius:var(--r-md);background:var(--toast-bg);border:1px solid rgba(var(--err-rgb),.5);color:var(--err);font-size:12px;display:none;z-index:3;box-shadow:0 8px 24px rgba(0,0,0,.4)}
@keyframes dkSpin{to{transform:rotate(360deg)}}
/* ===== 连接中提示（屏幕中央）+ 选项「?」气泡；全部走主题变量 ===== */
.spin{display:inline-block;width:16px;height:16px;flex:none;border:2px solid var(--line-hi);border-top-color:var(--accent);border-radius:50%;animation:dkSpin .8s linear infinite;vertical-align:-3px}
#connect.loading{display:inline-flex;align-items:center;gap:7px;opacity:1;cursor:progress}
#connect.loading .spin{width:12px;height:12px;border-color:rgba(var(--accent-rgb),.35);border-top-color:var(--accent-ink)}
#connToast{position:fixed;left:50%;top:50%;transform:translate(-50%,-50%);z-index:2000;display:none;max-width:min(440px,calc(100vw - 32px));min-width:240px;padding:14px 18px;border-radius:14px;background:var(--pop-solid);color:var(--text);border:1px solid var(--line-hi);box-shadow:var(--shadow),0 18px 48px rgba(0,0,0,.28);font-size:13px;line-height:1.55}
#connToast .ct-row{display:flex;align-items:flex-start;gap:10px}
#connToast .ct-msg{font-weight:600;overflow-wrap:anywhere}
#connToast .ct-sub{color:var(--muted);font-size:12px;font-weight:400;margin-top:2px;overflow-wrap:anywhere}
#connToast .ct-ico{flex:none;width:16px;height:16px;margin-top:2px;display:none;font-weight:700;text-align:center;line-height:16px}
#connToast .ct-act{display:none;justify-content:flex-end;gap:8px;margin-top:12px}
#connToast .ct-act button{height:28px;padding:0 12px;font-size:12px}
#connToast.connecting .ct-act.c,#connToast.retry .ct-act.c,#connToast.confirm .ct-act.f{display:flex}
#connToast.error{background:var(--toast-bg);border-color:rgba(var(--err-rgb),.55)}
#connToast.error .ct-msg,#connToast.error .ct-ico{color:var(--err)}
#connToast.warn{border-color:rgba(var(--warn-rgb),.6)}
#connToast.warn .ct-msg,#connToast.warn .ct-ico{color:var(--warn)}
#connToast.confirm{border-color:rgba(var(--warn-rgb),.6)}
#connToast.confirm .ct-msg{color:var(--warn)}
#connToast.error .ct-ico,#connToast.warn .ct-ico,#connToast.confirm .ct-ico{display:block}
#connToast.error .spin,#connToast.warn .spin,#connToast.confirm .spin{display:none}
.qm{flex:none;width:16px;height:16px;min-width:16px;padding:0;margin:0 0 0 -3px;border-radius:50%;background:var(--fill);color:var(--muted);border:1px solid var(--line-hi);font-size:11px;font-weight:700;line-height:14px;text-align:center;cursor:help}
.qm:hover,.qm.on{background:var(--fill-hover);color:var(--text);border-color:var(--accent);filter:none}
.qm:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.tip{position:fixed;z-index:3000;display:none;width:min(320px,calc(100vw - 24px));padding:10px 12px;border-radius:var(--r-md);background:var(--pop-solid);color:var(--text);border:1px solid var(--line-hi);box-shadow:var(--shadow);font-size:12px;line-height:1.6;font-weight:400}
.tip b{color:var(--warn)}
@media (max-width:900px){#connToast{padding:12px 14px}}
/* 窄窗口：头部换行、工具栏紧凑，不出现横向溢出 */
@media (max-width:1100px){.brand .bname{display:none}}
@media (max-width:900px){
  #app{padding:8px;gap:8px}
  header{padding:8px 10px}
  header input.server{width:130px}
  #tree-panel{width:180px}
  #conn-info{top:8px}
}
@media (prefers-reduced-motion:reduce){*,*::before,*::after,html.theme-anim *,html.theme-anim *::before,html.theme-anim *::after{animation:none!important;transition:none!important}}
</style>
<script>
// 主题：偏好 auto/light/gray/dark 存 localStorage；auto 跟随系统（亮→浅色，暗→深色，灰色仅手动选）。
(function(){
  var K='ra-theme',R=document.documentElement,mq=window.matchMedia?matchMedia('(prefers-color-scheme: dark)'):null,rm=window.matchMedia?matchMedia('(prefers-reduced-motion: reduce)'):null,pref='auto',timer=0,ls=[];
  try{var v=localStorage.getItem(K);if(v==='light'||v==='gray'||v==='dark')pref=v}catch(e){}
  function eff(){return pref==='auto'?(mq&&mq.matches?'dark':'light'):pref}
  function apply(anim){
    if(anim&&!(rm&&rm.matches)){R.classList.add('theme-anim');clearTimeout(timer);timer=setTimeout(function(){R.classList.remove('theme-anim')},260)}
    R.setAttribute('data-theme',eff());R.setAttribute('data-pref',pref);
    for(var i=0;i<ls.length;i++){try{ls[i](pref,eff())}catch(e){}}
  }
  function set(p){pref=p;try{if(p==='auto')localStorage.removeItem(K);else localStorage.setItem(K,p)}catch(e){}apply(true)}
  if(mq){var f=function(){if(pref==='auto')apply(true)};if(mq.addEventListener)mq.addEventListener('change',f);else if(mq.addListener)mq.addListener(f)}
  window.RATheme={get:function(){return pref},eff:eff,set:set,on:function(fn){ls.push(fn)}};
  apply(false);
})();
</script>
</head>
<body>
<div id="app">
  <header>
    <div class="brand"><svg viewBox="0 0 28 28" aria-hidden="true"><defs><linearGradient id="lg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" style="stop-color:var(--brand1)"/><stop offset="1" style="stop-color:var(--brand2)"/></linearGradient></defs><rect x="1" y="1" width="26" height="26" rx="8" fill="url(#lg)" fill-opacity=".22" stroke="url(#lg)" stroke-width="1.4"/><path d="M8 10.5h12v7H8z" fill="none" stroke="url(#lg)" stroke-width="1.6" stroke-linejoin="round"/><path d="M11.5 20.5h5" stroke="url(#lg)" stroke-width="1.6" stroke-linecap="round"/></svg><span class="bname">Remote Assist</span></div>
    <div class="hgrp"><span class="lbl">协助码</span><span class="codebox" id="codeBox" title="协助码：每格 4 位，可直接粘贴整串（支持 协助码@host:port）"><input class="cseg" maxlength="4" inputmode="text" autocomplete="off" autocapitalize="characters" spellcheck="false" aria-label="协助码第 1 段" placeholder="XXXX"><i>-</i><input class="cseg" maxlength="4" autocomplete="off" autocapitalize="characters" spellcheck="false" aria-label="协助码第 2 段" placeholder="XXXX"><i>-</i><input class="cseg" maxlength="4" autocomplete="off" autocapitalize="characters" spellcheck="false" aria-label="协助码第 3 段" placeholder="XXXX"></span><input type="hidden" id="code"></div>
    <div class="hgrp"><span class="lbl">中转地址</span><input class="server" id="server" placeholder="host:port" title="中转服务地址，如 127.0.0.1:28443。留空使用默认中转；填过的地址连接成功后会被记住"></div>
    <label class="chk"><input type="checkbox" id="noauth" aria-describedby="tipNoauth"><span>NoAuth</span></label><button type="button" class="qm" id="qmNoauth" data-tip="tipNoauth" aria-label="NoAuth 说明" aria-describedby="tipNoauth" aria-expanded="false">?</button>
    <div class="hgrp hacts"><button id="connect">连接</button><button class="sm ghost" id="btnProc">进程</button><button class="sm ghost" id="btnDesk" title="查看并操控对端桌面（对端需在已登录的图形会话中运行 share）">远程桌面</button></div>
    <span id="compat" hidden><span class="cm-txt">当前为兼容模式连接</span><button type="button" class="qm" id="qmCompat" data-tip="tipCompat" aria-label="兼容模式说明" aria-describedby="tipCompat" aria-expanded="false">?</button></span>
    <button id="status" title="连接详情" disabled>未连接</button>
    <button class="sm" id="themeBtn" title="主题" aria-label="切换主题" aria-haspopup="menu" aria-expanded="false"><svg viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="5.5"/><path d="M8 2.5a5.5 5.5 0 0 1 0 11z" fill="currentColor"/></svg></button>
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
        <div id="term-box"><div id="term-output"><div id="term-log"></div><div class="input-row"><span class="prompt" id="termPrompt">$</span><input id="termInput" autocomplete="off" autocapitalize="off" spellcheck="false" aria-label="命令输入"></div></div><div id="term-status"><span id="term-state" class="meta">未连接</span><button id="termStop" class="sec" type="button">中断 (Ctrl+C)</button><span class="spacer"></span><span class="meta" id="termNote"></span></div></div>
        <div id="search-box"><div class="sp"><label>正则:</label><input id="grepPattern" placeholder="TODO"><label>目录:</label><input id="grepRoot" placeholder="C:\\"><button class="sm" id="grepRun">搜索</button></div><div id="search-output"></div></div>
        <div id="proc-box"><div id="proc-output"></div></div>
        <div id="desk-box"><div id="desk-bar"><div class="dk-grp"><span class="dk-pill" id="deskPill" data-s="idle"><i class="dk-dot"></i><span id="deskPillTxt">未连接</span></span></div><div class="dk-grp dk-ctrls"><span class="dk-lbl">画质</span><div class="dk-seg" data-for="deskQ" role="group" aria-label="画质"></div><span class="dk-lbl">帧率</span><div class="dk-seg" data-for="deskFps" role="group" aria-label="帧率"></div><span class="dk-stat" id="deskStat" aria-live="off"></span><select id="deskW" class="dk-hide" aria-label="画面宽度"><option value="800">800</option><option value="1024">1024</option><option value="1280" selected>1280</option><option value="1600">1600</option><option value="1920">1920</option></select><select id="deskDisp" aria-label="显示器"><option value="0">显示器 1（主）</option></select></div><div class="dk-grp dk-acts"><button class="dk-btn" id="deskGo" title="刷新 / 重新连接"><svg viewBox="0 0 16 16"><path d="M13.5 8a5.5 5.5 0 1 1-1.7-4"/><path d="M13 2v3h-3"/></svg>刷新</button><button class="dk-btn" id="deskFit" aria-pressed="false" title="切换适应窗口 / 原始大小"><svg viewBox="0 0 16 16"><rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M5.5 8h5"/></svg><span id="deskFitTxt">适应窗口</span></button><button class="dk-btn" id="deskFull" title="全屏"><svg viewBox="0 0 16 16"><path d="M2 6V2h4M14 6V2h-4M2 10v4h4M14 10v4h-4"/></svg>全屏</button><span class="dk-sep"></span><label class="dk-sw" title="关闭后只看画面，不向对端发送鼠标键盘"><input type="checkbox" id="deskCtl" checked><span>允许操控</span></label></div><select id="deskFps" class="dk-hide" aria-label="帧率"><option value="0" selected>自动</option><option value="5">5</option><option value="10">10</option><option value="15">15</option><option value="30">30</option></select><select id="deskQ" class="dk-hide" aria-label="画质"><option value="0" selected>自动</option><option value="40">低</option><option value="60">中</option><option value="80">高</option></select></div><div id="desk-view" data-s="idle"><canvas id="deskCv" width="640" height="360"></canvas><textarea id="deskKb" aria-label="remote keyboard input" autocomplete="off" autocapitalize="off" spellcheck="false"></textarea><div class="dk-chip" id="deskHint"><span class="h-off"><svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M4 2l8 6-3.5.8L7 12.5z"/></svg>点击画面开始操控</span><span class="h-on"><svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><rect x="1.5" y="4" width="13" height="8" rx="1.5"/><path d="M4.5 9.5h7"/></svg>键盘已捕获</span><span class="h-ro"><svg width="12" height="12" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M1.5 8S4 3.5 8 3.5 14.5 8 14.5 8 12 12.5 8 12.5 1.5 8 1.5 8z"/><circle cx="8" cy="8" r="1.8"/></svg>只看不控</span></div><div id="deskToast"></div><div class="dk-ov" id="deskOv"><div class="dk-ic"><svg class="ic-n" viewBox="0 0 16 16"><rect x="1.5" y="2.5" width="13" height="8.5" rx="1.5"/><path d="M5.5 14h5M8 11v3"/></svg><svg class="ic-e" viewBox="0 0 16 16"><path d="M8 1.8l6.6 11.4H1.4z"/><path d="M8 6.5v3M8 11.3v.2"/></svg><svg class="sp" viewBox="0 0 16 16"><path d="M8 1.8a6.2 6.2 0 1 1-6.2 6.2"/></svg></div><div id="deskMsg"></div><button id="deskRetry"><svg viewBox="0 0 16 16"><path d="M13.5 8a5.5 5.5 0 1 1-1.7-4"/><path d="M13 2v3h-3"/></svg>重试</button></div></div></div>
        <div id="file-box"><div id="file-hdr"><span class="path" id="fpath"></span><span class="info" id="finfo"></span><button class="sm image-tool" id="fZoomOut" title="缩小" aria-label="缩小">−</button><button class="sm image-tool" id="fZoomFit" title="适合窗口">适配</button><button class="sm image-tool" id="fZoomIn" title="放大" aria-label="放大">+</button><button class="sm image-tool" id="fDownload" title="另存为" aria-label="另存为">⇩</button><button class="sm" id="fEdit" style="display:none">编辑</button><button class="sm" id="fSave" style="display:none">保存</button><button class="sm" id="fCancel" style="display:none">取消</button></div><div id="file-content"></div><div id="file-editor"><textarea id="fEditor"></textarea></div></div>
      </div>
    </div>
  </main>
</div>
<div id="connToast" role="status" aria-live="polite" data-state=""><div class="ct-row"><span class="spin" aria-hidden="true"></span><span class="ct-ico" aria-hidden="true">!</span><div><div class="ct-msg" id="connToastMsg"></div><div class="ct-sub" id="connToastSub"></div></div></div><div class="ct-act c"><button type="button" class="sm" id="connCancel">取消连接 (Esc)</button></div><div class="ct-act f"><button type="button" class="sm" id="connLegacyNo">取消</button><button type="button" id="connLegacyOk">以兼容模式继续</button></div></div>
<div class="tip" id="tipNoauth" role="tooltip"><b>NoAuth（免协助码）</b><br>不输入协助码直接连接。仅当被控端以 share --standalone --no-auth 启动、且网络完全可信（内网）时使用：任何能连到该中继的设备都能控制被控端。</div>
<div class="tip" id="tipCompat" role="tooltip"><b>兼容模式（协议 v1）</b><br>对端是 0.0.x 旧版，已自动用 v1 兼容模式连接。该模式关闭了 AAD 绑定、强制密文参数与抗重放，仅限可信网络使用。建议升级被控端到新版。</div>
<div id="conn-info"></div>
<div id="ctxmenu"></div>
<div id="themeMenu" role="menu" aria-label="主题"></div>
<script>
const $=s=>document.querySelector(s),status=$('#status');
// 访问令牌从启动 URL 的 query 里取，之后每次调 API 都带 X-Auth-Token 头。
// 用自定义头是刻意的：自定义头必然触发 CORS 预检，跨站页面连预检都过不去，
// 从根上挡掉「任意网页偷偷调本地 GUI 去操控远端机器」。EventSource 和下载用的 <a>
// 设不了头，只能退回 query 参数（令牌本身即凭据，跨站方拿不到，同样安全）。
const TOKEN=new URLSearchParams(location.search).get('token')||'';
function withToken(path){return path+(path.includes('?')?'&':'?')+'token='+encodeURIComponent(TOKEN)}
let connecting=null,protoLegacy=false;
let connected=false,cmdHistory=[],histIdx=-1,currentFilePath='',peerVersion='',helpVersion='',peerHost='';
let tabs=[],activeTab=null;
const ICO={dir:'<svg viewBox="0 0 16 16"><path d="M1.8 4.2c0-.7.5-1.2 1.2-1.2h3l1.5 1.7H13c.7 0 1.2.5 1.2 1.2v6c0 .7-.5 1.2-1.2 1.2H3c-.7 0-1.2-.5-1.2-1.2z"/></svg>',drv:'<svg viewBox="0 0 16 16"><rect x="1.8" y="8" width="12.4" height="5" rx="1.4"/><path d="M3 8l1.6-4.5c.2-.5.6-.8 1.1-.8h4.6c.5 0 .9.3 1.1.8L13 8"/><path d="M11 10.5h.01"/></svg>',fil:'<svg viewBox="0 0 16 16"><path d="M4 1.8h5.2L12.6 5.2V13c0 .7-.5 1.2-1.2 1.2H4.6c-.7 0-1.2-.5-1.2-1.2V3c0-.7.5-1.2 1.2-1.2z"/><path d="M9 1.9v3.4h3.4"/></svg>'};
function mkIco(k){const e=document.createElement('span');e.className='ico '+k;e.innerHTML=ICO[k];return e}

function setCompat(on){const c=$('#compat');if(!c)return;if(!on){const q=$('#qmCompat'),t=$('#tipCompat');t.style.display='none';q.classList.remove('on');q.setAttribute('aria-expanded','false');q._pin=false}c.hidden=!on}
function setStatus(ok,text){connected=ok;setCompat(ok&&protoLegacy);status.textContent=text;status.className=ok?'ok':(text==='未连接'?'':'bad');status.disabled=!ok;if(!ok)$('#conn-info').style.display='none';renderConnBtn()}
// 连接与断开共用一个按钮：连上了它就是「断开」，没连就是「连接」。
function renderConnBtn(){const b=$('#connect');if(!b)return;
  if(connecting){b.disabled=true;b.className='loading';b.innerHTML='<span class="spin" aria-hidden="true"></span>连接中…';b.setAttribute('aria-busy','true');return}
  b.removeAttribute('aria-busy');b.disabled=false;b.textContent=connected?'断开':'连接';b.className=connected?'sec':''}

const es=new EventSource(withToken('/api/events')); // EventSource 设不了自定义头，只能走 query
// 后端广播的全是具名事件（event: xxx，不带 data 字段），而 onmessage 只对带 data 的帧
// 触发——所以原先那条 onmessage→底部日志框的线永远不会有内容，日志框一直是空的。
// 日志框已移除：状态归右上角状态灯，值得留痕的（重连/丢失）直接说给终端。
// 断线原因：后端在 reconnecting/reconnect-fail/lost 事件的 data 里带 JSON {reason,text,hint,retry,max}，
// 区分被控端断开 / 被控端重启 / 中继隧道断开 / 网络不可达 等，给出不同文案与建议动作。
let dropInfo=null;
function parseDrop(e){try{const d=JSON.parse(e&&e.data);if(d&&d.reason)return d}catch(_){}return null}
// 工具调用错误文本（含 peer_gone / tunnel_lost / not_connected）转成同一套中文文案
function dropFromErr(s){
  s=String(s||'');
  if(/peer_gone/.test(s))return /连接已更新/.test(s)?'被控端已重启或热升级，正在重新连接':'被控端已断开，请确认对方程序仍在运行';
  if(/tunnel_lost/.test(s))return '中继或隧道已断开，请检查网络与中继服务';
  if(/not_connected/.test(s))return dropInfo?dropInfo.text+'：'+dropInfo.hint:'连接已断开，请重新连接';
  return '';
}
es.addEventListener('connected',()=>{dropInfo=null;setStatus(true,'已连接');loadDrives();termOnConnected();refreshProto()});
// 自动重连后协议版本可能变化（对端升级后会重新协商为 v2），以后端状态为准刷新兼容模式标识。
async function refreshProto(){const r=await api('/api/status',{});if(!connected||!r||!r.connected||!r.proto_version)return;protoLegacy=String(r.proto_version)==='1';setCompat(protoLegacy)}
es.addEventListener('disconnected',()=>{setStatus(false,'已断开');termOnDisconnected()});
es.addEventListener('reconnecting',e=>{
  const d=parseDrop(e);dropInfo=d;
  setStatus(false,d?'重连中（'+d.text+'）':'重连中...');
  termLog('<span class="meta">连接断开'+(d?'：'+esc(d.text)+'。'+esc(d.hint):'')+'；正在自动重连...</span>');
});
es.addEventListener('reconnect-fail',e=>{
  const d=parseDrop(e);if(!d)return;
  const changed=!dropInfo||dropInfo.reason!==d.reason;dropInfo=d;
  setStatus(false,'重连中（'+d.text+'，第 '+d.retry+'/'+d.max+' 次失败）');
  if(changed)termLog('<span class="meta">重连失败：'+esc(d.text)+'。'+esc(d.hint)+'</span>');
});
es.addEventListener('lost',e=>{
  const d=parseDrop(e);if(d)dropInfo=d;
  setStatus(false,d?'丢失（'+d.text+'）':'丢失');
  termLog('<span class="err">自动重连失败'+(d?'：'+esc(d.text)+'。'+esc(d.hint):'')+'；请处理后重新点击连接</span>');
});
es.addEventListener('upgrade-progress',e=>{const n=$('#upgrade-notice');n.style.display='flex';n.className='';$('#upgrade-msg').textContent=e.data});
es.onerror=()=>{};

// ===== 主题菜单 =====
(function(){
  const btn=$('#themeBtn'),menu=$('#themeMenu'),T=window.RATheme;
  const OPTS=[['auto','跟随系统'],['light','浅色'],['gray','灰色'],['dark','深色']];
  menu.innerHTML=OPTS.map(o=>'<div class="ti" role="menuitemradio" tabindex="0" data-v="'+o[0]+'"><span class="ck">✓</span>'+o[1]+'</div>').join('');
  const NAMES={auto:'跟随系统',light:'浅色',gray:'灰色',dark:'深色'};
  function sync(){const p=T.get();btn.title='主题：'+NAMES[p]+(p==='auto'?'（当前'+NAMES[T.eff()]+'）':'');menu.querySelectorAll('.ti').forEach(e=>e.setAttribute('aria-checked',e.dataset.v===p?'true':'false'))}
  function close(){menu.style.display='none';btn.setAttribute('aria-expanded','false')}
  function open(){const r=btn.getBoundingClientRect();menu.style.display='block';menu.style.top=(r.bottom+6)+'px';menu.style.right=Math.max(8,innerWidth-r.right)+'px';btn.setAttribute('aria-expanded','true');sync();const c=menu.querySelector('[aria-checked=true]');if(c)c.focus()}
  btn.addEventListener('click',e=>{e.stopPropagation();menu.style.display==='block'?close():open()});
  menu.addEventListener('click',e=>{e.stopPropagation();const t=e.target.closest('.ti');if(!t)return;T.set(t.dataset.v);close();btn.focus()});
  menu.addEventListener('keydown',e=>{
    const items=[...menu.querySelectorAll('.ti')],i=items.indexOf(document.activeElement);
    if(e.key==='ArrowDown'){e.preventDefault();items[(i+1)%items.length].focus()}
    else if(e.key==='ArrowUp'){e.preventDefault();items[(i+items.length-1)%items.length].focus()}
    else if(e.key==='Enter'||e.key===' '){e.preventDefault();if(i>=0)items[i].click()}
  });
  document.addEventListener('click',close);
  document.addEventListener('keydown',e=>{if(e.key==='Escape'&&menu.style.display==='block'){close();btn.focus()}});
  window.addEventListener('resize',close);
  T.on(sync);sync();
})();

async function api(path,body,sig){const ctrl=new AbortController();if(sig){if(sig.aborted)ctrl.abort();else sig.addEventListener('abort',()=>ctrl.abort(),{once:true})}const t=setTimeout(()=>ctrl.abort(),30000);try{const r=await fetch(path,{method:'POST',headers:{'Content-Type':'application/json','X-Auth-Token':TOKEN},body:JSON.stringify(body||{}),signal:ctrl.signal});return await r.json()}catch(e){return{ok:false,error:String(e)}}finally{clearTimeout(t)}}

function formatConnectedTime(seconds){
  seconds=Math.max(0,Number(seconds)||0);if(seconds<60)return seconds+' 秒';if(seconds<3600)return Math.floor(seconds/60)+' 分钟';return Math.floor(seconds/3600)+' 小时 '+Math.floor(seconds%3600/60)+' 分钟';
}
function connectionRow(label,value,cls){return'<div class="conn-row"><span class="conn-label">'+esc(label)+'</span><span class="conn-value '+(cls||'')+'">'+esc(value||'—')+'</span></div>'}
async function showConnectionInfo(){
  if(!connected)return;const panel=$('#conn-info');panel.style.display='block';panel.innerHTML='<div class="conn-title"><span>连接状态</span><span class="meta">测量中...</span></div>';
  const r=await api('/api/status',{});if(!r.connected){panel.innerHTML='<div class="conn-title"><span>连接状态</span></div>'+connectionRow('状态','已断开');return}
  const mode=r.p2p?(r.p2p_path==='udp_relay'?'UDP 中继':'P2P 直连'):'中继转发',latency=r.latency_ms!==undefined?r.latency_ms+' ms':'不可用';
  panel.innerHTML='<div class="conn-title"><span>连接状态</span><span class="ok">'+esc(mode)+'</span></div>'+
    connectionRow('服务器',r.server)+connectionRow('传输',mode,r.p2p&&r.p2p_path!=='udp_relay'?'transport-p2p':'')+connectionRow('往返延迟',latency)+
    connectionRow('远端主机',r.peer_host)+connectionRow('远端版本',r.peer_version)+connectionRow('工具协议',r.proto_version?('v'+r.proto_version+(r.proto_version==='1'?'（兼容模式，安全防护已降级）':'')):'')+connectionRow('Help 版本',r.help_version)+
    connectionRow('会话 ID',r.session_id)+connectionRow('已连接',formatConnectedTime(r.connected_seconds));
}
status.onclick=e=>{e.stopPropagation();const panel=$('#conn-info');if(panel.style.display==='block')panel.style.display='none';else showConnectionInfo()};
$('#conn-info').onclick=e=>e.stopPropagation();
document.addEventListener('click',()=>{$('#conn-info').style.display='none'});

// ========== 协助码分段输入 ==========
// 三格各 4 位；隐藏的 #code 始终是数据源（12 位无分隔大写 或 10 位大小写混合（旧版公网中转），或粘贴的 code@host:port 原串）。
(function(){
  const segs=Array.from(document.querySelectorAll('#codeBox .cseg')),hid=$('#code');
  // 保留大小写：旧版公网中转发 10 位大小写混合码（大小写敏感）；本版中转的 12 位码全大写，输入小写时同步转大写
  const clean=s=>String(s||'').replace(/[^A-Za-z0-9]/g,'');
  const norm=s=>s.length===12?s.toUpperCase():s;
  let raw=''; // 非空表示 #code 保留粘贴的原串（含 @host:port）
  function sync(){hid.value=raw||norm(segs.map(s=>s.value).join(''))}
  // 从第 start 格开始铺满字符，返回应聚焦的格序号与光标位置
  function fill(str,start){
    let i=start,pos=0;
    for(let k=start;k<segs.length;k++)segs[k].value='';
    while(str&&i<segs.length){const part=str.slice(0,4);segs[i].value=part;str=str.slice(4);pos=part.length;if(str&&i<segs.length-1)i++;else break}
    return{i,pos};
  }
  function focusAt(i,pos){const s=segs[i];s.focus();const p=pos==null?s.value.length:pos;s.setSelectionRange(p,p)}
  segs.forEach((s,idx)=>{
    s.addEventListener('input',()=>{
      raw='';
      const v=clean(s.value);
      if(v.length>4){const r=fill(v,idx);focusAt(r.i,r.pos)}
      else{s.value=v;if(v.length===4&&idx<2&&s.selectionStart>=4)focusAt(idx+1,0)}
      sync();
    });
    s.addEventListener('keydown',e=>{
      if(e.isComposing)return;
      if(e.key==='Enter'){e.preventDefault();if(!connected)$('#connect').click();return}
      if(e.key==='-'||e.key==='_'||e.key===' '){e.preventDefault();if(s.value.length>0&&idx<2)focusAt(idx+1,0);return} // 空格里的分隔符忽略：敲满 4 位已自动跳格，再按 - 不能多跳一格
      const atStart=s.selectionStart===0&&s.selectionEnd===0,atEnd=s.selectionStart===s.value.length&&s.selectionEnd===s.value.length;
      if(e.key==='Backspace'&&atStart&&idx>0){e.preventDefault();const p=segs[idx-1];p.value=p.value.slice(0,-1);raw='';sync();focusAt(idx-1);return}
      if(e.key==='ArrowLeft'&&atStart&&idx>0){e.preventDefault();focusAt(idx-1);return}
      if(e.key==='ArrowRight'&&atEnd&&idx<2){e.preventDefault();focusAt(idx+1,0)}
    });
    s.addEventListener('paste',e=>{
      e.preventDefault();
      const t=(e.clipboardData||window.clipboardData).getData('text').trim();
      const at=t.indexOf('@'),body=clean(at>=0?t.slice(0,at):t);
      const r=fill(body,body.length>=12?0:idx);
      raw=at>=0?t:'';
      focusAt(r.i,r.pos);sync();
    });
  });
  // 外部直接改 #code.value 后可派发 input 事件让三格跟随
  hid.addEventListener('input',()=>{const t=hid.value;const at=t.indexOf('@');fill(clean(at>=0?t.slice(0,at):t),0);raw=at>=0?t:''});
  const na=$('#noauth');
  function syncNA(){const off=na.checked;segs.forEach(s=>s.disabled=off);$('#codeBox').classList.toggle('off',off)}
  na.addEventListener('change',syncNA);syncNA();
})();

// ========== 连接流程：中央提示 + 取消 + 旧版自动降级 ==========
// 状态机：idle → connecting →（对端是旧版则 retry，由后端 SSE 通知）→ 成功 / 失败 / 需确认降级。
// connecting 非空期间连接按钮禁用，重复点击、回车都不会并发发起第二次。
let toastTimer=0;
const CONNECT_ABORT_MS=50000; // 略长于后端总时限（45s），让后端先给出归一化原因
function connToastHide(){clearTimeout(toastTimer);const t=$('#connToast');t.style.display='none';t.className='';t.setAttribute('data-state','')}
function connToastShow(state,msg,sub,autoHideMs){
  clearTimeout(toastTimer);
  const t=$('#connToast');t.className=state;t.setAttribute('data-state',state);
  $('#connToastMsg').textContent=msg;$('#connToastSub').textContent=sub||'';
  $('#connToastSub').style.display=sub?'block':'none';
  t.querySelector('.ct-ico').textContent=state==='error'?'×':'!';
  t.style.display='block';
  if(autoHideMs)toastTimer=setTimeout(connToastHide,autoHideMs);
}
function connectLabel(server,code){
  if(server)return server;
  const at=String(code||'').indexOf('@');
  return at>=0?code.slice(at+1).trim():'被协助端';
}
function cancelConnect(){if(connecting)connecting.ctrl.abort()}
$('#connCancel').onclick=cancelConnect;
$('#connLegacyNo').onclick=()=>{connToastHide();setStatus(false,'未连接')};
$('#connLegacyOk').onclick=()=>{connToastHide();doConnect(true)};
es.addEventListener('connect-fallback',()=>{if(connecting)connToastShow('retry','对端是旧版，正在以兼容模式重试…','仅限可信网络；兼容模式安全性较低')});

// 中转地址记忆：连接成功后保存，下次打开页面自动带出。失败/无存储时静默忽略。
function loadServerMemo(){try{const v=localStorage.getItem('ra-relay');const e=document.getElementById('server');if(v&&e&&!e.value)e.value=v}catch(_){}}
function saveServerMemo(v){try{if(v)localStorage.setItem('ra-relay',v);else localStorage.removeItem('ra-relay')}catch(_){}}
loadServerMemo();
async function doConnect(allowLegacy){
  if(connecting)return;
  const code=$('#code').value.trim(),server=$('#server').value.trim(),noauth=$('#noauth').checked;
  const label=connectLabel(server,code);
  const ctrl=new AbortController();connecting={ctrl};
  // 发起新连接时后端会先关掉旧连接：同样先停终端会话，避免取消/失败后残留红色中断字样
  if(connected&&window.PtyTerm)PtyTerm.disconnected();
  resetRemoteState(); // 可能换了一台远端，旧缓存一律作废
  setStatus(false,'连接中...');
  connToastShow('connecting','正在连接 '+label+'…','按 Esc 或点「取消连接」可中止');
  let timedOut=false; // 区分“前端兜底超时 abort”与“用户取消 abort”
  const timer=setTimeout(()=>{timedOut=true;ctrl.abort()},CONNECT_ABORT_MS);
  let r;
  try{
    const resp=await fetch('/api/connect',{method:'POST',headers:{'Content-Type':'application/json','X-Auth-Token':TOKEN},
      body:JSON.stringify({code,server,no_auth:noauth,allow_legacy:!!allowLegacy}),signal:ctrl.signal});
    r=await resp.json();
  }catch(e){r=ctrl.signal.aborted?(timedOut?{ok:false,code:'timeout',reason:'连接超时'}:{ok:false,code:'canceled'}):{ok:false,code:'other',reason:'请求失败',error:String(e)}}
  clearTimeout(timer);connecting=null;renderConnBtn();
  if(r.ok){
    saveServerMemo(server);
    const d=parseMCP(r.result);
    protoLegacy=String(d.proto_version||'')==='1';
    setStatus(true,'已连接');setUpgradeInfo(d);loadDrives();window.PtyTerm&&PtyTerm.connected();openTerm();termOnConnected();
    if(protoLegacy||r.fallback){
      const m='对端是旧版，已用兼容模式连接：安全性较低，仅限可信网络';
      connToastShow('warn',m,'协议版本 v1（AAD 绑定、强制密文参数、抗重放已关闭）',9000);
      termLog('<span class="err">[警告] '+esc(m)+'</span>');
    }else{
      connToastHide();
      if(d.warning)termLog('<span class="err">[警告] '+esc(d.warning)+'</span>');
    }
    return;
  }
  if(r.code==='canceled'){
    connToastHide();setStatus(false,'未连接');termLog('<span class="meta">已取消连接</span>');return;
  }
  setStatus(false,'失败');
  if(r.error)termLog('<span class="err">'+esc(r.error)+'</span>');
  if(r.code==='peer_legacy'&&r.needs_confirm){
    // 公网中继上不自动降级：中继是不可信第三方，可伪造「旧版」应答诱导降级，必须由用户确认
    connToastShow('confirm','对端似乎是旧版，需用户确认才能用兼容模式连接',
      '当前中继 '+label+' 不在内网/本机。兼容模式会关闭 AAD 绑定与抗重放，中继可能借此诱导降级或篡改。仅当确认对端确为旧版、且信任该中继时继续。');
    return;
  }
  let msg=r.reason||'连接失败';
  let sub='';
  if(r.code==='peer_legacy')sub='对端协议版本不兼容，请升级对端到新版。';
  else if(r.code==='bad_code')sub='请核对协助码，并确认被协助端已启动 share。';
  else if(r.code==='timeout'||r.code==='unreachable'||r.code==='refused')sub='请检查中继地址「'+label+'」与网络。';
  connToastShow('error',msg,sub,6000);
}
async function doDisconnect(){
  // 先停终端会话再断：否则后端关掉子进程时，PTY 流先收到“mcp process exited”会残留一行红字
  window.PtyTerm&&PtyTerm.disconnected();
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
  remoteOS='';protoLegacy=false;
  $('#treePath').value='';
  peerVersion='';helpVersion='';peerHost='';$('#upgrade-notice').style.display='none';$('#upgrade-notice').className='';
}
$('#connect').onclick=async()=>{
  if(connecting)return;
  const b=$('#connect');b.disabled=true;
  try{connected?await doDisconnect():await doConnect(false)}finally{if(!connecting){b.disabled=false;renderConnBtn()}}
};

// ========== 「?」气泡说明：悬停/聚焦显示，点击固定（窄屏/触屏靠点击开合），Esc 关闭 ==========
(function(){
  const btns=Array.from(document.querySelectorAll('.qm'));
  function tipOf(b){return document.getElementById(b.getAttribute('data-tip'))}
  function place(b){
    const t=tipOf(b),r=b.getBoundingClientRect();
    t.style.display='block';
    const w=t.offsetWidth,vw=document.documentElement.clientWidth;
    let x=Math.min(Math.max(8,r.left+r.width/2-w/2),vw-w-8);
    let y=r.bottom+8;
    if(y+t.offsetHeight>window.innerHeight-8)y=Math.max(8,r.top-t.offsetHeight-8);
    t.style.left=x+'px';t.style.top=y+'px';
  }
  function show(b){place(b);b.setAttribute('aria-expanded','true')}
  function hide(b){tipOf(b).style.display='none';b.classList.remove('on');b.setAttribute('aria-expanded','false');b._pin=false}
  function hideAll(except){btns.forEach(x=>{if(x!==except)hide(x)})}
  btns.forEach(b=>{
    b.addEventListener('mouseenter',()=>{hideAll(b);show(b)});
    b.addEventListener('mouseleave',()=>{if(!b._pin)hide(b)});
    b.addEventListener('focus',()=>{hideAll(b);show(b)});
    b.addEventListener('blur',()=>{if(!b._pin)hide(b)});
    b.addEventListener('click',e=>{
      e.preventDefault();e.stopPropagation();
      if(b._pin){hide(b);return}
      hideAll(b);show(b);b._pin=true;b.classList.add('on');
    });
  });
  document.addEventListener('click',()=>hideAll(null));
  window.addEventListener('resize',()=>hideAll(null));
  document.addEventListener('keydown',e=>{
    if(e.key!=='Escape')return;
    if(btns.some(b=>tipOf(b).style.display==='block')){hideAll(null);e._tipClosed=true}
  },true);
})();
// Esc 取消连接 / 关掉连接提示（气泡优先，见上方捕获阶段处理）
document.addEventListener('keydown',e=>{
  if(e.key!=='Escape'||e._tipClosed)return;
  if(connecting){e.preventDefault();cancelConnect();return}
  if($('#connToast').style.display==='block'&&$('#connToast').className!=='confirm')connToastHide();
});

function versionParts(s){
  const m=String(s||'').match(/(?:^|\s)v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?/i);
  return m?[Number(m[1]),Number(m[2]),Number(m[3]),Number(m[4]||0)]:null;
}
function isVersionOlder(current,latest){
  const a=versionParts(current),b=versionParts(latest);if(!a||!b)return false;
  for(let i=0;i<a.length;i++){if(a[i]!==b[i])return a[i]<b[i]}
  return false;
}
// peerHostIsWindows 与后端 update.PlatformFromHost 同规则：只看末尾的系统字段（末字段是架构），
// 不看开头的 user@host，叫 windows-box 的 Linux 不算 Windows。
function peerHostIsWindows(h){
  const f=String(h||'').trim().split(/\s+/);if(f.length<2)return false;
  const sys=f.slice(0,-1),w=x=>String(x||'').toLowerCase()==='windows';
  return w(sys[sys.length-1])||(sys.length>=2&&w(sys[sys.length-2]));
}
function setUpgradeInfo(meta){
  peerVersion=meta.peer_version||'';helpVersion=meta.help_version||'';peerHost=meta.peer_host||'';
  const n=$('#upgrade-notice');n.className='';
  if(!isVersionOlder(peerVersion,helpVersion)){n.style.display='none';return}
  $('#upgrade-msg').textContent='远端 share '+peerVersion+'，当前 help '+helpVersion;
  n.style.display='flex';$('#upgrade-select').disabled=false;$('#upgrade-select').style.display='inline-block';
}
$('#upgrade-select').onclick=()=>$('#upgrade-file').click();
$('#upgrade-file').onchange=async()=>{
  const input=$('#upgrade-file'),file=input.files&&input.files[0];if(!file)return;
  input.value='';
  // Windows 上 Defender 可能把 share 当威胁结束进程并隔离文件，升级会停在半途；先提醒加排除项。
  const defenderNote=peerHostIsWindows(peerHost)?'\n\n注意：Windows Defender 可能把 share 误报为威胁，结束进程并隔离文件，导致升级中断。建议先为 share 所在目录添加排除项。':'';
  if(!confirm('将 '+file.name+' 传到远端，验证后以先建后断方式升级 share。继续吗？'+defenderNote))return;
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
const BOXES=['term-box','search-box','proc-box','file-box','desk-box'];
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
  if(id!=='desk')deskAbort();
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
function termScroll(){const o=$('#term-output');o.scrollTop=o.scrollHeight}
// termLines 近似记录 #term-log 里的行数；超过上限就从头删，长输出（tail -f、编译日志）不会拖垮 DOM。
const TERM_MAX_LINES=5000;
let termLines=0,termAtLineStart=true;
function termTrim(){
  if(termLines<=TERM_MAX_LINES+500)return; // 留 500 行滞后量，避免每次追加都删
  const out=$('#term-log');
  while(termLines>TERM_MAX_LINES&&out.firstChild){
    const n=out.firstChild,t=n.textContent||'';
    for(let i=t.indexOf('\n');i>=0;i=t.indexOf('\n',i+1))termLines--;
    out.removeChild(n);
  }
}
function termClear(){$('#term-log').innerHTML='';termLines=0;termAtLineStart=true}
function termLog(s){
  if(!termAtLineStart){$('#term-log').insertAdjacentHTML('beforeend','\n');termLines++} // 上一条命令的半行输出后先换行
  $('#term-log').insertAdjacentHTML('beforeend',s+'\n');termAtLineStart=true;termLines++;termTrim();termScroll();
}
// termAppend 追加一段流式输出。用文本节点而不是 termLog 的 innerHTML 拼接：命令可以吐
// 半行（进度条、没换行的提示符），按行拼会把半行强行变成整行；textContent 也天然免转义。
function termAppend(text,cls){
  if(!text)return;
  const out=$('#term-log'),n=document.createElement('span');
  if(cls)n.className=cls;
  n.textContent=text;
  out.appendChild(n);
  for(let i=text.indexOf('\n');i>=0;i=text.indexOf('\n',i+1))termLines++;
  termAtLineStart=text.charAt(text.length-1)==='\n';
  termTrim();termScroll();
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
// 点击终端空白处（不是在选文字）就聚焦输入框，和真实终端一致
$('#term-output').addEventListener('mouseup',e=>{if(!String(window.getSelection()).length&&e.target!==$('#termInput'))$('#termInput').focus()});
function openTerm(){openTab('term','终端','term-box',()=>{if(window.PtyTerm&&PtyTerm.show())return;$('#termInput').focus()},true)}
function termHasSelection(){return String(window.getSelection()).length>0}
$('#termInput').addEventListener('keydown',e=>{
  const inp=$('#termInput');
  if(e.ctrlKey&&!e.altKey&&!e.metaKey&&(e.key==='c'||e.key==='C')){
    if(termHasSelection())return; // 有选中文字：保持浏览器默认的复制
    e.preventDefault();
    if(termRunning){termInterrupt();return}
    // 空闲时和 bash 一样：回显当前输入加 ^C，换新行
    termLog('<span class="cmd">'+esc(termPromptText()+' '+inp.value)+'</span><span class="meta">^C</span>');
    inp.value='';histIdx=-1;return;
  }
  if(e.ctrlKey&&!e.altKey&&!e.metaKey&&(e.key==='l'||e.key==='L')){e.preventDefault();termClear();return}
  if(e.key==='Enter'){e.preventDefault();if(termRunning)return;const v=inp.value;inp.value='';if(v.trim())execCmd(v);else termLog('<span class="cmd">'+esc(termPromptText())+'</span>')}
  else if(e.key==='ArrowUp'){e.preventDefault();if(cmdHistory.length){histIdx=Math.min(cmdHistory.length-1,histIdx+1);inp.value=cmdHistory[cmdHistory.length-1-histIdx]||''}}
  else if(e.key==='ArrowDown'){e.preventDefault();histIdx=Math.max(-1,histIdx-1);inp.value=histIdx<0?'':cmdHistory[cmdHistory.length-1-histIdx]||''}
});
// 点击终端的“中断”按钮等同 Ctrl+C
$('#termStop').addEventListener('click',()=>{termInterrupt();$('#termInput').focus()});
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
// ---- 终端会话状态：像真 shell 一样记住用户/主机/工作目录 ----
let termCwd='',termPrevCwd='',termUser='',termHostName='',termHome='',termRunning=false,termAbort=null,termProbeBusy=false,termProbeAt=0;
function termShortCwd(){
  let c=termCwd;if(!c)return '~';
  const h=termHome.replace(/[\\\/]$/,'');
  if(h&&(c===h||c.indexOf(h+'/')===0))c='~'+c.slice(h.length);
  return c;
}
function termPromptText(){
  if(remoteOS==='windows')return (termCwd||'PS')+'>';
  return (termUser?termUser+'@'+(termHostName||'remote')+':':'')+termShortCwd()+'$';
}
function termRenderPrompt(){
  $('#termPrompt').textContent=termRunning?'':termPromptText();
  $('#termStop').style.display=termRunning?'inline-block':'none';
  $('#term-state').textContent=termRunning?'运行中… 按 Ctrl+C 中断':(connected?'就绪':'未连接');
  $('#term-state').className=termRunning?'ok':'meta';
}
function termInterrupt(){
  if(!termRunning||!termAbort)return;
  termAbort.abort(); // 断开 /api/exec/stream：GUI 侧取消 → MCP cancelled → 隧道 ToolCancel → 远端杀进程树
}
// 连接成功后自动“进入 shell”：探测远端 OS/用户/主机/目录并打印欢迎信息
async function termOnConnected(){
  const now=Date.now();
  if(termProbeBusy||now-termProbeAt<3000)return; // doConnect 与 SSE 'connected' 会各触发一次，只探测一次
  termProbeBusy=true;
  try{
    remoteOS='';termCwd='';termPrevCwd='';termUser='';termHostName='';termHome='';
    const os=await detectOS();
    const argv=os==='windows'
      ?['powershell','-NoProfile','-Command','[Console]::OutputEncoding=[Text.Encoding]::UTF8;$env:USERNAME;$env:COMPUTERNAME;("Windows "+[System.Environment]::OSVersion.Version.ToString()+" "+$env:PROCESSOR_ARCHITECTURE);(Get-Location).Path;$env:USERPROFILE']
      :['sh','-c','id -un 2>/dev/null||whoami;hostname 2>/dev/null||uname -n;uname -sm;pwd;echo "$HOME"'];
    const r=await api('/api/call',{tool:'exec',args:{argv,timeout_ms:10000}});
    const d=r.ok?parseMCP(r.result):{};
    const L=String(d.stdout||'').split(/\r?\n/).map(s=>s.trim());
    if(r.ok&&L[0]){
      termUser=L[0];termHostName=L[1]||'';termCwd=L[3]||'';termHome=L[4]||'';
      termLog('<span class="ok">已连接 '+esc(termUser+'@'+termHostName)+' ('+esc(L[2]||os)+') — 工作目录 '+esc(termCwd)+'</span>');
    }else{
      termLog('<span class="ok">已连接对端</span><span class="meta">（未能探测系统信息：'+esc((d&&d.error)||r.error||'无输出')+'）</span>');
    }
    termLog('<span class="meta">提示：Ctrl+C 中断命令，Ctrl+L 清屏，上下键翻历史。</span>');
    termProbeAt=Date.now();
  }catch(e){termLog('<span class="err">探测远端失败: '+esc(String(e))+'</span>')}
  finally{termProbeBusy=false;termRenderPrompt()}
}
function termOnDisconnected(){
  termProbeAt=0;
  if(termRunning)termInterrupt();
  termRenderPrompt();
}
// 单独的 cd 在本地解析：每条命令都是新进程，cd 没法靠远端 shell 保留，所以前端维护 cwd。
// 先在远端实测目录是否存在、取规范化后的绝对路径，成功才更新。
async function termCd(line){
  let arg=line.trim().replace(/^cd\b/i,'').trim();
  if(remoteOS==='windows')arg=arg.replace(/^\/d\s+/i,'');
  if((arg[0]==='"'&&arg.slice(-1)==='"')||(arg[0]==="'"&&arg.slice(-1)==="'"))arg=arg.slice(1,-1);
  if(arg==='-'){arg=termPrevCwd;if(!arg){termLog('<span class="err">cd: 没有上一个目录</span>');return}}
  if(!arg||arg==='~'){
    if(!arg&&remoteOS==='windows'){termLog(esc(termCwd));return}
    arg=termHome||'';
  }else if(arg.slice(0,2)==='~/'||arg.slice(0,2)==='~\\'){arg=termHome+arg.slice(1)}
  let argv;
  if(remoteOS==='windows')argv=['powershell','-NoProfile','-Command',"[Console]::OutputEncoding=[Text.Encoding]::UTF8;Set-Location -LiteralPath '"+arg.replace(/'/g,"''")+"' -ErrorAction Stop; (Get-Location).Path"];
  else argv=['sh','-c','cd -- "$1" && pwd','sh',arg||'/'];
  const args={argv,timeout_ms:10000};if(termCwd)args.cwd=termCwd;
  const r=await api('/api/call',{tool:'exec',args});
  if(!r.ok){termLog('<span class="err">'+esc(r.error)+'</span>');return}
  const d=parseMCP(r.result);
  const out=String(d.stdout||'').trim().split(/\r?\n/).pop();
  if(d.exit_code||d.error||!out){
    termLog('<span class="err">'+esc(remoteOS==='windows'?('cd: 找不到目录 '+arg):String(d.stderr||d.error||('cd: '+arg+': 没有那个目录')).trim())+'</span>');return;
  }
  termPrevCwd=termCwd;termCwd=out;
}
async function execCmd(line){
  if(!connected||!line.trim()||termRunning)return;
  if(!cmdHistory.length||cmdHistory[cmdHistory.length-1]!==line)cmdHistory.push(line);histIdx=-1;
  termLog('<span class="cmd">'+esc(termPromptText()+' '+line)+'</span>');
  // clear/cls 是清「本地终端显示」，远端 shell 的 clear 清的是远端屏幕、传不回来，
  // 所以在前端直接拦截清空 DOM，不发给远端。
  const lc=line.trim().toLowerCase();
  if(lc==='clear'||lc==='cls'){termClear();return}
  termRunning=true;termRenderPrompt();
  try{
    const os=await detectOS();
    if(/^cd(\s|$)/i.test(line.trim()))await termCd(line);
    else await execStream(shellArgv(os,line));
  }catch(e){
    if(!(e&&e.name==='AbortError'))termLog('<span class="err">'+esc(String(e))+'</span>');
  }finally{
    termRunning=false;termAbort=null;termRenderPrompt();
    if(activeTab==='term')$('#termInput').focus();
  }
}

// execStream 边跑边渲染一条命令的输出：构建、tail、跑测试这类长命令不必等跑完才出字。
// 不走 api()——它有 30s abort，长命令必被腰斩；这里读 NDJSON 流直到远端收尾。
// 用户 Ctrl+C 时 abort 这次 fetch，由 GUI 把取消一路传到远端杀掉进程树。
async function execStream(argv){
  termAbort=new AbortController();
  const body={argv};if(termCwd)body.cwd=termCwd;
  try{
    const resp=await fetch('/api/exec/stream',{
      method:'POST',headers:{'Content-Type':'application/json','X-Auth-Token':TOKEN},body:JSON.stringify(body),signal:termAbort.signal
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
  }catch(e){
    if(e&&e.name==='AbortError'){termLog('<span class="meta">^C 已中断（已通知远端终止进程）</span>');return}
    throw e;
  }
}

function execEvent(ev,decs){
  if(ev.type==='chunk'){
    const dec=decs[ev.stream]||decs.stdout;
    termAppend(dec(b64bytes(ev.data||'')),ev.stream==='stderr'?'err':'');
    return;
  }
  if(ev.type==='error'){
    const m=String(ev.error||'');
    if(/deadline_exceeded/.test(m))termLog('<span class="err">命令已超时被终止（远端单条命令最长运行 5 分钟）；持续输出的命令请改用有限输出，如 tail -n 200。</span>');
    else termLog('<span class="err">'+esc(m)+'</span>');
    return;
  }
  if(ev.type==='result'){
    // 0.0.5 不会发流式块，而是把输出放在最终响应里；新版流式结果不带这两项，故兼容
    // 渲染不会重复输出。
    const d=parseMCP(ev.result);
    if(d.stdout)termAppend(d.stdout);
    if(d.stderr)termAppend(d.stderr,'err');
    if(d.error)termLog('<span class="err">无法执行: '+esc(d.error)+'</span>'); // 命令没能启动的原因
    if(d.exit_code)termLog('<span class="err">[退出码 '+esc(d.exit_code)+']</span>');
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
    bar.style.cssText='padding:8px 0;margin-top:6px;border-top:1px dashed var(--line-hi);display:flex;gap:8px;align-items:center';
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
  if(!startPath){tree.innerHTML='<div class="tloading">默认目录不可访问'+(lastError?'：'+esc(dropFromErr(lastError)||lastError):'')+'</div>';return}
  $('#treePath').value=startPath;addDirNode(tree,startPath,true,true);
}
function addDirNode(parent,path,isRoot,isExpanded){
  const name=path.replace(/[\\\/]$/,'');
  const nd=document.createElement('div');nd.className='tdir';nd.dataset.path=path;nd.dataset.kind='dir';
  const arrow=document.createElement('span');arrow.className='arr closed';nd.appendChild(arrow);
  const label=name.split(/[\\/]/).pop()||path;nd.appendChild(mkIco(isRoot?'drv':'dir'));nd.appendChild(document.createTextNode(label));
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
  nd.innerHTML='<span class="ico fil">'+ICO.fil+'</span>'+esc(entry.name)+'<span class="sz">'+formatSize(entry.size)+'</span>';
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

// ========== 远程桌面 ==========
// 画面：循环调用 screen_capture（请求-应答，串行，不堆积）；操控：鼠标/键盘事件排队，
// 合并连续 move 后串行调用 input，保证事件顺序。坐标一律按 0..1 归一化回传，由对端映射到真实像素。
const desk={running:false,loopId:0,queue:[],sending:false,pressed:new Set(),frames:0,drawn:0,lastInput:0,t0:0,bytes:0,disp:1,toastTimer:0};
function openDesk(){openTab('desk','远程桌面','desk-box',()=>{deskStart()},false)}
$('#btnDesk').onclick=()=>openDesk();
// 刷新/重连：先切到“加载中”态（旧画面变暗并显示覆盖层），首帧到达后回到 live
$('#deskGo').onclick=()=>{desk.loopId++;desk.running=false;deskStart(true)};
$('#deskRetry').onclick=()=>$('#deskGo').click();
// 文字提示：现在位于画面中央覆盖层，出错时 class=err
function deskMsg(s,isErr){const m=$('#deskMsg');m.textContent=s||'';m.className=isErr?'err':''}
// 界面状态：idle 未连接 / loading 等首帧 / live 有画面 / error 出错。同步画面区、状态胶囊与覆盖层文案
const DESK_PILL={idle:'未连接',loading:'连接中…',live:'已连接',error:'出错'};
function deskState(st,msg){
  $('#desk-view').dataset.s=st;
  const p=$('#deskPill');p.dataset.s=st;p.title=msg||'';
  $('#deskPillTxt').textContent=DESK_PILL[st]||st;
  if(st!=='live')$('#deskStat').textContent='';
  deskMsg(st==='live'?'':msg,st==='error');
}
// 有画面时的非致命错误（如输入下发失败）：用顶部短暂提示，不遮挡画面
function deskToast(s){
  const t=$('#deskToast');t.textContent=s;t.style.display='block';
  clearTimeout(desk.toastTimer);desk.toastTimer=setTimeout(()=>{t.style.display='none'},4000);
}
function deskErrText(e){
  const s=String(e||'');
  if(s.indexOf('desktop_disabled')>=0)return '对端未启用远程桌面（平台不支持或无可用显示）';
  if(s.indexOf('desktop_unsupported')>=0)return '对端系统暂不支持远程桌面';
  if(s.indexOf('unknown_tool')>=0)return '对端版本过旧，没有远程桌面功能';
  const dr=dropFromErr(s);if(dr)return dr;
  return s;
}
function deskBytesToBlob(b64){const bin=atob(b64);const u=new Uint8Array(bin.length);for(let i=0;i<bin.length;i++)u[i]=bin.charCodeAt(i);return new Blob([u],{type:'image/jpeg'})}
function deskSyncDisplays(n){
  n=deskInt(n,1,DESK_MAX_DISPLAYS,1);
  $('#deskDisp').classList.toggle('dk-hide',n<=1);
  if(n===desk.disp)return;desk.disp=n;
  const sel=$('#deskDisp'),cur=sel.value;sel.innerHTML='';
  for(let i=0;i<n;i++){const o=document.createElement('option');o.value=i;o.textContent='显示器 '+(i+1)+(i===0?'（主）':'');sel.appendChild(o)}
  if(cur&&+cur<n)sel.value=cur;
}
// 被控端返回的数据不可信（被控端可能被篡改或本身恶意）：所有来自 screen_capture 的字段在使用前
// 一律强制转成有界整数，之后统计栏、布局、画布、显示器列表都只用净化后的值，
// 避免 innerHTML/CSS 注入脚本，以及用极大的 displays/宽高卡死页面。
const DESK_MAX_DIM=16384,DESK_MAX_DISPLAYS=16;
function deskInt(v,min,max,dflt){const n=typeof v==='number'?v:(typeof v==='string'&&/^[0-9]{1,9}$/.test(v)?+v:NaN);return Number.isFinite(n)?Math.min(max,Math.max(min,Math.floor(n))):dflt}
function deskSanitize(d){
  if(!d||typeof d!=='object')return d;
  const o=Object.assign({},d); // 不改原对象
  o.width=deskInt(d.width,1,DESK_MAX_DIM,0);o.height=deskInt(d.height,1,DESK_MAX_DIM,0);
  o.screen_width=deskInt(d.screen_width,1,DESK_MAX_DIM,0);o.screen_height=deskInt(d.screen_height,1,DESK_MAX_DIM,0);
  o.displays=deskInt(d.displays,1,DESK_MAX_DISPLAYS,1);
  o.seq=deskInt(d.seq,0,Number.MAX_SAFE_INTEGER,0);
  o.proc_ms=(d.proc_ms==null)?null:deskInt(d.proc_ms,0,3600000,-1);
  o.unchanged=d.unchanged===true;
  o.data=typeof d.data==='string'?d.data:'';
  return o;
}
// 取一帧：返回 {r,t0,t1,key}，由调用方按顺序 await，保证画面按序绘制。
// key 标识这次请求的显示器/宽度/画质，用于判断「上一帧能否作为 since 复用」。
function deskFetch(args,sig){const t0=performance.now();const key=args.display+'/'+args.max_width+'/'+args.quality;return api('/api/call',{tool:'screen_capture',args},sig).then(r=>({r,t0,t1:performance.now(),key}))}
// 自适应策略（帧率选“自动”时）：
//  - 上限 30 fps；请求是「请求-应答」，帧率受往返时延限制，所以按需保持 1~3 个请求在途（流水线）：
//    在途数 = 往返时延 / 被控端单帧处理耗时（取整，上限 3），刚好让被控端一直有活干又不堆积；旧被控端没有处理耗时字段时用 2；
//  - 用「网络时延」(往返 - 被控端处理耗时 proc_ms) 的指数滑动平均(ema)判断链路好坏：>400ms 降画质到“低”，
//    >900ms 再把宽度压到 800 并收成单请求；恢复到 <150ms 后再升回用户选择的档位（留迟滞，避免来回抖动）。
//    被控端处理慢（抓屏/编码耗时）不是网络问题，降画质帮不上，所以不计入；旧被控端没有 proc_ms 时按整段往返计；
//  - 画面无变化不重复传：请求带上一帧的 seq 作 since，被控端发现画面和参数都没变就只回 {unchanged:true}，
//    前端跳过解码与绘制；静止一阵后降低请求频率（有鼠标键盘操作立即恢复），每 30 秒强制取一次完整帧兜底。
const DESK_KEYFRAME_MS=30000;
async function deskStart(forceLoading){
  if(desk.running)return;
  if(!connected){deskState('idle','尚未连接对端：请先在顶部输入协助码并点击「连接」');return}
  desk.running=true;const my=++desk.loopId;desk.frames=0;desk.drawn=0;desk.bytes=0;desk.t0=performance.now();
  if(forceLoading||$('#desk-view').dataset.s!=='live')deskState('loading','正在获取画面…');
  const cv=$('#deskCv'),ctx=cv.getContext('2d');
  const ac=new AbortController();desk.ac=ac;
  // ema：网络时延；pema：被控端处理耗时（-1 表示对端没有回传）；have：最近一次绘制的完整帧；still：连续“未变化”次数
  let ema=0,pema=-1,level=0,lastStart=0,inflight=[],fail=null,lastStat=0,still=0;
  const have={seq:0,key:'',at:0};
  const params=()=>{
    const wSel=+$('#deskW').value,qSel=+$('#deskQ').value;
    let w=wSel,q=qSel;
    // 画质“自动”：按链路档位取质量（好 70 / 慢 50 / 很慢 35），很慢时再把宽度压到 800
    if(!qSel){q=[70,50,35][level];if(level>=2)w=Math.min(w,800)}
    const a={display:+$('#deskDisp').value||0,max_width:w,quality:q};
    const key=a.display+'/'+a.max_width+'/'+a.quality;
    if(have.seq&&have.key===key&&performance.now()-have.at<DESK_KEYFRAME_MS)a.since=have.seq;
    return a;
  };
  const issue=()=>{lastStart=performance.now();inflight.push(deskFetch(params(),ac.signal))};
  const sleep=ms=>new Promise(res=>setTimeout(res,ms));
  while(my===desk.loopId&&connected&&activeTab==='desk'){
    const fpsSel=+$('#deskFps').value,auto=!fpsSel,anyAuto=auto||!+$('#deskQ').value;
    let interval=1000/(auto?30:fpsSel);
    // 静止且近期没有操作：放慢轮询，省被控端 CPU（每次轮询仍要抓屏+哈希）；一有操作或画面变化立即恢复
    const quiet=still>=8&&performance.now()-(desk.lastInput||0)>1500;
    if(quiet)interval=Math.max(interval,still>=40?200:100);
    let depth=1;
    if(auto&&!quiet&&level<2){depth=2;if(level===0&&pema>0)depth=Math.max(1,Math.min(3,Math.ceil((ema+pema)/pema)))}
    // 补足在途请求；相邻两次请求至少间隔 interval/depth，避免一次性打出两个相同的帧。
    // 已有在途请求时，等待间隔与“最早的请求返回”二者先到者处理，保证第 2、3 路真的能发出去。
    if(inflight.length<depth){
      const gap=interval/depth-(performance.now()-lastStart);
      if(gap<=0){issue();continue}
      if(!inflight.length){await sleep(gap);if(my!==desk.loopId)break;continue}
      const first=await Promise.race([inflight[0],sleep(gap).then(()=>null)]);
      if(my!==desk.loopId)break;
      if(first===null)continue;
    }
    if(!inflight.length)break;
    const {r,t0,t1,key}=await inflight.shift();
    if(my!==desk.loopId)break;
    const rtt=Math.round(t1-t0);
    if(!r.ok){fail=deskErrText(r.error);break}
    let d;try{d=deskSanitize(parseMCP(r.result))}catch(e){fail='解析帧失败';break}
    const unchanged=!!(d&&d.unchanged&&!d.data&&have.seq);
    if(!d||(!d.data&&!unchanged)){fail=deskErrText(JSON.stringify(d||{}));break}
    const proc=(d.proc_ms!=null&&d.proc_ms>=0)?d.proc_ms:-1;
    const net=proc>=0?Math.max(0,rtt-proc):rtt;
    if(proc>=0)pema=pema<0?proc:pema*0.8+proc*0.2;
    if(anyAuto){
      ema=ema?ema*0.8+net*0.2:net;
      if(ema>900)level=2;else if(ema>400)level=Math.max(level,1);else if(ema<150)level=0;
      else if(ema<300&&level===2)level=1;
    }else{ema=net;level=0}
    if(unchanged){still++}
    else{
      still=0;
      try{
        const bm=await createImageBitmap(deskBytesToBlob(d.data));
        if(d.width>0&&d.height>0&&(cv.width!==d.width||cv.height!==d.height)){cv.width=d.width;cv.height=d.height}
        // 显示大小由真实屏幕尺寸决定，与返回图像的像素尺寸（自动画质会改宽度）无关，画面不会忽大忽小
        if(d.screen_width>0&&d.screen_height>0&&(desk.sw!==d.screen_width||desk.sh!==d.screen_height)){desk.sw=d.screen_width;desk.sh=d.screen_height;deskLayout()}
        ctx.drawImage(bm,0,0);if(bm.close)bm.close();
      }catch(e){fail='绘制帧失败: '+e;break}
      // 旧被控端不回 seq：have.seq 保持 0，永远不会带 since，行为与改造前一致
      if(d.seq){have.seq=d.seq;have.key=key;have.at=performance.now()}
      desk.drawn++;
    }
    if($('#desk-view').dataset.s!=='live')deskState('live','');
    $('#deskPillTxt').innerHTML='已连接 · <span class="dk-num" style="min-width:3ch">'+Math.round(rtt)+'</span> ms';
    deskSyncDisplays(d.displays||1);
    desk.frames++;desk.bytes+=(d.data?d.data.length*0.75:0);
    const now=performance.now(),sec=(now-desk.t0)/1000;
    // 统计口径：fps = 画面实际更新的帧率（静止时为 0，并标注“静止”）；悬停提示里另给请求率。KB/s 为画面数据的网速。
    if(now-lastStat>=500&&sec>0){
      lastStat=now;
      const fpsT=(desk.drawn/sec).toFixed(1),reqT=(desk.frames/sec).toFixed(1),bps=desk.bytes/sec;
      const spT=bps>=1048576?(bps/1048576).toFixed(1):(bps/1024).toFixed(0),spU=bps>=1048576?'MB/s':'KB/s';
      const lowT=anyAuto&&level?' · 已降质':'',stillT=still>=5?' · 静止':'';
      const sw=deskInt(d.screen_width||desk.sw,0,DESK_MAX_DIM,0),sh=deskInt(d.screen_height||desk.sh,0,DESK_MAX_DIM,0);
      const st=fpsT+' fps · '+spT+' '+spU+' · '+sw+'x'+sh+stillT+lowT;
      // 数值放进定宽等宽数字容器，位数变化时整条文字不抖动（e2e 读 textContent 仍含 fps）
      $('#deskStat').innerHTML='<span class="dk-num" style="min-width:4ch">'+fpsT+'</span>fps · <span class="dk-num" style="min-width:4ch">'+spT+'</span>'+spU+' · '+sw+'x'+sh+stillT+lowT;
      $('#deskPill').title=st+'\n画面更新 '+fpsT+' 帧/秒，请求 '+reqT+' 次/秒；往返 '+rtt+' ms'+(proc>=0?'（被控端处理 '+proc+' ms，网络 '+net+' ms）':'');
      if(sec>=5){desk.frames=0;desk.drawn=0;desk.bytes=0;desk.t0=now} // 滚动窗口，反映最近的帧率
    }
  }
  ac.abort();
  if(desk.ac===ac)desk.ac=null;
  if(my===desk.loopId){desk.running=false;if(fail)deskState('error',fail);else if(!connected)deskState('idle',dropInfo?'连接已断开：'+dropInfo.text+'。'+dropInfo.hint:'连接已断开，请重新连接对端')}
}
function deskAbort(){
  if(desk.ac){
    desk.ac.abort();desk.ac=null;desk.loopId++;desk.running=false;
    // 离开桌面页后画布上还留着最后一帧；再次打开时若直接当成实时画面显示会误导用户（看起来“秒开”）。
    // 这里清空并回到“加载中”，等真正的新帧到达后才显示画面。
    const cv=$('#deskCv');if(cv){const c=cv.getContext('2d');c.clearRect(0,0,cv.width,cv.height)}
    deskState('loading','正在获取画面…');
  }
}
// 画面显示尺寸：适应窗口=按屏幕宽高比缩放到视口内；原始大小=1 屏幕像素对应 1 CSS 像素（可滚动）
function deskLayout(){
  const view=$('#desk-view'),cv=$('#deskCv');
  if(!desk.sw||!desk.sh||!view)return;
  if(view.classList.contains('actual')){cv.style.width=desk.sw+'px';cv.style.height=desk.sh+'px';return}
  const cs=getComputedStyle(view),aw=view.clientWidth-parseFloat(cs.paddingLeft)-parseFloat(cs.paddingRight),ah=view.clientHeight-parseFloat(cs.paddingTop)-parseFloat(cs.paddingBottom);
  if(aw<=0||ah<=0)return;
  const k=Math.min(aw/desk.sw,ah/desk.sh,2);
  cv.style.width=Math.max(1,Math.floor(desk.sw*k))+'px';cv.style.height=Math.max(1,Math.floor(desk.sh*k))+'px';
}
function deskPos(e){const cv=$('#deskCv'),r=cv.getBoundingClientRect();return{x:Math.min(1,Math.max(0,(e.clientX-r.left)/r.width)),y:Math.min(1,Math.max(0,(e.clientY-r.top)/r.height))}}
function deskBtn(e){return e.button===2?'right':(e.button===1?'middle':'left')}
function deskPush(ev){
  if(!connected||!$('#deskCtl').checked)return;
  desk.lastInput=performance.now(); // 有操作：帧循环退出“静止降频”
  const q=desk.queue;
  if(ev.type==='move'&&q.length&&q[q.length-1].type==='move')q[q.length-1]=ev;else q.push(ev);
  deskFlush();
}
async function deskFlush(){
  if(desk.sending)return;desk.sending=true;
  try{
    while(desk.queue.length){
      const evs=desk.queue.splice(0,100);
      const r=await api('/api/call',{tool:'input',args:{display:+$('#deskDisp').value||0,events:evs}});
      if(!r.ok){deskToast(deskErrText(r.error));desk.queue=desk.queue.filter(x=>x.type==='up'||x.type==='key_up');break}
    }
  }finally{desk.sending=false}
}
(function(){
  const cv=$('#deskCv'),kb=$('#deskKb');
  cv.addEventListener('mousemove',e=>{const p=deskPos(e);deskPush({type:'move',x:p.x,y:p.y})});
  cv.addEventListener('mousedown',e=>{kb.focus();e.preventDefault();(desk.downBtns=desk.downBtns||new Set()).add(e.button);const p=deskPos(e);deskPush({type:'down',x:p.x,y:p.y,button:deskBtn(e)})});
  const upHandler=e=>{e.preventDefault();const p=deskPos(e);deskPush({type:'up',x:p.x,y:p.y,button:deskBtn(e)})};
  window.addEventListener('mouseup',e=>{if(desk.downBtns&&desk.downBtns.has(e.button)){desk.downBtns.delete(e.button);upHandler(e)}});
  cv.addEventListener('contextmenu',e=>e.preventDefault());
  cv.addEventListener('wheel',e=>{e.preventDefault();const p=deskPos(e);const dy=Math.max(-1200,Math.min(1200,Math.round(e.deltaY*1.2)));if(dy)deskPush({type:'wheel',x:p.x,y:p.y,dy:dy})},{passive:false});
  kb.addEventListener('keydown',e=>{if(e.isComposing||e.key==='Process')return;
    if(!$('#deskCtl').checked)return;
    e.preventDefault();
    // 可打印字符且没按 Ctrl/Alt/Meta：直接发文本，避开键盘布局/Shift 状态差异
    if(e.key.length===1&&!e.ctrlKey&&!e.altKey&&!e.metaKey){deskPush({type:'text',text:e.key});return}
    desk.pressed.add(e.key);deskPush({type:'key_down',key:e.key});
  });
  kb.addEventListener('keyup',e=>{
    if(!$('#deskCtl').checked)return;
    e.preventDefault();
    if(!desk.pressed.has(e.key))return;
    desk.pressed.delete(e.key);deskPush({type:'key_up',key:e.key});
  });
  // 失焦时把还按着的键都抬起，防止对端卡键（比如按着 Ctrl 切走焦点）
  kb.addEventListener('blur',()=>{for(const k of desk.pressed)deskPush({type:'key_up',key:k});desk.pressed.clear()});
  // 输入法/非键盘来源的文本：隐藏 textarea 接收 composition 与 input，提交后转成 text 事件并清空。
  // Chrome 的最终 input(insertCompositionText) 先于 compositionend，Firefox 相反，所以 input 里一律忽略合成类事件，只在 compositionend 发送，避免重复。
  kb.addEventListener('compositionend',e=>{if(e.data&&$('#deskCtl').checked)deskPush({type:'text',text:e.data});kb.value=''});
  kb.addEventListener('input',e=>{if(e.isComposing||e.inputType==='insertCompositionText')return;const t=e.data||'';if(t&&$('#deskCtl').checked)deskPush({type:'text',text:t});kb.value=''});
})();
// 工具栏交互：分段按钮（同步底层 select）、适应/原始大小、全屏、只看不控
(function(){
  const view=$('#desk-view');
  document.querySelectorAll('#desk-bar .dk-seg').forEach(seg=>{
    const sel=$('#'+seg.dataset.for);
    const sync=()=>seg.querySelectorAll('button').forEach(b=>b.setAttribute('aria-pressed',String(b.dataset.v===sel.value)));
    for(const o of sel.options){
      const b=document.createElement('button');b.type='button';b.dataset.v=o.value;b.textContent=o.textContent;
      b.onclick=()=>{sel.value=o.value;sel.dispatchEvent(new Event('change'));sync()};
      seg.appendChild(b);
    }
    sel.addEventListener('change',sync);sync();
  });
  $('#deskDisp').classList.add('dk-hide');
  const fit=$('#deskFit');
  fit.onclick=()=>{
    const actual=!view.classList.contains('actual');
    view.classList.toggle('actual',actual);
    fit.setAttribute('aria-pressed',String(actual));
    $('#deskFitTxt').textContent=actual?'原始大小':'适应窗口';
    deskLayout();
  };
  if(window.ResizeObserver)new ResizeObserver(()=>deskLayout()).observe(view);else window.addEventListener('resize',deskLayout);
  const full=$('#deskFull');
  if(!view.requestFullscreen)full.style.display='none';
  full.onclick=()=>{if(document.fullscreenElement)document.exitFullscreen();else view.requestFullscreen().catch(e=>deskToast('无法进入全屏: '+e))};
  document.addEventListener('fullscreenchange',()=>{full.lastChild.textContent=document.fullscreenElement===view?'退出全屏':'全屏'});
  const ctl=$('#deskCtl');
  const syncCtl=()=>view.classList.toggle('ro',!ctl.checked);
  ctl.addEventListener('change',syncCtl);syncCtl();
})();

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
<link rel="stylesheet" href="/assets/pty/pty_term.css">
<script src="/assets/pty/pty_term.js"></script>
</body>
</html>`)
