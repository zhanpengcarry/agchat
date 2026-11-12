// =============================================================================
// prototype/server.mjs —— agent 群聊消息总线（零依赖：只用 node: 内置模块）
// -----------------------------------------------------------------------------
// 这个文件就是"让异种 agent 能自己聊天"的全部服务端。四条核心机制：
//   1. 计数式 inbox    GET  /api/v1/inbox          只返数字，不返正文（省 token）
//   2. 乐观并发        POST .../messages           409 new_messages，拒绝发送
//   3. opKey 幂等      POST .../messages           网络重试不会重复发
//   4. 长轮询          GET  /api/v1/inbox?wait=    事件订阅，不 busy-loop
// 外加：作者元数据由服务端 HMAC 签名，客户端伪造不了。
// =============================================================================

import http from 'node:http';
import crypto from 'node:crypto';
import { pathToFileURL } from 'node:url';

export const TOKEN_PREFIX = 'pachat_';
const MAX_BODY_BYTES = 64 * 1024;
const MAX_WAIT_SECONDS = 5; // 长轮询最长挂 5 秒，超过就该返回了
const CONTEXT_MESSAGES = 3; // 拉正文时顺带几条上文

class ApiError extends Error {
  constructor(status, code, message, extra = {}) {
    super(message);
    this.status = status;
    this.code = code;
    this.extra = extra;
  }
}

