/*
 * aiteam 看板主逻辑（B5-3：5s 主轮询 + ①②③区渲染 + tab 切换 + 失败处理；
 *                     B5-4：②区会话行点选三元组——对话面板本体已拆 dialog.js
 *                     （#24/#25/#27 对话三口见彼文件头）；
 *                     B5-5：token JS 流转——?token= 首入→localStorage→统一
 *                     fetch 封装注入 Authorization 头→401 提示（AC14.6）；
 *                     b3-W3：②区三层树（项目→栏目→会话行）——平铺会话心跳面
 *                     撤销，行配件（含 b8 命中徽标）整体吸收进 renderTree）
 *
 * 零构建红线（A10/§四总则）：原生 JS 零构建——无 import/无模块打包/无外部库，
 * 多文件经多个 <script defer> 按文档序直引（app.js 在前供工具，dialog.js 在后）；
 * ES2020+ 现代语法（浏览器原生支持）。数据端点为技术设计 §2.2/§2.3 冻结契约
 * （B3/B4 产物）：端点未上线时 fetch 404 与网络错同路径走失败兜底，上线即通。
 *
 * API 白名单机械核验（AC14.5 接口清单半的常驻闸）：本文件与 dialog.js 一切以
 * /api/v1 开头的路径字面量（含注释内出现）受 internal/server/static_test.go
 * TestAppJsApiWhitelist 约束（遍历 staticHandler 全部 js 资产扫描）——本文件
 * 仅读口 + ping，对话三口（#24 唯一写口/#25/#27）在 dialog.js。
 * 新增任何调用先改白名单测试表留显式决策痕，勿绕过。
 *
 * 分区：常量 → 运行态 → 工具 → token 流转 → 主题三态 → 失败处理 → 主轮询 →
 *       渲染① → 渲染②（会话行点选写入 dialog.js 的三元组全局态）→ 渲染③ →
 *       tab 切换 → 轻路由（b4-W3）→ 时钟/版本 → 启动
 */

'use strict';

/* ---------- 常量 ---------- */

// D1 主轮询周期 5s：#17 overview + #22 时间窗 + #20 资源（总线 tab 激活时叠加 #26）
const POLL_INTERVAL_MS = 5000;
// ①区当前时间本地每秒刷新（独立 interval，与数据轮询分离——5s 跳秒的时钟是坏的）
const CLOCK_INTERVAL_MS = 1000;
// #29 版本轮低频 60s：版本非实时关注点仅底栏展示（实现裁量，注释留痕）
const PING_INTERVAL_MS = 60000;

/* ---------- 运行态 ---------- */

// 各数据面上次快照（JSON 字符串 diff，§5.3 局部重绘：未变化区不触碰 DOM）
let lastOverviewJson = '';
let lastWindowsJson = '';
let lastResourcesJson = '';
let lastBusJson = '';
// overview 对象引用另存（json 串仅作 diff 键）：selectColumn 即时切换明细消费
let lastOverview = null;
// ②树点选的栏目 code（③栏目明细 tab 展示其信箱明细；再点同栏目取消选中）
let selectedColumnCode = null;
// ②会话行点选的三元组选中态（selectedSessionProject/Column/Name）与对话面板
// 运行态声明在 dialog.js（经典 script 顶层 let 为全局词法绑定，跨文件运行时
// 互读写；defer 按文档序执行完毕才触发任何轮询回调，声明时序无碍）
// 当前激活 tab（总线流仅激活时随主轮询拉取，NFR2 读压力口径）
let activeTab = 'tab-bus';
// 任一数据口见过 401 → 底栏 token 状态与顶栏鉴权提示的共同依据（B5-5 统一
// 入口：只在 fetchJSON/postJSON 两封装内置位，勿在调用点散判，防状态漂移）
let sawUnauthorized = false;
// 本轮 pollOnce 期间是否出现过数据口 401（每轮开头重置）——顶栏提示条
// 「鉴权失败」文案与自动恢复的轮次级判据（全局 sawUnauthorized 只增不减，
// 无法表达「服务恢复带 token 后提示自动消」）。含 dialog.js 经同封装的 401
// （置位落入本轮窗口同被消费，同为真实鉴权失败，展示无害）
let pollUnauthorized = false;
// ⑤底栏轮询倒计时秒数
let countdownSec = POLL_INTERVAL_MS / 1000;

/* ---------- 工具 ---------- */

const $ = (id) => document.getElementById(id);

// 建元素小工具：一律 textContent 赋值（天然转义，服务端数据无注入面）
function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

// 数据缺字段容错展示（#20 资源对象等约定俗成命名：缺字段显示空串不抛异常）
const txt = (v) => (v === undefined || v === null ? '' : String(v));

// 时间显示：解析失败原样回显（跨端时钟串容错）
function fmtTime(v) {
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? txt(v) : d.toLocaleString('zh-CN', { hour12: false });
}

// 秒→人类可读时长（配置展示区，B5-6）：900→15m、3600→60m、45→45s、
// 90→1m30s；缺值/非法值（旧服务端无 config 段或加载中）回 '–' 占位
function fmtDurationSec(v) {
  if (v === undefined || v === null || v === '') return '–';
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) return '–';
  if (n < 60) return n + 's';
  const m = Math.floor(n / 60);
  const s = n % 60;
  return s === 0 ? m + 'm' : m + 'm' + s + 's';
}

// 秒→短相对时长（b3-W3 会话行状态徽标共用）：45→45s、125→2m、3700→1h
// （≥1h 折 Nh 整除向下——失联徽标口径；与 fmtDurationSec 的 3600→60m 分 tier 不同）。
// 非有限/负值回 null（调用方回退既有「在线/失联」文案，防御钟面回拨同哲学）
function fmtRelSec(v) {
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) return null;
  if (n < 60) return Math.floor(n) + 's';
  if (n < 3600) return Math.floor(n / 60) + 'm';
  return Math.floor(n / 3600) + 'h';
}

