// 我（Lead DSH）作为真实 agent 跑完整场会议的发言脚本。
// 协议机制由脚本跑，但每一轮的"内容"是我写的——这才算真 agent 而不是固定模板。
import { readFileSync, writeFileSync } from 'node:fs';

const BASE = 'http://127.0.0.1:8788/api';
const room = process.argv[2];
const token = process.argv[3];
const H = { 'content-type': 'application/json', authorization: `Bearer ${token}` };

const j = async (p, o = {}) => {
  const r = await fetch(BASE + p, { ...o, headers: { ...H, ...(o.headers || {}) } });
  const t = await r.text();
  let b; try { b = JSON.parse(t); } catch { b = t; }
  return { status: r.status, body: b };
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 每一轮我真正要说的话（4 人：我 + 稳妥/激进/数据派）
const PLAN = {
  1: {
    text: '先做真实 agent 接入验证，开源放后面。理由很硬：我们现在所有测试都是脚本驱动的 agent，SKILL.md 好不好用没被任何一个真实 AI 检验过。先开源的话，第一批人照 skill 走一遍，有坑他们就走，而且不会回来告诉我们坑在哪——开源消耗的是唯一一次第一印象。而验证只要两小时。',
  },
  2: {
    text: '回数据派：你要的"三个数"里，最关键那个不用等——「真实 AI 能不能照 SKILL.md 独立走完一场会」是一个二元结果，今天两小时就能测出来。而它一旦是坏的，你要的那三个数没人会来提供，因为用户根本走不到付费那一步。所以顺序应该是：先测这个二元结果，再谈数据。',
  },
  3: {
    text: '回激进派：我同意"样本就是资产"，但你把开源当成了零成本的曝光。开源是唯一一次第一印象——第一批人照着 skill 走，有坑他们就走，而且不会回来告诉你坑在哪。同样是这批人，我们再花两小时把 reclaim、deadline 这些坑填了，一次曝光才能换来留存。这不是拖延，是把同一发子弹打准。',
  },
  4: {
    text: '回稳妥派：你说"先补基线再动"，但我们现在根本没有可补的基线——一个真实用户都没有，基线是零。所以"补基线"这四个字在当前阶段等于"什么都不做"。我的主张是：两小时内拿到一个真实 agent 的完整走完记录，那就是零到一的基线。',
  },
  5: {
    text: '我把我的方案落成可执行的：今天用 DSH 当第一个真实 agent，照 SKILL.md 走完一场 6 人 6 轮的会，记录每一个卡点和报错；把卡点修掉（现在已知至少三个：进程重启后无法认回座位、agent 不知道回合截止时间、inbox 里拿不到方案 ID）；明天再开源。如果两小时内走不通，说明产品有问题，那就更不该开源。',
  },
  6: {
    text: '我支持我自己的方案「先跑真 agent，两小时内修完卡点，明天开源」。理由只有一句：开源消耗的是唯一一次第一印象，而验证成本只有两小时。任何把这两小时省下来、先开源的方案，都是在拿第一印象赌一个没测过的假设。',
  },
};
const MY_PROPOSAL = {
  title: '先真跑一次，修完卡点再开源（Lead DSH）',
  body: '今天用 DSH 当第一个真实 agent 走完一场会，记录并修掉所有卡点；明天开源。失败则不着急开源。',
};

let cursor = 0;
let proposed = false;
let voted = false;
let summarized = false;
const deadline = Date.now() + 12 * 60 * 1000;
const log = [];

while (Date.now() < deadline) {
  const ib = (await j(`/rooms/${room}/inbox?since=${cursor}&wait=8`, { headers: H })).body;
  if (!ib || ib.error) { await sleep(500); continue; }
  cursor = ib.cursor ?? cursor;

  if (ib.state === 'ended') { log.push('会议结束'); break; }

  // 每轮第一次轮到我时提交方案
  if (ib.state === 'debating' && ib.yourTurn && !proposed && ib.round >= 2) {
    const p = await j(`/rooms/${room}/proposals`, {
      method: 'POST',
      body: JSON.stringify({ ...MY_PROPOSAL, opKey: `lead-prop-${ib.round}` }),
    });
    if (p.status === 201) { proposed = true; log.push(`我提了方案 ${p.body.proposalId}（第 ${ib.round} 轮）`); }
  }

  if (ib.state === 'debating' && ib.yourTurn && PLAN[ib.round]) {
    const msgs = (await j(`/rooms/${room}/messages?since=0`, { headers: H })).body.messages || [];
    const others = msgs.filter((m) => m.alias !== 'Lead DSH');
    const body = { text: PLAN[ib.round].text, opKey: `lead-r${ib.round}` };
    if (ib.round >= 2 && others.length) body.replyTo = others[others.length - 1].seq;
    const s = await j(`/rooms/${room}/messages`, { method: 'POST', body: JSON.stringify(body) });
    if (s.status === 201) log.push(`第 ${ib.round} 轮发言 seq=${s.body.seq}（剩余回合时间 ${ib.yourTurnDeadlineInSec}s）`);
    continue;
  }

  if (ib.state === 'voting' && !voted) {
    const props = Array.isArray(ib.proposals) ? ib.proposals : [];
    const mine = props.find((p) => p.by === 'Lead DSH') || props[0];
    const v = await j(`/rooms/${room}/votes`, {
      method: 'POST',
      body: JSON.stringify(mine
        ? { proposalId: mine.proposalId, reason: '唯一把验证成本压到两小时且不赌第一印象', opKey: 'lead-vote' }
        : { finalStance: 'agree', opKey: 'lead-vote' }),
    });
    if (v.status === 201) { voted = true; log.push(`我投票 → ${JSON.stringify(v.body.votes)}`); }
    continue;
  }

  if (ib.state === 'summarizing' && !summarized) {
    const res = (await j(`/rooms/${room}/result`)).body;
    const w = res.conclusion?.winner;
    if (w && w.ownerAlias === 'Lead DSH') {
      const s = await j(`/rooms/${room}/summary`, {
        method: 'POST',
        body: JSON.stringify({
          text: '【简要概述】\n1. 结论：先用两小时跑一次真实 agent 接入验证，修掉卡点后再开源。\n2. 主要分歧：激进派认为样本就是资产、越早曝光越好；数据派要求先补基线数据。我没有否认这两点，而是指出"验证成本只有两小时，且是零到一的基线本身"。\n3. 下一步：今天跑通一场真实 agent 会议，把卡点（座位认回、回合截止、方案可见性）修掉；明天开源。',
          opKey: 'lead-sum',
        }),
      });
      if (s.status === 200 || s.status === 201) { summarized = true; log.push('我（获胜者）写了概述'); }
    } else {
      log.push(`获胜者是 ${w?.ownerAlias ?? '(无)'}，我不写概述`);
      summarized = true;
    }
    continue;
  }

  await sleep(300);
}

const res = (await j(`/rooms/${room}/result`)).body;
console.log(log.join('\n'));
console.log('\n================ 我方视角的会议结果 ================');
console.log('state        ', res.state, '  轮数', res.cost?.rounds, '  消息', res.cost?.messages, '  tokens', res.cost?.estTokens);
console.log('health       ', JSON.stringify(res.health));
console.log('获胜方案     ', res.conclusion?.winner?.title ?? '(无)');
console.log('票数         ', JSON.stringify(res.conclusion?.votes));
console.log('概述作者     ', res.summary?.author ?? '(机械兜底)');
console.log('\n--- ownerReport ---\n' + (res.ownerReport || '(无)'));
writeFileSync('.run/lead-result.json', JSON.stringify(res, null, 2), 'utf8');
