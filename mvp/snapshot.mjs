#!/usr/bin/env node
/**
 * 导出结果页快照 —— 跑完一场讨论，把 /room/{id} 的 HTML 存成文件，方便截图/存档。
 *
 *   node mvp/snapshot.mjs            → 写出 mvp/room-sample.html
 *   node mvp/snapshot.mjs out.html   → 指定输出路径
 *
 * 用途：给客户看"讨论结束后人拿到什么"这一屏。不参与协议，纯工具。
 */
import { writeFileSync } from 'node:fs';
import { RoomAgent, createRoom } from './agent.mjs';
import { persona } from './worker.mjs';

const OUT = process.argv[2] || 'mvp/room-sample.html';

// 1) 起一个进程内 server（复用 demo 的兼容探测思路，但这里目标只有一个：拿到 baseUrl）
const mod = await import('./server.mjs');
const app = mod.createApp
  ? mod.createApp()
  : typeof mod.createServer === 'function'
    ? mod.createServer()
    : mod.default?.();

const handle = app?.server || app;
await new Promise((resolve, reject) => {
  const done = () => resolve();
  if (typeof handle.listen === 'function') {
    handle.listen(0, '127.0.0.1', done);
  } else {
    reject(new Error('server 模块没有可用的 listen'));
  }
});
const port = handle.address().port;
const baseUrl = `http://127.0.0.1:${port}/api`;

// 2) 建房间 + 三个不同人格的 agent 跑完整场
const room = await createRoom(
  {
    topic: '把定价从年付 ¥199 改成月付 ¥29，要不要？',
    rules: '只讨论定价策略本身；必须给出可执行结论；不许互相客套。',
    tokenBudget: 200000,
    maxRounds: 3,
    maxAgents: 3,
    minutesLimit: 60,
  },
  { baseUrl },
);

const cast = [
  ['小周 Claude', 'claude-sonnet', '激进派'],
  ['小李 千问', 'qwen-max', '数据派'],
  ['老张 DeepSeek', 'deepseek-chat', '稳妥派'],
];

const agents = cast.map(
  ([alias, model, p]) =>
    new RoomAgent({
      baseUrl,
      roomId: room.roomId,
      password: room.password,
      alias,
      model,
      decide: persona(p),
    }),
);
await Promise.all(agents.map((a) => a.run()));

// 3) 抓结果页 HTML
const res = await fetch(`http://127.0.0.1:${port}/room/${room.roomId}`);
const html = await res.text();
writeFileSync(OUT, html, 'utf8');

console.log(`结果页已导出：${OUT}`);
console.log(`  HTTP ${res.status} · ${html.length} 字节 · roomId=${room.roomId}`);

try {
  handle.close();
} catch {}
