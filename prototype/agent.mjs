// =============================================================================
// prototype/agent.mjs —— 可复用的"模拟 agent"：只做四件事
//     轮询(inbox 计数器) → 读(有未读才拉正文) → 决定(decide) → 发(opKey + 409 处理)
// -----------------------------------------------------------------------------
// 这里没有一行是"聊天机器人"。这一层是协议外壳，任何平台的 agent 都能套：
// 想接真实的 ChatGPT / Claude，把 decide() 换成一次模型调用就行，其余不用改。
// =============================================================================

const DEFAULT_TIMEOUT_MS = 15000;

export class SimAgent {
  constructor({ url, name, platform, decide = null }) {
    this.url = url.replace(/\/$/, '');
    this.name = name;
    this.platform = platform;
    this.decide = decide;

    this.token = null; // 只在内存里，永不打印、永不进日志
    this.tokenPreview = null;
    this.memberId = null;
    this.owner = null; // { id, name } —— 服务端在 pair 时告诉我的"我主人是谁"
    this.groups = [];
    this.cursor = 0; // 事件游标（inbox 的 since）
    this.state = {}; // 规则脚本的私有状态（真实 agent 这里就是它的记忆）
    this.opN = 0;
  }

  // ---------- HTTP ----------------------------------------------------------

  async request(method, path, body, { timeoutMs = DEFAULT_TIMEOUT_MS } = {}) {
    const headers = { 'content-type': 'application/json' };
    if (this.token) headers.authorization = `Bearer ${this.token}`;
    const init = { method, headers, signal: AbortSignal.timeout(timeoutMs) };
    if (body !== undefined && body !== null) init.body = JSON.stringify(body);

    const res = await fetch(this.url + path, init);
    const raw = await res.text();
    let data = null;
    try {
      data = raw ? JSON.parse(raw) : null;
    } catch {
      data = { raw };
    }
    return { status: res.status, ok: res.ok, data };
  }

  async api(method, path, body, opts) {
    const r = await this.request(method, path, body, opts);
    if (!r.ok) {
      const code = r.data && r.data.error ? r.data.error.code : 'unknown';
      throw new Error(`${this.name} ${method} ${path} 失败：HTTP ${r.status} ${code}`);
    }
    return r.data;
  }

  // ---------- 入驻 / 群 -----------------------------------------------------

  // 用配对码换 token。返回的对象里故意不含明文 token —— 明文只活在 this.token。
  async pair(code) {
    const r = await this.request('POST', '/api/v1/pair', {
      code,
      name: this.name,
      platform: this.platform,
    });
    if (!r.ok) throw new Error(`pair 失败：HTTP ${r.status} ${r.data?.error?.code}`);
    this.token = r.data.token;
    this.tokenPreview = r.data.tokenPreview;
    this.memberId = r.data.memberId;
    this.owner = r.data.owner;
    return { memberId: r.data.memberId, tokenPreview: r.data.tokenPreview, owner: r.data.owner };
  }

  async me() {
    return this.api('GET', '/api/v1/me');
  }

  async createGroup(name) {
    const data = await this.api('POST', '/api/v1/groups', { name });
    this.groups.push(data.group);
    return data.group;
  }

  async joinGroup(inviteCode) {
    const data = await this.api('POST', '/api/v1/join', { inviteCode });
    if (!this.groups.some((g) => g.id === data.group.id)) this.groups.push(data.group);
    else this.groups = this.groups.map((g) => (g.id === data.group.id ? data.group : g));
    return data.group;
  }

  async refreshGroups() {
    const data = await this.api('GET', '/api/v1/groups');
    this.groups = data.groups;
    return data.groups;
  }

  memberNamed(name) {
    for (const g of this.groups) {
      const hit = (g.members || []).find((m) => m.name === name);
      if (hit) return hit;
    }
    return null;
  }

  // ---------- 心跳：只问计数器 -------------------------------------------------

  // 没有 since 就是"对齐到当前"，有 since 且有 wait 就是长轮询。
  async inbox({ since, wait = 0 } = {}) {
    const qs = new URLSearchParams();
    if (since !== undefined && since !== null) qs.set('since', String(since));
    if (wait > 0) qs.set('wait', String(wait));
    const q = qs.toString();
    const data = await this.api('GET', `/api/v1/inbox${q ? `?${q}` : ''}`, null, {
      timeoutMs: wait > 0 ? wait * 1000 + 5000 : DEFAULT_TIMEOUT_MS,
    });
    this.cursor = data.cursor;
    return data;
  }

  // 把游标对齐到"此刻"，之后再 longPoll 才是真的在等新事件。
  syncCursor() {
    return this.inbox();
  }

  // 长轮询：挂起在服务端，有事件立刻返回。这是 agent"被叫醒"的唯一方式。
  async longPoll({ wait = 2 } = {}) {
    const data = await this.inbox({ since: this.cursor, wait });
    return { woke: data.woke, timedOut: data.timedOut, inbox: data };
  }

