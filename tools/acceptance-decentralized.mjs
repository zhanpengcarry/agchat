#!/usr/bin/env node
/**
 * tools/acceptance-decentralized.mjs · S1 端到端验收（本机宿主 + 邀请卡 + 直连）
 *
 * 规格唯一来源：docs/09-本地宿主规格-S1冻结.md（§3 接口 / §4 日志 / §7 CLI / §7 v1.1 增补）
 *
 * 零 npm 依赖。只用 Node 内置模块：node:child_process / node:fs / node:os / node:path
 * （另用 node:crypto 算文件指纹、node:zlib 解压并重写 bundle.zip，均为 Node 自带）。
 *
 * 做法：**模拟两台机器** = 两个独立的 AGCHAT_HOME 目录，全程只用 `agchat` 二进制，
 * 不启动任何服务器、不用 docker、不下发证书、不访问公网。
 *
 * 用法：
 *   node tools/acceptance-decentralized.mjs                    # 跑全部 AC
 *   node tools/acceptance-decentralized.mjs --only AC3,AC4     # 单跑
 *   node tools/acceptance-decentralized.mjs --keep             # 保留临时目录便于查证
 *   node tools/acceptance-decentralized.mjs --verbose          # 每条断言即时打印
 *   AGCHAT_BIN=C:\path\agchat.exe node tools/acceptance-decentralized.mjs
 *
 * 退出码：0 = 全部 PASS；1 = 有 FAIL；2 = 环境不就绪（缺二进制等，**不是跳过**）。
 *
 * 纪律：断言只按规格收紧，不为跑通放宽。跑不通就是别人没实现，报出来。
 */

import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.resolve(HERE, '..');

// ─────────────────────────── 参数 ───────────────────────────
const argv = process.argv.slice(2);
const opt = { only: null, keep: false, verbose: false, slow: false, timeout: 240000 };
for (let i = 0; i < argv.length; i++) {
  const a = argv[i];
  if (a === '--only') opt.only = String(argv[++i] || '').split(',').map((s) => s.trim().toUpperCase()).filter(Boolean);
  else if (a === '--keep') opt.keep = true;
  else if (a === '--verbose' || a === '-v') opt.verbose = true;
  else if (a === '--slow') opt.slow = true; // 给机械兜底（120s 概述超时）留时间
  else if (a === '--timeout') opt.timeout = Number(argv[++i] || 240000);
  else if (a === '--help' || a === '-h') { usage(); process.exit(0); }
  else { console.error(`未知参数: ${a}`); usage(); process.exit(2); }
}
const ALL = ['AC1', 'AC2', 'AC3', 'AC4', 'AC5', 'AC7', 'AC8'];
const want = (id) => !opt.only || opt.only.includes(id);

function usage() {
  console.log(`S1 端到端验收（本机宿主 + 邀请卡 + 直连）

  node tools/acceptance-decentralized.mjs [options]
    --only AC3,AC4   只跑指定验收项（${ALL.join(' / ')}）
    --keep           保留临时目录（会打印路径）
    --verbose        每条断言即时打印
    --slow           允许等待 120s 机械概述兜底（默认只等 15s）
    --timeout ms     单个 agchat 命令的超时（默认 240000）
  env AGCHAT_BIN     指定 agchat 二进制（默认 server/.bin/agchat.exe）
`);
}

// ─────────────────────── 结果收集 ───────────────────────
const checks = [];
const metrics = [];
const cmdLog = [];      // 只用 agchat 二进制跑的命令（AC1 的证据）
const helperLog = [];   // 仅用于采集资源数字的外部命令（Get-Process 等）