// 401 统一入口（勿在调用点散判）：置全局 sawUnauthorized（底栏 token 状态与
// 顶栏鉴权提示同源依据）+ 本轮信号 pollUnauthorized（提示条轮次级自动恢复判据）
function markUnauthorized() {
  sawUnauthorized = true;
  pollUnauthorized = true;
}

// fetch 一冻结端点并剥 {data:...} 壳；非 2xx（含 404 端点未上线常态）抛错走失败兜底。
// token 开态注入 Authorization 头（withAuth 集中注入，dialog.js 走同封装零改动；
// 无 token 不发头——服务端 token 关=零感知，AC14.6 反向判据）
async function fetchJSON(url) {
  const resp = await fetch(url, { headers: withAuth({ Accept: 'application/json' }) });
  if (resp.status === 401) markUnauthorized();
  if (!resp.ok) throw new Error('HTTP ' + resp.status + ' @ ' + url);
  const body = await resp.json();
  return body && body.data !== undefined ? body.data : body;
}

// POST 冻结端点（#24 唯一写口）并剥 {data:...} 壳；非 2xx 抛 Error——message
// 优先取响应 error.message，其次 error.code（target_session_not_found(404)/
// body_too_large(413)/空 body(400) 契约错误码直显面板），再退 HTTP 状态。
// 无 body/非 JSON 响应（代理错误页等）不抛解析错，走状态兜底。
async function postJSON(url, payload) {
  const resp = await fetch(url, {
    method: 'POST',
    headers: withAuth({ Accept: 'application/json', 'Content-Type': 'application/json' }),
    body: JSON.stringify(payload),
  });
  if (resp.status === 401) markUnauthorized();
  let data = null;
  try { data = await resp.json(); } catch { /* 兜底 HTTP 状态 */ }
  if (!resp.ok) {
    const errMsg = data && data.error ? (data.error.message || data.error.code) : null;
    throw new Error(errMsg || ('HTTP ' + resp.status));
  }
  return data && data.data !== undefined ? data.data : data;
}

// 文本槽位 diff 更新（值未变不写 DOM）
function setText(id, value) {
  const node = $(id);
  if (node && node.textContent !== String(value)) node.textContent = String(value);
}

// UTF-8 字节数（#24 body 4KB 前端预检口径，与服务端 byte 上限对齐）
const textEncoder = new TextEncoder();
const byteLen = (s) => textEncoder.encode(s).length;

/* ---------- token 流转（B5-5，AC14.6/§5.3：URL ?token= 首入 → localStorage 持久
              → 统一 fetch 封装注入 Authorization 头；服务端半归 B6 中间件） ----------
   失效语义（裁量留痕）：localStorage key 无过期/无清理逻辑——服务端 token 单值
   口径无轮换（YAGNI），失效由用户改 URL 重进覆盖；401 不清 storage（轮询继续，
   服务恢复带 token 重进即自愈，清了反而丢失「已启用」展示依据）。 */

// localStorage key：页面私有存储无命名空间冲突面，单 key 即可
const TOKEN_STORAGE_KEY = 'aiteam-token';

// 读存储 token（隐私模式/禁用存储时 localStorage 访问抛异常——按无 token 处理，
// 退化为无头请求走 401 提示，不崩页面）
function getStoredToken() {
  try { return localStorage.getItem(TOKEN_STORAGE_KEY); } catch { return null; }
}

// 统一注入点：有 token 则加 Authorization: Bearer <token>（Bearer 头优先于
// ?token= 查询参数是服务端取值序，页面恒走头形态）。集中在本封装注入，
// 勿在各调用点散加；无 token 原样返回（token 关=请求零差异）。
// 调用点须传新建字面量对象（变异式注入，勿传共享对象）。
function withAuth(headers) {
  const token = getStoredToken();
  if (token) headers.Authorization = 'Bearer ' + token;
  return headers;
}

// 首入捕获：URL ?token= 非空 → 存 localStorage → replaceState 清地址栏参数
// （token 不留在 URL/浏览器历史可见面；刷新/分享不再重放参数）。裁量：参数
// 存在但为空值（?token=）也清地址栏（空 token 存了等于没存，挂栏仅剩噪音）；
// 必须先于一切 fetch 执行（启动段首行），否则首轮轮询无头必 401。
function initTokenFromUrl() {
  const params = new URLSearchParams(location.search);
  if (!params.has('token')) return;
  const token = params.get('token') || '';
  if (token) {
    try { localStorage.setItem(TOKEN_STORAGE_KEY, token); } catch { /* 存储禁用：无头请求走 401 提示兜底 */ }
  }
  // 保留 hash：防未来 tab 状态上 hash 或外链锚点被 token 首入静默吞掉
  history.replaceState(null, '', location.pathname + location.hash);
}

/* ---------- 主题三态（b4-W1/FR8：跟系统→亮→暗循环，#theme-toggle ◐ 钮） ----------
   auto=移除 html data-theme 属性回落 @media (prefers-color-scheme: dark) 纯 CSS
   媒体查询（零 JS 监听——系统切换实时跟随，YAGNI：不做时间跟随自动切换）；
   light/dark=html data-theme 属性切换变量组（style.css 三块标准形态）。
   存储键=tech-design §5.3 冻结 aiteam.theme；读存储 try/catch 兜底=auto
   （getStoredToken 同哲学——隐私模式/禁用存储不崩页面）。
   启动序在 initTokenFromUrl() 之后、首轮 pollOnce() 之前（先于首轮渲染着色）。 */

const THEME_STORAGE_KEY = 'aiteam.theme';

// 读存储主题：存储禁用/读取异常原样返回 null（态源判定在 applyTheme/cycleTheme）
function getStoredTheme() {
  try { return localStorage.getItem(THEME_STORAGE_KEY); } catch { return null; }
}

// 应用主题态：auto 移除属性回落媒体查询；light/dark 上属性切变量组；
// 按钮 title/aria-label 反映当前态（读屏可感知）
function applyTheme(theme) {
  const root = document.documentElement;
  if (theme === 'light' || theme === 'dark') root.setAttribute('data-theme', theme);
  else root.removeAttribute('data-theme');
  const btn = $('theme-toggle');
  if (btn) {
    const label = theme === 'light' ? '亮色' : theme === 'dark' ? '暗色' : '跟系统';
    btn.title = '主题：' + label + '（点击切换）';
    btn.setAttribute('aria-label', btn.title);
  }
}

