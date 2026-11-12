// mvp/worker.mjs
// ─────────────────────────────────────────────────────────────────────────────
// BYOK 托管 worker + 内置讨论人格
//
// 存在的理由：豆包 / 千问 / DeepSeek / Kimi / 自建模型大多**不能自己发 HTTP**，
// 也不会自己读 install.md 轮询。它们只会"你给 prompt，我回文本"。
// 这个 worker 就是那条胳膊：它替模型长轮询房间、拼上下文、发请求、处理 409。
//
// 两条路径，同一个接口：
//   有 BYOK_*  → 调 OpenAI 兼容的 /chat/completions，让真模型发言
//   没有 key   → 直接用内置人格脚本发言（确定性、可离线、demo 不会崩）
//
// 用法：
//   const decide = persona('稳妥派');
//   new RoomAgent({ ..., decide })
//
// 环境变量（任选其一，OpenAI 兼容即可）：
//   BYOK_BASE_URL=https://api.deepseek.com/v1
//   BYOK_API_KEY=sk-xxxx
//   BYOK_MODEL=deepseek-chat
// ─────────────────────────────────────────────────────────────────────────────

const BYOK = {
  baseUrl: process.env.BYOK_BASE_URL || '',
  apiKey: process.env.BYOK_API_KEY || '',
  model: process.env.BYOK_MODEL || '',
};

/** 有没有可用的 BYOK 配置。没有就自动走内置人格。 */
export function hasBYOK() {
  return Boolean(BYOK.baseUrl && BYOK.apiKey && BYOK.model);
}

export function byokInfo() {
  return hasBYOK() ? { baseUrl: BYOK.baseUrl, model: BYOK.model } : null;
}

/* ══════════════════════════════════════════════════════════════════════════
   人格库
   ──────────────────────────────────────────────────────────────────────────
   设计要求（写死在这，别退化）：
   1. 四个人格必须**真的对立**。谁都不同意谁，谁都不客套。
   2. 每个点必须是"可被反驳的论断"，不是"我觉得"。
   3. 收敛时必须给出明确的 agree / disagree / abstain，不许和稀泥。
   ══════════════════════════════════════════════════════════════════════════ */

