// mvp/demo.mjs
// ─────────────────────────────────────────────────────────────────────────────
// 一键 demo：进程内起 server → 建一个纯 agent 讨论室 → 3 个不同人格的 agent
// 自己聊完、自己收敛 → 打印结论 / 讨论简要 / 成本统计。
//
//   node mvp/demo.mjs
//
// 全程没有人参与：人只建了房间、设了预算，然后看结果页。
// 零 npm 依赖，只用 node: 内置能力。
// ─────────────────────────────────────────────────────────────────────────────

import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

import { RoomAgent, createRoom, getResult, sleep } from './agent.mjs';
import { persona, hasBYOK, byokInfo } from './worker.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));

// 进程内 server 句柄：退出前要关掉，否则 listen 的 socket 会吊住事件循环
let SERVER = null;

/* ══════════════════════════════════════════════════════════════════════════
   议题与三个 agent
   ──────────────────────────────────────────────────────────────────────────
   人格论点被钉到具体议题上（¥199 / ¥29），这样输出读起来像真的在吵，
   而不是三段各自独白的模板。
   ══════════════════════════════════════════════════════════════════════════ */

const ROOM_SPEC = {
  topic: '把定价从年付 ¥199 改成月付 ¥29，要不要？',
  rules: '只讨论定价策略本身；必须给出可执行结论；不许互相客套。',
  tokenBudget: 200_000,
  maxRounds: 3,
  maxAgents: 3, // demo 只进 3 个 agent，所以房间容量就设 3，避免房间等满员
  minutesLimit: 60,
};

const TEAM = [
  {
    alias: '小周 Claude',
    model: 'claude-sonnet',
    persona: '激进派',
    stance: 'agree',
    conclusion: '该改，且不必二选一：新旧价格并行一个月，用数据决定谁留下。',
    points: [
      '我认为该改，而且现在就该改。年付 ¥199 真正的问题不是价格，是决策门槛：让一个还没用过产品的人先掏 199，等于把大部分人挡在门外，我们连"他会不会用"都测不到。',
      '担心现金流我理解，但这个担心换不来增长。月付 ¥29 的价值是一个月内拿到十几倍的行为样本，样本本身就是资产，而年付把样本锁在付费墙后面。',
      '而且这根本不是二选一。年付保留成"月付 × 10 个月"的折扣选项，两条腿走路，没人逼你砍掉哪一条。',
      '还有一点：我们已经聊了这么久还没定，讨论本身就在花钱。这个成本没人记进账。',
    ],
  },
  {
    alias: '小李 千问',
    model: 'qwen-max',
    persona: '数据派',
    stance: 'agree',
    conclusion: '有条件同意：先补齐基线与 90 天留存打点，30 天后按真实数据复盘，不达标回滚。',
    points: [
      '我两边都不站，先要三个数：现在的年付转化率、月付的预期转化率、以及 90 天留存。这三个数一个都没有，现在的讨论就是在拼直觉。',
      '"十几倍样本"这个说法我认同方向，但样本不等于收入。月付 ¥29 一年是 ¥348，比 ¥199 高 75%，续费撑过 7 个月才打平——这 7 个月才是争议核心，前面两位都没给。',
      '我的结论是有条件的：可以改，但必须同时埋好 90 天留存和第二次续费的打点，30 天后拿真实数据复盘，不达标就回滚。',
      '补一句常被忽略的：样本不够时，前两周的数据会骗人。别拿两周去下结论。',
    ],
  },
  {
    alias: '老张 DeepSeek',
    model: 'deepseek-chat',
    persona: '稳妥派',
    stance: 'disagree',
    conclusion: '不建议现在全量改；先灰度一小部分新用户跑满一个续费周期，再决定。',
    points: [
      '我反对现在就改。年付把回款提前了 12 个月，改月付等于把已经拿到的钱摊回 12 次小额收款，中间任何一次续费失败都得重算。最坏情况不是没效果，是现金流和信任一起出问题。',
      '反驳我听懂了，但它的前提是转化率一定升。我只问一句：如果三个月后月付留存比年付低 8 个点，我们退得回去吗？退不回去的方案不该一把梭。',
      '我的方案是灰度：先拿 10% 的新用户走月付，跑满两个续费周期再决定。最坏情况只损失那 10%。',
      '还有一点没人提：价格改动最容易伤到已经付了 ¥199 的老用户。没有补偿方案就不该上。',
    ],
  },
];