function check(ac, name, pass, detail = '') {
  if (!want(ac)) return !!pass;          // --only 时不为未选中的 AC 收行
  checks.push({ ac, name, pass: !!pass, blocked: false, detail: String(detail) });
  if (opt.verbose) console.log(`${pass ? 'PASS' : 'FAIL'} [${ac}] ${name}${detail ? ' — ' + detail : ''}`);
  return !!pass;
}
function blocked(ac, name, detail) {
  if (!want(ac)) return;
  checks.push({ ac, name, pass: false, blocked: true, detail: String(detail) });
  if (opt.verbose) console.log(`BLOCKED [${ac}] ${name} — ${detail}`);
}
function metric(name, value, note = '') {
  metrics.push({ name, value: String(value), note: String(note) });
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const now = () => Date.now();

// ─────────────────────── 二进制与执行 ───────────────────────
// 两种命名都认：go build -o 有时候带 .exe 有时候不带，别让验收因为一个后缀 BLOCKED。
const BIN_CANDIDATES = [
  process.env.AGCHAT_BIN,
  path.join(ROOT, 'server', '.bin', process.platform === 'win32' ? 'agchat.exe' : 'agchat'),
  path.join(ROOT, 'server', '.bin', 'agchat'),
  path.join(ROOT, 'server', '.bin', 'agchat.exe'),
  path.join(ROOT, 'server', 'agchat'),
  path.join(ROOT, 'server', 'agchat.exe'),
].filter(Boolean);
const FOUND = BIN_CANDIDATES.find((p) => fs.existsSync(p));
let BIN = FOUND || BIN_CANDIDATES[1];

// Windows 的 CreateProcess 只认 .exe：`go build -o .bin/agchat` 产出的无后缀文件
// 双击/直接跑都会 ENOENT（spawnSync 返回 exit=null）。这里补一个 .exe 副本。
if (FOUND && process.platform === 'win32' && !/\.exe$/i.test(FOUND)) {
  const withExe = FOUND + '.exe';
  try {
    if (!fs.existsSync(withExe)) fs.copyFileSync(FOUND, withExe);
    BIN = withExe;
  } catch { /* 复制失败就仍用原路径 */ }
}

function preflight() {
  const missing = [];
  if (!fs.existsSync(BIN)) missing.push(`找不到 agchat 二进制，试过：${BIN_CANDIDATES.join(' / ')}`);
  if (missing.length) {
    console.error('\n================ 环境不就绪，验收没有开始 ================');
    for (const m of missing) console.error('  ✗ ' + m);
    console.error('\n  这是"没跑"而不是"通过"。补齐方式：');
    console.error('    cd server && go build -o .bin/agchat ./cmd/agchat   # Windows 下产物是 .bin/agchat.exe');
    console.error('    或 AGCHAT_BIN=<已有二进制> node tools/acceptance-decentralized.mjs');
    console.error('  本次要求的验收项：' + (opt.only ? opt.only.join(',') : ALL.join(',')) + ' 全部 BLOCKED。');
    console.error('  另需 docs/09 §7 的命令：room new|status|close|resume, invite, join, next, post, propose, vote, result, export, verify');
    console.error('=========================================================\n');
    process.exit(2);
  }
  // 命令面自检（存在性，不执行副作用）：--help 里应当看到这些子命令
  const h = spawnSync(BIN, ['--help'], { encoding: 'utf8', windowsHide: true, timeout: 20000 });
  const helpText = `${h.stdout || ''}${h.stderr || ''}`;
  const need = ['room', 'invite', 'join', 'next', 'post', 'propose', 'vote', 'result', 'export', 'verify'];
  const absent = need.filter((c) => !new RegExp(`\\b${c}\\b`).test(helpText));
  if (absent.length && helpText.trim()) {
    console.error(`\n[警告] ${BIN} --help 里没看到这些子命令：${absent.join(', ')}（继续跑，缺哪个就 FAIL 哪个）\n`);
  }
}

/** 跑一条 agchat 命令（唯一被允许的二进制） */
function run(args, { home, input, timeout = 120000 } = {}) {
  const env = { ...process.env };
  if (home) env.AGCHAT_HOME = home;
  cmdLog.push({ argv: args.slice(), home });
  const r = spawnSync(BIN, args, { env, encoding: 'utf8', timeout, windowsHide: true, input });
  return {
    code: r.status === null ? 'null' : r.status,
    signal: r.signal || null,
    stdout: r.stdout || '',
    stderr: r.stderr || '',
    error: r.error ? String(r.error.message || r.error) : null,
  };
}
/** 跑一条命令并要求 stdout 是**单行 JSON**（docs/09 §7 v1.1：人话走 stderr） */
function runJson(args, opts) {
  const r = run([...args, '--json'], opts);
  const lines = String(r.stdout || '').split(/\r?\n/).filter((l) => l.trim());
  const raw = lines.length ? lines[lines.length - 1].trim() : '';
  let json = null;
  try { json = JSON.parse(raw); } catch { /* keep null */ }
  return { ...r, json, raw };
}
const txt = (r) => `${r.stdout || ''}${r.stderr || ''}`;

// ─────────────────────── 文件与指纹小工具 ───────────────────────
const opsPath = (home, ref) => path.join(home, 'rooms', ref, 'ops.jsonl');
const hostJsonPath = (home, ref) => path.join(home, 'rooms', ref, 'host.json');
const seatKeyPath = (home, ref) => path.join(home, 'seats', `${ref}.key`);

function readJsonFile(p) { try { return JSON.parse(fs.readFileSync(p, 'utf8')); } catch { return null; } }
function fileSize(p) { try { return fs.statSync(p).size; } catch { return -1; } }
function sha256File(p) { try { return 'sha256:' + crypto.createHash('sha256').update(fs.readFileSync(p)).digest('hex'); } catch { return null; } }

function opsStats(home, ref) {
  const p = opsPath(home, ref);
  const bytes = fileSize(p);
  const byOp = {};
  let lines = [];
  try { lines = fs.readFileSync(p, 'utf8').split(/\r?\n/).filter(Boolean); } catch { /* none */ }
  const indexes = [];
  const commits = [];
  let unparsable = 0;
  for (const l of lines) {
    let o;
    try { o = JSON.parse(l); } catch { unparsable++; continue; }
    if (o && typeof o.o === 'string') {           // 真正的 op 行（docs/09 §4）
      byOp[o.o] = (byOp[o.o] || 0) + 1;
      if (o.i != null) indexes.push(Number(o.i));
    } else if (o && o.commit != null) {            // 「日志先行 + commit 标记」的提交行（R-4）
      commits.push(Number(o.commit));
    } else if (o && o.intent) {                    // intent 行
      byOp['<intent>'] = (byOp['<intent>'] || 0) + 1;
    } else unparsable++;
  }
  const dupIndex = indexes.filter((v, i) => indexes.indexOf(v) !== i);
  const dupCommit = commits.filter((v, i) => commits.indexOf(v) !== i);
  const ascending = indexes.every((v, i) => i === 0 || v > indexes[i - 1]);
  return { bytes, count: lines.length, byOp, indexes, commits, dupIndex, dupCommit, ascending, unparsable };
}
function hostInfo(home, ref) { return readJsonFile(hostJsonPath(home, ref)); }

/** 从分层卡里取出机读块的 payload（用于本脚本的负向构造与隐私检查） */
function cardParts(cardText) {
  const s = String(cardText || '');
  const begin = s.indexOf('---agchat-invite-begin---');
  const end = s.indexOf('---agchat-invite-end---');
  const human = begin >= 0 ? s.slice(0, begin) : '';
  const machine = begin >= 0 && end > begin ? s.slice(begin, end + '---agchat-invite-end---'.length) : s;
  const m = s.match(/agchat-invite:v1:([A-Za-z0-9_\-\s>#&;\u200b\uff1a，。]*?)\.([A-Za-z0-9_\-\s>#&;]+)/);
  let payload = null, payloadRaw = null, sigRaw = null;
  if (m) {
    const strip = (x) => x.replace(/[^A-Za-z0-9_-]/g, '');
    payloadRaw = strip(m[1]); sigRaw = strip(m[2]);
    try { payload = JSON.parse(Buffer.from(payloadRaw, 'base64url').toString('utf8')); } catch { payload = null; }
  }
  return { human, machine, payload, payloadRaw, sigRaw, whole: s };
}

// ─────────────────────── 确定性脚本内容（AC4 对照场与中断场必须逐字一致） ───────────────────────
const SCRIPT = {
  post(agentKey, name, otherName, round) {
    return round <= 1
      ? `立场：我主张${agentKey === 'A' ? '先灰度 20% 再全量切换' : '直接全量切月付'}，因为可回滚才敢试。`
      : `交锋：回应 ${otherName} 的上一轮。我不同意那一步的判断，它会决定退款风险的上限。`;
  },
  proposal(agentKey, name) {
    return {
      title: `${name} 的${agentKey === 'A' ? '灰度' : '全量'}方案`,
      body: agentKey === 'A'
        ? '先 20% 灰度两周，退款率 >3% 回滚；成功标准：留存不降且退款率 <3%。'
        : '全量切月付，两周观察；成功标准：退款率 <3% 即继续。',
    };
  },
  summary(topic) {
    return `【简要概述】1. 结论：${topic} 按已选方案执行 2. 主要分歧：退款风险的估计 3. 下一步：先灰度两周看留存`;
  },
  voteTarget(proposals) {
    const ids = (proposals || []).map((p) => String(p.id || p.proposalId || '')).filter(Boolean).sort();
    return ids[0] || 'p_1';
  },
};

function writeCard(name, text) {
  const p = path.join(ctx.tmp, name);
  fs.writeFileSync(p, String(text), 'utf8'); // Node 默认不写 BOM
  return p;
}

// ─────────────────────── 资源采集（非 agchat 命令，仅测量） ───────────────────────
function procStats(pid) {
  if (!pid) return null;
  try {
    if (process.platform === 'win32') {
      const cmd = `$p = Get-Process -Id ${Number(pid)} -ErrorAction SilentlyContinue; if ($p) { '{0} {1}' -f $p.WorkingSet64, $p.CPU }`;
      for (const shell of ['pwsh', 'powershell']) {
        const r = spawnSync(shell, ['-NoProfile', '-NonInteractive', '-Command', cmd], { encoding: 'utf8', windowsHide: true, timeout: 20000 });
        helperLog.push({ argv: [shell, 'Get-Process', String(pid)], code: r.status });
        const out = String(r.stdout || '').trim();
        if (r.status === 0 && /^\d+\s+[\d.]+$/.test(out)) {
          const [ws, cpu] = out.split(/\s+/).map(Number);
          return { rssMB: ws / 1048576, cpuSec: cpu };
        }
      }
      const t = spawnSync('tasklist', ['/FI', `PID eq ${Number(pid)}`, '/FO', 'CSV', '/NH'], { encoding: 'utf8', windowsHide: true, timeout: 20000 });
      helperLog.push({ argv: ['tasklist', String(pid)], code: t.status });
      const mm = String(t.stdout || '').match(/"([^"]*)","([^"]*)","([^"]*)","([^"]*)","([^"]*)"/);
      if (t.status === 0 && mm) return { rssMB: Number(String(mm[4]).replace(/[^\d]/g, '')) / 1024, cpuSec: null };
    } else {
      const r = spawnSync('ps', ['-o', 'rss=,time=', '-p', String(pid)], { encoding: 'utf8', timeout: 20000 });
      helperLog.push({ argv: ['ps', String(pid)], code: r.status });
      const out = String(r.stdout || '').trim();
      if (r.status === 0 && out) {
        const [rssKb, t] = out.split(/\s+/);
        const mm = String(t || '').match(/(?:(\d+)-)?(\d+):(\d+):(\d+)/);
        const cpuSec = mm ? (Number(mm[1] || 0) * 86400 + Number(mm[2]) * 3600 + Number(mm[3]) * 60 + Number(mm[4])) : null;
        return { rssMB: Number(rssKb) / 1024, cpuSec };
      }
    }
  } catch { /* 采集失败不算断言失败 */ }
  return null;
}

function killPid(pid) {
  if (!pid) return { ok: false, detail: 'no pid' };
  try {
    if (process.platform === 'win32') {
      const r = spawnSync('taskkill', ['/F', '/PID', String(Number(pid))], { encoding: 'utf8', windowsHide: true, timeout: 20000 });
      helperLog.push({ argv: ['taskkill', String(pid)], code: r.status });
      return { ok: r.status === 0 || /not found|找不到/i.test(txt(r)), detail: txt(r).trim().slice(0, 200) };
    }
    process.kill(Number(pid), 'SIGKILL');
    return { ok: true, detail: 'SIGKILL' };
  } catch (e) { return { ok: false, detail: String(e.message || e) }; }
}

// ─────────────────────── zip 读写（只为 AC5 篡改一条 vote） ───────────────────────
const CRC_TABLE = (() => {
  const t = new Int32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c;
  }
  return t;
})();
function crc32(buf) {
  let c = -1;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ -1) >>> 0;
}
function readZip(file) {
  const b = fs.readFileSync(file);
  let eocd = -1;
  for (let i = b.length - 22; i >= 0 && i > b.length - 66000; i--) if (b.readUInt32LE(i) === 0x06054b50) { eocd = i; break; }
  if (eocd < 0) return { ok: false, reason: '不是 zip（找不到 EOCD）' };
  const total = b.readUInt16LE(eocd + 10);
  let off = b.readUInt32LE(eocd + 16);
  const entries = [];
  for (let n = 0; n < total; n++) {
    if (b.readUInt32LE(off) !== 0x02014b50) return { ok: false, reason: `中央目录第 ${n} 条损坏` };
    const method = b.readUInt16LE(off + 10);
    const compSize = b.readUInt32LE(off + 20);
    const nameLen = b.readUInt16LE(off + 28);
    const extraLen = b.readUInt16LE(off + 30);
    const commentLen = b.readUInt16LE(off + 32);
    const lho = b.readUInt32LE(off + 42);
    const name = b.toString('utf8', off + 46, off + 46 + nameLen);
    if (b.readUInt32LE(lho) !== 0x04034b50) return { ok: false, reason: `本地头损坏: ${name}` };
    const lNameLen = b.readUInt16LE(lho + 26);
    const lExtraLen = b.readUInt16LE(lho + 28);
    const dataOff = lho + 30 + lNameLen + lExtraLen;
    const comp = b.subarray(dataOff, dataOff + compSize);
    let data;
    try { data = method === 0 ? Buffer.from(comp) : zlib.inflateRawSync(comp); }
    catch { return { ok: false, reason: `解压失败: ${name} (method=${method})` }; }
    entries.push({ name, data, method });
    off += 46 + nameLen + extraLen + commentLen;
  }
  return { ok: true, entries };
}
function writeZip(file, entries) {
  const locals = [];
  const centrals = [];
  let offset = 0;
  const dosTime = 0, dosDate = 0x21; // 1980-01-01
  for (const e of entries) {
    const name = Buffer.from(e.name, 'utf8');
    const data = e.data;
    const crc = crc32(data);
    const lh = Buffer.alloc(30);
    lh.writeUInt32LE(0x04034b50, 0); lh.writeUInt16LE(20, 4); lh.writeUInt16LE(0x0800, 6);
    lh.writeUInt16LE(0, 8); lh.writeUInt16LE(dosTime, 10); lh.writeUInt16LE(dosDate, 12);
    lh.writeUInt32LE(crc, 14); lh.writeUInt32LE(data.length, 18); lh.writeUInt32LE(data.length, 22);
    lh.writeUInt16LE(name.length, 26); lh.writeUInt16LE(0, 28);
    locals.push(lh, name, data);
    const ch = Buffer.alloc(46);
    ch.writeUInt32LE(0x02014b50, 0); ch.writeUInt16LE(20, 4); ch.writeUInt16LE(20, 6);
    ch.writeUInt16LE(0x0800, 8); ch.writeUInt16LE(0, 10); ch.writeUInt16LE(dosTime, 12); ch.writeUInt16LE(dosDate, 14);
    ch.writeUInt32LE(crc, 16); ch.writeUInt32LE(data.length, 20); ch.writeUInt32LE(data.length, 24);
    ch.writeUInt16LE(name.length, 28); ch.writeUInt16LE(0, 30); ch.writeUInt16LE(0, 32);
    ch.writeUInt16LE(0, 34); ch.writeUInt16LE(0, 36); ch.writeUInt32LE(0, 38); ch.writeUInt32LE(offset, 42);
    centrals.push(ch, name);
    offset += 30 + name.length + data.length;
  }
  const cd = Buffer.concat(centrals);
  const eocd = Buffer.alloc(22);
  eocd.writeUInt32LE(0x06054b50, 0); eocd.writeUInt16LE(0, 4); eocd.writeUInt16LE(0, 6);
  eocd.writeUInt16LE(entries.length, 8); eocd.writeUInt16LE(entries.length, 10);
  eocd.writeUInt32LE(cd.length, 12); eocd.writeUInt32LE(offset, 16); eocd.writeUInt16LE(0, 20);
  fs.writeFileSync(file, Buffer.concat([...locals, cd, eocd]));
}

/** 在 bundle.zip 里改一条 vote 的载荷（保持 JSON 合法 → 必然只破坏 h/签名链） */
function tamperVoteInBundle(srcZip, dstZip) {
  const z = readZip(srcZip);
  if (!z.ok) return { ok: false, reason: z.reason };
  const opsEntry = z.entries.find((e) => /(^|\/)ops\.jsonl$/i.test(e.name));
  if (!opsEntry) return { ok: false, reason: `bundle 里没有 ops.jsonl（有：${z.entries.map((e) => e.name).join(', ')}）` };
  const raw = opsEntry.data.toString('utf8');
  const lines = raw.split('\n');
  let lineIdx = -1, op = null;
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i].trim()) continue;
    try { const o = JSON.parse(lines[i]); if (o && o.o === 'vote') { lineIdx = i; op = o; break; } } catch { /* skip */ }
  }
  if (lineIdx < 0) return { ok: false, reason: 'bundle 的 ops.jsonl 里找不到 vote' };
  const before = JSON.stringify(op);
  op.p = { ...(op.p || {}), reason: `(被验收脚本篡改) ${(op.p && op.p.reason) || ''}` };
  const after = JSON.stringify(op);
  if (before === after) return { ok: false, reason: '改不动这条 vote 的载荷' };
  lines[lineIdx] = after;
  opsEntry.data = Buffer.from(lines.join('\n'), 'utf8');
  writeZip(dstZip, z.entries);
  const i = Number(op.i);
  return { ok: true, at: Number.isFinite(i) ? i : lineIdx + 1, opIndexLine: lineIdx, names: z.entries.map((e) => e.name) };
}

