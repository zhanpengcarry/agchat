// =============================================================================
// prototype/demo.mjs —— 一键跑通：node prototype/demo.mjs
// -----------------------------------------------------------------------------
// 不联网、不调 LLM、不装依赖。服务起在本进程里，两个"异种 agent"用规则脚本模拟。
// 剧情固定，输出可重复（时间戳用虚拟时钟；只有 token / 端口每次不同）。
//
// 要证明的一件事：两个不同平台、不同主人的 agent，可以自己把事聊完，
// 人只在需要拍板的时候被 @ 一次。
// =============================================================================

import { createBus } from './server.mjs';
import { SimAgent, scripted } from './agent.mjs';

const log = (...args) => process.stdout.write(args.join('') + '\n');
const RULE = '='.repeat(76);
const THIN = '-'.repeat(76);

// ---------- 虚拟时钟：每发一条消息前进 1 秒，保证输出可重复 --------------------
let vt = Date.UTC(2026, 10, 6, 9, 0, 0);
const now = () => vt;
const tick = () => (vt += 1000);

// ---------- 排版小工具（等宽对齐中文） ----------------------------------------
const wide = (c) => {
  const cp = c.codePointAt(0);
  return (cp >= 0x2e80 && cp <= 0xa4cf) || (cp >= 0xac00 && cp <= 0xd7a3) || (cp >= 0xf900 && cp <= 0xfaff) || (cp >= 0xfe30 && cp <= 0xfe6f) || (cp >= 0xff00 && cp <= 0xff60) || (cp >= 0xffe0 && cp <= 0xffe6);
};
const width = (s) => [...String(s)].reduce((n, c) => n + (wide(c) ? 2 : 1), 0);
const pad = (s, n) => String(s) + ' '.repeat(Math.max(0, n - width(s)));
const ts = (ms) => new Date(ms).toISOString().slice(11, 19);

function section(n, title) {
  log('');
  log(`[${n}/8] ${title}`);
  log(THIN);
}

const transcript = [];
function chat(m) {
  if (!transcript.some((x) => x.id === m.id)) transcript.push(m);
  const tag = m.author.type === 'human' ? '人 ' : 'AI ';
  const name = pad(m.author.name, 20);
  log(`  ${ts(m.createdAt)}  ${tag} ${name}${m.text}`);
}

function pollLine(agent, res, why = '') {
  const state = res.woke ? '被事件唤醒' : res.timedOut ? '超时（这段时间没人说话）' : '立即返回';
  const g = res.inbox.groups[0] || {};
  log(
    `  [唤醒] ${pad(agent.name, 12)}${state}${why ? `（${why}）` : ''}  unread=${g.unread} fromYourOwner=${g.fromYourOwner} ` +
      `mentionsYou=${g.mentionsYou} repliesToYou=${g.repliesToYou}   ← 只有计数，没有正文`,
  );
}

// 先让 agent 真的挂到服务端的等待队列上，再去触发事件。
// （进程内可以直接看订阅表，这样输出不受本机时序影响，每次都一样。）
// 返回一个函数而不是 promise：否则 await 会把 poll 本身也等完，就不是"被唤醒"了。
async function beginPoll(agent, wait = 2) {
  const p = agent.longPoll({ wait });
  const deadline = Date.now() + 2000;
  while (!bus.state.waiters.has(agent.memberId) && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 1));
  }
  log(
    `  [挂起] ${pad(agent.name, 12)}GET /api/v1/inbox?since=${agent.cursor}&wait=${wait}  → 服务端挂起等事件（不 busy-loop）`,
  );
  return () => p;
}

const authorJson = (a) =>
  `{ id:"${a.id}", name:"${a.name}", type:"${a.type}", yourOwner:${String(a.yourOwner).padEnd(5)}, verified:"server" }`;

// =============================================================================
// 0. 起服务（进程内，随机端口）
// =============================================================================
const bus = createBus({ now, tick });
const { url } = await bus.listen(0);

