'use strict';

/* ---------- 基础工具 ---------- */

const $ = (sel) => document.querySelector(sel);

const state = {
  stats: null,
  containers: [],
  images: [],
  tasks: [],
  notifications: [],
  settings: null,
  tab: 'containers',
  containerSearch: '',
  imageSearch: '',
  // 镜像页勾选的镜像 ID。清理是批量操作，靠它决定删哪些。
  picked: new Set(),
  // 自检结果：当前版本、有没有新版本、更新内容与两套升级步骤
  self: null,
  // 远程节点
  nodes: [],
  nodesOnline: 0,
  // 展开的节点 id → 该节点最近的容器快照。
  // 按需拉取而不是随每次轮询全量取：节点多了之后「全部容器」是个大响应，
  // 而用户同一时刻通常只看一台。
  nodeOpen: new Map(),
  // 展开的节点 id → 读取快照失败的原因。
  // 读失败和「真的没有容器」必须分开：以前失败时把列表置空，页面就写
  // 「这台机器上除 Agent 自身外没有其它容器」，而卡片上还挂着 8 个容器、6 项待更新。
  nodeErr: new Map(),
  // 节点 id → 正在进行中的操作（目前只有更新）。
  // Hub 的列表接口每次都带回来，所以进度是跟着轮询自己走的，不需要长连接。
  nodeOps: {},
  // 节点 id → 一次「整项目依次更新」的进度（第几个 / 一共几个 / 卡在哪个）。
  // 与 ops 分开：ops 说的是「此刻正在动哪个容器」，这份说「这一批还剩几个」。
  nodeBatches: {},
  // 升级相关：版本落后的节点、正在升级的节点、每台上一次的结局。
  // 三样都由列表接口一起带回来，前端不做版本比较 —— 比版本的规则只该有一份。
  nodeUpgradable: new Set(),
  nodeUpgrading: new Set(),
  nodeUpgradeLast: {},
  hubVersion: '',
  // 删除确认窗里正在等确认的节点 id。弹窗里的「确认删除」拿它下发请求，
  // 不靠 DOM 里藏 id —— 卡片重渲染时节点行会被整段换掉。
  nodeDelID: null,
};

/* ---------- 登录态 ---------- */

// 页面走自己的登录表单，而不是让浏览器弹原生框：飞牛桌面用 iframe 加载页面，
// 原生弹框在里面体验很差，部分壳还会直接判定页面不可用变成白屏。
// 凭据以 Basic 形式随请求发送，存在 localStorage 里以便刷新后免登录。

const AUTH_KEY = 'autodocker.auth';
const USER_KEY = 'autodocker.user';
let authToken = readAuth();
let pollTimer = null;
// 上一轮轮询还没结束就不再开一轮，免得请求叠在一起越堆越多
let pollBusy = false;
// 空闲状态下每多少轮做一次整份重载
let idleTicks = 0;
// 上一轮轮询时有几个节点在更新。用来识别「刚跑完」的那一刻 ——
// 结束的那一轮要再拉一次容器，否则行状态会停在「待更新」。
let lastRunningOps = 0;
// 登录层还挂着的时候不要打接口：会 401，还会弹一串没用的提示
let uiReady = false;

function readAuth() {
  try { return localStorage.getItem(AUTH_KEY) || ''; } catch (_) { return ''; }
}

function writeAuth(v) {
  authToken = v || '';
  try {
    if (authToken) localStorage.setItem(AUTH_KEY, authToken);
    else localStorage.removeItem(AUTH_KEY);
  } catch (_) { /* 无痕模式下写不了，本次会话内依然可用 */ }
}

// 安装时可以自定义登录用户名，这里记住上一次成功登录的那个，
// 免得每次都默认填 admin 让人以为密码错了。
function rememberedUser() {
  try { return localStorage.getItem(USER_KEY) || ''; } catch (_) { return ''; }
}

function rememberUser(u) {
  try {
    if (u) localStorage.setItem(USER_KEY, u);
  } catch (_) { /* 同上 */ }
}

// 凭据是 base64("用户名:密码")，顺手把用户名解出来
function userFromToken() {
  try {
    const raw = atob(authToken);
    const i = raw.indexOf(':');
    return i > 0 ? raw.slice(0, i) : '';
  } catch (_) { return ''; }
}

/* ---------- 主题 ---------- */

// data-theme 属性在 index.html 的 <head> 里已经由内联脚本设好了
// （那段必须同步执行，否则深色主题下会先闪一下白底）。
// 这里只负责：把按钮的图标/提示语对上当前主题、以及切换后落库。
const THEME_KEY = 'autodocker.theme';

function currentTheme() {
  return document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
}

function applyTheme(name) {
  const t = name === 'light' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', t);
  // 认按钮上写着「切到哪个主题」，不是当前是什么主题。
  const next = t === 'dark' ? '浅色' : '深色';
  document.querySelectorAll('#btn-theme, #btn-theme-login').forEach((btn) => {
    btn.title = '切换到' + next + '主题';
    btn.setAttribute('aria-label', '切换到' + next + '主题');
  });
  try { localStorage.setItem(THEME_KEY, t); } catch (_) { /* 无痕模式下写不了，本次会话内依然生效 */ }
}

function toggleTheme() {
  applyTheme(currentTheme() === 'dark' ? 'light' : 'dark');
}

function b64(s) {
  const bytes = new TextEncoder().encode(s);
  let bin = '';
  bytes.forEach((b) => { bin += String.fromCharCode(b); });
  return btoa(bin);
}

function showLogin(message) {
  const mask = $('#login');
  if (!mask) return;
  mask.hidden = false;

  const err = $('#login-error');
  if (message) {
    err.textContent = message;
    err.hidden = false;
  } else {
    err.hidden = true;
  }

  const user = $('#login-user');
  if (user) user.value = rememberedUser() || 'admin';

  const pass = $('#login-pass');
  if (pass) {
    // 只在凭据失效重新弹出时清空；登录失败要留着让用户改
    if (!message) pass.value = '';
    setTimeout(() => pass.focus(), 30);
  }
}

function hideLogin() {
  const mask = $('#login');
  if (mask) mask.hidden = true;
}

async function api(path, opts = {}) {
  const headers = { 'Content-Type': 'application/json', ...(opts.headers || {}) };
  if (authToken) headers['Authorization'] = 'Basic ' + authToken;

  const res = await fetch(path, { ...opts, headers });

  if (res.status === 401) {
    writeAuth('');
    stopPolling();
    showLogin('');
    const err = new Error('需要登录');
    err.unauthorized = true;
    throw err;
  }
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText;
    try {
      const data = await res.json();
      if (data && data.error) msg = data.error;
    } catch (_) { /* 忽略解析失败 */ }
    throw new Error(msg);
  }
  if (res.status === 204) return null;
  return res.json();
}