const PERSONAS = {
  稳妥派: {
    label: '稳妥派',
    trait: '先问最坏情况，偏好灰度与可回退',
    // 给真模型用的系统提示
    system:
      '你是一个极度谨慎的决策者。你天然反对"一把梭"式的改变，习惯先问：最坏情况是什么、能不能回退、灰度要多少。' +
      '你说话直接、不客套，会明确说出"我反对"。你不为礼貌而同意别人。',
    stance: 'disagree',
    conclusion: '不建议现在全量改；先灰度一小部分跑满一个完整周期，拿到真实回落数据再决定。',
    // 内置脚本用的论点（{topic} 会被替换成议题）
    points: [
      '我倾向反对现在就动。这类决定，最坏情况不是"没效果"，而是"回不去"——先问一句：三个月后数据难看，我们恢复得回去吗？回不去的方案不该一把梭。',
      '我更信灰度。先拿一小部分人跑完整周期，最坏损失被锁在那一小块里，代价只是慢一个季度，换来的是不必赌。',
      '我最不放心的是没人说退出条件。没有止损线的改变，本质上是在赌自己判断对。',
      '再说一句不好听的：这件事如果真那么明显，它早就被做了。它一直没被做，说明有人已经算过账。',
    ],
  },

  激进派: {
    label: '激进派',
    trait: '认为不作为才是最大成本，主张快跑',
    system:
      '你是一个主张快速行动的决策者。你认为最大的风险是不动，而不是做错。' +
      '你说话锋利、喜欢直接反驳别人，会点名指出对方论证里的漏洞。你从不为了和气而同意。',
    stance: 'agree',
    conclusion: '该改，而且不必二选一：把新方案和旧方案并行一个月，用数据决定谁留下。',
    points: [
      '我不这么看，我认为该做，而且越快越好。这事一直拖着的成本每天都在发生，只是没人把它记进账。',
      '过度谨慎本身就是一种风险：为了不犯错，你把"可能更好"和"确定不好"放进了同一个篮子。',
      '这根本不是二选一。新旧两条路并存一个月，让数据替我们选，代价只是多维护一套配置。',
      '还有一点：讨论本身也在花钱。我们已经聊了这么久还没决定，这本身就是成本。',
    ],
  },

  数据派: {
    label: '数据派',
    trait: '拒绝选边，先要基线、幅度和时间窗',
    system:
      '你是一个数据派。你拒绝在没有基线、预期幅度和判定时间窗的情况下表态。' +
      '你会把对方模糊的说法翻译成可以被推翻的判据，并指出他们缺哪个数。你不站队，但你最后会给条件式的结论。',
    stance: 'agree',
    conclusion: '有条件同意：先补齐基线与观测打点，30 天后用真实数据复盘，数据不支持就回滚。',
    points: [
      '我两边都不站，先要数。这件事至少需要三个输入：现状基线、预期变化幅度、判定成功的时间窗。这三样现在一个都没有，讨论就是在拼直觉。',
      '方向我都能接受，但你们两位的说法都缺一个可被推翻的判据。没有判据，赢了也不知道自己为什么赢。',
      '我的条件是这个：可以动，但必须同时埋好观测，30 天后拿真实数据复盘。否则这次决定学不到任何东西。',
      '补充一个常被忽略的：样本量不够时，前两周的数据会骗人。别拿两周去下结论。',
    ],
  },

  抬杠派: {
    label: '抬杠派',
    trait: '质疑议题本身的前提，不急着选边',
    system:
      '你是一个专门抬杠的讨论者。你不接受议题给定的前提，会先问"这个问题问对了吗"。' +
      '你会指出所有人都在回答"怎么做"，却没人证明"该不该做"。你最后常常弃权。',
    stance: 'abstain',
    conclusion: '议题前提未被证明，弃权；建议先花更小的成本验证前提是否存在。',
    points: [
      '先别急着选边。这事的前提是"现在的做法有问题"，但这件事没人证明过——也许问题根本不在你打算改的那个地方。',
      '你们都在回答"怎么改"，跳过了"不改会怎样"。如果那个答案还算过得去，这次改动就是自找麻烦。',
      '我保留意见。不是因为方案不好，是因为支持它的证据还没出现。',
    ],
  },
};

export const PERSONA_NAMES = Object.keys(PERSONAS);

export function personaInfo(name) {
  const p = PERSONAS[name];
  return p ? { name, label: p.label, trait: p.trait, stance: p.stance } : null;
}

/* ══════════════════════════════════════════════════════════════════════════
   内置脚本引擎（无 key 时的降级路径）
   ══════════════════════════════════════════════════════════════════════════ */

function fill(template, ctx) {
  const topic = (ctx && ctx.brief && ctx.brief.topic) || '这个议题';
  return String(template).replace(/\{topic\}/g, topic);
}

/**
 * 找一条"别人的"发言作为回击对象。
 * 识别自己发过的话不靠 agentId（ctx 里不一定有），而是用 myPastMessages 的 seq 集合，
 * 这样即使上层换了实现也不会把自己的话当成别人。
 */
function pickTarget(ctx) {
  const mineSeqs = new Set(
    (ctx.myPastMessages || []).map((m) => m && m.seq).filter((s) => Number.isFinite(s)),
  );
  const others = (ctx.context || []).filter((m) => m && Number.isFinite(m.seq) && !mineSeqs.has(m.seq));
  if (!others.length) return null;
  return others[others.length - 1]; // 最近的一条
}

