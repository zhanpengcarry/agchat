# 审计报告 1 · 本地宿主（`server/internal/host`）

> 审计员：外部（不改源码；新增的只有本文件 + 一个探针测试文件 `server/internal/host/zz_audit_probe_test.go`，可随时删）
> 判定基准：`docs/09-本地宿主规格-S1冻结.md`（先读，后读代码）
> 审计方式：逐行读 `server/internal/host/*.go` + 必要的上下游（`room/ops.go`、`invite/invite.go`、`oplog/oplog.go`、`store/store.go`、`cmd/agchat/*`）；用探针测试落地验证。**复现**：`cd server && go test ./internal/host/ -run TestProbe -v`（断言基本写成日志，套件保持绿；探针只证明"现在是这样"，不是断言"应该这样"）。跑过 `go vet ./internal/host/...`（干净）与 `go test ./internal/host/`（绿）。
> 快照：审计期间工作树有并发改动（`server.go` 的 413 body_too_large、`log.go` 的 `count()` 等），下列行号取自以下哈希对应的内容，`git diff` 之后再对一遍行号。
>
> ```
> 167786E9EA50EC3D917598C602388FB634405412  host.go
> 4B069A7BAC2AED9A19457E77D051EDF48FB4ADA3  server.go
> 5AB4AD0503155141B16E4DF48D3732BBE9EEC54C  log.go
> 7497D5F49F31829F1633191726EDEAB7C23D7A80  export.go  28D2AF91B062C97EE419D272B3648C44B2A6B292  files.go
> 6F80BFC89621620D876E651DB154CC6F028BEC55  admit.go  3FA2606FD3E70EE27E53029D0B7F3D4ECFD72647  config.go
> ```
>
> 汇总：**高 3 / 中 8 / 低 13**。

---

## 一、逐条 FINDING

### HOST-01 · 高 · `host.go:1160`（调用点 `server.go:213-226`，CLI `cmd/agchat/commands.go:233`）

**现象.** `CloseRoom` 把 `rc.adminKey`（**明文**）直接交给 `eng.Close`，而引擎要的是**哈希**：
`room/ops.go:1157` 是 `if adminKeyHash == "" || adminKeyHash != r.AdminKeyHash`，而 `r.AdminKeyHash` 在 `engine.go:1032` 存的是 `domain.HashToken(adminKey)`。对照 `api/handlers.go:556-560`，agchatd 侧是先 `domain.HashToken(adminKey)` 再传。

**为什么会出问题.** 明文永远不等于哈希 → `engine.Close` 在 `ops.go:1158` 走 `domain.ErrNotFound` → 宿主 `toOpErr` 把它翻译成 `errNotFound`。结果是：`POST /v1/close` 与 `agchat room close` **100% 失败**，返回 `404 not_found`（人话还是"会议不存在"，把人往错方向指）。房主既不能提前收会，`end{reason:"admin_close"}` 这条 op 也永远写不出来，`reasonFor` 里 `admin_close` 分支（`host.go:620-622`、`637-639`、`709-711`）全是死码。`docs/11-S1验收报告.md` 的验收里也基本没提 close，所以漏了。

**怎么修.** `host.go:1160` 改成 `h.eng.Close(ctx, TenantID, rc.roomID, domain.HashToken(rc.adminKey))`。同时把 `roomCtx.adminKey` 的注释写清"存明文、调用前哈希"，或直接改存 `domain.HashToken(adminKey)`（`openRoom:249` 与 `Create:818` 两处写入点一起改）。

**回归测试.** 能，且已写成（`TestProbeCloseRoomWorks`）：建会 → `h.CloseRoom` → 断言无错、`state == ended`、`ops.jsonl` 里出现 `"o":"end"` 且 `reason=="admin_close"`；反面用例：把 `seats.json` 里的 `adminKey` 改一个字再 close，应 404。

---

### HOST-02 · 高 · `server.go:443`（配合 `room/ops.go:410`）

**现象.** 长轮询端点把 `since` 传成 `nil`：

```go
snap, err := h.eng.Inbox(ctx, TenantID, rc.roomID, tokenHashOf(s), nil, wait)
```

而 `Inbox` 的等待判据是 `if since == nil || wait <= 0 || event > *since || snap.State == StateEnded { return snap, nil }`（`room/ops.go:410`）。`since == nil` 直接短路。

**为什么会出问题.** `GET /v1/next?wait=20` **立即返回**。实测 `wait=2s` 只花 3.5ms。后果连锁：(a) 违反 §3「长轮询」契约；(b) agent 拿到 `yourTurn=false` 就只能自己 sleep 自旋，与 §8⑦「空转 CPU ≈ 0」相反；(c) viable 的 `wait` 上限 20s 变成装饰，`server.go:330-341` 的钳制白写；(d) 引擎 `Inbox` 里"状态一变就唤醒"的整套 `rt.waiters` 广播（`ops.go:414-438`）只有 agchatd 在用，宿主这条路径永远走不到。

**怎么修.** `roomCtx` 加一个进程序号游标（不要用 `snap.Seq`，那是消息序号）：`roomCtx.ev int64`，由 `syncLocked`/`tickPass` 在检测到跃迁时自增，`Next` 读 `beforeEv := rc.ev`（在 `rc.mu` 内），把 `&beforeEv` 传给 `Inbox`。若不想改引擎签名，则退一步：传一个"刚刚取到的游标"并在 `syncLocked` 之后重新判断一次是否需要重等。

**回归测试.** 能（`TestProbeLongPollReturnsImmediately`，已写）：`wait=2s`、无人发言时断言耗时 > 1.8s；再来一条：另一 goroutine 在 200ms 后写一条 op，断言 `Next` 在 1s 内返回且 `YourTurn` 变化。

---

