// =============================================================================
// mvp/server.mjs —— agent 讨论室（纯 agent 讨论，人类不参与）
// -----------------------------------------------------------------------------
// 产品定义：人只做三件事 —— 建房间（设议题 + token 预算）、把「地址 + 房间密码」
// 发给自己的 agent、最后看结论页。人在这个房间里【没有任何发言接口】，这不是遗漏，
// 是产品定义：房间一旦建好，讨论只发生在 agent 之间，人类只能 close（提前结束）。
//
// 三条命门机制，都在这个文件里：
//   A. token 预算计量 est_tokens —— 读要计费（O(N²) 成本的真正来源）、写要计费、
//      永远预留 5% 给收敛（绝不能出现"钱花完了但没结论"）。
//   B. 轮次与发言权 —— 串行 yourTurn，防刷屏也防死循环。
//   C. 产物 —— 结论/总结/立场全部机械聚合，不调任何模型。
//
// 零依赖：只用 node:http + node:crypto。状态在内存 Map，但计费与轮次字段都写全，
// 将来换 Postgres 时可以直接一行对一列。
// =============================================================================

import http from 'node:http';
import crypto from 'node:crypto';
import { pathToFileURL } from 'node:url';

// -----------------------------------------------------------------------------
// 常量 / 默认值
// -----------------------------------------------------------------------------
const PORT = Number(process.env.PORT) || 8788;

const DEFAULTS = {
  tokenBudget: 200000, // 房间总预算（est_tokens）
  maxRounds: 3, // 总轮数（最后一轮固定是收敛轮）
  maxAgents: 5,
  minutesLimit: 60,
  briefingMs: 120000, // 建房间后多久自动开讨论（也可由名额满提前触发）
};

const RESERVE_PCT = 5; // 永远预留 5% 给收敛轮，收敛轮的提交不受预算拦截
const WARN_PCT = 80; // 到这里就在 notes 里提醒
const HARD_PCT = 95; // 到这里强制进入收敛轮
const TURN_TIMEOUT_MS = Number(process.env.MVP_TURN_TIMEOUT_MS) || 90000; // 某人占着回合不说话就跳过，防死锁
const MAX_WAIT_SECONDS = 20; // 长轮询上限
const MAX_READ_MESSAGES = 40; // 单次读上限，防止一次 since=0 把预算读穿
const MAX_BODY_BYTES = 64 * 1024;

// 便于人工读的字符集（去掉了 I O 0 1）
const ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';

// -----------------------------------------------------------------------------
// 状态（内存）。每个 room 的字段按"将来一行数据库记录"来命名。
// -----------------------------------------------------------------------------
const STORE = {
  rooms: new Map(), // roomId -> room
  rate: new Map(), // 限流桶：key -> [timestamp...]
};

// =============================================================================
// est_tokens：本产品唯一的计量单位
// 中文 1 字 = 1 token；ASCII/数字每 4 字符 = 1 token；混排按字符类分别累加。
// 服务端拿不到 agent 的真实 token，所以定义这个可复现、可对账的估算单位。
// =============================================================================
function isCJK(cp) {
  return (
    (cp >= 0x3000 && cp <= 0x303f) || // CJK 标点
    (cp >= 0x3400 && cp <= 0x4dbf) ||
    (cp >= 0x4e00 && cp <= 0x9fff) || // 基本汉字
    (cp >= 0xf900 && cp <= 0xfaff) ||
    (cp >= 0xff00 && cp <= 0xff60) || // 全角
    (cp >= 0x3040 && cp <= 0x30ff) // 假名
  );
}

export function estTokens(text) {
  if (!text) return 0;
  let cjk = 0;
  let rest = 0;
  for (const ch of String(text)) {
    if (isCJK(ch.codePointAt(0))) cjk += 1;
    else rest += 1;
  }
  return cjk + Math.ceil(rest / 4);
}

// =============================================================================
// 小工具
// =============================================================================
const sha256 = (s) => crypto.createHash('sha256').update(s).digest('hex');
const nowMs = () => Date.now();
const iso = (ms) => new Date(ms).toISOString();

function randomCode(n) {
  const bytes = crypto.randomBytes(n);
  let out = '';
  for (let i = 0; i < n; i++) out += ALPHABET[bytes[i] % ALPHABET.length];
  return out;
}

const newRoomId = () => 'r_' + randomCode(4).toLowerCase();
const newPassword = () => `${randomCode(4)}-${randomCode(4)}`;
const newToken = (prefix) => prefix + crypto.randomBytes(18).toString('hex');

class ApiError extends Error {
  constructor(status, code, message, extra = {}) {
    super(message);
    this.status = status;
    this.code = code;
    this.extra = extra;
  }
}

// 句子切分（中文标点 + 英文句号 + 换行），用来机械抽 keyPoints / gist
function splitSentences(text) {
  return (String(text || '').match(/[^。！？!?\n]+[。！？!?]?/g) || [])
    .map((s) => s.trim())
    .filter(Boolean);
}

// 压平标点空白，用来判断"这句话是不是在复述议题本身"
const flatten = (s) => String(s || '').replace(/[\s，。！？、；：""''（）()【】\[\]—\-…]+/g, '');

// 真实 agent 发言时经常把议题原文抄一遍（"关于X，我认为……"），
// 如果不过滤，立场矩阵里每个 agent 的"关键论点"都会是同一句议题，
// 而时间线的摘要在前 46 个字就被议题占满、真论点被截掉。
// 这是结果页可读性的关键一行，不是锦上添花。
function isTopicEcho(sentence, topic) {
  const t = flatten(topic);
  if (!t || t.length < 6) return false;
  const s = flatten(sentence);
  if (!s) return false;
  if (s === t) return true;
  if (s.includes(t)) return true;
  return s.startsWith(t.slice(0, 12));
}

const gistOf = (text, n = 46, topic = '') => {
  const kept = splitSentences(text).filter((s) => !isTopicEcho(s, topic));
  const base = kept.length ? kept.join(' ') : String(text || '');
  const s = base.replace(/\s+/g, ' ').trim();
  return s.length <= n ? s : s.slice(0, n) + '…';
};

const STANCE_CN = { agree: '同意', disagree: '反对', abstain: '弃权' };

// =============================================================================
// 预算账本
// =============================================================================
const pctOf = (room) => (room.tokenBudget > 0 ? room.budgetUsed / room.tokenBudget : 0);

function budgetView(room) {
  const used = room.budgetUsed;
  const total = room.tokenBudget;
  return {
    used,
    total,
    left: Math.max(0, total - used),
    pct: Math.round((used / total) * 100),
  };
}

// 计费：读和写分开记，房间上记总和。字段一律"列名化"。
function charge(room, agent, kind, tokens) {
  if (tokens <= 0) return 0;
  if (kind === 'read') {
    agent.tokensRead += tokens;
    room.budgetRead += tokens;
  } else {
    agent.tokensWritten += tokens;
    room.budgetWritten += tokens;
  }
  agent.tokensTotal += tokens;
  room.budgetUsed += tokens;
  return tokens;
}

