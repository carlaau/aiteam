/*
 * aiteam 看板对话面板（B5-4 本体，自 app.js 拆出：#24 唯一写口 + #25 对话流 3s
 * 独立轮询 + #27 name→id 映射，AC15/D1/§5.3 开=叠加关即停）
 *
 * 加载序：<script defer> 置于 app.js 之后——defer 按文档序执行，本文件可引用
 * app.js 顶层工具（$/el/txt/fmtTime/fetchJSON/postJSON/byteLen）；②区会话行
 * 点选（app.js selectSession）调用本文件 openDialogPanel/closeDialogPanel，
 * 本文件顶层 let（selectedSession* 三元组等）为经典 script 全局词法绑定，
 * 两文件运行时互读写（声明序无碍：defer 执行完才触发任何轮询回调）。
 *
 * API 白名单机械核验（AC14.5 接口清单半的常驻闸）：本文件一切以 /api/v1 开头
 * 的路径字面量（含注释内出现）受 internal/server/static_test.go
 * TestAppJsApiWhitelist 约束（遍历 staticHandler 全部 js 资产扫描，防拆文件
 * 掏空核验面）——对话三口：唯一写口 #24 POST /api/v1/board/messages、
 * #25 GET /api/v1/board/sessions/{id}/dialog（前缀放行）、#27
 * GET /api/v1/sessions 建 (project|column|name)→id 映射。
 * 新增任何调用先改白名单测试表留显式决策痕，勿绕过。
 */

'use strict';

/* ---------- 常量 ---------- */

// D1 对话面板独立轮询周期 3s：仅面板激活（选中会话）时运行，关闭/切换即停
// （§5.3 开=叠加关即停——独立 interval 与主轮询 5s 互不合并，关闭零残余定时器）
const DIALOG_POLL_INTERVAL_MS = 3000;
// #24 body 上限 4KB：前端预检（TextEncoder 字节数），服务端 413 是兜底非首道闸
const DIALOG_BODY_MAX_BYTES = 4096;
// #25 单次拉取条数（冻结契约 ?limit=100；b3-W2 起服务端按 seq DESC 返回最新在前，
// 前端 renderDialog 内 reverse 成对话正序（旧→新）渲染，方向表述以此为准）
const DIALOG_LIMIT = 100;
// 对话流正文折叠阈值（§5.2 超长折叠，b3-W4）：UTF-8 字节数（byteLen 同 #24 预检
// 口径）>2048 默认收起，行上「展开全文/收回」切换——纯展示面，与服务端 413 上限无关
const DIALOG_BODY_FOLD_BYTES = 2048;
// #27 映射刷新节流（裁量=miss 独立节流，注释与行为一致）：常态每 10 轮（30s）
// 重建；miss 前 3 轮当轮重试（会话太新自愈），连 miss 超阈值后降频每 10 轮一试
const DIALOG_MAP_REFRESH_EVERY = 10, DIALOG_MAP_MISS_FAST_RETRY = 3;
// 空态引导文案（b4-W4/AC6：Q8 冻结主文案；b3 旧双轨文案清退——grep 零残留闸，
// 无测试锚引用本串）
const DIALOG_IDLE_TEXT = '在左侧树点选会话开始对话';
const DIALOG_IDLE_SUB_TEXT = '选中会话后此处展示对话流，底部输入框可直接发送消息。';

/* ---------- 运行态 ---------- */