### HOST-03 · 高 · `host.go:1525`、`1592-1596`（写侧 `log.go:263-265`、`log.go:91-93`）

**现象.** 幂等缓存只按 `(agent,kind,opKey)` 存，**不分成功/失败**：reject 分支的 `commitRec{OK:false,...OpKey:req.OpKey...}` 一样进 `l.idem`。命中后 `applyOnSeat` 直接 `return res, nil, nil`（`res` 为空、`ack` 为 nil），handler 走 `out["duplicate"]=true` 分支 → 客户端收到 **HTTP 200 `{"ok":true,"duplicate":true,"result":null}`**。

**为什么会出问题.** 客户端只要复用 opKey 重试一条**被拒**的 op（这正是 opKey 的公开语义："网络重试靠它去重"），就会拿到一个"成功"响应，于是不再重发 → 发言/投票**静默丢失**。`loadOpLog` 还会在 resume 后把 reject 的 opKey 重新灌进 idem（`log.go:91-93`），所以这个坑跨重启存在。实测：第 1 次 `403 wrong_round`，第 2 次同 opKey → `err=<nil> res=<nil> ack=false`。

**怎么修.** 在 `applyOnSeat` 的幂等命中分支里 `if !cr.OK { return nil,nil, 用 cr.Code/cr.T 重建的 opError }`（`commitRec` 已有 `Code`，再补一个 `Status`/`Detail` 字段落盘）；或在 `opLog.commit` 里对 `OK:false` 不写 idem——但前者更好，能保持"同 opKey 同结果"。另把 `server.go:362-368` 的 `duplicate` 语义与 `ok` 解耦：`duplicate:true` 且 `ok:false` 时不要回 200。

**回归测试.** 能（`TestProbeRejectedOpKeyReplay`，已写）：briefing 阶段发一条 post（被 403）→ 同 opKey 重发 → 断言仍 403 且 `code` 相同；另加一条正面用例：成功的 post 用同 opKey 重放仍返回 200 + 同一 `seq`。

---

### HOST-04 · 中 · `host.go:260-266`、`311-327`、`347-394`

**现象.** `adjustClockForResume` 按 `downtime = now - rc.lastAlive` 把所有绝对时间戳整体**加**（`CreatedAt`/`BriefingDeadline`/`StartedAt`/`PhaseStartedAt`/`RoundIdleSince`/`TurnStartedAt`，`host.go:359-374`）。但 `rc.lastAlive` 的新值要等 `openRoom` 后段的 `h.touch(rc)` + `h.persist(rc)`（`host.go:274-275`）才落盘；`persist` 又是 best-effort（`336-340`）。**DB 已经平移、`seats.json` 还是旧 `lastAlive`** 的这段窗口里一旦再崩（或 `saveState` 失败），下一次 resume 会按**同一个 downtime 再平移一遍** → 累积。

**为什么会出问题.** 平移是把时间戳往**未来**推：`t_new = t_old + (now - lastAlive)`，而 `deadlineDue`/`tick` 用的是 `now >= CreatedAt + MaxMinutes*60000`（`host.go:543`、`engine.go:836`）与 `now >= BriefingDeadline`。叠加后 `CreatedAt` 可以落到 `now` 之后 → `now - CreatedAt` 为负 → `MaxMinutes` 的有效时长被无限放大，briefing/投票/概述窗口同比例漂移；同时"真实流逝"和日志里的 `t` 脱钩。实测：真实只过了几毫秒，两次 resume 后 `CreatedAt` 被推到 **+2 小时**（`now-CreatedAt = -7.2e6 ms`）。

另外同函数 `if r.State == StateEnded { return nil }`（`host.go:356-358`）是在事务闭包里提前返回，跳过了 `SaveRoom`，但函数随后**照样** seal 并 commit 一条 `advance{resume_clock_adjust, downtimeMs}`（`381-393`）——已结束的房间会平白多一条 advance，且 `downtimeMs` 是一个没发生作用的数字。

**怎么修.** ① 先落盘再改库：在 `adjustClockForResume` 的事务**成功之后立刻**`h.touch(rc); h.persist(rc)`，成功才 seal advance；② 把 `lastAlive` 改成"已平移到的水位"语义，或额外存 `clockShiftedAt`，resume 时 `dt = now - max(lastAlive, shiftedAt)`；③ ended 分支 `return errSkip` 让调用方跳过 advance。

**回归测试.** 能（`TestProbeDoubleClockAdjust`，已写，用"删 map 条目 + 写回旧 `lastAlive` + 再 openRoom"模拟崩溃窗口）：断言第二次平移后 `CreatedAt <= nowMillis()`；再来一条 ended 房间的用例：断言 resume 后 `ops.jsonl` 行数不变。

---

### HOST-05 · 中 · `host.go:1258-1290`

**现象.** 第二条进场入口只验 `ref` + `agentPub`（`1259` 取房间、`1280` 在 `rc.byPub` 里找座位），**不要求任何签名**。拿到座位后立刻走引擎认回分支（`1327-1329`，`Reclaim:true, ForceReclaim:true`），而引擎的认回会 `DeleteTokenDirectoryByAgent` + `RotateAgentToken`（`room/ops.go:279-287`），把响应里的新 token 发出去。

**为什么会出问题.** `agentPub` 本质是**公开值**（不是秘密），于是"ref + agentPub"就是一张可转手的 bearer 凭证：任何知道二者的人都能把某个座位的 token 顶掉，合法持有者的**旧 token 当场失效**（实测：受害者的 `oldTok` 变成 `bad_token`，攻击者的 token 可用）。`docs/09` §1/§3 的说法是"座位靠 agentKey 认回、卡是唯一凭证"，实现上并没有证明"我拿着私钥"。风险面：agent 常把 pub / 错误日志贴给主人或第三方；再叠加 HOST-12 的 `ref` 无格式校验，攻击路径很短。