// ─────────────────────── 会话上下文 ───────────────────────
const ctx = {
  tmp: null,
  A: null, B: null, C: null, // AGCHAT_HOME 目录（模拟三台机器：A 发起人 / B 被邀请人 / C 陌生机器）
  room: null,                // { ref, topic, seats, rounds, roomJson, card, cardParts, humanCard }
  meeting: null,             // AC3 结果
  startedAt: 0,
};

function mkHome(name) {
  const p = path.join(ctx.tmp, name);
  fs.mkdirSync(p, { recursive: true });
  return p;
}

// ─────────────────────── 阶段 1：开 + 卡 + 同意门 + 跑完 ───────────────────────
async function phaseCardFlow() {
  const topic = '把定价从年付改成月付，值不值得冒退款风险';
  const seats = 2, rounds = 4;

  // ---- 建会（走人话路径：A 侧第一步必须说人话，这条只能在非 JSON 输出上验）----
  const created = run(['room', 'new', '--topic', topic, '--seats', String(seats), '--rounds', String(rounds), '--minutes', '30'], { home: ctx.A, timeout: opt.timeout });
  const newHuman = txt(created);
  const refFromText = (newHuman.match(/roomRef:\s*([A-Za-z0-9_-]{43})/) || [])[1] || '';
  const addrFromText = (newHuman.match(/addr:\s*(127\.0\.0\.1:\d+|localhost:\d+|\[::1\]:\d+)/) || [])[1] || '';
  const seatsFromText = (newHuman.match(/seats:\s*(\d+)/) || [])[1];
  const roundsFromText = (newHuman.match(/maxRounds:\s*(\d+)/) || [])[1];
  const estFromText = (newHuman.match(/estimate:.*?([\d,]+)\s*est_tokens/) || [])[1];

  const roomOk = check('AC3', 'room new 成功并返回 43 字符 roomRef', created.code === 0 && refFromText.length === 43,
    `exit=${created.code} ref=${refFromText ? refFromText.slice(0, 12) + '…(' + refFromText.length + ')' : '(空)'}${created.code !== 0 ? ' stderr=' + created.stderr.trim().slice(0, 300) : ''}`);
  if (!roomOk) return;
  const ref = refFromText;
  const st = runJson(['room', 'status', '--ref', ref], { home: ctx.A, timeout: 60000 });
  const rj = st.json || {};

  check('AC8', 'A 侧第一步说人话：本机常驻 / 关机=停摆 / 需 room resume',
    /本机常驻/.test(newHuman) && /room\s+resume/i.test(newHuman),
    `原文片段=${JSON.stringify(newHuman.replace(/\s+/g, ' ').slice(0, 220))}`);
  check('AC1', '宿主只绑定 loopback', /^(127\.0\.0\.1|localhost|\[::1\]):\d+$/.test(addrFromText), `addr=${addrFromText || '(未打印)'}`);
  check('AC8', 'room new 给出了座位数/轮次/预算估计',
    Number(seatsFromText) === seats && Number(roundsFromText) === rounds && Number(String(estFromText || '0').replace(/,/g, '')) > 0,
    `seats=${seatsFromText} rounds=${roundsFromText} estimate=${estFromText}`);
  check('AC3', 'room status --json 能复核这一场会（ref/seats/maxRounds 一致）',
    !!rj.ref && rj.ref === ref && Number(rj.seats) === seats && Number(rj.maxRounds) === rounds,
    `status=${JSON.stringify({ ref: rj.ref, seats: rj.seats, maxRounds: rj.maxRounds, joined: rj.joined, state: rj.state })}`);
  const hj = hostInfo(ctx.A, ref);
  check('AC4', 'host.json 落了 rendezvous 文件（含 pid/addr）', !!(hj && hj.pid && hj.addr), `host.json=${JSON.stringify(hj)}`);

  // ---- 发卡 ----
  const inv = runJson(['invite', '--ref', ref, '--uses', String(seats)], { home: ctx.A, timeout: 60000 });
  const card = (inv.json && inv.json.card) || (inv.code === 0 ? inv.stdout.trim() : '');
  const parts = cardParts(card);
  const inviteOk = check('AC8', 'invite 打印分层卡（人读块 + begin/end 包住的机读块）',
    inv.code === 0 && parts.human.length > 0 && /---agchat-invite-begin---/.test(parts.machine) && parts.machine.includes('agchat-invite:v1:'),
    `exit=${inv.code} human=${parts.human.length}B machine=${parts.machine.replace(/\s+/g, '').length}B${inv.code !== 0 ? ' stderr=' + inv.stderr.slice(0, 200) : ''}`);
  if (!inviteOk) return;

  ctx.room = { ref, topic, seats, rounds, roomJson: rj, card, parts, humanCard: parts.human };

  // ---- AC8：人读块四要素 + 长度 + 无 URL ----
  const humanOneLine = parts.human.replace(/^\[人读块[^\]]*\]\s*/m, '').replace(/\s*\n\s*/g, ' ').trim();
  const ownerName = (parts.payload && parts.payload.on) || rj.owner || rj.on || '';
  const hostName = (parts.payload && parts.payload.hn) || rj.hostName || rj.hn || '';
  const opener = ownerName || hostName;   // 人读块首行用的是「谁开的」（payload.on）
  check('AC8', '人读块含「谁开的」', !!opener && humanOneLine.includes(opener), `opener=${opener} 行=${JSON.stringify(humanOneLine)}`);
  check('AC8', '人读块含「议题」', humanOneLine.includes(topic.slice(0, 8)), `行=${JSON.stringify(humanOneLine)}`);
  check('AC8', '人读块含「几人」', new RegExp(`${seats}\\s*人`).test(humanOneLine), `期望 ${seats} 人；行=${JSON.stringify(humanOneLine)}`);
  check('AC8', '人读块含「什么时候前有效」', /前(可进|有效)|有效期|截止|exp/i.test(humanOneLine), `行=${JSON.stringify(humanOneLine)}`);
  check('AC8', '人读块 ≤ 500 字符（IM 安全）', humanOneLine.length <= 500, `${humanOneLine.length} 字符`);
  check('AC8', '卡里没有可点击 URL（IM 预览机器人会自动 GET）', !/https?:\/\//i.test(parts.human) && !/https?:\/\/(?!127\.0\.0\.1|localhost|\[::1\])/i.test(parts.machine),
    `human 含 URL=${/https?:\/\//i.test(parts.human)}`);
  const machineToken = (parts.machine.match(/agchat-invite:v1:\S+/) || [''])[0];
  check('AC8', '机读块单行、≤1024 字符、带 topicHash 而不是长议题',
    machineToken.length > 0 && machineToken.length <= 1024 && !!(parts.payload && parts.payload.th) && String(parts.payload.th).length <= 16,
    `machine=${machineToken.length} 字符 th=${parts.payload && parts.payload.th}`);

  // ---- AC1：卡里的 endpoint 只能是自己机器 ----
  const eps = (parts.payload && Array.isArray(parts.payload.ep)) ? parts.payload.ep : [];
  const badEp = eps.filter((e) => e && e.u && !/^https?:\/\/(127\.0\.0\.1|localhost|\[::1\])(:|\/|$)/i.test(String(e.u)));
  check('AC1', '卡里没有内网/公网 endpoint（默认只写 loopback）', badEp.length === 0, `ep=${JSON.stringify(eps)}`);

  // ---- AC2：B 侧不带 --yes（人话一遍给人看，JSON 一遍给机器判）----
  const cardFile = writeCard('card-R1.txt', card);
  const opsBeforePreview = opsStats(ctx.A, ref);
  const previewHuman = run(['join', '--card', cardFile, '--as', 'B家 DeepSeek'], { home: ctx.B, timeout: 60000 });
  const preview = runJson(['join', '--card', cardFile, '--as', 'B家 DeepSeek'], { home: ctx.B, timeout: 60000 });
  const pj = preview.json || {};
  const pc = pj.consent || pj;            // 同意门字段可能在 consent 子对象里，也可能平铺
  const pvText = txt(previewHuman);
  check('AC2', '不带 --yes 时退出码 = 3（需确认，未进场）', previewHuman.code === 3 && preview.code === 3,
    `人话路径 exit=${previewHuman.code} / json 路径 exit=${preview.code}`);
  check('AC2', '不带 --yes 时机器可读地声明未进场', pj.joined === false || /"joined"\s*:\s*false/.test(preview.stdout) || /未进场/.test(`${txt(preview)}${pvText}`),
    `json=${JSON.stringify(pj).slice(0, 300)}`);
  check('AC2', '同意门报「谁开的」', !!(pc.owner || pc.hostName) && (pvText.includes(String(pc.owner || pc.hostName)) || (!!ownerName && pvText.includes(ownerName))),
    `owner=${pc.owner || pc.hostName || '(缺)'} 人话=${JSON.stringify(pvText.replace(/\s+/g, ' ').slice(0, 160))}`);
  check('AC2', '同意门报「议什么」', (pc.topic && String(pc.topic).includes(topic.slice(0, 8))) || pvText.includes(topic.slice(0, 8)), `topic=${pc.topic}`);
  check('AC2', '同意门报「预计花你多少」', !!(pc.estimate && Number(pc.estimate.tokens) > 0) || /est_tokens|预计花你/.test(pvText),
    `estimate=${JSON.stringify(pc.estimate)}`);
  check('AC2', '同意门是给人看的三行人话（不是一行 JSON）', /邀你参加|谁开的|预计花你/.test(pvText) && /进\s*\/\s*不进|要不要参加|未进场/.test(pvText) && !/^\{/.test(pvText.trim()),
    `人话片段=${JSON.stringify(pvText.replace(/\s+/g, ' ').slice(0, 200))}`);
  check('AC2', 'B 说"不进"时卡没被消耗（宿主侧 joined 计数仍是 0）',
    pc.joinedCount === 0 || pc.joined === 0 || pc.used === 0 || (!('joinedCount' in pc) && !opsStats(ctx.A, ref).byOp.join),
    `consent.joinedCount=${pc.joinedCount} ops=${JSON.stringify(opsStats(ctx.A, ref).byOp)}`);
  check('AC2', 'B 说"不进"时没落座位私钥', !fs.existsSync(seatKeyPath(ctx.B, ref)), `seats/${ref}.key 存在=${fs.existsSync(seatKeyPath(ctx.B, ref))}`);

  // ---- 双方进场（同机模拟两台机器，各自独立的 home）----
  const joinA = runJson(['join', '--card', cardFile, '--as', 'A家 Claude', '--yes'], { home: ctx.A, timeout: 60000 });
  const joinB = runJson(['join', '--card', cardFile, '--as', 'B家 DeepSeek', '--yes'], { home: ctx.B, timeout: 60000 });
  check('AC3', 'A 侧（房主）持卡进场成功', joinA.code === 0 && (joinA.json ? joinA.json.joined !== false : true),
    `exit=${joinA.code} json=${JSON.stringify(joinA.json || {}).slice(0, 200)}${joinA.code !== 0 ? ' stderr=' + joinA.stderr.slice(0, 300) : ''}`);
  check('AC3', 'B 侧 --yes 进场成功（与 A 用同一张卡的两个座位）', joinB.code === 0 && (joinB.json ? joinB.json.joined !== false : true),
    `exit=${joinB.code} json=${JSON.stringify(joinB.json || {}).slice(0, 200)}${joinB.code !== 0 ? ' stderr=' + joinB.stderr.slice(0, 300) : ''}`);
  if (joinA.code !== 0 || joinB.code !== 0) {
    check('AC3', '两方进场后才能跑会', false, '进场失败，后续会议流程无法开始（不是跳过，是 FAIL）');
    return;
  }
  check('AC3', 'B 侧有座位私钥（座位靠它认回，不靠名字）', fs.existsSync(seatKeyPath(ctx.B, ref)), `seats/<ref>.key=${seatKeyPath(ctx.B, ref)}`);

  const opsAfterJoin = opsStats(ctx.A, ref);
  const st2 = runJson(['room', 'status', '--ref', ref], { home: ctx.A, timeout: 60000 });
  check('AC2', '带 --yes 才真的进：宿主侧 joined=2', Number((st2.json || {}).joined) === seats,
    `status.joined=${(st2.json || {}).joined}（期望 ${seats}）`);
  check('AC2', '两个 join 都进了 ops.jsonl（docs/09 §4：每条 op 一行，join 也要在）',
    (opsAfterJoin.byOp.join || 0) >= seats,
    `ops.jsonl 里 join=${opsAfterJoin.byOp.join || 0}；全部 op=${JSON.stringify(opsAfterJoin.byOp)}`);

  // ---- 记录会前的 ops 体积，然后真跑 ----
  const opsBefore = opsStats(ctx.A, ref).bytes;
  const tMeeting = now();
  const played = await playMeeting({ ref, topic, seats, rounds, homes: { A: ctx.A, B: ctx.B } });
  const meetingMs = now() - tMeeting;

  // ---- AC3 断言 ----
  check('AC3', '会议真的走到 ended（不是超时退出）', played.ended, `steps=${played.steps} seen=${JSON.stringify(played.seen)}`);
  check('AC3', '立场轮发生（每人第 1 轮都发了言，无 replyTo）', played.roundsWithPosts.has(1), `有发言的轮次=${[...played.roundsWithPosts].join(',')}`);
  check('AC3', '交锋轮发生（第 2 轮起必须带 replyTo 且被接受）', played.replyToOk >= 1, `带 replyTo 成功的发言=${played.replyToOk}`);
  check('AC3', '提案发生（≥1 个方案）', played.proposals >= 1, `proposals=${played.proposals}`);
  check('AC3', '投票发生（两方都投）', played.votes >= 2, `votes=${played.votes}`);
  check('AC3', '概述一步发生', played.summaryDone || played.sawSummarizing, `mechanicalFallback=${played.mechanicalSummary} sawSummarizing=${played.sawSummarizing}`);
  const opsEnded = opsStats(ctx.A, ref);
  check('AC3', 'ops.jsonl 里有 end op 与 advance op（docs/09 §4 / arch B-14：终态快照要随 end 落盘）',
    (opsEnded.byOp.end || 0) >= 1 && (opsEnded.byOp.advance || 0) >= 1,
    `ops=${JSON.stringify(opsEnded.byOp)}`);

  const resA = runJson(['result', '--ref', ref], { home: ctx.A, timeout: 60000 });
  const resB = runJson(['result', '--ref', ref], { home: ctx.B, timeout: 60000 });
  const ra = resA.json || {}, rb = resB.json || {};
  const dhA = ra.decisionHash || (ra.decisionSet && ra.decisionSet.hash) || null;
  const thA = ra.transcriptHash || null;
  check('AC3', 'result --json 给出 decisionHash 与 transcriptHash', !!dhA && !!thA, `decisionHash=${dhA} transcriptHash=${thA}`);
  check('AC3', '两端拿到的结论指纹一致（同一场会同一结论）', !!dhA && dhA === (rb.decisionHash || (rb.decisionSet && rb.decisionSet.hash)),
    `A=${dhA} B=${rb.decisionHash || (rb.decisionSet && rb.decisionSet.hash)}`);
  const repA = String(ra.ownerReport || ''), repB = String(rb.ownerReport || '');
  const firstLine = (s) => (String(s).split(/\r?\n/).find((l) => l.trim()) || '').trim();
  const fl = firstLine(repA);
  check('AC3', 'ownerReport 第一行含「会名/议题」', fl.includes(topic.slice(0, 8)) || /会名/.test(fl), `首行=${JSON.stringify(fl)}`);
  check('AC3', 'ownerReport 第一行含「谁开的」', (!!ownerName && fl.includes(ownerName)) || (!!hostName && fl.includes(hostName)),
    `首行=${JSON.stringify(fl)} owner=${ownerName} host=${hostName}`);
  check('AC3', 'ownerReport 第一行含「结论」', /结论|决定|选出|chosen|winner/i.test(fl), `首行=${JSON.stringify(fl)}`);
  check('AC3', '双方都拿到了 ownerReport', repA.length > 50 && repB.length > 50, `A=${repA.length}字 B=${repB.length}字`);
  const ds = ra.decisionSet || {};
  const votedCount = ds.participation && (ds.participation.voted ?? ds.participation.Voted);
  check('AC3', '结论里的投票人数 = 实际参会人数（票没被吞）', Number(votedCount) === seats, `participation=${JSON.stringify(ds.participation || null)}`);
  ctx.meeting = { ref, decisionHash: dhA, transcriptHash: thA, report: repA, played, meetingMs };

  // ---- 资源数字（会前/会后）----
  const opsAfter = opsStats(ctx.A, ref);
  metric('ops.jsonl 会前字节', opsBefore, 'room new + invite + join 之后、发言之前');
  metric('ops.jsonl 会后字节', opsAfter.bytes, `${played.steps} 步 · ${opsAfter.count} 条 op · ${JSON.stringify(opsAfter.byOp)}`);
  metric('ops.jsonl 增量', opsAfter.bytes - opsBefore, `${seats} 人 ${rounds} 轮`);
  metric('整场耗时（秒）', (meetingMs / 1000).toFixed(1), '从第一次 next 到 ended');

  // ---- 空转 CPU（10s 增量）+ RSS ----
  const hi = hostInfo(ctx.A, ref);
  const pid = hi && hi.pid;
  const s1 = procStats(pid);
  if (s1) {
    metric('宿主 RSS（MB）', s1.rssMB.toFixed(1), '目标 < 100 MB（docs/09 §8.7）');
    if (s1.cpuSec != null) {
      await sleep(10000);
      const s2 = procStats(pid);
      if (s2 && s2.cpuSec != null) {
        const d = s2.cpuSec - s1.cpuSec;
        metric('空转 10s CPU 增量（秒）', d.toFixed(3), '目标 ≈ 0（ticker 1s，无长轮询不做事）');
        check('AC3', '空转时不烧 CPU（10s 增量 < 1.0 CPU 秒）', d < 1.0, `${d.toFixed(3)} CPU 秒 / 10s`);
      }
    }
  } else {
    metric('宿主 RSS（MB）', '采集失败', `pid=${pid}`);
  }

  // ---- AC5：离线可验 + 篡改 ----
  await phaseVerify(ctx.meeting.ref);
}