// ②会话行点选的三元组选中态（B5-4）：sessions.name 仅 UNIQUE(project_id,
// column_id,name) 跨项目可重名——三元组才唯一定位。project 键=projects.code
// （全仓先例一面倒：resolve.go/HTTP 头/CLI 全线 code，code UNIQUE 必非空，||name 死分支）。
// #25/#24 都用数字 id——#24 定位键 2026-10-03 裁定回改：按会话唯一 id 定位禁按
// name（跨栏目同名防御，先行段与现实对齐，B4-3 收敛先例），须经 #27 建
// (project|column|name)→id 映射换取；B5-1 实现 #27 时 project 字段须回 code
// 并对齐此键。
let selectedSessionProject = null;
let selectedSessionColumn = null;
let selectedSessionName = null;
// 对话面板运行态：3s 轮询句柄（null=停）/单飞闸/轮次与 miss 计数（#27 节流依据）/
// (project|column|name)→sessions.id 映射
let dialogTimer = null;
let dialogFetchInFlight = false;
let dialogPollTick = 0;
let sessionMapMissCount = 0;
let sessionIdMap = new Map();
// 对话流 JSON diff 键（§5.3 局部重绘口径）：b3-W4 起=messages+positions 合串比对
// （六键行含 kind/from、顶层 positions 任一变化都触发重绘——chip 翻拍靠 positions
// 变化驱动）；内容未变不触碰 #dialog-log；开/关面板时重置强制首轮重绘（防两会话
// 消息恰好同 json 被 diff 跳过）
let lastDialogJson = '';
// 行上超长折叠的跨重绘记忆（b3-W4，按 seq 记——全局 seq 唯一，跨会话无串扰）：
// dialogUnfoldedSeqs=超长正文被手动展开的 seq（默认折叠，展开即入集）。3s 轮询
// 重绘（消息/位点变化触发）按集合恢复展开态；每轮渲染按当前 limit 窗口裁剪，
// 防长期驻留无界增长
const dialogUnfoldedSeqs = new Set();
// #24 发送单飞闸（防连点重复投递）
let sendingMessage = false;

/* ---------- 对话面板（B5-4：#24 唯一写口 + #25 对话流 3s 独立轮询 + #27 id 映射，
              AC15 + D1 + §5.3 开=叠加关即停） ----------
   焦点保护：轮询渲染只写 #dialog-log/#dialog-target/#dialog-error/按钮态，永不
   触碰 #dialog-input（3s 重绘不抢焦点）。失败口径（裁量）：三口失败走面板内
   #dialog-error 不亮全局条（局部功能不拖累①②③区）；#25 无已读字段不发明 UI。 */

// (project|column|name)→sessions.id 映射键（三元组——sessions.name 跨项目可重名）
function sessionMapKey(project, column, name) {
  return project + '|' + column + '|' + name;
}

function lookupSessionId() {
  if (!selectedSessionName) return null;
  return sessionIdMap.get(
    sessionMapKey(selectedSessionProject || '', selectedSessionColumn || '', selectedSessionName)) || null;
}

// 拉 #27 重建三元组→id 映射；失败静默保留旧映射（miss 走「定位中」提示自愈，
// 不亮全局失败条，见本区头部失败口径）。键口径：project=code——B5-1 实现 #27
// 时 project 字段须回 code 对齐行键（返回 name 则映射恒 miss 走降频兜底）。
async function refreshSessionIdMap() {
  try {
    const data = await fetchJSON('/api/v1/sessions');
    const map = new Map();
    for (const s of (data && Array.isArray(data.sessions) ? data.sessions : [])) {
      map.set(sessionMapKey(txt(s.project), txt(s.column), txt(s.name)), s.id);
    }
    sessionIdMap = map;
  } catch {
    /* 保留旧映射，miss 自愈路径兜底 */
  }
}