**怎么修.** 在 `JoinReq` 加 `sig`（对 `"agchat-join-reclaim:v1" + 0x00 + ref + 0x00 + nonce` 的签名，域沿用 `oplog` 的"一域一函数"纪律加 `oplog.SignReclaim`），ref-only 分支里 `ed25519.Verify(pub, ...)` 后再认回；带卡分支可保留免签（卡本身是凭证）。

**回归测试.** 能（`TestProbeJoinByPubOnly` 已写，先断言"现在不签名也能拿 token"，改完把断言翻成"必须带 sig"）：造一个只有 pub 的请求 → 期望 400/401；带正确 sig → 200 reclaimed；带别的私钥的 sig → 401。

---

### HOST-06 · 中 · `host.go:1562-1568`、`1652-1718`（配合 `server.go:389-402`）

**现象.** 验签之后，宿主只校验 `op.O`（1514）、`op.A`（1517）、`i/ph`（1558）、`h` 重算（1566）、`sig`（1569），然后 `off, _ := rc.log.intent(op)` **原样落盘**客户端给的 `op.P` 字节。`op.P` 里的 `seq/round/agentId/alias` 与数据库里的真值从不比对，也不做规范化（§4「载荷本身是规范化 JSON：键升序、无空格、无 null」）。

**为什么会出问题.** 三个后果都被实测复现（`TestProbePayloadTrust`）：一条 post 的载荷谎报 `seq:999, round:77, agentId:"m_9", alias:"假名"`，引擎照收（真实 `Seq:1`），落盘行保留客户端的键序（`seq,round,agentId,alias,text`，不是升序）。而 `transcriptHashOf`（`export.go:99-122`）恰恰只从 `op.P` 取 `{seq,round,alias,text,...}` 算指纹 —— 于是 **`transcriptHash` 锚的是"客户端自述"，不是"参与者实际看到的"**；`/v1/next` 里展示的是 DB 里的真值，两者可以长期不一致，而 verify 两边都读 op 流，仍然 `verified`，不会报。另外 §4.1 冻结了 `p` 的字段集，未知字段现在被静默接受。

**怎么修.** 在 `applyOnSeat` 进 `runEngine` 之前加一个 `checkPayloadFields(kind, op.P, snap)`：`seq == snap.Seq+1`、`round == snap.Round`、`agentId == s.AgentID`、`alias == s.Alias`、无未知键；不符就按 reject 处理（或至少诊断为 warn 字段）。落盘前对 `op.P` 做一次 `oplog.CanonicalJSON` 并**用规范化后的字节重算 h 再验签**（客户端若用 `oplog.Seal` 本来就是规范字节，行为不变）。

**回归测试.** 能：把 `TestProbePayloadTrust` 的载荷从"全部撒谎"拆成 4 条独立用例，逐条断言 reject 码；再加一条规范字节的正面用例断言 `ops.jsonl` 里的 `p` 键序升序。

---

### HOST-07 · 中 · `host.go:225-231` 与 `276-279`（入口 `server.go:234-239`）

**现象.** `openRoom` 是典型的 check-then-act：先 `h.mu.Lock()` 查 `_, ok := h.rooms[ref]` 再 `Unlock` 做一长串 IO（读密钥、`loadOpLog`、`loadState`、`peek`、写盘、commit），最后才重新加锁插入 `h.rooms[ref]`。`resume` handler 自己也先 `roomByRef` 判空再 `openRoom`（`server.go:234`）。两个并发 `/v1/resume` 会都通过判空、都 `loadOpLog`。

**为什么会出问题.** 后插入者覆盖先插入者：先建的那份 `roomCtx` 连同它的 `*os.File`（`loadOpLog:139` 的 O_APPEND 句柄）**从此不可达也永不 close**（`Close()` 只遍历 `h.rooms`）→ 句柄泄漏；Windows 上还会长期占着文件（探针收尾时就撞到 `ops.jsonl ... being used by another process`）。两份内存 `ops/commits/idem` 各自演化，`nextIndex` 可能给出同一序号。另外 `loadOpLog:122-138` 会做 `Truncate`，两个加载交错时截断与 append 互相干扰。

**怎么修.** 在 `rooms` 里放"占位符"：`h.mu.Lock()` 后若不存在就写入 `nil`（或 `sync.Once`+`make(chan struct{})`）再 `Unlock`，真实加载完再替换并 `broadcast`；后来者阻塞等同一个 `openRoom` 结果（`sync.OnceValues` 是最短实现，key 是 ref）。

**回归测试.** 能：删掉 map 条目后并发起 8 个 `openRoom(ref)`，断言 `h.rooms` 只有 1 项、`len(rc.log.committedOps())` 与磁盘行数一致；进程级断言句柄数不变（Windows 上用 `GetProcessHandleCount` 或再 `os.Rename` 目录成功）。

---

### HOST-08 · 中 · `host.go:1413`（触发点 `1359-1364`）

**现象.** ref-only 入口里 `card` 是 nil（不入 `1234` 分支），认回失败且还有空位时走"兜底开新座"（`1359-1364` 不 return，继续往下），最后 `seat` 构造用 `Caps: append([]string{}, card.P.Caps...)`。

**为什么会出问题.** `card` 为 nil → `card.P.Caps` **空指针 panic**。`net/http` 会按连接 recover，所以表现为**连接被掐断**（客户端看到 EOF，而不是 JSON 错误），座位/日志可能已经写了一半。当前之所以还没爆，是因为 `1328` 恒为 `ForceReclaim:true`，`member_active` 在 `room/ops.go:273` 的前置条件 `!in.ForceReclaim` 下永假，兜底分支实际是死路——也就是说 **`isMemberActive` 判断 + 兜底逻辑现在是纯装饰**，一旦有人把 ForceReclaim 改成条件式（很可能，因为它防双开的意义还在）就会踩到。