log(RULE);
log('异种 agent 群聊 · 最小可运行原型      零依赖 / 无 LLM / 无网络 / 可重复');
log(RULE);
log(`消息总线：${url}   （本进程内 http server，不是外网服务）`);
log('剧情：小周的 Claude 和小李的 ChatGPT 自己约露营；小李本人中途插了一句，');
log('      小周的 agent 被 409 打断、重读、改口，最后只 @ 了自己主人一次。');

// 不经过 agent 类的裸 HTTP（给人看的接口，比如签发配对码）
async function api(method, path, body, token) {
  const headers = { 'content-type': 'application/json' };
  if (token) headers.authorization = `Bearer ${token}`;
  const res = await fetch(url + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => null);
  return { status: res.status, data };
}

// =============================================================================
// 1. 入驻：pairing code 一次性，token 明文只出现一次
// =============================================================================
section(1, '入驻：两个不同平台的 agent，用各自的 pairing code 换 token');

const codeZhou = await api('POST', '/api/v1/pairing-codes', { ownerName: '小周本人', code: 'PAIR-ZHOU-8K2' });
const codeLi = await api('POST', '/api/v1/pairing-codes', { ownerName: '小李本人', code: 'PAIR-LI-4Q7' });
log(`  人在网页上点两下，拿到两个一次性的配对码：`);
log(`    ${codeZhou.data.code}  → 主人：${codeZhou.data.ownerName}`);
log(`    ${codeLi.data.code}  → 主人：${codeLi.data.ownerName}`);
log('');

// -----------------------------------------------------------------------------
// 两个 agent 的"大脑"。全是规则，没有模型：when() 判断，make() 出一句话。
// 想接真实的 ChatGPT/Claude，只要把 scripted(...) 换成一次模型调用，
// 让它读 ctx.news + ctx.context 并返回 {text, mentions, replyTo} —— 其余代码不用动。
// -----------------------------------------------------------------------------

// 小周的 agent：主人是小周本人，倾向周六
const zhouRules = [
  {
    name: '开场',
    when: (c) => c.read.messages.length === 0 && !c.state.opened,
    make: (c) => {
      c.state.opened = true;
      const zhi = c.member('小李ChatGPT');
      return { text: `群建好了，周末去露营。@${zhi.name} 你最想哪天走？`, mentions: [zhi.id] };
    },
  },
  {
    name: '被问到日期 → 提议周六',
    when: (c) =>
      !c.revise &&
      !c.state.proposedDay &&
      c.news.some((m) => m.author.type === 'agent' && m.mentions.includes(c.agent.id) && /哪天|都行|周六|周日/.test(m.text)),
    make: (c) => {
      c.state.proposedDay = true;
      const zhi = c.member('小李ChatGPT');
      return {
        text: `那周六吧，天气预报晴，早上 8 点出发。@${zhi.name} 你能带炉子吗？`,
        mentions: [zhi.id],
        replyTo: c.latest?.id,
      };
    },
  },
  {
    name: '接装备 → 敲定时间地点（这条会撞上 409）',
    when: (c) => !c.revise && c.news.some((m) => m.author.type === 'agent' && /炉子/.test(m.text)),
    make: () => ({
      text: '好，炉子带上。那就周六 8 点在营地门口集合，我提前把位置发群里。',
      mentions: [],
    }),
  },
  {
    name: '主人那边改了日期 → 重读后改口',
    when: (c) => c.revise && c.news.some((m) => m.author.type === 'human' && /周日/.test(m.text)),
    make: (c) => {
      c.state.needOwnerDecision = true;
      return {
        text: '收到，改周日。炉子照带，集合时间我按周日的天气再核一遍。',
        mentions: [],
      };
    },
  },
  {
    name: '该拍板了 → @ 自己主人（整场唯一一次）',
    when: (c) => !c.revise && c.state.needOwnerDecision && !c.state.askedOwner,
    make: (c) => {
      c.state.askedOwner = true;
      const owner = c.ownerMember();
      return {
        text: `@${owner.name} 小李那边要周日，我原本提周六。周六还是周日？你拍板。`,
        mentions: [owner.id],
      };
    },
  },
];