// ─────────────────────── 驱动一场会 ───────────────────────
async function playMeeting({ ref, topic, seats, rounds, homes, hooks = {} }) {
  const agents = [
    { key: 'A', name: 'A家 Claude', home: homes.A },
    { key: 'B', name: 'B家 DeepSeek', home: homes.B },
  ];
  const postedByRound = new Map();
  const firedRounds = new Set();
  const out = {
    ended: false, steps: 0, seen: [], roundsWithPosts: new Set(), replyToOk: 0, proposals: 0,
    votes: 0, summaryDone: false, mechanicalSummary: false, sawSummarizing: false, errors: [],
    posts: 0, lastSeq: {}, posted: new Set(), proposed: new Set(),
  };
  const hasSummaryCmd = hasSummaryCommand();
  const deadline = now() + Math.max(90000, opt.timeout);
  let step = 0;
  while (!out.ended && now() < deadline && step < 400) {
    step++; out.steps = step;
    let progressed = false;
    for (const ag of agents) {
      const n = runJson(['next', '--ref', ref, '--wait', '2'], { home: ag.home, timeout: 30000 });
      const j = n.json;
      if (!j) {
        out.errors.push(`${ag.key}: next 不可解析 exit=${n.code} ${n.stderr.trim().slice(0, 120)}`);
        continue;
      }
      if (!out.seen.includes(j.state)) out.seen.push(j.state);
      if (j.state === 'ended') { out.ended = true; progressed = true; break; }
      if (j.state === 'summarizing') {
        out.sawSummarizing = true;
        if (j.youAreWinner && hasSummaryCmd && !out.summaryDone) {
          const s = run(['summary', '--ref', ref, '--text', SCRIPT.summary(topic)], { home: ag.home, timeout: 60000 });
          if (s.code === 0) { out.summaryDone = true; progressed = true; }
          else out.errors.push(`${ag.key}: summary 被拒 exit=${s.code} ${txt(s).trim().slice(0, 120)}`);
        }
        // 没有 summary 子命令时只能等机械兜底（不假装成功，由调用方据 mechanicalSummary 判定）
      } else if (j.state === 'voting') {
        const mine = j.myVote ?? j.myVoteProposalId ?? j.vote ?? null;
        if (!mine) {
          const target = SCRIPT.voteTarget(j.proposals);
          const v = run(['vote', '--ref', ref, '--proposal', String(target)], { home: ag.home, timeout: 30000 });
          if (v.code === 0) { out.votes++; progressed = true; }
          else out.errors.push(`${ag.key}: vote 被拒 exit=${v.code} ${txt(v).trim().slice(0, 120)}`);
        }
      } else if (j.state === 'debating' && j.yourTurn) {
        const round = Number(j.round || 1);
        const other = agents.find((x) => x.key !== ag.key);
        const tag = `${ag.key}:${round}`;
        if (out.posted.has(tag)) continue;         // 同一轮不重复刷
        const round1 = round <= 1;
        const args = ['post', '--ref', ref, '--text', SCRIPT.post(ag.key, ag.name, other.name, round)];
        if (!round1) {
          const seq = out.lastSeq[other.key];
          if (seq == null) { out.errors.push(`${ag.key}: 第 ${round} 轮拿不到对方 seq，无法 replyTo`); continue; }
          args.push('--reply-to', String(seq));
        }
        const p = run(args, { home: ag.home, timeout: 30000 });
        if (p.code === 0) {
          out.posts++; out.posted.add(tag); out.roundsWithPosts.add(round);
          if (!postedByRound.has(round)) postedByRound.set(round, new Set());
          postedByRound.get(round).add(ag.key);
          if (!round1) out.replyToOk++;
          const s = parseSeq(p);
          if (s != null) out.lastSeq[ag.key] = s;
          progressed = true;
          // 提案：第 2 轮起每个 agent 只提 1 个（规则上限是每人 2 个，别把额度刷爆）
          if (round >= 2 && !out.proposed.has(ag.key)) {
            const spec = SCRIPT.proposal(ag.key, ag.name);
            const pr = run(['propose', '--ref', ref, '--title', spec.title, '--body', spec.body], { home: ag.home, timeout: 30000 });
            if (pr.code === 0) { out.proposals++; out.proposed.add(ag.key); progressed = true; }
            else out.errors.push(`${ag.key}: propose 被拒 exit=${pr.code} ${txt(pr).trim().slice(0, 120)}`);
          }
        } else {
          out.errors.push(`${ag.key}: post 被拒 round=${round} exit=${p.code} ${txt(p).trim().slice(0, 160)}`);
        }
      }
    }
    if (!progressed) await sleep(opt.slow ? 1500 : 400);
    // 一轮里所有人都发完言（且提案已提）后触发一次钩子（AC4 用它在中途 kill 宿主）
    for (const [r, set] of postedByRound) {
      if (set.size >= agents.length && !firedRounds.has(r) && typeof hooks.onRoundDone === 'function') {
        firedRounds.add(r);
        await hooks.onRoundDone(r, out);
      }
    }
  }
  if (!out.ended) {
    // 机械概述兜底需要 120s：--slow 时再等
    if (opt.slow && out.sawSummarizing) {
      const extra = now() + 130000;
      while (!out.ended && now() < extra) {
        await sleep(2000);
        for (const ag of agents) {
          const n = runJson(['next', '--ref', ref, '--wait', '2'], { home: ag.home, timeout: 30000 });
          if (n.json && n.json.state === 'ended') { out.ended = true; out.mechanicalSummary = !out.summaryDone; break; }
        }
      }
    }
  }
  return out;
}
function parseSeq(r) {
  try {
    const lines = String(r.stdout || '').split(/\r?\n/).filter((l) => l.trim());
    const last = lines[lines.length - 1];
    const j = JSON.parse(last);
    const v = j.seq ?? j.Seq ?? (j.message && j.message.seq);
    if (v != null) return Number(v);
  } catch { /* fallthrough */ }
  const m = txt(r).match(/\bseq\b\s*[:=]?\s*(\d+)/i);
  return m ? Number(m[1]) : null;
}
let _hasSummary = null;
function hasSummaryCommand() {
  if (_hasSummary !== null) return _hasSummary;
  const h = run(['--help'], { timeout: 20000 });
  const t = txt(h);
  // 子命令表里出现 summary 即认为可用；但避免把 "summarizing" 状态名误判成子命令
  _hasSummary = /\n\s*summary\b|\bsummary\s+--ref|agchat summary\b/.test(t);
  return _hasSummary;
}