**怎么修.** `1359-1364` 的兜底路径里把能力来源定下来：`caps` 已在上面的 `card == nil` 分支从 `prev.Caps` 取过（`1285`），`1413` 改成 `Caps: append([]string{}, caps...)`，顺带把 `1299` 的 `_ = caps` 删掉（它正是被这么写废的）。同时决定 `member_active` 还要不要留。

**回归测试.** 能：把 `eng.Join` 的 `ForceReclaim` 临时置 false（或以 alias 相同的新 pub 制造 `member_active`）→ 断言 ref-only 兜底不 panic 且返回 409 而非 EOF；再加一条 `go test -race -run Join` 兜 general。

---

### HOST-09 · 中 · `host.go:598-599`、`655-661`、`669`、`684-685`

**现象.** `syncLocked` 把 `after.Seq != before.Seq` 也算作跃迁，而 `reasonFor` 对"state/round 都没变"的跃迁一律回 `ReasonTurnTimeout`（`661`）。于是**每一条正常发言**都会追写一条 `advance{reason:"turn_timeout", deadlineHit:false}`（`684-685` 里 TurnStartedAt 差 ~0，恒 false）。

**为什么会出问题.** 实测 3 条正常发言 → 3 条 advance，其中 2 条谎称 `turn_timeout`（第 3 条是 `rounds_done`）。`advance` 是 §4 里"每次时间引发跃迁都必须写"的判定输入、也是 verify 要按 §10.2⑥ 闭枚 reason 校验的对象；把"轮到下一个人了"记成"上一个人超时"，会让事后的"谁在拖时间"完全失真。附带把日志量翻倍（1 条发言 2 行），与 §8⑦ 的 2MB 预算冲突。

**怎么修.** `reasonFor` 加一档：`after.State == before.State && after.Round == before.Round && after.Seq != before.Seq && after.PlanPos != before.PlanPos` → 新增 `oplog.ReasonTurnPosted`（若不愿加枚举值，就用 `reason:"round_idle"` 之外的现有值并在 `deadlineHit=false` 时说明）；再把"是否真的超时"作为 `deadlineHit` 的依据（`logTransition` 已在算，但只在 reason 命中 turn_timeout 时算，需提到前面）。若 §10.2⑥ 的枚举不许加，则改成"只有 `PlanSkipped` 增长才写 advance"。

**回归测试.** 能（`TestProbeAdvanceLabeling` 已写）：3 人各发一条 → 断言 advance 里 `reason=="turn_timeout"` 的条数与 `deadlineHit==true` 的条数一致（当前是 2 vs 0）。

---

### HOST-10 · 中 · `host.go:1381-1409`（对照 `host.go:1539-1550`）

**现象.** `applyOnSeat` 严格做到"`peek` → `eng.View`(tick) → `syncLocked` → 再 act"（1539-1550），但 `Join` 没有：它在 `1381` 用**缓存的 `rc.snap.State`** 判 `Briefing/Debating` 闸门，随后直接 `h.eng.Join`（1401），只在**动作之后**才 `syncLocked`（1407）。

**为什么会出问题.** 违反 G10 的"每个改状态请求之前先推进"。① 闸门读的是可能过期的快照（`rc.snap` 只在 `syncLocked`/`tickPass` 更新，两次 tick 之间最长 1s），会有一秒钟的窗口按旧状态放行/拦截；② 由此产生的跃迁（比如满员触发 briefing→debating）只能事后记，链上顺序变成"先 act 后 advance"；而且 `reasonFor` 对 briefing→debating 返回空串（`649-654`），这条跃迁**根本不进日志**——实测 3 人进场后 `ops.jsonl` 里没有任何 advance。

**怎么修.** `Join` 在 `peek` 后照抄 1543 那一行 `h.eng.View(..., noop)` + `syncLocked`，然后再读闸门、再 `eng.Join`；顺手超时 `briefing→debating` 的 reason（或确认 §10.2⑥ 允许新增）。

**回归测试.** 能：`tickPass` 之外人为把 `rc.snap.State` 改成 `StateEnded` 但 DB 仍是 briefing，断言 join 仍按 DB 真值判定；另外断言满员开场时有 advance 记录。

---

### HOST-11 · 中 · `host.go:398-399`、`431-443`

**现象.** ① `ctx, cancel := context.WithCancel(ctx)` 的 `cancel` 只存进 `h.cancel`，**从不调用**；② `select` 的 errCh 分支（`432-436`）直接 `return`，既没有 `Shutdown` 也没有 `h.Close()`；③ `Shutdown` 的徒期固定 5s（`439`），而 `MaxWaitSeconds` 是 20（`room/engine.go:44`），且返回的错误被 `_ =` 吞掉（`441`），下一行就 `h.Close()` 关库关文件。

**为什么会出问题.** ② 一个监听失败就带走整个进程的收尾 → `h.st`（SQLite）与所有 `ops.jsonl` 句柄留在 "已无 ticker/HTTP" 的半死状态；① 泄漏一个 context 直到外层取消；③ 只要有人挂着 `next?wait=20`，5s 后强行进入 `Close()`：在飞请求撞上 `sql: database is closed`，而如果已经写了一条 op 行（日志先行）却没写进库，下次 resume 只能靠截尾巴兜——这恰好是 `log.go` 想避免的那类中间态。

**怎么修.** `defer cancel()`；把两个退出分支合并成"`Shutdown` → `Close` → `return err`"；`shutCtx` 徒期取 `MaxWaitSeconds + 5s`；`Shutdown` 错误至少 `fmt.Fprintf(os.Stderr, ...)`；`Close()` 前置一个 `h.cancel()` 让 ticker 立刻停。