function scriptedDecide(p, opts, ctx) {
  const points = (opts.points && opts.points.length ? opts.points : p.points).slice();
  const myTurns = Array.isArray(ctx.myPastMessages) ? ctx.myPastMessages.length : 0;

  // ① 收敛轮：必须交立场
  if (ctx.state === 'converging') {
    return {
      finalStance: opts.stance || p.stance,
      conclusion: opts.conclusion || p.conclusion,
    };
  }

  // ② 第一轮：立论
  if (myTurns === 0 && ctx.round <= 1) {
    return { text: fill(points[0], ctx) };
  }

  // ③ 后续：找个人回击，用没用过的论点
  const target = pickTarget(ctx);
  const point = points[Math.min(myTurns, points.length - 1)];
  const exhausted = myTurns >= points.length;

  // 交锋轮（第 2 轮起）服务端强制要求 replyTo 别人的消息，没有就发不出去
  const mustReply = ctx.round >= 2;

  if (exhausted) {
    // 论点用完：不改口，收一句，避免刷屏
    const short = target ? String(target.alias || '').split(' ')[0] : '你们';
    return {
      text: `${short}说的我还是不能接受。理由前面已经摆完了，改口需要新证据，不是新措辞。`,
      replyTo: target ? target.seq : undefined,
    };
  }

  if (!target && mustReply) {
    // 没有可回复的对象却非要回——这不该发生，用通用反驳兜底而不是硬发
    return { text: fill(point, ctx) };
  }

  // 只用名字前半段（"老张 DeepSeek" → "老张"），避免"老张 DeepSeek 这条……老张担心……"这种重复。
  // 不用 @ 前缀：服务端把 @别名 解析成 mentions 会额外派发发言位，demo 想保持节奏稳定。
  const short = target ? String(target.alias || '').split(' ')[0] : '';
  const lead = short ? `回复${short}：` : '我再补一点——';
  return {
    text: lead + fill(point, ctx),
    replyTo: target ? target.seq : undefined,
  };
}

/* ══════════════════════════════════════════════════════════════════════════
   BYOK 路径：调 OpenAI 兼容接口
   ══════════════════════════════════════════════════════════════════════════ */

function buildPrompt(p, ctx) {
  const brief = ctx.brief || {};
  const topic = brief.topic || '（未提供议题）';
  const rules = brief.rules ? `房间规则：${brief.rules}` : '';

  const transcript = (ctx.context || [])
    .slice(-24)
    .map((m) => `[${m.seq}] ${m.alias}（${m.model || 'agent'}）: ${m.text}`)
    .join('\n');

  const system =
    `你是「${p.label}」：${p.trait}。\n` +
    `${p.system}\n` +
    `你正在一个**纯 agent 讨论室**里和其他 AI agent 讨论一个真实议题，没有人类参与。\n` +
    `${rules}\n\n` +
    `硬性要求：\n` +
    `1. 必须与其他人产生实质分歧，禁止客套（禁止"我同意""说得好""有道理"）。\n` +
    `2. 不复述别人说过的话，每轮必须提供新的论据或反驳。\n` +
    `3. 一段话说完，不要分点，不要 markdown，80-160 字。\n` +
    `4. 只输出 JSON，不要任何解释性文字。\n\n` +
    `输出格式：\n` +
    `普通轮：{"text":"你的发言","replyTo":<要回复的消息 seq，可省略>}\n` +
    `收敛轮：{"finalStance":"agree|disagree|abstain","conclusion":"一句话结论，40 字以内"}`;

  const user =
    `议题：${topic}\n` +
    `当前：第 ${ctx.round}/${ctx.roundsTotal} 轮，房间状态 ${ctx.state}\n` +
    `你已经发言 ${ctx.myPastMessages ? ctx.myPastMessages.length : 0} 次\n\n` +
    `最近的发言记录：\n${transcript || '（还没有人发言，你先立论）'}\n\n` +
    (ctx.state === 'converging'
      ? '现在进入收敛轮：请给出你的最终立场和一句话结论。'
      : '现在轮到你发言。请针对上面最后一条发言做出反驳或补充。');

  return { system, user };
}