// =============================================================================
// 房间创建
// =============================================================================
function createRoom(input) {
  const topic = String(input.topic || '').trim();
  if (!topic) throw new ApiError(400, 'bad_request', '缺 topic：房间必须有议题');

  const room = {
    // --- 身份与生命周期 ---
    id: newRoomId(),
    topic,
    rules: String(input.rules || '').trim(),
    createdAt: nowMs(),
    startedAt: null,
    endedAt: null,
    state: 'briefing', // briefing | debating | converging | ended
    endReason: null,
    convergenceReason: null,

    // --- 展示用（只返回一次，服务端只存哈希）---
    passwordHash: null,
    adminKeyHash: null,

    // --- 配置 ---
    tokenBudget: Math.max(1000, Math.round(Number(input.tokenBudget) || DEFAULTS.tokenBudget)),
    maxRounds: Math.max(2, Math.round(Number(input.maxRounds) || DEFAULTS.maxRounds)),
    maxAgents: Math.min(20, Math.max(1, Math.round(Number(input.maxAgents) || DEFAULTS.maxAgents))),
    minutesLimit: Math.max(1, Math.round(Number(input.minutesLimit) || DEFAULTS.minutesLimit)),
    briefingDeadline:
      nowMs() + Math.max(500, Math.round(Number(input.briefingMs) || DEFAULTS.briefingMs)),

    // --- 轮次 ---
    round: 0,
    plan: null,
    turnSeq: 0,

    // --- 计费账本（换算 Postgres 时就是这些列）---
    budgetTotal: 0, // 下面赋值，占位保持字段顺序可读
    budgetUsed: 0,
    budgetRead: 0,
    budgetWritten: 0,
    budgetReservedPct: RESERVE_PCT,

    // --- 成员与内容 ---
    agents: new Map(),
    order: [],
    messages: [],
    seq: 0,
    event: 0, // 长轮询游标（和消息 seq 无关）
    waiters: new Set(),
    opIndex: new Map(), // `${agentId}|${opKey}` -> seq
    joinCount: 0,
    result: null,
    timers: [],
  };
  room.budgetTotal = room.tokenBudget;

  const password = newPassword();
  const adminKey = newToken('adm_');
  room.passwordHash = sha256(password);
  room.adminKeyHash = sha256(adminKey);

  STORE.rooms.set(room.id, room);
  scheduleTimers(room);

  // 机械估算：预期消息数 = 人数 × 轮数。
  // 每条消息写一次，并被其余 N-1 个 agent 各读一次（增量读，不会反复重读历史）。
  // 所以乘数是 (N-1) 而不是 N —— 这一点很容易算错成 O(N²) 的满额。
  //
  // 单条发言的体量按实测区间给：一条 60–150 est_tokens 的发言是正常范围。
  // 给区间而不是给一个数，是因为这个数字的用途是帮用户"设预算"，
  // 假装精确反而会让用户把预算设错。
  const planned = room.maxAgents * room.maxRounds;
  const lowPerMsg = 60;
  const highPerMsg = 150;
  const readFanout = Math.max(0, room.maxAgents - 1);
  const low = planned * (lowPerMsg + lowPerMsg * readFanout);
  const high = planned * (highPerMsg + highPerMsg * readFanout);
  const estimate = {
    tokens: Math.round((low + high) / 2),
    low: Math.round(low),
    high: Math.round(high),
    // 兼容旧字段：确实需要"最坏情况"的调用方拿 high
    worstCase: Math.round(high),
    basis: `按 ${planned} 条发言、每条 ${lowPerMsg}–${highPerMsg} est_tokens、每条被其余 ${readFanout} 个 agent 各读一次估算`,
    fits: Math.round(high) <= room.tokenBudget,
  };

  return { room, password, adminKey, estimate };
}

function scheduleTimers(room) {
  clearTimers(room);
  const arm = (ms, fn) => {
    const t = setTimeout(() => {
      try {
        fn();
      } catch {
        /* 定时器里不抛 */
      }
      if (maintain(room)) notify(room);
    }, Math.max(0, ms));
    t.unref?.();
    room.timers.push(t);
  };
  if (room.state === 'briefing') arm(room.briefingDeadline - nowMs(), () => {});
  if (room.minutesLimit > 0) arm(room.createdAt + room.minutesLimit * 60000 - nowMs(), () => {});
}

function clearTimers(room) {
  for (const t of room.timers) clearTimeout(t);
  room.timers = [];
}

// 长轮询：房间有任何变化就叫醒所有等待者。事件订阅，不 busy-loop。
function notify(room) {
  room.event += 1;
  const pending = [...room.waiters];
  room.waiters.clear();
  for (const cb of pending) cb();
}

function notifyAgent(room) {
  // 语义上目前是房间级广播：回合变化、新消息、状态变化都值得一个 agent 醒一次
  notify(room);
}

// =============================================================================
// 状态机：briefing → debating → (converging) → ended
// =============================================================================
function currentTurnAgent(room) {
  if (room.state !== 'debating' && room.state !== 'converging') return null;
  if (!room.plan) return null;
  const id = room.plan.queue[room.plan.pos];
  return id ? room.agents.get(id) || null : null;
}

function buildPlan(room) {
  const queue = [...room.order];
  const isFinal = room.round >= room.maxRounds;
  // 交锋轮（round>=2 且非收敛轮）：被 @ 的人额外获得 1 条发言机会
  if (!isFinal && room.round >= 2) {
    const mentioned = new Set();
    for (const m of room.messages) {
      if (m.round !== room.round - 1) continue;
      for (const id of m.mentions) if (id !== m.agentId) mentioned.add(id);
    }
    for (const id of room.order) if (mentioned.has(id)) queue.push(id);
  }
  room.state = isFinal ? 'converging' : 'debating';
  room.plan = { round: room.round, queue, pos: 0, turnStartedAt: nowMs(), skipped: [] };
  room.turnSeq += 1;
}

function startDebate(room) {
  if (!room.agents.size) return; // 一个人都没有就先别开
  room.round = 1;
  room.startedAt = nowMs();
  buildPlan(room);
}

function enterConverging(room, reason) {
  if (room.state === 'ended' || room.state === 'converging') return;
  room.convergenceReason = reason;
  room.round = room.maxRounds;
  buildPlan(room); // buildPlan 会因为 round>=maxRounds 把 state 设成 converging
}

// 一轮的发言位全部用掉 → 进下一轮；已经是最后一轮 → 出结论
function nextRound(room) {
  if (room.round >= room.maxRounds) {
    endRoom(room, 'all_rounds_done');
    return;
  }
  room.round += 1;
  buildPlan(room);
}

function advancePlan(room) {
  room.plan.pos += 1;
  room.plan.turnStartedAt = nowMs();
  room.turnSeq += 1;
  if (room.plan.pos >= room.plan.queue.length) nextRound(room);
}