// 对话流拉取一轮（#25）：3s 独立轮询体 + 发送成功后的立即刷新共用入口；单飞闸
// 防响应慢于 3s 堆积。过期响应丢弃（竞态防线）：进函数快照选中三元组键，每个
// await 恢复后比对——不一致即丢弃，防切会话后旧响应/错误挂到新面板、关面板后写 DOM
async function fetchDialogOnce() {
  if (!selectedSessionName || dialogFetchInFlight) return;
  dialogFetchInFlight = true;
  const selKey = sessionMapKey(selectedSessionProject || '', selectedSessionColumn || '', selectedSessionName);
  const stale = () => sessionMapKey(
    selectedSessionProject || '', selectedSessionColumn || '', selectedSessionName) !== selKey;
  try {
    dialogPollTick++;
    let id = lookupSessionId();
    // #27 映射刷新：常态每 10 轮/miss 当轮，miss 连打超阈值降频（P2 裁量 b，注释与行为一致）
    const throttled = !id && sessionMapMissCount >= DIALOG_MAP_MISS_FAST_RETRY &&
      dialogPollTick < DIALOG_MAP_REFRESH_EVERY;
    if ((!id || dialogPollTick >= DIALOG_MAP_REFRESH_EVERY) && !throttled) {
      await refreshSessionIdMap();
      if (stale()) return; // 刷新期间切会话/关面板：丢弃
      id = lookupSessionId();
      sessionMapMissCount = id ? 0 : sessionMapMissCount + 1;
      dialogPollTick = 0;
    }
    if (!id) {
      renderDialogPlaceholder('会话「' + selectedSessionName + '」定位中或已不可用，稍后自动重试。');
      return;
    }
    // #25：{id}=sessions 数字主键（#27 映射所得，非 name）；b3-W2 起服务端按
    // seq DESC 返回最新在前+顶层 positions 双维度位点，正序化在 renderDialog 内做
    const data = await fetchJSON(`/api/v1/board/sessions/${id}/dialog?limit=${DIALOG_LIMIT}`);
    if (stale()) return; // 响应期间切会话/关面板：丢弃（关面板后不写任何 DOM）
    renderDialog(
      data && Array.isArray(data.messages) ? data.messages : [],
      data && data.positions ? data.positions : null);
    // 成功仅清加载类错误（kind 判别），不吞超长预检/发送失败类提示
    const bar = $('dialog-error');
    if (bar && !bar.hidden && bar.dataset.kind === 'load') setDialogError('');
  } catch (e) {
    if (stale()) return; // 错误提示同样校验：A 的 404 不挂到 B 面板
    setDialogError('对话流加载失败（' + (e && e.message ? e.message : '网络错误') + '），' +
      (DIALOG_POLL_INTERVAL_MS / 1000) + ' 秒后自动重试', 'load'); // 旧消息保留不清屏
  } finally {
    dialogFetchInFlight = false;
  }
}

// 开=启动独立 3s 轮询（先清旧句柄防叠加，切换=关旧开新）；重选=显式重试，miss 计数归零
function startDialogPolling() {
  stopDialogPolling();
  dialogPollTick = 0;
  sessionMapMissCount = 0;
  fetchDialogOnce(); // 激活立即拉一轮（不等首个 3s 周期，对齐主轮询首屏口径）
  dialogTimer = setInterval(fetchDialogOnce, DIALOG_POLL_INTERVAL_MS);
}

// 关=clearInterval 即停（§5.3 关即停：无叠加强漏，句柄判空幂等）
function stopDialogPolling() {
  if (dialogTimer !== null) clearInterval(dialogTimer);
  dialogTimer = null;
}

function openDialogPanel() {
  lastDialogJson = ''; // 重置 diff 键：切换会话强制首轮重绘（防同 json 被跳过）
  setDialogError('');
  const target = $('dialog-target');
  if (target) target.textContent = '会话「' + txt(selectedSessionName) + '」 @ ' +
    (txt(selectedSessionProject) || '—') + ' / 栏目' + (txt(selectedSessionColumn) || '—');
  const closeBtn = $('dialog-close');
  if (closeBtn) closeBtn.hidden = false;
  renderDialogPlaceholder('对话流加载中…');
  updateSendState(); // 选中后解除发送禁用
  startDialogPolling();
}

// 「关闭」=取消选中+停轮询+回引导态（引导文案对齐 B5-3 栏目明细占位）；面板常驻五区布局
function closeDialogPanel() {
  selectedSessionProject = selectedSessionColumn = selectedSessionName = null;
  stopDialogPolling();
  lastDialogJson = '';
  setDialogError('');
  renderDialogPlaceholder(DIALOG_IDLE_TEXT, DIALOG_IDLE_SUB_TEXT);
  const target = $('dialog-target');
  if (target) target.textContent = DIALOG_IDLE_TEXT;
  const closeBtn = $('dialog-close');
  if (closeBtn) closeBtn.hidden = true;
  // b3-W3 选择器连带：会话行入三层树（#tree-body），原平铺会话列表面已撤销
  document.querySelectorAll('#tree-body .session-row')
    .forEach((row) => row.classList.remove('selected'));
  updateSendState();
}

