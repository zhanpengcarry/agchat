// v2 后半段验收：10 轮 → 提案 → 投票 → 获胜者写概述 → 带回主人
const BASE = process.argv[2] || 'http://127.0.0.1:8799/api';
const HOST = BASE.replace(/\/api$/, '');

const j = async (path, opts = {}) => {
  const r = await fetch(BASE + path, {
    ...opts,
    headers: { 'content-type': 'application/json', ...(opts.headers || {}) },
  });
  const t = await r.text();
  let b; try { b = JSON.parse(t); } catch { b = t; }
  return { status: r.status, body: b };
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ok = (c, m) => console.log(`${c ? '  ✓' : '  ✗'} ${m}`);

console.log('\n【4】建一场 2 人 × 10 轮的小会，跑完整个流程');
const created = await j('/rooms', {
  method: 'POST',
  body: JSON.stringify({ topic: '要不要把定价改成月付？', maxRounds: 10, maxAgents: 2, tokenBudget: 500000 }),
});
const room = created.body;
const key = String(room.roomNo).replace(/\s/g, '');

const cast = [
  ['小周 Claude', 'claude-sonnet'],
  ['小李 千问', 'qwen-max'],
];
const agents = [];
for (const [alias, model] of cast) {
  const r = await j(`/rooms/${key}/join`, {
    method: 'POST',
    body: JSON.stringify({ password: room.password, alias, model }),
  });
  agents.push({ alias, token: r.body.agentToken, id: r.body.agentId, order: r.body.speakingOrder });
}
console.log(`  入场：${agents.map((a) => `${a.alias}(#${a.order})`).join(', ')}`);

const auth = (a) => ({ authorization: `Bearer ${a.token}` });
let cursor = 0;
let proposalsMade = 0;
let roundsSeen = 0;

// 驱动会议：每一轮找到 yourTurn 的 agent 让它发言
for (let step = 0; step < 400; step++) {
  let acted = false;
  for (const a of agents) {
    const inbox = await j(`/rooms/${key}/inbox?since=${cursor}&wait=2`, { headers: auth(a) });
    if (inbox.status !== 200) continue;
    const ib = inbox.body;
    cursor = ib.cursor;
    roundsSeen = Math.max(roundsSeen, ib.round || 0);

    if (ib.state === 'ended') break;

    if (ib.state === 'voting') {
      // 投票：投给第一个方案（或弃权）
      const res = await j(`/rooms/${key}/result`);
      const props = res.body.proposals || [];
      const choice = props.length ? props[step % props.length].proposalId : null;
      const v = await j(`/rooms/${key}/votes`, {
        method: 'POST',
        headers: auth(a),
        body: JSON.stringify(
          props.length
            ? { proposalId: choice, reason: '方案更可执行', opKey: `vote-${a.id}` }
            : { finalStance: 'agree', opKey: `vote-${a.id}` },
        ),
      });
      if (v.status === 201) console.log(`  ${a.alias} 投票 → ${v.status} ${JSON.stringify(v.body.votes)}`);
      acted = true;
      continue;
    }

    if (ib.state === 'summarizing') {
      const res = await j(`/rooms/${key}/result`);
      const w = res.body.conclusion?.winner;
      if (w && w.ownerAlias === a.alias) {
        const s = await j(`/rooms/${key}/summary`, {
          method: 'POST',
          headers: auth(a),
          body: JSON.stringify({
            text: '【简要概述】\n1. 结论：采用月付并保留年付入口。\n2. 主要分歧：续费风险承受度。\n3. 下一步：30 天后按留存复盘。',
            opKey: 'sum-1',
          }),
        });
        console.log(`  ${a.alias}（获胜者）写概述 → ${s.status}`);
      } else {
        await sleep(300);
      }
      acted = true;
      continue;
    }

    if (ib.state === 'debating' && ib.yourTurn) {
      const msgs = await j(`/rooms/${key}/messages?since=0`, { headers: auth(a) });
      const list = (msgs.body.messages || []).filter((m) => m.agentId !== a.id);
      // 前 2 轮各提一个方案
      if (ib.round >= 2 && ib.round <= 3 && proposalsMade < 2) {
        const p = await j(`/rooms/${key}/proposals`, {
          method: 'POST',
          headers: auth(a),
          body: JSON.stringify({
            title: a.alias.includes('Claude') ? '月付为主，年付保留折扣' : '先灰度 20% 用户',
            body: '按 30 天留存决定是否全量；不达标回滚。',
            opKey: `prop-${a.id}-${proposalsMade}`,
          }),
        });
        if (p.status === 201) {
          proposalsMade++;
          console.log(`  ${a.alias} 提交方案 ${p.body.proposalId}（第 ${ib.round} 轮）`);
        }
      }
      const body = {
        text: `第 ${ib.round} 轮：我支持先灰度再全量，关键看 30 天留存。`,
        opKey: `msg-${a.id}-r${ib.round}`,
      };
      if (list.length) body.replyTo = list[list.length - 1].seq;
      const sent = await j(`/rooms/${key}/messages`, { method: 'POST', headers: auth(a), body: JSON.stringify(body) });
      if (sent.status === 201) { console.log(`  ${a.alias} 第 ${ib.round} 轮发言 → seq ${sent.body.seq}`); acted = true; }
      continue;
    }
  }
  const st = await j(`/rooms/${key}/result`);
  if (st.body.state === 'ended') break;
  if (!acted) await sleep(200);
}

const res = (await j(`/rooms/${key}/result`)).body;
console.log('\n【5】结果');
console.log(`  state            ${res.state}`);
console.log(`  轮数             ${res.cost?.rounds}`);
console.log(`  消息数           ${res.cost?.messages}`);
console.log(`  选中方案         ${res.conclusion?.winner?.title ?? '(无)'}  票数 ${JSON.stringify(res.conclusion?.votes)}`);
console.log(`  结论             ${res.conclusion?.text}`);
console.log(`  概述作者         ${res.summary?.author ?? '(机械兜底)'}`);
ok(res.state === 'ended', '会议正常结束');
ok(!!res.conclusion, '有结论');
ok(!!res.summary, '有简要概述');
ok(!!res.ownerReport, '有可贴回主人的 ownerReport');
ok(!JSON.stringify(res).includes('agentToken'), '结果里不含 agentToken');
console.log('\n  ── ownerReport（agent 直接贴给主人）──');
console.log(String(res.ownerReport).split('\n').map((l) => '   ' + l).join('\n'));
console.log(`\n结果页： ${HOST}/room/${key}`);

// 可选：把结果页 HTML 落盘，便于截图
if (process.env.DUMP_HTML) {
  const html = await (await fetch(`${HOST}/room/${key}`)).text();
  const { writeFileSync } = await import('node:fs');
  writeFileSync(process.env.DUMP_HTML, html, 'utf8');
  console.log(`结果页 HTML 已导出 → ${process.env.DUMP_HTML}（${html.length} 字节）`);
}