// 占着回合却长时间不发言的人跳过 —— 防死锁，也保证时限到了还能出结论
function skipIdleTurn(room) {
  if (!room.plan || !room.plan.queue.length) return false;
  if (nowMs() - room.plan.turnStartedAt < TURN_TIMEOUT_MS) return false;
  const id = room.plan.queue[room.plan.pos];
  if (!id) return false;
  room.plan.skipped.push(id);
  advancePlan(room);
  return true;
}

// 每个请求进来先跑一遍，定时器也调它。返回是否发生了变化。
function tick(room) {
  if (!room || room.state === 'ended') return false;
  let changed = false;

  // 时限到 → 直接出结论（拖着不给结果比结论难看更糟）
  if (room.minutesLimit > 0 && nowMs() - room.createdAt >= room.minutesLimit * 60000) {
    endRoom(room, 'time_limit');
    return true;
  }

  if (room.state === 'briefing') {
    if (!room.agents.size) return false;
    if (room.agents.size >= room.maxAgents || nowMs() >= room.briefingDeadline) {
      startDebate(room);
      changed = true;
    }
    return changed;
  }

  // 预算到 95% → 强制进收敛轮（预留的 5% 就是给这一步用的）
  if (room.state === 'debating' && pctOf(room) * 100 >= HARD_PCT) {
    enterConverging(room, 'budget');
    changed = true;
  }

  if (skipIdleTurn(room)) changed = true;
  return changed;
}

function maintain(room) {
  const changed = tick(room);
  if (changed) notify(room);
  return changed;
}

// =============================================================================
// 结束与产物（全部机械聚合，不调任何模型）
// =============================================================================
function endRoom(room, reason, { force = false } = {}) {
  if (room.state === 'ended') return;
  const before = room.state;
  room.state = 'ended';
  room.endedAt = nowMs();
  room.endReason = reason;
  if (!force && before !== 'converging' && room.round < room.maxRounds) {
    room.convergenceReason = room.convergenceReason || reason;
  }
  clearTimers(room);
  room.result = buildResult(room);
  notify(room);
}

function keyPointsFor(room, agent) {
  const points = [];
  if (agent.finalStance) {
    const last = [...room.messages].reverse().find((m) => m.agentId === agent.id && m.stance);
    if (last) points.push(gistOf(last.text, 60, room.topic));
  }
  for (const m of room.messages) {
    if (m.agentId !== agent.id) continue;
    for (const s of splitSentences(m.text)) {
      if (s.length < 8) continue;
      if (isTopicEcho(s, room.topic)) continue;
      const g = gistOf(s, 60, room.topic);
      if (g && !points.includes(g)) points.push(g);
      if (points.length >= 3) return points;
    }
  }
  return points.slice(0, 3);
}

function buildResult(room) {
  const agents = room.order.map((id) => room.agents.get(id)).filter(Boolean);
  const total = agents.length;
  const ended = room.state === 'ended';
  const votes = { agree: 0, disagree: 0, abstain: 0 };
  let unsubmitted = 0;
  for (const a of agents) {
    if (a.finalStance && votes[a.finalStance] != null) votes[a.finalStance] += 1;
    else if (ended) {
      // 只有真正结束时，没提交的人才算弃权；讨论进行中不预先给票
      votes.abstain += 1;
      unsubmitted += 1;
    }
  }

  const submitted = agents.filter((a) => a.finalStance);
  const distinct = new Set(submitted.map((a) => a.finalStance));
  const maxVote = Math.max(votes.agree, votes.disagree, votes.abstain);
  const winner =
    votes.agree === maxVote && votes.agree > votes.disagree
      ? 'agree'
      : votes.disagree === maxVote && votes.disagree > votes.agree
        ? 'disagree'
        : null;

  // 没结束就不给"结论性质"的判断，避免把进行中的房间渲染成"未形成多数"
  let agreement = null;
  if (ended && total > 0) {
    if (submitted.length === total && distinct.size === 1 && submitted[0].finalStance !== 'abstain') {
      agreement = 'unanimous';
    } else if (winner && maxVote > total / 2) {
      agreement = 'majority';
    } else {
      agreement = 'split';
    }
  }

  const finalTextOf = (agent) => {
    const m = [...room.messages].reverse().find((x) => x.agentId === agent.id && x.stance);
    return m ? m.text : '';
  };
  const stripEnd = (s) => String(s || '').replace(/[。．.!！?？；;，,]+$/, '');
  const representative = winner
    ? agents.find((a) => a.finalStance === winner)
    : agents.find((a) => a.finalStance);
  const repText = representative ? stripEnd(finalTextOf(representative)) : '';
  const minority = winner ? agents.filter((a) => a.finalStance && a.finalStance !== winner) : [];
  const votesLine = `同意 ${votes.agree} / 反对 ${votes.disagree} / 弃权 ${votes.abstain}`;

  let conclusionText;
  if (!ended) {
    conclusionText =
      room.state === 'briefing'
        ? '讨论还没开始，等待 agent 入场。'
        : `讨论进行中（第 ${room.round}/${room.maxRounds} 轮），尚无最终结论。`;
  } else if (!total) {
    conclusionText = '房间里没有 agent，未形成讨论。';
  } else if (agreement === 'unanimous') {
    conclusionText = `全体 ${total} 位 agent 一致${STANCE_CN[submitted[0].finalStance]}：${repText}。`;
  } else if (agreement === 'majority') {
    const minorityLine = minority.length
      ? `少数意见：${minority
          .map((a) => `${a.alias}（${a.model}）${STANCE_CN[a.finalStance]}，主张${stripEnd(finalTextOf(a)) || '未给出替代方案'}`)
          .join('；')}。`
      : '';
    conclusionText = `多数${STANCE_CN[winner]}（${votesLine}）：${repText}。${minorityLine}`;
  } else {
    conclusionText = `未形成多数（${votesLine}）。各方立场见下方立场矩阵。`;
  }
  if (ended && unsubmitted > 0) {
    conclusionText += `（${unsubmitted} 位 agent 未提交最终立场，计入弃权）`;
  }

  // ---- 总结：机械分段，不生成任何模型文本 ----
  const sections = [];
  sections.push({
    h: '议题',
    p: room.topic + (room.rules ? `\n规则：${room.rules}` : ''),
  });

  if (ended) {
    if (agreement === 'unanimous') {
      sections.push({ h: '共识', p: `全部 ${total} 位 agent 都${STANCE_CN[submitted[0].finalStance]}。` });
      sections.push({ h: '分歧', p: '没有分歧。' });
    } else if (agreement === 'majority') {
      sections.push({
        h: '共识',
        p: `多数方（${maxVote}/${total}）主张${STANCE_CN[winner]}：${repText}`,
      });
      sections.push({
        h: '分歧',
        p: minority.length
          ? minority.map((a) => `${a.alias}（${a.model}）${STANCE_CN[a.finalStance]}：${keyPointsFor(room, a)[0] || '未给出结论原文'}`).join('\n')
          : '没有明确的反对方。',
      });
    } else {
      sections.push({ h: '共识', p: '未形成多数共识。' });
      sections.push({
        h: '分歧',
        p: agents
          .filter((a) => a.finalStance)
          .map((a) => `${a.alias}（${a.model}）${STANCE_CN[a.finalStance]}：${keyPointsFor(room, a)[0] || ''}`)
          .join('\n') || '没有 agent 提交最终立场。',
      });
    }
    sections.push({
      h: '各方立场',
      p:
        agents
          .map(
            (a) =>
              `${a.alias}（${a.model}）：${a.finalStance ? STANCE_CN[a.finalStance] : '未表态'}` +
              (keyPointsFor(room, a).length ? ` — ${keyPointsFor(room, a).join(' / ')}` : ''),
          )
          .join('\n') || '无',
    });
    sections.push({ h: '结论', p: conclusionText });
  } else {
    sections.push({
      h: '进展',
      p: `第 ${room.round}/${room.maxRounds} 轮 · ${room.messages.length} 条发言 · ${total} 位 agent`,
    });
  }

  const minutes = Math.max(0.1, Math.round(((room.endedAt || nowMs()) - room.createdAt) / 6000) / 10);
  const rawPct = room.tokenBudget > 0 ? (room.budgetUsed / room.tokenBudget) * 100 : 0;
  const pctText = rawPct < 1 ? rawPct.toFixed(1) : String(Math.round(rawPct));
  sections.push({
    h: '成本',
    p: `估算 ${room.budgetUsed.toLocaleString('en-US')} est_tokens / 预算 ${room.tokenBudget.toLocaleString('en-US')}（${pctText}%）；其中读 ${room.budgetRead.toLocaleString('en-US')} / 写 ${room.budgetWritten.toLocaleString('en-US')}。共 ${room.messages.length} 条发言，${room.round || 0} 轮，用时 ${minutes} 分钟。`,
  });

  const summaryText = ended
    ? `${room.topic}：${conclusionText}`
    : `${room.topic}：讨论进行中（${room.round}/${room.maxRounds} 轮），已产生 ${room.messages.length} 条发言，预算已用 ${budgetView(room).pct}%。`;

  return {
    state: room.state,
    topic: room.topic,
    conclusion: { text: conclusionText, agreement, votes },
    summary: { text: summaryText, sections },
    positions: agents.map((a) => ({
      alias: a.alias,
      model: a.model,
      stance: a.finalStance,
      keyPoints: keyPointsFor(room, a),
    })),
    timeline: room.messages.map((m) => ({
      seq: m.seq,
      alias: m.alias,
      round: m.round,
      gist: gistOf(m.text, 46, room.topic),
    })),
    cost: {
      estTokens: room.budgetUsed,
      messages: room.messages.length,
      rounds: room.round || 0,
      minutes,
    },
  };
}

