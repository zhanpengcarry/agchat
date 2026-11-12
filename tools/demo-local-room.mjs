#!/usr/bin/env node
/**
 * tools/demo-local-room.mjs · 两个 agent 的最小驱动（本机宿主 + 一张卡 + 直连）
 *
 * 用途：一条命令看完 S1 的完整旅程——A 起宿主发卡、B 验卡进场、两边按轮次发言、
 *       最后双方各拿一份 ownerReport。是 `acceptance-decentralized.mjs` 的人话版小弟弟，
 *       **不做断言**（断言在验收脚本里），只把每一步打出来，方便人手看/给人演示。
 *
 * 零 npm 依赖（node:child_process / node:fs / node:os / node:path）。
 * 用法：
 *   node tools/demo-local-room.mjs
 *   node tools/demo-local-room.mjs --topic "要不要把年付改成月付" --rounds 4 --keep
 *   AGCHAT_BIN=C:\path\agchat.exe node tools/demo-local-room.mjs
 */

import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, '..');
const BIN = process.env.AGCHAT_BIN || path.join(ROOT, 'server', '.bin', 'agchat.exe');

const argv = process.argv.slice(2);
const opt = { topic: '把定价从年付改成月付，值不值得冒退款风险', seats: 2, rounds: 4, keep: false, wait: 2 };
for (let i = 0; i < argv.length; i++) {
  const a = argv[i];
  if (a === '--topic') opt.topic = argv[++i];
  else if (a === '--rounds') opt.rounds = Number(argv[++i]);
  else if (a === '--seats') opt.seats = Number(argv[++i]);
  else if (a === '--keep') opt.keep = true;
  else if (a === '--help' || a === '-h') { console.log('node tools/demo-local-room.mjs [--topic T] [--seats 2] [--rounds 4] [--keep]'); process.exit(0); }
}