async function callModel(p, ctx) {
  const { system, user } = buildPrompt(p, ctx);
  const url = String(BYOK.baseUrl).replace(/\/+$/, '') + '/chat/completions';

  const baseBody = {
    model: BYOK.model,
    messages: [
      { role: 'system', content: system },
      { role: 'user', content: user },
    ],
    temperature: 0.9,
    max_tokens: 400,
  };

  const doFetch = async (body) => {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 60_000);
    try {
      const res = await fetch(url, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          authorization: `Bearer ${BYOK.apiKey}`,
        },
        body: JSON.stringify(body),
        signal: ctrl.signal,
      });
      return { res, text: await res.text() };
    } finally {
      clearTimeout(timer);
    }
  };

  // 有些兼容实现不接受 response_format，失败一次就退回纯文本
  let { res, text } = await doFetch({ ...baseBody, response_format: { type: 'json_object' } });
  if (!res.ok && (res.status === 400 || res.status === 422)) {
    ({ res, text } = await doFetch(baseBody));
  }
  if (!res.ok) throw new Error(`模型接口 ${res.status}: ${text.slice(0, 200)}`);

  const json = JSON.parse(text);
  const content =
    (json.choices && json.choices[0] && json.choices[0].message && json.choices[0].message.content) || '';
  const parsed = extractJson(content);
  if (!parsed) throw new Error('模型没有返回可解析的 JSON');

  if (ctx.state === 'converging') {
    const st = parsed.finalStance;
    if (!['agree', 'disagree', 'abstain'].includes(st)) throw new Error('finalStance 非法');
    return { finalStance: st, conclusion: String(parsed.conclusion || '').slice(0, 80) };
  }
  const out = { text: String(parsed.text || '').trim() };
  if (!out.text) throw new Error('模型返回空发言');
  if (Number.isFinite(parsed.replyTo)) out.replyTo = parsed.replyTo;
  return out;
}

function extractJson(s) {
  if (!s) return null;
  const direct = s.trim();
  try {
    return JSON.parse(direct);
  } catch {
    /* 继续找 */
  }
  const m = direct.match(/\{[\s\S]*\}/);
  if (!m) return null;
  try {
    return JSON.parse(m[0]);
  } catch {
    return null;
  }
}

/* ══════════════════════════════════════════════════════════════════════════
   persona(name, opts) → decide(ctx)
   ──────────────────────────────────────────────────────────────────────────
   opts:
     points     覆盖内置论点（demo 用它把论点钉到具体议题上）
     stance     覆盖收敛立场
     conclusion 覆盖收敛结论
     forceScripted  true = 即使有 key 也用内置脚本（测试用）
   ══════════════════════════════════════════════════════════════════════════ */

export function persona(name, opts = {}) {
  const p = PERSONAS[name];
  if (!p) {
    throw new Error(`未知人格 "${name}"，可用：${PERSONA_NAMES.join(' / ')}`);
  }

  // 熔断器：模型连续失败 2 次后，本场剩余发言一律走内置人格。
  // 否则一个挂起的接口会让每个 agent 每轮都等满 60 秒超时，demo 会像卡死。
  const BREAKER_LIMIT = 2;
  let consecutiveFailures = 0;
  let tripped = false;

  return async function decide(ctx) {
    const useModel = hasBYOK() && !opts.forceScripted && !tripped;

    if (useModel) {
      try {
        const out = await callModel(p, ctx);
        consecutiveFailures = 0;
        return out;
      } catch (err) {
        consecutiveFailures += 1;
        if (consecutiveFailures >= BREAKER_LIMIT) tripped = true;
        // 优雅降级：模型挂了、key 错了、返回格式崩了，都退回内置人格。
        // demo 绝不能因为模型侧的问题整场崩掉。
        if (typeof opts.onModelError === 'function') {
          opts.onModelError(err, name, { tripped });
        }
      }
    }
    return scriptedDecide(p, opts, ctx);
  };
}

/** 给 demo 用的：一次性建好三个人格，共享一份议题论点 */
export function debateTeam(spec = []) {
  return spec.map((s) => ({
    alias: s.alias,
    model: s.model,
    persona: s.persona,
    decide: persona(s.persona, {
      points: s.points,
      stance: s.stance,
      conclusion: s.conclusion,
      forceScripted: s.forceScripted,
      onModelError: s.onModelError,
    }),
  }));
}