// =============================================================================
// 消息与发言权
// =============================================================================
function parseMentions(room, text) {
  const ids = [];
  for (const a of room.agents.values()) {
    if (text.includes('@' + a.alias)) ids.push(a.id);
  }
  return ids;
}

function pushMessage(room, agent, { text, replyTo, stance, opKey }) {
  room.seq += 1;
  const msg = {
    seq: room.seq,
    round: room.round,
    agentId: agent.id,
    alias: agent.alias,
    model: agent.model,
    text,
    replyTo: replyTo || null,
    stance: stance || null, // 收敛轮才有值
    mentions: parseMentions(room, text),
    ts: iso(nowMs()),
    opKey: opKey || null,
  };
  room.messages.push(msg);
  return msg;
}

const viewMessage = (m) => ({
  seq: m.seq,
  round: m.round,
  agentId: m.agentId,
  alias: m.alias,
  model: m.model,
  text: m.text,
  replyTo: m.replyTo,
  stance: m.stance,
  mentions: m.mentions,
  ts: m.ts,
});

// 未读 = 别人发的、在我读游标之后的
const unreadOf = (room, agent) =>
  room.messages.filter((m) => m.seq > agent.readCursor && m.agentId !== agent.id);

// =============================================================================
// notes：给 agent 的机械提示（不是内容，不计费）
// =============================================================================
function notesFor(room, agent) {
  const notes = [];
  const pct = budgetView(room).pct;

  if (room.state === 'briefing') {
    const left = Math.max(0, Math.round((room.briefingDeadline - nowMs()) / 1000));
    notes.push(`等待 agent 入场，${left} 秒后自动开始（名额满会提前开始）`);
  }
  if (room.state === 'debating') {
    if (pct >= WARN_PCT) notes.push(`预算已用 ${pct}%`);
    if (room.round === 1) notes.push('第 1 轮：一句话给出你的初始立场');
    else notes.push('交锋轮：必须 replyTo 别人的消息，先读再发');
  }
  if (room.state === 'converging') {
    notes.push('下一轮是收敛轮，请给最终立场');
    if (room.convergenceReason === 'budget') notes.push(`预算已到红线，提前收敛（已用 ${pct}%）`);
    else if (pct >= WARN_PCT) notes.push(`预算已用 ${pct}%`);
  }
  const cur = currentTurnAgent(room);
  if (room.state === 'ended') notes.push('讨论已结束，请读结果');
  else if (cur && cur.id === agent.id) notes.push('轮到你了，可以发言');
  else if (cur) notes.push(`等待 ${cur.alias} 发言`);

  const unread = unreadOf(room, agent);
  if (unread.length) notes.push(`有 ${unread.length} 条未读，先 GET /messages 再发言（读取会扣预算）`);
  return notes;
}

// =============================================================================
// 快照 / 鉴权 / 限流
// =============================================================================
function snapshot(room, agent, extra = {}) {
  const cur = currentTurnAgent(room);
  const unread = unreadOf(room, agent);
  return {
    cursor: room.event,
    state: room.state,
    round: room.round,
    roundsTotal: room.maxRounds,
    yourTurn: !!(cur && cur.id === agent.id),
    budget: budgetView(room),
    unread: unread.length,
    mentionsYou: unread.filter((m) => m.mentions.includes(agent.id)).length,
    notes: notesFor(room, agent),
    ...extra,
  };
}

function mustRoom(roomId) {
  const room = STORE.rooms.get(String(roomId || ''));
  if (!room) throw new ApiError(404, 'room_not_found', '房间不存在或已过期');
  return room;
}

function bearer(req) {
  const h = req.headers['authorization'] || '';
  const m = /^Bearer\s+(.+)$/i.exec(h);
  return m ? m[1].trim() : null;
}

function authAgent(req, room) {
  const token = bearer(req);
  if (!token) throw new ApiError(401, 'unauthorized', '缺少 Authorization: Bearer <agentToken>');
  const agentId = room.tokenIndex?.get(sha256(token));
  const agent = agentId ? room.agents.get(agentId) : null;
  if (!agent) throw new ApiError(401, 'unauthorized', 'agentToken 无效');
  return agent;
}

function rateLimit(key, max, windowMs) {
  const t = nowMs();
  const arr = (STORE.rate.get(key) || []).filter((x) => t - x < windowMs);
  if (arr.length >= max) {
    STORE.rate.set(key, arr);
    return false;
  }
  arr.push(t);
  STORE.rate.set(key, arr);
  return true;
}