// 展开态集合按当前窗口裁剪：只留 liveSeqs 中存在的成员（滚出 #25 limit 窗口的旧
// seq 随渲染释放，见 dialogUnfoldedSeqs 注）。遍历中删除
// Set 成员是 JS 安全操作
function pruneSeqSet(set, liveSeqs) {
  for (const s of set) {
    if (!liveSeqs.has(s)) set.delete(s);
  }
}

// 对话流渲染（b3-W4 六键行 {seq,body,from_board,kind,from,created_at} + 顶层
// positions{mailbox,dialog}，无已读字段不发明 UI）：
// 排序——b3-W2 起服务端按 seq DESC 返回最新在前，此处 reverse 成对话正序（旧→新）；
// kind 分支（buildMessageRow）——chat 走既有 from_board 双侧气泡（board 右/agent 左，
// 零回归）、direct 走 CLI 侧样式行（非气泡+from 身份进详情行）、其余 kind（receipt
// 等理论不进流，防御）按 direct 同构灰态；行上一切换——正文 >2KB（UTF-8 字节）默认
// 折叠「展开全文/收回」；seq/created_at/from 三键常驻 meta 行（dialog-meta-1：用户
// 验收体验推翻 b3-W4 收合判据，「详情」按钮机制删除）；from_board=true 行挂两态
// chip——当轮 positions.dialog >=
// 该行 seq 翻「已处理」否则「已投递」，渲染时按当轮值算不做乐观翻转（发送成功后
// 不提前翻，等下一次轮询数据回来自然翻）。
// JSON diff 键=messages+positions 合串（未变不重绘）；展开/收合态按 seq 记忆跨重绘
// 保持；全 textContent 无 innerHTML；只写 #dialog-log（焦点保护）；贴底时新消息
// 自动跟随滚动
function renderDialog(messages, positions) {
  const log = $('dialog-log');
  if (!log) return;
  const json = JSON.stringify([messages, positions || null]);
  if (json === lastDialogJson) return;
  const nearBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 48;
  lastDialogJson = json;
  const liveSeqs = new Set(messages.map((m) => m.seq));
  pruneSeqSet(dialogUnfoldedSeqs, liveSeqs);
  log.textContent = '';
  if (messages.length === 0) {
    log.append(el('p', 'placeholder', '（暂无消息，向该会话发送第一条消息吧）'));
    return;
  }
  const ordered = messages.slice().reverse(); // 服务端 seq DESC → 对话正序（旧→新）
  // 对话位（chip 翻拍依据）：缺位/非法防御回 null——chip 恒「已投递」不误翻
  const dialogPos = positions && typeof positions.dialog === 'number' ? positions.dialog : null;
  for (const m of ordered) {
    log.append(buildMessageRow(m, dialogPos));
  }
  if (nearBottom) log.scrollTop = log.scrollHeight;
}