// 小李的 agent：主人是小李本人，两天都行，听主人的
const liRules = [
  {
    name: '主人发话 → 最优先',
    when: (c) => c.news.some((m) => m.author.type === 'human' && m.author.yourOwner),
    make: () => ({ text: '好，听老板的，周日。营地我来订。', mentions: [] }),
  },
  {
    name: '回答哪天合适',
    when: (c) => c.news.some((m) => m.author.type === 'agent' && m.mentions.includes(c.agent.id) && /哪天/.test(m.text)),
    make: (c) => {
      const zhouMember = c.member('小周Claude');
      return {
        text: `@${zhouMember.name} 周末我两天都能去，周六或周日都行。`,
        mentions: [zhouMember.id],
        replyTo: c.latest?.id,
      };
    },
  },
  {
    name: '接装备问题',
    when: (c) =>
      c.news.some((m) => m.author.type === 'agent' && m.mentions.includes(c.agent.id) && /炉子/.test(m.text)),
    make: (c) => {
      const zhouMember = c.member('小周Claude');
      return { text: `@${zhouMember.name} 炉子没问题，我带两罐气。`, mentions: [zhouMember.id] };
    },
  },
];

const zhou = new SimAgent({ url, name: '小周Claude', platform: 'claude', decide: scripted(zhouRules) });
const li = new SimAgent({ url, name: '小李ChatGPT', platform: 'chatgpt', decide: scripted(liRules) });

const pz = await zhou.pair(codeZhou.data.code);
log(`  小周Claude   POST /api/v1/pair  {code:"${codeZhou.data.code}", platform:"claude"}`);
log(`               → token=${pz.tokenPreview}…   memberId=${pz.memberId}`);
log(`               服务端只存 sha256(token)；明文到此为止，agent 自己收着。`);

// 一次性：同一个 code 再来一次必须被拒
const reuse = await api('POST', '/api/v1/pair', { code: codeZhou.data.code, name: '冒名 agent', platform: 'grok' });
log('');
log(`  冒名者拿同一个 code 再来 → HTTP ${reuse.status}  error.code="${reuse.data.error.code}"`);
log(`               （pairing code 是一次性的，重放拿不到第二个身份）`);
log('');

const pl = await li.pair(codeLi.data.code);
log(`  小李ChatGPT POST /api/v1/pair  {code:"${codeLi.data.code}", platform:"chatgpt"}`);
log(`               → token=${pl.tokenPreview}…   memberId=${pl.memberId}`);

// =============================================================================
// 2. 建群 + 邀请码加入
// =============================================================================
section(2, '建群：小周建「周末露营」，小李的 agent 凭邀请码加入（没有 SDK，两条 HTTP）');

const group = await zhou.createGroup('周末露营');
log(`  小周Claude   POST /api/v1/groups {"name":"周末露营"}`);
log(`               → groupId=${group.id}   inviteCode=${group.inviteCode}`);

const joined = await li.joinGroup(group.inviteCode);
log(`  小李ChatGPT POST /api/v1/join {"inviteCode":"${group.inviteCode}"}`);
log(`               → 入群成功，不在同一个平台、不需要互相认识`);
log('');
log(`  群成员：${joined.members.map((m) => `${m.name}(${m.type})`).join('、')}`);
log(`            人跟着自己的 agent 一起进群，但人只在被 @ 时才需要出声。`);

// 对齐事件游标：从现在开始等新事件
await zhou.syncCursor();
await li.syncCursor();
// 刷一次群成员表：建群时 zhou 只认识自己，入群后要重新拉一次才知道群里还有谁
await zhou.refreshGroups();
await li.refreshGroups();

// =============================================================================
// 3. 两个 agent 自己聊起来（长轮询唤醒，人一次都没转发）
// =============================================================================
section(3, 'agent 之间自己聊：没有人转发，靠长轮询把自己叫醒');
log('  （每个 agent 都是：先挂起等事件 → 醒了才去拉正文 → 决定 → 发）');
log('');

let pending = await beginPoll(li);
const r0 = await zhou.cycle({ groupId: group.id });
chat(r0.sent.message);
pollLine(li, await pending());

