// mvp/agent.mjs
// ─────────────────────────────────────────────────────────────────────────────
// 通用 agent 客户端：RoomAgent
//
// 为什么长这样：真实环境里的 agent（ChatGPT / Claude Code / Cursor / 自建 loop）
// 没有一个"常驻进程"可以被动收消息，只能被宿主的定时任务或主循环唤起。
// 所以 run() 就是那个主循环的抽象：
//
//     长轮询 inbox  →  有未读才拉正文  →  decide()  →  发言  →  处理冲突
//        │                  │                │           │
//   宿主 cron/routine    省 token 的关键   模型调用     工具调用
//
// decide() 是唯一需要替换的部分：
//   - 真实 ChatGPT / Claude：decide 里把模型叫起来，把 ctx 拼成 prompt
//   - 脚本化 / 测试：用本文件的 scripted()，或 worker.mjs 里的人格
// 本文件不含任何 LLM 依赖，只做 I/O 与协议。
// ─────────────────────────────────────────────────────────────────────────────

import { randomUUID } from 'node:crypto';

export const DEFAULT_BASE_URL = 'http://127.0.0.1:8788/api';

/* ══════════════════════════════════════════════════════════════════════════
   错误类型
   ══════════════════════════════════════════════════════════════════════════ */

export class RoomError extends Error {
  constructor(status, code, body) {
    super(`[${status === 0 ? 'network' : status}] ${code}`);
    this.name = 'RoomError';
    this.status = status;
    this.code = code;
    this.body = body || {};
    // 409 new_messages 时，服务端会把"你写作期间别人发的话"塞在响应体里。
    // 不同实现可能放在不同层级，这里统一抽出来。
    this.messages = pickMessages(body);
  }
}

function pickMessages(body) {
  if (!body || typeof body !== 'object') return [];
  const candidates = [
    body.messages,
    body.error && body.error.messages,
    body.data && body.data.messages,
    body.new_messages,
    body.newMessages,
  ];
  for (const c of candidates) if (Array.isArray(c)) return c;
  return [];
}

function codeFromStatus(status) {
  const table = {
    400: 'bad_request',
    401: 'unauthorized',
    402: 'budget_exhausted',
    403: 'wrong_round',
    404: 'not_found',
    409: 'conflict',
    410: 'room_ended',
    429: 'rate_limited',
    500: 'server_error',
  };
  return table[status] || `http_${status}`;
}

export function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/* ══════════════════════════════════════════════════════════════════════════
   底层 HTTP：统一超时、统一错误形状
   ══════════════════════════════════════════════════════════════════════════ */

async function request(baseUrl, path, opts = {}) {
  const { method = 'GET', token, body, query, timeoutMs = 30_000 } = opts;

  const url = new URL(String(baseUrl).replace(/\/+$/, '') + path);
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v !== undefined && v !== null) url.searchParams.set(k, String(v));
    }
  }

  const headers = { accept: 'application/json' };
  if (token) headers.authorization = `Bearer ${token}`;
  let payload;
  if (body !== undefined) {
    headers['content-type'] = 'application/json';
    payload = JSON.stringify(body);
  }

  // 长轮询的请求本身要活到 wait 秒之后，所以超时按 wait 动态放宽
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  let res;
  try {
    res = await fetch(url, { method, headers, body: payload, signal: ctrl.signal });
  } catch (err) {
    throw new RoomError(0, 'network_error', { message: String((err && err.message) || err) });
  } finally {
    clearTimeout(timer);
  }

  const text = await res.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    json = { raw: text };
  }

  if (res.ok) return { status: res.status, data: json };

  const code =
    (json && json.error && json.error.code) ||
    (json && json.code) ||
    codeFromStatus(res.status);
  throw new RoomError(res.status, code, json);
}

/* ══════════════════════════════════════════════════════════════════════════
   RoomAgent
   ══════════════════════════════════════════════════════════════════════════ */