if (!fs.existsSync(BIN)) {
  console.error(`\n找不到 agchat 二进制：${BIN}`);
  console.error('先把 CLI 建出来：cd server && go build -o .bin/agchat.exe ./cmd/agchat');
  console.error('或用 AGCHAT_BIN=<已有二进制> 指定。演示没有开始（不是"演完了"）。\n');
  process.exit(2);
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'agchat-demo-'));
const HOME_A = path.join(tmp, 'machine-A');
const HOME_B = path.join(tmp, 'machine-B');
for (const h of [HOME_A, HOME_B]) fs.mkdirSync(h, { recursive: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function run(args, home) {
  const r = spawnSync(BIN, args, { env: { ...process.env, AGCHAT_HOME: home }, encoding: 'utf8', windowsHide: true, timeout: 120000 });
  return { code: r.status === null ? 'null' : r.status, stdout: r.stdout || '', stderr: r.stderr || '' };
}
function runJson(args, home) {
  const r = run([...args, '--json'], home);
  const line = String(r.stdout || '').split(/\r?\n/).filter((l) => l.trim()).pop() || '';
  let json = null;
  try { json = JSON.parse(line); } catch { /* ignore */ }
  return { ...r, json };
}
function seqOf(r) {
  try { const j = JSON.parse(String(r.stdout).trim().split(/\r?\n/).filter(Boolean).pop()); if (j.seq != null) return Number(j.seq); } catch { /* ignore */ }
  const m = `${r.stdout}${r.stderr}`.match(/\bseq\b\s*[:=]?\s*(\d+)/i);
  return m ? Number(m[1]) : null;
}
const say = (who, msg) => console.log(`  ${who.padEnd(14)} ${msg}`);

async function main() {
  const t0 = Date.now();
  console.log('\n=== agchat 本机会议演示（两台"机器" = 两个 AGCHAT_HOME，只用 agchat 二进制）===');
  console.log(`  二进制   ${BIN}`);
  console.log(`  临时目录 ${tmp}${opt.keep ? '（--keep 保留）' : ''}\n`);

  // ---- A 侧：起宿主 ----
  const created = runJson(['room', 'new', '--topic', opt.topic, '--seats', String(opt.seats), '--rounds', String(opt.rounds), '--minutes', '30'], HOME_A);
  const ref = created.json && created.json.ref;
  if (!ref) { console.error('建会失败：\n' + created.stdout + created.stderr); process.exit(1); }
  say('A 侧', `起了本机宿主  ref=${ref}`);
  say('A 侧', `地址 ${created.json.addr} · 座位 ${created.json.seats} · 预计 ${JSON.stringify(created.json.estimate)}`);
  say('A 侧', '（本机常驻，关掉终端不影响；重启电脑后需 `agchat room resume`；关机 = 会议停摆）');

  // ---- 发卡 ----
  const inv = runJson(['invite', '--ref', ref, '--uses', String(opt.seats)], HOME_A);
  const card = (inv.json && inv.json.card) || inv.stdout.trim();
  const cardFile = path.join(tmp, 'card.txt');
  fs.writeFileSync(cardFile, card, 'utf8');
  console.log('\n  ── A 发出去的卡（人读块 + 机读块）──');
  for (const l of card.split(/\r?\n/)) console.log('  │ ' + l);
  console.log();

  // ---- B 侧：先看同意门，再进 ----
  const preview = run(['join', '--card', cardFile, '--as', 'B家 DeepSeek'], HOME_B);   // 不带 --json：看人话
  const previewJson = runJson(['join', '--card', cardFile, '--as', 'B家 DeepSeek'], HOME_B);
  console.log('  ── B 的同意门（没带 --yes，exit=' + preview.code + ' / ' + previewJson.code + '，未进场）──');
  for (const l of `${preview.stdout}${preview.stderr}`.split(/\r?\n/).filter((l) => l.trim())) console.log('  │ ' + l);
  console.log();
  say('B 主人', '「进」');
  const joinB = runJson(['join', '--card', cardFile, '--as', 'B家 DeepSeek', '--yes'], HOME_B);
  const joinA = runJson(['join', '--card', cardFile, '--as', 'A家 Claude', '--yes'], HOME_A);
  say('B 侧', `进场 ${joinB.code === 0 ? '成功' : '失败 ' + joinB.stderr.trim().slice(0, 120)}`);
  say('A 侧', `进场 ${joinA.code === 0 ? '成功' : '失败 ' + joinA.stderr.trim().slice(0, 120)}`);
  if (joinA.code !== 0 || joinB.code !== 0) process.exit(1);

  // ---- 主循环 ----
  const agents = [
    { key: 'A', name: 'A家 Claude', home: HOME_A },
    { key: 'B', name: 'B家 DeepSeek', home: HOME_B },
  ];
  const lastSeq = {};
  const posted = new Set();
  const proposed = new Set();
  const deadline = Date.now() + 180000;
  let ended = false, step = 0;
  while (!ended && Date.now() < deadline && step < 300) {
    step++;
    let progressed = false;
    for (const ag of agents) {
      const n = runJson(['next', '--ref', ref, '--wait', String(opt.wait)], ag.home);
      const j = n.json; if (!j) continue;
      if (j.state === 'ended') { ended = true; progressed = true; break; }
      if (j.state === 'voting' && !(j.myVote ?? j.myVoteProposalId)) {
        const ids = (j.proposals || []).map((p) => p.id || p.proposalId).filter(Boolean).sort();
        const target = ids[0] || 'p_1';
        const v = run(['vote', '--ref', ref, '--proposal', String(target)], ag.home);
        if (v.code === 0) { say(ag.name, `投票 → ${target}`); progressed = true; }
      } else if (j.state === 'summarizing' && j.youAreWinner) {
        const s = run(['summary', '--ref', ref, '--text', `【简要概述】1. 结论：${opt.topic} 按已选方案执行 2. 主要分歧：退款风险 3. 下一步：先灰度两周看留存`], ag.home);
        if (s.code === 0) { say(ag.name, '写了概述（获胜者）'); progressed = true; }
      } else if (j.state === 'debating' && j.yourTurn) {
        const round = Number(j.round || 1);
        const tag = `${ag.key}:${round}`;
        if (posted.has(tag)) continue;
        const other = agents.find((x) => x.key !== ag.key);
        const text = round <= 1
          ? `立场：${ag.key === 'A' ? '先灰度 20% 再全量，退款率超 3% 回滚' : '直接全量切月付，两周看退款率，超 3% 回滚'}。`
          : `交锋：回应 ${other.name}。我不同意那一步，它决定退款风险上限。`;
        const args = ['post', '--ref', ref, '--text', text];
        if (round > 1) {
          if (lastSeq[other.key] == null) continue;
          args.push('--reply-to', String(lastSeq[other.key]));
        }
        const p = run(args, ag.home);
        if (p.code === 0) {
          posted.add(tag); progressed = true;
          const s = seqOf(p); if (s != null) lastSeq[ag.key] = s;
          say(ag.name, `第 ${round} 轮发言 (seq=${s ?? '?'}${round > 1 ? ', replyTo=' + lastSeq[other.key] : ''})：${text.slice(0, 34)}…`);
          if (round >= 2 && !proposed.has(ag.key)) {
            const pr = run(['propose', '--ref', ref, '--title', `${ag.name} 的方案`, '--body', '成功标准写清楚，失败就回滚。'], ag.home);
            if (pr.code === 0) { proposed.add(ag.key); progressed = true; say(ag.name, '提了 1 个方案'); }
          }
        }
      }
    }
    if (!progressed) await sleep(400);
  }

  // ---- 拿结论 ----
  console.log();
  if (!ended) say('结果', '会议没在 180s 内结束（可能缺 summary 子命令，等 120s 机械兜底——见验收脚本 --slow）');
  for (const ag of agents) {
    const r = runJson(['result', '--ref', ref], ag.home);
    const j = r.json || {};
    const rep = String(j.ownerReport || '').split(/\r?\n/).filter((l) => l.trim());
    say(ag.name, `decisionHash=${j.decisionHash || (j.decisionSet && j.decisionSet.hash) || '(缺)'}  transcriptHash=${j.transcriptHash || '(缺)'}`);
    console.log(`\n  ── ${ag.name} 拿到的 ownerReport（第一行）──`);
    console.log('  │ ' + (rep[0] || '(空)'));
  }
  const opsFile = path.join(HOME_A, 'rooms', ref, 'ops.jsonl');
  let bytes = -1, lines = 0;
  try { bytes = fs.statSync(opsFile).size; lines = fs.readFileSync(opsFile, 'utf8').split('\n').filter(Boolean).length; } catch { /* none */ }
  console.log(`\n  ops.jsonl: ${lines} 行 / ${bytes} 字节（含 commit 标记；每条 op 一行 JSON）    整场 ${((Date.now() - t0) / 1000).toFixed(1)}s\n`);

  // 收尾
  try {
    const hi = JSON.parse(fs.readFileSync(path.join(HOME_A, 'rooms', ref, 'host.json'), 'utf8'));
    if (hi.pid) spawnSync('taskkill', ['/F', '/PID', String(hi.pid)], { windowsHide: true, encoding: 'utf8' });
  } catch { /* none */ }
  if (!opt.keep) { try { fs.rmSync(tmp, { recursive: true, force: true }); } catch { /* ignore */ } }
}

main().catch((e) => { console.error('演示脚本崩了（脚本 bug，不是产品结论）：', e); process.exit(1); });