// ◐ 点击循环 跟系统→亮→暗→跟系统，回写存储（auto=移除键；存储禁用静默，
// 本次会话内属性仍生效）
function cycleTheme() {
  const cur = document.documentElement.getAttribute('data-theme') || 'auto';
  const next = cur === 'light' ? 'dark' : cur === 'dark' ? 'auto' : 'light';
  try {
    if (next === 'auto') localStorage.removeItem(THEME_STORAGE_KEY);
    else localStorage.setItem(THEME_STORAGE_KEY, next);
  } catch { /* 存储禁用：不报错，属性切换仍生效 */ }
  applyTheme(next);
}

function initTheme() {
  applyTheme(getStoredTheme());
  const btn = $('theme-toggle');
  if (btn) btn.addEventListener('click', cycleTheme);
}

/* ---------- 失败处理（不白屏不静默不弹窗：顶部提示条，恢复成功自动隐藏） ----------
   #load-error 单条双 kind（B5-5 扩展，kind 存 bar.dataset.kind 供判别清除，
   同 dialog.js setDialogError 模式）：net=数据加载失败（网络错/404 端点未就绪）、
   auth=鉴权失败（本轮任一数据口 401）。auth 优先展示——已知原因比网络错误更有
   行动价值；两条互斥共槽，随每轮 pollOnce 重估（恢复即消，不残留）。 */

// 文案秒数与主轮询周期一致：失败后下一轮 5s 自动重试
const LOAD_ERROR_TEXT = '数据加载失败（端点未就绪或服务异常），' + (POLL_INTERVAL_MS / 1000) + ' 秒后重试';
// 鉴权失败文案（AC14.6：role=alert 读屏可感知，不弹窗不打断——轮询继续，
// 用户带 token 重进后下轮自动恢复；提示而非登出，页面无登出控件）
const AUTH_ERROR_TEXT = '鉴权失败：请带 token 访问（携带 token 后下轮自动恢复）';

function setLoadError(msg, kind) {
  const bar = $('load-error');
  if (!bar) return;
  if (msg) {
    if (bar.textContent !== msg) bar.textContent = msg;
    if (bar.dataset.kind !== kind) bar.dataset.kind = kind;
    bar.hidden = false;
  } else if (!bar.hidden || bar.dataset.kind) {
    bar.textContent = '';
    delete bar.dataset.kind;
    bar.hidden = true;
  }
}

// 每轮 pollOnce 尾部重估提示条：本轮有 401 → 鉴权文案；否则失败 → 网络文案；
// 全部成功 → 隐藏（「服务恢复带 token 后自动消」）
function refreshLoadError(failed) {
  if (pollUnauthorized) setLoadError(AUTH_ERROR_TEXT, 'auth');
  else if (failed) setLoadError(LOAD_ERROR_TEXT, 'net');
  else setLoadError('');
}

/* ---------- 主轮询（D1：5s 三端点；总线 tab 激活时叠加总线流） ---------- */

async function pollOnce() {
  countdownSec = POLL_INTERVAL_MS / 1000; // 重置⑤底栏倒计时
  pollUnauthorized = false; // 本轮 401 信号重置（轮次级，refreshLoadError 判据）
  const wantBus = activeTab === 'tab-bus';
  // allSettled：单端点失败不拖垮其余区渲染（局部可用优于全页不可用）
  const [ov, win, res, bus] = await Promise.allSettled([
    fetchJSON('/api/v1/status?mode=overview'), // #17：①②③(栏目明细) 同源（AC14.1）
    fetchJSON('/api/v1/windows/now'),          // #22：时间窗矩阵
    fetchJSON('/api/v1/resources'),            // #20：资源分组表（AC14.4）
    // #26 总线流：spec 未显式规定其刷新时机，实现裁量=激活才拉（NFR2 读压力
    // 口径）——切到该 tab 立即拉一次 + 随主轮询刷新，离开 tab 即停拉
    wantBus ? fetchJSON('/api/v1/board/bus-stream') : Promise.resolve(null),
  ]);

  // 渲染段双层防线之二：数据成功但渲染抛错也须亮失败条（不白屏不静默），
  // 且单区异常不中断其余区渲染
  let failed = false;
  const safeRender = (ok, render, value) => {
    if (!ok) {
      failed = true;
      return;
    }
    try {
      render(value);
    } catch {
      failed = true;
    }
  };
  safeRender(ov.status === 'fulfilled', renderOverview, ov.value);
  safeRender(win.status === 'fulfilled', renderWindows, win.value);
  safeRender(res.status === 'fulfilled', renderResources, res.value);
  if (wantBus) { // 未激活轮次 bus 恒为 fulfilled(null)，不计失败
    safeRender(bus.status === 'fulfilled', renderBus, bus.value);
  }
  refreshLoadError(failed);
}

/* ---------- 渲染① 全局状态条（数据全部来自 #17 overview 同源，AC14.1） ---------- */