let toastTimer = null;
function toast(msg, kind) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast ' + (kind || 'ok');
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 3200);
}

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function fmtTime(t) {
  if (!t) return '—';
  const d = new Date(t);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtDuration(from, to) {
  if (!from) return '';
  const a = new Date(from), b = to ? new Date(to) : new Date();
  if (isNaN(a.getTime()) || isNaN(b.getTime())) return '';
  const sec = Math.max(0, Math.round((b - a) / 1000));
  if (sec < 60) return sec + ' 秒';
  return Math.floor(sec / 60) + ' 分 ' + (sec % 60) + ' 秒';
}

function fmtSize(n) {
  if (!n || n <= 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = n, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
}

// 镜像 ID 太长，只留 sha256: 加前 12 位，足够区分又看得下
function shortImageId(id) {
  const s = String(id || '');
  const i = s.indexOf(':');
  if (i >= 0) return s.slice(0, i + 1) + s.slice(i + 1, i + 13);
  return s.length > 12 ? s.slice(0, 12) : s;
}

// 悬空镜像拿短 ID 当标题时，去掉 sha256: 前缀 —— `docker images` 的 IMAGE ID
// 列也是这么显示的（形如 d825b9c26134），用户能直接拿去命令行里对。
function bareImageId(id) {
  const s = String(id || '');
  const i = s.indexOf(':');
  const hex = i >= 0 ? s.slice(i + 1) : s;
  return hex.length > 12 ? hex.slice(0, 12) : hex;
}

/* ---------- 状态字典 ---------- */

// 状态徽章只服务于「容器」页。镜像页不再展示检测状态 —— 见 renderImages 的说明。
const STATUS = {
  up_to_date: { text: '已是最新', cls: 'ok' },
  update_available: { text: '有更新', cls: 'warn' },
  new_version_tag: { text: '可选更新', cls: 'info' },
  // 一个 compose 项目里有成员没查出结果。与「未知」分开：未知是单个容器的状态，
  // 这里是整行的结论，得让用户一眼看出「没查全」而不是「不用更新」。
  check_error: { text: '检测异常', cls: 'danger' },
  unknown: { text: '未知', cls: 'muted' },
  ignored: { text: '已忽略', cls: 'muted' },
};

const MODE = { auto: '自动', 'digest-only': '摘要比对', 'pin-watch': '版本监听' };

const TASK = {
  pending: { text: '排队中', cls: 'muted' },
  running: { text: '执行中', cls: 'info' },
  success: { text: '成功', cls: 'ok' },
  failed: { text: '失败', cls: 'danger' },
  rolled_back: { text: '已回滚', cls: 'warn' },
};

const CARD = {
  pending: { text: '待处理', cls: 'warn' },
  updating: { text: '更新中', cls: 'info' },
  done: { text: '已完成', cls: 'ok' },
  failed: { text: '失败', cls: 'danger' },
  rollback: { text: '已回滚', cls: 'warn' },
  ignored: { text: '已忽略', cls: 'muted' },
  snoozed: { text: '已稍后', cls: 'muted' },
};

const KIND = { new_version: '有新版本', new_tag: '可选更新' };

function badge(map, key) {
  const it = map[key] || { text: key || '—', cls: 'muted' };
  return `<span class="badge ${it.cls}">${esc(it.text)}</span>`;
}

/* ---------- 渲染 ---------- */

// 首页这排数字说的是**容器**，不是镜像。
// 镜像页讲「磁盘上有什么、能不能清」，容器页讲「在跑的东西该不该更新」，
// 顶部这条跟着「该不该更新」走。标签也写明白，免得看的人以为数错了。
// 「未使用镜像」是唯一一格讲磁盘的：它对应镜像页那批可以清理的行，
// 单独放在这里是因为「有更新」与「可选更新」说的都是同一件事（有新版），
// 并排放着看的人会以为数重了。
function renderStats() {
  const s = state.stats || {};
  // 「有更新」这一格说的是「你总共还欠多少个容器要更新」，所以要把远程节点算进来 ——
  // 只管本机的话，接了三台 NAS 的人得挨个点开节点页才知道真实工作量。
  // 本机与节点分开留着，是为了标签上能注明其中多少来自节点：数字合在一起、
  // 点进容器页却只数出本机那几个，最容易让人以为哪里算错了。
  const localUpdates = s.update_available || 0;
  const nodeUpdates = s.node_update || 0;
  const items = [
    { label: '监控容器', value: s.total || 0, cls: '' },
    {
      label: nodeUpdates > 0 ? `有更新 · 含节点 ${nodeUpdates}` : '有更新',
      value: localUpdates + nodeUpdates,
      cls: 'warn',
      title: nodeUpdates > 0 ? `本机 ${localUpdates} + 远程节点 ${nodeUpdates}` : '',
    },
    // 后端读不到 Docker 时给 null —— 显示「—」，别拿 0 假装「没有可清理的」
    { label: '未使用镜像', value: s.unused_images == null ? '—' : s.unused_images, cls: 'info' },
    { label: '未读通知', value: s.unread || 0, cls: 'accent' },
    { label: '已是最新', value: s.up_to_date || 0, cls: 'ok' },
  ];
  $('#stats').innerHTML = items.map((it) => `
    <div class="stat ${it.cls}"${it.title ? ` title="${esc(it.title)}"` : ''}>
      <div class="stat-value">${it.value}</div>
      <div class="stat-label">${it.label}</div>
    </div>
  `).join('');

  const statusEl = $('#global-status');
  statusEl.title = '';
  if (s.updating) {
    statusEl.textContent = '更新执行中';
    statusEl.className = 'pill info';
  } else if (s.scanning) {
    statusEl.textContent = '扫描中';
    statusEl.className = 'pill info';
  } else {
    // 多个渠道可能同时配好了，胶囊只写「已连接」，具体是哪几个挂 title 上。
    // 写死渠道名（「飞书 已连接」）在同时配了别的渠道时是错的。
    const chans = (s.notify_channels || []).filter((c) => c.configured);
    const active = chans.filter((c) => c.enabled);
    // 关掉的渠道说「已暂停」而不是「未连接」：
    // 后者会让人以为坏了，跑去改配置；实际只是自己关的，配置好着呢。
    const textOf = (c) => {
      if (!c.enabled) return '已暂停';
      return c.online ? '已连接' : '未连接' + (c.error ? '（' + shortErr(c.error, 60) + '）' : '');
    };
    const lines = chans.map((c) => c.name + '：' + textOf(c));

    if (!chans.length) {
      statusEl.textContent = '通知渠道未配置';
      statusEl.className = 'pill warn';
    } else if (!active.length) {
      // 全都关着：不是故障，是用户的选择。文案别写成「未连接」。
      statusEl.textContent = '通知已暂停';
      statusEl.className = 'pill warn';
      statusEl.title = lines.join('\n');
    } else if (active.some((c) => c.online)) {
      statusEl.textContent = '已连接';
      statusEl.className = 'pill ok';
      statusEl.title = lines.join('\n');
    } else {
      statusEl.textContent = '未连接';
      statusEl.className = 'pill warn';
      // 完整原因挂在 title 上，鼠标停一下就能看到，不用翻设置页。
      // 飞书那条给不出具体原因（它的 SDK 没有可读的错误状态），
      // 但也不能因此整行消失 —— 否则双渠道里飞书掉了，悬停只看得到钉钉的错。
      statusEl.title = active
        .filter((c) => !c.online)
        .map((c) => c.name + '：' + (c.error || '链路未就绪，稍等片刻或查看容器日志'))
        .join('\n') || lines.join('\n');
    }
  }
}

// 把后端返回的失败原因压成一行，完整内容挂在 title 上。
// 列表里全是「未知」却看不到原因，是最让人抓瞎的情形。
function shortErr(s, n) {
  const t = String(s || '').replace(/\s+/g, ' ').trim();
  const max = n || 64;
  return t.length > max ? t.slice(0, max) + '…' : t;
}

// 用户在飞书卡片上点过「忽略本版本」：远端摘要没再变过，这一版就不再提醒。
//
// 这一行必须显式标出来。否则页面上写着「有更新」、卡片却再也不来，
// 只会让人以为通知坏了或者按钮没生效 —— 其实是自己点过的放行。
function ackIgnored(img) {
  return !img.ignored && !!img.ignored_digest && img.ignored_digest === img.remote_digest;
}

/* 容器列表：一条记录对应一个真实容器，「更新」等于按原配置重建它。 */
function renderContainers() {
  const kw = state.containerSearch.trim().toLowerCase();
  const rows = state.containers.filter((img) => {
    if (!kw) return true;
    return (img.container_name || '').toLowerCase().includes(kw) ||
      (img.reference || '').toLowerCase().includes(kw);
  });

  if (!rows.length) {
    $('#containers-body').innerHTML = `<div class="empty">${state.containers.length ? '没有匹配的容器' : '还没有容器。挂载 docker.sock 后扫描一次即可自动发现。'}</div>`;
    return;
  }

  const html = rows.map((img) => {
    // 「更新」只在该出现的时候出现，不给灰着的占位：
    // 检测模式是自动判定的，「已是最新」根本没有新镜像可拉，摆一个点不动的按钮
    // 只会让人反复去点、点完没反应；「未知」「已忽略」同理 —— 前者是这次没查到，
    // 后者是你自己说了先不动。检测失败的原因已经写在容器名下方的小字里，
    // 不需要再用一个灰按钮来占位提示。跟镜像页是同一条原则。
    const canUpdate = !!img.container_id &&
      (img.status === 'update_available' || img.status === 'new_version_tag');
    const err = img.status === 'unknown' ? (img.last_error || '') : '';
    const acked = ackIgnored(img);

    // 「可用」列的含义是「仓库里有没有更高的版本号」，不是「能拉到什么」。
    // 三种情况要分开写，否则会让人以为没取到数：
    // · 有更高版本 → 显示那个 tag；
    // · 没有更高版本但摘要变了（同一个 tag 被作者重推了内容）→ 写一句「内容已更新」。
    //   这类镜像的 tag 里带的数字是架构而不是版本号（v3-386 / v3-arm64 这种并列发布），
    //   永远不可能有「更高版本」，点更新拉到的还是同一个 tag，只是它的最新内容；
    // · 其余（已是最新 / 未知）→ 留横杠。
    const avail = img.latest_tag
      ? `<div class="cell-main mono">${esc(img.latest_tag)}</div>`
      : (img.status === 'update_available'
        ? `<div class="cell-main hint-text" title="仓库里 ${esc(img.reference)} 这个 tag 已被重新推送，点「更新」拉到它的最新内容，版本号不变">内容已更新</div>`
        : '<div class="cell-main mono">—</div>');
    return `
    <div class="row" data-id="${img.id}">
      <div class="cell grow">
        <div class="cell-main">${esc(img.container_name || '—')}</div>
        <div class="cell-sub">${esc(img.reference)}</div>
        ${err ? `<div class="cell-err" title="${esc(err)}">${esc(shortErr(err))}</div>` : ''}
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">当前</div>
        <div class="cell-main mono">${esc(img.local_tag || '—')}</div>
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">可用</div>
        ${avail}
      </div>
      <div class="cell col-mode">
        <div class="cell-sub">检测模式</div>
        <select class="mode-select" data-mode="${img.id}"
                title="自动：版本号 tag（1.25）走版本监听，latest 等浮动 tag 走摘要比对">
          ${['auto', 'digest-only', 'pin-watch'].map((m) =>
            `<option value="${m}"${(img.mode || 'auto') === m ? ' selected' : ''}>${MODE[m]}</option>`).join('')}
        </select>
      </div>
      <div class="cell col-status center"${err ? ` title="${esc(err)}"` : ''}>${
        acked
          ? '<span class="badge muted" title="你在飞书卡片上点过「忽略本版本」，这版内容不会再提醒；仓库一旦推了新内容，会照常通知">已忽略本版本</span>'
          : badge(STATUS, img.status)}</div>
      <div class="cell actions">
        ${canUpdate ? '<button class="btn small primary" data-act="update">更新</button>' : ''}
        <button class="btn small ghost" data-act="ignore"${
          acked ? ' title="清除「已忽略本版本」，下次扫描若仍是这一版会重新提醒"' : ''}>${
          img.ignored ? '恢复' : (acked ? '恢复提醒' : '忽略')}</button>
        <button class="btn small ghost danger" data-act="remove">移除</button>
      </div>
    </div>`;
  }).join('');

  $('#containers-body').innerHTML = html;
}

/* 镜像列表：本机真实存在的镜像（含没被任何容器使用的），
   外加只在远端盯梢的监控项。这里关心的是占了多少磁盘、能不能清掉。

   这一页刻意**不展示检测状态**（可选更新 / 未检测 / 未知）：
   镜像有没有新版，是「容器」页的问题 —— 那边才有「更新」这个动作，
   点下去会重建容器。镜像页上摆一串状态徽章，加上一个点了只会重新拉取的
   「更新」按钮，等于把两件事混在一起，看的人只会更糊涂。
   磁盘上这一层归谁用、能不能清掉，才是这一页要回答的。 */
function renderImages() {
  const kw = state.imageSearch.trim().toLowerCase();
  const rows = state.images.filter((img) => {
    if (!kw) return true;
    return (img.name || '').toLowerCase().includes(kw) ||
      (img.reference || '').toLowerCase().includes(kw) ||
      (img.id || '').toLowerCase().includes(kw);
  });

  if (!rows.length) {
    $('#images-body').innerHTML = `<div class="empty">${state.images.length ? '没有匹配的镜像' : '本机还没有镜像，或服务尚未连上 Docker。'}</div>`;
    return;
  }

  const html = rows.map((img) => {
    // 统一成「第一行镜像名、第二行镜像 ID」，跟 `docker images` 一眼就能对上号。
    // 悬空镜像（被新版本顶下来的旧层）在 Docker 里没有 tag，但后端已经尽力从
    // 镜像自带的 OCI 标签里挖过名字；实在挖不到才退回显示短 ID —— 一屏好几个
    // <none> 谁也分不清谁，短 ID 至少能唯一指认，也能拿去命令行里对。
    const named = !!img.name;
    const title = named ? img.name
      : (img.remote_only ? (img.reference || '—') : bareImageId(img.id));
    const sub = img.remote_only ? '本机无此镜像，仅盯远端仓库'
      : (img.dangling && !named ? '历史残留镜像层' : shortImageId(img.id));
    const fullRef = img.dangling ? img.id : (img.reference || img.name || '');
    const err = img.last_error || '';

    let usage;
    if (img.remote_only) {
      usage = '<span class="badge muted">仅监控</span>';
    } else if (img.in_used) {
      const names = (img.used_by || []).join('、');
      usage = `<span class="badge ok" title="${esc(names)}">使用中</span>`;
    } else {
      // 悬空镜像一并归到「未使用」：从「能不能清理」的角度看，它和一个没容器
      // 在用的普通镜像没有区别 —— 单摆一个「无 TAG」的徽章反而像是另一类东西。
      // 它没有 tag 这件事，放在副标题里说明一句就够了。
      usage = '<span class="badge warn">未使用</span>';
    }

    // 这一页只剩「删除」一个动作，不多摆按钮占位：
    // 使用中的镜像不给删 —— 删了它，容器下次重启就起不来。这里直接不渲染，
    // 摆个灰按钮仍然会让人去点。复选框同步禁用，免得被「删除所选」批量带走。
    const canDelete = !img.remote_only && !img.in_used;
    const btns = canDelete
      ? ['<button class="btn small ghost danger" data-act="del">删除</button>']
      : [];

    return `
    <div class="row" data-imgid="${esc(img.id)}">
      <div class="cell col-pick">
        <input type="checkbox" data-pick="${esc(img.id)}"
               ${state.picked.has(img.id) ? 'checked' : ''}
               ${canDelete ? '' : 'disabled'}
               title="${canDelete ? '选中后可批量删除' : (img.remote_only ? '纯远端监控项，本机没有可删的镜像' : '镜像正在被容器使用，不能删除')}">
      </div>
      <div class="cell grow">
        <div class="cell-main${!named && !img.remote_only ? ' mono' : ''}" title="${esc(fullRef)}">${esc(title)}</div>
        <div class="cell-sub mono" title="${esc(img.id)}">${esc(sub)}</div>
        ${err ? `<div class="cell-err" title="${esc(err)}">${esc(shortErr(err))}</div>` : ''}
      </div>
      <div class="cell col-usage">${usage}</div>
      <div class="cell col-tag">
        <div class="cell-sub">TAG</div>
        <div class="cell-main mono">${esc(img.tag || '—')}</div>
      </div>
      <div class="cell col-size">
        <div class="cell-sub">大小</div>
        <div class="cell-main">${esc(fmtSize(img.size_bytes))}</div>
      </div>
      <div class="cell col-created">
        <div class="cell-sub">创建时间</div>
        <div class="cell-main">${esc(fmtTime((img.created || 0) * 1000))}</div>
      </div>
      <div class="cell actions">${btns.length ? btns.join('') : '<span class="no-action">—</span>'}</div>
    </div>`;
  }).join('');

  $('#images-body').innerHTML = html;
}

// nodeTag 标出这条记录发生在哪台机器上。
//
// 本机不加标签：绝大多数记录都是本机的，每行都挂一个「本机」只是噪声；
// 真正需要区分的是「这条其实是在另一台机器上跑的」。
function nodeTag(id, name) {
  if (!id) return '';
  return ` <span class="badge muted">${esc(name || '节点 ' + id)}</span>`;
}

function renderTasks() {
  if (!state.tasks.length) {
    $('#tasks-body').innerHTML = '<div class="empty">还没有更新记录。</div>';
    return;
  }
  $('#tasks-body').innerHTML = state.tasks.map((t) => `
    <div class="row clickable" data-task="${t.id}">
      <div class="cell narrow mono">#${t.id}</div>
      <div class="cell grow">
        <div class="cell-main">${esc(t.container_name || '—')}${nodeTag(t.node_id, t.node_name)}</div>
        <div class="cell-sub">${esc(t.image_ref)}</div>
      </div>
      <div class="cell col-status center">${badge(TASK, t.status)}</div>
      <div class="cell col-src">
        <div class="cell-sub">来源</div>
        <div class="cell-main">${esc(t.triggered_by || '—')}</div>
      </div>
      <div class="cell col-cost">
        <div class="cell-sub">耗时</div>
        <div class="cell-main">${esc(fmtDuration(t.started_at, t.finished_at) || '—')}</div>
      </div>
      <div class="cell col-time">
        <div class="cell-sub">时间</div>
        <div class="cell-main">${esc(fmtTime(t.created_at))}</div>
      </div>
    </div>
  `).join('');
}

// notifUpdateBtn 一条提醒上的「更新」入口。
//
// 待处理时是「立即更新」，卡在更新中时是「重试更新」——后者只在真的没在跑
// 时才会生效，服务端会先确认一遍。失败 / 已回滚的也能重来：那次失败多半是
// 一次性的（镜像源抽了一下、目标机磁盘满），修好之后最自然的动作就是原地点一次，
// 而不是先把记录删掉再等下一轮巡检重新推。已忽略 / 已稍后 / 已完成的不给按钮：
// 那些状态下再点一次没有意义，只会让人以为能重来。
function notifUpdateBtn(n) {
  if (n.card_state === 'pending') {
    return `<button class="btn small primary" data-notif-update="${n.id}"
      title="在这条提醒对应的机器上按原配置重建容器">立即更新</button>`;
  }
  if (n.card_state === 'updating' || n.card_state === 'failed' || n.card_state === 'rollback') {
    const tip = n.card_state === 'updating'
      ? '如果那次更新其实已经结束（服务重启、连接断开留下的状态），点它重新下发一遍'
      : '重新下发一次更新；上次失败的原因写在下面那行红字里';
    return `<button class="btn small ghost" data-notif-update="${n.id}" title="${tip}">重试更新</button>`;
  }
  return '';
}

// notifDetail 上一次更新的结果说明。
//
// 只在失败与回滚时露出来：成功那句「已重建完成」每行都挂一遍只是噪声。
// 而失败时它就是唯一能照做的线索 —— 网页上原来只有一个「失败」徽章，
// 用户既看不出是拉不到镜像、还是容器起不来，也没法把原因贴出来求助。
//
// 老记录（这一版之前失败的）没有存原因，给一句能照着做的话：
// 让用户知道点上「重试更新」跑一次就能拿到真正的报错。
function notifDetail(n) {
  if (n.card_state !== 'failed' && n.card_state !== 'rollback') return '';
  if (!n.card_detail) {
    return '<div class="notif-detail dim">这次失败发生在旧版本，原因没有记录。点「重试更新」重新执行一次，若仍失败会在下面写出具体原因。</div>';
  }
  return `<div class="notif-detail">${esc(n.card_detail)}</div>`;
}

// 合并提醒里每个容器自己的结果。
const MEMBER_STATE = {
  pending: { text: '待更新', cls: 'warn' },
  updating: { text: '更新中', cls: 'info' },
  done: { text: '已更新', cls: 'ok' },
  failed: { text: '失败', cls: 'danger' },
};

// notifMembers 合并提醒的成员清单。
//
// 单容器提醒不显示：那一行本来就只有它自己，再列一遍纯属噪声。
// 合并的必须列出来 —— 卡片上写着「6 个容器」，用户得能看清是哪 6 个、
// 哪个成功了、哪个卡住了，否则「部分成功」只是一句没法验证的话。
function notifMembers(n) {
  if (!n.members || n.members.length < 2) return '';
  const items = n.members.map((m) => {
    const st = MEMBER_STATE[m.state] || { text: m.state || '待更新', cls: 'warn' };
    const name = esc(m.container_name || m.image_ref);
    const tip = m.error ? ` title="${esc(m.error)}"` : '';
    return `<span class="notif-member"${tip}>${name}<span class="badge ${st.cls}">${st.text}</span></span>`;
  }).join('');
  return `<div class="notif-members">${items}</div>`;
}

function notifSub(n) {
  const count = n.members && n.members.length ? n.members.length : (n.member_count || 1);
  if (count > 1) {
    return `${esc(n.compose_project || '同一个项目')} · ${count} 个容器`;
  }
  return `${esc(n.container_name || '（远端监控）')} · ${esc(n.image_ref)}`;
}

function renderNotifications() {
  if (!state.notifications.length) {
    $('#notifications-body').innerHTML = '<div class="empty">暂无通知。</div>';
    return;
  }
  $('#notifications-body').innerHTML = state.notifications.map((n) => `
    <div class="row notif-row">
      <div class="cell grow">
        <div class="cell-main">${n.read ? '' : '<span class="dot"></span>'}${esc(n.title || n.image_ref)}</div>
        <div class="cell-sub">${notifSub(n)}</div>
        ${notifMembers(n)}
        ${notifDetail(n)}
      </div>
      <div class="cell col-kind">
        <div class="cell-sub">类型</div>
        <div class="cell-main">${esc(KIND[n.kind] || n.kind)}</div>
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">版本</div>
        ${n.latest_tag
          ? `<div class="cell-main mono">${esc(n.latest_tag)}</div>`
          : (n.kind === 'new_version'
            ? '<div class="cell-main hint-text" title="这条通知说的是：仓库里同一个 tag 的内容变了，没有新的版本号">内容已更新</div>'
            : '<div class="cell-main mono">—</div>')}
      </div>
      <div class="cell col-status center">${badge(CARD, n.card_state)}</div>
      <div class="cell col-time">
        <div class="cell-sub">时间</div>
        <div class="cell-main">${esc(fmtTime(n.created_at))}</div>
      </div>
      <div class="cell actions">
        ${notifUpdateBtn(n)}
        ${n.read ? '' : `<button class="btn small ghost" data-read="${n.id}">标为已读</button>`}
        <button class="btn small ghost danger" data-notif-del="${n.id}"
          title="只删这条记录；同一版本若仍可更新，下一轮巡检会再提醒一次">删除</button>
      </div>
    </div>
  `).join('');
}

/* ---------- 远程节点 ---------- */

// 节点状态与容器状态是两件事：前者讲「这台机器连不连得上」，
// 后者讲「它上面的容器该不该更新」。混用一套颜色会让人把「离线」
// 误读成「没更新」。
const NODE_STATUS = {
  online: { text: '在线', cls: 'ok' },
  offline: { text: '离线', cls: 'warn' },
  never: { text: '未连接', cls: 'muted' },
};

// 判断一个时间字段到底有没有值。
//
// Go 的零值时间序列化成 "0001-01-01T00:00:00Z"，在 JS 里是 truthy ——
// 直接写 `if (v)` 会把「从未发生过」当成 0001 年 1 月 1 日显示出来。
function hasTime(v) {
  return !!v && String(v).slice(0, 4) !== '0001';
}

function renderNodes() {
  const box = $('#nodes-body');
  if (!box) return;

  // 顶部那排统计卡只数本机 —— 装上远程节点之后，那里会一直是 0，
  // 容易让人以为整个系统没有待更新项。所以在这里单独给一行合计，
  // 把口径摆明：本机和远端分开算，各自都不含糊。
  const summary = $('#nodes-summary');
  if (summary) {
    if (!state.nodes.length) {
      summary.textContent = '';
    } else {
      const total = state.nodes.reduce((n, x) => n + (x.container_count || 0), 0);
      const upd = state.nodes.reduce((n, x) => n + (x.update_count || 0), 0);
      summary.textContent = `${state.nodesOnline} / ${state.nodes.length} 台在线 · 合计 ${total} 个容器 · ${upd} 项待更新`;
    }
  }

  if (!state.nodes.length) {
    box.innerHTML = '<div class="empty">还没有远程节点。点右上角「添加节点」，把生成的命令粘到目标机上执行一次即可。</div>';
    return;
  }

  box.innerHTML = state.nodes.map((n) => {
    const st = NODE_STATUS[n.status] || NODE_STATUS.never;
    const open = state.nodeOpen.has(n.id);
    const list = state.nodeOpen.get(n.id);
    // 心跳只证明连着，快照才证明容器数据是新的。刚装好、或巡检正跑第一轮时
    // 快照还没到，这时候显示 0 会让人以为这台机器空着。
    const pending = n.status === 'online' && !hasTime(n.last_report_at);
    // ops 的 key 是数字，经过 JSON 之后成了字符串，取的时候必须 String()
    const op = state.nodeOps[String(n.id)];
    const batch = state.nodeBatches[String(n.id)];
    const upgradable = state.nodeUpgradable.has(n.id);
    const upgrading = state.nodeUpgrading.has(n.id);
    const lastUp = state.nodeUpgradeLast[String(n.id)];
    const loadErr = state.nodeErr.get(n.id);
    // 版本落后时直接把「现在是哪一版、要升到哪一版」摆在 meta 行里，
    // 用户不用去猜「升级」按钮会升到什么版本。
    const verText = n.agent_version
      ? 'Agent ' + n.agent_version + (upgradable && state.hubVersion ? ' → ' + state.hubVersion : '')
      : '';
    const meta = [n.hostname, n.os, verText].filter(Boolean).join(' · ');
    // 上次升级失败的原因留在卡片上：升级失败时节点版本没变，
    // 除了这一行，页面上看不出任何「刚才试过但没成」的痕迹。
    const upNote = (!upgrading && lastUp && !lastUp.ok)
      ? `<div class="cell-err" title="${esc(lastUp.error || '')}">上次升级未成功：${esc(shortErr(lastUp.error || '未知原因'))}</div>`
      : '';
    const detail = open
      ? `<div class="node-detail">${loadErr
        ? `<div class="node-load-err">读取节点容器失败：${esc(loadErr)}
            <button class="btn small ghost" data-node-reload="${n.id}">重试</button></div>`
        : list === null
          ? '<div class="empty">正在读取…</div>'
          : (pending && !list.length ? '<div class="empty">正在巡检，结果稍后自动出现。</div>' : nodeContainerRows(list, n, op, batch))}</div>`
      : '';
    return `
    <div class="node-card">
      <div class="node-head" data-node-toggle="${n.id}">
        <div class="cell grow">
          <div class="cell-main">
            ${esc(n.name || '未命名节点')}
            <span class="badge ${st.cls}">${st.text}</span>
          </div>
          <div class="cell-sub">${esc(meta || '尚未上报信息')}</div>
          <div class="cell-sub mono">${esc(n.hub_url || '—')}</div>
          ${upNote}
        </div>
        <div class="cell col-tag">
          <div class="cell-sub">容器</div>
          <div class="cell-main">${pending
            ? '<span class="hint-text" title="已连上，正在跑第一轮巡检">巡检中…</span>'
            : (n.container_count || 0)}</div>
        </div>
        <div class="cell col-tag">
          <div class="cell-sub">待更新</div>
          <div class="cell-main">${pending ? '—' : (n.update_count ? `<span class="badge warn">${n.update_count}</span>` : '0')}</div>
        </div>
        <div class="cell col-time">
          <div class="cell-sub">最后心跳</div>
          <div class="cell-main">${hasTime(n.last_seen_at) ? fmtTime(n.last_seen_at) : '—'}</div>
        </div>
        <div class="cell actions">
          ${(upgradable || upgrading) ? `<button class="btn small ${upgrading ? 'ghost' : 'primary'}"
            data-node-upgrade="${n.id}" ${(upgrading || n.status !== 'online') ? 'disabled' : ''}
            title="把这台机器上的 Agent 换成 Hub 这一版，节点会重启一次">${upgrading ? '升级中…' : '升级 Agent'}</button>` : ''}
          <button class="btn small ghost" data-node-scan="${n.id}" ${n.status === 'online' ? '' : 'disabled'}>立即巡检</button>
          <button class="btn small ghost" data-node-token="${n.id}">重发命令</button>
          <button class="btn small ghost danger" data-node-del="${n.id}">删除</button>
        </div>
      </div>
      ${opBanner(op)}
      ${detail}
    </div>`;
  }).join('');
}

// opBanner 一条「这台机器此刻在干什么」的横幅。
//
// 不做进度条：拉镜像的层进度对用户没有意义（99% 卡十分钟是常态），
// 反而让人以为出问题了。给当前动作的一句话就够，动没动看得见。
function opBanner(op) {
  if (!op) return '';
  const running = op.phase !== 'done' && op.phase !== 'failed';
  const cls = op.phase === 'failed' ? 'danger' : (running ? 'busy' : 'ok');
  const label = op.phase === 'failed' ? '更新失败' : (running ? '正在更新' : '更新完成');
  const who = op.container_name ? ' ' + op.container_name : '';
  // 只在「有额外信息」的阶段显示后半句：进行中显示当前动作、失败显示原因。
  // start 的「开始更新 X」和 done 的「更新完成」跟前面的标签重复，说了等于没说。
  const detail = op.phase === 'failed' || op.phase === 'progress';
  const msg = detail && op.message ? `<span class="node-op-msg">${esc(op.message)}</span>` : '';
  return `<div class="node-op ${cls}">
    <span class="node-op-dot"></span>
    <b>${label}${esc(who)}</b>${msg}
  </div>`;
}

// nodeContainerRows 渲染某台机器上的容器。
//
// 只列容器维度的项（origin=container）：镜像维度那几条（本机存在但没人用的镜像、
// 纯远端盯梢）在节点页没有操作入口，摆出来只会让人以为它们也能在这里更新。
//
// 同一个 compose 项目里有多个容器时合成一行：safeline 这种项目一次发版就是
// 六个服务，六个「更新」按钮要挨个点六次，而且每点一次还要等它跑完再点下一个。
function nodeContainerRows(list, node, op, batch) {
  const items = (list || []).filter((c) => c.origin === 'container');
  if (!items.length) return '<div class="empty">这台机器上除 Agent 自身外没有其它容器。</div>';
  const rows = groupNodeItems(items)
    .map((g) => (g.members.length > 1 ? nodeProjectRow(g, node, op, batch) : nodeOneRow(g.members[0], node, op)))
    .join('');
  return `<div class="node-containers">${rows}</div>`;
}

// groupNodeItems 按 compose 项目归组，顺序沿用后端给的顺序。
//
// 只有一个容器的项目不合并成「项目行」：合并等于什么也没做，
// 反而把用户认得的容器名藏起来了。
function groupNodeItems(items) {
  const groups = new Map();
  const order = [];
  items.forEach((c) => {
    const key = c.compose_project || '';
    if (!key) {
      order.push({ project: '', members: [c] });
      return;
    }
    let g = groups.get(key);
    if (!g) {
      g = { project: key, members: [] };
      groups.set(key, g);
      order.push(g);
    }
    g.members.push(c);
  });
  return order.map((g) => (g.members.length > 1 ? g : { project: '', members: g.members }));
}

function hasUpdate(c) {
  return c.status === 'update_available' || c.status === 'new_version_tag';
}

function nodeOneRow(c, node, op) {
  // 查不动的那一个：状态写「检测异常」而不是「未知」，原因整句铺出来。
  //
  // 截到 64 字会把真正有用的那句切掉 —— 镜像源的错误通常是「所有镜像源均失败：
  // 源 A（401 要求登录认证）；源 B（403 免费节点当前繁忙）」，后半截才是能照着做的。
  const bad = c.status === 'unknown' || !!c.last_error;
  const shown = bad ? 'check_error' : c.status;
  return `
    <div class="row">
      <div class="cell grow">
        <div class="cell-main">${esc(c.container_name || '—')}</div>
        <div class="cell-sub">${esc(c.image_ref)}</div>
        ${c.last_error ? `<div class="cell-err wrap" title="${esc(c.last_error)}">${esc(shortErr(c.last_error, 110))}</div>` : ''}
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">当前</div>
        <div class="cell-main mono">${esc(c.local_tag || '—')}</div>
      </div>
      <div class="cell col-tag">
        <div class="cell-sub">可用</div>
        <div class="cell-main mono">${esc(c.latest_tag || (c.status === 'update_available' ? '内容已更新' : '—'))}</div>
      </div>
      <div class="cell col-mode">${badge(STATUS, shown)}</div>
      <div class="cell actions">${nodeUpdateButton(c, node, op)}</div>
    </div>`;
}

// batchFailText 把批次的失败原因拼成一句话。
//
// 预拉阶段本来就没开始重建，写「X 更新失败：拉取镜像失败：…」会读成两件坏事；
// 这时直接说「X 拉取镜像失败：…」，后面那句「整批没有改动」才是重点。
function batchFailText(b) {
  const raw = b.error || '未知原因';
  if (raw.startsWith('拉取镜像失败')) return `${b.failed} ${raw}`;
  // 撞上另一把锁：镜像没拉、容器也没重建，说「X 更新失败」会让人以为 X 已经动过了。
  // 这句话本身已经带上了是谁占着锁，原样显示比在前面再挂一个容器名更清楚。
  if (raw.endsWith('（整批没有改动）')) return raw;
  return `${b.failed} 更新失败：${raw}`;
}

// shortContainerName 去掉容器名前面的项目名。
//
// 组标题已经写着 safeline 了，括号里再写一遍 safeline-pg 只会把这一格撑爆。
function shortContainerName(name, project) {
  if (!name) return '';
  for (const sep of ['-', '_']) {
    const prefix = project + sep;
    if (project && name.startsWith(prefix)) return name.slice(prefix.length);
  }
  return name;
}

// groupTags 拼出「当前」那一格的版本。
//
// 以前版本不一致就只写「多个版本」—— 等于没说：用户看不出谁是少数派，
// 也判断不了该不该动手。而实际情形常常是两套彼此无关的版本线
// （safeline 的 pg 是 15.x，其余组件是 9.x），那本来就不是「有问题」。
// 取出现最多的当主版本，其余挂在括号里。名字写全会把这一格撑成三行，
// 超过一行半就只留版本号 —— 谁在哪个版本上，左边那行容器清单与悬停提示里查得到。
function groupTags(members, project) {
  const tags = [...new Set(members.map((m) => m.local_tag).filter(Boolean))];
  if (!tags.length) return { text: '—', title: '' };
  if (tags.length === 1) return { text: tags[0], title: '' };

  const byTag = new Map();
  for (const m of members) {
    if (!m.local_tag) continue;
    if (!byTag.has(m.local_tag)) byTag.set(m.local_tag, []);
    byTag.get(m.local_tag).push(shortContainerName(m.container_name, project));
  }
  // 出现最多的当主版本；打平时取先出现的那个，保证同一份数据每轮渲染都一样。
  let main = tags[0];
  for (const [t, names] of byTag) {
    if (names.length > byTag.get(main).length) main = t;
  }

  const detailed = [];
  const plain = [];
  for (const [t, names] of byTag) {
    if (t === main) continue;
    plain.push(t);
    detailed.push(names.length === 1 ? `${names[0]} ${t}` : `${names[0]} 等 ${names.length} 个 ${t}`);
  }
  const detail = detailed.join('、').length <= 24
    ? detailed.join('、')
    : `另有 ${plain.slice(0, 3).join('、')}${plain.length > 3 ? ` 等 ${plain.length} 种` : ''}`;
  return {
    text: `${main}（${detail}）`,
    title: members.map((m) => `${m.container_name} ${m.local_tag || '—'}`).join('\n'),
  };
}

// nodeProjectRow 一个 compose 项目一行，一个按钮更新整个项目。
//
// 「可用」那一格只在所有成员的目标版本一致时才写 tag —— 版本各不相同的时候
// 挑一个写上，等于给用户一个并不存在的结论。
function nodeProjectRow(g, node, op, batch) {
  const members = g.members;
  const pending = members.filter(hasUpdate);
  const tagCell = groupTags(members, g.project);
  const targets = [...new Set(pending.map((m) => m.latest_tag || '内容已更新'))];
  const mine = batch && batch.project === g.project ? batch : null;
  const runningHere = !!(mine && mine.running);
  // 更新失败的那一行只在刚跑完那阵子留着：隔了一天的红字会让人以为刚才又失败了。
  const fresh = mine && mine.updated_at && (Date.now() - Date.parse(mine.updated_at) < 30 * 60 * 1000);
  const failLine = (mine && !mine.running && mine.failed && fresh)
    ? `<div class="cell-err wrap" title="${esc(mine.error || '')}">${esc(batchFailText(mine))}</div>`
    : '';
  // 查不动的成员：有明确原因的（last_error），或者连状态都没查出来的（unknown）。
  //
  // 只数有个数是不够的 —— 「2 个容器检测异常」既不说哪两个、也不说为什么，
  // 用户手里没有任何可动手的线索，只能看着这行红字一天天挂着。
  const errMembers = members.filter((m) => m.last_error || m.status === 'unknown');
  const errLine = (!failLine && errMembers.length)
    ? errMembers.slice(0, 3).map((m) => {
      const why = m.last_error ? shortErr(m.last_error, 110) : '这次没查出结果，状态未知';
      return `<div class="cell-err wrap" title="${esc(m.last_error || '')}">${esc(m.container_name)}：${esc(why)}</div>`;
    }).join('') + (errMembers.length > 3 ? `<div class="cell-err">另有 ${errMembers.length - 3} 个容器没查出结果</div>` : '')
    : '';

  // 有成员没查成功时，徽章不能写「已是最新」。
  //
  // 下面那行红字正说着「检测异常」，徽章却是一片绿 —— 一绿一红摆在一起，
  // 用户根本不知道这一行到底能不能更新。缺结果就是缺结果，单独一个状态。
  const status = pending.length
    ? 'update_available'
    : (errMembers.length
      ? 'check_error'
      : (members.every((m) => m.status === 'up_to_date') ? 'up_to_date' : members[0].status));
  const avail = targets.length === 1 ? targets[0] : (pending.length ? `${pending.length} 项更新` : '—');

  let btn = '';
  if (runningHere) {
    // 预拉阶段容器一个都没动，数字得说「拉了几个镜像」而不是「更新了几个容器」，
    // 否则用户盯着 0/7 会以为卡住了。
    const label = mine.stage === '拉取镜像'
      ? `拉取镜像 ${mine.pulled || 0}/${mine.total}`
      : `更新中 ${mine.done}/${mine.total}`;
    btn = `<button class="btn small" disabled>${label}</button>`;
  } else if (pending.length) {
    // 失败之后按钮仍然给出来：用户看完原因下一步就是再点一次，
    // 而批处理会跳过已经成功的那些，重试不会白跑。
    btn = nodeProjectButton(node, g, op);
  }

  return `
    <div class="row">
      <div class="cell grow">
        <div class="cell-main">${esc(g.project)}<span class="node-proj-count">${members.length} 个容器</span></div>
        <div class="cell-sub">${members.map((m) => esc(m.container_name)).join(' · ')}</div>
        ${failLine}${errLine}
      </div>
      <div class="cell col-tag tag-wide">
        <div class="cell-sub">当前</div>
        <div class="cell-main mono${tagCell.title ? ' tag-mixed' : ''}"${tagCell.title ? ` title="${esc(tagCell.title)}"` : ''}>${esc(tagCell.text)}</div>
      </div>
      <div class="cell col-tag tag-wide">
        <div class="cell-sub">可用</div>
        <div class="cell-main mono">${esc(avail)}</div>
      </div>
      <div class="cell col-mode">${badge(STATUS, status)}</div>
      <div class="cell actions">${btn}</div>
    </div>`;
}

// nodeProjectButton 整项目的更新按钮。
//
// 一台机器同时只跑一个更新，所以别的容器在跑时这里一律置灰；
// 文案保持「更新」而不是「更新中」，避免让人以为点一次把整台机器都动了。
function nodeProjectButton(node, g, op) {
  const running = !!op && (op.phase !== 'done' && op.phase !== 'failed');
  const disabled = running || node.status !== 'online' || !g.members.some((m) => m.container_id);
  return `<button class="btn small" data-node-project="${node.id}" data-project="${esc(g.project)}"
    ${disabled ? 'disabled' : ''}>更新</button>`;
}

// nodeUpdateButton 只在「确实有更新」的行上给按钮。
//
// 已是最新、检测失败、已忽略的行都不给 —— 一个点了也不会有任何效果的按钮，
// 比没有按钮更让人困惑。
//
// 「更新中」只贴给真正在跑的那一行。一台机器同时只跑一个更新，所以其余行
// 一律置灰，但文案保持「更新」：它们并没有在更新，写「更新中」会让人以为
// 点了一个按钮把整台机器的容器全动了。
function nodeUpdateButton(c, node, op) {
  const has = c.status === 'update_available' || c.status === 'new_version_tag';
  if (!has || c.ignored || !c.container_id) return '';
  const running = !!op && (op.phase !== 'done' && op.phase !== 'failed');
  const mine = running && op.container_id === c.container_id;
  const disabled = running || node.status !== 'online';
  return `<button class="btn small" data-node-update="${node.id}" data-cid="${esc(c.container_id)}"
    ${disabled ? 'disabled' : ''}>${mine ? '更新中' : '更新'}</button>`;
}

async function loadNodes() {
  const data = await api('/api/nodes');
  state.nodes = data.nodes || [];
  state.nodesOnline = data.online || 0;
  // ops 的 key 是节点 ID 的字符串形式，取的时候用 String(n.id)，别写 n.id
  state.nodeOps = data.ops || {};
  state.nodeBatches = data.batches || {};
  state.hubVersion = data.hub_version || '';
  state.nodeUpgradable = new Set(data.upgradable || []);
  state.nodeUpgrading = new Set(data.upgrading || []);
  state.nodeUpgradeLast = data.upgrade_last || {};
  renderNodes();
}

// refreshNodes 手动刷新节点列表。
//
// 自动轮询在空闲时要隔好几轮才做一次全量刷新，刚在目标机上装完 Agent
// 的人等不了那几十秒；这个按钮让「装完立刻回来看」有明确的动作可做。
async function refreshNodes(opts) {
  const quiet = !!(opts && opts.quiet);
  const btn = $('#btn-refresh-nodes');
  const label = btn ? btn.textContent : '';
  if (btn && !quiet) {
    btn.disabled = true;
    btn.textContent = '刷新中…';
  }
  try {
    await loadNodes();
    if (!quiet) toast('已刷新');
  } catch (e) {
    if (!quiet) toast('刷新失败：' + e.message, 'danger');
  } finally {
    if (btn && !quiet) {
      btn.disabled = false;
      btn.textContent = label || '刷新';
    }
  }
}

async function toggleNode(id) {
  if (state.nodeOpen.has(id)) {
    state.nodeOpen.delete(id);
    state.nodeErr.delete(id);
    renderNodes();
    return;
  }
  await loadNodeContainers(id);
}

// loadNodeContainers 拉一个节点的容器快照并重绘。
//
// 先放 null 占位，界面立刻展开并显示「正在读取」，
// 而不是点一下没反应、等接口回来才突然弹出来。
// 失败时把原因留在 state.nodeErr 里而不是把列表当空的渲染 ——
// 「读不出来」和「真的没有容器」长得一样的话，用户会被引到错误的方向上去。
async function loadNodeContainers(id) {
  state.nodeErr.delete(id);
  state.nodeOpen.set(id, null);
  renderNodes();
  try {
    const data = await api(`/api/nodes/${id}/containers`);
    state.nodeOpen.set(id, data.containers || []);
  } catch (e) {
    state.nodeErr.set(id, e.message);
    state.nodeOpen.set(id, []);
    toast('读取节点容器失败：' + e.message, 'danger');
  }
  renderNodes();
}

function openNodeForm() {
  const form = $('#node-form');
  if (!form) return;
  form.hidden = false;
  $('#node-result').hidden = true;
  $('#node-result').innerHTML = '';
  resetNodeCreateButton();
  // 预填当前访问地址：从哪儿打开的这个页面，多半就是目标机能访问到的地址。
  // 用户可以直接改，不强制。
  const hub = $('#node-hub');
  if (!hub.value) {
    try { hub.value = location.origin; } catch (_) { /* 某些内嵌浏览器不给 origin */ }
  }
  $('#node-name').focus();
}

function closeNodeForm() {
  const form = $('#node-form');
  if (form) form.hidden = true;
  resetNodeCreateButton();
}

// 生成过命令之后就把「生成」按住，避免用户重复点出第二个同名节点。
// 想再建一个先关掉这个表单（取消）再重新打开。
function markNodeCommandShown() {
  const btn = $('#btn-node-create');
  if (btn) {
    btn.disabled = true;
    btn.textContent = '已生成命令';
  }
}

function resetNodeCreateButton() {
  const btn = $('#btn-node-create');
  if (btn) {
    btn.disabled = false;
    btn.textContent = '生成安装命令';
  }
}

// showNodeCommand 展示安装命令。extra 是给表单下方追加的一行补充说明，
// 只在「新建节点」这条路径上传 —— 重发令牌时按钮状态不同，说的是另一回事。
function showNodeCommand(command, extra) {
  const box = $('#node-result');
  if (!box) return;
  box.hidden = false;
  box.innerHTML = `
    <div class="hint" style="margin:10px 0 6px">在目标机上执行这一条命令。令牌只显示这一次，关掉后就拿不回来了。</div>
    <pre class="node-cmd">${esc(command)}</pre>
    <button class="btn small" data-copy-node-cmd>复制命令</button>
    ${extra ? `<div class="hint" style="margin:10px 0 0">${esc(extra)}</div>` : ''}`;
}

async function copyNodeCommand() {
  const pre = $('#node-result .node-cmd');
  if (!pre) return;
  const ok = await copyText(pre.textContent);
  // 复制回落路径（execCommand）在某些浏览器上静默失败，必须给明确结果，
  // 否则用户以为粘走了，到目标机上执行的是上一份东西。
  toast(ok ? '安装命令已复制' : '复制失败，请手动选中命令', ok ? 'ok' : 'warn');
}

async function createNode() {
  const name = ($('#node-name').value || '').trim();
  const hub = ($('#node-hub').value || '').trim();
  if (!name) {
    toast('请填写节点名称', 'warn');
    return;
  }
  if (!/^https?:\/\//i.test(hub)) {
    toast('Hub 访问地址要以 http:// 或 https:// 开头', 'warn');
    return;
  }
  const btn = $('#btn-node-create');
  btn.disabled = true;
  btn.textContent = '生成中…';
  let created = false;
  try {
    const res = await api('/api/nodes', {
      method: 'POST',
      body: JSON.stringify({ name, hub_url: hub }),
    });
    created = true;
    showNodeCommand(res.command, '要再添加一台机器，点「取消」关掉这个表单再重新打开。');
    // 命令只在这一刻存在。按钮留着不禁用的话，用户很容易再点一次，
    // 于是多出一个同名节点和一条永远用不上的旧令牌。
    markNodeCommandShown();
  } catch (e) {
    toast('创建失败：' + e.message, 'danger');
  } finally {
    // 节点其实已经建好了，只是刷新列表失败，不能提示「创建失败」，
    // 也不能把按钮放开——那样用户会再建一个重复节点。
    if (!created) resetNodeCreateButton();
  }
  if (created) {
    try {
      await loadNodes();
    } catch (_) { /* 列表下一轮轮询会补上，不影响命令本身 */ }
  }
}

// nodeUpgrade 让一台节点把自己换成 Hub 这一版。
//
// 接口只等到「准备工作做完」：真正换容器是在连接断开之后发生的，
// 所以这里说「正在升级」，之后靠轮询看版本号自己变过去。
async function nodeUpgrade(id) {
  const n = state.nodes.find((x) => x.id === id);
  const name = n ? n.name : '节点';
  try {
    const data = await api(`/api/nodes/${id}/upgrade`, { method: 'POST' });
    toast(data.message || `正在升级「${name}」，约 10 秒后自动回来`);
    // Agent 会被停掉再拉起来，这段时间列表里是离线状态 —— 等它回来再刷两次。
    setTimeout(() => refreshNodes({ quiet: true }), 8000);
    setTimeout(() => refreshNodes({ quiet: true }), 25000);
  } catch (e) {
    toast('无法升级：' + e.message, 'danger');
  }
}

async function nodeScan(id) {
  const n = state.nodes.find((x) => x.id === id);
  try {
    await api(`/api/nodes/${id}/scan`, { method: 'POST' });
    toast(`已通知「${n ? n.name : '节点'}」巡检，结果稍后自动刷新`);
  } catch (e) {
    toast('无法巡检：' + e.message, 'danger');
  }
}

// nodeUpdate 在远端机器上重建一个容器。
//
// 接口会一直等到 Agent 回执才返回，所以这里的失败提示是准的 ——
// 「节点不在线」「Agent 版本过旧」「已经有任务在跑」都会当场说出来，
// 而不是让用户对着一条挂几分钟的横幅猜。
async function nodeUpdate(nodeID, containerID) {
  const card = state.nodes.find((x) => x.id === nodeID);
  const name = card ? card.name : '节点';
  try {
    const data = await api(`/api/nodes/${nodeID}/update`, {
      method: 'POST',
      body: JSON.stringify({ container_id: containerID }),
    });
    toast(`${name}：${data.message || '已开始更新'}`);
    // 立刻刷一次，让「正在更新」的横幅尽快出现；进度靠轮询继续追。
    await refreshNodes({ quiet: true });
    await refreshOpenNodeContainers(nodeID);
  } catch (e) {
    toast('更新失败：' + e.message, 'danger');
  }
}

// nodeProjectUpdate 一个 compose 项目点一次，服务端依次重建里面的容器。
//
// 请求只等「已开始」：六个容器全跑完是十几分钟，HTTP 挂那么久会被浏览器、
// 反代、飞牛壳各掐一次，用户看到的是「点了没反应」。进度靠轮询：
// 节点卡片上的「更新中 2/6」与容器行状态自己会往前走。
async function nodeProjectUpdate(nodeID, project) {
  const card = state.nodes.find((x) => x.id === nodeID);
  const name = card ? card.name : '节点';
  try {
    const data = await api(`/api/nodes/${nodeID}/update`, {
      method: 'POST',
      body: JSON.stringify({ compose_project: project }),
    });
    toast(`${name}：${data.message || '已开始更新'}（${project}）`);
    await refreshNodes({ quiet: true });
    await refreshOpenNodeContainers(nodeID);
  } catch (e) {
    toast('更新失败：' + e.message, 'danger');
  }
}

// refreshOpenNodeContainers 重拉某个已展开节点的容器列表。
// 更新会把整行状态换掉（待更新 → 已是最新），不重拉的话页面停在上一次的样子。
async function refreshOpenNodeContainers(nodeID) {
  if (!state.nodeOpen.has(nodeID)) return;
  try {
    const data = await api(`/api/nodes/${nodeID}/containers`);
    state.nodeOpen.set(nodeID, data.containers || []);
    state.nodeErr.delete(nodeID);
    renderNodes();
  } catch (_) { /* 下一轮轮询会再试 */ }
}

// refreshOpenNodes 把所有展开着的节点容器一起重拉。
// 只在有操作进行中时调用，避免平白多打请求。
async function refreshOpenNodes() {
  const ids = [...state.nodeOpen.keys()];
  if (!ids.length) return;
  let changed = false;
  await Promise.all(ids.map(async (id) => {
    try {
      const data = await api(`/api/nodes/${id}/containers`);
      state.nodeOpen.set(id, data.containers || []);
      state.nodeErr.delete(id);
      changed = true;
    } catch (_) { /* 忽略，下一轮再说 */ }
  }));
  if (changed) renderNodes();
}

async function rotateNodeToken(id) {
  if (!confirm('重新生成令牌会让这个节点立刻掉线，必须在目标机上重跑一次新命令。继续？')) return;
  try {
    const res = await api(`/api/nodes/${id}/token`, { method: 'POST' });
    const n = state.nodes.find((x) => x.id === id);
    openNodeForm();
    if (n) {
      $('#node-name').value = n.name || '';
      if (n.hub_url) $('#node-hub').value = n.hub_url;
    }
    showNodeCommand(res.command);
    markNodeCommandShown();
    toast('新令牌已生成，旧令牌已失效');
  } catch (e) {
    toast('生成失败：' + e.message, 'danger');
  }
}

// UNINSTALL_CMD 目标机上卸载 Agent 的命令。
//
// 与文档、README 里给的那条保持一致：容器删掉，数据卷一起删 ——
// 只删容器会留下带令牌的卷，下次用同一个卷起回来还是旧身份，
// 用户在 Hub 里删掉的节点会以「又连上来了」的形式复活。
const UNINSTALL_CMD = 'docker rm -f autodocker-agent && docker volume rm autodocker-agent-data';

// openNodeDelModal 删除节点前的确认窗。
//
// 浏览器的 confirm 只放得下一句话：既说不清「删掉之后目标机上还剩什么」，
// 也放不下那条卸载命令 —— 用户要复制还得自己去文档里翻。删节点是不可逆的
// （令牌当场作废，要让机器回来得重新添加），值得把这几件事一次交代清楚。
function openNodeDelModal(id) {
  const n = state.nodes.find((x) => x.id === id);
  if (!n) return;
  const box = $('#node-del-body');
  if (!box) return;
  state.nodeDelID = id;

  const st = NODE_STATUS[n.status] || NODE_STATUS.never;
  const rows = [
    ['状态', `<span class="badge ${st.cls}">${st.text}</span>`],
    ['主机', esc([n.hostname, n.os, n.arch].filter(Boolean).join(' · ') || '尚未上报')],
    ['Agent', esc(n.agent_version || '尚未上报')],
    ['Hub 地址', `<span class="mono">${esc(n.hub_url || '—')}</span>`],
    ['容器', `${n.container_count || 0} 个${n.update_count ? ` · 待更新 ${n.update_count}` : ''}`],
    ['最后心跳', hasTime(n.last_seen_at) ? fmtTime(n.last_seen_at) : '—'],
    ['添加时间', hasTime(n.created_at) ? fmtTime(n.created_at) : '—'],
  ];
  if (n.note) rows.push(['备注', esc(n.note)]);

  box.innerHTML = `
    <h3>删除节点「${esc(n.name || '未命名节点')}」</h3>
    <p class="field-hint">
      这里删掉的是 Hub 侧的记录和巡检数据。目标机上的 Agent 不受影响、会一直尝试重连，
      要一起清掉就在那台机器上执行下面这条命令。
    </p>
    <dl class="node-del-info">
      ${rows.map(([k, v]) => `<div class="node-del-row"><dt>${k}</dt><dd>${v}</dd></div>`).join('')}
    </dl>
    <div class="field-hint" style="margin:16px 0 6px">在目标机上执行（停掉并删除 Agent 与它的数据卷）：</div>
    <pre class="node-cmd">${esc(UNINSTALL_CMD)}</pre>
    <button type="button" class="btn small" data-copy-uninstall>复制命令</button>
    <p class="field-hint" style="margin-top:14px">
      窗口关掉后这条命令就找不到了，需要先复制再删。删掉之后这个节点的令牌立刻作废，
      要让这台机器回来得重新添加节点、重新执行安装命令。
    </p>
    <div class="form-actions">
      <button type="button" class="btn danger" id="btn-node-del-ok">确认删除</button>
      <button type="button" class="btn ghost" data-close-modal="1">取消</button>
    </div>`;
  $('#node-del-modal').hidden = false;
}

function closeNodeDelModal() {
  const modal = $('#node-del-modal');
  if (modal) modal.hidden = true;
  state.nodeDelID = null;
  const box = $('#node-del-body');
  if (box) box.innerHTML = '';
}

// confirmNodeDel 真正下发删除。
//
// 失败时把按钮放开并留在弹窗里：关掉弹窗再重来一次，用户还得重新找一遍这个节点。
async function confirmNodeDel() {
  const id = state.nodeDelID;
  if (id === null || id === undefined) return;
  const btn = $('#btn-node-del-ok');
  if (btn) {
    btn.disabled = true;
    btn.textContent = '删除中…';
  }
  try {
    await api(`/api/nodes/${id}`, { method: 'DELETE' });
    state.nodeOpen.delete(id);
    state.nodeErr.delete(id);
    closeNodeDelModal();
    toast('已删除');
    await loadNodes();
  } catch (e) {
    toast('删除失败：' + e.message, 'danger');
    if (btn) {
      btn.disabled = false;
      btn.textContent = '确认删除';
    }
  }
}

async function copyUninstallCmd() {
  const pre = $('#node-del-body .node-cmd');
  if (!pre) return;
  const ok = await copyText(pre.textContent);
  toast(ok ? '卸载命令已复制' : '复制失败，请手动选中命令', ok ? 'ok' : 'warn');
}

// 通知渠道清单。
//
// 只列**已经能用**的四个。曾经在这里摆过 Slack / Discord / Gotify / ntfy / Bark / 邮件，
// 一律标「施工中」—— 占位摆了大半年也没做，反而让人以为「是不是我没配出来」。
const CHANNELS = [
  { id: 'feishu', name: '飞书' },
  { id: 'telegram', name: 'Telegram' },
  { id: 'dingtalk', name: '钉钉' },
  { id: 'wecom', name: '企业微信' },
];

// currentChannel 当前应该显示哪个渠道的设置。
//
// 未指定时按「谁配好了用谁」推断，与后端 EffectiveChannel 的判断保持一致；
// 都没配就默认显示飞书（它是第一个做的，也是文档里最先讲的）。
function currentChannel() {
  const s = state.settings || {};
  const ch = s.notify_channel;
  if (ch === 'telegram' || ch === 'feishu' || ch === 'dingtalk' || ch === 'wecom') return ch;
  if (!s.feishu_app_id && s.telegram_bot_token) return 'telegram';
  if (!s.feishu_app_id && !s.telegram_bot_token && s.dingtalk_client_id) return 'dingtalk';
  if (!s.feishu_app_id && !s.telegram_bot_token && !s.dingtalk_client_id && s.wecom_bot_id) return 'wecom';
  return 'feishu';
}

function channelConfigured(id) {
  const s = state.settings || {};
  if (id === 'feishu') return !!(s.feishu_app_id && s.feishu_app_secret);
  if (id === 'telegram') return !!s.telegram_bot_token;
  if (id === 'dingtalk') return !!(s.dingtalk_client_id && s.dingtalk_client_secret);
  if (id === 'wecom') return !!(s.wecom_bot_id && s.wecom_secret);
  return false;
}

// channelEnabledState 渠道总开关。
// 后端老库缺这个字段时会按「凭据齐不齐」推导，前端只做展示。
function channelEnabledState(id) {
  const s = state.settings || {};
  const v = s[id + '_enabled'];
  if (v === true || v === false) return v;
  return channelConfigured(id);
}

// 这个渠道离「能发出去」还差什么。
// 钉钉要四样齐才发得出卡片，缺一样都得说清楚，不然用户只能一项项试。
function channelMissing(id) {
  const s = state.settings || {};
  if (id === 'feishu') {
    return ['feishu_app_id', 'feishu_app_secret', 'feishu_chat_id'].filter((k) => !s[k]);
  }
  if (id === 'telegram') {
    return ['telegram_bot_token', 'telegram_chat_id'].filter((k) => !s[k]);
  }
  if (id === 'dingtalk') {
    return ['dingtalk_client_id', 'dingtalk_client_secret', 'dingtalk_card_template_id', 'dingtalk_conversation_id']
      .filter((k) => !s[k]);
  }
  if (id === 'wecom') {
    return ['wecom_bot_id', 'wecom_secret', 'wecom_chat_id'].filter((k) => !s[k]);
  }
  return [];
}

// stashChannelFields 把当前可见渠道的输入暂存进 state.settings。
//
// 切换渠道会整块重绘表单，浏览器不会替你记住刚输入的内容；
// 先存一份，切回来时值还在，也保证保存时不会塞进一空串把别的渠道凭据抹掉。
function stashChannelFields() {
  const s = state.settings;
  if (!s) return;
  const take = (id, key) => {
    const el = $('#' + id);
    if (el) s[key] = el.value.trim();
  };
  take('feishu_app_id', 'feishu_app_id');
  take('feishu_app_secret', 'feishu_app_secret');
  take('feishu_chat_id', 'feishu_chat_id');
  take('telegram_bot_token', 'telegram_bot_token');
  take('telegram_chat_id', 'telegram_chat_id');
  take('telegram_api_base', 'telegram_api_base');
  take('telegram_proxy', 'telegram_proxy');
  take('dingtalk_client_id', 'dingtalk_client_id');
  take('dingtalk_client_secret', 'dingtalk_client_secret');
  take('dingtalk_card_template_id', 'dingtalk_card_template_id');
  take('dingtalk_conversation_id', 'dingtalk_conversation_id');
  take('wecom_bot_id', 'wecom_bot_id');
  take('wecom_secret', 'wecom_secret');
  take('wecom_chat_id', 'wecom_chat_id');
}

function channelChipsHtml(ch) {
  return CHANNELS.map((c) => {
    const active = c.id === ch ? ' active' : '';
    // 三种状态要分开：没配、配好了在推、配好了但用户主动关掉。
    // 都写成「已填写凭据」的话，关掉之后看不出是暂停还是坏了。
    let sub = '待配置';
    if (channelConfigured(c.id)) sub = channelEnabledState(c.id) ? '已启用' : '已暂停';
    return `<button type="button" class="channel-chip${active}" data-channel="${c.id}">
      <span class="chip-name">${esc(c.name)}</span>
      <span class="chip-sub">${sub}</span></button>`;
  }).join('');
}

// channelSwitchHtml 渠道总开关。
//
// 关掉只停「推新提醒」：凭据、模板、会话全部保留，已发出去的卡片也照样回写状态。
// 文案必须把「配置保留」说清楚，否则用户会担心关一下就等于清空，只能一直开着。
function channelSwitchHtml(id, name) {
  const on = channelEnabledState(id);
  const ready = channelConfigured(id);
  let hint = '填完下面的凭据、测通后会自动打开';
  if (ready) {
    hint = on
      ? '发现新版本时推送到这个渠道'
      : '已暂停：配置保留，不再推送新提醒';
  }
  return `
    <div class="chan-switch">
      <div class="sw-text">
        <span class="sw-title">启用${esc(name)}通知</span>
        <span class="sw-sub">${esc(hint)}</span>
      </div>
      <label class="sw">
        <input type="checkbox" id="${id}_enabled" data-chan-switch="${id}" ${on ? 'checked' : ''}>
        <span class="track"></span>
      </label>
    </div>`;
}

function feishuFieldsHtml(s) {
  return `
    <div class="field">
      <label for="feishu_app_id">App ID</label>
      <input id="feishu_app_id" name="feishu_app_id" type="text" value="${esc(s.feishu_app_id)}" placeholder="cli_xxxxxxxxxxxx">
    </div>
    <div class="field">
      <label for="feishu_app_secret">App Secret</label>
      <input id="feishu_app_secret" name="feishu_app_secret" type="password" value="${esc(s.feishu_app_secret)}" placeholder="留空则关闭飞书通知">
      <p class="field-hint">仅保存在本机数据库里，不会外传。</p>
    </div>
    <div class="field">
      <label for="feishu_chat_id">接收群 chat_id</label>
      <input id="feishu_chat_id" name="feishu_chat_id" type="text" value="${esc(s.feishu_chat_id)}" placeholder="oc_xxxxxxxx">
      <p class="field-hint">三个值一并留空，即完全关闭飞书通知。</p>
    </div>`;
}

function telegramFieldsHtml(s) {
  return `
    <div class="field">
      <label for="telegram_bot_token">Bot Token</label>
      <input id="telegram_bot_token" name="telegram_bot_token" type="password"
             value="${esc(s.telegram_bot_token)}" placeholder="123456789:AAHxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx">
      <p class="field-hint">找 <code>@BotFather</code> 发 <code>/newbot</code> 拿到。仅保存在本机数据库里。</p>
    </div>
    <div class="field">
      <label for="telegram_chat_id">接收会话 chat_id</label>
      <div class="input-row">
        <input id="telegram_chat_id" name="telegram_chat_id" type="text"
               value="${esc(s.telegram_chat_id)}" placeholder="-1001234567890">
        <button type="button" id="btn-tg-chats" class="btn ghost">读取会话</button>
      </div>
      <p class="field-hint">
        先把 bot 拉进目标群，在群里 <code>@它</code> 说一句话 —— 服务收到这条消息才知道会话 ID
        （bot 默认读不到群里的普通聊天）。私聊是正数，群与频道是 <code>-100</code> 开头。
      </p>
      <div id="tg-chat-list" class="chat-pick" hidden></div>
    </div>
    <div class="field">
      <label for="telegram_api_base">API 地址（可留空）</label>
      <input id="telegram_api_base" name="telegram_api_base" type="text"
             value="${esc(s.telegram_api_base)}" placeholder="https://api.telegram.org">
      <p class="field-hint">直连 <code>api.telegram.org</code> 不通时填自建反代地址；留空用官方地址。</p>
    </div>
    <div class="field">
      <label for="telegram_proxy">代理地址（可留空）</label>
      <div class="input-row">
        <input id="telegram_proxy" name="telegram_proxy" type="text"
               value="${esc(s.telegram_proxy)}" placeholder="http://192.168.1.2:7890">
        <button type="button" id="btn-tg-check" class="btn ghost">检测连通性</button>
      </div>
      <p class="field-hint">
        国内直连 Telegram 通常不通（报 TLS 握手超时）。这台机器上已有 Clash / v2ray 之类的话，
        填它的代理地址即可，例如 <code>192.168.1.2:7890</code>；
        不写协议按 <code>http</code> 处理，<code>socks5://</code> 也认。
        填完直接点右边「检测连通性」—— 通了会<b>自动保存并生效</b>，不用再点保存。
      </p>
      <div id="tg-check-result" class="chat-pick" hidden></div>
    </div>`;
}

function dingtalkFieldsHtml(s) {
  return `
    <div class="field">
      <label for="dingtalk_client_id">应用 Client ID</label>
      <input id="dingtalk_client_id" name="dingtalk_client_id" type="text"
             value="${esc(s.dingtalk_client_id)}" placeholder="dingxxxxxxxxxxxxxxxx">
      <p class="field-hint">钉钉开放平台 → 应用开发 → 企业内部应用 → 凭证与基础信息。</p>
    </div>
    <div class="field">
      <label for="dingtalk_client_secret">应用 Client Secret</label>
      <div class="input-row">
        <input id="dingtalk_client_secret" name="dingtalk_client_secret" type="password"
               value="${esc(s.dingtalk_client_secret)}" placeholder="只显示一次，复制过来">
        <button type="button" id="btn-dt-check" class="btn ghost">检测连通性</button>
      </div>
      <p class="field-hint">
        仅保存在本机数据库里。填完直接点右边检测 —— 通了会<b>自动保存</b>，
        并立刻拉起长连接，不用再点「保存设置」。
      </p>
      <div id="dt-check-result" class="chat-pick" hidden></div>
    </div>
    <div class="field">
      <label for="dingtalk_card_template_id">卡片模板 ID</label>
      <input id="dingtalk_card_template_id" name="dingtalk_card_template_id" type="text"
             value="${esc(s.dingtalk_card_template_id)}" placeholder="xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.schema">
      <p class="field-hint">
        在<a href="https://open-dev.dingtalk.com/fe/card" target="_blank" rel="noreferrer">卡片平台</a>建好模板并发布后复制。
        模板里必须有 <code>title</code>、<code>status</code>、<code>body</code> 三个变量，
        以及回传参数为 <code>{"action":"update"}</code> 等三个按钮 —— 具体见右栏第 4 步。
      </p>
    </div>
    <div class="field">
      <label for="dingtalk_conversation_id">接收群会话 ID</label>
      <div class="input-row">
        <input id="dingtalk_conversation_id" name="dingtalk_conversation_id" type="text"
               value="${esc(s.dingtalk_conversation_id)}" placeholder="cidxxxxxxxxxxxxxxxxxx==">
        <button type="button" id="btn-dt-chats" class="btn ghost">读取会话</button>
      </div>
      <p class="field-hint">
        把机器人拉进目标群，在群里<b>@它说一句话</b>，再点「读取会话」挑一条 ——
        钉钉不提供「机器人所在群」的列表接口，只能等它在群里收到消息时自己报上来。
      </p>
      <div id="dt-chat-list" class="chat-pick" hidden></div>
    </div>`;
}

function wecomFieldsHtml(s) {
  return `
    <div class="field">
      <label for="wecom_bot_id">机器人 BotID</label>
      <input id="wecom_bot_id" name="wecom_bot_id" type="text"
             value="${esc(s.wecom_bot_id)}" placeholder="企业微信智能机器人的 BotID">
      <p class="field-hint">
        管理后台 → <b>智能专区</b> → 智能机器人 → 选中机器人 → 「API 配置」里能看到。
      </p>
    </div>
    <div class="field">
      <label for="wecom_secret">长连接 Secret</label>
      <input id="wecom_secret" name="wecom_secret" type="password"
             value="${esc(s.wecom_secret)}" placeholder="只显示一次，复制过来">
      <p class="field-hint">
        同一页「API 模式 → 长连接」下方那个 Secret。注意<b>不是</b>接收 URL 模式的
        Token / EncodingAESKey，两者不通用，填错了会一直连不上。
        仅保存在本机数据库里；保存后自动建立长连接。
      </p>
    </div>
    <div class="field">
      <label for="wecom_chat_id">接收会话</label>
      <div class="input-row">
        <input id="wecom_chat_id" name="wecom_chat_id" type="text"
               value="${esc(s.wecom_chat_id)}" placeholder="群聊填 chatid，单聊填 userid">
        <button type="button" id="btn-wc-chats" class="btn ghost">读取会话</button>
      </div>
      <p class="field-hint">
        把机器人加进目标群，在群里 <b>@它说一句话</b>（单聊就直接给它发一句），
        再点「读取会话」挑一条 —— 企业微信没有「机器人所在群」的接口，
        只能等它在会话里收到消息时自己报上来。
      </p>
      <div id="wc-chat-list" class="chat-pick" hidden></div>
    </div>`;
}

// wecomStatusText 右栏「当前状态」那一行。
//
// 长连接状态只能从后端问：企业微信没有独立的「检测」接口，
// 认证就是建立长连接本身，连上了才算凭据对、网络通。
function wecomStatusText() {
  const s = state.settings || {};
  const st = state.wecom || {};
  if (!s.wecom_bot_id || !s.wecom_secret) {
    return '未配置，发现新版本只会记录在「通知」标签里';
  }
  if (st.running) return '已配置，长连接在线';
  return '已配置，但长连接未建立' +
    (st.last_error ? `：${st.last_error}` : '（保存后稍等几秒，或看容器日志）');
}

async function refreshWeComStatus() {
  try {
    state.wecom = await api('/api/wecom/status');
  } catch (_) {
    return;
  }
  // 只更新那一行，不重绘整块：重绘会把用户展开的步骤折叠回去。
  const el = $('#wc-live-status');
  if (el) el.textContent = wecomStatusText();
}

function wecomGuideHtml() {
  return `
    <div class="guide-card">
      <h3>企业微信 · 配置流程</h3>
      <p class="field-hint">
        配置完后发现新版本会往企业微信推一张带按钮的卡片，点按钮直接重建容器。
        走<b>长连接</b>，不需要公网地址、不需要域名备案、也不用加解密。
      </p>
      <p class="field-hint">当前状态：<b id="wc-live-status">${esc(wecomStatusText())}</b></p>
      <details class="setup-guide" open>
        <summary>共 6 步，第一次配置照着做</summary>
        <ol class="setup-steps">
          <li>
            <b>建机器人</b>：<a href="https://work.weixin.qq.com/wework_admin/frame" target="_blank" rel="noreferrer">企业微信管理后台</a>
            → <b>智能专区</b> → <b>智能机器人</b> → 创建 → 新建机器人，
            创建方式选 <b>「API 模式创建」</b>，名字随意。
          </li>
          <li>
            <b>开长连接</b>：同一个创建页，右侧「API 配置」选<b>「使用长连接」</b>。
          </li>
          <li>
            <b>授权能力</b>：往下「使用模式 → 可使用权限」点<b>「去授权」</b>，
            勾上 <b>消息</b>（发卡片要用）与 <b>通讯录</b>（认得出是谁在 @ 它），
            文档、邮件、待办、日程、会议、微盘都不用勾；「使用方式」选 <b>「多人使用」</b>，
            否则只有你自己能点卡片上的按钮。
          </li>
          <li>
            <b>取凭据</b>：同一页拿到 <code>BotID</code> 与<b>长连接专用</b>
            <code>Secret</code>，填到左侧两项，点「保存设置」。
          </li>
          <li>
            <b>取会话</b>：把机器人加进目标群，在群里 @它 说一句话（单聊就直接发一句），
            然后点左侧「读取会话」挑一条填入。<br>
            <span class="field-hint">只有<b>企业内部群</b>能加机器人；互联群、上下游群、
            外部群都加不进去，硬拉会收不到任何消息。</span>
          </li>
          <li>
            <b>验证</b>：点下方「发送企业微信测试卡片」，卡片到了再点一下卡片上的任意按钮 ——
            卡片当场变样，才算「发得出去 + 收得回按钮」两条链路都通。
          </li>
        </ol>
      </details>
      <p class="field-hint">
        有一个与其它渠道不同的限制：<b>卡片只能在点按钮后 5 秒内更新</b>。
        所以点完按钮卡片会变成「正在更新」，更新结果会<b>另发一条消息</b> ——
        原来那张卡片会一直停在「正在更新」，这是协议限制，不是坏了。
      </p>
    </div>`;
}

function feishuGuideHtml() {
  const s = state.settings || {};
  const configured = !!(s.feishu_app_id && s.feishu_app_secret);
  const live = state.stats && state.stats.feishu_longconn;
  const statusText = !configured
    ? '未配置，发现新版本只会记录在「通知」标签里'
    : (live ? '已配置，按钮回调长连接正常' : '已配置，但长连接尚未就绪（保存后稍等片刻，或查看日志）');
  return `
    <div class="guide-card">
      <h3>飞书 · 配置流程</h3>
      <p class="field-hint">
        配好后发现新版本会往飞书群推一张卡片，点卡片上的按钮直接重建容器 ——
        手机上也能操作。本应用用<b>长连接</b>接收按钮回调，不需要公网地址、不需要域名备案。
      </p>
      <p class="field-hint">当前状态：<b>${esc(statusText)}</b></p>
      <details class="setup-guide" open>
        <summary>共 8 步，第一次配置照着做</summary>
        <ol class="setup-steps">
          <li>
            <b>建应用</b>：打开
            <a href="https://open.feishu.cn/app" target="_blank" rel="noreferrer">飞书开放平台 · 开发者后台</a>，
            点「创建企业自建应用」，名字随意。
          </li>
          <li>
            <b>加机器人能力</b>：左侧「应用能力 → 机器人」→ 添加应用能力，
            确认「消息卡片」开关是打开的。
          </li>
          <li>
            <b>取凭据</b>：左侧「凭证与基础信息」，把 <code>App ID</code> 和
            <code>App Secret</code> 填到左侧设置项里。
          </li>
          <li>
            <b>开权限</b>：左侧「开发配置 → 权限管理 → API 权限」，搜到下面两项并开通，
            权限类型都选<b>「应用身份」</b>：
            <ul class="scope-list">
              <li><code>im:message:send_as_bot</code></li>
              <li><code>im:chat:readonly</code></li>
            </ul>
          </li>
          <li>
            <b>配回调</b>：左侧「开发配置 → 事件与回调」→ 切到<b>「回调配置」标签页</b>：
            <ul class="scope-list">
              <li>「订阅方式」选<b>「使用长连接接收回调」</b>，点保存</li>
              <li>「已订阅的回调」→ 添加回调 → 搜<b>「卡片回传交互」</b>
                （<code>card.action.trigger</code>）→ 确认添加</li>
            </ul>
          </li>
          <li>
            <b>发布版本</b>：左侧「应用发布 → 版本管理与发布」→ 创建版本 → 提交发布。
            改完权限和回调不发布，点按钮会报 <code>200340</code>。
          </li>
          <li>
            <b>把机器人拉进群</b>：飞书里打开目标群 → 右上角设置 → 群机器人 → 添加机器人，
            选中你刚建的这个应用。
          </li>
          <li>
            <b>取群 ID</b>：打开
            <a href="https://open.feishu.cn/api-explorer" target="_blank" rel="noreferrer">飞书 API 调试台</a>，
            搜「获取机器人所在的群列表」，以<b>应用身份</b>调用，
            返回里的 <code>chat_id</code> 填到左侧设置项里。
          </li>
        </ol>
      </details>
    </div>`;
}

function telegramGuideHtml() {
  const s = state.settings || {};
  const live = state.stats && state.stats.telegram_polling;
  const statusText = !s.telegram_bot_token
    ? '未配置，发现新版本只会记录在「通知」标签里'
    : (live ? '已配置，按钮回调长轮询运行中' : '已配置，但长轮询尚未就绪（保存后稍等片刻，或看下方报错）');
  return `
    <div class="guide-card">
      <h3>Telegram · 配置流程</h3>
      <p class="field-hint">
        和飞书一样：发现新版本推一条带按钮的消息，点按钮直接重建容器。
        服务用<b>长轮询</b>接收按钮点击，同样不需要公网地址 ——
        只要这台机器能访问 <code>api.telegram.org</code>。
      </p>
      <p class="field-hint">当前状态：<b>${esc(statusText)}</b></p>
      <details class="setup-guide" open>
        <summary>共 5 步，第一次配置照着做</summary>
        <ol class="setup-steps">
          <li>
            <b>建 bot</b>：在 Telegram 里搜 <code>@BotFather</code> → 发 <code>/newbot</code> →
            依次给它一个显示名和用户名（用户名必须以 <code>bot</code> 结尾）→
            它回你一串 <code>123456789:AAH…</code>，这就是 <b>Bot Token</b>，填到左侧。
          </li>
          <li>
            <b>保存一次</b>：点页面底部「保存设置」。填了 Token 服务就会开始长轮询，
            这一步不做，后面的会话 ID 读不到。
          </li>
          <li>
            <b>让 bot 收到一条消息</b>：把 bot 拉进目标群（群里加成员时搜它的用户名），
            然后<b>在群里 @ 它说一句话</b>；只给自己用就直接私聊它发个「hi」。
            <br>Telegram 的 bot 默认只看得到「@ 它」和「/ 命令」，群里别人闲聊它收不到 ——
            成员列表里显示「无权读取消息」就是这个原因。想让它读到所有消息，
            找 <code>@BotFather</code> 发 <code>/setprivacy</code> → 选它 → <code>Disable</code>，
            然后<b>把 bot 移出群再重新拉进来</b>。
            <br>这条消息只是为了让服务知道「会话 ID 是多少」。
          </li>
          <li>
            <b>取 chat_id</b>：点左侧「读取会话」，列表里会冒出刚说话的会话，
            点一下即自动填入。群与频道是 <code>-100</code> 开头的负数，私聊是正数。
          </li>
          <li>
            <b>验证按钮</b>：保存后点「发送测试消息」，消息里带一个按钮 ——
            点一下、消息变成「回调已收到」，才算「发消息 + 收回调」两条链路都通了。
          </li>
        </ol>
      </details>
      <p class="field-hint">
        收不到消息时按顺序看：先点上面的「检测连通性」——
        国内直连 <code>api.telegram.org</code> 基本不通，报的是 TLS 握手超时，
        填好代理地址就通了；连通之后还是读不到会话，才是隐私模式或 webhook 的问题
        （群里普通聊天 bot 默认读不到，只认 <code>@它</code> 与 <code>/命令</code>）。
      </p>
    </div>`;
}

function dingtalkGuideHtml() {
  const s = state.settings || {};
  const configured = channelConfigured('dingtalk');
  const live = state.stats && state.stats.dingtalk_stream;
  let statusText = '未配置，发现新版本只会记录在「通知」标签里';
  if (configured) {
    statusText = live
      ? '已配置，Stream 长连接运行中'
      : '已配置，但长连接尚未就绪（保存后稍等片刻，或看下方说明）';
  }
  return `
    <div class="guide-card">
      <h3>钉钉 · 配置流程</h3>
      <p class="field-hint">
        和飞书一样：发现新版本推一张卡片，点卡片上的按钮直接重建容器。
        本应用用 <b>Stream 长连接</b>接收按钮回调，不需要公网地址、不需要域名备案，
        也不需要公网 IP。
      </p>
      <p class="field-hint">当前状态：<b>${esc(statusText)}</b></p>
      <details class="setup-guide" open>
        <summary>共 6 步，第一次配置照着做</summary>
        <ol class="setup-steps">
          <li>
            <b>建应用</b>：打开
            <a href="https://open-dev.dingtalk.com/" target="_blank" rel="noreferrer">钉钉开放平台</a> →
            「应用开发 → <b>钉钉应用</b>」→ 创建应用。
            建好后在「凭证与基础信息」里复制 <code>Client ID</code> 与
            <code>Client Secret</code> 填到左边。
            <br>
            <span class="field-hint">
              注意：左侧还有一个与「钉钉应用」<b>并列</b>的「机器人」入口，那个是旧模型，
              钉钉官方已说明「将逐渐退出历史舞台，不要用它来创建」。本流程需要的卡片模板关联、
              权限、版本发布都在「钉钉应用」详情页里，从应用入口建才能一条路走完。
            </span>
          </li>
          <li>
            <b>加机器人</b>：在刚建好的应用里，左侧「应用能力」→ 机器人 → 添加/配置。
            <b>除「消息接收模式」外，其余五项全是必填</b>，少一项发布不了：
            <ul class="scope-list">
              <li>机器人名称：<code>AutoDocker</code>（群里显示的就是它）</li>
              <li>机器人图标：建议 240×240 方形，可用项目里的 <code>fnos/ICON_256.PNG</code></li>
              <li>机器人简介：<b>最多 10 个字符</b>，例如 <code>镜像更新提醒</code></li>
              <li>机器人描述：<b>最多 200 字符</b>，写清「发现新版本推卡片、点按钮重建容器」</li>
              <li>机器人消息预览图：png/jpg、<b>不超过 2M</b>，可用一张界面截图</li>
              <li>消息接收模式：<b>Stream</b>（不是 HTTP，选了 HTTP 就得填公网地址）</li>
            </ul>
            <span class="field-hint">
              简介和描述会出现在群成员「添加机器人」时的列表里 —— 写成「机器人1号」那种，
              别人不知道是干什么的、也就不会去加。改完机器人配置要去
              「版本管理与发布」发新版本，否则群里看到的还是旧信息。
            </span>
          </li>
          <li>
            <b>开权限</b>：左侧「权限管理」，搜到下面两项并开通：
            <ul class="scope-list">
              <li><code>Card.Instance.Write</code>（互动卡片实例写权限）</li>
              <li><code>qyapi_robot_sendmsg</code>（企业内机器人发送消息）</li>
            </ul>
          </li>
          <li>
            <b>建卡片模板</b>：打开
            <a href="https://open-dev.dingtalk.com/fe/card" target="_blank" rel="noreferrer">卡片平台</a>
            → 新建模板 → 按下面「模板怎么搭」拖好组件 → <b>发布</b> →
            复制模板 ID（形如 <code>xxxx-xxxx.schema</code>）填到左边。
          </li>
          <li>
            <b>发布应用版本</b>：左侧「版本管理与发布」→ 创建版本 → 发布。
            <b>改了权限、机器人或模板却不发布新版本，等于什么都没改</b> ——
            现象是配置看着全对，发卡片一直报权限或模板错误。
          </li>
          <li>
            <b>拉群并取会话 ID</b>：钉钉群里 → 群设置 → 智能群助手 → 添加机器人 →
            选你刚建的那个；然后在群里 <b>@它说一句话</b>，回来点左边「读取会话」，
            列表里会冒出这个群，点一下即自动填入并保存。
          </li>
        </ol>
      </details>

      <details class="setup-guide">
        <summary>第 4 步的模板怎么搭（变量名要一字不差）</summary>
        <p class="field-hint">
          钉钉不允许代码定义卡片长什么样，只能先在平台搭好骨架、代码往里填数据。
          所以下面这些名字是硬约定，写错了卡片发出去就是一片空白。
        </p>
        <ul class="scope-list">
          <li>标题：<b>文本</b>组件，变量名 <code>title</code></li>
          <li>状态：<b>文本</b>组件，变量名 <code>status</code>（放右上角当小标签即可）</li>
          <li>正文：<b>Markdown</b>组件，变量名 <code>body</code></li>
        </ul>
        <p class="field-hint">
          再放三个按钮，每个都配「回传请求」，参数分别填：
        </p>
        <ul class="scope-list">
          <li>立即更新 → <code>{"action":"update"}</code></li>
          <li>稍后提醒 → <code>{"action":"snooze"}</code></li>
          <li>忽略本版本 → <code>{"action":"ignore"}</code></li>
        </ul>
        <p class="field-hint">
          代码只认 <code>params.action</code>，不认按钮 ID，所以排版随你怎么调都不会坏。
          测试卡片用的是同一个模板，点上面任意一个按钮都算「回调验证通过」。
        </p>
      </details>

      <p class="field-hint">
        点按钮没反应时按顺序看：先点上面的「检测连通性」——
        它只验凭据和出网，一次把这两件事说清；连通之后还收不到卡片，
        才去看模板有没有发布、按钮的回传参数有没有配对。
        卡片收到了但点了没变化，多半是第 5 步的版本没发布。
      </p>
    </div>`;
}

function renderGuide(ch) {
  const guide = $('#settings-guide');
  if (!guide) return;
  if (ch === 'telegram') {
    guide.innerHTML = telegramGuideHtml();
    return;
  }
  if (ch === 'dingtalk') {
    guide.innerHTML = dingtalkGuideHtml();
    return;
  }
  if (ch === 'wecom') {
    guide.innerHTML = wecomGuideHtml();
    // 状态要去后端问一次：长连接是否在线只有服务端知道。
    refreshWeComStatus();
    return;
  }
  guide.innerHTML = feishuGuideHtml();
}

// pushSnippet 给调用方抄的 curl 示例。
//
// 协议与端口跟着当前页面走：用户从哪打开的，示例里就写哪个地址 ——
// 写死 19999 的话，改了端口或者走反代的人抄过去全是连不上。
function pushSnippet(token) {
  const hint = token || '<先在设置页生成令牌>';
  return [
    `curl -X POST ${location.origin}/api/push \\`,
    "  -H 'Content-Type: application/json' \\",
    `  -H 'X-Push-Token: ${hint}' \\`,
    `  -d '{"title":"备份完成","body":"共 3.2 GB"}'`,
  ].join('\n');
}

function renderSettings() {
  const s = state.settings;
  if (!s) return;
  const selfSt = state.self || {};
  const ch = currentChannel();
  const chName = { feishu: '飞书', telegram: 'Telegram', dingtalk: '钉钉', wecom: '企业微信' }[ch] || '飞书';
  const testLabel = {
    feishu: '发送飞书测试卡片',
    telegram: '发送测试消息',
    dingtalk: '发送钉钉测试卡片',
    wecom: '发送企业微信测试卡片',
  }[ch] || '发送测试卡片';
  const fields = {
    feishu: feishuFieldsHtml,
    telegram: telegramFieldsHtml,
    dingtalk: dingtalkFieldsHtml,
    wecom: wecomFieldsHtml,
  }[ch](s);

  $('#settings-form').innerHTML = `
    <div class="field">
      <label for="scan_interval">扫描间隔（秒，0 表示关闭定时扫描）</label>
      <input id="scan_interval" name="scan_interval" type="number" min="0" value="${s.scan_interval}">
      <p class="field-hint">最小 30 秒。修改后无需重启即可生效。</p>
    </div>
    <div class="field">
      <label for="health_check_wait">更新后健康检查观察窗口（秒）</label>
      <input id="health_check_wait" name="health_check_wait" type="number" min="0" max="600" value="${s.health_check_wait}">
      <p class="field-hint">新容器启动后至少等这么久。容器自带 HEALTHCHECK 时，会按它的探测间隔与重试次数自动延长（不短于 120 秒）；到期仍在启动的按「已启动、未确认」处理，不回滚。中途出现连续探测失败也继续观察，到期仍未恢复才回滚。</p>
    </div>

    <h3 class="form-section">通知渠道</h3>
    <p class="field-hint">发现新版本时把提醒推到哪里。<b>开着开关、凭据齐全的渠道都会收到</b>；关掉某个渠道只是不再推新提醒，配置一项都不会丢。右栏是当前所选渠道的完整配置流程 —— 选谁只决定改哪份配置。</p>
    <div class="channel-picker">${channelChipsHtml(ch)}</div>
    ${channelSwitchHtml(ch, chName)}
    ${fields}

    <div class="notify-test">
      <button type="button" id="btn-test-notify" class="btn ghost">${testLabel}</button>
      <p class="field-hint">
        填完上面的凭据<b>先保存一次</b>，再点这里发一条测试消息 ——
        消息里带按钮，点一下、消息变成「回调已收到」，才算「发得出去 + 收得回来」两条链路都通了。
      </p>
      <div id="notify-callback-state" class="callback-state" hidden></div>
    </div>

    <h3 class="form-section">注册表加速</h3>
    <p class="field-hint">
      国内直连 Docker Hub 大多不通，这会表现为镜像列表里一片「未知」。
      填上加速地址即可解决；本服务查 digest 是自己发请求，不会自动继承
      Docker 守护进程的 <code>registry-mirrors</code>，所以这里需要单独配一次。
    </p>
    <div class="field">
      <label for="registry_mirror">镜像加速地址（可填多个）</label>
      <input id="registry_mirror" name="registry_mirror" type="text" value="${esc(s.registry_mirror)}"
             placeholder="例如 docker.1ms.run, docker.1panel.live">
      <p class="field-hint">
        多个地址用逗号分隔，按顺序回退，全部失败才走直连。仅作用于 <b>Docker Hub</b>；
        ghcr.io / quay.io / 私有仓库仍直连，不会被带偏。留空则先沿用 Docker 守护进程已配的加速器，
        仍为空时使用内置默认（<code>docker.1ms.run</code> / <code>docker.1panel.live</code>）。
      </p>
    </div>
    <div class="field">
      <button type="button" id="btn-test-registry" class="btn ghost">检测注册表连通性</button>
      <div id="registry-test-result" class="registry-test" hidden></div>
    </div>

    <h3 class="form-section">访问控制</h3>
    <p class="field-hint">
      默认<b>不启用登录</b>，打开页面即可使用。本应用能重建任意容器（等同于系统最高权限），
      如果端口会暴露到内网之外，建议在这里设一个访问密码。
    </p>
    <div class="field">
      <label for="admin_user">登录用户名</label>
      <input id="admin_user" name="admin_user" type="text" value="${esc(s.admin_user)}" placeholder="admin">
    </div>
    <div class="field">
      <label for="admin_password">访问密码</label>
      <input id="admin_password" name="admin_password" type="password" value="${esc(s.admin_password)}" placeholder="留空表示不需要登录">
      <p class="field-hint">保存后立即生效，无需重启；留空即关闭登录。改动后本页会自动改用新凭据，不用手动重登。</p>
    </div>

    <h3 class="form-section">消息推送接口</h3>
    <p class="field-hint">
      本机其它应用（备份脚本、监控告警之类）可以调 <code>POST /api/push</code>，
      把消息推到上面已经配好的渠道。调用方只需要下面这个令牌，不用保管登录密码。
    </p>
    <div class="field">
      <label for="push_token">推送令牌</label>
      <div class="token-row">
        <input id="push_token" type="text" readonly value="${esc(s.push_token || '')}" placeholder="尚未生成，接口不可用">
        <button type="button" class="btn ghost" id="btn-push-token">${s.push_token ? '重新生成' : '生成令牌'}</button>
        <button type="button" class="btn ghost" id="btn-push-revoke" ${s.push_token ? '' : 'hidden'}>停用</button>
      </div>
      <p class="field-hint" id="push-hint">
        ${s.push_token
          ? '重新生成后旧令牌立即失效，正在用它的应用要同步换成新的。'
          : '生成之后接口才可用；不生成，这个入口就不存在。'}
      </p>
    </div>
    <div class="field">
      <label>调用示例</label>
      <div class="cmd">
        <pre id="push-example">${esc(pushSnippet(s.push_token))}</pre>
        <button type="button" class="btn ghost small" id="btn-copy-push">复制</button>
      </div>
      <p class="field-hint">
        <code>channel</code> 填 <code>all</code>（默认）就发到所有已就绪渠道，
        也可以只写 <code>feishu</code> / <code>telegram</code> / <code>dingtalk</code> / <code>wecom</code>。
        指名要某个渠道而它没配好时直接报错，不会悄悄改发到别处。
      </p>
    </div>

    <h3 class="form-section">更新策略</h3>
    <div class="checks">
      <label class="check"><input type="checkbox" id="rollback_on_failure" ${s.rollback_on_failure ? 'checked' : ''}> 更新失败时自动回滚到原容器</label>
      <label class="check"><input type="checkbox" id="keep_old_container" ${s.keep_old_container ? 'checked' : ''}> 更新成功后保留旧容器（便于手动回退）</label>
      <label class="check"><input type="checkbox" id="registry_insecure" ${s.registry_insecure ? 'checked' : ''}> 允许 http 明文注册表（本地地址自动走 http，无需勾选）</label>
    </div>
    <p class="field-hint">更新动作始终由人确认：提醒消息里点「立即更新」，或在此页「容器」标签点「更新」。
      环境变量 <code>AUTO_UPDATE=true</code> 可改为发现即更新，该开关不在本页展示，保存设置不会改动它。</p>

    <h3 class="form-section">远程节点</h3>
    <label class="check">
      <input type="checkbox" id="agent_auto_upgrade" ${s.agent_auto_upgrade ? 'checked' : ''}>
      节点 Agent 自动跟随 Hub 升级
    </label>
    <p class="field-hint">
      开着的话，Hub 升级后节点连上来时若版本落后，会自动把自己换成同一版：
      停一次容器、按原配置重建，约 10 秒回到线上。关掉就只能在这一页手动点「升级 Agent」。
      两版之前装的 Agent 不认识升级指令，需要在目标机上重跑一次安装命令；升上来之后就不用了。
    </p>

    <label class="check">
      <input type="checkbox" id="node_mirror_follow" ${s.node_mirror_follow ? 'checked' : ''}>
      把上面的镜像加速地址下发给所有节点
    </label>
    <p class="field-hint">
      节点的检测由 Agent 自己发请求，不经过 Docker 守护进程，所以它用不上那台机器
      <code>daemon.json</code> 里的加速器 —— 表现就是宿主机拉得动镜像、节点页却整列「检测异常」。
      开着（默认）之后，保存就会推给在线节点，下一次巡检改用新的源；这一格留空则让节点
      回到它自己的配置。节点要升到 1.12.0 才认这条指令，没升级的会静默跳过。
    </p>

    <label class="check">
      <input type="checkbox" id="node_offline_alert" ${s.node_offline_alert ? 'checked' : ''}>
      节点失联时推送一条消息
    </label>
    <div class="field">
      <label for="node_offline_after">失联多久后推送（分钟）</label>
      <input id="node_offline_after" name="node_offline_after" type="number" min="2" max="1440"
             value="${esc(String(s.node_offline_after || 10))}">
      <p class="field-hint">
        判定分两层：连接断开 90 秒（三次心跳没回应）就在页面上标离线；超过这里填的时间
        才推消息。默认 10 分钟，短于 2 分钟按 2 分钟算 —— 节点自动升级要重建一次容器，
        那几十秒的断线是计划内的。同一次失联只推一条，节点回来时会补一条「已恢复」。
        全部节点同时失联只推一条汇总，因为那多半是本机网络的问题。
      </p>
    </div>

    <h3 class="form-section">关于</h3>
    <div class="about">
      <div class="about-main">
        <div class="about-title">AutoDocker <b>${esc(selfSt.current || 'dev')}</b></div>
        <p class="field-hint" id="about-hint">${esc(aboutHint())}</p>
      </div>
      <button type="button" class="btn ghost" id="btn-selfcheck">检查更新</button>
    </div>
    <label class="check">
      <input type="checkbox" id="check_updates" ${s.check_updates ? 'checked' : ''}>
      每天检查一次新版本（只提示，不会自动升级）
    </label>
    <p class="field-hint">
      更新提示只在页面里显示，不会推到通知渠道。
      项目地址 <a class="link" href="${esc(selfSt.repo_url || GITHUB_URL)}" target="_blank" rel="noreferrer">${esc(selfSt.repo_url || GITHUB_URL)}</a>
    </p>

    <div class="selfnet">
      <p class="field-hint">
        查版本时会先直连 GitHub，连不上会自动换公共加速（gh-proxy.com 等），
        <b>两格都留空就能用</b>。只有下面两种情况才需要填。
      </p>
      <div class="field">
        <label for="selfcheck_proxy">代理地址</label>
        <input id="selfcheck_proxy" name="selfcheck_proxy" type="text" value="${esc(s.selfcheck_proxy || '')}"
               placeholder="http://192.168.1.2:7890">
        <p class="field-hint">
          NAS 上跑着 Clash 之类就填它的端口 —— <code>http://</code> 与 <code>socks5h://</code> 都认。
          填了就不再走公共加速，因为代理比它快也稳。
        </p>
      </div>
      <div class="field">
        <label for="selfcheck_mirror">加速前缀</label>
        <input id="selfcheck_mirror" name="selfcheck_mirror" type="text" value="${esc(s.selfcheck_mirror || '')}"
               placeholder="https://gh-proxy.com/">
        <p class="field-hint">
          只想用某个固定的加速服务时才填，写法是 <code>前缀 + https://github.com/…</code>。
          填了就一直用它，不再自动换。
        </p>
      </div>
    </div>

    <div class="form-actions">
      <button type="submit" class="btn primary">保存设置</button>
    </div>
  `;

  renderGuide(ch);
}

/* ---------- 自检更新 ---------- */

// 后端只负责「告诉你有没有新版本」，升级动作一律由用户自己做：
// 飞牛包归应用中心，Docker 归 compose。这两个地方都不归本程序管，
// 替它们动手一旦失败，用户手上就没有能救回来的办法。
const GITHUB_URL = 'https://github.com/haihaipypy/autodocker';

// 上一次看的是哪一栏（飞牛 / Docker），重开弹窗时沿用
let updateForm = '';
// 页面上每个「复制」按钮对应一条命令，按下标取，避免把多行命令塞进 HTML 属性
let updateCmds = [];

function aboutHint() {
  const st = state.self || {};
  // 后端已经把「为什么失败、下一步怎么办」写全了，这里别再套一层
  if (st.error) return st.error;
  if (st.dev_build) return '开发构建，不参与版本比较。';
  // 直连不通时后端会自动走公共加速，把它标出来 —— 出问题时这是第一条线索
  const via = st.via ? ' · 经 ' + mirrorHost(st.via) + ' 加速' : '';
  if (st.has_update) return '发现新版本 ' + st.latest + '，顶栏的「有新版本」里有更新内容和升级步骤。';
  if (st.checked_at) return '已是最新版本 · ' + fmtTime(st.checked_at) + ' 检查' + via;
  return '还没检查过。';
}

// mirrorHost 把加速前缀缩成域名，提示里只放这个就够看
function mirrorHost(u) {
  return String(u || '').replace(/^https?:\/\//, '').replace(/\/+$/, '');
}

async function loadSelfCheck() {
  try {
    state.self = await api('/api/selfcheck');
  } catch (_) {
    // 自检只是锦上添花，接口挂了不该影响主界面
    return;
  }
  renderSelfCheckPill();
  refreshAbout();
}

// refreshAbout 只改「关于」那两行，不整块重绘设置页 ——
// 设置页是表单，重绘会把用户还没保存的改动抹掉。
// 首次打开时设置页比自检结果先渲染出来，版本号会停在 dev，得在这里补一次。
function refreshAbout() {
  const ver = document.querySelector('.about-title b');
  if (ver) ver.textContent = (state.self && state.self.current) || 'dev';
  const hint = $('#about-hint');
  if (hint) hint.textContent = aboutHint();
}

function renderSelfCheckPill() {
  const el = $('#selfcheck-pill');
  if (!el) return;
  const st = state.self || {};
  // 没新版本就彻底不占位 —— 顶栏已经够挤了
  el.hidden = !st.has_update;
  if (!st.has_update) return;
  el.textContent = '新版本 ' + (st.latest || '');
  el.title = '当前 ' + (st.current || 'dev') + '，点开看更新内容与升级步骤';
}

function openUpdateModal() {
  renderUpdateModal();
  $('#update-modal').hidden = false;
}

function closeUpdateModal() {
  const m = $('#update-modal');
  if (m) m.hidden = true;
}

// noteLineHtml 把 Release 正文的一行渲染成弹窗里的一行。
// 只认 `#` 小标题、`-` 列表、`**加粗**`、反引号 —— 够用，犯不上为这点东西引个 markdown 库。
function noteLineHtml(line) {
  let s = String(line || '').trim();
  let cls = 'note-p';
  if (/^#{1,6}\s/.test(s)) {
    cls = 'note-h';
    s = s.replace(/^#{1,6}\s*/, '');
  } else if (/^[-*]\s/.test(s)) {
    cls = 'note-li';
    s = s.replace(/^[-*]\s*/, '');
  } else if (/^>\s?/.test(s)) {
    cls = 'note-quote';
    s = s.replace(/^>\s?/, '');
  }
  let html = esc(s);
  html = html.replace(/\*\*(.+?)\*\*/g, '<b>$1</b>');
  html = html.replace(/`([^`]+)`/g, '<code>$1</code>');
  return `<div class="${cls}">${html}</div>`;
}

function currentGuide() {
  const st = state.self || {};
  const list = st.guides || [];
  if (!list.length) return null;
  const want = updateForm || st.deploy_form || '';
  return list.find((g) => g.key === want) || list[0];
}

function renderUpdateModal() {
  const st = state.self || {};
  const body = $('#update-body');
  if (!body) return;

  const title = $('#update-title');
  if (title) title.textContent = st.has_update ? '发现新版本' : '版本信息';

  const list = st.guides || [];
  const active = currentGuide();
  updateCmds = [];

  const versions = st.latest
    ? `<div class="update-versions">
         <span class="ver">当前 ${esc(st.current || 'dev')}</span>
         <span class="ver-arrow">→</span>
         <span class="ver new">最新 ${esc(st.latest)}</span>
         <span class="hint">${st.checked_at ? esc(fmtTime(st.checked_at)) + ' 检查' : ''}</span>
       </div>`
    : `<p class="field-hint">${esc(st.error || '还没查到最新版本')}</p>`;

  const notes = (st.notes || []).length
    ? `<div class="notes">${st.notes.map(noteLineHtml).join('')}</div>`
    : `<p class="field-hint">这次没拿到 Release 说明，完整内容见 <a class="link" href="${esc(st.release_url || GITHUB_URL + '/releases')}" target="_blank" rel="noreferrer">发布页</a>。</p>`;

  const tabs = list.length > 1
    ? `<div class="update-tabs">${list.map((g) => `
        <button type="button" class="update-tab${active && g.key === active.key ? ' active' : ''}" data-form="${esc(g.key)}">${esc(g.title)}</button>`).join('')}
      </div>`
    : '';

  const steps = (active && active.steps) || [];
  const stepsHtml = steps.map((step, i) => {
    let cmd = '';
    if (step.cmd) {
      const idx = updateCmds.push(step.cmd) - 1;
      cmd = `<div class="cmd">
               <pre>${esc(step.cmd)}</pre>
               <button type="button" class="btn ghost small" data-copy="${idx}">复制</button>
             </div>`;
    }
    return `<li><span class="step-no">${i + 1}</span><div class="step-body"><span>${esc(step.text)}</span>${cmd}</div></li>`;
  }).join('');

  body.innerHTML = `
    ${versions}
    <h4 class="update-sub">更新内容</h4>
    ${notes}
    <h4 class="update-sub">怎么升级</h4>
    <p class="field-hint">${esc(active ? active.hint : '')}</p>
    ${tabs}
    <ol class="steps">${stepsHtml}</ol>
    <div class="update-links">
      <a class="link" href="${esc(st.repo_url || GITHUB_URL)}" target="_blank" rel="noreferrer">项目地址</a>
      <a class="link" href="${esc(st.release_url || GITHUB_URL + '/releases')}" target="_blank" rel="noreferrer">Release 页</a>
    </div>
  `;
}

// copyText 复制命令。http 页面没有安全上下文，navigator.clipboard 用不了，
// 退回 textarea + execCommand —— 局域网里访问 NAS 基本都是 http，这条退路是主路。
async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) { /* 落到下面的老办法 */ }
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}

async function runSelfCheck() {
  const btn = $('#btn-selfcheck');
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '检查中…'; }
  try {
    state.self = await api('/api/selfcheck', { method: 'POST' });
    renderSelfCheckPill();
    refreshAbout();
    // 手动点检查就一定要给个结果：新版本弹「发现新版本」，已经最新弹「版本信息」。
    // 只弹 toast 的话，用户看不到更新内容与升级步骤，等于只回答了一半问题。
    openUpdateModal();
    if (state.self.has_update) {
      toast('发现新版本 ' + state.self.latest);
    } else if (state.self.error) {
      toast(state.self.error, 'warn');
    } else {
      toast('已是最新版本');
    }
  } catch (e) {
    toast('检查失败：' + e.message, 'danger');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = label || '检查更新'; }
  }
}

/* ---------- 数据加载 ---------- */

async function loadStats() {
  state.stats = await api('/api/stats');
  renderStats();
}

async function loadContainers() {
  const data = await api('/api/containers');
  state.containers = data.items || [];
  renderContainers();
}

async function loadImages() {
  const data = await api('/api/images');
  state.images = data.items || [];
  // 丢掉已经不存在的勾选，免得删完之后按钮还指着空气
  const alive = new Set(state.images.map((it) => it.id));
  state.picked.forEach((id) => { if (!alive.has(id)) state.picked.delete(id); });
  renderImages();
}

async function loadTasks() {
  const data = await api('/api/tasks');
  state.tasks = data.items || [];
  renderTasks();
}

async function loadNotifications() {
  const data = await api('/api/notifications');
  state.notifications = data.items || [];
  renderNotifications();
}

async function loadSettings() {
  state.settings = await api('/api/settings');
  renderSettings();
}

async function refresh(opts) {
  const quiet = !!(opts && opts.quiet);
  try {
    await Promise.all([loadStats(), loadContainers(), loadImages(), loadTasks(), loadNotifications(), loadSelfCheck(), loadNodes()]);
  } catch (e) {
    // 轮询触发的刷新失败不提示：局域网抖一下是常事，下一轮自己会好，
    // 每隔几秒弹一次红条反而比数据旧一会儿更烦人。
    if (!quiet) toast('加载失败：' + e.message, 'danger');
  }
}

/* ---------- 交互 ---------- */

async function doScan(force) {
  const btn = force ? $('#btn-force') : $('#btn-scan');
  btn.disabled = true;
  btn.textContent = '扫描中…';
  try {
    const res = await api('/api/scan' + (force ? '?force=1' : ''), { method: 'POST' });
    if (res.result === 'scan already running') {
      toast('已有扫描在执行', 'warn');
    } else {
      toast('扫描完成：共 ' + (res.scan ? res.scan.total : 0) + ' 项，发现 ' + (res.scan ? res.scan.with_update : 0) + ' 项更新');
    }
  } catch (e) {
    toast('扫描失败：' + e.message, 'danger');
  } finally {
    btn.disabled = false;
    btn.textContent = force ? '强制扫描' : '立即扫描';
    refresh();
  }
}

async function triggerUpdate(imageId) {
  try {
    await api('/api/update', { method: 'POST', body: JSON.stringify({ image_id: imageId }) });
    toast('已开始更新，进度可在「更新历史」查看');
    refresh();
  } catch (e) {
    toast('无法更新：' + e.message, 'danger');
  }
}

async function toggleIgnore(img) {
  // 三种状态必须分开算，不能一律发 !img.ignored：
  // · 正常 → 点「忽略」：ignored=true，整条不再检测；
  // · 已忽略 → 点「恢复」：ignored=false；
  // · 已忽略本版本 → 点「恢复提醒」：此时 ignored 本来就是 false，要清的是
  //   ignored_digest，再发 !ignored(=true) 会变成「整条忽略」，跟按钮文字正好相反。
  //   后端 SetImageIgnored(false) 会把 ignored_digest 一并清掉，所以这里发 false 即可。
  const acked = ackIgnored(img);
  const next = acked ? false : !img.ignored;
  try {
    await api(`/api/images/${img.id}/ignored`, {
      method: 'PUT',
      body: JSON.stringify({ ignored: next }),
    });
    toast(next ? '已忽略该镜像' : (acked ? '已恢复提醒，下次扫描若仍是这一版会重新通知' : '已恢复监控'));
    loadContainers();
  } catch (e) {
    toast('操作失败：' + e.message, 'danger');
  }
}

// 切换检测模式。override=false 表示恢复「自动判定」。
async function setMode(id, mode) {
  try {
    await api(`/api/images/${id}/mode`, {
      method: 'PUT',
      body: JSON.stringify({ mode, override: mode !== 'auto' }),
    });
    toast(mode === 'auto' ? '已恢复自动判定' : '检测模式已固定为「' + MODE[mode] + '」');
  } catch (e) {
    toast('设置失败：' + e.message, 'danger');
  }
  loadContainers();
}

// 移除一条容器监控记录（不动容器本身）
async function removeMonitorItem(id) {
  if (!confirm('确定移除这条监控记录？不会影响正在运行的容器。')) return;
  try {
    await api('/api/images/' + id, { method: 'DELETE' });
    toast('已移除');
    loadContainers();
    loadStats();
  } catch (e) {
    toast('移除失败：' + e.message, 'danger');
  }
}

/* ---------- 镜像操作 ---------- */

// 删本机镜像：不可逆，需要时只能重新拉
async function doRemoveImages(ids, okMsg) {
  try {
    const res = await api('/api/images/remove', {
      method: 'POST',
      body: JSON.stringify({ ids, force: false }),
    });
    const failed = (res.results || []).filter((r) => !r.ok);
    if (failed.length && !res.removed) {
      toast('删除失败：' + failed[0].error, 'danger');
    } else if (failed.length) {
      toast(`已删除 ${res.removed} 个，${failed.length} 个跳过（${failed[0].error}）`, 'warn');
    } else {
      toast(okMsg || `已删除 ${res.removed} 个镜像`);
    }
  } catch (e) {
    toast('删除失败：' + e.message, 'danger');
  }
  refresh();
}

async function removeLocalImage(img) {
  const label = img.dangling ? '（无 TAG 镜像 ' + shortImageId(img.id) + '）' : img.reference;
  if (!confirm(`确认删除镜像 ${label}？\n\n删除后本机就没有这份了，需要时得重新拉取。`)) return;
  await doRemoveImages([img.id], `已删除 ${label}`);
}

// 清理候选。按镜像 ID 去重：同一个镜像可能挂着多个 tag，
// 不去重会对着同一个镜像删两次，第二次必然报错。
function cleanCandidates(mode) {
  const seen = new Set();
  return state.images.filter((img) => {
    if (img.remote_only || img.in_used) return false;
    if (mode === 'dangling' && !img.dangling) return false;
    if (seen.has(img.id)) return false;
    seen.add(img.id);
    return true;
  });
}

function renderCleanPreview() {
  const box = $('#clean-preview');
  if (!box) return;
  const picked = document.querySelector('input[name="clean-mode"]:checked');
  const mode = picked ? picked.value : 'dangling';
  const items = cleanCandidates(mode);
  if (!items.length) {
    box.innerHTML = `<div class="preview-empty">${mode === 'dangling' ? '没有无 TAG 镜像' : '没有未使用的镜像'}，无需清理。</div>`;
    return;
  }
  const total = items.reduce((a, b) => a + (b.size_bytes || 0), 0);
  box.innerHTML = items.map((img) => `
    <div class="preview-row">
      <span>${esc(img.dangling ? '（无 TAG） ' + shortImageId(img.id) : img.reference)}</span>
      <span class="mono">${esc(fmtSize(img.size_bytes))}</span>
    </div>`).join('') +
    `<div class="preview-row"><span>共 ${items.length} 个</span><span class="mono">${esc(fmtSize(total))}</span></div>`;
}

function openCleanModal() {
  const modal = $('#clean-modal');
  if (!modal) return;
  modal.hidden = false;
  renderCleanPreview();
}

function closeCleanModal() {
  const modal = $('#clean-modal');
  if (modal) modal.hidden = true;
}

async function doClean() {
  const picked = document.querySelector('input[name="clean-mode"]:checked');
  const mode = picked ? picked.value : 'dangling';
  const items = cleanCandidates(mode);
  if (!items.length) {
    toast('没有需要清理的镜像', 'warn');
    return;
  }
  const total = items.reduce((a, b) => a + (b.size_bytes || 0), 0);
  if (!confirm(`确认删除 ${items.length} 个镜像（约 ${fmtSize(total)}）？\n\n删除后需要时得重新拉取。`)) return;

  const btn = $('#btn-clean-ok');
  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = '清理中…';
  try {
    // 一次交太多会把 Docker 和这台机器一起拖住，分批发
    let removed = 0, failed = 0;
    for (let i = 0; i < items.length; i += 10) {
      const chunk = items.slice(i, i + 10).map((it) => it.id);
      const res = await api('/api/images/remove', {
        method: 'POST',
        body: JSON.stringify({ ids: chunk, force: false }),
      });
      removed += res.removed || 0;
      failed += (res.results || []).filter((r) => !r.ok).length;
    }
    closeCleanModal();
    toast(failed
      ? `已清理 ${removed} 个，${failed} 个跳过（仍被容器占用）`
      : `已清理 ${removed} 个镜像，释放约 ${fmtSize(total)}`);
    refresh();
  } catch (e) {
    toast('清理失败：' + e.message, 'danger');
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

async function removePicked() {
  if (!state.picked.size) {
    toast('先勾选要删除的镜像', 'warn');
    return;
  }
  const items = state.images.filter((it) => state.picked.has(it.id));
  const total = items.reduce((a, b) => a + (b.size_bytes || 0), 0);
  if (!confirm(`确认删除勾选的 ${items.length} 个镜像（约 ${fmtSize(total)}）？`)) return;

  const ids = items.map((it) => it.id);
  state.picked.clear();
  await doRemoveImages(ids, `已删除 ${ids.length} 个镜像`);
}

async function openTaskLog(id) {
  try {
    const t = await api('/api/tasks/' + id);
    $('#drawer-title').textContent = `任务 #${t.id} · ${t.container_name || ''} · ${TASK[t.status] ? TASK[t.status].text : t.status}`;
    const head = [
      `镜像      ${t.image_ref}`,
      `来源      ${t.triggered_by || '—'}`,
      `开始      ${fmtTime(t.started_at)}`,
      `结束      ${fmtTime(t.finished_at)}`,
      `耗时      ${fmtDuration(t.started_at, t.finished_at) || '—'}`,
      `新容器    ${t.new_container_id || '—'}`,
      `备份名    ${t.backup_name || '—'}`,
      t.error ? `错误      ${t.error}` : '',
      '──────────── 执行日志 ────────────',
      '',
    ].filter(Boolean).join('\n');
    $('#drawer-body').textContent = head + (t.log || '（无日志）');
    $('#drawer').hidden = false;
  } catch (e) {
    toast('读取日志失败：' + e.message, 'danger');
  }
}

// offlineAfterMinutes 读「失联多久后推送」，把非法值收敛到 2~1440 分钟。
//
// 输入框允许留空、也允许手打 0；直接原样送上去会被后端拒掉，
// 用户只看到一句「保存失败」，却不知道是哪一格的问题。
function offlineAfterMinutes() {
  const el = $('#node_offline_after');
  const n = parseInt(el ? el.value : '10', 10);
  if (!Number.isFinite(n)) return 10;
  return Math.min(1440, Math.max(2, n));
}

// settingsPayload 把当前表单收集成一份完整设置。
//
// 切换渠道会整块重绘表单，没显示的那一组输入已不在 DOM 里。
// 先 stash 再取值，否则在 Telegram 页保存会把飞书凭据一起抹成空串。
function settingsPayload() {
  stashChannelFields();
  const prev = state.settings || {};
  const cur = state.settings || {};
  return {
    scan_interval: Number($('#scan_interval').value),
    health_check_wait: Number($('#health_check_wait').value),
    notify_channel: currentChannel(),
    feishu_app_id: cur.feishu_app_id || '',
    feishu_app_secret: cur.feishu_app_secret || '',
    feishu_chat_id: cur.feishu_chat_id || '',
    telegram_bot_token: cur.telegram_bot_token || '',
    telegram_chat_id: cur.telegram_chat_id || '',
    telegram_api_base: cur.telegram_api_base || '',
    telegram_proxy: cur.telegram_proxy || '',
    dingtalk_client_id: cur.dingtalk_client_id || '',
    dingtalk_client_secret: cur.dingtalk_client_secret || '',
    dingtalk_card_template_id: cur.dingtalk_card_template_id || '',
    dingtalk_conversation_id: cur.dingtalk_conversation_id || '',
    wecom_bot_id: cur.wecom_bot_id || '',
    wecom_secret: cur.wecom_secret || '',
    wecom_chat_id: cur.wecom_chat_id || '',
    // 四个开关必须每次都回传：后端把「缺字段」当成「没设置过」，
    // 会退回按凭据推导，用户显式关掉的状态就丢了。
    feishu_enabled: channelEnabledState('feishu'),
    telegram_enabled: channelEnabledState('telegram'),
    dingtalk_enabled: channelEnabledState('dingtalk'),
    wecom_enabled: channelEnabledState('wecom'),
    registry_mirror: $('#registry_mirror').value.trim(),
    // 自检开关每次都要带上，否则保存一次设置就把用户关掉的检查又打开了
    check_updates: $('#check_updates') ? $('#check_updates').checked : true,
    agent_auto_upgrade: $('#agent_auto_upgrade') ? $('#agent_auto_upgrade').checked : true,
    // 远程节点的两个新开关：默认开，页面上没有这两个控件时（老页面残留）也不能丢。
    node_mirror_follow: $('#node_mirror_follow') ? $('#node_mirror_follow').checked : true,
    node_offline_alert: $('#node_offline_alert') ? $('#node_offline_alert').checked : true,
    node_offline_after: offlineAfterMinutes(),
    // 这两个也是「留空即直连」，所以空串要如实回传，不能丢字段
    selfcheck_proxy: ($('#selfcheck_proxy') ? $('#selfcheck_proxy').value : '').trim(),
    selfcheck_mirror: ($('#selfcheck_mirror') ? $('#selfcheck_mirror').value : '').trim(),
    admin_user: ($('#admin_user').value || '').trim(),
    admin_password: $('#admin_password').value || '',
    rollback_on_failure: $('#rollback_on_failure').checked,
    keep_old_container: $('#keep_old_container').checked,
    registry_insecure: $('#registry_insecure').checked,
    // 这两个开关页面上没有，原样回传即可。
    // 早先这里硬编码成 false/true，会把用户通过环境变量开的自动更新悄悄关掉。
    auto_update: !!prev.auto_update,
    require_confirm: prev.require_confirm !== undefined ? prev.require_confirm : true,
  };
}

// persistSettings 提交当前表单，成功后刷新页面手里的那一份设置。
// 失败时抛出去，由调用方决定怎么提示 —— 「保存设置」与「检测连通性」共用它。
async function persistSettings() {
  const payload = settingsPayload();
  state.settings = await api('/api/settings', { method: 'PUT', body: JSON.stringify(payload) });

  // 凭据变了就立刻换掉本页手里的那一份，
  // 否则下一个请求就会被自己刚设的密码挡在门外。
  if (payload.admin_password) {
    const user = payload.admin_user || 'admin';
    writeAuth(b64(user + ':' + payload.admin_password));
    rememberUser(user);
  } else {
    writeAuth('');
  }
  return payload;
}

async function saveSettings(ev) {
  ev.preventDefault();
  const prev = state.settings || {};
  let payload;
  try {
    payload = await persistSettings();
  } catch (e) {
    toast('保存失败：' + e.message, 'danger');
    return;
  }

  renderSettings();

  const mirrorChanged = (prev.registry_mirror || '') !== payload.registry_mirror;
  // 加速地址变了、又开着「下发给节点」，那就别只说「本机生效」——
  // 用户改这个格子十有八九就是为了让节点也用上，说清楚已经推过去了。
  const followOn = payload.node_mirror_follow !== false;
  const mirrorSaved = mirrorChanged
    ? (followOn
      ? '已保存，加速地址已发给在线节点，下次巡检就用新的源'
      : '已保存，加速地址已生效，点「立即扫描」验证')
    : '';
  // 代理/前缀刚改完，用户要的就是「检查更新能不能通」。顺手在后台试一次，
  // 免得他保存完还得多点一下才知道填对没填对。
  const netChanged = (prev.selfcheck_proxy || '') !== payload.selfcheck_proxy
    || (prev.selfcheck_mirror || '') !== payload.selfcheck_mirror;
  if (netChanged) {
    reconnectSelfCheck();
    toast(mirrorSaved || '已保存，正在用新配置试一次检查更新…');
    return;
  }
  toast(mirrorSaved || '设置已保存并生效');
}

// reconnectSelfCheck 换了 GitHub 连接方式后静默试一次，结果落在「关于」那一行。
async function reconnectSelfCheck() {
  try {
    state.self = await api('/api/selfcheck', { method: 'POST' });
  } catch (_) {
    return;
  }
  renderSelfCheckPill();
  refreshAbout();
  if (state.self.error) {
    toast(state.self.error, 'warn');
  } else {
    toast(state.self.has_update ? '已保存，发现新版本 ' + state.self.latest : '已保存，检查更新正常');
  }
}

// notifyStatusURL 各渠道的链路状态地址。
function notifyStatusURL(ch) {
  if (ch === 'dingtalk') return '/api/dingtalk/status';
  if (ch === 'wecom') return '/api/wecom/status';
  return ch === 'telegram' ? '/api/telegram/status' : '/api/feishu/status';
}

// 各渠道「点什么、在哪点」的文案。四个渠道的说法完全不一样，
// 写死一句「请到群里点一下」会让人在钉钉里找飞书卡片。
const PICK_HINT = {
  feishu: '请到飞书群里点一下卡片上的按钮',
  telegram: '请在 Telegram 里点一下消息上的按钮',
  dingtalk: '请在钉钉群里点一下卡片上的按钮',
  wecom: '请在企业微信会话里点一下卡片上的按钮',
};

// testNotify 给当前渠道发一条测试消息，然后盯着按钮回调。
//
// 「消息发得出去」与「按钮点得动」是两条独立链路：卡片发到了群里只能证明前者。
// 所以发完不撒手，继续等回调计数变大，等到了才算是真的配对了。
async function testNotify() {
  const ch = currentChannel();
  const btn = $('#btn-test-notify');
  if (btn) btn.disabled = true;

  // 先取一次计数当基线：用增量而不是绝对时间，避免被上一次测试留下的旧记录干扰。
  let base = 0;
  try {
    const st = await api(notifyStatusURL(ch));
    base = st.callback_count || 0;
  } catch (_) { /* 状态读不到不影响发消息 */ }

  const testURL = ch === 'telegram'
    ? '/api/telegram/test'
    : (ch === 'dingtalk'
      ? '/api/dingtalk/test'
      : (ch === 'wecom' ? '/api/wecom/test' : '/api/feishu/test'));

  try {
    await api(testURL, { method: 'POST' });
    toast('测试消息已发送，' + (PICK_HINT[ch] || PICK_HINT.feishu));
  } catch (e) {
    toast('发送失败：' + e.message, 'danger');
    if (btn) btn.disabled = false;
    return;
  }
  if (btn) btn.disabled = false;
  waitCallback(base, ch);
}

// waitCallback 轮询等待按钮回调到达，最多 90 秒。
function waitCallback(base, ch) {
  const el = $('#notify-callback-state');
  if (!el) return;
  el.hidden = false;
  el.className = 'callback-state waiting';
  el.textContent = '⏳ 等待按钮回调…' + (PICK_HINT[ch] || PICK_HINT.feishu);

  let left = 45; // 45 次 × 2 秒 = 90 秒
  const timer = setInterval(async () => {
    left -= 1;
    if (left <= 0) {
      clearInterval(timer);
      el.className = 'callback-state bad';
      if (ch === 'dingtalk') {
        el.innerHTML = '未收到按钮回调。请检查：① 卡片模板里三个按钮是否都配了「回传请求」，' +
          '参数是否写成 <code>{"action":"update"}</code> 这种形式；' +
          '② 改完模板后是否<b>重新发布了应用版本</b>；③ 机器人是否还在那个群里。';
      } else if (ch === 'wecom') {
        el.innerHTML = '未收到按钮回调。请检查：① 机器人配置页的「API 模式」是否选了<b>「长连接」</b>；' +
          '② 填的 Secret 是不是<b>长连接专用</b>那个（不是接收 URL 模式的 Token）；' +
          '③ 右栏「当前状态」里长连接是否显示在线；④ 机器人是否还在那个会话里。';
      } else if (ch === 'telegram') {
        el.innerHTML = '未收到按钮回调。请确认 bot 没在别处设过 webhook（它与长轮询互斥），' +
          '并查看设置页上方的长轮询状态与容器日志。';
      } else {
        el.innerHTML = '未收到按钮回调。请检查「回调配置」里是否加了 <code>card.action.trigger</code>，' +
          '以及改完权限/回调后是否<b>发布了新版本</b>。';
      }
      return;
    }
    try {
      const st = await api(notifyStatusURL(ch));
      // 三个渠道共用一个回调计数器，必须再核一次「这次回调来自本渠道」。
      if ((st.callback_count || 0) > base && st.last_channel === ch) {
        clearInterval(timer);
        el.className = 'callback-state ok';
        const t = st.last_callback_at ? fmtTime(Date.parse(st.last_callback_at)) : '';
        el.textContent = '✅ 已收到按钮回调' + (t ? '（' + t + '）' : '') + '，按钮链路正常';
        // 测通即视为「我要用它」：开关本来是关的就顺手打开并保存。
        if (!channelEnabledState(ch)) enableChannelAfterTest(ch);
      }
    } catch (_) { /* 单次失败忽略，下一轮重试 */ }
  }, 2000);
}

// enableChannelAfterTest 测通后自动打开该渠道的开关并保存。
//
// 用户点「发送测试」的意图本来就是「我要用它」，测通了还要再去拨一下开关，
// 纯属多一道会忘的操作。只在这个渠道原本是关的时候才动，避免覆盖用户的显式选择。
async function enableChannelAfterTest(ch) {
  if (!state.settings) return;
  state.settings[ch + '_enabled'] = true;
  const name = (CHANNELS.find((c) => c.id === ch) || {}).name || ch;
  try {
    await persistSettings();
    // 保存会重绘整块表单，原来的结果框已经不在文档里，重新取一次填回去。
    renderSettings();
    const out = $('#notify-callback-state');
    if (out) {
      out.hidden = false;
      out.className = 'callback-state ok';
      out.textContent = '✅ 按钮链路正常，已自动启用「' + name + '」通知';
    }
    loadStats();
  } catch (e) {
    toast('已测通，但自动启用失败：' + e.message, 'danger');
  }
}

// toggleChannelSwitch 拨动某个渠道的总开关，立即保存。
//
// 即时保存而不是等「保存设置」：用户拨开关的意图就是此刻生效，
// 若还要再点一次保存，很容易漏掉 —— 然后以为开关坏了。
async function toggleChannelSwitch(ch, on) {
  if (!state.settings) return;
  // 先把输入框里的值收进 state，否则刚填完还没保存的凭据会被判成「没配」
  stashChannelFields();

  const name = (CHANNELS.find((c) => c.id === ch) || {}).name || ch;
  if (on && !channelConfigured(ch)) {
    toast('先把 ' + name + ' 的凭据填完，再打开开关', 'danger');
    renderSettings(); // 勾选框恢复成关闭状态
    return;
  }

  state.settings[ch + '_enabled'] = on;
  try {
    await persistSettings();
    renderSettings();
    loadStats();
    toast(on
      ? '已启用「' + name + '」通知'
      : '已暂停「' + name + '」通知，配置已保留');
  } catch (e) {
    toast('保存失败：' + e.message, 'danger');
    renderSettings();
  }
}

// checkTelegram 只验证「连不连得上 + Token 对不对」。
//
// 这一步刻意不要求 chat_id：「读不到会话」和「连不上 Telegram」现象很像，
// 但病因完全不同。带上前一次填的值去试，是因为刚改完代理还没保存时最想验证的就是它。
async function checkTelegram() {
  const box = $('#tg-check-result');
  const btn = $('#btn-tg-check');
  if (!box) return;
  if (btn) btn.disabled = true;
  box.hidden = false;
  box.className = 'chat-pick loading';
  box.textContent = '正在连接 Telegram…';
  try {
    const r = await api('/api/telegram/check', {
      method: 'POST',
      body: JSON.stringify({
        telegram_bot_token: ($('#telegram_bot_token') || {}).value || '',
        telegram_api_base: ($('#telegram_api_base') || {}).value || '',
        telegram_proxy: ($('#telegram_proxy') || {}).value || '',
      }),
    });
    box.className = 'chat-pick empty';
    const via = r.proxy ? '代理 ' + esc(r.proxy) : '直连';
    if (r.ok) {
      // 检测通过就把这份配置存下来。
      //
      // 检测用的是「输入框里的值」，长轮询用的是「已保存的值」：只填不保存时
      // 检测永远是通的，会话列表却一直是空的 —— 现象上像检测在骗人。
      // 而这本来就是用户填代理的目的，多让他点一次保存纯属多余。
      const saved = state.settings || {};
      const dirty = ['telegram_bot_token', 'telegram_chat_id', 'telegram_api_base', 'telegram_proxy']
        .some((k) => (saved[k] || '').trim() !== ((($('#' + k) || {}).value || '').trim()))
        || currentChannel() !== 'telegram';

      let note;
      if (dirty) {
        try {
          await persistSettings();
          // 保存会重绘整块表单，原来的结果框已经不在文档里了，
          // 重新渲染之后要再取一次新节点填回去。
          renderSettings();
          note = '<p class="chat-ok">✓ 已自动保存并生效，现在可以「读取会话」</p>';
        } catch (e) {
          note = `<p class="chat-warn">⚠ 检测通过，但保存失败：${esc(e.message)}
            —— 点页面底部「保存设置」重试。</p>`;
        }
      } else {
        note = '<p class="hint-text">这份配置已经保存过了，无需再次保存。</p>';
      }

      const out = $('#tg-check-result') || box;
      out.hidden = false;
      out.className = 'chat-pick empty';
      out.innerHTML = `<p class="chat-ok">✓ 连得上，Token 有效：@${esc(r.username)}</p>
        <p class="hint-text">${esc(r.apiBase)} · ${via}</p>${note}`;
      return;
    } else {
      box.innerHTML = `<p class="chat-err">${esc(r.error || '未知错误')}</p>
        <p>这里失败说明是<b>网络或代理</b>的问题，跟 chat_id 无关 ——
        填好代理地址再点一次，通了再去读会话。</p>
        <p class="hint-text">${esc(r.apiBase)} · ${via}</p>`;
    }
  } catch (e) {
    box.className = 'chat-pick empty';
    box.innerHTML = `<p class="chat-err">检测失败：${esc(e.message)}</p>`;
  } finally {
    if (btn) btn.disabled = false;
  }
}

// loadTelegramChats 读取 bot 最近收到过消息的会话，点一下就填进 chat_id。
//
// Telegram 拿不到「这个 bot 在哪些群里」的列表接口，只能上报最近见过的会话；
// 所以文档里那一步「先在群里说句话」是必须的。而「说句话」能不能被 bot 收到，
// 还取决于群的隐私模式 —— 空列表时把这两件事一起说清楚，否则用户会以为是自己配错了。
async function loadTelegramChats() {
  const box = $('#tg-chat-list');
  const btn = $('#btn-tg-chats');
  if (!box) return;
  if (btn) btn.disabled = true;
  box.hidden = false;
  box.className = 'chat-pick loading';
  box.textContent = '读取中…';
  try {
    const st = await api('/api/telegram/status');
    const chats = st.chats || [];
    if (!chats.length) {
      box.className = 'chat-pick empty';
      const err = st.last_error
        ? `<p class="chat-err">长轮询报错：${esc(st.last_error)}</p>`
        : '';
      // 有报错就先说网络，没报错才轮到隐私模式 —— 两种病因的现象一模一样，
      // 一起列出来会让人两条路都试一半。
      const body = st.last_error
        ? `<p><b>先解决连不上的问题：</b>填好上面的代理地址，点「检测连通性」跑通，
             再回来读会话。国内直连 <code>api.telegram.org</code> 基本不通。</p>
           <p>如果检测连通性明明是通的，那就是代理<b>没保存成功</b> ——
             长轮询只认保存过的配置，点一下「保存并应用」再重新读。</p>`
        : `<p><b>群里的普通聊天，bot 默认是收不到的</b>（成员列表里会显示「无权读取消息」）。两个办法：</p>
           <p>① 在群里 <code>@你的bot用户名</code> 说一句话，或发 <code>/start</code>；<br>
              ② 找 <code>@BotFather</code> 发 <code>/setprivacy</code> → 选中这个 bot → <code>Disable</code>，
              改完<b>把 bot 移出群再重新拉进来</b>（不退群重进不生效）。</p>
           <p class="hint-text">做完再点一次「读取会话」。私聊 bot 不受这个限制。</p>`;
      // 底下一行是「服务实际在用什么配置」。读不到会话时这三项比任何文案都管用：
      // 轮询没跑起来、地址填错、代理没生效，一看就知道该改哪儿。
      const diag = [
        '轮询：' + (st.polling ? '运行中' : (st.configured ? '未运行' : 'Token 未配置')),
        'API：' + (st.api_base || 'https://api.telegram.org'),
        '代理：' + (st.proxy || '直连'),
      ].join(' · ');
      box.innerHTML = `<p>还没收到过任何消息。</p>${err}${body}
        <p class="chat-diag mono">${esc(diag)}</p>`;
      return;
    }
    box.className = 'chat-pick';
    box.innerHTML = chats.map((c) => `
      <button type="button" class="chat-item" data-chat="${esc(c.id)}" data-field="telegram_chat_id">
        <span class="chat-title">${esc(c.title || c.id)}</span>
        <span class="chat-id mono">${esc(c.id)}</span>
      </button>`).join('');
  } catch (e) {
    box.className = 'chat-pick empty';
    box.textContent = '读取失败：' + e.message;
  } finally {
    if (btn) btn.disabled = false;
  }
}

// checkDingTalk 只验「凭据对不对、能不能连上钉钉」。
//
// 刻意不要求模板与会话：「发送测试卡片」必须先有这两样才跑得起来，
// 报出来的却是「还缺卡片模板」，人就跑去折腾模板，而真正的问题在别处。
async function checkDingTalk() {
  const box = $('#dt-check-result');
  const btn = $('#btn-dt-check');
  if (!box) return;
  if (btn) btn.disabled = true;
  box.hidden = false;
  box.className = 'chat-pick loading';
  box.textContent = '正在连接钉钉…';
  try {
    const r = await api('/api/dingtalk/check', {
      method: 'POST',
      body: JSON.stringify({
        dingtalk_client_id: ($('#dingtalk_client_id') || {}).value || '',
        dingtalk_client_secret: ($('#dingtalk_client_secret') || {}).value || '',
      }),
    });
    box.className = 'chat-pick empty';
    if (r.ok) {
      // 检测用输入框里的值、长连接用已保存的值：只填不保存时检测永远是通的，
      // 「读取会话」却一直是空的 —— 现象上像检测在骗人。通了就直接存下来。
      const saved = state.settings || {};
      const dirty = ['dingtalk_client_id', 'dingtalk_client_secret']
        .some((k) => (saved[k] || '').trim() !== ((($('#' + k) || {}).value || '').trim()));

      let note;
      if (dirty) {
        try {
          await persistSettings();
          // 保存会重绘表单，原来的结果框已经不在文档里，重新取一次填回去。
          renderSettings();
          note = '<p class="chat-ok">✓ 已自动保存，长连接正在建立</p>';
        } catch (e) {
          note = `<p class="chat-warn">⚠ 检测通过，但保存失败：${esc(e.message)}
            —— 点页面底部「保存设置」重试。</p>`;
        }
      } else {
        note = '<p class="hint-text">这份配置已经保存过了。</p>';
      }

      const out = $('#dt-check-result') || box;
      out.hidden = false;
      out.className = 'chat-pick empty';
      out.innerHTML = `<p class="chat-ok">✓ 凭据有效，能正常调用钉钉接口</p>
        <p class="hint-text">接着填卡片模板 ID 与会话 ID，再点上面的「发送钉钉测试卡片」。</p>${note}`;
      return;
    }
    box.innerHTML = `<p class="chat-err">${esc(r.error || '未知错误')}</p>
      <p>这里失败说明是<b>凭据写错</b>或<b>这台机器连不上 api.dingtalk.com</b>，
      跟卡片模板、会话 ID 都没关系。</p>`;
  } catch (e) {
    box.className = 'chat-pick empty';
    box.innerHTML = `<p class="chat-err">检测失败：${esc(e.message)}</p>`;
  } finally {
    if (btn) btn.disabled = false;
  }
}

// loadDingTalkChats 读取机器人在钉钉上收到过消息的会话。
//
// 钉钉没有「机器人所在群列表」这种接口，会话 ID 只能在机器人收到消息时
// 随着回调带上来 —— 所以「先在群里 @它 说句话」是必须的，不是多余的步骤。
async function loadDingTalkChats() {
  const box = $('#dt-chat-list');
  const btn = $('#btn-dt-chats');
  if (!box) return;
  if (btn) btn.disabled = true;
  box.hidden = false;
  box.className = 'chat-pick loading';
  box.textContent = '读取中…';
  try {
    const st = await api('/api/dingtalk/status');
    const chats = st.chats || [];
    if (!chats.length) {
      box.className = 'chat-pick empty';
      const err = st.last_error ? `<p class="chat-err">长连接报错：${esc(st.last_error)}</p>` : '';
      const body = st.last_error
        ? `<p><b>先解决连不上的问题：</b>点上面的「检测连通性」跑通，再回来读会话。</p>`
        : `<p><b>钉钉不会主动告诉服务「机器人在哪些群」</b>，只有机器人在群里收到一条消息时，
             才会把会话 ID 一起带上来。</p>
           <p>① 确认机器人已经加进目标群；<br>
              ② 在群里 <code>@机器人 说句话</code>；<br>
              ③ 回来再点一次「读取会话」。</p>
           <p class="field-hint">收到一次就够了 —— 会话会存进数据库，之后重启、装新包都不用再 @。</p>`;
      // 底下一行是「服务实际在用什么配置」：读不到会话时这三项比任何文案都管用
      const diag = [
        '长连接：' + (st.stream ? '运行中' : (st.configured ? '未运行' : '凭据未配置')),
        '会话：' + (st.conversation_id || '未填'),
        '还缺：' + ((st.missing || []).join('、') || '无'),
      ].join(' · ');
      box.innerHTML = `<p>还没收到过任何消息。</p>${err}${body}
        <p class="chat-diag mono">${esc(diag)}</p>`;
      return;
    }
    box.className = 'chat-pick';
    box.innerHTML = chats.map((c) => `
      <button type="button" class="chat-item" data-chat="${esc(c.id)}" data-field="dingtalk_conversation_id">
        <span class="chat-title">${esc(c.title || c.id)}</span>
        <span class="chat-id mono">${esc(c.id)}</span>
      </button>`).join('') +
      // 这句是给用户吃定心丸的：早先版本这份列表只在内存里，装个包重启就空了，
      // 每次都得重新 @ 一次机器人。现在落库了，得让人看得见这个变化。
      `<p class="field-hint">列表已存进数据库，重启后仍在；只有加了一个新群才需要再 @ 一次机器人。</p>`;
  } catch (e) {
    box.className = 'chat-pick empty';
    box.textContent = '读取失败：' + e.message;
  } finally {
    if (btn) btn.disabled = false;
  }
}

// loadWeComChats 读取机器人在企业微信上收到过消息的会话。
//
// 与钉钉同一套逻辑：企业微信也没有「机器人所在群列表」接口，
// 会话 ID 只能在它收到消息时随回调带上来。
async function loadWeComChats() {
  const box = $('#wc-chat-list');
  const btn = $('#btn-wc-chats');
  if (!box) return;
  if (btn) btn.disabled = true;
  box.hidden = false;
  box.className = 'chat-pick loading';
  box.textContent = '读取中…';
  try {
    const st = await api('/api/wecom/status');
    state.wecom = st;
    const chats = st.chats || [];
    if (!chats.length) {
      box.className = 'chat-pick empty';
      const err = st.last_error ? `<p class="chat-err">长连接报错：${esc(st.last_error)}</p>` : '';
      const body = st.last_error
        ? '<p><b>先解决连不上的问题：</b>长连接没建立时，机器人收不到任何消息。</p>'
        : `<p><b>企业微信不会主动告诉服务「机器人在哪些群」</b>，只有机器人在会话里收到一条消息时，
             才会把会话 ID 一起带上来。</p>
           <p>① 确认机器人已经加进目标群；<br>
              ② 在群里 <code>@机器人 说句话</code>（单聊就直接发一句）；<br>
              ③ 回来再点一次「读取会话」。</p>
           <p class="field-hint">收到一次就够了 —— 会话会存进数据库，之后重启、装新包都不用再 @。</p>`;
      const diag = [
        '长连接：' + (st.running ? '运行中' : (st.configured ? '未运行' : '凭据未配置')),
        '会话：' + (st.chat_id || '未填'),
        '还缺：' + ((st.missing || []).join('、') || '无'),
      ].join(' · ');
      box.innerHTML = `<p>还没收到过任何消息。</p>${err}${body}
        <p class="chat-diag mono">${esc(diag)}</p>`;
      return;
    }
    box.className = 'chat-pick';
    box.innerHTML = chats.map((c) => `
      <button type="button" class="chat-item" data-chat="${esc(c.id)}" data-field="wecom_chat_id">
        <span class="chat-title">${esc(c.title || c.id)}</span>
        <span class="chat-id mono">${esc(c.id)}</span>
      </button>`).join('') +
      '<p class="field-hint">列表已存进数据库，重启后仍在；只有加了一个新群才需要再 @ 一次机器人。</p>';
  } catch (e) {
    box.className = 'chat-pick empty';
    box.textContent = '读取失败：' + e.message;
  } finally {
    if (btn) btn.disabled = false;
  }
}

function renderRegistryTest(res) {  const rows = (res.results || []).map((r) => `
    <div class="test-line ${r.ok ? 'ok' : 'bad'}">
      <span class="test-host mono">${esc(r.label)}${r.mirror ? '<i class="tag-mini">加速</i>' : ''}</span>
      <span class="test-res">${r.ok
        ? '连通 · ' + r.elapsed_ms + ' ms'
        : '失败 · ' + esc(r.error_brief || r.error || '未知原因')}</span>
    </div>`).join('');

  const dm = res.daemon_mirrors || [];
  const hint = dm.length
    ? `<p class="field-hint">Docker 守护进程已配加速器：<code>${esc(dm.join(', '))}</code>，加速地址留空时会自动沿用。</p>`
    : `<p class="field-hint">Docker 守护进程未配置 registry-mirrors。</p>`;

  const allBad = (res.results || []).length > 0 && res.results.every((r) => !r.ok);
  const advice = allBad
    ? `<p class="field-hint warn-text">全部不通。若网络能正常上网，多半是 Docker Hub 在国内不可达，
       请在上一栏填入可用的加速地址（例如 <code>docker.1ms.run</code> 或 <code>docker.1panel.live</code>）后重试。</p>`
    : '';

  return `
    <p class="field-hint">探针镜像：<code>${esc(res.reference)}</code>${res.docker_hub ? '（Docker Hub）' : ''}</p>
    ${rows}
    ${hint}
    ${advice}
  `;
}

// 逐个探测直连与各加速源。镜像全是「未知」时，这一步能把
// 「到底是网络不通、还是 tag 不在」直接摊开，不用去猜。
// ---- 推送令牌 ----

// rotatePushToken 生成或重置推送令牌。
//
// 只改这一块的 DOM，不整体重渲染设置页：用户很可能还有别的字段没保存，
// 整页重绘会把输入的内容一起清掉。
async function rotatePushToken() {
  const btn = $('#btn-push-token');
  if (btn) { btn.disabled = true; btn.textContent = '生成中…'; }
  try {
    const r = await api('/api/push/token', { method: 'POST' });
    if (state.settings) state.settings.push_token = r.push_token;
    const input = $('#push_token');
    if (input) input.value = r.push_token;
    if (btn) btn.textContent = '重新生成';
    const hint = $('#push-hint');
    if (hint) hint.textContent = '重新生成后旧令牌立即失效，正在用它的应用要同步换成新的。';
    const pre = $('#push-example');
    if (pre) pre.textContent = pushSnippet(r.push_token);
    const revoke = $('#btn-push-revoke');
    if (revoke) revoke.hidden = false;
    toast('令牌已生成，复制后填到调用方配置里');
  } catch (e) {
    toast(e.message, 'error');
    if (btn) btn.textContent = state.settings && state.settings.push_token ? '重新生成' : '生成令牌';
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function revokePushToken() {
  if (!confirm('停用后，正在调用推送接口的应用会立刻发不出去。确定？')) return;
  try {
    await api('/api/push/token', { method: 'DELETE' });
    if (state.settings) state.settings.push_token = '';
    const input = $('#push_token');
    if (input) input.value = '';
    const btn = $('#btn-push-token');
    if (btn) btn.textContent = '生成令牌';
    const revoke = $('#btn-push-revoke');
    if (revoke) revoke.hidden = true;
    const hint = $('#push-hint');
    if (hint) hint.textContent = '生成之后接口才可用；不生成，这个入口就不存在。';
    const pre = $('#push-example');
    if (pre) pre.textContent = pushSnippet('');
    toast('推送接口已停用');
  } catch (e) { toast(e.message, 'error'); }
}

async function copyPushSnippet() {
  const pre = $('#push-example');
  if (!pre) return;
  const ok = await copyText(pre.textContent);
  toast(ok ? '示例已复制' : '复制失败，请手动选中');
}

async function testRegistry() {
  const btn = $('#btn-test-registry');
  const box = $('#registry-test-result');
  if (!btn || !box) return;

  btn.disabled = true;
  const label = btn.textContent;
  btn.textContent = '检测中…';
  box.hidden = false;
  box.innerHTML = '<p class="field-hint">正在逐个探测，可能要十几秒…</p>';

  try {
    const res = await api('/api/registry/test', {
      method: 'POST',
      body: JSON.stringify({ reference: '' }),
    });
    box.innerHTML = renderRegistryTest(res);
  } catch (e) {
    box.innerHTML = `<p class="test-line bad">检测失败：${esc(e.message)}</p>`;
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

const TABS = ['containers', 'nodes', 'images', 'tasks', 'notifications', 'settings'];

// 标签页写进 hash，刷新后还能停在原来那一页
function initialTab() {
  const h = (location.hash || '').replace(/^#/, '');
  return TABS.includes(h) ? h : 'containers';
}

function switchTab(name) {
  state.tab = name;
  document.querySelectorAll('.tab').forEach((el) => {
    el.classList.toggle('active', el.dataset.tab === name);
  });
  document.querySelectorAll('.panel').forEach((el) => {
    el.classList.toggle('active', el.id === 'panel-' + name);
  });
  try {
    const want = name === 'containers' ? location.pathname : '#' + name;
    const cur = location.hash || location.pathname;
    if (cur !== want) history.replaceState(null, '', want);
  } catch (_) { /* 某些内嵌浏览器禁用 history，忽略即可 */ }

  // 切到节点页时补刷一次：节点在别的机器上装好后，用户的第一反应是切过来看，
  // 这里不该让他等定时轮询。静默刷，不弹提示。
  if (name === 'nodes') refreshNodes({ quiet: true });
}

/* ---------- 事件绑定 ---------- */

document.addEventListener('click', async (ev) => {
  const tab = ev.target.closest('.tab');
  if (tab) { switchTab(tab.dataset.tab); return; }

  if (ev.target.closest('[data-close]')) {
    $('#drawer').hidden = true;
    return;
  }

  if (ev.target.closest('[data-close-modal]')) {
    closeCleanModal();
    closeUpdateModal();
    closeNodeDelModal();
    return;
  }

  // 节点相关。按钮放在 node-head 里，而 node-head 本身带展开动作，
  // 所以这几个分支必须排在「点整行」之前，否则点按钮会连带展开/收起。
  if (ev.target.closest('#btn-add-node')) { openNodeForm(); return; }
  if (ev.target.closest('#btn-refresh-nodes')) { refreshNodes(); return; }
  if (ev.target.closest('#btn-node-cancel')) { closeNodeForm(); return; }
  if (ev.target.closest('#btn-node-create')) { createNode(); return; }

  const nUpg = ev.target.closest('[data-node-upgrade]');
  if (nUpg) { nodeUpgrade(Number(nUpg.dataset.nodeUpgrade)); return; }

  const nScan = ev.target.closest('[data-node-scan]');
  if (nScan) { nodeScan(Number(nScan.dataset.nodeScan)); return; }

  const nReload = ev.target.closest('[data-node-reload]');
  if (nReload) { loadNodeContainers(Number(nReload.dataset.nodeReload)); return; }

  const nUpd = ev.target.closest('[data-node-update]');
  if (nUpd) { nodeUpdate(Number(nUpd.dataset.nodeUpdate), nUpd.dataset.cid); return; }

  const nProj = ev.target.closest('[data-node-project]');
  if (nProj) { nodeProjectUpdate(Number(nProj.dataset.nodeProject), nProj.dataset.project); return; }

  const nTok = ev.target.closest('[data-node-token]');
  if (nTok) { rotateNodeToken(Number(nTok.dataset.nodeToken)); return; }

  const nDel = ev.target.closest('[data-node-del]');
  if (nDel) { openNodeDelModal(Number(nDel.dataset.nodeDel)); return; }

  if (ev.target.id === 'btn-node-del-ok') { confirmNodeDel(); return; }
  if (ev.target.closest('[data-copy-uninstall]')) { copyUninstallCmd(); return; }

  if (ev.target.closest('[data-copy-node-cmd]')) {
    copyNodeCommand();
    return;
  }

  const nToggle = ev.target.closest('[data-node-toggle]');
  if (nToggle && !ev.target.closest('button')) {
    toggleNode(Number(nToggle.dataset.nodeToggle));
    return;
  }

  const row = ev.target.closest('.row');
  if (row && row.dataset.task && !ev.target.closest('button')) {
    openTaskLog(row.dataset.task);
    return;
  }

  const btn = ev.target.closest('button[data-act]');
  if (btn) {
    const rowEl = btn.closest('.row');
    const act = btn.dataset.act;

    // 镜像页只剩「删除」一个动作；更新容器请到「容器」页
    if (act === 'del') {
      const img = state.images.find((i) => i.id === rowEl.dataset.imgid);
      if (!img) return;
      removeLocalImage(img);
      return;
    }

    // 容器页：更新 = 拉新镜像 + 重建容器
    const id = Number(rowEl.dataset.id);
    const img = state.containers.find((i) => i.id === id);
    if (!img) return;
    if (act === 'update') triggerUpdate(id);
    if (act === 'ignore') toggleIgnore(img);
    if (act === 'remove') removeMonitorItem(id);
    return;
  }

  const notifDel = ev.target.closest('button[data-notif-del]');
  if (notifDel) {
    if (!confirm('删除这条通知？')) return;
    api('/api/notifications/' + notifDel.dataset.notifDel, { method: 'DELETE' })
      .then(() => { toast('已删除'); loadNotifications(); loadStats(); })
      .catch((e) => toast('删除失败：' + e.message, 'danger'));
    return;
  }

  const notifUp = ev.target.closest('button[data-notif-update]');
  if (notifUp) {
    // 判定全在服务端：真在跑就回一句「还在进行中」，不会起第二个任务。
    api('/api/notifications/' + notifUp.dataset.notifUpdate + '/update', { method: 'POST' })
      .then((d) => { toast(d.message || '已下发更新'); loadNotifications(); loadStats(); })
      .catch((e) => toast('无法更新：' + e.message, 'danger'));
    return;
  }

  const readBtn = ev.target.closest('button[data-read]');
  if (readBtn) {
    api('/api/notifications/' + readBtn.dataset.read + '/read', { method: 'POST' })
      .then(() => { loadNotifications(); loadStats(); })
      .catch((e) => toast('操作失败：' + e.message, 'danger'));
    return;
  }

  // 渠道切换：先把当前可见的输入存回 state，再换渠道重绘，
  // 否则刚填的 token 会随表单一起丢掉。
  const chip = ev.target.closest('.channel-chip[data-channel]');
  if (chip) {
    stashChannelFields();
    if (state.settings) state.settings.notify_channel = chip.dataset.channel;
    renderSettings();
    return;
  }

  // 点会话即填入并直接保存。
  // 那串负数（飞书/Telegram）或 cid 开头的长串（钉钉）没人愿意抄，
  // 填进去却还要再点一次「保存设置」，等于白填。
  const chatItem = ev.target.closest('.chat-item[data-chat]');
  if (chatItem) {
    const field = chatItem.dataset.field || 'telegram_chat_id';
    const input = $('#' + field);
    if (input) input.value = chatItem.dataset.chat;
    const box = $('#dt-chat-list');
    const tgBox = $('#tg-chat-list');
    if (field === 'dingtalk_conversation_id' && box) box.hidden = true;
    if (field === 'telegram_chat_id' && tgBox) tgBox.hidden = true;
    try {
      await persistSettings();
      // 保存成功要重绘：渠道胶囊的「已启用/已暂停」、顶栏状态都得跟着变。
      renderSettings();
      toast('已填入会话 ID 并保存');
    } catch (e) {
      toast('已填入会话 ID，但保存失败：' + e.message, 'danger');
    }
    return;
  }

  // 升级步骤里的命令：按 data-copy 下标取原文，不从 DOM 里回读 —— pre 里带换行，
  // textContent 取回来会被浏览器规整过，粘出去就变形了
  const copyBtn = ev.target.closest('button[data-copy]');
  if (copyBtn) {
    const text = updateCmds[Number(copyBtn.dataset.copy)];
    if (text) {
      copyText(text).then((ok) => toast(ok ? '已复制' : '复制失败，请手动选中命令', ok ? 'ok' : 'warn'));
    }
    return;
  }

  const upTab = ev.target.closest('.update-tab[data-form]');
  if (upTab) {
    updateForm = upTab.dataset.form;
    renderUpdateModal();
    return;
  }

  if (ev.target.id === 'selfcheck-pill') openUpdateModal();
  if (ev.target.id === 'btn-selfcheck') runSelfCheck();
  if (ev.target.id === 'btn-scan') doScan(false);
  if (ev.target.id === 'btn-force') doScan(true);
  if (ev.target.id === 'btn-test-notify') testNotify();
  if (ev.target.id === 'btn-tg-chats') loadTelegramChats();
  if (ev.target.id === 'btn-tg-check') checkTelegram();
  if (ev.target.id === 'btn-dt-chats') loadDingTalkChats();
  if (ev.target.id === 'btn-wc-chats') loadWeComChats();
  if (ev.target.id === 'btn-dt-check') checkDingTalk();
  if (ev.target.id === 'btn-test-registry') testRegistry();
  if (ev.target.id === 'btn-push-token') rotatePushToken();
  if (ev.target.id === 'btn-push-revoke') revokePushToken();
  if (ev.target.id === 'btn-copy-push') copyPushSnippet();
  if (ev.target.id === 'btn-clean') openCleanModal();
  if (ev.target.id === 'btn-clean-ok') doClean();
  if (ev.target.id === 'btn-remove-picked') removePicked();
  if (ev.target.id === 'btn-read-all') {
    api('/api/notifications/read-all', { method: 'POST' })
      .then(() => { loadNotifications(); loadStats(); toast('已全部标记为已读'); })
      .catch((e) => toast('操作失败：' + e.message, 'danger'));
  }
  if (ev.target.id === 'btn-clear-read') {
    if (!confirm('清空全部已读通知？此操作不可撤销。')) return;
    api('/api/notifications/read', { method: 'DELETE' })
      .then((r) => { loadNotifications(); loadStats(); toast('已清空 ' + (r.deleted || 0) + ' 条'); })
      .catch((e) => toast('操作失败：' + e.message, 'danger'));
  }
  if (ev.target.id === 'btn-add') {
    const ref = prompt('输入要监控的镜像引用，例如 nginx:latest 或 ghcr.io/foo/bar:1.2.3');
    if (!ref) return;
    api('/api/images', { method: 'POST', body: JSON.stringify({ reference: ref.trim() }) })
      .then(() => { toast('已添加，下次扫描开始监控'); loadImages(); })
      .catch((e) => toast('添加失败：' + e.message, 'danger'));
  }
});

document.addEventListener('input', (ev) => {
  if (ev.target.id === 'container-search') {
    state.containerSearch = ev.target.value;
    renderContainers();
  }
  if (ev.target.id === 'image-search') {
    state.imageSearch = ev.target.value;
    renderImages();
  }
});

document.addEventListener('change', (ev) => {
  const sw = ev.target.closest('input[data-chan-switch]');
  if (sw) {
    toggleChannelSwitch(sw.dataset.chanSwitch, sw.checked);
    return;
  }

  const sel = ev.target.closest('select[data-mode]');
  if (sel) {
    setMode(Number(sel.dataset.mode), sel.value);
    return;
  }

  const pick = ev.target.closest('input[data-pick]');
  if (pick) {
    const id = pick.dataset.pick;
    if (pick.checked) state.picked.add(id);
    else state.picked.delete(id);
    return;
  }

  if (ev.target.name === 'clean-mode') renderCleanPreview();
});

document.addEventListener('submit', (ev) => {
  if (ev.target.id === 'settings-form') saveSettings(ev);
});

/* ---------- 启动 ---------- */

// 轮询间隔。列表内容只有两个来源会变：本机扫描，以及**在飞书里点了卡片按钮**。
// 后者完全绕开页面 —— 页面不主动回查，就会一直停在旧数据上：
// 容器早就重建完了，列表里还写着「有更新」，看着像根本没生效。
const POLL_MS = 5000;
// 空闲时也按轮数定期整份重载。飞书那边的操作不经过页面，
// 只靠「忙 → 闲」的状态跳变会漏掉（比如页面停在别的标签时）。
const IDLE_FULL_EVERY = 6;

async function pollTick() {
  if (pollBusy || !uiReady) return;
  pollBusy = true;
  try {
    const wasBusy = !!(state.stats && (state.stats.updating || state.stats.scanning));
    await loadStats();
    const busy = !!(state.stats && (state.stats.updating || state.stats.scanning));

    // 任务在跑 → 整份重载看进度；刚跑完 → 再整份重载一次，把最终结果取回来。
    // 这一次是必须的：更新结束的那一刻数据才真正落定，只刷统计会漏掉列表变化。
    if (busy || wasBusy) {
      idleTicks = 0;
      await refresh({ quiet: true });
      return;
    }

    idleTicks = (idleTicks + 1) % IDLE_FULL_EVERY;
    if (idleTicks === 0) await refresh({ quiet: true });

    // 停在节点页时自己盯着点：
    // 1. 有节点还没交过巡检结果 —— 否则「巡检中…」会一直挂在那儿；
    // 2. 有操作在进行（更新要几十秒到几分钟）—— 进度就靠这一轮轮询推进。
    // 更新刚结束的那一轮也补拉一次容器，行状态才会从「待更新」翻成「已是最新」。
    if (state.tab === 'nodes') {
      const waiting = state.nodes.some((n) => n.status === 'online' && !hasTime(n.last_report_at));
      const batching = Object.values(state.nodeBatches || {}).some((b) => b && b.running);
      if (Object.keys(state.nodeOps || {}).length || waiting || batching) {
        await refreshNodes({ quiet: true });
      }
      const running = Object.values(state.nodeOps || {})
        .filter((o) => o.phase !== 'done' && o.phase !== 'failed').length;
      if (running || lastRunningOps) await refreshOpenNodes();
      lastRunningOps = running;
    }
  } catch (_) {
    // 网络抖一下就跳过这一轮，下一轮自己会好
  } finally {
    pollBusy = false;
  }
}

function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(pollTick, POLL_MS);
}

// 页面被切走再切回来、或者重新拿到焦点时补一次整份刷新。
// 飞牛桌面是用 iframe 常驻加载页面的，切来切去并不会重新执行 boot()，
// 数据就停在切走的那一刻 —— 重新看到页面时它应该是当下的事实。
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && uiReady) refresh({ quiet: true });
});
window.addEventListener('focus', () => {
  if (uiReady) refresh({ quiet: true });
});

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

async function doLogin(ev) {
  ev.preventDefault();

  const user = ($('#login-user').value || '').trim() || 'admin';
  const pass = $('#login-pass').value || '';
  const prev = authToken;

  writeAuth(b64(user + ':' + pass));

  try {
    await loadStats();
  } catch (err) {
    writeAuth(prev);
    if (err && err.unauthorized) {
      showLogin('用户名或密码不正确');
    } else {
      showLogin((err && err.message) || '登录失败');
    }
    return;
  }

  hideLogin();
  uiReady = true;
  switchTab(initialTab());
  toast('已登录');
  rememberUser(user);
  loadSettings().catch(() => {});
  refresh();
  startPolling();
}

async function boot() {
  try {
    await loadStats();
  } catch (err) {
    if (err && err.unauthorized) {
      // 登录层已经显示，等用户输入
      return;
    }
    toast((err && err.message) || '无法连接到服务', 'danger');
    return;
  }

  hideLogin();
  uiReady = true;
  switchTab(initialTab());
  rememberUser(userFromToken());
  // 打开页面先整份取一次，不要先摆上一屏旧数据再等轮询慢慢补
  loadSettings().catch(() => {});
  refresh();
  startPolling();
}

document.addEventListener('submit', (ev) => {
  if (ev.target.id === 'login-form') doLogin(ev);
});

// 主题按钮有两个（顶栏一个、登录层一个），绑同一个切换动作。
// data-theme 已由 <head> 里的内联脚本设好，这里只把按钮的提示语对齐。
document.querySelectorAll('#btn-theme, #btn-theme-login').forEach((btn) => {
  btn.addEventListener('click', toggleTheme);
});
applyTheme(currentTheme());

boot();
