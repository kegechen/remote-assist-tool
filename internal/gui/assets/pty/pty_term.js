/* 真终端（PTY + xterm.js）。
 * 由主页面在 </body> 前以 <script src="/assets/pty/pty_term.js"> 引入；对外只暴露 window.PtyTerm：
 *   connected()    远端连接成功（重置回退状态，若当前是终端页则自动开会话）
 *   disconnected() 主动断开：关闭会话
 *   show()         终端页被激活；返回 true 表示由 xterm 接管（调用方不必再聚焦旧输入框）
 * 对端不支持 pty（unknown_tool 等）时自动退回旧的 exec 命令行模式：本脚本只切换 CSS 类，
 * 旧终端的 DOM 与代码路径原样保留。 */
(function () {
  'use strict';
  var BASE = '/assets/xterm/';
  var host = null, term = null, fit = null, libsPromise = null;
  var isConnected = false;
  var mode = '';            // '' 未尝试 | 'pty' | 'exec'（已回退）
  var state = 'idle';       // idle | opening | live | ended
  var sessionId = '', abortCtl = null, gotOutput = false, gen = 0;
  var token = new URLSearchParams(location.search).get('token') || '';
  var sendQ = [], flushing = false, flushTimer = 0, sessionWaiters = [];
  var resizeTimer = 0, lastSize = '';
  var FLUSH_MS = 6;         // 键入合并窗口（5~10ms）
  var enc = new TextEncoder();

  function $(s) { return document.querySelector(s); }
  function termBox() { return $('#term-box'); }
  function termVisible() { var b = termBox(); return !!b && b.style.display !== 'none' && b.style.display !== ''; }

  function post(path, body, signal) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Auth-Token': token },
      body: JSON.stringify(body || {}),
      signal: signal
    });
  }
  function b64ToBytes(b64) {
    var s = atob(b64), a = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) a[i] = s.charCodeAt(i);
    return a;
  }
  function bytesToB64(u8) {
    var s = '';
    for (var i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000));
    return btoa(s);
  }

  // ---------- 资源加载（本地内嵌，离线可用） ----------
  function loadScript(src) {
    return new Promise(function (res, rej) {
      var s = document.createElement('script');
      s.src = src; s.onload = res; s.onerror = function () { rej(new Error('加载失败: ' + src)); };
      document.head.appendChild(s);
    });
  }
  function loadLibs() {
    if (libsPromise) return libsPromise;
    var l = document.createElement('link');
    l.rel = 'stylesheet'; l.href = BASE + 'xterm.css'; document.head.appendChild(l);
    libsPromise = loadScript(BASE + 'xterm.js').then(function () { return loadScript(BASE + 'addon-fit.js'); });
    libsPromise.catch(function () { libsPromise = null; });
    return libsPromise;
  }

  // ---------- 主题：读 CSS 变量，没有则用深色默认值 ----------
  function cssVar(name, dflt) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    v = v && v.trim();
    return v || dflt;
  }
  function currentTheme() {
    return {
      background: cssVar('--term-bg', '#0c0d1b'),
      foreground: cssVar('--term-fg', '#e3e6f7'),
      cursor: cssVar('--term-cursor', '#a6e3a1'),
      cursorAccent: cssVar('--term-bg', '#0c0d1b'),
      selectionBackground: cssVar('--term-selection', 'rgba(137,180,250,0.35)')
    };
  }
  function applyTheme() {
    if (!term) return;
    var t = currentTheme();
    term.options.theme = t;
    if (host) host.style.background = t.background;
  }

  // ---------- 创建 xterm ----------
  function ensureTerm() {
    if (term) return Promise.resolve();
    return loadLibs().then(function () {
      if (term) return;
      var box = termBox();
      host = document.createElement('div');
      host.id = 'xterm-host';
      box.appendChild(host);
      var mono = cssVar('--mono', 'Consolas, "Microsoft YaHei UI", monospace');
      term = new window.Terminal({
        fontFamily: mono, fontSize: 13, lineHeight: 1.15, cursorBlink: true,
        scrollback: 5000, allowProposedApi: true, theme: currentTheme(), convertEol: false
      });
      fit = new window.FitAddon.FitAddon();
      term.loadAddon(fit);
      term.open(host);
      host.style.background = currentTheme().background;
      term.attachCustomKeyEventHandler(keyHandler);
      term.onData(function (d) { onInput(enc.encode(d)); });
      term.onBinary(function (d) {
        var u = new Uint8Array(d.length);
        for (var i = 0; i < d.length; i++) u[i] = d.charCodeAt(i) & 255;
        onInput(u);
      });
      term.onResize(function (sz) { scheduleResize(sz.cols, sz.rows); });
      if (window.ResizeObserver) new ResizeObserver(doFit).observe(host);
      window.addEventListener('resize', doFit);
      if (window.matchMedia) {
        var mq = window.matchMedia('(prefers-color-scheme: light)');
        (mq.addEventListener ? mq.addEventListener.bind(mq, 'change') : mq.addListener.bind(mq))(applyTheme);
      }
      new MutationObserver(applyTheme).observe(document.documentElement, { attributes: true, attributeFilter: ['class', 'style', 'data-theme'] });
    });
  }

  // 键盘：Ctrl+C 默认就是真实中断（直接透传）；仅在有选区时才拦下来做复制。
  // Ctrl+Shift+C / Ctrl+Shift+V 作为显式复制粘贴，不发给远端。
  function keyHandler(ev) {
    if (ev.type !== 'keydown') return true;
    var k = (ev.key || '').toLowerCase();
    if (ev.ctrlKey && !ev.altKey && !ev.metaKey) {
      if (k === 'c' && (ev.shiftKey || term.hasSelection())) {
        // 复制后清掉选区：否则选区一直残留，之后每次 Ctrl+C 都被当成“复制”，看起来像没反应
        if (term.hasSelection()) { copyText(term.getSelection()); term.clearSelection(); }
        return false;
      }
      if (k === 'v' && ev.shiftKey) return false; // 让浏览器触发 paste 事件，由 xterm 处理
    }
    return true;
  }
  function copyText(t) {
    try { navigator.clipboard.writeText(t); return; } catch (e) { /* 回退 */ }
    var ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch (e) { /* ignore */ }
    ta.remove(); if (term) term.focus();
  }

  function doFit() {
    if (!term || !fit || !host || host.clientWidth < 20 || host.clientHeight < 20 || !termVisible()) return;
    try { fit.fit(); } catch (e) { /* 不可见时可能抛错 */ }
  }

  // ---------- resize 同步 ----------
  function scheduleResize(cols, rows) {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () {
      var key = cols + 'x' + rows;
      if (!sessionId || state !== 'live' || key === lastSize) return;
      lastSize = key;
      post('/api/pty/resize', { id: sessionId, cols: cols, rows: rows }).catch(function () {});
    }, 80);
  }

  // ---------- 输入：5~10ms 合并后串行发送 ----------
  function onInput(bytes) {
    if (state === 'ended') {
      // 会话结束后按回车重新开始，其余按键忽略
      for (var i = 0; i < bytes.length; i++) if (bytes[i] === 13) { startSession(); return; }
      return;
    }
    if (state !== 'live' && state !== 'opening') return;
    sendQ.push(bytes);
    if (!flushing && !flushTimer) flushTimer = setTimeout(flush, FLUSH_MS);
  }
  function waitSession() {
    if (sessionId) return Promise.resolve(true);
    return new Promise(function (res) { sessionWaiters.push(res); });
  }
  async function flush() {
    flushTimer = 0;
    if (flushing) return;
    flushing = true;
    var myGen = gen;
    try {
      while (sendQ.length) {
        if (!(await waitSession()) || myGen !== gen) { sendQ = []; break; }
        var total = 0, i;
        for (i = 0; i < sendQ.length; i++) total += sendQ[i].length;
        var buf = new Uint8Array(total), off = 0;
        for (i = 0; i < sendQ.length; i++) { buf.set(sendQ[i], off); off += sendQ[i].length; }
        sendQ = [];
        try {
          await post('/api/pty/write', { id: sessionId, data: bytesToB64(buf) });
        } catch (e) { /* 网络抖动：这批输入丢弃，流若已断会走结束流程 */ }
      }
    } finally { flushing = false; }
  }

  // ---------- 会话 ----------
  function notice(msg, cls) {
    if (!term) return;
    term.write('\r\n\x1b[' + (cls || '33') + 'm' + msg + '\x1b[0m\r\n');
  }
  // 把底层错误翻译成对用户有意义的回退原因（不再笼统说“无 PTY”）
  function fallbackReason(msg) {
    msg = msg || '';
    if (/unknown_tool/.test(msg)) return '对端版本过旧，不支持真终端（缺少 pty_open 工具）。请升级被控端到最新版后重新连接；当前使用命令行模式，不支持 vim/top 等全屏程序。';
    if (/exec_denied/.test(msg)) return '被控端设置了 --allow-exec 命令白名单，已禁用真终端（shell 可绕过白名单）。如确需真终端，请让被控端加 --allow-pty 重启；当前使用命令行模式，不支持 vim/top 等全屏程序。';
    if (/pty_unsupported/.test(msg)) return '被控端系统不支持 ConPTY（需要 Windows 10 1809 或更新版本）；当前使用命令行模式，不支持 vim/top 等全屏程序。';
    if (/找不到可用的 shell|启动 shell 失败/.test(msg)) return '被控端无法启动 shell（' + msg + '）；当前使用命令行模式，不支持 vim/top 等全屏程序。';
    return '真终端不可用（' + msg + '）；当前使用命令行模式，不支持 vim/top 等全屏程序。';
  }
  function useExecMode(reason) {
    mode = 'exec';
    state = 'idle';
    var b = termBox();
    if (b) b.classList.remove('pty-on');
    var note = $('#termNote');
    if (note) { note.textContent = '命令行模式：' + (reason || '').replace(/[；;。].*$/, ''); note.title = reason || ''; }
    if (typeof termLog === 'function') {
      termLog('<span class="err">' + String(reason || '对端不支持真终端，已回退到命令行模式').replace(/[&<>]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c]; }) + '</span>');
    }
    var inp = $('#termInput'); if (inp && termVisible()) inp.focus();
  }
  function shouldFallback(msg) {
    return /unknown_tool|pty_unsupported|exec_denied|找不到可用的 shell|启动 shell 失败/.test(msg || '');
  }

  async function startSession() {
    if (mode === 'exec' || state === 'opening' || state === 'live') return;
    if (!isConnected) return;
    var myGen = ++gen;
    state = 'opening'; gotOutput = false; sessionId = ''; lastSize = '';
    sendQ = [];
    try { await ensureTerm(); } catch (e) { useExecMode('终端组件加载失败，已回退到命令行模式: ' + e.message); return; }
    if (myGen !== gen) return;
    var b = termBox(); if (b) b.classList.add('pty-on');
    term.reset();
    doFit();
    var ctl = abortCtl = new AbortController();
    var cols = term.cols, rows = term.rows;
    lastSize = cols + 'x' + rows;
    if (termVisible()) term.focus();
    var errText = '', gotResult = false;
    try {
      var resp = await post('/api/pty/open', { cols: cols, rows: rows }, ctl.signal);
      if (!resp.ok) {
        var t = ''; try { t = await resp.text(); } catch (e) { /* ignore */ }
        throw new Error(t || ('HTTP ' + resp.status));
      }
      var reader = resp.body.getReader(), dec = new TextDecoder(), buf = '';
      for (;;) {
        var r = await reader.read();
        if (r.done) break;
        buf += dec.decode(r.value, { stream: true });
        var nl;
        while ((nl = buf.indexOf('\n')) >= 0) {
          var line = buf.slice(0, nl); buf = buf.slice(nl + 1);
          if (!line) continue;
          var m; try { m = JSON.parse(line); } catch (e) { continue; }
          if (myGen !== gen) return;
          if (m.type === 'session') {
            sessionId = m.id; state = 'live'; mode = 'pty';
            sessionWaiters.splice(0).forEach(function (f) { f(true); });
            if (sendQ.length && !flushing && !flushTimer) flushTimer = setTimeout(flush, FLUSH_MS);
          } else if (m.type === 'chunk') {
            gotOutput = true;
            term.write(b64ToBytes(m.data));
          } else if (m.type === 'result') {
            gotResult = true;
          } else if (m.type === 'error') {
            errText = m.error || '未知错误';
          }
        }
      }
    } catch (e) {
      if (e.name === 'AbortError' || myGen !== gen) { return; }
      errText = errText || String(e.message || e);
    }
    if (myGen !== gen) return;
    sessionId = '';
    sessionWaiters.splice(0).forEach(function (f) { f(false); });
    if (!gotOutput && errText && shouldFallback(errText)) {
      useExecMode(fallbackReason(errText));
      return;
    }
    state = 'ended';
    if (errText) notice('[会话中断: ' + errText + '；按回车重新开始]', '31');
    else notice('[进程已退出，按回车重新开始]', '33');
    if (termVisible()) term.focus();
    void gotResult;
  }

  function stopSession(why) {
    gen++;
    var id = sessionId;
    if (abortCtl) { try { abortCtl.abort(); } catch (e) { /* ignore */ } abortCtl = null; }
    if (id) post('/api/pty/close', { id: id }).catch(function () {});
    sessionId = ''; sendQ = [];
    sessionWaiters.splice(0).forEach(function (f) { f(false); });
    state = 'idle';
    if (why && term) notice(why, '2');
  }

  // ---------- 对外接口 ----------
  window.PtyTerm = {
    connected: function () {
      isConnected = true;
      if (mode === 'exec') {
        mode = ''; // 换了对端/重连：重新探测
        var n = $('#termNote'); if (n) { n.textContent = ''; n.title = ''; }
      }
      if (termVisible()) startSession();
    },
    disconnected: function () {
      isConnected = false;
      stopSession('[已断开连接]');
    },
    show: function () {
      if (mode === 'exec') return false;
      if (isConnected && (state === 'idle')) startSession();
      if (term && state !== 'idle') { doFit(); term.focus(); }
      return mode !== 'exec';
    },
    _fallbackReason: fallbackReason, // 供测试核对各回退原因文案
    _state: function () { return { mode: mode, state: state, id: sessionId, cols: term ? term.cols : 0, rows: term ? term.rows : 0 }; }
  };

  // 与主页面的 SSE 连接状态联动（自动重连后新开会话）
  function hookEvents() {
    var src = (typeof es !== 'undefined') ? es : null;
    if (!src) return;
    src.addEventListener('connected', function () { window.PtyTerm.connected(); });
    src.addEventListener('lost', function () { isConnected = false; });
    src.addEventListener('disconnected', function () { isConnected = false; });
  }
  try { hookEvents(); } catch (e) { /* 主页面变量不可见时忽略 */ }

  // 页面关闭：流中断即触发服务端取消并清理远端进程树，这里再尽力发一次 close
  window.addEventListener('pagehide', function () {
    if (sessionId) { try { fetch('/api/pty/close', { method: 'POST', keepalive: true, headers: { 'Content-Type': 'application/json', 'X-Auth-Token': token }, body: JSON.stringify({ id: sessionId }) }); } catch (e) { /* ignore */ } }
  });

  // 主页面在本脚本加载前就执行过 openTerm()，这里补一次：若已连接且终端页可见则自动开会话
  setTimeout(function () {
    fetch('/api/status', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Auth-Token': token }, body: '{}' })
      .then(function (r) { return r.json(); })
      .then(function (d) { if (d && (d.connected || (d.status && d.status.connected))) window.PtyTerm.connected(); })
      .catch(function () {});
  }, 0);
})();