function renderOverview(ov) {
  if (!ov) return; // data:null 防御（fetchJSON 对 data:null 返回 null），与其余渲染函数守卫对齐
  const json = JSON.stringify(ov);
  if (json === lastOverviewJson) return; // §5.3 JSON diff：overview 未变不触碰同源 DOM
  lastOverviewJson = json;
  lastOverview = ov; // 对象引用另存（selectColumn 明细即时切换消费，免反解 json 串）

  const projects = Array.isArray(ov.projects) ? ov.projects : [];
  const columnCount = projects.reduce(
    (n, p) => n + (Array.isArray(p.columns) ? p.columns.length : 0), 0);
  let lostCount = 0;
  for (const p of projects) {
    for (const s of (p.sessions || [])) if (s.alive === false) lostCount++;
  }
  const blockCount = Array.isArray(ov.block_unreceipted) ? ov.block_unreceipted.length : 0;

  setText('stat-projects', projects.length);
  setText('stat-columns', columnCount);
  // 失联会话数 >0 槽位红显（AC14.2 计数面）：骨架 lost-value 常红语义由 JS
  // 接管——首轮数据起 0 值去红、>0 红显加粗
  const lostEl = $('stat-lost');
  if (lostEl) {
    lostEl.textContent = String(lostCount);
    lostEl.classList.toggle('lost', lostCount > 0);
    lostEl.classList.toggle('lost-value', lostCount > 0);
  }
  setText('stat-blocks', blockCount);

  renderTree(projects, ov.generated_at); // b3-W3：三层树吸收平铺面（generated_at 供相对时长同源钟）
  renderColumnDetail(projects); // 栏目明细同源自 #17（mailboxes 在 overview 内）
  renderConfigBar(ov);          // 配置展示区同源 #17（config 段+windows 汇总，B5-6）
}

/* ---------- 配置展示区（B5-6 只读：①区下方一行，三阈值+时间窗概览） ----------
   数据全部来自主轮询既有 #17 响应（config 回显段 + projects[].columns[].windows
   汇总），零新增请求、零编辑控件（AC23.4 只读红线——PRD 配置双层模型：编辑走
   服务配置文件不改看板）。秒→人类可读（900s→15m）；时间窗计数=配置窗栏目数+
   生效中（allowed）窗口数。 */

function renderConfigBar(ov) {
  const cfg = ov.config && typeof ov.config === 'object' ? ov.config : {};
  setText('cfg-heartbeat', fmtDurationSec(cfg.heartbeat_timeout_sec));
  setText('cfg-sentinel', fmtDurationSec(cfg.sentinel_timeout_sec));
  setText('cfg-stale', fmtDurationSec(cfg.progress_stale_after_sec));
  let configured = 0; // 配置了窗（≥1 启用阶段在列）的栏目数
  let allowed = 0;    // 生效中（allowed）窗口数
  const projects = Array.isArray(ov.projects) ? ov.projects : [];
  for (const p of projects) {
    for (const c of (p.columns || [])) {
      const wins = c.windows && typeof c.windows === 'object' ? c.windows : {};
      const stages = Object.keys(wins);
      if (stages.length > 0) configured++;
      for (const st of stages) {
        if (wins[st] && wins[st].status === 'allowed') allowed++;
      }
    }
  }
  setText('cfg-windows', configured + ' 栏目配置窗，' + allowed + ' 窗生效中');
}

/* ---------- 渲染② 项目-栏目-会话三层树（AC14.1/14.2；b3-W3 三层化） ----------
   焦点保护（架构性隔离）：本区渲染函数只写 #tree-body，永不触碰 #dialog-panel
   内部——重绘路径与 ④ 区无任何 DOM/引用交集，对话输入框焦点不因数据轮询丢失。
   结构：项目节点（▶ 可折叠）→栏目节点（▶ 可折叠）→会话行；默认全展开。
   折叠=容器 .collapsed 纯 class 切换（CSS display:none，不落 localStorage）；
   另以运行时 Set 记忆折叠态——generated_at 每轮必变触发整树重绘，无记忆则
   折叠撑不过一个 5s 轮询周期（重载即复位=非持久化语义不变）。 */

// 折叠节点运行时记忆：键=树坐标（'p|'+项目 code / 'c|'+项目 code+'|'+栏目 code）
const collapsedTreeNodes = new Set();

function toggleTreeNode(container, key) {
  if (container.classList.toggle('collapsed')) collapsedTreeNodes.add(key);
  else collapsedTreeNodes.delete(key);
}

function renderTree(projects, generated_at) {
  const root = $('tree-body');
  if (!root) return;
  root.textContent = '';
  // generated_at 同源钟面毫秒（在线时长与哨兵命中年龄共用——禁本地时钟，
  // 快照一致性，CLI status 同口径）；不可解析=各徽标走防御回退分支
  const genMs = new Date(generated_at).getTime();
  for (const p of projects) {
    const projKey = 'p|' + txt(p.code);
    const proj = el('div', 'tree-project');
    if (collapsedTreeNodes.has(projKey)) proj.classList.add('collapsed');
    const head = el('div', 'tree-project-head');
    head.append(
      el('span', 'tree-caret', '▶'),
      el('b', null, txt(p.name) || txt(p.code)),
      el('span', 'status-tag', txt(p.status)),
    );
    // 项目头无点选语义，整行=折叠开关；栏目行点选须留给 selectColumn（见下）
    head.addEventListener('click', () => toggleTreeNode(proj, projKey));
    const projBody = el('div', 'tree-children');
    // sessions 是项目级平铺数组（s.column=栏目 code）——按栏目分组挂第三层；
    // 装配后剩余键=未列栏目（archived 栏目会话仍在列——store 往返 2/6 过滤
    // 口径不对称），尾部兜底补组防缺行
    const byColumn = new Map();
    for (const s of (p.sessions || [])) {
      const code = txt(s.column);
      if (!byColumn.has(code)) byColumn.set(code, []);
      byColumn.get(code).push(s);
    }
    for (const c of (p.columns || [])) {
      const code = txt(c.code);
      projBody.append(buildColumnNode(p, code, txt(c.status), byColumn.get(code) || [], genMs));
      byColumn.delete(code);
    }
    for (const [code, sess] of byColumn) { // 未列栏目兜底组（AC1.3 不缺行）
      projBody.append(buildColumnNode(p, code, null, sess, genMs));
    }
    if (projBody.children.length === 0) {
      projBody.append(el('div', 'tree-empty-row placeholder', '（无栏目）')); // AC1.3 空态（真无栏目且无会话——兜底组已挂则为空判真）
    }
    proj.append(head, projBody);
    root.append(proj);
  }
  if (projects.length === 0) {
    // 树空态引导（b4-W4/AC6 手机树空态=主文案冻结「还没有项目或会话」+CLI 登记指引；
    // 桌面空库同块共用）；（无会话）/（无栏目）占位行（b3 AC1.3）保留不动
    const guide = el('div', 'tree-empty-guide');
    guide.append(el('p', 'empty-guide-main', '还没有项目或会话'));
    guide.append(el('p', 'empty-guide-sub', '用 aiteam CLI 登记项目与栏目后，会话将出现在这里（详见接入指南）。'));
    root.append(guide);
  }
  tryScrollToSelectedRow(); // 深链选中待办：首轮渲染后滚到位（applyRoute 首跑时树未渲染）
}