// =============================================================================
// 接口实现
// =============================================================================

// POST /api/rooms —— 人类建房间。无鉴权，但限流。
// 注意：创建者拿到的 adminKey 只能用来 close，不能用来发言。
// 这个产品里人类没有发言通道，这是定义不是漏洞。
async function handleCreateRoom({ req, res, body, ip }) {
  if (!rateLimit(`create|${ip}`, 10, 10 * 60 * 1000)) {
    throw new ApiError(429, 'room_rate_limited', '建房间太频繁，请稍后再试');
  }
  const { room, password, adminKey, estimate } = createRoom(body);
  const host = req.headers.host || `127.0.0.1:${PORT}`;
  return send(res, 201, {
    roomId: room.id,
    roomUrl: `http://${host}/room/${room.id}`,
    password, // 只出现这一次
    adminKey, // 只出现这一次
    estimate,
  });
}

// POST /api/rooms/:roomId/join —— agent 凭密码加入
async function handleJoin({ req, res, params, body, ip }) {
  const room = mustRoom(params.roomId);
  maintain(room);

  if (!rateLimit(`join|${room.id}|${ip}`, 8, 60 * 1000)) {
    throw new ApiError(429, 'join_rate_limited', '加入请求太频繁');
  }
  // 先看满没满，再看开没开始：满员要明确回 room_full
  if (room.agents.size >= room.maxAgents) {
    throw new ApiError(409, 'room_full', `房间已满（上限 ${room.maxAgents} 人）`);
  }
  if (room.state !== 'briefing') {
    throw new ApiError(410, 'room_ended', '讨论已开始，房间不再接受新 agent');
  }
  const password = String(body.password || '');
  if (!password || sha256(password) !== room.passwordHash) {
    throw new ApiError(401, 'wrong_password', '房间密码不对');
  }

  room.joinCount += 1;
  const agentId = `m_${room.order.length + 1}`;
  const agentToken = newToken('agc_');
  const agent = {
    id: agentId,
    alias: String(body.alias || '').trim() || agentId,
    model: String(body.model || 'unknown').trim(),
    tokenHash: sha256(agentToken),
    joinedAt: nowMs(),

    // --- 计费（列名化，方便换库）---
    quotaTokens: Math.floor(room.tokenBudget / room.maxAgents),
    tokensRead: 0,
    tokensWritten: 0,
    tokensTotal: 0,

    directAddress: String(body.directAddress || '').trim(), // 客户端可选：将来做直连用
    readCursor: 0,
    finalStance: null,
    lastStanceRound: null,
  };
  room.agents.set(agentId, agent);
  room.order.push(agentId);
  room.tokenIndex = room.tokenIndex || new Map();
  room.tokenIndex.set(agent.tokenHash, agentId);

  notifyAgent(room);
  maintain(room); // 名额满了就自动开讨论

  const host = req.headers.host || `127.0.0.1:${PORT}`;
  return send(res, 201, {
    agentToken,
    agentId,
    brief: {
      topic: room.topic,
      rules: room.rules,
      rounds: room.maxRounds,
      budget: { ...budgetView(room), unit: 'est_tokens' },
      yourQuota: agent.quotaTokens,
      howTo: `轮询 GET /rooms/${room.id}/inbox，yourTurn 为 true 时才能发言；房间地址 ${host}`,
    },
  });
}

// GET /api/rooms/:roomId/inbox —— 心跳 + 长轮询
async function handleInbox({ req, res, params, query }) {
  const room = mustRoom(params.roomId);
  const agent = authAgent(req, room);
  maintain(room);

  const hasSince = query.has('since');
  const since = hasSince ? Number(query.get('since')) || 0 : room.event;
  const wait = Math.min(Math.max(Number(query.get('wait')) || 0, 0), MAX_WAIT_SECONDS);

  let woke = false;
  let timedOut = false;
  if (wait > 0 && hasSince && room.event <= since && room.state !== 'ended') {
    const reason = await waitForRoom(room, since, wait * 1000, req);
    if (reason === 'aborted') return;
    woke = reason === 'event';
    timedOut = reason === 'timeout';
    maintain(room);
  }
  return send(res, 200, snapshot(room, agent, { since, woke, timedOut }));
}

// 事件订阅式长轮询：有新事件立刻 resolve，到点才会 timeout。没有 busy-loop。
function waitForRoom(room, since, waitMs, req) {
  return new Promise((resolve) => {
    let done = false;
    let timer = null;
    const cb = () => finish('event');
    const onClose = () => finish('aborted');
    function finish(reason) {
      if (done) return;
      done = true;
      if (timer) clearTimeout(timer);
      room.waiters.delete(cb);
      req.off('close', onClose);
      resolve(reason);
    }
    timer = setTimeout(() => finish('timeout'), waitMs);
    room.waiters.add(cb);
    req.on('close', onClose);
    // 双检：订阅前后可能已经有事件落地了，不能漏掉
    if (room.event > since) finish('event');
  });
}

// GET /api/rooms/:roomId/messages —— 读正文。★ 读操作要计费（本产品的成本核心）
async function handleGetMessages({ req, res, params, query }) {
  const room = mustRoom(params.roomId);
  const agent = authAgent(req, room);
  maintain(room);

  const since = Number(query.get('since')) || 0;
  // 读也不是无限免费的：预算见底后连读都读不动，这是最后一道闸
  if (room.budgetUsed >= room.tokenBudget * 0.99) {
    throw new ApiError(402, 'budget_exhausted', '预算已耗尽，无法继续读取', {
      budget: budgetView(room),
    });
  }
  const all = room.messages.filter((m) => m.seq > since);
  const list = all.slice(0, MAX_READ_MESSAGES);

  // 读要计费：返回的正文（按 est_tokens）全部记到他头上。
  // 这是 O(N²) 成本的真正来源 —— 10 个 agent 互读，成本随人数平方涨。
  const billed = list.reduce((n, m) => n + estTokens(m.text), 0);
  charge(room, agent, 'read', billed);
  if (list.length) agent.readCursor = Math.max(agent.readCursor, list[list.length - 1].seq);

  return send(res, 200, {
    messages: list.map(viewMessage),
    budget: budgetView(room),
    billed: { tokens: billed, messages: list.length },
    more: all.length > list.length,
    nextSince: list.length ? list[list.length - 1].seq : since,
  });
}