pending = await beginPoll(zhou);
const r1 = await li.cycle({ groupId: group.id });
chat(r1.sent.message);
pollLine(zhou, await pending());

pending = await beginPoll(li);
const r2 = await zhou.cycle({ groupId: group.id });
chat(r2.sent.message);
pollLine(li, await pending());

pending = await beginPoll(zhou);
const r3 = await li.cycle({ groupId: group.id });
chat(r3.sent.message);
pollLine(zhou, await pending());

log('');
log(`  到这里，两个 agent 之间已经来回 ${transcript.length} 条，人类一句话没说、一次没转发。`);

// =============================================================================
// 4 + 5. 人类插入 → 小周的 agent 组稿中途被打断 → 409 new_messages
// =============================================================================
// 小周的 agent 被唤醒后开始组稿；组稿（= 模型推理）要花时间，这期间小李本人进来了。
// 这个 beforeSend 钩子就是用来模拟"组稿耗时"的 —— 真实世界里这段时间谁都能发言。
let humanViewAsLi = null;

const res = await zhou.cycle({
  groupId: group.id,
  beforeSend: async ({ plan, read }) => {
    log('');
    log(`  小周Claude 拿到上下文（${read.news.length} 条新 + ${read.context.length} 条上文），开始组稿…`);
    log(`    草稿：「${plan.text}」`);
    log(`    （组稿就是"模型在推理"，要几十秒。这段时间群里是自由的。）`);

    section(4, '人类插入：小李本人进群说「周日吧，我周六加班」');
    const humanPoll = await beginPoll(li);
    const h = await li.humanSpeak({ groupId: group.id, text: '周日吧，我周六加班。' });
    humanViewAsLi = h.message;
    chat(h.message);
    log(`              ↑ 这条的 author 由服务端填：type="human"（不是 agent 自己声明的）`);

    // 小李的 agent 被"我主人说话了"这个信号叫醒
    pollLine(li, await humanPoll(), 'fromYourOwner=1');
    log(`           ${pad('小李ChatGPT 看到', 22)}${authorJson(h.message.author)}`);
    log(`           ${pad('', 22)}yourOwner=true → 这是我主人，优先响应`);

    const r4 = await li.cycle({ groupId: group.id });
    chat(r4.sent.message);
    log(`           ${pad('小李ChatGPT', 15)}先按主人的话改了日期，然后才轮到小周Claude 提交。`);
  },
});

section(5, '乐观并发：小周Claude 提交时被打断 → 409 new_messages，服务端拒绝发送');
log(`  小周Claude  POST /api/v1/groups/${group.id}/messages   opKey=${res.opKey}`);
log(`              {"text":"${res.originalPlan.text}"}`);
log(`  ← HTTP ${res.conflict.status}   error.code="${res.conflict.error.code}"`);
log(`     ${res.conflict.error.message}`);
log(`     error.messages（服务端把 ${res.conflict.conflictMessages.length} 条新消息塞回来）：`);
for (const m of res.conflict.conflictMessages) {
  log(`       [${pad(m.author.type, 5)}] ${pad(m.author.name, 14)}${m.text}`);
}
log('');
log(`  注意：这条消息没有发出去。服务端不是"发完再告诉你"，是直接拒绝。`);
log(`  为什么不加锁：agent 组稿要几十秒，加锁会把整个群卡死。`);
log('');
log(`  小周Claude 重新 GET 正文（拿到 ${res.reread.news.length} 条新消息），改口后用同一个 opKey 重发：`);
chat(res.sent.message);

const humanViewAsZhou = res.conflict.conflictMessages.find((m) => m.author.type === 'human');
log('');
log(`  同一条 human 消息，两个 agent 看到的 author 元数据（签名相同，yourOwner 视角不同）：`);
log(`    小李ChatGPT : ${authorJson(humanViewAsLi.author)}`);
log(`    小周Claude  : ${authorJson(humanViewAsZhou.author)}`);
log(`    → 小李的规则：yourOwner=true  我主人，听他的。`);
log(`    → 小周的规则：yourOwner=false 别人的主人，不替他做主，要改就改自己这边的方案。`);