// 单条消息行装配（b3-W4）：kind 分支选行形态 + 常驻 meta 行 + 超长折叠 + 两态 chip。
// 行类——kind='direct'→.kind-direct（CLI 样式行）；'chat' 或缺失（老服务端四键
// 响应防御）→既有 .from-board/.from-agent 双侧气泡零回归；其余 kind→.kind-other
// （direct 同构灰态兜底）。chip 仅 from_board===true 行挂（board 发恒 chat，判据
// 按 AC 字面量独立于 kind 分支）。
function buildMessageRow(m, dialogPos) {
  const kind = txt(m.kind);
  const mine = m.from_board === true;
  let rowCls;
  if (kind === 'direct') rowCls = 'kind-direct';
  else if (kind === 'chat' || kind === '') rowCls = mine ? 'from-board' : 'from-agent';
  else rowCls = 'kind-other';
  const row = el('div', 'msg-row ' + rowCls);
  const seq = m.seq;

  // 正文（chat=气泡/direct=CLI 行同用 .msg-body，形态差异全在 CSS 类面）+
  // 超长折叠：byteLen>2KB 默认收起（.msg-folded），「展开全文/收回」切换
  const body = el('div', 'msg-body', txt(m.body));
  const isLong = byteLen(txt(m.body)) > DIALOG_BODY_FOLD_BYTES;
  let foldBtn = null; // 仅超长行创建（非超长无折叠钮）
  if (isLong) {
    foldBtn = el('button', 'msg-toggle');
    foldBtn.type = 'button';
    const applyFold = () => {
      const unfolded = dialogUnfoldedSeqs.has(seq);
      body.classList.toggle('msg-folded', !unfolded);
      foldBtn.textContent = unfolded ? '收回' : '展开全文';
    };
    foldBtn.addEventListener('click', () => {
      if (dialogUnfoldedSeqs.has(seq)) dialogUnfoldedSeqs.delete(seq);
      else dialogUnfoldedSeqs.add(seq);
      applyFold();
    });
    applyFold();
  }

  // 行内详情常驻（seq/created_at/from 三键，dialog-meta-1）：免点开直达元信息；
  // from='-'（board 发契约值）显示「看板」对齐既有 meta 行称谓，空值防御 '—'
  const details = el('div', 'msg-details');
  const fromVal = txt(m.from);
  details.textContent = '#' + txt(seq) + ' · ' + fmtTime(m.created_at) +
    ' · from: ' + (fromVal === '-' ? '看板' : (fromVal === '' ? '—' : fromVal));

  const actions = el('div', 'msg-actions');
  if (foldBtn) actions.append(foldBtn);
  if (mine) {
    const done = dialogPos !== null && dialogPos >= seq;
    actions.append(el('span', 'chip ' + (done ? 'chip-done' : 'chip-delivered'),
      done ? '已处理' : '已投递'));
  }

  row.append(body, details, actions);
  return row;
}

// 占位渲染（引导/定位 miss/加载中）：b4-W4 升级空态引导块——div.empty-guide
// （主文案 p+可选次文案 p，居中排版）；短路口径不变（组装后 textContent 比对，
// 同内容不重建 DOM——轮询 miss 每 3s 同占位不闪动）
function renderDialogPlaceholder(text, sub) {
  const log = $('dialog-log');
  if (!log) return;
  const content = text + (sub || '');
  if (log.textContent === content) return;
  lastDialogJson = '';
  log.textContent = '';
  const guide = el('div', 'empty-guide');
  guide.append(el('p', 'empty-guide-main', text));
  if (sub) guide.append(el('p', 'empty-guide-sub', sub));
  log.append(guide);
}