**回归测试.** 能：起 Serve，挂一个 `wait=20` 的 `next`，取消 ctx，断言 `Shutdown` 在 >5s 后仍有正常响应、`ops.jsonl` 无未提交尾巴；另一条：让 `Serve` 在一个已被占用的端口上失败，断言 `h.st` 被关闭（对同一路径再 `store.Open` 不报锁）。

---

### HOST-12 · 低 · `server.go:228-249`（`host.go:103-105`、`134-149`）

**现象.** `/v1/resume` 直接用请求体里的 `ref` 拼路径：`filepath.Join(c.Home, "rooms", ref, ...)`，`ref` 除 `TrimSpace` 外没有任何格式校验（无字符集、无长度）。实测 `ref:"../evil"` → `200`，读到了 `rooms/` **之外**的 `host.key`，并往 `<Home>/evil/host.json`、`host.pid`、`seats.json` 写了文件。

**为什么会出问题.** 一个受控的 `ref` 就能让宿主读写 `Home` 下任意相对路径的东西（只要有 `host.key` 可读）。最扎眼的是 `ref:"../agchat.db"`：`loadOpLog` 会把数据库文件当 JSONL 读，`lastCommitEnd` 为 0 时它还会 **`Truncate` 到 0** —— 即"用错了 ref 名就把库清了"（**未验证**，值得单独试一把）。窗户不大（要已持 ctl token），但 "ref 是 43 字符 base64url" 是这个项目自己的铁律，这里是唯一不守的地方。

**怎么修.** `Config` 加 `func validRef(s string) bool`（`len==43` 且全部在 base64url 字母表内），`resume`/`status`/`export`/`card` 统一走一遍；`Join` 里的 `in.Ref` 同样（否则 ref-only 路径还能拿它去查）。

**回归测试.** 能：表列 `"../evil"`/`""`/`"a/b"`/43 字符正常 ref，断言状态码与 `hostFile` 路径。

---

### HOST-13 · 低 · `host.go:714-728`

**现象.** `end` op 的终态快照里 `"emptyRounds": 0`、`"roundSkipped": false` 是**写死的**，而 `domain.Room` 明明白白有 `EmptyRounds`/`RoundSkipped`（`engine.go:658-669` 在用），`snapInfo` 却没采。

**为什么会出问题.** `end` 快照是离线复算终态的唯一输入（§4.1 `end` 载荷）。一场靠"连续空轮"进投票的会，和一场正常轮次走完的会，在快照里完全一致；tier 2 严格复算（S2）一上来就会卡在这里。

**怎么修.** `snapInfo` 加两个字段、`snapOf` 里采 `r.EmptyRounds`/`r.RoundSkipped`、`logEnd` 用真值；`snapshot.planSkipped` 已经是真值（`727`），照那个写法办。

**回归测试.** 能：制造 3 个空轮进投票再结束，断言 `end` 的 `emptyRounds==3`。

---

### HOST-14 · 低 · `server.go:50-98`

**现象.** Host 头校验有三处漏：① 白名单按**字节精确**比对，`LocalHost:PORT` 被拒（DNS 名本来就大小写无关）；② 无端口形态一律跳过端口一致性校验（`port != "" && port != want`），`Host: 127.0.0.1` 直接放行；③ `splitHostPort` 用 `LastIndex(":")`，裸 IPv6 `Host: ::1` 会被解成 `name=":"`、`port="1"` → **403**（规格允许 `::1`）。实测：`LocalHost:PORT`→403、`::1`→403，而 `127.0.0.1`（无端口）、`127.0.0.1:PORT`、`[::1]:PORT`→200；`evil.example.com:PORT`、`127.0.0.1:PORT.evil.com` 都被正确挡住，rebinding 主防线有效。

**为什么会出问题.** 漏的是可用性/口径（①③ 会让人以为宿主没起来；② 让"端口对不上"这条校验形同虚设，因为最省事的写法就是不带端口）。安全性影响很小（名字仍须在 loopback/LAN 白名单里）。

**怎么修.** `name = strings.ToLower(name)`；裸 IPv6 先试 `net.ParseIP(host)` 成功就只取名字；端口缺失时 `want != "" → 403`（或明确允许并写注释）；也可以用 `net.SplitHostPort` + 回退，替掉手写 `LastIndex`。

**回归测试.** 能：参数化表用例（就是 `TestProbeHostHeaderEdges` 那张表），改完把期望值翻过来。

---

### HOST-15 · 低 · `host.go:1553-1568`

**现象.** `if op.PH == "" { op.PH = head }` 这条"便利补全"实际走不通：`oplog.Seal/SealOp` 会把 `""` 归一成 `ZeroHash`（64 个 0，`oplog.go:248-251`、`287`），于是 `op.PH == ""` 为假 → 保持全零 → `1560` 判 `op.PH != head` → **409 head_conflict**；而手搓一个字面 `PH:""` 的 op 又会命中补全、随后 `1566` 的 `h2 != op.H` → **400 bad_op_hash**（因为客户端签的 h 是按 `ph=ZeroHash` 算的）。两种口径都验不过。

**为什么会出问题.** 客户端契约写着"i/t/ph 由作者填"（`oplog.SealOp` 注释），但宿主又暗示"空就帮你填"。任何第三方客户端（就是 S1 要让人来参会的那类）第一次写都会撞上，且 409/400 两个码都不能自解释。

**怎么修.** 要么删掉补全、在注释里写死"PH 必须给"；要么保留补全但**重算前先把 `op.H` 校验拆两步**：`PH==""` 时先按 `ZeroHash` 验签名，通过后再用 head 替换（即"补全只在验签后生效"）。

**回归测试.** 能（`TestProbeEmptyPH` 已写两种口径）：断言 `Seal("")` 的 op 被接受、字面 `""` 也被接受；非法 PH 仍 400。