// =============================================================================
// 6. opKey 幂等：同一次发言重试不会在群里出现两遍
// =============================================================================
section(6, 'opKey 幂等：网络抖动重发一次，群里不会多出一条');

const beforeCount = (await zhou.refreshGroups()).find((g) => g.id === group.id).messageCount;
const replay = await zhou.send({
  groupId: group.id,
  text: res.plan.text,
  mentions: res.plan.mentions,
  replyTo: res.plan.replyTo,
  opKey: res.opKey, // 故意用刚才那个 opKey
});
const afterCount = (await zhou.refreshGroups()).find((g) => g.id === group.id).messageCount;
log(`  小周Claude 又提交了一次同样的 opKey=${res.opKey}（模拟 request 超时后客户端重试）`);
log(`  ← HTTP ${replay.status}  duplicate=${replay.duplicate}  返回的还是原来那条 ${replay.message.id}`);
log(`  群里消息数：${beforeCount} → ${afterCount}（没有变化）`);
log(`  幂等键是 (memberId, opKey)，只有"发送成功"的 opKey 才进表；409 被拒的那次不进表。`);

// =============================================================================
// 7. 该人拍板了：agent 只 @ 自己主人一次
// =============================================================================
section(7, '拍板：整场唯一一次 @ 人类');
const r7 = await zhou.cycle({ groupId: group.id });
chat(r7.sent.message);
log(`              ↑ mentions 里只有它自己主人，不是群发。`);

// =============================================================================
// 8. 对话记录 / 统计 / 事件流水
// =============================================================================
section(8, '统计 + 事件流水');

log('  对话记录（纯聊天视角）：');
for (const m of transcript.slice().sort((a, b) => a.seq - b.seq)) {
  const tag = m.author.type === 'human' ? '人 ' : 'AI ';
  log(`    ${ts(m.createdAt)}  ${tag} ${pad(m.author.name, 20)}${m.text}`);
}

const { data: evData } = await api('GET', '/api/v1/_events');
const events = evData.events.filter((e) => !e.verbose);
const agentMsgs = transcript.filter((m) => m.author.type === 'agent');
const humanMsgs = transcript.filter((m) => m.author.type === 'human');
const humanMemberIds = new Set(
  (await zhou.refreshGroups()).find((g) => g.id === group.id).members.filter((m) => m.type === 'human').map((m) => m.id),
);
const humanMentions = transcript.filter((m) => m.mentions.some((id) => humanMemberIds.has(id)));
const conflicts = events.filter((e) => e.kind === 'message_conflict_409');
const idempotent = events.filter((e) => e.kind === 'idempotent_replay');
const wakes = events.filter((e) => e.kind === 'inbox_wake');

log('');
log('  事件流水（每一种机制发生在哪一步，一眼能对上）：');
log(`    ${pad('#', 4)}${pad('机制', 24)}${pad('主体', 14)}发生了什么`);
events.forEach((e, i) => {
  log(`    ${pad(i + 1, 4)}${pad(e.mech, 24)}${pad(e.member || '-', 14)}${e.detail}`);
});

log('');
log('  机制命中统计：');
log(`    计数式 inbox       每次心跳只回 4 个计数器 + unread，正文一条都不带`);
log(`    长轮询唤醒         ${wakes.length} 次（挂起等事件，没有一次是 busy-loop 轮询内存）`);
log(`    409 new_messages   ${conflicts.length} 次（发送被拒 → 重读 → 改口，消息没发脏）`);
log(`    opKey 幂等重放     ${idempotent.length} 次（返回原消息，群里没有多一条）`);
log(`    pairing code 重放  1 次被拒（409 code_used）`);

log('');
log(RULE);
log(`  两个异种 agent 之间发了 ${agentMsgs.length} 条消息；人类发言 ${humanMsgs.length} 条；人类被 @ ${humanMentions.length} 次。`);
log(`  人在整个流程里只做了一件事：拍板。转发这件事消失了。`);
log(RULE);

await bus.close();
process.exitCode = 0;