// 栏目节点装配（第二层+第三层 children）：statusText=null=未列栏目兜底组
// （无 status 透出面）；无会话栏目挂（无会话）空态占位行（AC1.3）
function buildColumnNode(p, code, statusText, sessions, genMs) {
  const colKey = 'c|' + txt(p.code) + '|' + code;
  const node = el('div', 'tree-column');
  if (collapsedTreeNodes.has(colKey)) node.classList.add('collapsed');
  const row = el('div', 'tree-column-row');
  row.dataset.columnCode = code;
  if (row.dataset.columnCode === selectedColumnCode) row.classList.add('selected');
  const caret = el('span', 'tree-caret', '▶');
  caret.addEventListener('click', (e) => {
    e.stopPropagation(); // 行点选=selectColumn 既有动线不变，折叠只认指示符
    toggleTreeNode(node, colKey);
  });
  row.append(caret, el('span', null, '栏目 ' + code));
  if (statusText !== null) row.append(el('span', 'status-tag', statusText));
  // 栏目行点选 → ③栏目明细 tab 切换展示对象
  row.addEventListener('click', () => selectColumn(row.dataset.columnCode));
  const body = el('div', 'tree-children');
  if (sessions.length === 0) {
    body.append(el('div', 'tree-empty-row placeholder', '（无会话）')); // AC1.3 空态
  } else {
    for (const s of sessions) body.append(buildSessionRow(p, s, genMs));
  }
  node.append(row, body);
  return node;
}

// 会话行装配（b4-W4 两行制：首行=名称+状态 pill+未读角标，次行=角色@栏目+相对
// 时间；b3 四件套语义与 dataset 三元组/选中态/点选动线零变；命中徽标段逐字保留）：
function buildSessionRow(p, s, genMs) {
  const row = el('div', 'session-row');
  row.dataset.sessionName = txt(s.name);
  // B5-4：补 project/column——sessions.name 跨项目可重名，三元组才唯一定位；
  // project 键=外层 projects.code（全仓先例 resolve.go/HTTP 头/CLI 全线 code，
  // code UNIQUE 必非空，||name 是死分支；B5-1 #27 的 project 字段须回 code 对齐）
  row.dataset.project = txt(p.code);
  row.dataset.column = txt(s.column);
  if (s.alive === false) row.classList.add('lost'); // 行级红显（AC14.2，.lost 类名保留）
  if (isSessionRowSelected(row)) row.classList.add('selected');
  // 相对时长一次算两处用（pill 文案与次行同源，复用 fmtRelSec；null=时刻缺/不可
  // 解析/钟面回拨——pill 回退既有「在线/失联」文案防御分支，次行省略相对时间段）
  let rel = null;
  if (s.alive === false) {
    rel = s.lost_for_sec > 0 ? fmtRelSec(s.lost_for_sec) : null;
  } else {
    const seenMs = new Date(s.last_seen_at).getTime();
    rel = Number.isFinite(genMs) && Number.isFinite(seenMs) && genMs >= seenMs
      ? fmtRelSec((genMs - seenMs) / 1000) : null;
  }
  const main = el('div', 'session-main');
  main.append(el('b', null, txt(s.name)));
  // 状态 pill（b4-W4 视觉范式迁移：哨兵在挂绿→蓝对齐 ZCode「运行中」蓝，批内留痕；
  // 文案语义零变）：在线=绿 pill「Ns/Nm/Nh 前」/失联=红 pill「失联 Nh/Nm」/在挂=蓝 pill
  if (s.alive === false) {
    main.append(el('span', 'pill pill-lost', rel ? '失联 ' + rel : '失联'));
  } else {
    const alive = el('span', 'pill pill-ok', rel ? rel + ' 前' : '在线');
    alive.title = txt(s.last_seen_at); // 悬停看精确心跳时刻（命中徽标 title 同款）
    main.append(alive);
  }
  // 哨兵在挂 pill：与下方 b8 命中徽标（最近命中时刻）两语义并存不合并
  if ((s.sentinels || []).some((sn) => sn && sn.alive === true)) {
    main.append(el('span', 'pill pill-sentinel', '哨兵'));
  }
  // 未读角标（既有 .unread-badge 保留=参考基准「行首活跃蓝点」对齐项）：0 不渲染，
  // 99+ 封顶（裁量留痕：三位数挤占窄栏会话行）
  const unread = (Number(s.unread_mailbox) || 0) + (Number(s.unread_dialog) || 0);
  if (unread > 0) main.append(el('span', 'unread-badge', unread > 99 ? '99+' : String(unread)));
  const sub = el('div', 'session-sub');
  sub.append(el('span', 'session-meta', txt(s.role) + ' @ 栏目' + txt(s.column)));
  if (rel) sub.append(el('span', 'session-rel', rel + ' 前'));
  row.append(main, sub);
  // 哨兵命中徽标（b8-W3）：sentinel_last_hit_at 非空时追加「哨兵·N 分钟前命中」
  // （「在挂≠在干活」服务端可见面）；title=原始命中时刻（悬停看精确值）。年龄取
  // 响应 generated_at 同源钟（禁本地时钟——快照一致性，CLI status 同口径）；
  // 时刻不可解析或钟面回拨（负差）=防御性不加徽标（fmtTime 容错同哲学）。
  // （b3-W3 吸收原样保留：TestAppJsSentinelHitBadge 源码锚定本段文案形态）
  if (s.sentinel_last_hit_at) {
    const hitMs = new Date(s.sentinel_last_hit_at).getTime();
    if (Number.isFinite(genMs) && Number.isFinite(hitMs) && genMs >= hitMs) {
      const mins = Math.floor((genMs - hitMs) / 60000);
      const badge = el('span', 'sentinel-hit', '哨兵·' + mins + ' 分钟前命中');
      badge.title = s.sentinel_last_hit_at;
      row.append(badge);
    }
  }
  // 会话行点选 → 对话面板激活（B5-4）：按三元组选中，再点同行取消
  row.addEventListener('click', () =>
    selectSession(row.dataset.project, row.dataset.column, row.dataset.sessionName));
  return row;
}