/* ══════════════════════════════════════════════════════════════════════════
   输出格式化（中文宽度对齐，干净不带 emoji）
   ══════════════════════════════════════════════════════════════════════════ */

function dispWidth(s) {
  let w = 0;
  for (const ch of String(s)) {
    const c = ch.codePointAt(0);
    const wide =
      (c >= 0x2e80 && c <= 0xa4cf) ||
      (c >= 0xac00 && c <= 0xd7a3) ||
      (c >= 0xf900 && c <= 0xfaff) ||
      (c >= 0xfe30 && c <= 0xfe6f) ||
      (c >= 0xff00 && c <= 0xff60) ||
      (c >= 0xffe0 && c <= 0xffe6);
    w += wide ? 2 : 1;
  }
  return w;
}

function pad(s, n) {
  const str = String(s ?? '');
  return str + ' '.repeat(Math.max(0, n - dispWidth(str)));
}

/**
 * 按宽度折行。中文按字折，拉丁字母/数字/型号（deepseek-chat、¥199）整词不拆，
 * 否则会出现 "DeepSee / k" 这种截图很难看的情况。
 */
function wrapLines(text, width = 76) {
  const src = String(text ?? '').replace(/\s+/g, ' ').trim();
  if (!src) return [];
  const tokens = src.match(/[A-Za-z0-9_@#¥$%.,:/'"()\-+]+|\s+|./gu) || [];
  const lines = [];
  let line = '';
  let w = 0;
  for (const tok of tokens) {
    const tw = dispWidth(tok);
    if (w + tw > width && line.trim()) {
      lines.push(line.replace(/\s+$/, ''));
      line = '';
      w = 0;
      if (/^\s+$/.test(tok)) continue; // 折行后丢掉行首空格
    }
    line += tok;
    w += tw;
  }
  if (line.trim()) lines.push(line.replace(/\s+$/, ''));
  return lines;
}

function wrap(text, width = 76, indent = 4) {
  const padStr = ' '.repeat(indent);
  return wrapLines(text, width)
    .map((l) => padStr + l)
    .join('\n');
}

const LINE = '─'.repeat(72);
const DLINE = '═'.repeat(72);

function heading(title) {
  console.log('');
  console.log(DLINE);
  console.log('  ' + title);
  console.log(DLINE);
  console.log('');
}

function num(n) {
  if (n === undefined || n === null) return '-';
  const v = Number(n);
  return Number.isFinite(v) ? v.toLocaleString('en-US') : String(n);
}

function hhmmss(ts) {
  const d = ts ? new Date(ts) : new Date();
  if (Number.isNaN(d.getTime())) return '--:--:--';
  const p = (x) => String(x).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/* ══════════════════════════════════════════════════════════════════════════
   启动进程内 server（对导出形态做容错）
   ──────────────────────────────────────────────────────────────────────────
   队友写的 server.mjs 可能导出 createServer(opts) / createServer(port) /
   直接导出 http.Server / 甚至 import 即监听。这里统一适配成 { port, close }。
   ══════════════════════════════════════════════════════════════════════════ */

async function bootServer() {
  const serverPath = join(HERE, 'server.mjs');
  if (!existsSync(serverPath)) {
    throw new Error(
      `找不到 ${serverPath}\n` +
        '  demo 需要同目录下的 server.mjs 导出 createServer()。\n' +
        '  如果 server 还在写，请稍后重试：node mvp/demo.mjs',
    );
  }

  const mod = await import(pathToFileURL(serverPath).href);
  // 兼容队友可能采用的各种导出名：createApp / createServer / startServer / default
  const factory =
    mod.createApp ||
    mod.createServer ||
    (mod.default && (mod.default.createApp || mod.default.createServer)) ||
    mod.startServer ||
    (typeof mod.default === 'function' ? mod.default : null);

  if (typeof factory !== 'function') {
    // 兜底：模块 import 时可能已经自己监听了 8788
    const fallback = await tryFixedPort(8788);
    if (fallback) return fallback;
    throw new Error('server.mjs 没有导出 createServer()，也探测不到已在监听的端口');
  }

  // 依次尝试几种常见签名
  const attempts = [
    () => factory({ port: 0, host: '127.0.0.1' }),
    () => factory({ port: 0 }),
    () => factory(0),
    () => factory(),
  ];

  let lastErr = null;
  for (const attempt of attempts) {
    let raw;
    try {
      raw = await attempt();
    } catch (err) {
      lastErr = err;
      continue;
    }
    const normalized = await normalizeHandle(await raw);
    if (normalized) return normalized;
  }
  throw lastErr || new Error('createServer() 的返回值无法识别出监听端口');
}

function normalizeHandle(handle) {
  return new Promise((resolve) => {
    if (!handle) return resolve(null);

    // 直接给了端口号
    if (typeof handle === 'number' && handle > 0) {
      return resolve({ port: handle, close: async () => {} });
    }
    // 给了 URL 字符串
    if (typeof handle === 'string') {
      try {
        const u = new URL(handle);
        return resolve({ port: Number(u.port), close: async () => {} });
      } catch {
        return resolve(null);
      }
    }
    // { port, close }
    if (typeof handle.port === 'number' && handle.port > 0) {
      return resolve({
        port: handle.port,
        close: async () => {
          if (typeof handle.close === 'function') {
            try {
              await handle.close();
            } catch {
              /* 忽略关闭错误 */
            }
          }
        },
      });
    }
    if (typeof handle.url === 'string') {
      try {
        const u = new URL(handle.url);
        return resolve({
          port: Number(u.port),
          close: async () => {
            if (typeof handle.close === 'function') await handle.close();
          },
        });
      } catch {
        /* 继续 */
      }
    }

    // 形如 createApp() 返回的封装：{ server, listen(port), close() }
    // listen 是 Promise 风格并返回 { url, port }，优先走这条，因为它还会清掉房间定时器
    if (typeof handle.listen === 'function' && typeof handle.close === 'function') {
      Promise.resolve()
        .then(() => handle.listen(0, '127.0.0.1'))
        .then((r) => {
          if (r && typeof r.port === 'number' && r.port > 0) {
            return resolve({ port: r.port, close: () => handle.close() });
          }
          resolve(null); // 不是这类封装，交给下面的 http.Server 分支
        })
        .catch(() => resolve(null));
      return;
    }

    // http.Server（或包了一层 { server }）
    const srv =
      handle.server && typeof handle.server.listen === 'function'
        ? handle.server
        : typeof handle.listen === 'function'
          ? handle
          : null;

    if (!srv) return resolve(null);

    const closeFn = async () => {
      try {
        await new Promise((res) => srv.close(res));
      } catch {
        /* 忽略 */
      }
    };

    if (srv.listening && srv.address()) {
      return resolve({ port: srv.address().port, close: closeFn });
    }
    srv.once('error', () => resolve(null));
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      resolve({ port: addr && addr.port ? addr.port : 0, close: closeFn });
    });
  });
}

async function tryFixedPort(port) {
  try {
    const res = await fetch(`http://127.0.0.1:${port}/api/rooms`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ topic: '__probe__', probe: true }),
      signal: AbortSignal.timeout(2000),
    });
    // 任何结构化响应都说明服务在
    if (res.status >= 200 && res.status < 500) return { port, close: async () => {} };
  } catch {
    /* 没有服务 */
  }
  return null;
}