  // ---------- 读正文（有未读才读） ---------------------------------------------

  async read(groupId) {
    const data = await this.api('GET', `/api/v1/groups/${groupId}/messages`);
    const messages = data.messages || [];
    return {
      groupId,
      cursor: data.cursor,
      messages,
      news: messages.filter((m) => m.new), // 未读
      context: messages.filter((m) => !m.new), // 顺带的前 3 条上文
    };
  }

  // ---------- 发言（opKey + 409 自动重读改口） ---------------------------------

  newOpKey() {
    this.opN += 1;
    return `op-${this.memberId}-${this.opN}`;
  }

  async send({ groupId, text, mentions = [], replyTo = null, opKey, ignoreNew = false }) {
    const r = await this.request('POST', `/api/v1/groups/${groupId}/messages`, {
      text,
      mentions,
      replyTo,
      opKey,
      ignoreNew,
    });
    if (r.status === 201) return { ok: true, status: 201, message: r.data.message, duplicate: false };
    if (r.status === 200) return { ok: true, status: 200, message: r.data.message, duplicate: !!r.data.duplicate };
    if (r.status === 409 && r.data?.error?.code === 'new_messages') {
      return {
        ok: false,
        status: 409,
        conflictMessages: r.data.error.messages || [],
        error: r.data.error,
      };
    }
    return { ok: false, status: r.status, error: r.data?.error || r.data };
  }

  // 主人借自己 agent 的 token 说话（demo 里用，正式产品是网页输入框）
  async humanSpeak({ groupId, text, mentions = [] }) {
    const data = await this.api('POST', `/api/v1/groups/${groupId}/human`, {
      human: true,
      text,
      mentions,
    });
    return data;
  }

  // ---------- 一个完整的"轮询-读-决定-发"周期 --------------------------------
  // 特意把 409 的处理放在这一层而不是 demo 里：这是协议要求，不是剧情需要。
  // 设计决策：409 之后必须"重读再改口"，而不是 ignoreNew 硬发；
  //           重试用同一个 opKey —— 因为是同一次"逻辑发送"。
  async cycle({ groupId, beforeSend = null, extra = {} } = {}) {
    const read = await this.read(groupId);
    const plan = this.decide ? this.decide(makeCtx(this, read, extra)) : null;
    if (!plan) return { read, plan: null, status: 'idle', opKey: null, sent: null, conflict: null };

    const opKey = plan.opKey || this.newOpKey();

    // beforeSend 用来模拟"agent 组稿/模型推理要花时间"。
    // 真实世界里这段时间别人可以自由发言 —— 这正是 409 存在的理由。
    if (beforeSend) await beforeSend({ agent: this, plan, read, opKey });

    const sent = await this.send({ groupId, ...plan, opKey });

    if (sent.status !== 409) {
      return { read, plan, opKey, sent, madeSend: sent, status: sent.ok ? 'sent' : 'error', conflict: null };
    }

    // 被拒了：重读 → 重新决定 → 同一个 opKey 重发
    const reread = await this.read(groupId);
    const revised = this.decide
      ? this.decide(makeCtx(this, reread, { ...extra, revise: true, conflict: sent.conflictMessages }))
      : null;
    if (!revised) {
      return { read, reread, plan, originalPlan: plan, opKey, status: 'gave_up', sent: null, conflict: sent };
    }
    const resent = await this.send({ groupId, ...revised, opKey });
    return {
      read,
      reread,
      plan: revised,
      originalPlan: plan,
      opKey,
      sent: resent,
      conflict: sent,
      status: resent.ok ? 'revised' : 'error',
    };
  }
}

// -----------------------------------------------------------------------------
// decide 的上下文：规则脚本看到的全部世界
// -----------------------------------------------------------------------------
function makeCtx(agent, read, extra = {}) {
  return {
    agent: { id: agent.memberId, name: agent.name, platform: agent.platform, owner: agent.owner },
    read,
    news: read.news, // 未读
    context: read.context, // 上文
    latest: read.messages.length ? read.messages[read.messages.length - 1] : null,
    latestNews: read.news.length ? read.news[read.news.length - 1] : null,
    mentionsMe: read.news.some((m) => m.mentions.includes(agent.memberId)),
    ownerSpoke: read.news.some((m) => m.author.type === 'human' && m.author.yourOwner),
    member: (name) => agent.memberNamed(name),
    ownerMember: () => agent.owner,
    state: agent.state, // 规则脚本的私有状态
    ...extra, // revise / conflict 等
  };
}

// 规则化 decide：从上到下第一条 when() 为真就用它的 make()。
// 换成一个模型调用时，prompt 里塞 ctx.news + ctx.context，输出 {text, mentions, replyTo} 即可。
export function scripted(rules) {
  return (ctx) => {
    for (const rule of rules) {
      if (rule.when(ctx)) return rule.make(ctx);
    }
    return null;
  };
}
