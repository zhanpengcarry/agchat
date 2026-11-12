// v2 会议流程逐条验收：直接打 HTTP，不依赖 mvp/ 的 v1 客户端
const BASE = process.argv[2] || 'http://127.0.0.1:8799/api';
const HOST = BASE.replace(/\/api$/, '');

const j = async (path, opts = {}) => {
  const r = await fetch(BASE + path, {
    ...opts,
    headers: { 'content-type': 'application/json', ...(opts.headers || {}) },
  });
  const text = await r.text();
  let body;
  try { body = JSON.parse(text); } catch { body = text; }
  return { status: r.status, body };
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ok = (c, m) => console.log(`${c ? '  ✓' : '  ✗'} ${m}`);

// ---- 1. 建会：不传 tokenBudget，验证默认值 ----
console.log('\n【1】会议号 + 默认预算 2000w');
const created = await j('/rooms', {
  method: 'POST',
  body: JSON.stringify({ topic: '把定价从年付 ¥199 改成月付 ¥29，要不要？', maxRounds: 10, maxAgents: 6 }),
});
const room = created.body;
ok(created.status === 201, `建会 HTTP ${created.status}`);
ok(/^\d{9}$/.test(String(room.roomNo).replace(/\s/g, '')), `会议号是 9 位数字：${room.roomNo}`);
ok(room.password && room.adminKey, `密码 ${room.password} / adminKey ${String(room.adminKey).slice(0, 8)}…`);
console.log(`  返回的 estimate: ${JSON.stringify(room.estimate)}`);

// ---- 2. 六人并发进入，验证主席唯一 + 发言顺序 ----
console.log('\n【2】最多 6 个 agent，第一个进入者是主席');
const names = ['小周 Claude', '小李 千问', '老张 DeepSeek', '小王 豆包', '小陈 Kimi', '小刘 Hermes'];
const joined = await Promise.all(
  names.map((alias, i) =>
    j(`/rooms/${room.roomNo.replace(/\s/g, '')}/join`, {
      method: 'POST',
      body: JSON.stringify({ password: room.password, alias, model: `m${i}` }),
    }),
  ),
);
const agents = joined.map((r, i) => ({ alias: names[i], ...r.body }));
ok(agents.every((a) => a.agentToken), '6 人全部进入成功');
const mods = agents.filter((a) => a.isModerator);
ok(mods.length === 1, `主席恰好 1 个（${mods[0]?.alias}）`);
const orders = agents.map((a) => a.speakingOrder).sort((x, y) => x - y);
ok(JSON.stringify(orders) === JSON.stringify([1, 2, 3, 4, 5, 6]), `发言顺序是 1..6：${orders}`);
const brief = agents[0].brief;
ok(brief.prompt && brief.prompt.length > 200, `预先出示的 prompt 已下发（${brief.prompt?.length} 字）`);
console.log('\n  ── brief.prompt 前 12 行 ──');
String(brief.prompt).split('\n').slice(0, 12).forEach((l) => console.log('   ' + l));
console.log(`\n  limits: ${JSON.stringify(brief.limits)}`);
ok(brief.limits.tokenBudget === 20000000, `默认预算 = 20,000,000（2000w）：${brief.limits.tokenBudget}`);
ok(brief.limits.maxOutputTokens > 0 && brief.limits.maxMinutes > 0, '最长输出与最长时长都已下发');

// ---- 3. 满 6 人后再进一个，必须被拒 ----
const seventh = await j(`/rooms/${room.roomNo.replace(/\s/g, '')}/join`, {
  method: 'POST',
  body: JSON.stringify({ password: room.password, alias: '多余的人', model: 'x' }),
});
ok(seventh.status === 409, `第 7 人进入返回 ${seventh.status}（期望 409）`);

console.log('\n—— 以上验证了：会议号 / 默认预算 / 6 人上限 / 主席唯一 / 顺序 / 预先 prompt ——');
console.log(`房间地址： ${HOST}/room/${room.roomNo.replace(/\s/g, '')}`);