// ─────────────────────── AC5：离线可验 + 篡改一条 vote ───────────────────────
async function phaseVerify(ref) {
  const out = path.join(ctx.tmp, 'bundle.zip');
  const exp = run(['export', '--ref', ref, '--out', out], { home: ctx.A, timeout: 120000 });
  check('AC5', 'agchat export 产出 bundle.zip', exp.code === 0 && fs.existsSync(out) && fileSize(out) > 0,
    `exit=${exp.code} size=${fileSize(out)}B out=${txt(exp).trim().slice(0, 160)}`);
  if (!fs.existsSync(out)) return;

  // 换一台"陌生机器"（全新 AGCHAT_HOME，没有数据库、没有座位私钥）
  const v1 = runJson(['verify', '--bundle', out], { home: ctx.C, timeout: 120000 });
  const vj = v1.json || {};
  const status1 = vj.status || vj.state || '';
  check('AC5', '陌生机器上 verify = verified（结论一致 + 转写一致 + 签名全有效）', status1 === 'verified',
    `status=${status1 || '(缺)'} code=${v1.code} json=${JSON.stringify(vj).slice(0, 400)}`);
  check('AC5', 'verify 分别给出结论/转写的比对结果', vj.decisionMatch !== undefined && vj.transcriptMatch !== undefined,
    `decisionMatch=${vj.decisionMatch} transcriptMatch=${vj.transcriptMatch}`);
  if (vj.sigsValid !== undefined || vj.sigsTotal !== undefined) {
    check('AC5', '干净的 bundle 里每一条签名都验得过（sigsValid == sigsTotal）',
      Number(vj.sigsValid) === Number(vj.sigsTotal) && Number(vj.sigsTotal) > 0,
      `sigsValid=${vj.sigsValid} sigsTotal=${vj.sigsTotal} reasons=${JSON.stringify(vj.reasons || [])}`);
  }

  // 篡改 bundle 里的一条 vote
  const bad = path.join(ctx.tmp, 'bundle-tampered.zip');
  const t = tamperVoteInBundle(out, bad);
  check('AC5', '能构造"篡改一条 vote"的 bundle（验收脚手架自检）', t.ok, t.ok ? `改了第 ${t.at} 条 op` : t.reason);
  if (!t.ok) return;
  const v2 = runJson(['verify', '--bundle', bad], { home: ctx.C, timeout: 120000 });
  const v2j = v2.json || {};
  const status2 = v2j.status || v2j.state || '';
  const at = v2j.failed_at ?? v2j.failedAt ?? v2j.failAt ?? v2j.failedOpIndex ?? null;
  check('AC5', '篡改一条 vote 后 verify = failed', status2 === 'failed', `status=${status2 || '(缺)'} json=${JSON.stringify(v2j).slice(0, 400)}`);
  const mentions = new RegExp(`\\b${t.at}\\b`).test(txt(v2));
  check('AC5', 'failed 指出是第几条 op（不是泛泛的"校验失败"）', Number(at) === Number(t.at) || mentions,
    `期望第 ${t.at} 条；failed_at=${at}；输出片段=${JSON.stringify(txt(v2).replace(/\s+/g, ' ').slice(0, 200))}`);
}