export class RoomAgent {
  /**
   * @param {object} o
   * @param {string} o.baseUrl    形如 http://127.0.0.1:8788/api
   * @param {string} o.roomId
   * @param {string} o.password   房间密码（进场用，进场后换 agentToken）
   * @param {string} o.alias      群里显示的昵称
   * @param {string} o.model      模型标识，只用于展示与统计
   * @param {Function} o.decide   决策函数，见 _ctx()
   * @param {number} [o.pollWait] 长轮询等待秒数
   * @param {Function} [o.log]    日志回调 (event, payload) => void
   */
  constructor({
    baseUrl = DEFAULT_BASE_URL,
    roomId,
    password,
    alias,
    model = 'unknown',
    decide,
    pollWait = 20,
    log = null,
    maxSteps = 600,
  } = {}) {
    if (!roomId) throw new Error('RoomAgent 需要 roomId');
    if (!password) throw new Error('RoomAgent 需要 password');
    if (typeof decide !== 'function') throw new Error('RoomAgent 需要 decide(ctx) 函数');

    this.baseUrl = baseUrl;
    this.roomId = roomId;
    this.password = password;
    this.alias = alias || model;
    this.model = model;
    this.decide = decide;
    this.pollWait = pollWait;
    this.log = log;
    this.maxSteps = maxSteps;

    // 凭证：join 之后才有
    this.agentToken = null;
    this.agentId = null;
    this.brief = null;

    // 协议状态
    this.cursor = 0; // inbox 游标，只增
    this.lastSeq = 0; // 消息已读位置
    this.state = 'briefing';
    this.round = 1;
    this.roundsTotal = 3;
    this.yourTurn = false;
    this.turnMode = 'unknown'; // 'explicit' | 'free'
    this.budget = null;
    this.unreadCount = 0;
    this.mentionsYou = 0;
    this.notes = [];

    // 本地上下文
    this.unread = []; // 最近一次拉到的未读
    this.context = []; // 全部已知消息（按 seq 去重、升序）
    this.mine = []; // 我发出去的
    this.stopped = false;
    this.stats = { speaks: 0, conflicts: 0, polls: 0, reads: 0 };
  }

  /* ── 进场 ─────────────────────────────────────────────────────────────── */

  async join() {
    const { data } = await request(this.baseUrl, `/rooms/${this.roomId}/join`, {
      method: 'POST',
      body: { password: this.password, alias: this.alias, model: this.model },
      timeoutMs: 30_000,
    });
    if (!data || !data.agentToken) {
      throw new Error('join 响应里没有 agentToken，检查 server 是否符合契约');
    }
    this.agentToken = data.agentToken;
    this.agentId = data.agentId;
    this.brief = data.brief || null;
    if (this.brief && Number.isFinite(this.brief.rounds)) {
      this.roundsTotal = this.brief.rounds;
    }
    this._emit('join', { agentId: this.agentId, alias: this.alias });
    return data;
  }

  _emit(event, payload) {
    if (typeof this.log === 'function') {
      try {
        this.log(event, payload || {});
      } catch {
        /* 日志永不打断主循环 */
      }
    }
  }

  /* ── inbox：只拿计数，不拿正文（省钱的关键） ───────────────────────── */

  async inbox({ wait = this.pollWait } = {}) {
    const { data } = await request(this.baseUrl, `/rooms/${this.roomId}/inbox`, {
      token: this.agentToken,
      query: { since: this.cursor, wait },
      // 长轮询：服务端最多挂 wait 秒，再给足网络余量
      timeoutMs: (Number(wait) + 20) * 1000,
    });
    this.stats.polls += 1;

    if (Number.isFinite(data.cursor)) this.cursor = data.cursor;
    if (data.state) this.state = data.state;
    if (Number.isFinite(data.round)) this.round = data.round;
    if (Number.isFinite(data.roundsTotal)) this.roundsTotal = data.roundsTotal;
    if (data.budget) this.budget = data.budget;
    if (Number.isFinite(data.unread)) this.unreadCount = data.unread;
    if (Number.isFinite(data.mentionsYou)) this.mentionsYou = data.mentionsYou;
    if (Array.isArray(data.notes)) this.notes = data.notes;

    if (typeof data.yourTurn === 'boolean') {
      this.yourTurn = data.yourTurn;
      this.turnMode = 'explicit';
    } else {
      // 服务端没给 yourTurn：退化成"有未读就能说"的自由模式
      this.turnMode = 'free';
      this.yourTurn = this.unreadCount > 0;
    }
    return data;
  }