// 当前行是否命中三元组选中态（主轮询重绘时保持高亮依据）
function isSessionRowSelected(li) {
  return li.dataset.project === selectedSessionProject &&
    li.dataset.column === selectedSessionColumn &&
    li.dataset.sessionName === selectedSessionName;
}

function selectSession(project, column, name) {
  if (selectedSessionName === name && selectedSessionProject === project &&
    selectedSessionColumn === column) {
    // 再点同会话行：桌面=取消选中（AC1.2 既有口径不变）；移动=重新进入会话页
    // 保持选中（b4-T6 裁定——取消唯一路径=会话页「关闭」，动线差异 W6 文档明示）
    if (DESKTOP_MQ.matches) closeDialogPanel();
    else location.hash = sessionHash(project, column, name);
    return;
  }
  selectedSessionProject = project;
  selectedSessionColumn = column;
  selectedSessionName = name;
  markSelectedSessionRow();
  openDialogPanel();
  // 移动形态点选写 hash 入会话页（applyRoute 幂等消化）；桌面不写（无导航概念）
  if (!DESKTOP_MQ.matches) location.hash = sessionHash(project, column, name);
}

function markSelectedSessionRow() {
  document.querySelectorAll('#tree-body .session-row').forEach((row) => {
    row.classList.toggle('selected',
      row.dataset.project === selectedSessionProject &&
      row.dataset.column === selectedSessionColumn &&
      row.dataset.sessionName === selectedSessionName);
  });
}

function selectColumn(code) {
  selectedColumnCode = selectedColumnCode === code ? null : code; // 再点同栏目=取消
  document.querySelectorAll('#tree-body .tree-column-row').forEach((row) => {
    row.classList.toggle('selected', row.dataset.columnCode === selectedColumnCode);
  });
  // 明细面板即时切换（overview 对象已在内存，不必等下一轮轮询、免反解 json 串）
  renderColumnDetail(
    lastOverview && Array.isArray(lastOverview.projects) ? lastOverview.projects : []);
}

/* ---------- 渲染③ 四 tab ---------- */

// 分级徽标（AC14.3）：normal 灰 / important 橙 / block 红底白字最醒目
const LEVEL_CLASS = { normal: 'lv-normal', important: 'lv-important', block: 'lv-block' };

// 表格小工具：表头一行 + 空 tbody 留调用方填
function buildTable(headers) {
  const table = el('table', 'data-table');
  const hr = el('tr');
  for (const h of headers) hr.append(el('th', null, h));
  const thead = el('thead');
  thead.append(hr);
  table.append(thead, el('tbody'));
  return table;
}

// 总线消息流（#26）：服务端已倒序（ORDER BY seq DESC），按响应序渲染
function renderBus(data) {
  const pane = $('tab-bus');
  if (!pane) return;
  const messages = data && Array.isArray(data.messages) ? data.messages : [];
  const json = JSON.stringify(messages);
  if (json === lastBusJson) return;
  lastBusJson = json;

  pane.textContent = '';
  if (messages.length === 0) {
    pane.append(el('p', 'placeholder', '总线暂无消息。'));
    return;
  }
  for (const m of messages) {
    const row = el('div', 'bus-row');
    row.append(
      el('span', 'bus-seq', '#' + txt(m.seq)),
      el('span', 'badge ' + (LEVEL_CLASS[m.level] || 'lv-normal'), txt(m.level)),
      el('b', null, txt(m.sender)),
      el('span', 'bus-time', fmtTime(m.created_at)),
      el('span', 'bus-body', txt(m.body_preview)),
    );
    pane.append(row);
  }
}

// 资源占用（#20）：按 type 分组表——固定三组在前（port/account_range/data_range），
// 未知类型兜底成尾组；字段容错缺显示空串（AC14.4 与 CLI resource list 同源）
const RESOURCE_GROUPS = [
  ['port', '端口'],
  ['account_range', '账号段'],
  ['data_range', '数据段'],
];

function renderResources(data) {
  const pane = $('tab-resources');
  if (!pane) return;
  const list = data && Array.isArray(data.resources) ? data.resources : [];
  const json = JSON.stringify(list);
  if (json === lastResourcesJson) return;
  lastResourcesJson = json;

  pane.textContent = '';
  if (list.length === 0) {
    pane.append(el('p', 'placeholder', '暂无在用资源登记。'));
    return;
  }
  const groups = new Map();
  for (const [type, label] of RESOURCE_GROUPS) groups.set(type, { label, rows: [] });
  for (const r of list) {
    const type = txt(r.type) || 'unknown';
    if (!groups.has(type)) groups.set(type, { label: type, rows: [] });
    groups.get(type).rows.push(r);
  }
  for (const g of groups.values()) {
    if (g.rows.length === 0) continue;
    pane.append(el('h3', 'detail-title', g.label + '（' + g.rows.length + '）'));
    const table = buildTable(['值', '项目', '栏目', '备注']);
    for (const r of g.rows) {
      const tr = el('tr');
      tr.append(
        el('td', null, txt(r.value)),
        el('td', null, txt(r.project)),
        el('td', null, txt(r.column)),
        el('td', null, txt(r.note)),
      );
      table.append(tr);
    }
    pane.append(table);
  }
}