// ─────────────────────── AC7：三类卡的失败必须是三句不同的人话 ───────────────────────
async function phaseBadCards() {
  const R = ctx.room;
  if (!R) return;
  const card = R.card;

  // (a) 截断：把机读块砍掉一半
  const cutAt = card.indexOf('agchat-invite:v1:') + 30;
  const truncated = card.slice(0, cutAt);
  const a = runJson(['join', '--card', writeCard('card-truncated.txt', truncated), '--as', 'B家 DeepSeek'], { home: ctx.B, timeout: 60000 });
  const aText = txt(a);
  const sayA = /卡不完整/.test(aText);

  // (b) 篡改：改机读块 payload 里的一个字符（base64url 字母表内替换 → 结构仍合法、签名必不过）
  const mi = card.indexOf('agchat-invite:v1:') + 'agchat-invite:v1:'.length;
  let tampered = card;
  for (let i = mi + 10; i < card.length; i++) {
    const ch = card[i];
    if (/[A-Za-z0-9_-]/.test(ch) && card[i + 1] !== '.') {
      const rep = ch === 'A' ? 'B' : 'A';
      tampered = card.slice(0, i) + rep + card.slice(i + 1);
      break;
    }
  }
  const tamperedSame = tampered === card;
  const b = runJson(['join', '--card', writeCard('card-tampered.txt', tampered), '--as', 'B家 DeepSeek'], { home: ctx.B, timeout: 60000 });
  const bText = txt(b);
  const sayB = /卡对不上/.test(bText);
  check('AC7', '脚手架确实改到了卡里的一个字符', !tamperedSame, tamperedSame ? '一个字符都没改动（卡太短或格式不符）' : 'ok');

  // (c) 另一场会的卡：另起一场会，拿它的卡去 B 的 home（B 已经在这场会里了）
  let sayC = false, cText = '', cExit = null;
  const r2 = runJson(['room', 'new', '--topic', '另一场完全不相干的会', '--seats', '2', '--rounds', '2', '--minutes', '10'], { home: ctx.A, timeout: 120000 });
  const ref2 = (r2.json && r2.json.ref) || '';
  if (ref2) {
    const inv2 = runJson(['invite', '--ref', ref2, '--uses', '1'], { home: ctx.A, timeout: 60000 });
    const card2 = (inv2.json && inv2.json.card) || (inv2.code === 0 ? inv2.stdout.trim() : '');
    const card2File = writeCard('card-other-room.txt', card2);
    // 第三类失败（验签通过但 ref 与当前会话不符）需要一个显式入口；这里把可能的写法都试一遍，
    // 哪种生效都能 PASS，一种都不生效就 FAIL 并把尝试记录打出来（接口缺口，不是脚本瞎猜）。
    const attempts = [
      ['join', '--card', card2File, '--ref', R.ref, '--as', 'B家 DeepSeek'],   // Lead 定的入口：--ref 与卡不符 → 第三类
      ['join', '--card', card2File, '--as', 'B家 DeepSeek'],
      ['join', '--card', card2File, '--expect-ref', R.ref, '--as', 'B家 DeepSeek'],
    ];
    const tried = [];
    for (const args of attempts) {
      const c = runJson(args, { home: ctx.B, timeout: 60000 });
      const t = txt(c);
      tried.push(`[${args.slice(2).join(' ').replace(card2File, '<card>')}] exit=${c.code} → ${t.replace(/\s+/g, ' ').slice(0, 110)}`);
      cText += '\n' + t; cExit = c.code;
      if (/另一场会/.test(t)) { sayC = true; break; }
    }
    if (!sayC) cText = tried.join('\n');
  }

  check('AC7', '截断的卡被拒，且说「卡不完整…请重发」', a.code !== 0 && sayA, `exit=${a.code} 片段=${JSON.stringify(aText.replace(/\s+/g, ' ').slice(0, 180))}`);
  check('AC7', '被改的卡被拒，且说「卡对不上…不要用它」', b.code !== 0 && sayB, `exit=${b.code} 片段=${JSON.stringify(bText.replace(/\s+/g, ' ').slice(0, 180))}`);
  check('AC7', '另一场会的卡被拒，且说「这是另一场会的卡」', sayC, `exit=${cExit} 尝试记录=${JSON.stringify(cText.replace(/\s+/g, ' ').slice(0, 260))}`);
  check('AC7', '第三类的退出码与「截断/被改」两类不同（三类可被机器区分）',
    sayC && cExit != null && cExit !== a.code && cExit !== b.code,
    `截断 exit=${a.code} / 被改 exit=${b.code} / 另一场会 exit=${cExit}`);
  check('AC7', '三句人话互不相同（不是同一句"验签失败"复读）',
    sayA && sayB && sayC && !/卡不完整/.test(bText) && !/卡不完整/.test(cText) && !/卡对不上/.test(cText),
    `A=${sayA} B=${sayB} C=${sayC}`);
  check('AC7', '判定顺序是「先结构、再验签、最后比身份」（截断不会报成被改）',
    sayA && !/卡对不上/.test(aText), `截断响应=${JSON.stringify(aText.replace(/\s+/g, ' ').slice(0, 160))}`);
}