  /* ── 拉正文 ───────────────────────────────────────────────────────────── */

  async readMessages({ since } = {}) {
    const from = since === undefined ? this.lastSeq : since;
    const { data } = await request(this.baseUrl, `/rooms/${this.roomId}/messages`, {
      token: this.agentToken,
      query: { since: from },
      timeoutMs: 30_000,
    });
    this.stats.reads += 1;
    if (data && data.budget) this.budget = data.budget;
    const list = Array.isArray(data && data.messages) ? data.messages : [];
    this.unread = this._ingest(list);
    return list;
  }

  /** 把消息合进本地上下文：按 seq 去重、升序、截断 */
  _ingest(list) {
    if (!Array.isArray(list) || list.length === 0) return [];
    const seen = new Set(this.context.map((m) => m.seq));
    const fresh = [];
    for (const m of list) {
      if (!m || !Number.isFinite(m.seq)) continue;
      if (seen.has(m.seq)) continue;
      seen.add(m.seq);
      fresh.push(m);
      if (m.seq > this.lastSeq) this.lastSeq = m.seq;
      if (this.agentId && m.agentId === this.agentId) this.mine.push(m);
    }
    if (fresh.length) {
      this.context.push(...fresh);
      this.context.sort((a, b) => a.seq - b.seq);
      // 上下文窗口保护：只留最近 200 条，长讨论靠 server 侧摘要兜底
      if (this.context.length > 200) this.context = this.context.slice(-200);
    }
    return fresh;
  }

  /* ── 决策上下文 ───────────────────────────────────────────────────────── */

  _ctx() {
    // 契约字段 + 两个便利字段（agentId / alias）。多给不会坏事，
    // 让 decide 实现不必自己去猜"哪条是我发的"。
    return {
      brief: this.brief,
      round: this.round,
      roundsTotal: this.roundsTotal,
      state: this.state,
      yourTurn: this.yourTurn,
      unread: this.unreadCount,
      news: this.unread.slice(),
      context: this.context.slice(-40),
      budget: this.budget,
      notes: this.notes.slice(),
      myPastMessages: this.mine.slice(-10),
      agentId: this.agentId,
      alias: this.alias,
    };
  }

  /* ── 该不该说话 ───────────────────────────────────────────────────────── */

  _shouldSpeak() {
    if (this.state === 'ended') return false;
    // 服务端给了明确的 yourTurn，就严格按回合来（串行发言，防刷屏）
    if (this.turnMode === 'explicit') return this.yourTurn === true;
    // 服务端没给 yourTurn 的自由模式：有未读就说；收敛轮必须表态
    if (this.state === 'converging') return true;
    return this.unreadCount > 0;
  }

  /* ── 发言：含 409 和解（协议要求，必须在 Agent 层做掉） ─────────────── */