// 面板内错误/提示位（不全局弹窗）：发送失败/超长预检/加载失败共用一槽。
// kind=错误类别（'load' 加载/'send' 发送/'limit' 超长）存 bar.dataset.kind——
// 分类清除走 dataset 判别，防前缀文案比对随文案修改静默失效
function setDialogError(msg, kind) {
  const bar = $('dialog-error');
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

// 发送按钮态：未选中/发送中/空 body/超 4KB 任一即禁用（空=disabled 反馈，超长=错误槽提示）
function updateSendState() {
  const input = $('dialog-input');
  const btn = $('dialog-send');
  const bar = $('dialog-error');
  if (!input || !btn) return;
  const n = byteLen(input.value);
  const tooLong = n > DIALOG_BODY_MAX_BYTES;
  btn.disabled = !selectedSessionName || sendingMessage ||
    input.value.trim().length === 0 || tooLong;
  if (tooLong) {
    setDialogError('消息超过 4KB 上限（当前 ' + n + ' 字节，上限 ' + DIALOG_BODY_MAX_BYTES + '），请缩短后发送', 'limit');
  } else if (bar && !bar.hidden && bar.dataset.kind === 'limit') {
    setDialogError(''); // 仅清超长提示，不吞发送失败/加载失败类错误
  }
}

// #24 发送（唯一写口）：session=会话数字 id（2026-10-03 裁定：按会话唯一 id 定位
// 禁按 name——跨栏目同名不唯一，经 #27 映射换取；映射 miss=会话尚未定位成功禁发，
// 走既有错误槽提示由轮询自愈后再发）；from 恒不发——契约可选，
// 且 AC14.5 唯一表单机械闸（input=1）不设第二个输入控件（裁量留痕）。
// 过期丢弃（与读路径防线同标准）：快照选中键，postJSON 恢复后比对——不一致时
// 跳过清输入框/清错误槽/立即拉取等 DOM 副作用（消息已投递则静默成功，失败不挂新面板）
async function sendDialogMessage() {
  if (!selectedSessionName || sendingMessage) return;
  const input = $('dialog-input');
  if (!input) return;
  const body = input.value.trim();
  if (!body || byteLen(body) > DIALOG_BODY_MAX_BYTES) { updateSendState(); return; }
  sendingMessage = true;
  updateSendState();
  const selKey = sessionMapKey(selectedSessionProject || '', selectedSessionColumn || '', selectedSessionName);
  const stale = () => sessionMapKey(
    selectedSessionProject || '', selectedSessionColumn || '', selectedSessionName) !== selKey;
  // 定位键=会话数字 id（回改见上注）：miss 不发空 id 请求（kind='load'——读路径
  // 成功即定位成功，届时自动清除本提示）
  const id = lookupSessionId();
  if (!id) {
    setDialogError('会话「' + selectedSessionName + '」定位中，稍后自动重试', 'load');
    sendingMessage = false;
    updateSendState();
    return;
  }
  try {
    await postJSON('/api/v1/board/messages', { session: id, body: body });
    if (stale()) return; // 发送期间切会话/关面板：静默成功，不碰新面板
    input.value = ''; // 发送成功才清空输入框（失败保留原文便于改后再发）
    setDialogError('');
    fetchDialogOnce(); // 立即拉一轮不等 3s（若正被轮询单飞占用则并入下一轮，最坏 3s）
  } catch (e) {
    if (!stale()) setDialogError('发送失败（' + (e && e.message ? e.message : '网络错误') + '）', 'send');
  } finally {
    sendingMessage = false;
    updateSendState(); // 仅按钮态恢复（非面板内容写入），不属过期副作用
  }
}

function initDialogPanel() {
  const input = $('dialog-input');
  const send = $('dialog-send');
  const closeBtn = $('dialog-close');
  if (send) send.addEventListener('click', sendDialogMessage);
  if (closeBtn) {
    closeBtn.addEventListener('click', () => {
      // b4-W3 移动联动：关闭=回主页（先判选中再关——closeDialogPanel 清三元组全局；
      // 桌面行为零变不写 hash；#/board 经 applyRoute 切路由类）
      const hadSelection = !!selectedSessionName;
      closeDialogPanel();
      if (hadSelection && !DESKTOP_MQ.matches) location.hash = '#/board';
    });
  }
  if (input) {
    input.addEventListener('input', updateSendState);
    // Enter 快捷发送（容器非 form 原生不触发 submit；isComposing 防输入法选词回车误发）
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.isComposing && send && !send.disabled) sendDialogMessage();
    });
  }
  updateSendState(); // 初始态（未选中+空输入）禁用发送
}

/* ---------- 启动（defer 时 DOM 已就绪；对话面板初始化由本文件自启，app.js 不插手） ---------- */

initDialogPanel();

// b4-W3 深链首跑：applyRoute 依赖本文件 openDialogPanel，app.js 顶层先于本文件
// 求值不可调——首跑挂本文件启动尾（路由本体仍在 app.js，本文件仅此一行联动）
applyRoute();