// ─────────────────────── AC4：断点恢复，结论指纹不变 ───────────────────────
async function phaseResume() {
  const topic = '断点恢复对照：同一份发言与投票，kill 宿主后 resume 结论指纹是否一致';
  const mkRoom = () => {
    const c = runJson(['room', 'new', '--topic', topic, '--seats', '2', '--rounds', '3', '--minutes', '30'], { home: ctx.A, timeout: 120000 });
    const ref = (c.json && c.json.ref) || '';
    if (!ref) return null;
    const inv = runJson(['invite', '--ref', ref, '--uses', '2'], { home: ctx.A, timeout: 60000 });
    const card = (inv.json && inv.json.card) || (inv.code === 0 ? inv.stdout.trim() : '');
    const ja = runJson(['join', '--card', card, '--as', 'A家 Claude', '--yes'], { home: ctx.A, timeout: 60000 });
    const jb = runJson(['join', '--card', card, '--as', 'B家 DeepSeek', '--yes'], { home: ctx.B, timeout: 60000 });
    if (ja.code !== 0 || jb.code !== 0) return { ref, fail: `join A=${ja.code} B=${jb.code} ${ja.stderr}${jb.stderr}` };
    return { ref };
  };
  const fp = (home, ref) => {
    const r = runJson(['result', '--ref', ref], { home, timeout: 60000 });
    const j = r.json || {};
    return {
      decisionHash: j.decisionHash || (j.decisionSet && j.decisionSet.hash) || null,
      transcriptHash: j.transcriptHash || null,
      ended: (j.decisionSet && j.decisionSet.status && j.decisionSet.status !== 'in_progress') || !!j.decisionHash,
    };
  };

  // ---- 对照场：不杀 ----
  const ctl = mkRoom();
  if (!ctl || ctl.fail) { check('AC4', '对照场（不 kill）能开起来', false, ctl ? ctl.fail : 'room new 失败'); return; }
  const ctlPlay = await playMeeting({ ref: ctl.ref, topic, seats: 2, rounds: 3, homes: { A: ctx.A, B: ctx.B } });
  const ctlFp = fp(ctx.A, ctl.ref);
  check('AC4', '对照场跑完并给出指纹', ctlPlay.ended && !!ctlFp.decisionHash && !!ctlFp.transcriptHash,
    `ended=${ctlPlay.ended} decisionHash=${ctlFp.decisionHash} transcriptHash=${ctlFp.transcriptHash}`);

  // ---- 中断场：同一套驱动 + 同一套内容，在第 2 轮发完后 kill 宿主，再 resume 跑完 ----
  const brk = mkRoom();
  if (!brk || brk.fail) { check('AC4', '中断场能开起来', false, brk ? brk.fail : 'room new 失败'); return; }
  const hi = hostInfo(ctx.A, brk.ref);
  const oldPid = hi && hi.pid;
  check('AC4', '能从来宾文件读到宿主 pid', !!oldPid, `host.json=${JSON.stringify(hi)}`);
  if (!oldPid) return;

  let killedAtRound = 0, killProof = null, resumeProof = null, newPid = null;
  const brkPlay = await playMeeting({
    ref: brk.ref, topic, seats: 2, rounds: 3, homes: { A: ctx.A, B: ctx.B },
    hooks: {
      onRoundDone: async (round) => {
        if (round !== 2 || killedAtRound) return;
        killedAtRound = round;
        await sleep(400);
        const k = killPid(oldPid);
        const stillAlive = !!procStats(oldPid);
        killProof = { ok: k.ok && !stillAlive, alive: stillAlive, detail: k.detail };
        // resume 不在 docs/09 的 --json 保证清单里：先用裸命令，再拿 status --json 核对 ref
        const resume = run(['room', 'resume', '--ref', brk.ref], { home: ctx.A, timeout: 120000 });
        const st = runJson(['room', 'status', '--ref', brk.ref], { home: ctx.A, timeout: 60000 });
        const hi2 = hostInfo(ctx.A, brk.ref);
        newPid = hi2 && hi2.pid;
        resumeProof = {
          code: resume.code,
          ref: (st.json && (st.json.ref || st.json.roomRef)) || null,
          text: txt(resume).trim().slice(0, 200),
          statusText: txt(st).trim().slice(0, 200),
        };
        await sleep(800);
      },
    },
  });
  const brkFp = fp(ctx.A, brk.ref);
  check('AC4', '宿主进程被真的杀掉（kill -9 / taskkill /F）', !!(killProof && killProof.ok),
    killProof ? `taskkill=${killProof.ok} 进程还在=${killProof.alive} ${killProof.detail}` : '没走到 kill（第 2 轮没发完）');
  check('AC4', `agchat room resume 退出码 0（在第 ${killedAtRound || '?'} 轮后）`, !!(resumeProof && resumeProof.code === 0),
    resumeProof ? `exit=${resumeProof.code} ${resumeProof.text}` : 'resume 没执行');
  check('AC4', 'resume 没有另开一场会（roomRef 不变）', !!(resumeProof && (resumeProof.ref === brk.ref || resumeProof.ref == null)),
    `resume.ref=${resumeProof && resumeProof.ref}`);
  check('AC4', 'resume 后 host.json 的 pid 换成了新宿主', !!newPid && Number(newPid) !== Number(oldPid), `旧 pid=${oldPid} 新 pid=${newPid}`);
  check('AC4', '中断后仍能跑完（没有卡在半截）', brkPlay.ended,
    `ended=${brkPlay.ended} kill 前已发 ${brkPlay.posts} 条；errors=${brkPlay.errors.slice(0, 2).join(' | ')}`);
  check('AC4', 'resume 后 decisionHash 与对照场逐字符相同', !!ctlFp.decisionHash && ctlFp.decisionHash === brkFp.decisionHash,
    `对照=${ctlFp.decisionHash} 中断=${brkFp.decisionHash}`);
  // 转写内容逐条比对（比"跨场 transcriptHash 相等"更准确：transcriptHash 的域里含链内序号 i，
  // 而 resume 前后可能多出 advance op，序号本来就会移位——那不是丢发言）
  const postsOf = (home, roomRef) => {
    const p = opsPath(home, roomRef);
    const rows = [];
    let lines = [];
    try { lines = fs.readFileSync(p, 'utf8').split(/\r?\n/).filter(Boolean); } catch { return rows; }
    for (const l of lines) {
      let o; try { o = JSON.parse(l); } catch { continue; }
      if (o && o.o === 'post') rows.push([o.p && o.p.seq, o.p && o.p.alias, o.p && o.p.round, o.p && o.p.text, o.p && o.p.replyTo].join('|'));
      else if (o && o.o === 'vote') rows.push(`vote|${o.p && o.p.alias}|${o.p && o.p.proposalId}`);
    }
    return rows;
  };
  const ctlPosts = postsOf(ctx.A, ctl.ref), brkPosts = postsOf(ctx.A, brk.ref);
  const samePosts = ctlPosts.length > 0 && ctlPosts.join('\n') === brkPosts.join('\n');
  check('AC4', 'resume 后转写内容逐条相同（发言没丢、没重、没换顺序）', samePosts,
    samePosts ? `${ctlPosts.length} 条一致` : `对照=${ctlPosts.length} 条 / 中断=${brkPosts.length} 条；首处不同=${JSON.stringify((ctlPosts.find((x, i) => x !== brkPosts[i]) || '(长度不同)').slice(0, 120))}`);
  const brkStats = opsStats(ctx.A, brk.ref);
  check('AC4', 'resume 后日志链序号没有重号/回退（docs/09 §4：i 是链内序号，只增不重）',
    brkStats.dupIndex.length === 0 && brkStats.ascending,
    `i=${brkStats.indexes.join(',')} 重复=${JSON.stringify(brkStats.dupIndex)} 单调递增=${brkStats.ascending}`);
  check('AC4', '恢复后没有"票丢了"（结论里投票人数仍是 2）',
    (() => {
      const r = runJson(['result', '--ref', brk.ref], { home: ctx.A, timeout: 60000 });
      const p = (r.json && r.json.decisionSet && r.json.decisionSet.participation) || {};
      return Number(p.voted ?? p.Voted) === 2;
    })(), '见 result.decisionSet.participation');
  if (ctlFp.transcriptHash !== brkFp.transcriptHash) {
    metric('AC4 transcriptHash 提示', '两场不同',
      'transcriptHash 域内含链内序号 i，resume 后若多了 advance op 序号会移位；结论以"内容逐条相同"为准');
  }
  metric('AC4 驱动步数（对照场 / 中断场）', `${ctlPlay.steps} / ${brkPlay.steps}`, '步数而非秒，便于发现异常空转');
}