  async _speakOnce(ctx) {
    let out = await this.decide(ctx);
    if (!out) return { skipped: 'declined' };

    // opKey 在"同一条逻辑消息"的所有重试里保持不变：
    // 即使上一次其实已经落库（只是响应丢了），服务端也不会重复发。
    const opKey = out.opKey || randomUUID();
    let attempt = 0;

    while (attempt < 5) {
      attempt += 1;
      const payload = this._payload(out, opKey);
      try {
        const { data } = await request(this.baseUrl, `/rooms/${this.roomId}/messages`, {
          method: 'POST',
          token: this.agentToken,
          body: payload,
          timeoutMs: 30_000,
        });
        this._afterSend(out, data);
        return { ok: true, data };
      } catch (err) {
        if (!(err instanceof RoomError)) throw err;

        // ── 409 new_messages：有人在你写作期间发了话 ──────────────────
        // 协议要求的三步：① 收下新消息 ② 重新决策 ③ 用**同一个 opKey** 重发。
        if (err.code === 'new_messages' || (err.status === 409 && err.messages.length)) {
          this.stats.conflicts += 1;
          this._ingest(err.messages);
          this.unread = err.messages.slice();
          this._emit('conflict', { attempt, got: err.messages.length });
          // 让 decide 看到最新局面：它可能改口，也可能决定不说了
          out = await this.decide(this._ctx());
          if (!out) return { skipped: 'declined_after_conflict' };
          continue;
        }

        // ── 没轮到你：正常现象，不是错误 ─────────────────────────────
        if (err.code === 'not_your_turn') {
          this.yourTurn = false;
          return { skipped: 'not_your_turn' };
        }

        // ── 预算耗尽：整场结束 ───────────────────────────────────────
        if (err.code === 'budget_exhausted' || err.status === 402) {
          this.state = 'ended';
          return { skipped: 'budget_exhausted' };
        }

        // ── 轮次不符：可能刚翻轮，刷新状态再试 ───────────────────────
        if (err.code === 'wrong_round' || err.status === 403) {
          try {
            await this.inbox({ wait: 0 });
          } catch {
            /* 刷新失败就先退避 */
          }
          continue;
        }

        // ── 房间结束 / token 失效：不重试 ────────────────────────────
        if (err.code === 'room_ended' || err.status === 410) {
          this.state = 'ended';
          return { skipped: 'room_ended' };
        }
        if (err.status === 401) throw err;

        // ── 网络抖动：同一个 opKey 重试，绝不会重复发 ────────────────
        if (err.status === 0) {
          await sleep(800 * attempt);
          continue;
        }
        throw err;
      }
    }
    return { skipped: 'gave_up' };
  }

  _payload(out, opKey) {
    // 收敛轮：交立场 + 一句话结论
    if (out.finalStance) {
      return { finalStance: out.finalStance, conclusion: out.conclusion || '', opKey };
    }
    // 普通轮：发言
    const body = { text: String(out.text || '').trim(), opKey };
    if (out.replyTo !== undefined && out.replyTo !== null) body.replyTo = out.replyTo;
    if (out.stance) body.stance = out.stance;
    return body;
  }

  _afterSend(out, data) {
    this.stats.speaks += 1;
    if (data && data.budget) this.budget = data.budget;
    if (data && data.state) this.state = data.state;
    if (data && Number.isFinite(data.seq)) {
      this.lastSeq = Math.max(this.lastSeq, data.seq);
      const rec = {
        seq: data.seq,
        round: this.round,
        agentId: this.agentId,
        alias: this.alias,
        model: this.model,
        // 收敛轮的正文是 conclusion，不是 text——本地记录跟着走，
        // 否则自己那条会在转写里显示成空。
        text: out.text || out.conclusion || '',
        replyTo: out.replyTo ?? null,
        stance: out.finalStance || out.stance || null,
        ts: new Date().toISOString(),
      };
      // 自己的发言也要进上下文，否则下一轮看不到自己说过什么
      if (!this.context.some((m) => m.seq === data.seq)) {
        this.context.push(rec);
        this.context.sort((a, b) => a.seq - b.seq);
      }
      this.mine.push(rec);
    }
    this.unread = [];
    this.unreadCount = 0;
    this._emit('speak', { seq: data && data.seq, stance: out.finalStance || null });
  }

  /* ── 主循环 ───────────────────────────────────────────────────────────── */

  async run() {
    if (!this.agentToken) await this.join();
    let step = 0;
    let consecutiveErrors = 0;

    while (!this.stopped && step < this.maxSteps) {
      step += 1;

      // ① 长轮询：在真实 agent 里，这一步就是宿主把进程唤起
      let info;
      try {
        info = await this.inbox();
        consecutiveErrors = 0;
      } catch (err) {
        if (err instanceof RoomError && (err.status === 410 || err.code === 'room_ended')) {
          this.state = 'ended';
          break;
        }
        if (err instanceof RoomError && err.status === 401) throw err;
        consecutiveErrors += 1;
        if (consecutiveErrors > 8) throw err;
        await sleep(Math.min(1000 * consecutiveErrors, 5000));
        continue;
      }

      if (this.state === 'ended') break;

      // ② 有未读才拉正文。inbox 只给计数，正文按需取——这是省钱的第一道闸
      if (this.unreadCount > 0) {
        try {
          await this.readMessages();
        } catch (err) {
          if (err instanceof RoomError && err.status === 401) throw err;
          await sleep(500);
          continue;
        }
      }

      // ③ 决定要不要发言，然后发
      if (!this._shouldSpeak()) continue;

      const res = await this._speakOnce(this._ctx());
      if (res && res.skipped === 'budget_exhausted') break;
      if (res && res.skipped === 'room_ended') break;
    }

    this._emit('exit', { state: this.state, ...this.stats });
    return { state: this.state, agentId: this.agentId, alias: this.alias, ...this.stats };
  }
}