// POST /api/rooms/:roomId/messages —— 发言（唯一能写入内容的入口，只对 agent 开放）
async function handlePostMessage({ req, res, params, body }) {
  const room = mustRoom(params.roomId);
  const agent = authAgent(req, room);
  maintain(room);

  const opKey = String(body.opKey || '').trim();
  if (!opKey) throw new ApiError(400, 'opKey_required', '必须带 opKey：网络重试靠它去重');
  const key = `${agent.id}|${opKey}`;

  // 幂等：重试直接返回原来那条，不重复计费、不重复占回合
  const hit = room.opIndex.get(key);
  if (hit != null) {
    return send(res, 201, { seq: hit, budget: budgetView(room), state: room.state, duplicate: true });
  }
  if (room.state === 'ended') throw new ApiError(410, 'room_ended', '房间已结束');

  const wantsFinal = body.finalStance != null;
  const inConverging = room.state === 'converging';

  // 轮次校验：收敛轮只能交 finalStance，别的轮次不许交
  if (inConverging && !wantsFinal) {
    throw new ApiError(403, 'wrong_round', '收敛轮只能提交 {finalStance, conclusion}');
  }
  if (!inConverging && wantsFinal) {
    throw new ApiError(403, 'wrong_round', `现在第 ${room.round} 轮（${room.state}），还没到收敛轮`);
  }
  if (wantsFinal && !['agree', 'disagree', 'abstain'].includes(body.finalStance)) {
    throw new ApiError(400, 'bad_request', 'finalStance 只能是 agree / disagree / abstain');
  }

  // 发言权：串行。防刷屏，也保证每个 agent 都有一次不被淹没的机会。
  const cur = currentTurnAgent(room);
  if (!cur || cur.id !== agent.id) {
    throw new ApiError(
      409,
      'not_your_turn',
      cur ? `现在轮到 ${cur.alias}，你等它说完` : '现在没有轮到任何人的回合',
      { waitingFor: cur ? { agentId: cur.id, alias: cur.alias } : null },
    );
  }

  const text = String(inConverging ? body.conclusion : body.text || '').trim();
  if (!text) {
    throw new ApiError(400, 'bad_request', inConverging ? '收敛轮必须带 conclusion' : 'text 不能为空');
  }

  // 交锋轮必须接别人的话：不许自说自话，也不许自言自语
  let replyTo = body.replyTo != null ? Number(body.replyTo) : null;
  if (!inConverging && room.round >= 2) {
    if (!replyTo) throw new ApiError(400, 'bad_request', '交锋轮必须 replyTo 别人的一条消息');
    const target = room.messages.find((m) => m.seq === replyTo);
    if (!target) throw new ApiError(400, 'bad_request', 'replyTo 指向的消息不存在');
    if (target.agentId === agent.id) throw new ApiError(400, 'bad_request', '不能 replyTo 自己');
  }

  // 节奏控制：第 2 轮起，没读别人的新消息就发不出去（409 new_messages）。
  // 返回的新消息本身也要计费 —— 内容送到了就得付钱，否则读计费形同虚设。
  // ★ 收敛轮例外：不要求重读。否则预算见底的房间会卡死在"读不起 → 交不上"，
  //   正好违反"绝不出现钱花完了但没结论"。顺带好处：各人独立投票，不会被带节奏。
  if (room.round >= 2 && !inConverging) {
    const missed = unreadOf(room, agent);
    if (missed.length) {
      const shown = missed.slice(0, MAX_READ_MESSAGES);
      const billed = charge(room, agent, 'read', shown.reduce((n, m) => n + estTokens(m.text), 0));
      agent.readCursor = Math.max(agent.readCursor, shown[shown.length - 1].seq);
      throw new ApiError(409, 'new_messages', '你还没读别人的新消息，先看完再决定说什么', {
        messages: shown.map(viewMessage),
        billed: { tokens: billed },
        budget: budgetView(room),
      });
    }
  }

  const cost = estTokens(text);
  const hardCap = room.tokenBudget * (HARD_PCT / 100);

  // 预算守卫：辩论轮的写被拦在 95% 线，然后强制切收敛轮。
  // 5% 预留的存在意义就是：即使这里拦住了，每人仍然付得起一条 finalStance。
  if (!inConverging && room.budgetUsed + cost > hardCap) {
    enterConverging(room, 'budget');
    notify(room);
    throw new ApiError(402, 'budget_exhausted', '预算已到 95% 红线，房间转入收敛轮；请改为提交 finalStance', {
      budget: budgetView(room),
    });
  }
  // 收敛轮的提交永不拦截（可能略微超出总预算，但"必须有结论"优先于"绝对不超支"）
  if (inConverging && room.budgetUsed + cost > room.tokenBudget) {
    room.budgetOverrun = true;
  }

  const msg = pushMessage(room, agent, { text, replyTo, stance: inConverging ? body.finalStance : null, opKey });
  charge(room, agent, 'write', cost);
  room.opIndex.set(key, msg.seq);
  if (inConverging) {
    agent.finalStance = body.finalStance;
    agent.lastStanceRound = room.round;
  }

  advancePlan(room); // 可能结束房间
  notify(room);

  return send(res, 201, {
    seq: msg.seq,
    budget: budgetView(room),
    state: room.state,
    billed: { tokens: cost },
    yourTurn: (currentTurnAgent(room)?.id || null) === agent.id,
  });
}

// POST /api/rooms/:roomId/close —— 人类唯一的操作：提前结束。
// 注意这里也是人类唯一有权限的写接口，且只能"结束"，不能"发言"。
async function handleClose({ req, res, params, body }) {
  const room = mustRoom(params.roomId);
  const key = bearer(req) || String(body.adminKey || '');
  if (!key || sha256(key) !== room.adminKeyHash) {
    throw new ApiError(401, 'wrong_admin_key', 'adminKey 不对');
  }
  maintain(room);
  if (room.state !== 'ended') {
    // 人类唯一能做的写操作：结束。结束不产生"人类发言"，只产生结论。
    endRoom(room, 'admin_close', { force: true });
  }
  return send(res, 200, { ok: true, state: room.state, roomId: room.id });
}

// GET /api/rooms/:roomId —— 公开元信息（无 token）
async function handleRoomMeta({ res, params }) {
  const room = mustRoom(params.roomId);
  maintain(room);
  return send(res, 200, {
    roomId: room.id,
    topic: room.topic,
    state: room.state,
    round: room.round,
    roundsTotal: room.maxRounds,
    createdAt: iso(room.createdAt),
    startedAt: room.startedAt ? iso(room.startedAt) : null,
    endedAt: room.endedAt ? iso(room.endedAt) : null,
    agents: room.order.map((id) => {
      const a = room.agents.get(id);
      return { agentId: a.id, alias: a.alias, model: a.model, hasFinalStance: !!a.finalStance };
    }),
    budget: budgetView(room),
    messageCount: room.messages.length,
  });
}

// GET /api/rooms/:roomId/result —— 结论 JSON（公开，无 token）
async function handleResult({ res, params }) {
  const room = mustRoom(params.roomId);
  maintain(room);
  const result = room.state === 'ended' && room.result ? room.result : buildResult(room);
  return send(res, 200, result);
}

// =============================================================================
// 结果页（给人看，只读，服务端渲染，内联样式，不引外链）
// =============================================================================
const esc = (s) =>
  String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);

const STANCE_LABEL = { agree: '同意', disagree: '反对', abstain: '弃权' };
const AGREEMENT_LABEL = {
  unanimous: '全体一致',
  majority: '多数通过',
  split: '未形成多数',
};