// 时间窗（#22）：栏目×阶段矩阵——allowed 绿标 / waiting 灰标；window 原样展示
// （含跨午夜 23:00-09:00 形态，不换算不解释）
function renderWindows(data) {
  const pane = $('tab-windows');
  if (!pane) return;
  const json = JSON.stringify(data); // as_of 变化也触发重绘（时点提示随数据走）
  if (json === lastWindowsJson) return;
  lastWindowsJson = json;

  pane.textContent = '';
  const asOf = data && data.as_of ? data.as_of : '';
  if (asOf) pane.append(el('p', 'detail-asof', '数据时点 ' + asOf));
  const entries = data && Array.isArray(data.entries) ? data.entries : [];
  if (entries.length === 0) {
    pane.append(el('p', 'placeholder', '暂无时间窗登记（未登记栏目默认 allowed）。'));
    return;
  }
  const table = buildTable(['项目', '栏目', '阶段', '状态', '时间窗']);
  for (const e of entries) {
    const tr = el('tr');
    const stTd = el('td');
    stTd.append(el('span', e.status === 'waiting' ? 'win-waiting' : 'win-allowed', txt(e.status)));
    tr.append(
      el('td', null, txt(e.project)),
      el('td', null, txt(e.column)),
      el('td', null, txt(e.stage)),
      stTd,
      el('td', null, e.window ? txt(e.window.start) + '-' + txt(e.window.end) : '—'),
    );
    table.append(tr);
  }
  pane.append(table);
}

// 栏目明细：②树选中栏目的信箱明细——role/pending/position/latest(seq+level)/
// 哨兵活性（dead 红）。数据同源自 #17 overview（首屏一次请求，AC14.1）
function renderColumnDetail(projects) {
  const pane = $('tab-column-detail');
  if (!pane) return;
  pane.textContent = '';
  if (!selectedColumnCode) {
    pane.append(el('p', 'placeholder', '在左侧项目树点选栏目行，查看其信箱明细（待消费/哨兵活性）。'));
    return;
  }
  let column = null;
  for (const p of projects) {
    for (const c of (p.columns || [])) if (txt(c.code) === selectedColumnCode) column = c;
  }
  if (!column) {
    pane.append(el('p', 'placeholder', '栏目 ' + selectedColumnCode + ' 不在当前 overview 数据中。'));
    return;
  }
  pane.append(el('h3', 'detail-title', '栏目 ' + selectedColumnCode + ' 信箱明细'));
  const mailboxes = Array.isArray(column.mailboxes) ? column.mailboxes : [];
  if (mailboxes.length === 0) {
    pane.append(el('p', 'placeholder', '（该栏目暂无信箱）'));
    return;
  }
  const table = buildTable(['信箱', '待消费', '位置', '最新消息', '哨兵']);
  for (const mb of mailboxes) {
    const latest = mb.latest || {};
    const tr = el('tr');
    tr.append(
      el('td', null, txt(mb.role)),
      el('td', null, txt(mb.pending)),
      el('td', null, txt(mb.position)),
      el('td', null, latest.seq !== undefined
        ? '#' + txt(latest.seq) + ' ' + txt(latest.level)
        : '—'),
      // 哨兵：dead 红显 / alive 正常 / none 显示 —
      el('td', mb.sentinel === 'dead' ? 'lost' : null,
        mb.sentinel === 'none' || mb.sentinel === undefined || mb.sentinel === null ? '—' : txt(mb.sentinel)),
    );
    table.append(tr);
  }
  pane.append(table);
}

/* ---------- tab 切换（ARIA 三件套：点击 + 键盘左右/Home/End roving tabindex） ---------- */

function initTabs() {
  const tabs = Array.from(document.querySelectorAll('.tab-bar .tab-btn'));
  if (tabs.length === 0) return;

  function activate(tabId, focus) {
    activeTab = tabId;
    for (const t of tabs) {
      const on = t.dataset.tab === tabId;
      t.classList.toggle('active', on);
      t.setAttribute('aria-selected', on ? 'true' : 'false');
      t.tabIndex = on ? 0 : -1; // roving tabindex：Tab 键序仅含激活 tab
      const pane = $(t.dataset.tab);
      if (pane) pane.classList.toggle('active', on);
      if (on && focus) t.focus();
    }
    // 激活才拉口径：切到总线 tab 立即拉一轮，进入即见最新（不等下个 5s 周期）
    if (tabId === 'tab-bus') {
      fetchJSON('/api/v1/board/bus-stream').then(renderBus).catch(() => refreshLoadError(true));
    }
  }

  for (const t of tabs) {
    t.addEventListener('click', () => activate(t.dataset.tab, false));
    t.addEventListener('keydown', (e) => {
      // WAI-ARIA tabs 键盘惯例：左右循环，Home/End 首尾
      const idx = tabs.indexOf(t);
      let next = -1;
      if (e.key === 'ArrowRight') next = (idx + 1) % tabs.length;
      else if (e.key === 'ArrowLeft') next = (idx - 1 + tabs.length) % tabs.length;
      else if (e.key === 'Home') next = 0;
      else if (e.key === 'End') next = tabs.length - 1;
      if (next >= 0) {
        e.preventDefault();
        activate(tabs[next].dataset.tab, true);
      }
    });
  }
}

/* ---------- 轻路由（b4-W3：手机两页导航 <1100px；桌面零导航概念） ----------
   hash 仅两形态（§三.6 冻结面）：#/board（缺省等价，不回写地址栏）与
   #/session/<p>/<c>/<n>（三段 encodeURIComponent——三元组才唯一定位 PRD §1.4，
   b4-T5 裁定；%2F 编码不破 / 分隔切分——先 split 后 decode）。畸形 hash 回落
   board 不回写地址栏。路由类 body[data-route] 仅被移动媒体查询规则消费
   （断点+hash 双条件），桌面零视觉影响、点选不写 hash。
   轮询零联动：切页=纯 dataset 类切换，不销毁 DOM 不启停定时器——主轮询 5s 恒跑
   （两页同文档共享）、对话 3s 轮询随选中态非随页面（返回主页选中保持、轮询继续；
   「关闭」/桌面再点取消才是停轮询唯一路径=§5.3 开=叠加关即停口径不变）。
   与 ?token= 首入共存：initTokenFromUrl 的 replaceState 保留 location.hash
   （token 段既有防线），token+hash 双参数深链互不吞。 */