// ─────────────────────── AC1：全程零服务器部署（在最后统一判定） ───────────────────────
function finalizeAC1() {
  const flat = cmdLog.map((c) => c.argv.join(' '));
  const docker = flat.filter((s) => /docker/i.test(s));
  const badAddr = flat.filter((s) => {
    const m = s.match(/--addr\s+(\S+)/);
    if (!m) return false;
    const a = m[1];
    return !/^(127\.0\.0\.1|localhost|\[::1\]):\d+$/.test(a);
  });
  const lan = flat.filter((s) => /--expose-lan|--expose\b/.test(s));
  const tls = flat.filter((s) => /--tls|--cert|--key-file|--https\b/.test(s));
  const curl = flat.filter((s) => /\bcurl\b|\binvoke-webrequest\b/i.test(s));

  check('AC1', '全程没有 docker', docker.length === 0, docker.length ? docker.slice(0, 2).join(' | ') : `${flat.length} 条命令里 0 次`);
  check('AC1', '全程没有公网/内网 --addr（只 loopback）', badAddr.length === 0 && lan.length === 0, `非 loopback addr=${JSON.stringify(badAddr)} 暴露标志=${lan.length}`);
  check('AC1', '全程没有证书/HTTPS 配置', tls.length === 0, tls.slice(0, 2).join(' | '));
  check('AC1', '全程没有用 curl/Invoke-WebRequest 直接打 HTTP（只走 agchat 二进制）', curl.length === 0, curl.slice(0, 2).join(' | '));
  check('AC1', '唯一的可执行文件就是 agchat 二进制', path.basename(BIN).startsWith('agchat'), BIN);
  metric('agchat 命令次数', cmdLog.length, '所有验收动作都经这个二进制');
  metric('测量用外部命令次数', helperLog.length, '仅 Get-Process/tasklist（采资源数字），不参与开会');
}

// ─────────────────────── 报告 ───────────────────────
function printReport() {
  const pad = (s, n) => {
    let w = 0; for (const ch of String(s)) w += /[\u4e00-\u9fff\uff00-\uffef]/.test(ch) ? 2 : 1;
    return String(s) + ' '.repeat(Math.max(0, n - w));
  };
  console.log('\n================ S1 端到端验收 · 本机宿主 + 邀请卡 + 直连 ================');
  console.log(`二进制   : ${BIN}`);
  console.log(`Node     : ${process.version}  平台: ${process.platform}`);
  console.log(`临时目录 : ${ctx.tmp}${opt.keep ? '（--keep 保留）' : ''}`);
  console.log(`验收项   : ${(opt.only || ALL).join(', ')}`);
  console.log('---------------------------------------------------------------------------');
  console.log(pad('AC', 6) + pad('检查项', 52) + pad('结果', 8) + '证据');
  let order = [];
  for (const c of checks) if (!order.includes(c.ac)) order.push(c.ac);
  for (const ac of order) {
    for (const c of checks.filter((x) => x.ac === ac)) {
      const st = c.blocked ? 'BLOCKED' : c.pass ? 'PASS' : 'FAIL';
      console.log(pad(ac, 6) + pad(c.name, 52) + pad(st, 8) + (c.detail || ''));
    }
  }
  console.log('---------------------------------------------------------------------------');
  const pass = checks.filter((c) => c.pass && !c.blocked).length;
  const fail = checks.filter((c) => !c.pass && !c.blocked).length;
  const blk = checks.filter((c) => c.blocked).length;
  console.log(`合计: ${pass} PASS / ${fail} FAIL / ${blk} BLOCKED`);
  const byAC = {};
  const notRun = [];
  for (const ac of (opt.only || ALL)) {
    const rows = checks.filter((c) => c.ac === ac);
    if (!rows.length) { byAC[ac] = '未执行'; notRun.push(ac); }
    else if (rows.some((r) => !r.pass && !r.blocked)) byAC[ac] = 'FAIL';
    else if (rows.every((r) => r.blocked)) byAC[ac] = 'BLOCKED';
    else byAC[ac] = 'PASS';
  }
  console.log('逐项: ' + Object.entries(byAC).map(([k, v]) => `${k}=${v}`).join('  '));
  if (notRun.length) {
    console.log(`\n【未执行】${notRun.join(', ')} —— 一条断言都没跑出来，这不算通过（通常是前置步骤就失败了，见上面的 FAIL 行）`);
  }
  const failedRows = checks.filter((c) => !c.pass && !c.blocked);
  if (failedRows.length) {
    console.log('\n【FAIL 明细】');
    for (const c of failedRows) console.log(`  · ${c.ac} ${c.name}\n      ${c.detail}`);
  }
  if (metrics.length) {
    console.log('\n【轻量表】');
    console.log(pad('指标', 34) + pad('值', 22) + '备注');
    for (const m of metrics) console.log(pad(m.name, 34) + pad(m.value, 22) + m.note);
  }
  console.log('\n' + '='.repeat(75) + '\n');
}

// ─────────────────────── 主流程 ───────────────────────
async function main() {
  ctx.startedAt = now();
  preflight();
  ctx.tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'agchat-acc-'));
  ctx.A = mkHome('machine-A');
  ctx.B = mkHome('machine-B');
  ctx.C = mkHome('machine-C-verify-only');

  const phase = async (name, fn) => {
    try { await fn(); }
    catch (e) {
      check(name, `阶段异常（${e && e.message ? e.message : e}）`, false, String((e && e.stack) || e).split('\n').slice(0, 3).join(' '));
    }
  };

  if (want('AC1') || want('AC2') || want('AC3') || want('AC5') || want('AC7') || want('AC8') || !opt.only) {
    await phase('AC3', phaseCardFlow);
  }
  if (want('AC7')) await phase('AC7', phaseBadCards);
  if (want('AC4')) await phase('AC4', phaseResume);
  if (want('AC1')) finalizeAC1();

  // 收尾：杀掉本次起过的所有宿主（含 resume 换出来的新 pid）
  const pids = new Set();
  for (const home of [ctx.A, ctx.B, ctx.C]) {
    let dirs = [];
    try { dirs = fs.readdirSync(path.join(home, 'rooms')); } catch { /* none */ }
    for (const d of dirs) {
      const hi = readJsonFile(path.join(home, 'rooms', d, 'host.json'));
      if (hi && hi.pid) pids.add(Number(hi.pid));
      try { const p2 = Number(fs.readFileSync(path.join(home, 'rooms', d, 'host.pid'), 'utf8').trim()); if (p2) pids.add(p2); } catch { /* none */ }
    }
  }
  for (const pid of pids) killPid(pid);

  metric('整场验收耗时（秒）', ((now() - ctx.startedAt) / 1000).toFixed(1), '含全部 AC');

  printReport();
  if (!opt.keep) { try { fs.rmSync(ctx.tmp, { recursive: true, force: true }); } catch { /* ignore */ } }
  const fail = checks.filter((c) => !c.pass && !c.blocked).length;
  const blk = checks.filter((c) => c.blocked).length;
  const missing = (opt.only || ALL).filter((ac) => !checks.some((c) => c.ac === ac));
  // 有任何一条没跑出来也算失败：不许让"没跑"伪装成"通过"
  process.exit(fail > 0 || blk > 0 || missing.length > 0 ? 1 : 0);
}

main().catch((e) => {
  console.error('验收脚手架自己崩了（这是脚本 bug，不是产品结论）：');
  console.error(e && e.stack ? e.stack : e);
  process.exit(1);
});