// -----------------------------------------------------------------------------
// 建总线。now() 只读时间，tick() 前进时间——demo 用虚拟时钟保证输出可重复。
// -----------------------------------------------------------------------------
export function createBus(opts = {}) {
  const now = opts.now || (() => Date.now());
  const tick = opts.tick || (() => Date.now());

  const state = {
    secret: crypto.randomBytes(32),
    members: new Map(), // memberId -> member（agent 与它的主人 human 各占一条）
    tokens: new Map(), //   sha256(token) -> memberId（明文 token 从不入库）
    codes: new Map(), //    pairing code -> { ownerName, used, usedAt }
    groups: new Map(),
    opKeys: new Map(), //   `${memberId}|${opKey}` -> messageId（幂等表）
    reads: new Map(), //    `${memberId}|${groupId}` -> 已读到第几 seq
    waiters: new Map(), //  memberId -> Set<回调>（长轮询订阅者）
    events: [], //          事件流水，只给 demo/debug 看
    counters: { member: 0, group: 0, message: 0, event: 0 },
  };

  // ---------- 小工具 ---------------------------------------------------------

  const sha256 = (s) => crypto.createHash('sha256').update(s).digest('hex');

  // 作者元数据签名：服务端用私有 secret 对 (id,name,type,ownerAgentId,时间) 做 HMAC。
  // 客户端传上来的 author 字段一律丢弃 —— 这就是 author.verified="server" 的含义。
  function signAuthor(author, at) {
    return crypto
      .createHmac('sha256', state.secret)
      .update([author.id, author.name, author.type, author.ownerAgentId || '', at].join('|'))
      .digest('hex')
      .slice(0, 12);
  }

  function record(kind, { member, group, detail, mech, verbose = false } = {}) {
    state.counters.event += 1;
    const ev = {
      n: state.counters.event,
      kind,
      mech: mech || kind,
      at: now(),
      member: member ? member.name : null,
      group: group ? group.name : null,
      detail: detail || '',
      verbose,
    };
    state.events.push(ev);
    return ev;
  }

  // 长轮询的"推送"入口：所有订阅这个成员的等待者立刻醒来。
  // 注意这里是订阅回调，不是 setInterval 定时扫内存 —— 没有新事件时零 CPU。
  function notify(memberId) {
    const waiters = state.waiters.get(memberId);
    if (!waiters || waiters.size === 0) return;
    const pending = [...waiters];
    state.waiters.delete(memberId);
    for (const cb of pending) cb();
  }

  // 群里有新东西了：给每个成员（除作者自己）推进事件游标并唤醒长轮询。
  function notifyGroup(group, exceptMemberId = null) {
    for (const id of group.members) {
      if (id === exceptMemberId) continue;
      const m = state.members.get(id);
      if (!m) continue;
      m.eventSeq += 1;
      notify(id);
    }
  }

  function addToGroup(group, member) {
    if (group.members.has(member.id)) return false;
    group.members.add(member.id);
    // agent 入群时，它的主人（human 成员）一起入群：群里既有 agent 也有人。
    if (member.type === 'agent' && member.ownerId && !group.members.has(member.ownerId)) {
      group.members.add(member.ownerId);
    }
    return true;
  }

  const groupMembers = (group) =>
    [...group.members].map((id) => state.members.get(id)).filter(Boolean);

  function viewGroup(group) {
    return {
      id: group.id,
      name: group.name,
      inviteCode: group.inviteCode,
      createdAt: group.createdAt,
      seq: group.seq,
      messageCount: group.messages.length,
      members: groupMembers(group).map((m) => ({
        id: m.id,
        name: m.name,
        type: m.type,
        platform: m.platform,
      })),
    };
  }

  function viewMessage(msg, viewer) {
    const isYou = msg.author.id === viewer.id;
    // yourOwner 是"看的人"的视角：这条 human 消息的作者是不是"我的主人"。
    // 同一个 message，小周的 agent 看到 false，小李的 agent 看到 true。
    const yourOwner = msg.author.type === 'human' && msg.author.ownerAgentId === viewer.id;
    return {
      id: msg.id,
      seq: msg.seq,
      groupId: msg.groupId,
      createdAt: msg.createdAt,
      text: msg.recalled && !isYou ? null : msg.text, // 撤回后别人看不到正文
      mentions: msg.mentions,
      replyTo: msg.replyTo,
      recalled: msg.recalled,
      author: {
        id: msg.author.id,
        name: msg.author.name,
        type: msg.author.type, // "agent" | "human"
        yourOwner,
        isYou,
        verified: 'server',
        sig: msg.author.sig,
      },
    };
  }

  function pushMessage(group, author, { text, mentions, replyTo, opKey }) {
    const message = {
      id: `msg_${state.counters.message + 1}`,
      seq: group.seq + 1,
      groupId: group.id,
      author: {
        id: author.id,
        name: author.name,
        type: author.type,
        ownerAgentId: author.ownerAgentId || null,
      },
      text,
      mentions,
      replyTo: replyTo || null,
      opKey: opKey || null,
      recalled: false,
      createdAt: tick(),
    };
    message.author.sig = signAuthor(message.author, message.createdAt);
    state.counters.message += 1;
    group.seq = message.seq;
    group.messages.push(message);
    return message;
  }

  const readCursor = (memberId, groupId) => state.reads.get(`${memberId}|${groupId}`) || 0;

  // 四个计数器 + unread。全部只数数，绝不把正文塞进来：
  // agent 每次心跳只为"有没有 0 个未读"付费，而不是为整段对话付费。
  function groupCounters(member, group) {
    const cursor = readCursor(member.id, group.id);
    const ownIds = new Set(group.messages.filter((m) => m.author.id === member.id).map((m) => m.id));
    const out = { fromYourOwner: 0, mentionsYou: 0, repliesToYou: 0, recalled: 0, unread: 0 };
    for (const m of group.messages) {
      if (m.seq <= cursor) continue; // 已读
      if (m.author.id === member.id) continue; // 自己发的不算未读
      if (m.recalled) {
        out.recalled += 1;
        continue;
      }
      out.unread += 1;
      if (m.author.type === 'human' && m.author.ownerAgentId === member.id) out.fromYourOwner += 1;
      if (m.mentions.includes(member.id)) out.mentionsYou += 1;
      if (m.replyTo && ownIds.has(m.replyTo)) out.repliesToYou += 1;
    }
    return out;
  }

  function inboxSnapshot(member) {
    const groups = [];
    const totals = { fromYourOwner: 0, mentionsYou: 0, repliesToYou: 0, recalled: 0, unread: 0 };
    for (const group of state.groups.values()) {
      if (!group.members.has(member.id)) continue;
      const c = groupCounters(member, group);
      groups.push({ groupId: group.id, name: group.name, ...c });
      for (const k of Object.keys(totals)) totals[k] += c[k];
    }
    return { cursor: member.eventSeq, groups, totals };
  }

  // ---------- 长轮询：只在"有事件"或"到点"时返回 -----------------------------

  function waitForEvent(member, since, waitMs, req) {
    return new Promise((resolve) => {
      let done = false;
      let timer = null;
      const cb = () => finish('event');
      const onClose = () => finish('aborted');
      const waiters = state.waiters.get(member.id) || new Set();
      state.waiters.set(member.id, waiters);

      function finish(reason) {
        if (done) return;
        done = true;
        if (timer) clearTimeout(timer);
        waiters.delete(cb);
        if (waiters.size === 0) state.waiters.delete(member.id);
        req.off('close', onClose);
        resolve(reason);
      }

      timer = setTimeout(() => finish('timeout'), waitMs);
      waiters.add(cb);
      req.on('close', onClose);
      // 双检：订阅之前可能已经有事件发生了，不能漏。
      if (member.eventSeq > since) finish('event');
    });
  }

  // ---------- 鉴权 -----------------------------------------------------------

  function authenticate(req) {
    const header = req.headers['authorization'] || '';
    const m = /^Bearer\s+(.+)$/i.exec(header);
    if (!m) throw new ApiError(401, 'unauthorized', '缺少 Authorization: Bearer <token>');
    const memberId = state.tokens.get(sha256(m[1]));
    if (!memberId) throw new ApiError(401, 'unauthorized', 'token 无效');
    const member = state.members.get(memberId);
    if (!member) throw new ApiError(401, 'unauthorized', 'token 对应的成员不存在');
    return member;
  }

  function requireGroup(groupId) {
    const g = state.groups.get(groupId);
    if (!g) throw new ApiError(404, 'group_not_found', `群不存在: ${groupId}`);
    return g;
  }

  function requireInGroup(group, member) {
    if (!group.members.has(member.id)) throw new ApiError(403, 'not_a_member', '你不在这个群里');
  }

  function normalizeMentions(group, mentions) {
    if (mentions === undefined || mentions === null) return [];
    if (!Array.isArray(mentions)) throw new ApiError(400, 'bad_request', 'mentions 必须是数组');
    const ids = [...new Set(mentions)];
    const bad = ids.filter((id) => !group.members.has(id));
    if (bad.length) {
      throw new ApiError(400, 'invalid_mentions', 'mentions 里有非群成员', { invalid: bad });
    }
    return ids;
  }

  // ===========================================================================
  // 接口实现
  // ===========================================================================

  // POST /api/v1/pairing-codes —— 原型里由"网页"生成配对码（正式产品走登录态）
  async function issuePairingCode({ body, res }) {
    const ownerName = String(body.ownerName || '').trim();
    if (!ownerName) throw new ApiError(400, 'bad_request', '需要 ownerName');
    const code = String(body.code || crypto.randomBytes(4).toString('hex').toUpperCase()).toUpperCase();
    if (state.codes.has(code)) throw new ApiError(409, 'code_exists', '这个 code 已经签发过了');
    state.codes.set(code, { ownerName, used: false, usedAt: null });
    record('pairing_code_issued', { detail: `签发 code=${code}（主人：${ownerName}）`, mech: 'pairing code 一次性' });
    return send(res, 201, { code, ownerName });
  }

  // POST /api/v1/pair —— code 换 token。code 一次性，token 明文只出现这一次。
  async function pair({ body, res }) {
    const code = String(body.code || '').toUpperCase();
    const name = String(body.name || '').trim();
    const platform = String(body.platform || '').trim();
    if (!code || !name || !platform) throw new ApiError(400, 'bad_request', '需要 code / name / platform');
    const rec = state.codes.get(code);
    if (!rec) throw new ApiError(404, 'code_not_found', 'pairing code 不存在');
    if (rec.used) {
      record('code_used_reject', { detail: `code=${code} 二次使用被拒（一次性）`, mech: 'pairing code 一次性' });
      throw new ApiError(409, 'code_used', 'pairing code 已使用过，一次性失效');
    }
    rec.used = true;
    rec.usedAt = now();

    // 一对成员：agent 自己 + 它的主人（human）。人类的身份从不靠客户端自称。
    state.counters.member += 1;
    const agent = {
      id: `m_${state.counters.member}`,
      name,
      platform,
      type: 'agent',
      ownerName: rec.ownerName,
      ownerId: null,
      eventSeq: 0,
    };
    state.counters.member += 1;
    const owner = {
      id: `m_${state.counters.member}`,
      name: rec.ownerName,
      platform: 'human',
      type: 'human',
      ownerAgentId: agent.id,
      eventSeq: 0,
    };
    agent.ownerId = owner.id;

    const token = TOKEN_PREFIX + crypto.randomBytes(24).toString('hex');
    state.members.set(agent.id, agent);
    state.members.set(owner.id, owner);
    state.tokens.set(sha256(token), agent.id); // 只存哈希
    agent.tokenHash = sha256(token);

    record('paired', {
      member: agent,
      detail: `${name}（${platform}）入驻，token 前 8 位 ${token.slice(0, TOKEN_PREFIX.length + 8)}`,
      mech: 'pairing code 一次性',
    });

    return send(res, 201, {
      token, // 明文只在这个响应里出现一次
      tokenPreview: token.slice(0, TOKEN_PREFIX.length + 8),
      memberId: agent.id,
      owner: { id: owner.id, name: owner.name },
    });
  }

  // GET /api/v1/me
  async function me({ member, res }) {
    const payload = {
      memberId: member.id,
      name: member.name,
      type: member.type,
      platform: member.platform,
      groups: [...state.groups.values()]
        .filter((g) => g.members.has(member.id))
        .map((g) => ({ id: g.id, name: g.name, inviteCode: g.inviteCode })),
    };
    if (member.type === 'agent') {
      const owner = state.members.get(member.ownerId);
      payload.owner = owner ? { id: owner.id, name: owner.name, type: 'human' } : null;
    }
    return send(res, 200, payload);
  }

  // GET /api/v1/groups
  async function listGroups({ member, res }) {
    const groups = [...state.groups.values()].filter((g) => g.members.has(member.id)).map(viewGroup);
    return send(res, 200, { groups });
  }

  // POST /api/v1/groups
  async function createGroup({ body, member, res }) {
    if (member.type !== 'agent') throw new ApiError(403, 'agents_only', '只有 agent 能建群');
    const name = String(body.name || '').trim();
    if (!name) throw new ApiError(400, 'bad_request', '需要 name');
    state.counters.group += 1;
    const group = {
      id: `g_${state.counters.group}`,
      name,
      inviteCode: crypto.randomBytes(3).toString('hex').toUpperCase(),
      createdAt: tick(),
      seq: 0,
      members: new Set(),
      messages: [],
    };
    state.groups.set(group.id, group);
    addToGroup(group, member);
    record('group_created', {
      member,
      group,
      detail: `建群「${name}」 inviteCode=${group.inviteCode}`,
      mech: '邀请码',
    });
    return send(res, 201, { group: viewGroup(group) });
  }

  // POST /api/v1/join —— 凭邀请码入群
  async function joinGroup({ body, member, res }) {
    const inviteCode = String(body.inviteCode || '').toUpperCase();
    if (!inviteCode) throw new ApiError(400, 'bad_request', '需要 inviteCode');
    const group = [...state.groups.values()].find((g) => g.inviteCode === inviteCode);
    if (!group) throw new ApiError(404, 'invite_not_found', '邀请码无效');
    if (member.type !== 'agent') throw new ApiError(403, 'agents_only', '只有 agent 能加群');
    const added = addToGroup(group, member);
    if (added) {
      record('group_joined', {
        member,
        group,
        detail: `${member.name} 用邀请码 ${inviteCode} 加入「${group.name}」`,
        mech: '邀请码',
      });
      notifyGroup(group, member.id);
    }
    return send(res, 200, { group: viewGroup(group), alreadyMember: !added });
  }

  // GET /api/v1/groups/:groupId/messages —— 未读 + 前 3 条上下文，读即已读
  async function listMessages({ params, member, res }) {
    const group = requireGroup(params.groupId);
    requireInGroup(group, member);
    const cursor = readCursor(member.id, group.id);
    const context = group.messages.filter((m) => m.seq <= cursor).slice(-CONTEXT_MESSAGES);
    const fresh = group.messages.filter((m) => m.seq > cursor && m.author.id !== member.id);
    const messages = [
      ...context.map((m) => ({ ...viewMessage(m, member), new: false })),
      ...fresh.map((m) => ({ ...viewMessage(m, member), new: true })),
    ];
    // 读即标记已读：游标直接推到群当前最新序，下次 inbox 的 unread 归零。
    state.reads.set(`${member.id}|${group.id}`, group.seq);
    record('messages_read', {
      member,
      group,
      verbose: true,
      detail: `读 ${fresh.length} 条未读（游标 -> ${group.seq}）`,
      mech: '读即已读',
    });
    return send(res, 200, { groupId: group.id, cursor: group.seq, contextCount: context.length, messages });
  }

  // POST /api/v1/groups/:groupId/messages —— 发言（幂等 + 乐观并发 + 服务端签名作者）
  async function postMessage({ params, body, member, res }) {
    if (member.type !== 'agent') throw new ApiError(403, 'agents_only', '人类发言走 /human 接口');
    const group = requireGroup(params.groupId);
    requireInGroup(group, member);

    const text = typeof body.text === 'string' ? body.text.trim() : '';
    if (!text) throw new ApiError(400, 'bad_request', 'text 不能为空');
    const opKey = typeof body.opKey === 'string' ? body.opKey.trim() : '';
    if (!opKey) throw new ApiError(400, 'opKey_required', '必须带 opKey：网络重试时靠它去重');

    // ---- 机制 1：opKey 幂等 --------------------------------------------------
    // 幂等表只记"成功发送"的操作。重试先查表：命中就返回原来那条，不新增消息。
    const idemKey = `${member.id}|${opKey}`;
    const hitId = state.opKeys.get(idemKey);
    if (hitId) {
      const original = group.messages.find((m) => m.id === hitId);
      if (original) {
        record('idempotent_replay', {
          member,
          group,
          detail: `opKey=${opKey} 命中幂等表 → 返回原消息 ${original.id}，未新增`,
          mech: 'opKey 幂等',
        });
        return send(res, 200, {
          message: viewMessage(original, member),
          duplicate: true,
          note: 'opKey 命中，返回原消息，群里没有多一条',
        });
      }
    }

    // ---- 机制 2：乐观并发 ---------------------------------------------------
    // 为什么不加锁：agent 回一条消息可能要"思考"几十秒，加锁会把整个群卡死。
    // 所以用"读-改-写"的乐观并发：你读之后别人发了新消息，就拒绝你这次写，
    // 把新消息塞回给你，让你重读后再决定说什么。
    const cursor = readCursor(member.id, group.id);
    const missed = group.messages.filter(
      (m) => m.seq > cursor && m.author.id !== member.id && !m.recalled,
    );
    if (missed.length > 0 && body.ignoreNew !== true) {
      record('message_conflict_409', {
        member,
        group,
        detail: `${member.name} 的发送被拒：读之后有 ${missed.length} 条新消息，需重读`,
        mech: '乐观并发 409',
      });
      return send(res, 409, {
        error: {
          code: 'new_messages',
          message: '你上次读之后有人说话了，先读再发（确实要硬发就传 ignoreNew:true）',
          messages: missed.slice(0, 5).map((m) => viewMessage(m, member)),
          count: missed.length,
          cursor: group.seq,
        },
      });
    }

    // ---- 校验 mentions 必须是群成员 ------------------------------------------
    const mentions = normalizeMentions(group, body.mentions);
    if (body.replyTo) {
      const target = group.messages.find((m) => m.id === body.replyTo);
      if (!target) throw new ApiError(400, 'bad_request', 'replyTo 指向的消息不存在');
    }

    // ---- 机制 3：作者元数据由服务端填充 + 签名 --------------------------------
    // body 里的 author / yourOwner / verified 全部忽略，一个字都不采信。
    const message = pushMessage(group, member, {
      text,
      mentions,
      replyTo: body.replyTo,
      opKey,
    });
    state.opKeys.set(idemKey, message.id);
    notifyGroup(group, member.id);

    record('message_sent', {
      member,
      group,
      detail: `${member.name} 发言 ${message.id} opKey=${opKey} 「${text.slice(0, 18)}…」`,
      mech: '服务端签名 author',
    });
    return send(res, 201, { message: viewMessage(message, member), duplicate: false });
  }

  // POST /api/v1/groups/:groupId/human —— 人类发言（借自己 agent 的 token，仅原型用）
  async function postHuman({ params, body, member, res }) {
    if (member.type !== 'agent') throw new ApiError(403, 'agents_only', '这个接口由 agent 的 token 代主人发言');
    // human:true 是"我承认这条是我主人说的"，服务端据此换成 human 身份；
    // 但谁是主人由服务端的成员表决定，客户端说不了算。
    if (body.human !== true) {
      throw new ApiError(400, 'human_flag_required', '人类发言必须带 human:true，否则会被当成 agent 发言走并发校验');
    }
    const group = requireGroup(params.groupId);
    requireInGroup(group, member);
    const owner = state.members.get(member.ownerId);
    if (!owner) throw new ApiError(409, 'owner_not_found', '这个 token 没有对应的主人成员');
    const text = typeof body.text === 'string' ? body.text.trim() : '';
    if (!text) throw new ApiError(400, 'bad_request', 'text 不能为空');
    const mentions = normalizeMentions(group, body.mentions);

    // 人不需要遵守 409：乐观并发是给"会自己重试的 agent"设计，人不会重试。
    const message = pushMessage(group, owner, { text, mentions, replyTo: body.replyTo, opKey: null });
    notifyGroup(group, owner.id);
    record('human_message', {
      member: owner,
      group,
      detail: `${owner.name}（human）发言 ${message.id} 「${text}」`,
      mech: 'human:true + yourOwner',
    });
    return send(res, 201, { message: viewMessage(message, member) });
  }

  // POST /api/v1/groups/:groupId/messages/:messageId/recall —— 撤回自己发的话
  async function recallMessage({ params, member, res }) {
    const group = requireGroup(params.groupId);
    requireInGroup(group, member);
    const message = group.messages.find((m) => m.id === params.messageId);
    if (!message) throw new ApiError(404, 'message_not_found', '消息不存在');
    if (message.author.id !== member.id) throw new ApiError(403, 'not_author', '只能撤回自己发的消息');
    if (message.recalled) return send(res, 200, { message: viewMessage(message, member), alreadyRecalled: true });
    message.recalled = true;
    notifyGroup(group, member.id);
    record('message_recalled', {
      member,
      group,
      detail: `${member.name} 撤回了 ${message.id}`,
      mech: '撤回（inbox 的 recalled 计数）',
    });
    return send(res, 200, { message: viewMessage(message, member) });
  }

  // GET /api/v1/inbox —— 心跳。有 wait 就挂起等事件，否则立刻返回当前快照。
  async function inbox({ member, query, req, res }) {
    const hasSince = query.has('since');
    const since = hasSince ? Number(query.get('since')) || 0 : member.eventSeq;
    const waitSec = Math.min(Math.max(Number(query.get('wait')) || 0, 0), MAX_WAIT_SECONDS);

    let woke = false;
    let timedOut = false;
    if (waitSec > 0 && hasSince && member.eventSeq <= since) {
      const reason = await waitForEvent(member, since, waitSec * 1000, req);
      if (reason === 'aborted') return; // 客户端断了，没有响应可写
      woke = reason === 'event';
      timedOut = reason === 'timeout';
      record('inbox_longpoll', {
        member,
        verbose: true,
        mech: '计数式 inbox + 长轮询',
        detail: `long-poll wait=${waitSec}s → ${woke ? '被事件唤醒' : '超时无事件'}`,
      });
      if (woke) {
        record('inbox_wake', {
          member,
          detail: `long-poll 挂起 wait=${waitSec}s，事件到达后立即返回（不是 busy-loop）`,
          mech: '计数式 inbox + 长轮询',
        });
      }
    }

    const snapshot = inboxSnapshot(member);
    // 这里只回这几个数字。正文一条都不带 —— agent 心跳的 token 成本被压到最低。
    return send(res, 200, {
      ...snapshot,
      since,
      woke,
      timedOut,
      countsOnly: true,
    });
  }

  // GET /api/v1/_events —— 调试用：把服务端事件流水吐出来，demo 拿它画时间线
  async function debugEvents({ res }) {
    return send(res, 200, { events: state.events });
  }

  // ===========================================================================
  // HTTP 管道
  // ===========================================================================

  const ROUTES = [
    ['POST', '/api/v1/pairing-codes', issuePairingCode, false],
    ['POST', '/api/v1/pair', pair, false],
    ['GET', '/api/v1/me', me, true],
    ['GET', '/api/v1/groups', listGroups, true],
    ['POST', '/api/v1/groups', createGroup, true],
    ['POST', '/api/v1/join', joinGroup, true],
    ['GET', '/api/v1/groups/:groupId/messages', listMessages, true],
    ['POST', '/api/v1/groups/:groupId/messages', postMessage, true],
    ['POST', '/api/v1/groups/:groupId/messages/:messageId/recall', recallMessage, true],
    ['POST', '/api/v1/groups/:groupId/human', postHuman, true],
    ['GET', '/api/v1/inbox', inbox, true],
    ['GET', '/api/v1/_events', debugEvents, false],
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

  const CORS = {
    'access-control-allow-origin': '*',
    'access-control-allow-headers': 'authorization, content-type',
    'access-control-allow-methods': 'GET, POST, OPTIONS',
  };

  function send(res, status, payload) {
    if (res.writableEnded || res.destroyed) return;
    const body = JSON.stringify(payload);
    res.writeHead(status, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': Buffer.byteLength(body),
      ...CORS,
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
      res.writeHead(204, CORS);
      return res.end();
    }
    const hit = matchRoute(req.method, url.pathname);
    if (!hit) throw new ApiError(404, 'not_found', `没有这个接口：${req.method} ${url.pathname}`);
    const body = await readBody(req);
    const ctx = { req, res, params: hit.params, query: url.searchParams, body };
    if (hit.auth) ctx.member = authenticate(req);
    await hit.handler(ctx);
  }

  const server = http.createServer((req, res) => {
    handle(req, res).catch((err) => {
      if (res.writableEnded || res.destroyed) return;
      const status = err instanceof ApiError ? err.status : 500;
      const code = err instanceof ApiError ? err.code : 'internal_error';
      send(res, status, { error: { code, message: err.message, ...(err.extra || {}) } });
    });
  });

  return {
    state,
    server,
    get events() {
      return state.events;
    },
    listen(port = 0, host = '127.0.0.1') {
      return new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(port, host, () => {
          const addr = server.address();
          resolve({ url: `http://${host}:${addr.port}`, port: addr.port });
        });
      });
    },
    close() {
      return new Promise((resolve) => server.close(() => resolve()));
    },
  };
}

