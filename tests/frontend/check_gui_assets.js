// GUI 前端（internal/gui/assets/assets.go 里内嵌的 HTML/JS）的自动检查。
//
// 为什么需要它：那段 JS 是 Go 源码里的一个字符串常量，go build / go vet / go test
// 一律看不见它。语法错、拿路径当 CSS 选择器、base64 编码写错——这些只有在浏览器里
// 点到才会暴露。这个脚本把最容易静默回归的几条钉住。
//
// 无 node 时 run_tests 会跳过本检查，因此不给这个纯 Go 项目引入硬依赖。
//
// 跑法：node tests/frontend/check_gui_assets.js

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ASSETS = path.join(__dirname, '..', '..', 'internal', 'gui', 'assets', 'assets.go');
const src = fs.readFileSync(ASSETS, 'utf8');
const htmlMatch = src.match(/var IndexHTML = \[\]byte\(`([\s\S]*)`\)/);
if (!htmlMatch) {
  console.error('FAIL: 没能从 assets.go 里取出 IndexHTML');
  process.exit(1);
}
const html = htmlMatch[1];
const jsMatch = html.match(/<script>([\s\S]*?)<\/script>/);
if (!jsMatch) {
  console.error('FAIL: IndexHTML 里没有 <script> 块');
  process.exit(1);
}
const js = jsMatch[1];