---

### HOST-16 · 低 · `server.go:389-402`

**现象.** `decodeBody` 是 `json.NewDecoder(...).Decode(v)` 一遍了事：① 未知字段静默忽略（`CreateReq` 写错 `{"seat":3}` → `Seats=0` → 引擎默认为 6 人，没任何提示）；② 合法 JSON 之后再跟垃圾字节也返回成功（`{"topic":"t2"} garbage` → 200）。

**为什么会出问题.** 静默改语义。"手滑少打一个 s"和"故意多传字段" 在宿主侧无法区分；尾部垃圾则让 `Content-Length` 与实际内容不一致的中间层问题隐形。体积上限本身是好的（`MaxBytesReader` + 413，`390-396`）。

**怎么修.** `dec.DisallowUnknownFields()`；`Decode` 后再 `if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) → 400 bad_json`（`invite.decodePayload:446-448` 已有现成写法，照抄口径）。

**回归测试.** 能（`TestProbeDecodeBody` 已写）：未知字段→400；尾部垃圾→400；超长→413 保持不变。

---

### HOST-17 · 低 · `host.go:1182`、`1390`、`1499`、`783`（题目进卡 `1043-1061`）

**现象.** 一批外部字符串完全没有长度上限：`CreateReq.Topic`（只 `TrimSpace`，4MB 请求体全塞进去都行）、`JoinReq.Alias`/`Model`、`AgentOpReq.OpKey`、`CreateReq.MaxOutput`（引擎不夹，`engine.go:998-1001`）。`MaxMinutes` 有下限无上限（`engine.go:970-976`）。

**为什么会出问题.** `topic` 会原样进人读块（`humanLines:1053`），而 §2.1 要求人读块 ≤ 500 字符 —— 现在没有强制，长议题会把人读块撑爆；`alias`/`opKey` 会进 `seats.json`、每条 op 行（`opKey` 还进 `idem` 的 map 键，4MB 的键基本等于常驻泄漏）；`MaxOutput` 放大单条发言上限，把"单会 ≤ 2MB 日志"（§8⑦）往上顶。

**怎么修.** 在 handler 侧定上界并给稳定错误码：`topic ≤ 200 rune`、`alias ≤ 40`、`model ≤ 64`、`opKey ≤ 128`、`maxOutput ≤ 4000`、`minutes ≤ 600`；`opKey` 再加一次"只许 base64url 字母表"。

**回归测试.** 能：逐字段的边界表用例（`199/200/201`）断言 400 与错误码稳定。

---

### HOST-18 · 低 · `invite.go:392-395`（入口 `host.go:1236`）

**现象.** `splitMachine` 对剥离后的整段做 `strings.Split(body, ".")`，要求恰好 2 段；而 `/v1/join` 的请求体上限是 4MB。

**为什么会出问题.** 4MB 全是 `.` 时 `Split` 会先分配约 400 万个 `string` 头（≈64MB）再因为 `len(parts) != 2` 全部丢掉，单请求瞬时内存放大约 20×。本机低频，够不上攻击，但配合"`join` 是唯一无 token 入口"值得一提（另：`join` 也没有任何限流，§3 说好的"默认全关"是有意的）。

**怎么修.** 切之前先 `strings.Count(body, ".") != 1` 判一次（O(n) 无分配），不满足直接结构性失败；或者 `Index`/`LastIndex` 两刀。

**回归测试.** 能：`strings.Repeat(".", 4<<20)` 作卡体，断言状态码与耗时；再加 `alloc` 断言（`testing.AllocsPerRun` 可选）。

---

### HOST-19 · 低 · `host.go:336-340`（`saveState:311-327`、`openRoom:254-257`）

**现象.** `persist` 吞掉 `saveState` 的所有错误（`_ =`），上层无从得知；`saveState` 失败还会把 `rc.dirty` 提前清零（`325`，在 `writeJSON` 之前），于是"写失败 → dirty=false → 再没人重试"。另一头：`loadState` 返回任何非 `NotExist` 错误都会让 `openRoom` 失败，房间被 `loadRooms` 跳过（`216-219`），**没有修复路径**。

**为什么会出问题.** 写失败（盘满、只读、杀软锁）后 `seats.json` 悄悄落后：里面存的是 agent token（`seat.Token`），落后等于 resume/幂等认回丢座位；而 `dirty` 被清零让下一次 persist 也跳过。`loadRooms` 的"跳过并继续"没有止损，只往 stderr 写一行，之后的 `/v1/resume` 只会回 404 + 底层路径。

**怎么修.** `saveState` 只在 `writeJSON` 成功后清 `dirty`；`persist` 返回 error，关键路径（join/reclaim/issue）把失败上抛成 500（会议可以继续，但要让人知道凭据没落地）；`loadState` 遇到坏 JSON 时保留"空座位表 + 打 `stderr` + 在 `StatusResp` 加一个 `degraded` 标记"，别让房间整个不可用。

**回归测试.** 能：把 `rooms/<ref>` 目录设成只读后 join，断言 500 且 `dirty` 仍为 true；把 `seats.json` 写成 `{` 后重启，断言房间仍可 `status`、`degraded==true`。

---

### HOST-20 · 低 · `files.go:29-46`、`171-178`；`config.go:61-70`

**现象.** 所有秘密都靠 `os.WriteFile(tmp, b, 0o600)` 这一层权限：`control.token`（`files.go:129`）、`rooms/<ref>/host.key`（roomKey 私钥 + 会议密码 + adminKey）、`seats.json`（明文 agent token）。`ensureHome` 也只对**新建**目录 `MkdirAll(0o700)`，已存在目录不收紧。`AGCHAT_HOME` 完全可控，不做任何规范化（相对路径按 cwd 解析）。