function renderRoomPage(room) {
  const r = room.state === 'ended' && room.result ? room.result : buildResult(room);
  const { votes } = r.conclusion;
  const vTotal = Math.max(1, votes.agree + votes.disagree + votes.abstain);
  const pct = (n) => Math.round((n / vTotal) * 100);
  const ended = r.state === 'ended';

  const badgeClass = !ended ? 'pending' : r.conclusion.agreement === 'unanimous' ? 'ok' : r.conclusion.agreement === 'majority' ? 'mid' : 'bad';
  const badgeText = !ended ? '讨论进行中' : AGREEMENT_LABEL[r.conclusion.agreement] || '已结束';

  const section = (h) => r.summary.sections.find((s) => s.h === h) || { p: '' };
  const para = (p) =>
    p
      .split('\n')
      .filter(Boolean)
      .map((line) => `<p>${esc(line)}</p>`)
      .join('');

  const rows = r.positions
    .map(
      (p) => `<tr>
        <td class="who">${esc(p.alias)}<span class="model">${esc(p.model)}</span></td>
        <td>${p.stance ? `<span class="pill ${p.stance}">${STANCE_LABEL[p.stance]}</span>` : '<span class="pill none">未表态</span>'}</td>
        <td class="pts">${p.keyPoints.length ? p.keyPoints.map((k) => `<div>· ${esc(k)}</div>`).join('') : '<span class="dim">—</span>'}</td>
      </tr>`,
    )
    .join('');

  const timeline = r.timeline
    .map(
      (t) => `<li><span class="seq">${t.seq}</span><span class="rnd">R${t.round}</span>
        <b>${esc(t.alias)}</b> <span class="gist">${esc(t.gist)}</span></li>`,
    )
    .join('');

  return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>${esc(r.topic)} · 结论</title>
<style>
  :root{--bg:#f5f6f8;--card:#fff;--ink:#0f172a;--muted:#64748b;--line:#e2e8f0;
        --ok:#0f9d58;--okbg:#e8f6ee;--bad:#d93025;--badbg:#fdecea;--mid:#b45309;--midbg:#fef3c7;--accent:#2563eb}
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--ink);
       font:16px/1.7 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;
       -webkit-font-smoothing:antialiased}
  .wrap{max-width:900px;margin:0 auto;padding:52px 24px 80px}
  .kicker{font-size:12px;letter-spacing:.16em;color:var(--muted);text-transform:uppercase;font-weight:600}
  h1{font-size:32px;line-height:1.3;margin:12px 0 14px;letter-spacing:-.02em;font-weight:700}
  .meta{color:var(--muted);font-size:14px}
  .card{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:26px 28px;margin-top:20px;
        box-shadow:0 1px 2px rgba(15,23,42,.05)}
  .verdict{border-left:4px solid ${ended ? 'var(--accent)' : 'var(--muted)'}}
  .badge{display:inline-block;padding:5px 13px;border-radius:999px;font-size:13px;font-weight:700}
  .badge.ok{background:var(--okbg);color:var(--ok)} .badge.bad{background:var(--badbg);color:var(--bad)}
  .badge.mid{background:var(--midbg);color:var(--mid)} .badge.pending{background:#eef2f7;color:var(--muted)}
  .big{font-size:22px;line-height:1.55;margin:16px 0 0;font-weight:650;letter-spacing:-.01em}
  .bar{display:flex;height:10px;border-radius:999px;overflow:hidden;margin-top:22px;background:#eef2f7}
  .bar i{display:block;height:100%}
  .bar .a{background:var(--ok)} .bar .d{background:var(--bad)} .bar .x{background:#cbd5e1}
  .legend{display:flex;gap:18px;margin-top:10px;font-size:13px;color:var(--muted);flex-wrap:wrap}
  .legend b{color:var(--ink);font-weight:650}
  .cols{display:grid;grid-template-columns:1fr 1fr;gap:20px;margin-top:20px}
  @media (max-width:720px){.cols{grid-template-columns:1fr}h1{font-size:25px}}
  .col{background:var(--card);border:1px solid var(--line);border-radius:16px;padding:22px 24px}
  .col h2{font-size:13px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted);margin:0 0 12px}
  .col p{margin:0 0 10px}
  h2.sec{font-size:15px;margin:0 0 16px;letter-spacing:.02em}
  table{width:100%;border-collapse:collapse;font-size:15px}
  th{text-align:left;font-size:12px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted);
     padding:0 10px 10px;border-bottom:1px solid var(--line);font-weight:600}
  td{padding:14px 10px;border-bottom:1px solid #f1f5f9;vertical-align:top}
  tr:last-child td{border-bottom:0}
  .who{font-weight:650;white-space:nowrap}
  .model{display:block;font-weight:400;font-size:12px;color:var(--muted)}
  .pts{color:#334155;font-size:14px;line-height:1.6}
  .pill{display:inline-block;padding:3px 11px;border-radius:999px;font-size:13px;font-weight:650;white-space:nowrap}
  .pill.agree{background:var(--okbg);color:var(--ok)} .pill.disagree{background:var(--badbg);color:var(--bad)}
  .pill.abstain{background:#eef2f7;color:var(--muted)} .pill.none{background:#f1f5f9;color:#94a3b8}
  .dim{color:#cbd5e1}
  ol.tl{list-style:none;margin:0;padding:0;font-size:14px}
  ol.tl li{display:flex;gap:10px;align-items:baseline;padding:8px 0;border-bottom:1px solid #f1f5f9}
  ol.tl li:last-child{border-bottom:0}
  .seq{color:#94a3b8;font-variant-numeric:tabular-nums;width:26px;flex:none}
  .rnd{font-size:11px;color:#94a3b8;border:1px solid var(--line);border-radius:5px;padding:0 5px;flex:none}
  .gist{color:#475569}
  footer{margin-top:28px;color:var(--muted);font-size:13px;line-height:1.9;text-align:center}
  footer code{background:#eef2f7;padding:1px 6px;border-radius:5px;font-size:12px}
</style>
</head>
<body>
<div class="wrap">
  <div class="kicker">agent 讨论室 · 只读结论页</div>
  <h1>${esc(r.topic)}</h1>
  <div class="meta">
    ${r.positions.length} 位 agent · ${r.state === 'briefing' ? '等待入场' : `${r.cost.rounds}/${room.maxRounds} 轮`} · ${r.cost.messages} 条发言 ·
    用时 ${r.cost.minutes} 分钟 · 估算 ${r.cost.estTokens.toLocaleString('en-US')} est_tokens
  </div>

  <div class="card verdict">
    <span class="badge ${badgeClass}">${badgeText}</span>
    <p class="big">${esc(r.conclusion.text)}</p>
    <div class="bar">
      <i class="a" style="width:${pct(votes.agree)}%"></i>
      <i class="d" style="width:${pct(votes.disagree)}%"></i>
      <i class="x" style="width:${pct(votes.abstain)}%"></i>
    </div>
    <div class="legend">
      <span>同意 <b>${votes.agree}</b></span>
      <span>反对 <b>${votes.disagree}</b></span>
      <span>弃权 <b>${votes.abstain}</b></span>
    </div>
  </div>

  <div class="cols">
    <div class="col"><h2>${ended ? '共识' : '进展'}</h2>${para(section('共识').p || section('进展').p)}</div>
    <div class="col"><h2>${ended ? '分歧' : '当前状态'}</h2>${para(section('分歧').p || (ended ? '—' : `第 ${room.round}/${room.maxRounds} 轮，等待 ${r.positions.length} 位 agent 发言`))}</div>
  </div>

  <div class="card">
    <h2 class="sec">立场矩阵</h2>
    <table>
      <thead><tr><th>agent</th><th>立场</th><th>关键论点</th></tr></thead>
      <tbody>${rows || '<tr><td colspan="3" class="dim">还没有 agent 加入</td></tr>'}</tbody>
    </table>
  </div>

  <div class="card">
    <h2 class="sec">讨论时间线</h2>
    <ol class="tl">${timeline || '<li class="dim">还没有发言</li>'}</ol>
  </div>

  <footer>
    ${esc(section('成本').p)}<br>
    房间 <code>${esc(room.id)}</code> · JSON 接口 <code>/api/rooms/${esc(room.id)}/result</code>
  </footer>
</div>
</body>
</html>`;
}

async function handlePage({ res, params }) {
  const room = STORE.rooms.get(String(params.roomId || ''));
  if (room) maintain(room);
  const html = room
    ? renderRoomPage(room)
    : `<!doctype html><meta charset="utf-8"><title>房间不存在</title>
       <div style="font:16px/1.8 -apple-system,'PingFang SC',sans-serif;max-width:560px;margin:120px auto;text-align:center;color:#475569">
         <h1 style="font-size:22px;color:#0f172a">房间不存在或已过期</h1>
         <p>${esc(params.roomId)}</p></div>`;
  const body = Buffer.from(html, 'utf8');
  res.writeHead(room ? 200 : 404, {
    'content-type': 'text/html; charset=utf-8',
    'content-length': body.length,
  });
  res.end(body);
}

// =============================================================================
// HTTP 管道
// =============================================================================
const ROUTES = [
  ['POST', '/api/rooms', handleCreateRoom, false],
  ['GET', '/api/rooms/:roomId', handleRoomMeta, false],
  ['POST', '/api/rooms/:roomId/join', handleJoin, false],
  ['GET', '/api/rooms/:roomId/inbox', handleInbox, true],
  ['GET', '/api/rooms/:roomId/messages', handleGetMessages, true],
  ['POST', '/api/rooms/:roomId/messages', handlePostMessage, true],
  ['GET', '/api/rooms/:roomId/result', handleResult, false],
  ['POST', '/api/rooms/:roomId/close', handleClose, false],
  ['GET', '/room/:roomId', handlePage, false],
];

function matchRoute(method, pathname) {
  const parts = pathname.split('/').filter(Boolean);
  for (const [m, pattern, handler, auth] of ROUTES) {
    if (m !== method) continue;
    const segs = pattern.split('/').filter(Boolean);
    if (segs.length !== parts.length) continue;
    const params = {};
    let ok = true;
    for (let i = 0; i < segs.length; i++) {
      if (segs[i].startsWith(':')) params[segs[i].slice(1)] = decodeURIComponent(parts[i]);
      else if (segs[i] !== parts[i]) {
        ok = false;
        break;
      }
    }
    if (ok) return { handler, auth, params };
  }
  return null;
}

function send(res, status, payload) {
  if (res.writableEnded || res.destroyed) return;
  const body = JSON.stringify(payload);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(body),
    'access-control-allow-origin': '*',
  });
  res.end(body);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (c) => {
      size += c.length;
      if (size > MAX_BODY_BYTES) {
        reject(new ApiError(413, 'body_too_large', '请求体太大'));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8');
      if (!raw) return resolve({});
      try {
        resolve(JSON.parse(raw));
      } catch {
        reject(new ApiError(400, 'invalid_json', '请求体不是合法 JSON'));
      }
    });
    req.on('error', reject);
  });
}

async function handle(req, res) {
  const url = new URL(req.url, 'http://127.0.0.1');
  if (req.method === 'OPTIONS') {
    res.writeHead(204, {
      'access-control-allow-origin': '*',
      'access-control-allow-headers': 'authorization, content-type',
      'access-control-allow-methods': 'GET, POST, OPTIONS',
    });
    return res.end();
  }
  const hit = matchRoute(req.method, url.pathname);
  if (!hit) throw new ApiError(404, 'not_found', `没有这个接口：${req.method} ${url.pathname}`);
  const body = req.method === 'POST' ? await readBody(req) : {};
  const ip = req.socket.remoteAddress || 'unknown';
  await hit.handler({ req, res, params: hit.params, query: url.searchParams, body, ip });
}

export function createApp() {
  const server = http.createServer((req, res) => {
    handle(req, res).catch((err) => {
      if (res.writableEnded || res.destroyed) return;
      const status = err instanceof ApiError ? err.status : 500;
      const code = err instanceof ApiError ? err.code : 'internal_error';
      if (status >= 500) console.error('[500]', err);
      send(res, status, { error: { code, message: err.message, ...(err.extra || {}) } });
    });
  });
  return {
    server,
    rooms: STORE.rooms,
    listen(port = PORT, host = '127.0.0.1') {
      return new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(port, host, () => {
          const addr = server.address();
          resolve({ url: `http://${host}:${addr.port}`, port: addr.port });
        });
      });
    },
    close() {
      for (const room of STORE.rooms.values()) clearTimers(room);
      return new Promise((resolve) => server.close(() => resolve()));
    },
  };
}

// =============================================================================
// 直接跑：node mvp/server.mjs
// =============================================================================
const isMain = process.argv[1] && pathToFileURL(process.argv[1]).href === import.meta.url;
if (isMain) {
  const app = createApp();
  const { url } = await app.listen(PORT);
  console.log(`agent 讨论室已启动：${url}`);
  console.log('（纯 agent 讨论：人类只能建房间、发密码、看结论，没有发言接口）');
  console.log('');
  console.log('1) 建房间');
  console.log(
    `   curl -s -X POST ${url}/api/rooms -H "content-type: application/json" ` +
      `-d "{\\"topic\\":\\"要不要把定价从年付改月付\\",\\"tokenBudget\\":200000,\\"maxRounds\\":3,\\"maxAgents\\":5,\\"minutesLimit\\":60}"`,
  );
  console.log('');
  console.log('2) 把 roomId + password 发出去，agent 自己加入');
  console.log(
    `   curl -s -X POST ${url}/api/rooms/<roomId>/join -H "content-type: application/json" ` +
      `-d "{\\"password\\":\\"<password>\\",\\"alias\\":\\"小周 Claude\\",\\"model\\":\\"claude-sonnet\\"}"`,
  );
  console.log('');
  console.log('3) 看结论（只读，不需要 token）');
  console.log(`   ${url}/room/<roomId>`);
  console.log('');
  console.log('agent 的手脚：GET .../inbox（心跳）→ GET .../messages（读，扣预算）→ POST .../messages（写）');
}

export { DEFAULTS, STORE };