let failed = 0;
const check = (name, ok, detail) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok || !detail ? '' : ' -> ' + detail}`);
  if (!ok) failed++;
};

// ---------- 1. 语法 ----------
try {
  new vm.Script(js, { filename: 'assets-inline.js' });
  check('JS 语法', true);
} catch (e) {
  check('JS 语法', false, e.message);
  process.exit(1); // 语法都不过，后面没必要跑
}

// ---------- 2. DOM stub ----------
// querySelector 必须复刻真实浏览器的两个行为，否则测不出原始 bug：
//   - 非法选择器（含 : \ / 的路径）抛 SyntaxError
//   - 元素不存在返回 null（调用方随后 .style 会 TypeError）
const realIds = new Set([...html.matchAll(/id="([a-zA-Z][\w-]*)"/g)].map((m) => m[1]));
const VALID_IDENT = /^-?[_a-zA-Z][_a-zA-Z0-9-]*$/;

function mkEl(id) {
  const cls = new Set();
  return {
    id, style: {}, dataset: {}, children: [], _text: '', _html: '', _h: {},
    classList: { add: (c) => cls.add(c), remove: (c) => cls.delete(c), contains: (c) => cls.has(c) },
    set textContent(v) { this._text = String(v); },
    get textContent() { return this._text; },
    // 真实 DOM 里 innerHTML='' 会连子节点一起清掉，stub 也照做，否则 renderTabs 重绘后
    // children 会一直累积，数 tab 的断言就没意义了
    set innerHTML(v) { this._html = String(v); if (v === '') this.children = []; },
    get innerHTML() { return this._html; },
    appendChild(c) { this.children.push(c); return c; },
    remove() {}, focus() {}, click() {},
    // 记下 listener 以便测试里手动触发（拖拽这类交互只能这么测）
    addEventListener(type, fn) { (this._h[type] || (this._h[type] = [])).push(fn); },
    _fire(type, ev) { (this._h[type] || []).forEach((f) => f(ev)); },
    setPointerCapture() {}, releasePointerCapture() {},
    getBoundingClientRect: () => ({ left: 0, top: 0, width: 260, height: 600 }),
    scrollTop: 0, scrollHeight: 0, value: '', disabled: false,
  };
}
const els = new Map([...realIds].map((id) => [id, mkEl(id)]));
const document = {
  querySelector(sel) {
    if (!sel.startsWith('#')) return mkEl('anon');
    const ident = sel.slice(1);
    if (!VALID_IDENT.test(ident)) {
      const e = new Error(`'${sel}' is not a valid selector`);
      e.name = 'SyntaxError';
      throw e;
    }
    return els.get(ident) || null;
  },
  createElement: () => mkEl('created'),
  addEventListener() {},
  body: mkEl('body'),
};
const ctx = {
  document, console,
  EventSource: function () { return { addEventListener() {}, onmessage: null, onerror: null }; },
  fetch: async () => ({ json: async () => ({ ok: true, result: {} }) }),
  setTimeout, clearTimeout, AbortController, TextEncoder, TextDecoder, btoa, atob, URLSearchParams,
  location: { search: '?token=test-token-123', href: 'http://127.0.0.1:8731/?token=test-token-123' },
  URL: { createObjectURL: () => 'blob:x', revokeObjectURL() {} },
  Blob: function () {},
  window: { innerWidth: 1280, innerHeight: 800 },
};
ctx.globalThis = ctx;
vm.createContext(ctx);
try {
  vm.runInContext(js, ctx, { filename: 'assets-inline.js' });
  check('加载并初始化', true);
} catch (e) {
  check('加载并初始化', false, `${e.name}: ${e.message}`);
  process.exit(1);
}

// ---------- 3. tab 与 DOM 元素解耦 ----------
// tab id 含远端路径（C:\... 或 /var/...）。曾把 tab id 直接当成 DOM 元素 id 用，
// 导致点开任何文件都抛异常。
for (const id of ['file:C:\\Windows\\System32\\drivers\\etc\\hosts', 'file:/var/log/syslog', 'dir:C:\\temp']) {
  try {
    ctx.openTab(id, 'x', 'file-box', () => {});
    check(`openTab(${JSON.stringify(id)}) 显示 file-box`, els.get('file-box').style.display === 'flex');
  } catch (e) {
    check(`openTab(${JSON.stringify(id)})`, false, `${e.name}: ${e.message}`);
  }
}
try {
  ctx.openTerm();
  check('切到终端后隐藏其它 box',
    els.get('term-box').style.display === 'flex' && els.get('file-box').style.display === 'none');
} catch (e) {
  check('切到终端', false, e.message);
}

// ---------- 4. base64 编码 ----------
// write_file 的 content 是 base64。发明文时，恰好落在 base64 字母表且长度为 4 倍数
// 的短内容会被静默解成垃圾字节写进远端文件（"test" -> b5 eb 2d）。
const b64ok = (label, got, want) => check(`b64 ${label}`, got === want, `得到 ${got}，期望 ${want}`);
b64ok('ASCII', ctx.b64FromText('hello world'), 'aGVsbG8gd29ybGQ=');
b64ok('4 字节陷阱内容', ctx.b64FromText('test'), 'dGVzdA==');
b64ok('中文', ctx.b64FromText('中文内容'), Buffer.from('中文内容', 'utf8').toString('base64'));
b64ok('空串', ctx.b64FromText(''), '');
const bin = new Uint8Array([0, 1, 2, 255, 254, 65]);
b64ok('二进制字节', ctx.b64FromBytes(bin), Buffer.from(bin).toString('base64'));
const big = new Uint8Array(300000).fill(88); // 分块编码，不能爆栈
try {
  b64ok('300KB 不爆栈', ctx.b64FromBytes(big), Buffer.from(big).toString('base64'));
} catch (e) {
  check('b64 300KB 不爆栈', false, e.message);
}

// ---------- 5. 终端流式输出的解码 ----------
// 远端控制台不一定说 UTF-8：中文 Windows 上 ping 吐的是 GBK，按 UTF-8 解就是 "���� Ping"。
// 更阴的是跨块切断：GBK 的 0xD5 恰好是合法的 UTF-8 起始字节，用 fatal TextDecoder 试探时
// 它会被吞进解码器内部缓冲，等下一块抛错才改判 GBK，那几个字节已经找不回来——中文首字
// 被静默吃掉。所以这里逐条钉住，包括"每次只喂 1 字节"这种最恶劣的切法。
const GBK_ZHENGZAI = [0xd5, 0xfd, 0xd4, 0xda]; // "正在"
const decEq = (label, got, want) => check(`decode ${label}`, got === want, `得到 ${JSON.stringify(got)}，期望 ${JSON.stringify(want)}`);
// 一次喂完
decEq('GBK 整块', ctx.makeDecoder()(Uint8Array.from([...GBK_ZHENGZAI, 0x20, 0x50, 0x69, 0x6e, 0x67])), '正在 Ping');
decEq('UTF-8 整块', ctx.makeDecoder()(Buffer.from('你好世界', 'utf8')), '你好世界');
decEq('纯 ASCII', ctx.makeDecoder()(Buffer.from('hello\r\n', 'ascii')), 'hello\r\n');
// 跨块切断
{
  const d = ctx.makeDecoder(), b = Buffer.from('你好', 'utf8');
  decEq('UTF-8 跨块切断', d(b.subarray(0, 2)) + d(b.subarray(2)), '你好');
}
{
  const d = ctx.makeDecoder(), b = Uint8Array.from(GBK_ZHENGZAI);
  decEq('GBK 跨块切断', d(b.subarray(0, 1)) + d(b.subarray(1)), '正在');
}
// 先 ASCII 后中文：不能因为开头是合法 UTF-8 就把整条流锁死在 UTF-8
{
  const d = ctx.makeDecoder();
  decEq('ASCII 后接 GBK', d(Buffer.from('Reply from ', 'ascii')) + d(Uint8Array.from(GBK_ZHENGZAI)), 'Reply from 正在');
}
// 逐字节喂（最恶劣切法）
for (const [label, bytes, want] of [
  ['UTF-8 逐字节', Buffer.from('你好世界', 'utf8'), '你好世界'],
  ['GBK 逐字节', Uint8Array.from([...GBK_ZHENGZAI, ...GBK_ZHENGZAI]), '正在正在'],
]) {
  const d = ctx.makeDecoder();
  let s = '';
  for (let i = 0; i < bytes.length; i++) s += d(bytes.subarray(i, i + 1));
  decEq(label, s, want);
}

// ---------- 6. 面板滚动 ----------
// flex 子项默认 min-height:auto = "不许缩到比内容矮"，于是输出区把自己撑高、被
// .tabcontent 的 overflow:hidden 裁掉：没有滚动条且内容被截断。overflow:auto 必须
// 配 min-height:0 才生效。
const cssMatch = html.match(/<style>([\s\S]*?)<\/style>/);
const css = cssMatch ? cssMatch[1] : '';
for (const sel of ['#term-output', '#file-content', '#search-output', '#proc-output', '#term-box', '#file-box', '#search-box', '#proc-box']) {
  const rule = css.match(new RegExp(sel.replace(/[#]/g, '\\$&') + '\\{([^}]*)\\}'));
  check(`${sel} 可滚动(min-height:0)`, !!rule && /min-height:0/.test(rule[1]),
    rule ? `规则里没有 min-height:0: ${rule[1]}` : '找不到该 CSS 规则');
}

// ---------- 7. 目录树宽度拖拽 ----------
// 这里原本只有 CSS、没有任何 JS：分隔条拖不动，且横拖会把界面文字选中。
{
  const rz = els.get('resizer'), panel = els.get('tree-panel'), body = document.body;
  const ev = (x) => ({ pointerId: 1, clientX: x, preventDefault() {} });
  rz._fire('pointerdown', ev(260));
  check('拖动时禁止选中文字(body.resizing)', body.classList.contains('resizing'));
  rz._fire('pointermove', ev(400));
  check('拖动改变目录树宽度', panel.style.width === '400px', `得到 ${panel.style.width}`);
  rz._fire('pointerup', ev(400));
  check('松手后恢复可选中', !body.classList.contains('resizing'));
  // 松手后再动不应继续改宽度
  rz._fire('pointermove', ev(700));
  check('松手后拖动不再生效', panel.style.width === '400px', `得到 ${panel.style.width}`);
  // 边界夹取：太窄看不到树，太宽把右侧挤没
  rz._fire('pointerdown', ev(400));
  rz._fire('pointermove', ev(10));
  check('宽度下限 120px', panel.style.width === '120px', `得到 ${panel.style.width}`);
  rz._fire('pointermove', ev(99999));
  check('宽度上限 innerWidth-240', panel.style.width === '1040px', `得到 ${panel.style.width}`);
  rz._fire('pointerup', ev(10));
}

// ---------- 8. 连接/断开是同一个按钮 ----------
{
  const btn = els.get('connect');
  ctx.setStatus(true, '已连接');
  check('已连接时按钮显示「断开」', btn.textContent === '断开', `得到 ${btn.textContent}`);
  ctx.setStatus(false, '未连接');
  check('未连接时按钮显示「连接」', btn.textContent === '连接', `得到 ${btn.textContent}`);
  check('已移除独立的断开按钮', !realIds.has('disconnect'));
  check('已移除终端按钮', !realIds.has('btnTerm'));
  check('已移除底部日志框', !realIds.has('log'));
}

// ---------- 9. 终端页常驻 ----------
// 顶部「终端」按钮已移除，终端页若还能关掉就再也开不出来了。
{
  ctx.openTab('file:/tmp/a.txt', 'a.txt', 'file-box', () => {});
  ctx.openTerm();
  const bar = els.get('tabbar');
  const termEl = bar.children.find((d) => d.children[0] && d.children[0].textContent === '终端');
  const fileEl = bar.children.find((d) => d.children[0] && d.children[0].textContent === 'a.txt');
  check('终端页没有关闭叉', !!termEl && termEl.children.length === 1, termEl ? `子节点 ${termEl.children.length} 个` : '没找到终端页');
  check('普通页有关闭叉', !!fileEl && fileEl.children.length === 2, fileEl ? `子节点 ${fileEl.children.length} 个` : '没找到文件页');
  ctx.closeTab('term');
  const still = els.get('tabbar').children.find((d) => d.children[0] && d.children[0].textContent === '终端');
  check('终端页关不掉', !!still);
}

console.log(failed ? `\n${failed} 项失败` : '\n全部通过');
process.exit(failed ? 1 : 0);
