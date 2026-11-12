// 用 N 个脚本化 agent 把一场会议跑完（v2 完整流程：发言 → 提案 → 投票 → 概述）
//
//   node tools/mock-agents.mjs --base http://127.0.0.1:8788/api --room 123456789 --password XXXX --agents 4
//
// 用途：
//   1. 真人只占一个座位时，用这个补齐其他座位，方便一个人体验产品
//   2. 压测 / 回归：无人值守跑完整场
//
// 这不是"智能"agent —— 它按固定人格与轮次规则发言。真正验证 AI 可用性要用真 agent。

const arg = (k, d) => {
  const i = process.argv.indexOf('--' + k);
  return i > -1 ? process.argv[i + 1] : d;
};
const BASE = arg('base', 'http://127.0.0.1:8788/api');
const ROOM = arg('room');
const PASSWORD = arg('password');
const COUNT = Number(arg('agents', 3));
if (!ROOM || !PASSWORD) {
  console.error('用法：--room <会议号> --password <密码> [--agents 3] [--base url]');
  process.exit(2);
}

const j = async (p, o = {}) => {
  const r = await fetch(BASE + p, {
    ...o,
    headers: { 'content-type': 'application/json', ...(o.headers || {}) },
  });
  const t = await r.text();
  let b; try { b = JSON.parse(t); } catch { b = t; }
  return { status: r.status, body: b };
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 四种互相制衡的人格：必须真的对立，否则会议没有意义。
// second 是"第二选择"（必须是别人的立场）：投票时优先跨票给它，
// 只按 support 找会让 4 个 mock 各投自己 → cross 全 0 → 每次必平票，demo 永远显示"未形成多数"。
//
// ★ 稳妥派的 second 取「并行灰度」而不是「先定指标再改」：这不是随手选的——
//   四个人格两两互投会产生 2:2 的死平票（靠主席票才出结果），人一个人开会时看到的
//   永远是"未形成多数"。让稳妥派也认可"并行灰度"（对保守派来说，先灰度再决定本来就比
//   立刻全量稳妥），4 张跨票会形成 3:1，demo 才能演示出 v2.2 真正想说的那件事：
//   **多数是由跨票——别人认可你——构成的，不是靠自投堆出来的。**
const PERSONAS = [
  {
    name: '稳妥派', tag: '保守',
    line: (r) => `第 ${r} 轮：我倾向保守。先别动，把基线数据补齐再说——现在改，出了问题我们连"哪里变差了"都测不出来。`,
    support: '维持现状，先补数据',
    second: '并行灰度', // 折中派
    body: '不改。补齐基线 30 天，再决定。',
  },
  {
    name: '激进派', tag: '激进',
    line: (r) => `第 ${r} 轮：拖下去的代价没人记账。每多拖一个月，就少一批行为样本；样本本身才是资产。`,
    support: '立刻改，小步快跑',
    second: '并行灰度', // 折中派
    body: '立刻全量改，用真实数据代替空想。',
  },
  {
    name: '数据派', tag: '中立',
    line: (r) => `第 ${r} 轮：两边都在讲直觉。我要三个数：现状基线、预期变化幅度、判定成功的时间窗。一个都没有，讨论就是拼嗓门。`,
    support: '先定指标再改',
    second: '并行灰度', // 折中派
    body: '先定义三个指标和判定窗，30 天后按数据复盘。',
  },
  {
    name: '折中派', tag: '折中',
    line: (r) => `第 ${r} 轮：为什么一定是二选一？新旧并行一个月，用数据决定谁留下，成本只多一点点。`,
    support: '并行灰度',
    second: '先定指标再改', // 数据派
    body: '新旧并行 30 天，按留存决定去留。',
  },
];

const agents = [];
for (let i = 0; i < COUNT; i++) {
  const p = PERSONAS[i % PERSONAS.length];
  const r = await j(`/rooms/${ROOM}/join`, {
    method: 'POST',
    body: JSON.stringify({ password: PASSWORD, alias: `${p.name}-mock${i}`, model: 'scripted' }),
  });
  if (!r.body?.agentToken) {
    console.error(`第 ${i + 1} 个 agent 加入失败:`, r.status, JSON.stringify(r.body));
    process.exit(1);
  }
  agents.push({ ...p, id: r.body.agentId, token: r.body.agentToken, order: r.body.speakingOrder, alias: `${p.name}-mock${i}` });
}
console.log(`已加入 ${agents.length} 个 mock agent：${agents.map((a) => `${a.alias}#${a.order}`).join(', ')}`);

const auth = (a) => ({ authorization: `Bearer ${a.token}` });
// cursor 是【每个 agent 各自】的，绝不能共享 —— 共享会让先到的那个把游标推走，后面的漏事件。
const cursors = new Map(agents.map((a) => [a.id, 0]));
const proposed = new Set();
let summaryDone = false;
const deadline = Date.now() + Number(arg('max-seconds', 900)) * 1000;

for (let guard = 0; Date.now() < deadline; guard++) {
  let acted = false;

  for (const a of agents) {
    const since = cursors.get(a.id) ?? 0;
    const ib = (await j(`/rooms/${ROOM}/inbox?since=${since}&wait=2`, { headers: auth(a) })).body;
    if (!ib || ib.error) continue;
    cursors.set(a.id, ib.cursor ?? since);
    if (ib.state === 'ended') { console.log('\n会议结束'); guard = 1e9; break; }

    if (ib.state === 'voting') {
      const props = Array.isArray(ib.proposals) ? ib.proposals : (await j(`/rooms/${ROOM}/result`)).body.proposals || [];
      // 两段式投票（v2.2）：
      //   ① 先跨票——在【别人的】方案里找与自己第二选择 second 匹配的那个；
      //   ② 再自投——匹配自己的 support（v2.2 允许投给自己，但那只是次序兜底，不是共识）；
      //   ③ 都没有就退别人的第一个方案，最后 `null` 弃权。
      // 为什么不直接按 support 找：4 个人格各命中自己的方案 → 4 张全自投、cross 全 0 → 每次必平票，
      // dogfood/demo 里只会永远显示「未形成多数」，等于什么也没演示。
      const others = props.filter((p) => p.by !== a.alias);
      const crossPick = others.find((p) => p.title.includes(a.second)) || null;
      const ownPick = props.find((p) => p.title.includes(a.support)) || null;
      const pick = crossPick || ownPick || others[0] || null;
      const isSelf = Boolean(pick && pick.by === a.alias);
      const why = !pick ? '无方案可投' : isSelf ? '投自己提的方案' : crossPick ? `第二选择 ${a.second}` : '无匹配方案，改投别人的';
      const v = await j(`/rooms/${ROOM}/votes`, {
        method: 'POST', headers: auth(a),
        body: JSON.stringify(
          props.length
            ? {
                proposalId: pick ? pick.proposalId : null,
                reason: pick ? `${a.tag}立场：${why}` : '无方案可投',
                opKey: `vote-${a.id}`,
              }
            : { finalStance: 'agree', opKey: `vote-${a.id}` },
        ),
      });
      if (v.status === 201) {
        const target = pick
          ? `${pick.proposalId}「${pick.title}」${isSelf ? '（自投）' : '（跨票）'}`
          : '弃权 null';
        console.log(`${a.alias} 投给 ${target} → 票数 ${JSON.stringify(v.body.votes)}`);
        acted = true;
      } else console.log(`${a.alias} 投票失败 HTTP ${v.status} ${JSON.stringify(v.body?.error?.code || v.body)}`);
      continue;
    }

    if (ib.state === 'summarizing') {
      if (!summaryDone) {
        const res = (await j(`/rooms/${ROOM}/result`)).body;
        const w = res.conclusion?.winner;
        if (w && w.ownerAlias === a.alias) {
          await j(`/rooms/${ROOM}/summary`, {
            method: 'POST', headers: auth(a),
            body: JSON.stringify({
              text: `【简要概述】\n结论：${w.title}。\n主要分歧：${res.proposals?.filter((p) => p.proposalId !== w.proposalId).map((p) => p.title).join('、') || '无'}。\n下一步：30 天后按数据复盘。`,
              opKey: 'sum-1',
            }),
          });
          summaryDone = true;
          console.log(`${a.alias}（获胜者）已写概述`);
        }
      }
      acted = true;
      await sleep(200);
      continue;
    }

    if (ib.state === 'debating' && ib.yourTurn) {
      const msgs = (await j(`/rooms/${ROOM}/messages?since=0`, { headers: auth(a) })).body.messages || [];
      const others = msgs.filter((m) => m.agentId !== a.id);
      // 第 2 轮每人提一个方案（每个 agent 只提一次）
      if (ib.round === 2 && !proposed.has(a.id)) {
        const p = await j(`/rooms/${ROOM}/proposals`, {
          method: 'POST', headers: auth(a),
          body: JSON.stringify({ title: `${a.support}（${a.alias}）`, body: a.body, opKey: `prop-${a.id}` }),
        });
        if (p.status === 201) { proposed.add(a.id); console.log(`${a.alias} 提方案 ${p.body.proposalId}`); }
      }
      const body = { text: a.line(ib.round), opKey: `m-${a.id}-r${ib.round}` };
      if (others.length) body.replyTo = others[others.length - 1].seq;
      const s = await j(`/rooms/${ROOM}/messages`, { method: 'POST', headers: auth(a), body: JSON.stringify(body) });
      if (s.status === 201) { console.log(`${a.alias} 第 ${ib.round} 轮发言 seq=${s.body.seq}`); acted = true; }
      continue;
    }
  }

  const st = (await j(`/rooms/${ROOM}/result`)).body;
  if (st.state === 'ended') break;
  if (!acted) await sleep(200);
}

const res = (await j(`/rooms/${ROOM}/result`)).body;
console.log('\n================ 结果 ================');
console.log('状态    ', res.state);
console.log('轮数    ', res.cost?.rounds, ' 消息', res.cost?.messages, ' est_tokens', res.cost?.estTokens);
console.log('获胜    ', res.conclusion?.winner?.title ?? '(无)');
console.log('票数    ', JSON.stringify(res.conclusion?.votes));
console.log('\n' + (res.ownerReport || '(无 ownerReport)'));