**为什么会出问题.** Windows 上 Go 的 `mode` 基本只映射成只读属性，**不设 DACL**：实际保护来自父目录的继承 ACL。于是 `AGCHAT_HOME=C:\Windows\Temp\x` 这种人人可写的父目录下，任何本地用户都能读到 roomKey 私钥（用它伪造任意卡、签 advance/end）。**未验证**：需第二个本地账户实测 `icacls`。

**怎么修.** 文档/启动时检查：`Home` 落在"非用户目录"时打一条警告；`New` 里对 `Home/rooms/seats` 做一次 `os.Chmod(0o700)` 纠正；`roomSecret` 里的密码与私钥考虑只存内存 + 首启写盘（S2 再说）。

**回归测试.** 半能：`ensureHome` 后把目录 `Chmod(0o755)` 再 `New`，断言被改回；ACL 部分只能手工（`icacls` 对拍）。

---

### HOST-21 · 低 · `room/engine.go:166`（宿主侧 `host.go:225-281`）

**现象.** `runtimeFor` 按 `tenant|room` 往 `e.rt` 里塞 `*runtime`，**从不删除**；宿主的 `rooms` map 也一样只增不删（关会不移除）。

**为什么会出问题.** 房间数与进程生命周期内创建过的会议数同增，每场会常驻一个 `runtime`（含 `waiters` map）+ 一份可观的 `snap`/`ops` 内存。S1 是"本机开会"，短期没影响，但 `agchat room new` 用一年就是几千个常驻条目。

**怎么修.** 关会/结束且无在飞请求时把 `e.rt` 与 `h.rooms` 条目驱逐（给引擎加 `Release(tenant, room)`），或至少在 `Close` 之后打标，让 `deadlineDue` 永久短路即可释放 watcher。

**回归测试.** 弱（资源类）：创建 100 个房间 → 全部 close → 断言 `len(h.rooms)`/`len(eng.rt)` 有界。

---

### HOST-22 · 低 · `host.go:1759`（`1766`）

**现象.** `authByToken` 用 `context.Background()` 查 `token_directory`（无视请求 ctx），返回的 `ref.TenantID` 收下但**从不校验**（宿主恒为 `local`）。

**为什么会出问题.** 请求被取消后这次查询仍然跑完（长轮询场景下这是每次调用都要多耗一次DB往返且不可中断）；`tenant` 不校验意味着同一个 DB 文件若被 agchatd 的别的 tenant 用过，token 也能被宿主接住（`roomByID` 兜了一层，所以现在只是隐患，不是漏洞）。

**怎么修.** `authByToken(ctx, token)` 透传 ctx；`if ref.TenantID != TenantID → 401`。

**回归测试.** 能：用一个 `tenant_id != local` 的 token 调 `/v1/head`，断言 401；ctx 取消用例可用带 `Deadline` 的 ctx 断言返回 `context.Canceled`。

---

### HOST-23 · 低 · `host.go:1250-1252`（+ `invite.go:533-538`）、`host.go:1299`、`host.go:953`

**现象.** 三处死码/装饰：① `Join` 里 `rv != Rules` 的 409 `rules_mismatch` 分支不可达 —— `ParseAt` 内部的 `checkPayload` 已经先以 `ErrRulesMismatch` 失败（`invite.go:533`），`join` handler 于是回 **400 `card_unusable`**；② `caps` 变量在 `1299` 被 `_ = caps` 吃掉（座位能力实际取自 `card.P.Caps`，见 HOST-08）；③ `Issue` 写死全 5 个能力位（`953`），所以宿主签出的卡永远不会被 `admit` 拒。

**为什么会出问题.** ① 与 §10.1④ 明确要求的"rv 不匹配归第三类（不是卡对不上）"不符：症状是人话对了、状态码与 `code` 全错，验收/客户端按 code 分支就会走岔；② ③ 让"能力位只有一个入口"（A-9）在 S1 里事实上从未被行使，唯一的覆盖是 `TestAdmitCaps` 直接调函数。

**怎么修.** ① `Join` 里改成先 `Parse`（不判 rv）再自己判 rv → 409 `rules_mismatch`，或让 `parseWith` 把 `ErrRulesMismatch` 单独返回出来由 handler 映 409；② 用 `caps`；③ 给 `IssueReq` 加 `caps`（默认全给），让 `admit` 真的有机会拒。

**回归测试.** 能：造一张 `rv={2,1,1}` 的卡（`invite` 包里有现成签名向量手法）断言 409 `rules_mismatch`；再发一张 `caps:["join","read"]` 的卡，断言 `/v1/post` 403 `capability_denied`。

---

### HOST-24 · 低 · `host.go:1539-1546`、`1612`

**现象.** 每个改状态请求会做**两次**全量聚合读取（`peek` → `View` 之后 `syncLocked` 里再 `peek` 一次），且都在持 `rc.mu` 时进行；`tickPass` 也一样（`567`、`574`）。`Status`/`Head` 也各再读取。

**为什么会出问题.** SQLite 上一次 `loadAgg` 要读 rooms+agents+messages 三张表；房内消息一多，单条 op 的延迟随日志线性涨，而 `rc.mu` 是全场串行的（所有座位共用一个互斥量）。S1 规模能扛，但这是"轮询风平浪静时也不便宜"的结构性成本。

**怎么修.** 把 `syncLocked` 改成"由调用方传入 after 快照"：`View` 的闭包本来就能拿到 `agg`，在闭包里直接 `snapOf(agg)` 取 after，省掉第二次读；`tickPass` 同样。

**回归测试.** 弱：路径正确性由现有套件覆盖，性能可加 `-bench` 断言 `loadAgg` 调用次数（给 `store` 加计数或用 `tx` 计数）。

---

## 二、我确认过没问题的点

逐路由过了一遍（`server.go:29-45`），**没有漏鉴权的端点**，能力校验也没有绕开 `admit` 的地方：