/* ══════════════════════════════════════════════════════════════════════════
   scripted(rules)：把"规则表"变成一个 decide 函数
   ──────────────────────────────────────────────────────────────────────────
   两种写法：

   1) 规则列表（按顺序命中第一条）
      scripted([
        { when: (ctx) => ctx.round === 1, text: '我先说个初步判断：……' },
        { when: (ctx) => ctx.mentionsYou > 0, text: (ctx) => '回 @……', replyTo: (ctx) => ctx.news[0].seq },
      ])

   2) 带收敛表态
      scripted({
        rules: [ { when: () => true, text: '……' } ],
        converge: () => ({ finalStance: 'agree', conclusion: '可以试，但要设止损。' }),
      })

   返回 null 表示"这轮我不说"——这是合法且常见的选择。
   ══════════════════════════════════════════════════════════════════════════ */

export function scripted(rules = {}) {
  const isList = Array.isArray(rules);
  const list = isList ? rules : Array.isArray(rules.rules) ? rules.rules : [];
  const converge = isList ? null : rules.converge || rules.stance || null;

  const resolve = (v, ctx) => (typeof v === 'function' ? v(ctx) : v);

  return function decide(ctx) {
    // 收敛轮优先：能表态就表态
    if (ctx.state === 'converging' && converge) {
      const out = resolve(converge, ctx);
      if (out && out.finalStance) {
        return { finalStance: out.finalStance, conclusion: out.conclusion || '' };
      }
    }
    for (const rule of list) {
      if (!rule) continue;
      if (typeof rule.when === 'function' && !rule.when(ctx)) continue;
      if (rule.finalStance) {
        return {
          finalStance: resolve(rule.finalStance, ctx),
          conclusion: resolve(rule.conclusion, ctx) || '',
        };
      }
      const text = resolve(rule.text, ctx);
      if (!text) continue;
      const out = { text: String(text) };
      const replyTo = resolve(rule.replyTo, ctx);
      if (replyTo !== undefined && replyTo !== null) out.replyTo = replyTo;
      return out;
    }
    return null;
  };
}

/* ══════════════════════════════════════════════════════════════════════════
   房间级别的辅助函数（demo / 运营脚本用，不是 agent 该做的事）
   ══════════════════════════════════════════════════════════════════════════ */

/** 建房。房间是"人"建的，所以这里不带 agent token。 */
export async function createRoom(room, { baseUrl = DEFAULT_BASE_URL } = {}) {
  const { data } = await request(baseUrl, '/rooms', {
    method: 'POST',
    body: room,
    timeoutMs: 30_000,
  });
  return data;
}

/** 读结果页（公开接口，不需要 token）。 */
export async function getResult({ baseUrl = DEFAULT_BASE_URL, roomId }) {
  const { data } = await request(baseUrl, `/rooms/${roomId}/result`, { timeoutMs: 30_000 });
  return data;
}

/** 关房（需要 adminKey）。demo 一般不用，留作运维接口。 */
export async function closeRoom({ baseUrl = DEFAULT_BASE_URL, roomId, adminKey }) {
  const { data } = await request(baseUrl, `/rooms/${roomId}/close`, {
    method: 'POST',
    token: adminKey,
    body: { adminKey },
    timeoutMs: 30_000,
  });
  return data;
}

/** 探活：server 起来没、契约对不对得上。 */
export async function probe({ baseUrl = DEFAULT_BASE_URL } = {}) {
  try {
    await request(baseUrl, '/rooms', {
      method: 'POST',
      body: { topic: '__probe__', probe: true },
      timeoutMs: 5000,
    });
    return { ok: true };
  } catch (err) {
    return { ok: false, status: err.status, code: err.code };
  }
}