const DESKTOP_MQ = window.matchMedia('(min-width: 1100px)');

// 会话页 hash（三元组三段编码）
function sessionHash(p, c, n) {
  return '#/session/' + [p, c, n].map(encodeURIComponent).join('/');
}

// 解析当前 hash：{view:'session', p, c, n} | {view:'board'}（空/畸形=board 不回写）
function parseHash() {
  const h = location.hash;
  if (h.startsWith('#/session/')) {
    try {
      const parts = h.slice('#/session/'.length).split('/').map(decodeURIComponent);
      if (parts.length === 3 && parts[0] && parts[1] && parts[2]) {
        return { view: 'session', p: parts[0], c: parts[1], n: parts[2] };
      }
    } catch { /* 畸形编码回落 board */ }
  }
  return { view: 'board' };
}

// 深链选中行滚动待办（scrollIntoView 小件）：启动首跑 applyRoute 时树未渲染
// （首轮 pollOnce 未归），选中行不存在——记待办，renderTree 尾见命中行滚到位即清；
// 运行期 hashchange 进会话页时树已渲染则当场滚（本函数直滚）
let pendingScrollToSelected = false;

function tryScrollToSelectedRow() {
  if (!pendingScrollToSelected) return;
  const row = document.querySelector('#tree-body .session-row.selected');
  if (!row) return; // 树未渲染——留待 renderTree 尾重试
  pendingScrollToSelected = false;
  row.scrollIntoView({ block: 'nearest' });
}

// 应用路由：body[data-route]='board'|'session'；session 且三元组≠当前选中→执行与
// 点选等价的选中激活路径（置三全局+markSelectedSessionRow+openDialogPanel+滚到位）；
// 三元组已选中=幂等 no-op（防 hashchange 自写循环）。board 不主动清选中（返回主页
// 保持选中=3s 对话轮询继续；「关闭」才是取消唯一路径）
function applyRoute() {
  const route = parseHash();
  document.body.dataset.route = route.view;
  if (route.view !== 'session') return;
  // 桌面点选权威（b4-W3 审查修复）：桌面态已有选中时，hash 残留不得经断点切换
  // 回退选中（跨断点拖窗场景）；桌面无选中且带深链仍激活（深链直达恢复语义不变）
  if (DESKTOP_MQ.matches && selectedSessionName) return;
  if (route.p === selectedSessionProject && route.c === selectedSessionColumn &&
    route.n === selectedSessionName) return; // 已选中=幂等
  selectedSessionProject = route.p;
  selectedSessionColumn = route.c;
  selectedSessionName = route.n;
  pendingScrollToSelected = true;
  markSelectedSessionRow();
  tryScrollToSelectedRow();
  openDialogPanel();
}

// 移动主页分段导航（b4-W3）：「会话｜总线」两钮切 body[data-mview]+active 态。
// 内存态不上 hash（§三.6）、不持久化（b3 折叠 YAGNI 口径延续，§三.8）；
// 仅在移动主页可见（CSS 显隐全控），点击只切类——零请求零轮询联动
function initMobileNav() {
  const nav = $('m-home-nav');
  if (!nav) return;
  const btns = Array.from(nav.querySelectorAll('[data-mview-btn]'));
  for (const btn of btns) {
    btn.addEventListener('click', () => {
      document.body.dataset.mview = btn.dataset.mviewBtn;
      for (const b of btns) b.classList.toggle('active', b === btn);
    });
  }
}

/* ---------- ①区时钟 + ⑤底栏倒计时（独立 1s interval，与数据轮询分离） ---------- */

function tickSecond() {
  const clock = $('stat-clock');
  if (clock) clock.textContent = new Date().toLocaleString('zh-CN', { hour12: false });
  countdownSec -= 1;
  if (countdownSec < 1) countdownSec = POLL_INTERVAL_MS / 1000;
  const cd = $('footer-countdown');
  if (cd) cd.textContent = countdownSec + 's';
  // token 状态槽位（B5-5 接管细化，与 sawUnauthorized 同源防状态漂移）：
  //   见过 401 → 服务端 token 开且凭证被拒；本地存有 token → 已配置
  //   （措辞「已配置」非「已启用」——服务端关态+localStorage 残留场景下
  //   「已启用」是错误陈述，本地事实只到「配置过 token」）；皆无 → 未启用
  let tokenLabel = '未启用';
  if (sawUnauthorized) tokenLabel = '已启用（需 token）';
  else if (getStoredToken()) tokenLabel = '已配置（已带 token）';
  setText('footer-token', tokenLabel);
}

/* ---------- #29 版本轮（低频 60s，仅底栏展示） ---------- */

async function refreshVersion() {
  try {
    const data = await fetchJSON('/api/v1/ping');
    setText('footer-version', txt(data.version) || '未知');
  } catch {
    // 版本获取失败不打扰主画面（失败条由主轮询负责），底栏保持占位
  }
}

/* ---------- 启动：defer 脚本执行时 DOM 已解析完毕，立即拉一轮再进周期 ---------- */

// token 首入捕获必须第一执行：withAuth 注入依赖已存的 localStorage 态，
// 晚于 refreshVersion/pollOnce 首拉则开态首轮请求无头必 401
initTokenFromUrl();
initTheme();
initTabs();
initMobileNav();
tickSecond();
setInterval(tickSecond, CLOCK_INTERVAL_MS);
refreshVersion();
setInterval(refreshVersion, PING_INTERVAL_MS);
// 路由监听（hashchange/matchMedia 事件均为任务级回调——触发时 app.js/dialog.js 均
// 已求值，注册安全；首跑 applyRoute() 在 dialog.js 启动尾，见彼文件）
window.addEventListener('hashchange', applyRoute);
DESKTOP_MQ.addEventListener('change', () => applyRoute());
pollOnce(); // 页面加载立即拉一轮（不等首个 interval，首屏可用性）
setInterval(pollOnce, POLL_INTERVAL_MS);