/* ══════════════════════════════════════════════════════════════════════════
   主流程
   ══════════════════════════════════════════════════════════════════════════ */

function printPreamble() {
  console.log('');
  console.log(DLINE);
  console.log('  agchat · 纯 agent 讨论室 · 端到端演示');
  console.log('  人只建房间设预算，讨论和收敛全部由 agent 自己完成');
  console.log(DLINE);
  console.log('');
  console.log('  议题     ' + ROOM_SPEC.topic);
  console.log('  规则     ' + ROOM_SPEC.rules);
  console.log(
    '  预算     ' +
      num(ROOM_SPEC.tokenBudget) +
      ' tokens · 最多 ' +
      ROOM_SPEC.maxRounds +
      ' 轮 · 最多 ' +
      ROOM_SPEC.maxAgents +
      ' 个 agent',
  );
  const info = byokInfo();
  console.log(
    '  发言来源 ' +
      (hasBYOK() ? `BYOK 真模型（${info.model} @ ${info.baseUrl}）` : '内置讨论人格（未配置 BYOK，离线确定性）'),
  );
  console.log('');
}

async function main() {
  const t0 = Date.now();
  printPreamble();

  /* ── 1. 起服务 ──────────────────────────────────────────────────────── */
  console.log('[1/4] 启动进程内 server');
  const server = await bootServer();
  SERVER = server;
  const baseUrl = `http://127.0.0.1:${server.port}/api`;
  console.log('      已就绪  ' + baseUrl);
  console.log('');

  /* ── 2. 建房 ────────────────────────────────────────────────────────── */
  console.log('[2/4] 建立讨论室');
  const room = await createRoom(ROOM_SPEC, { baseUrl });
  const roomId = room.roomId;
  if (!roomId) throw new Error('建房失败：响应里没有 roomId');

  console.log('      roomId    ' + roomId);
  console.log('      roomUrl   ' + (room.roomUrl || `http://127.0.0.1:${server.port}/room/${roomId}`));
  console.log('      password  ' + room.password);
  console.log('      adminKey  ' + String(room.adminKey || '').slice(0, 12) + '…');
  if (room.estimate) {
    console.log(
      '      预估      ' +
        num(room.estimate.tokens) +
        ' tokens（最坏 ' +
        num(room.estimate.worstCase) +
        '）',
    );
  }
  console.log('');

  /* ── 3. agent 入驻 ──────────────────────────────────────────────────── */
  console.log('[3/4] ' + TEAM.length + ' 个 agent 进场（各带一个不同人格）');
  const warned = new Set(); // 降级提示每人只说一次
  const agents = TEAM.map((spec) => {
    const agent = new RoomAgent({
      baseUrl,
      roomId,
      password: room.password,
      alias: spec.alias,
      model: spec.model,
      pollWait: 5,
      decide: persona(spec.persona, {
        points: spec.points,
        stance: spec.stance,
        conclusion: spec.conclusion,
        // 模型侧出问题就退回内置人格，绝不让整场崩掉。同一个 agent 只提示一次，避免刷屏。
        onModelError: (err) => {
          if (warned.has(spec.alias)) return;
          warned.add(spec.alias);
          console.log(`      降级  ${spec.alias} 模型调用失败，本场改用内置人格：${err.message}`);
        },
      }),
    });
    return { spec, agent };
  });

  await Promise.all(
    agents.map(async (a) => {
      const data = await a.agent.join();
      const tag = pad(a.spec.alias, 16);
      console.log(`      ${tag}${pad(a.spec.model, 18)}${pad(a.spec.persona, 10)}${a.agent.agentId}`);
      if (data.brief && data.brief.yourQuota) {
        // 每人配额只打印一次，避免刷屏
      }
    }),
  );
  console.log('');

  /* ── 4. 开跑 ────────────────────────────────────────────────────────── */
  console.log('[4/4] 讨论进行中（长轮询驱动，无人类介入）');
  console.log('');

  const DEADLINE_MS = 150_000;
  const deadline = Date.now() + DEADLINE_MS;

  // 后台盯结果：一旦 ended，就把所有 agent 的循环停掉
  let finalResult = null;
  const settler = (async () => {
    while (Date.now() < deadline) {
      try {
        const r = await getResult({ baseUrl, roomId });
        if (r && r.state === 'ended') {
          finalResult = r;
          agents.forEach((a) => (a.agent.stopped = true));
          return r;
        }
      } catch {
        /* 结果页还没准备好，继续等 */
      }
      await sleep(400);
    }
    return null;
  })();

  const runs = agents.map(async ({ agent }) => {
    try {
      return await agent.run();
    } catch (err) {
      if (agent.state === 'ended') return { state: 'ended' };
      return { state: agent.state, error: err.message };
    }
  });

  await Promise.all(runs);
  if (!finalResult) finalResult = await settler;
  agents.forEach((a) => (a.agent.stopped = true));

  /* ── 收尾：拿结果页 ─────────────────────────────────────────────────── */
  if (!finalResult) {
    for (let i = 0; i < 20 && !finalResult; i++) {
      try {
        const r = await getResult({ baseUrl, roomId });
        if (r && r.state === 'ended') finalResult = r;
      } catch {
        /* 还没好 */
      }
      if (!finalResult) await sleep(300);
    }
  }

  /* ══════════════════════════════════════════════════════════════════════
     输出
     ══════════════════════════════════════════════════════════════════════ */

  const transcript = collectTranscript(agents);
  renderTranscript(transcript);

  const result = finalResult || {};
  renderConclusion(result);
  renderSummary(result);
  renderCost(result, agents, transcript, Date.now() - t0);

  /* ── 断言 ───────────────────────────────────────────────────────────── */
  const sections = pickSections(result);
  const conclusionText = pickConclusionText(result);
  const costTokens = pickCostTokens(result, transcript);

  const failures = [];
  if (result.state !== 'ended') failures.push(`state 应为 "ended"，实际 "${result.state}"`);
  if (!conclusionText) failures.push('conclusion.text 为空');
  if (!sections.length) failures.push('summary.sections 为空');
  if (!(Number(costTokens) > 0)) failures.push('cost.estTokens 应 > 0');
  if (!transcript.length) failures.push('一条消息都没有，讨论没跑起来');

  console.log('');
  if (failures.length) {
    console.log('✗ 断言失败：');
    for (const f of failures) console.log('   - ' + f);
    console.log('');
    console.log('  诊断：');
    for (const { spec, agent } of agents) {
      console.log(
        `   ${pad(spec.alias, 16)} state=${pad(agent.state, 10)} speaks=${agent.stats.speaks} ` +
          `polls=${agent.stats.polls} conflicts=${agent.stats.conflicts}`,
      );
    }
    console.log('');
    return { ok: false, result, transcript };
  }

  console.log('✓ 全部断言通过：state=ended · conclusion 非空 · summary.sections 非空 · cost>0');
  console.log('');
  return { ok: true, result, transcript };
}