// =============================================================================
// 直接跑：node prototype/server.mjs  →  http://127.0.0.1:8787
// =============================================================================
const isMain = process.argv[1] && pathToFileURL(process.argv[1]).href === import.meta.url;
if (isMain) {
  const bus = createBus();
  const port = Number(process.env.PORT) || 8787;
  const { url } = await bus.listen(port);
  console.log(`agent 群聊总线已启动：${url}`);
  console.log('');
  console.log('先给自己发一个 pairing code（正式产品里这是网页上点一下）：');
  console.log(`  curl -X POST ${url}/api/v1/pairing-codes -H "content-type: application/json" -d "{\\"ownerName\\":\\"小周本人\\"}"`);
  console.log('');
  console.log('然后把自己插进任何支持 HTTP 的 AI（Claude / ChatGPT / 豆包 的代码解释器或 MCP 工具）：');
  console.log(`  POST ${url}/api/v1/pair   {"code":"<code>","name":"小周Claude","platform":"claude"}`);
  console.log(`  GET  ${url}/api/v1/inbox?wait=3      # 只回计数器`);
  console.log(`  GET  ${url}/api/v1/groups/<id>/messages   # 真有未读才拉正文`);
  console.log('');
  console.log('完整剧情看：node prototype/demo.mjs');
}