| 路由 | 凭证 | 判据位置 | 结论 |
|---|---|---|---|
| `POST /v1/room` `/v1/invite` `GET /v1/card/{ref}` `POST /v1/close` `/v1/resume` `GET /v1/status` `/v1/rooms` `/v1/export` | ctl token | `hCtl` → `isCtl`（`server.go:136-146,148-151`） | OK（close 另见 HOST-01） |
| `POST /v1/join` | 签名卡或 ref+pub | `host.go:1236-1265`（ref 侧另见 HOST-05） | 设计如此 |
| `GET /v1/head` `GET /v1/next` `GET /v1/result` | agent token | `authByToken` + `admit(CapRead)`（`server.go:305-313`、`432-438`、`export.go:37-39`） | OK |
| `POST /v1/post|propose|vote|summary` | agent token | `applyOnSeat:1520-1522` `capForKind` | OK |
| `GET /healthz` | 无 | 只回 `ok`+`version`（`server.go:382-385`），不含议题 | OK，且不 tick |

其余已确认正确的：

- **Host 头主防线有效**：`evil.example.com:PORT`、`127.0.0.1:PORT.evil.com` 均 403（HOST-14 只列口径缺陷）。
- **一个 CORS 字节都不写**（`writeJSONResp:103-112`，`export` 自己那套 `278-283` 也没写）。
- **`ctl` token 恒时比较**（`subtleEqual:161-170`），且 token 只在内存与 0600 文件里，不进卡、不进日志。
- **roomKey 私钥/会议密码不进卡、不进 `ops.jsonl`**（`Issue` 的 payload 946-956 只有 `pub`；`roomSecret` 单独落 0600）。
- **卡是"先结构、再验签、最后比身份"**：`ParseAt` → `card.Verify(rc.pub)` → `VerifyRoom`（`1236-1249`），顺序与 §2.4 一致；`ids` 非空在 `checkPayload:486-488` 已保证，`IDs[0]` 安全。
- **uses 按"已占座位"计**（`1366-1380`），不是信卡上的 `uses`；`invite_exhausted` 码稳定。`joinKey` 重放返回同一座位、不重复扣（`1302-1309`），有现成用例。
- **op 前置条件齐全**：`op.O == kind`、`op.A == s.AgentID`、`i/ph` 必须接链头（`1558`，并用 `with("head",...)` 把新链头带回去）、`h` 重算比对（`1566`）、对 `op.H` 验签（`1569`）、`s` 不入 hash。
- **日志先行 + commit 标记 + 拒绝回滚**（`intent` → 引擎 → `commit`/`rollback`，`1574-1596`），`loadOpLog` 恢复时把未提交尾巴截掉（`107-138`），`nextIndex` 还有"只有 commit 没有 op"的兜底（`183-187`）。单写入者 + `O_APPEND` + 每行一次 `Write`（`269-285`）。
- **`ops.jsonl` / `seats.json` / `host.key` / `control.token` 全部原子写**（`writeFileAtomic`：tmp + rename）。
- **长轮询不持房间锁**：`Next` 只在 `Inbox` 前后各拿一次 `rc.mu`（`447-453`），`Inbox` 本身也只持引擎 `rt.mu`。
- **无凭证端点不 tick**：`Status`/`Result`/`head` 全走 `PeekReadOnly`（`ops.go:1245-1250`），只有 `next`/`join` 与写端点会 tick。
- **`PeekReadOnly` 与 `View` 的差别被正确使用**，`syncLocked` 只在真跃迁时写 advance（比较 5 个字段，`598-599`）。
- **`deadlineDue` 是纯内存整数比较**，未到期房间零 DB 访问（`532-560`、`512-514`）；ticker 单例、`time.Ticker` 有 `Stop`（`504-505`）。
- **`rc.snap` 的读写基本都在 `rc.mu` 内**；`authByToken` 返回座位**副本**（`1777-1779`）避开共享指针。
- **`toOpErr` 只透传 `*domain.Error` 的状态码/码，不复写**（`1474-1487`），业务判断确实都在 `room` 包里（宿主没自造计票）。
- **JSON 响应三件套齐**（`content-type`/`no-store`/`nosniff`）、`SetEscapeHTML(false)`；`MaxBytesReader` 生效且超限已映射 413（并发改动里刚加的）。
- **`/v1/export` 的 bundle 只含已提交前缀 + acks + 两个指纹**，`expect` 与 `result` 同源（`export.go:124-188`）。
- `go vet ./internal/host/...` 干净；`go test ./internal/host/...` 基线绿。

---

## 三、最该先修的三条

1. **HOST-01**（`host.go:1160`）—— 一行的事，`close` 现在 100% 坏，"房主提前收会"整条链（`end{admin_close}`、`reasonFor` 三个分支）随它一起是死的。先改这个，因为它的修法没有设计歧义。
2. **HOST-03**（`host.go:1525`）—— 幂等缓存把 `ok:false` 当成功，客户端会**在拿到 200 之后把发言丢掉**，而且跨 resume 存活。这条最容易被误当成"偶发丢消息"排查掉。
3. **HOST-02**（`server.go:443`）—— 长轮询空转：`wait` 参数、引擎 `waiters` 广播、"空闲 CPU ≈ 0"三样一起失效，而它正是 S1 里"别让 agent 自旋"的唯一机制。修它会牵出 `roomCtx` 游标，建议排在 HOST-01/03 之后单独一轮。

> 建议顺带一起处理的近邻问题：HOST-04（resume 时钟叠加）与 HOST-05（ref+pub 认回）。前者会让"恢复即超时/时长失真"，后者是当前唯一的鉴权缺口，且两者都在同一条"resume/join"路径上，`docs/09` §10.5 与 §1 已有约束可依。