/* ══════════════════════════════════════════════════════════════════════════
   结果收集与渲染
   ══════════════════════════════════════════════════════════════════════════ */

function collectTranscript(agents) {
  const bySeq = new Map();
  for (const { agent } of agents) {
    for (const m of agent.context) {
      if (m && Number.isFinite(m.seq) && !bySeq.has(m.seq)) bySeq.set(m.seq, m);
    }
  }
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

function renderTranscript(messages) {
  heading('讨论记录');

  if (!messages.length) {
    console.log('  （没有捕获到任何消息）');
    return;
  }

  let currentRound = null;
  for (const m of messages) {
    const round = Number.isFinite(m.round) ? m.round : null;
    if (round !== null && round !== currentRound) {
      if (currentRound !== null) console.log('');
      console.log('  ── 第 ' + round + ' 轮 ' + '─'.repeat(52));
      console.log('');
      currentRound = round;
    }

    const who = pad(m.alias || m.agentId || '?', 16);
    const meta = hhmmss(m.ts) + (m.replyTo ? `   回复 #${m.replyTo}` : '');
    console.log(`  ${who}${meta}`);

    const body = String(m.text || '').trim();
    if (m.stance) {
      const label = { agree: '同意', disagree: '反对', abstain: '弃权' }[m.stance] || m.stance;
      const lines = wrapLines(`【立场：${label}】${body}`, 66);
      for (const l of lines) console.log('      ' + l);
    } else {
      for (const l of wrapLines(body, 66)) console.log('      ' + l);
    }
    console.log('');
  }
}

function pickConclusionText(result) {
  const c = result && result.conclusion;
  if (!c) return '';
  if (typeof c === 'string') return c;
  return String(c.text || c.conclusion || '').trim();
}

function pickSections(result) {
  const s = result && result.summary;
  if (!s) return [];
  const raw = Array.isArray(s.sections) ? s.sections : Array.isArray(s) ? s : [];
  return raw.filter(Boolean);
}

function pickCostTokens(result, transcript) {
  const cost = (result && result.cost) || {};
  const v = cost.estTokens ?? cost.tokens ?? cost.totalTokens ?? cost.estimated;
  if (Number.isFinite(Number(v)) && Number(v) > 0) return Number(v);
  // 兜底：按正文字数粗估（1 汉字 ≈ 1.5 token）
  const chars = transcript.reduce((n, m) => n + String(m.text || '').length, 0);
  return chars > 0 ? Math.round(chars * 1.5) : 0;
}

function renderConclusion(result) {
  heading('结论');

  // 服务端拼出来的句子偶尔会出现 "。；" 这种叠标点，展示层做一次收敛，不改含义
  const text = (pickConclusionText(result) || '（未产出结论）').replace(/。；/g, '；');
  const c = (result && result.conclusion) || {};

  // 结论用大字：上下留白 + 4 格缩进
  console.log('');
  for (const line of wrapLines(text, 66)) console.log('    ' + line);
  console.log('');

  const votes = c.votes;
  const parts = [];
  if (votes && typeof votes === 'object' && !Array.isArray(votes)) {
    for (const [k, v] of Object.entries(votes)) parts.push(`${kN(k)} ${num(v)}`);
  } else if (Array.isArray(votes)) {
    const tally = { agree: 0, disagree: 0, abstain: 0 };
    for (const v of votes) {
      const s = typeof v === 'string' ? v : v && v.stance;
      if (s && tally[s] !== undefined) tally[s]++;
    }
    for (const [k, v] of Object.entries(tally)) if (v) parts.push(`${kN(k)} ${v}`);
  }
  if (parts.length) console.log('  表决    ' + parts.join(' · '));
  if (c.agreement !== undefined && c.agreement !== null) {
    const AGREEMENT = { unanimous: '全体一致', majority: '多数通过', split: '未形成多数' };
    const a = Number(c.agreement);
    const label = AGREEMENT[c.agreement] || (Number.isFinite(a) ? a.toFixed(2) : String(c.agreement));
    console.log('  判定    ' + label);
  }
  console.log('');
}

function kN(k) {
  return { agree: '同意', disagree: '反对', abstain: '弃权' }[k] || k;
}

function renderSummary(result) {
  heading('讨论简要');

  const sections = pickSections(result) || [];

  if (!sections.length) {
    console.log('  （无分节内容）');
    console.log('');
    return;
  }

  for (const sec of sections) {
    if (typeof sec === 'string') {
      for (const l of wrapLines(sec, 68)) console.log('  ' + l);
      console.log('');
      continue;
    }
    // 服务端用的是 { h, p }（heading + paragraph），也兼容 { title, items }
    const title = sec.h || sec.title || sec.heading || sec.name || sec.key || '要点';

    // "结论"上面已经用大字单独印过一次了，这里不再重复
    if (/^结论$/.test(title)) continue;

    // "各方立场"这段原文是一整坨拼接文本，读起来很吵；
    // 用 result.positions 渲染成紧凑的三行更好看，信息也不丢。
    if (/立场/.test(title) && Array.isArray(result.positions) && result.positions.length) {
      console.log('  [' + title + ']');
      renderPositionsTable(result.positions);
      console.log('');
      continue;
    }

    console.log('  [' + title + ']');

    const para = sec.p ?? sec.content ?? sec.body ?? sec.text ?? null;
    const list =
      (Array.isArray(sec.items) && sec.items) ||
      (Array.isArray(sec.points) && sec.points) ||
      (Array.isArray(sec.bullets) && sec.bullets) ||
      (para ? [para] : []);

    for (const item of list) {
      const s = typeof item === 'string' ? item : item && (item.text || item.content || JSON.stringify(item));
      const lines = wrapLines('- ' + s, 68);
      for (const line of lines) console.log('    ' + line);
    }
    console.log('');
  }
}

/** 立场矩阵：三个人三行，第一行关键论点，比拼成一坨好读 */
function renderPositionsTable(positions) {
  const STANCE = { agree: '同意', disagree: '反对', abstain: '弃权' };
  for (const p of positions) {
    const who = `${p.alias || '?'}（${p.model || 'agent'}）`;
    const stance = STANCE[p.stance] || '未表态';
    console.log('    ' + pad(who, 30) + stance);
    const kp = Array.isArray(p.keyPoints) ? p.keyPoints[0] : null;
    if (kp) for (const line of wrapLines(kp, 60)) console.log('      ' + line);
  }
}

function renderCost(result, agents, transcript, elapsedMs) {
  heading('成本与运行');

  const cost = (result && result.cost) || {};
  const tokens = pickCostTokens(result, transcript);
  const perAgent = Math.round(tokens / Math.max(1, agents.length));

  const rows = [
    ['估算 tokens', num(tokens)],
    ['人均 tokens', num(perAgent)],
    ['消息条数', num(transcript.length)],
    ['参与 agent', num(agents.length)],
    ['房间状态', String(result.state || '-')],
    ['总耗时', (elapsedMs / 1000).toFixed(1) + ' s'],
  ];

  const COST_LABEL = { rounds: '实际轮数', minutes: '用时(分钟)' };
  // estTokens / messages 上面已有对应行，跳过避免重复
  const extraKeys = Object.keys(cost).filter(
    (k) => !['estTokens', 'tokens', 'totalTokens', 'estimated', 'messages'].includes(k),
  );
  for (const k of extraKeys) {
    const v = cost[k];
    if (typeof v === 'number' || typeof v === 'string') {
      rows.push([COST_LABEL[k] || k, typeof v === 'number' ? num(v) : v]);
    }
  }

  for (const [k, v] of rows) console.log('  ' + pad(k, 18) + v);

  console.log('');
  console.log('  各 agent 出场统计');
  for (const { spec, agent } of agents) {
    console.log(
      '    ' +
        pad(spec.alias, 16) +
        pad(spec.persona, 10) +
        `发言 ${agent.stats.speaks} 次   长轮询 ${agent.stats.polls} 次   409 和解 ${agent.stats.conflicts} 次`,
    );
  }
  console.log('');
  console.log(LINE);
  console.log('');
}

/* ══════════════════════════════════════════════════════════════════════════
   入口
   ══════════════════════════════════════════════════════════════════════════ */

main()
  .then((r) => {
    if (!r.ok) {
      console.error('demo 失败：见上方诊断。');
      process.exitCode = 1;
    } else {
      console.log('demo 完成。');
      process.exitCode = 0;
    }
  })
  .catch((err) => {
    console.error('');
    console.error('demo 执行失败：' + (err && err.stack ? err.stack : err));
    console.error('');
    process.exitCode = 1;
  })
  .finally(async () => {
    // 关掉进程内 server，让 node 能干净退出
    if (SERVER && typeof SERVER.close === 'function') {
      try {
        await SERVER.close();
      } catch {
        /* 忽略关闭错误 */
      }
    }
  });
