# 审计报告 2 · 邀请卡 / 操作日志 / 离线验签 / CLI（对抗性）

- 审计对象：`D:\code\AI智能体\agchat`（工作树 HEAD + 当时未提交改动）
- 判定基准：`docs/09-本地宿主规格-S1冻结.md` §2 / §4 / §6 / §10（先说 §2.1、§2.2、§2.4、§4.1、§6、§10.1、§10.2、§10.9）
- 范围：`server/internal/invite/`、`server/internal/oplog/`、`server/internal/verify/`、`server/cmd/agchat/`（宿主侧只读到"与这四块接口相关"的位置）
- 纪律：只报告不改代码；每条都用 `文件:行` 取证；带「实测」的条目由一次性探针跑出来（探针已删，源码见附录 A）；标「未验证」的是读代码得出但没跑通的推断
- 基线：`go test ./internal/invite ./internal/oplog ./internal/verify -count=1` 三包全绿；`go vet` 未跑
- **工作树在本轮审计期间被其他 agent 改过**（`commands.go` 16:11、`client.go` 16:09、`host.go` 16:09）。行号已按改动后的现状重新锚定；功能面改动只有 `exitAuth=5` 与 `decodeBody` 的 413，不影响本报告任何结论。另有一份别人留下的 `server/internal/host/zz_audit_probe_test.go`（未跟踪，`go test ./internal/host` 目前是绿的），不是本次产出。

严重度口径：**高** = 能让人得到错的身份/错的议题/错的判决，或让规格明确要求的检查整条失效；**中** = 需要一点点构造或需要另一个缺陷配合；**低** = 稳健性/一致性/可维护性。

条数：26 条（高 6 / 中 7 / 中低 3 / 低 10）。A1–A8 邀请卡、A9–A15 操作日志与证据包、A16–A19 离线验签、A20–A26 CLI。

---

## 一、邀请卡（`internal/invite`）

### A1（高）机读块取"第一个 begin/end 对 + 第一次出现的前缀"，一张前置伪造卡可以把真卡整段顶掉

- 位置：`server/internal/invite/invite.go:345-359`（`extractMachine` 用 `strings.Index` 取**第一个** begin 与**第一个** end）、`invite.go:387-391`（`splitMachine` 用 `strings.Index` 取**第一个** `agchat-invite:v1:`）、`server/cmd/agchat/client.go:344-357`（`--card` 传文件名时读整个文件后交给 `Parse`）
- 现象：「恰好一个机读块」从未被要求。解析器只看第一段，后面那一整张真卡被**静默丢弃**，不报错、不告警。
  - 实测 ①：`Parse(伪卡 + "\n" + 真卡)` → 成功，返回的是伪卡（`th=247100b71de90f69`，真卡 `th=b0245751458b9682`）。
  - 实测 ⑪：更能骗过人的版本 —— 人读块逐字抄真卡（含真卡的 8 位短指纹和真议题），**只换机读块** → 仍成功，解析到伪卡的 `th` 和伪卡的 ref（`S_A1v__U` vs 真卡 `QILHi5ss`）。
- 利用方式：攻击者拿到一张真卡，把真卡的人读块原样保留，把自己那张卡的机读块（指向自己的宿主、自己的 `ep`）拼在前面发出去。B 侧 `Parse` 通过、`cardEndpoint()` 取到伪卡的端点（`client.go:359-368`），于是 B 连到攻击者的宿主；攻击者的宿主用自己的 `rc.pub` 验签 —— **验得过**（卡是攻击者自己签的），`VerifyRoom` 也过，同意门三行显示的是攻击者房间的议题。整条链上没有任何一处能发现"B 的目标其实是攻击者"，除了人眼看到两段卡。
- 怎么修：`extractMachine` 要求 begin/end **恰好各一次**且成对；`splitMachine` 在剥离后的整串里要求前缀**唯一**（多于一个 → 新的人话「卡里有多段机读块，无法判断用哪段」）。若想更宽容：把每段都解析出来，交给 `Verify(pub)` 决定用哪段（解析层没有公钥，所以"选哪段"必须下沉到验签层，见 A3）。
- 回归测试：能。`Parse(fake + real)` 与 `Parse(realHumanBlock + fakeMachine)` 两条，断言 `ErrTruncated`（严格版）或断言 `Verify(pub_real)` 失败/成功的选择规则（宽容版）。现有 17 条黄金向量没有一条是"两张卡"。

### A2（高）人读块与 `th` 的交叉校验根本没实现；`th` 是纯写死字段

- 位置：`server/internal/invite/invite.go:238-255`（`CheckHumanBlock`，注释自己承认「**不一样**人读块里的议题与 payload 的 th 一致」）、`server/internal/host/host.go:951`（宿主唯一一处算 `th` 的地方）、`server/internal/host/host.go:1053`（人读块里写议题原文）；全仓 `P.TH` 除 `invite` 包内自检外**零消费者**
- 现象：§2.1 最后一行要求"人读块里的议题必须与 payload 的 `th` 一致；不一致 → 篡改"。实际上 `th` 只是被算出来、签进去、存着，没有任何一方（CLI 或宿主）拿它和任何东西比。`CheckHumanBlock` 只查 ≤500 字符和"含 ref 前 8 字符"——而 ref 前 8 字符攻击者本来就控制。
  - 实测 ③：`Parse("[人读块] 议题：完全无关的另一件事\n" + 真卡)` → 成功，`payload.th` 仍是真议题的哈希，没人报不一致。
- 利用方式：原样转发真卡的机读块（一个字不动，签名有效、`th` 有效），只把人读块改成另一个议题，例如「董事会席位怎么分」。B 的 agent 看到的人读块是假的；如果 B 走 A1 的路径连上也照样签约成功。附带副作用：`consent` 门用的是**宿主的** `r.Topic`（`host.go:1450`），所以它不会被骗——但人读块是 B 唯一能带去别处核对的文本。
- 怎么修：`CheckHumanBlock` 增参 `topic string`，内部比 `TopicHash(topic) == p.TH`；`IssueLayered` 的调用方传真议题。接收侧再加一条软校验：人读块里若能抽出议题，就与 `p.TH` 比对，不一致走新的第三类人话「人读块被改过（议题对不上）」。
- 回归测试：能。黄金向量加一条"人读块议题与 th 不符"，期望 `bad_signature` 类的新哨兵。

### A3（高）判定顺序被提前执行的字段检查污染：`rv`/`exp` 能在**签名没验**的情况下定成第三类

- 位置：`invite.go:293-298`（`Parse` 里先 `checkPayload` 再 `checkJoinBy`，都在验签之前）、`invite.go:533-538`（rv 不符直接返回 `ErrRulesMismatch`）、`invite.go:559-563`（过期返回 `ErrExpired`）、`invite.go:508-516`（exp/iat 合理性）；**连带死代码** `server/internal/host/host.go:1250-1252`（join 里那条显式 rv 判定永远到不了）
- 现象：`Parse` 不下发"待验签"，而是立刻做完整字段自洽。字段错误一律包成 `ErrBadSignature`，`rv` 和 `exp` 更是直接给第三类/过期类结论，**而签名一个字节都没验**。
  - 实测 ⑤：`rv=2` + **全零签名** → `ErrRulesMismatch`（人话「这是另一版规则办的会」）。
  - 实测 ⑥：`exp` 已过 + 全零签名 → `ErrExpired`（人话「已过进场时间」）。
- 利用方式：拿到任意一张卡，改一个字节（rv 或 exp 的明文字节），签名必然失效，但用户看到的是"另一版规则办的会"/"卡过期了，让对方重发一张"，而不是"卡对不上，内容被改过，别用"。这正是 §2.4 要避免的最坏 UX。宿主侧同样被打穿：rv 不符时 `Parse` 先失败 → 返回 400 `card_unusable`，而 `host.go:1255` 的 409 `rules_mismatch` 成为死路径。
- 怎么修：`Parse` 只做**结构**（marker/切段/base64/长度/JSON/字段类型）并保留原始字节；把 ref==sha256(key)、rv、iat/exp 合理性移到 `Verify(pub)` 成功之后做，或在 `Parse` 里返回"分类未定 + 命中项"，由调用方在验签后决定归类。同时删掉/接通 `host.go:1250`。
- 回归测试：能。黄金向量把 `rv_mismatch` / `expired` 两条改成"签名改坏"版本，断言得到 `bad_signature` 而不是规则/过期类。

### A4（高）`uses`/`seats` 自证缺失，`ids` 只认第 1 个（§10.1②③ 未落地）

- 位置：`server/internal/host/host.go:1256`（`inviteID = card.P.IDs[0]`，`ids[1:]` 直接丢）、`host.go:1365-1379`（余额只比宿主自己的 `iv.Uses`）；`card.P.Seats` **全仓零读取**（人读块里印了它，同意门的 `Seats` 取自活房间 `r.MaxAgents`，`host.go:1451`）
- 现象：§10.1② 要求 join 时校验 `uses == Σ maxUses(ids)`、§10.1③ 要求校验 `seats == create.seats`，两条都没有实现。卡上的 `uses` 只是展示；真正起作用的是宿主自建 `inviteRec.Uses`（`host.go:975`）。`checkPayload` 只做了卡内自洽（`uses ≤ seats`、都 ≥1，`invite.go:499-507`）。
  - 因此下面这张卡在任何 3 人房里都能过：`seats:99`、`uses:3`、`ids:["iv_a","iv_b"]`——只要 `iv_a` 还有余额。多 id 卡永远只用到第一张，`uses` 永远不可能被跨两张记录兑现。
- 利用方式：(a) 伪造/被改的 `seats`、`uses` 不进任何判定，A 事后补发卡也不会被"Σ 对不上"抓住（§10.1② 那句"否则 A 偷偷多发卡不可查"失去意义）；(b) 一卡多 id 的分发方式实际不可用（第二张起白写）。
- 怎么修：join 时按 `card.P.IDs` 全量查 `rc.inv`，缺 id → 拒；校验 `card.P.Uses == Σ inviteRec[id].Uses`；校验 `card.P.Seats == 当前 snap.MaxAgents`；扣减按 id 各自计数。
- 回归测试：能。`host_test.go` 已有 `TestJoinUsesExhausted` 的量级，加三条（`seats` 不符 / `uses` 与记录不符 / `ids` 含未知 id）即可。

### A5（高）`verified` 判决里用来"指认这场会是谁"的字段，全部没签名、也不和 create 载荷比对

- 位置：`server/internal/verify/verify.go:153-158`、`:278-292`（`checkRuleVersions` 只查 `resultSchema` 的值与另外两项**是否非空**）；`manifest.roomRef` 只被 `ack.go:62` 用来比 ack 的 ref；`Manifest.Topic / Seats / InviteIDs` 在 `verify` 包内**零引用**（grep 证据）；`VoteRule`/`HashDomain` 只做存在性检查（`host` 导出时写死 `"1"`，`server/internal/host/export.go:174`）
- 现象：`manifest.json` 与 `verify-target.json` 都是打包者自己写的甲骨文，而 `create` op 载荷（**签过名**，带 `ref/key/topic/seats/ids/rv`）就在同一份包里，却从不比对。
  - 实测 ①：把 `manifest.topic` 改成「完全无关的另一件事」→ 仍 `verified/ok`。
  - 实测 ②：把 `manifest.seats` 改成 99、`inviteIds` 换成 `["iv_x","iv_y"]` → 仍 `verified/ok`。
  - 实测 ③：把 `manifest.roomRef` 换成别人的 ref → `failed/ack_ref_mismatch`（acks 里的 ref 兜住了 ref 一项）。
  - 实测 ④：换 ref **并且删掉 acks** → `unverifiable/ack_missing`（掉出 `verified`，但也只是"证据不足"）。
- 利用方式：拿一份通过的 bundle，`unzip`，改 `manifest.json`（`topic` 改成另一件更耸动的议题、`seats` 改成"就我们俩"、或塞一批不存在的 `inviteIds`），`zip` 回去。`agchat verify` 仍报 `verified`，而 `verify-target.json` 里两个指纹**一个字都没动**，所以看起来"完全没被改过"。判决里那句"这场会"于是是假的。
- 怎么修：manifest 不要信文件，从 `create` op 载荷**现算**并与文件比对，任何一项不等 → `failed/manifest_mismatch`（新码，追加）。至少先做 `ref`、`topic`、`seats`、`inviteIds`、`rv` 五项；`voteRule`/`hashDomain` 在 `room` 侧导出常量后逐字比。
- 回归测试：能。`verify_test.go` 的 fixture 已能直接改 `b.Manifest.*`，照实测①②③各写一条断言即可（现在会失败，正好先红后绿）。

### A6（中低）裸机读行（无 marker）的尾随文本处理很脆

- 位置：`invite.go:412-417`（`trimSigJunk` 只在长度 >86 时从尾部砍到 86）、`invite.go:378-402`（切 `.` 必须恰好 2 段）
- 现象：没有 marker 时整段文本都进解析器（`invite.go:355-358`）。签名段的尾部垃圾只有"多余字母"这一种能被吃掉；尾随文本里只要有**一个句点**，就变成 3 段 → `ErrTruncated`。而纯中文尾随会被静默忽略，人看不出被吃了。
  - 实测 ⑦：`裸行 + "\n备注.18点前有效"` → `ErrTruncated`（卡其实完好）。
  - 实测 ⑦b：`裸行 + "\n备注 18点前有效"` → 成功（备注被吃，无提示）。
  - 实测 ⑦c：两张卡直接相连（无 marker）→ `ErrTruncated`（同 A1 的另一面）。
- 利用：不需要攻击者，用户粘"卡 + 一句带句号的备注"就得到「卡不完整，让对方重发」。
- 怎么修：无 marker 时先按行找唯一含前缀的行再切；或直接按正则锚定机读行 `^agchat-invite:v1:[A-Za-z0-9_-]{1,1024}\.[A-Za-z0-9_-]{86}$`，取第一处整行匹配，多余的按行丢弃。
- 回归测试：能（实测⑦⑦b 两条）。

### A7（中）解析侧不强制任何长度上限，`ids`/`ep`/`caps` 也没数量上限

- 位置：`invite.go:200-203`（签发侧才查 1024）、`invite.go:227-229` / `243-246`（只在 `IssueLayered`/`CheckHumanBlock` 里查 500）、`invite.go:266-300`（`parseWith` 无长度检查）、`invite.go:486-554`（`ids`/`ep`/`caps` 只查内容不查个数）
- 现象：`MaxMachineLineChars` / `MaxHumanBlockChars` 是"签发侧自觉"，接收侧一律放行。
  - 实测 ⑨：人读块 1 万字符 + 1000 个换行 → 成功。
- 利用：一张卡可以塞几百 KB（payload 段不受限，`ids` 可以上万条），宿主会照单全收并进 `consent`/日志；`--expose-lan` 档还有 4 MiB 请求体（`server.go:391`，本轮刚加的 413 只兜住更大的）。
- 怎么修：`parseWith` 里先断言人读块 ≤500、机读行 ≤1024 再继续；`checkPayload` 给 `ids`/`ep`/`caps` 各设上限（如 64/8/5）和单字段长度上限。
- 回归测试：能（实测⑨ + 超长 payload 段两条）。

### A8（中低）零宽字符被静默剥掉：可接受，但"人看到的字节"与"签名的字节"可以不同而无人提示

- 位置：`invite.go:363-371`（`keepMachineRune` 只留字母表 + `-_.:`）、`invite.go:381-385`
- 现象：实测 ⑧ —— 往 payload 段中间插 U+200B 仍解析成功（签名覆盖的是解码后的字节，字节没变，所以签名也对）。
- 影响：正面看是容错；反面看，一张卡的"可复制文本"和"被签名的内容"不是同一件东西，B 无法靠比对文本发现"我这串和别人那串不一样"。配合 A2/A3 会加深"看起来没变"的错觉。
- 怎么修：不改行为，但在 `Card` 上留一个"剥离了多少字符/是否剥过"的标志，`--json` 与同意门里回显，让"这串被加工过"可见。
- 回归测试：能。

---

## 二、操作日志与证据包（`internal/oplog`）

### A9（高）ack 收据的"漏记检测"没有闭环：`seats/<ref>.receipts.jsonl` 写了，从没人读

- 位置：`server/cmd/agchat/client.go:254-268`（`appendReceipt`，CLI 每收一条 ack 就落盘）、`client.go`/`commands.go` 里 `ReceiptsPath` 只出现在写入与 `--json` 回显（`commands.go:370`、`:451`）；`agchat verify` 只吃 `--bundle`（`server/cmd/agchat/verify.go:31`），没有 `--receipts`；`verify/ack.go:32-93` 的 acks 全部来自包里
- 现象：C-4 说收据用来判断"宿主是否漏记参与者 op"，但 acks 和 ops 出自同一个（可能是恶意的）宿主，两者自洽就查不出漏记。
  - 实测：删掉一整条参与者 `post`、连它的 ack 一起删、整链重封、声明值重算 → `state=verified`、`ackNote="ack 收据 6 条，覆盖参与者 op 6/6"`。
- 利用方式：恶意宿主漏记一条对自己不利的发言，重封链、重算 `transcriptHash` 写进 `verify-target`，产物完全自洽。`verified` 照给。参与者手上那份 receipts 是**唯一**能咬住这件事的东西，但它躺在参与者自己的磁盘上没被用过。
- 怎么修：`agchat verify --receipts <seats/<ref>.receipts.jsonl>`：收据里存在而 op 流里缺的 `(i,h)` → `failed/ack_orphan`；`a.T` 与 `op.T` 不等 → `failed`。缺 `--receipts` 时 `ackNote` 明说"收据来自包内，未与本地收据交叉"。
- 回归测试：能（实测路径即可复现：删 op + 重算声明值 + 提供 receipts 断言非 verified）。

### A10（中）同名条目静默"后者覆盖"，重复无检测

- 位置：`server/internal/oplog/bundle.go:209-233`（`for _, f := range zr.File` 按名字赋值，后一份直接覆盖前一份）
- 现象：实测 —— 两份 `ops.jsonl` → `err=<nil>`，只有 1 条 op，取的是**后一份**（`ops[0].I=2`）。
- 利用：塞两份 ops 流（一份让判决过、一份是真相），不同 zip 阅读器/不同实现的取法可能不同；即使同一实现，也永远只有一份生效，另一份是纯装饰。配合 A9（漏记）可以放一份"好看"的 ops 在前、被骑过的一份在后，或者反过来。
- 怎么修：同名第二份出现即 `ErrBadBundle`（或至少在 Report 里出一条 `duplicate_entry` 警告并声明"取最后一份"）。
- 回归测试：能（附录 A 的 `TestAuditProbeBundle` ①）。

### A11（中）常规压缩器打出来的包一律读不出：条目名只做精确匹配

- 位置：`bundle.go:210-216`（`switch name` 精确匹配，未知条目 `continue`），`bundle.go:234-236`（于是"缺必需条目"）
- 现象：实测 ② `bundle/manifest.json` → `缺必需条目`；实测 ③ 反斜杠前缀 `bundle\manifest.json` → 同样失败。
  - 而 CLI 自己的 `countOpsInBundle` 用 `filepath.Base(f.Name)` 匹配（`commands.go:909`）—— CLI 认得、`verify` 不认得，两边口径不一致。
- 利用/出错场景：最普通的转发方式（解压看一眼再打包、或用 `zip -r out.zip ./bundle`）产出的包，`agchat verify` 直接报 `bundle_unreadable`，用户会以为是文件坏了。
- 怎么修：匹配前先 `path.Base(strings.ReplaceAll(name, "\\", "/"))`；或者精确名优先、basename 兜底。
- 回归测试：能（实测②③）。

### A12（中）`manifest.expect` 一空就放弃与 `verify-target.json` 的交叉校验

- 位置：`bundle.go:271-276`（`if e := b.Manifest.Expect; e.DecisionHash != "" || ...`，空则整段跳过）
- 现象：实测 ④ —— 把 `expect` 改成 `{}`、`verify-target` 随便写 → `err=<nil>`，且采信的 `decisionHash` 就是 `verify-target` 里那个 `sha256:zz`。
- 影响：`expect` 与 `verify-target` 是同一份声明值的两份拷贝，这条"两处不一致就是被改过"的检查是包内唯一的自证；删掉 `expect` 就把它白送了。虽然 §10.2④ 说 verify 不信任 `expect`（真正判决靠重算），但少了这层，改包者可以只改一份而毫无痕迹。
  - 注意：`bundle.go:268` 的 `target.Ref != ""`、`:272` 的 `expect` 都存在"空即放行"，`ReadBundle` 也不要求 `manifest.roomRef` 非空（实测 ⑤：空 `roomRef` 也过，靠后面 `checkRuleVersions` 兜成 `unverifiable`）。
- 怎么修：`ReadBundle` 里 `manifest.roomRef` 必须非空、`expect` 两个字段都必须非空；缺 → `ErrBadBundle`。
- 回归测试：能。

### A13（中低）`CanonicalJSON` 的 Unicode/重复键口径没有冻结

- 位置：`server/internal/oplog/canonical.go:44-61`（解码进 `map`，重复键静默折叠）、`:75-83`（只查字面量里是否含 `.eE`）
- 现象（实测）：`{"a":1,"a":2}` → `{"a":2}` 无错；`3.0` → `3` 无错（`3.5` 才拒）；`1e3` 拒；NFC `café` 与 NFD `cafe\u0301` 字节**不等**；`Topic` 与 `topic` **不等**；全角 `ｔｏｐｉｃ` 与半角不等；非法 UTF-8 的 `\xff` 与 `\xfe` **变成同一份**规范字节（都进 U+FFFD）。
- 影响：`th` 的跨实现一致性、`decisionHash`/`transcriptHash` 的跨实现一致性都靠"唯一一份 CanonicalJSON"兜着（同仓没问题），但只要出现第二个产生方（第三方工具、S2 的 envelope、SKILL 里手写的载荷），NFC/重复键/坏字节就会让同一段语义算出不同字节。`3.0` 被悄悄接受也让"禁止浮点"看起来比实际严。
- 怎么修：规格里明确"文本必须 NFC、比较按字节"；`canonicalize` 用 `json.Decoder` 逐 token 走一遍并检出重复键（或解码成 `[]struct` 保序再查重）；把"拒浮点"的判据从字面量改成"来源类型必须是整型"。
- 回归测试：能（重复键、NFC/NFD、坏字节各一条）。

### A14（低）`oplog.VerifyChain` 不校验 `i` 连续，且返回位置的语义与 `verify` 的 `FailAt` 不一致

- 位置：`server/internal/oplog/oplog.go:466-497`（只校验 `ph`/`h`/签名，从不看 `op.I`；返回的 `pos` 是**切片下标**）、`server/internal/verify/chain.go:47-52`（`checkChain` 有 `o.I != pos` 判定）、`server/internal/verify/verify.go:74-76` 与 `:296-303`（`FailAt` 的语义是 **op 的 i 字段**）
- 现象：两个"链校验器"的判据不同。生产路径走 `checkChain` 所以现在不咬人；`VerifyChain` 只被它自己的测试调用（grep：`oplog_test.go:341/351/362/369/375/387`）。
  - 实测：重封后插入一条 `i` 重号的 op，`verify` 正确报 `chain_broken`；同一份流交给 `VerifyChain` 则会通过。
- 影响：谁先 import `VerifyChain`（S2 的 envelope 收集路径很可能）谁就得到一个"跳号也能过"的校验器；`i` 与位置不等时 `FailAt` 会指到另一条 op 上。
- 怎么修：`VerifyChain` 补 `op.I != pos` 判定；把文档里的"1-based 序号"明确成"op.I"，并在越界时给 `0`。
- 回归测试：能。

### A15（低）落盘行的编码与"载荷是规范化 JSON"不一致（`json.Marshal` 而非 `CanonicalJSON`）

- 位置：`server/internal/host/log.go:215-227`（`intent` 用 `json.Marshal(op)`，键序按结构体顺序、HTML 转义开着）
- 现象：`ops.jsonl` 里 `p` 的字节与 `CanonicalJSON` 输出不同（`<`/`>`/`&` 被转成 `\u003c` 等）。因为验签方会重新 canonicalize 再算 `h`，**目前不影响判定**；但 §4.1 规则 2 写的是"载荷本身是规范化 JSON"，文件里不是。
- 影响：任何人拿 `ops.jsonl` 直接对 `p` 做字节级比对/用别的语言算 `h` 都要先自己 canonicalize，否则算不对；也给"文件被改过"的取证留下噪声。
- 怎么修：`intent` 改成 `CanonicalJSON(op)`（`Op.P` 是 `json.RawMessage`，需要先在解码侧确认已规范），或落盘前对 `p` 做一次 `CanonicalJSON` 断言。
- 回归测试：能（读一行 `ops.jsonl` 断言 `p` 等于 `CanonicalJSON(p)`）。

---

## 三、离线验签（`internal/verify`）

### A16（低）`endPayload.Snap` 是死字段

- 位置：`server/internal/verify/replay.go:496-499`（`Snap` 带注释「兼容键，仅为了不误判」）、`replay.go:251-254`（只看 `p.Snapshot`）
- 现象：end op 只写 `"snap"` 时会走 `snapshot_missing` → `unverifiable`，与注释写的意图相反。
- 怎么修：`if p.Snapshot == nil { p.Snapshot = p.Snap }`。
- 回归测试：能。

### A17（低）`create` op 不受"必须是第一条"约束，多条 create 静默覆盖

- 位置：`replay.go:51-80`（`first := ops[0]` 只借 `T`；`case "create"` 每次覆盖 `Topic`/`MaxRounds`/…）、`chain.go:60-65`（`pubs[o.A] = keys` 每次覆盖）
- 现象：`ops[0]` 不是 create 也不报错（靠后面"没有公钥"降级成 `unverifiable` 兜住，不是靠判定）；第二条 create 会改掉议题与上限，`pubs["host"]` 换成新 key，**前面的 op 却仍用旧 key 验过**（验签发生在各自的迭代里）。`rv` 在 create 载荷里被解析（`replay.go:425`）但从不校验。
- 影响：`manifest` 与 create 已经分开（A5），这里再放开"哪条 create 算数"，等于没有可信的房间定义来源。
- 怎么修：断言 `ops[0].O == "create"`；出现第二条 create → `failed`（或 `unverifiable/duplicate_create`）；校验 create 载荷 `rv` == 支持版本。
- 回归测试：能。

### A18（低）断链/指纹不符时的 `FailAt` 语义与输出文案有错位

- 位置：`verify.go:186-190`（指纹不符时 `FailAt = endOpIndex(b.Ops)`，即"指向 end op"）、`verify.go:74-76`（`FailAt` 声明为 op 序号；0 = "无法定位"）
- 现象：指纹级不一致并不是 end op 的错，`FailAt` 却给了一个正数；同时 `server/cmd/agchat/verify.go:110-112` 只有在 `FailAt == 0` 时才说"无法定位到单条 op"。于是 `decision_mismatch` 会印成"第 13 条 op 处发现不一致"（实测 ⑩），而真正的破损可能在任意一条 post 上。
- 影响：人照着第 13 条去查，查不到东西。`FailAt` 这一列被"用满"了，失掉了"确实能定位"的区分度。
- 怎么修：指纹级不一致时 `FailAt = 0` 并保留"锚在 end 快照"的说明句；或新增 `Anchor` 字段专放 end op 序号。
- 回归测试：能。

### A19（低）`checkAcks` 的几处松动

- 位置：`server/internal/verify/ack.go:45-48`（`bySeq[a.I] = a`，同 `i` 的重复收据后者覆盖、不报）、`:56-60`（只比 `h`，不比 `a.T`）、`:62`（`a.Ref == ""` 时跳过 ref 比对）、`:41`（`seat != "host"` 的座位来自 join 载荷，join 由宿主签）
- 现象：`acks.jsonl` 里可以夹任意多条重复/多余收据，前一条被覆盖后永不校验；`a.T` 虽在签名域内（`oplog.go:409-416`）却从不与 `op.T` 比；`a.Ref` 空着可以绕开"是不是这场会"的检查（在 `roomKeyPub == nil` 时它还能顺带得到 `unverifiable` 而不是 `failed`）。
- 怎么修：重复 `i` → `failed/ack_duplicate`；`a.T != op.T` → `failed`；`a.Ref` 必须非空并等值 `manifest.ref`。
- 回归测试：能。

---

## 四、CLI（`cmd/agchat`）

### A20（中）`join --ref`（认回座位）完全不过同意门、也不给 exit 3

- 位置：`server/cmd/agchat/commands.go:341-343`（无 `--card` 且有 `--ref` → 直接 `reclaimByRef`）、`commands.go:487-491`（发 `/v1/join` 不带 `DryRun`，于是零值 false=真进）；宿主侧 `server/internal/host/host.go:1289-1298`（同意门整段在 `if card != nil` 之内）
- 现象：§7 v1.1 第 3 条写"`join` 不带 `--yes` 时必须打印同意门三行、退出码 3"。`--ref` 路径一条都不给：直接进场、exit 0。宿主也不记录"同意门是否展示过"，`DryRun` 完全由客户端决定，任何非 CLI 调用方都可以直接 `dryRun:false` 跳过。
- 利用/出错场景：agent 沙箱被换掉后 `join --ref` 一步到位，三个数字（几人/几轮/多少钱）从没被看见；验收脚本按 exit 3 判"未进场"时对这条路径失效。
  - 实测：`join --ref someRef`（本机无座位凭据）→ exit 1；有凭据时按代码直达 `joined`。
- 怎么修：无卡路径也调 `consentLocked`（`Owner` 用缺省"房主"），`DryRun` 时同样返回；或规定 `--ref` 必须显式带 `--yes`。宿主侧再补一条"同意门 token"最彻底。
- 回归测试：能（`host_test.go` 加"无卡 + dryRun=true 只回 consent"用例；CLI 侧断言 exit 3）。

### A21（中）`/v1/join` 没有持钥证明，知道 agentPub 就能拿到别人的座位 token（并顶掉原 token）

- 位置：`server/internal/host/host.go:1279-1286`（无卡路径只凭 `rc.byPub[pubB64(pub)]` 认人，请求里没有任何签名）、`host.go:1327-1334`（`Reclaim:true, ForceReclaim:true`）、`server/internal/room/ops.go:279-287`（引擎侧 `DeleteTokenDirectoryByAgent` + `RotateAgentToken`，旧 token 目录项被删）；`keys.go:49-59` 的 `joinKey` 也只用 `agentPub‖inviteID‖nonce`
- 现象：`agentPub` 是**公钥**，而 `/v1/join` 把它当成唯一凭据：任何人 POST `{ref, agentPub:<别人的>}` 都能拿到该座位的新 token。旧 token 随后被引擎删除。
- 利用：`agentPub` 出现在 join op 载荷里（导出包、以及任何拿到包的人手上）、在 `--expose-lan` 明文请求里也在线上。拿到它就能：(a) `GET /v1/result`、`/v1/next` 读完整个会场；(b) 把原 agent 的 token 顶掉，它的下一次调用 401。写操作仍要原私钥（`applyOnSeat` 用座位的 `s.pub` 验签，`host.go:1569`），所以不是完整冒充，而是**读取 + 抖动**；原 agent 得再 `join --ref` 顶回来，于是 ping-pong。
  - 未验证：旧 token 是否在**所有**场景下立刻失效（`authByToken` 查的是 store 的 token 目录，`rc.seats` 是内存表；`engine.Join` 的事务里删了目录项，但宿主内存里 `prev.Token` 也同步换了，两边一致，看起来确实失效——没有跑到端到端确认）。
- 怎么修：`/v1/join` 带一个 `proof`：`ed25519.Sign(agentPriv, "agchat-join:v1\x00" + ref + "\x00" + nonce)`（宿主用提交的 agentPub 验），至少 `--ref` 认回路径必须带。
- 回归测试：能。先 `join` 建座、再用同一 pub 从"另一个客户端"`join --ref`，断言拿到 token 且旧 token 401。

### A22（中）`--expose-lan` 档的卡在远端不可用：loopback 恒排第一且不回落

- 位置：`server/internal/host/host.go:1019-1023`（先无条件塞 loopback，再在 `ExposeLAN` 时追加 lan）、`server/cmd/agchat/client.go:359-368`（`cardEndpoint` 无条件优先 loopback，且只返回**一个**地址）
- 现象：B 在另一台机器上 `join` 这张卡 → 连到 `http://127.0.0.1:<端口>`，那是 B 自己的回环 → 连接被拒；代码里没有"失败再试下一个端点"。
- 影响：`--expose-lan`（§10.7 的四件套之一）的主路径直接不可用；而 S1 验收不覆盖它，所以不容易被发现。
- 怎么修：`cardEndpoint` 允许"本机不是房主"时优先 `lan`；更稳的是按顺序试完全部端点，只对 `401/403/409` 停下，连不通才换下一个。
- 回归测试：能（纯函数测 `cardEndpoint` 的顺序即可，不需要网络）。

### A23（低）退出码语义冲突：`2` 既是"无法判定"又是"参数用错"

- 位置：`server/cmd/agchat/client.go:29-34`（0/1/2/3/4/5 六个码，`exitUsage = 2`）、`server/cmd/agchat/verify.go:36`、`:40`、`:44`、`:79`（用法错、写 JSON 失败都 `return 2`）、`server/internal/verify/verify.go:96-106`（`unverifiable` 也 `2`）
- 现象（实测）：`verify` 无参数 → exit 2；`verify --bundle 不存在.zip --json` → exit 2（这次是判决）；`verify --bundle x.zip extra` → exit 2。三种完全不同的情形同一个码。同类：`join` 的结构性失败与认证失败都是 `exit 1`（`commands.go:349-355`，只有第三类给 `4`）。
- 影响：验收脚本把 2 读成"证据不足"时，一个打错的参数会被判成"无法判定"；而 `1` 兼了"失败"与两类不同的卡故障。
- 怎么修：用法错误换个专用码（如 64）；`join` 的三类失败按 `errors.Is` 分开给 3 个码（注意与 `exitConsent=3` 的语义不要撞名）。
- 回归测试：能（跑一次无参 `verify` 断言码）。

### A24（低）`--json` 的失败路径不产出任何 JSON

- 位置：`commands.go:348-355`（`join` 的任何失败都走 `fail()` → 只写 stderr，stdout 空）、`client.go:338-342`（`fail`）
- 现象（实测）：`join --card <截断卡> --json` → stdout 0 行、exit 1，只有一个中文句子在 stderr。
- 影响：§7 只承诺"成功时单行 JSON"，所以不算违约；但对脚本/agent，"失败"完全没有机读形态，只能解析中文（而 `verify` 的失败是有 JSON 的——两套风格）。
- 怎么修：失败也输出单行 `{"ok":false,"code":...,"human":...}` 到 stdout（或把"仅成功路径有 JSON"写进规格并在 SKILL 里声明）。
- 回归测试：能。

### A25（低）`ref` 直接拼进文件路径，不做字符白名单

- 位置：`server/internal/host/config.go:134-155`（`filepath.Join(home, "seats", ref+".key")`、`rooms/<ref>/…`），`server/internal/host/host.go:828-838`（建会时 `MkdirAll`）
- 现象：`--ref ../../x` 会走出 home 读/写，`MkdirAll` 还会在 home 之外建目录。卡里的 ref 永远是 43 字 base64url（`checkPayload` 强制自洽），所以只有手输 `--ref` 才会中招。
- 怎么修：`--ref` 入口做 `^[A-Za-z0-9_-]{1,64}$` 校验，或对解析结果做一次 `filepath.Clean` + 前缀断言。
- 回归测试：能。

### A26（低）`invite --uses` 静默夹到房间座位数且不回显

- 位置：`server/internal/host/host.go:926-928`（`uses > snap.MaxAgents` 直接夹成 `MaxAgents`）、`server/cmd/agchat/commands.go:316-319`（人读路径只打印卡，不打印最终 `uses`）
- 现象：3 人房 `invite --uses 9` 得到 `uses:3`，人看不出被夹过（`--json` 里有 `uses` 字段，人读没有）。
- 怎么修：夹了就 stderr 说一句"已按座位数夹成 N"。
- 回归测试：能。

---

## 五、确认没问题的点（对着攻击面逐条否掉）

1. **验签确实吃"收到的原始字节"**：`invite.go:318` 用 `c.RawPayload`，不是重新序列化；`TestParseFailClosed`（`invite_test.go:314-326`）显式断言"插空格后重签仍能验过"且 `RawPayload` 不被规范化。这是 A-1 的核心，做到了。
2. **签名域隔离到位**：卡域 `"agchat-invite:v1\x00"`（`invite.go:44`）、op 域 `"agchat-op:v1\x00"`、ack 域 `"agchat-ack:v1\x00"`（`oplog.go:29-31`），且 `oplog` 不提供通用 `Sign()`（`oplog.go:325-326`）—— 卡签名当不了 op 签名。
3. **`s` 不进 hash**：`Hash` 的入域记录只有 `i,t,o,a,p,ph`（`oplog.go:225-232`），`s` 不在其中；`Seal` 先算 `h` 再签 `h`。
4. **null / 浮点 / 空载荷 fail closed**：`CanonicalJSON` 拒 `nil`、拒 null 字段（`canonical.go:66-67`、`:106-110`）、拒带 `.eE` 的字面量（`:79-81`），`Hash` 拒空 `p`（`oplog.go:218-220`）。
5. **首条 op 的 `ph` 必须是 64 个 0**：`chain.go:41` + `chain.go:53`、`oplog.go:470-479`，所以不能从链条中段截一段冒充完整证据。
6. **链校验的判据是齐的**：`i == 位置`（`chain.go:47`）、`ph` 接上一条 `h`（`:53`）、`h` 重算（`:82`）、`s` 用**该作者的**公钥验（`:89-110`）；公钥只从 op 流自建，不回查库（B-13）。
7. **"不知道"没有被说成"通过"**：断链时 `decisionMatch/transcriptMatch` 保持 false 且 reasons 里明写"未比对成功，不代表不一致"（`verify.go:141-147`，实测 ⑨）；空 ops、缺快照、快照未 ended、缺指纹、规则版本不符、`advance` reason 枚举外、作者公钥无从核验，全部 `unverifiable`（`verify.go:127`、`:170-176`、`:238-243`；`replay.go:240-244`、`:267-287`）。`bundle_empty` 也没有被当成失败。
8. **ack 是 roomKey 签的（不是 hostKey）且必须与 op 的 `h` 对齐**，验不过判 `failed`（`ack.go:56-80`、`oplog.go:376-406`）；缺收据才判 `unverifiable`，两类分得清。
9. **zip 炸弹被兜住**：整包 64 MiB / 单条目 32 MiB / 单行 8 MiB（`bundle.go:26-30`），条目用 `io.LimitReader` 读（`:289`）；条目不解压落盘，所以**没有** zip slip 的写入面（`../../evil.json` 只被忽略，实测 ⑥）。
10. **卡上塞密码会被拒**：`payloadKeys` 是闭枚键集（`invite.go:170-173`），`pass`/`adminKey` 之类进不了卡，`TestParseFailClosed` 用 `"pass":"hunter2"` 钉住了这条；`Payload` 结构里确实没有任何长期秘密。
11. **端点枚举闭枚 + `lan` 必须 `ins:1`**（`invite.go:542-554`），`tunnel` 也算已知值。
12. **卡内自洽检查很密**：`v`/`api` 未知值、`ref != sha256(key)`、`ids` 空/重复/含空串、`uses<1`、`seats<1`、`uses>seats`、`exp<0`、`iat<=0`、`iat>exp`、`th` 非 16 位小写 hex、`caps` 空/未知/重复、`n` 空 —— 全部 fail closed（`invite.go:473-541`）。
13. **`rv` 不匹配归第三类**（不是"卡对不上"），且 `ErrRulesMismatch` 同时满足 `errors.Is(…, ErrWrongRoom)`；`HumanError` 的 switch 把 `ErrRulesMismatch` 排在 `ErrWrongRoom` 前面（`invite.go:652-655`），不会被外层顺序吃掉。分类问题只在 A3 说的"验签前"。
14. **`th` 的唯一实现**是 `TopicHash`（`invite.go:593-619`），空白/零宽折叠有测试钉住（`invite_test.go:52` `TestTopicHash`），没人在别处手写第二份归一化。
15. **`--json` 成功路径都是单行且不含密钥**：`printJSON` 用 `json.Encoder`（`client.go:332-336`），`/v1/head`、`/v1/result` 的服务端编码也是单行（`server.go:103-112`）；座位凭据里只有 `keyFile` 路径，`token`/`agentPriv` 不进 `--json`。`verify --json` 的 stdout 只有一行（实测）。
16. **控制面是 bearer token + 常时比较 + 0600**（`server.go:148-170`、`files.go:129-132`），`/v1/status` 要 token，`/healthz` 只有 `{"ok":true,"version":…}`（`server.go:382-385`），确实不含会议数据；`Host` 头校验 + 不写 CORS 头（`server.go:50-74`、`:107`）。
17. **`--card` 第三类失败在客户端就断掉**：`--ref` 与卡不符时不联网、走 `HumanWrongRoom` + exit 4（`commands.go:353-356`；实测 exit=4）。第三句话没有被吞。
18. **`sendOp` 的处理是克制的**：先 `GET /v1/head` 再签，`head_conflict` 只重试一次且复用 `opKey`（`commands.go:595-630`），不会无限重放；`op.a` 必须等于座位自己的 `agentId`（`host.go:1517-1519`），alias 不参与授权。
19. **`grep` 门禁的两个"单实现"约束没被破**：`refOf` 直接复用 `invite.RefOf`（`keys.go:45-47`），`advance` reason 集合只有 `oplog.KnownReason` 一份（`verify/replay.go:44-48` 不另抄表）。
20. **fsync/原子写**：`ops.jsonl` 追加即 `Sync`（`log.go:269-285`），bundle 用临时文件 + `fsync` + rename（`bundle.go:363-389`）。

---

## 六、最该先修的三条

1. **A5 + A1/A2 一起修**（`verified` 的身份面）：先把 `manifest` 的 `ref/topic/seats/inviteIds` 与签过名的 `create` 载荷对撞、不一致就 `failed`；再把"机器块唯一 + 人读块议题与 `th` 一致"补上。现在一份 `verified` 判决**说不清是哪场会、在议什么**，这是整个离线验签的立身之本。
2. **A9（收据走参与者自己的文件，做成 `--receipts`）**：不做这条，C-4 就是空转——恶意宿主漏记一条 op、重封链、重算声明值，`agchat verify` 照样给 `verified`（实测）。参与者的 `receipts.jsonl` 已经在写了，只差一个消费者和"收据有、op 流没有 → failed"这条判据。
3. **A3 + A4（把验签挪到字段自洽之前；补 `uses == Σ maxUses(ids)` 与 `seats == create.seats`、`ids` 全量参与）**：这两条决定"卡坏了会得到哪句话"和"卡能不能被 A 事后动手脚"。现在改一个 `rv` 字节就能让用户看到"这是另一版规则办的会"（签名根本没验），而卡上的 `uses`/`seats` 没有任何一方核对。

---

## 附录 A · 复现探针（临时文件，跑完已删）

三个包各放一个 `zz_audit_probe_test.go`，`go test ./internal/<pkg> -run TestAuditProbe -v` 即可看到本报告里的「实测」输出（都在 `t.Logf` 里，不断言）。要点：

```go
// invite：填卡用固定私钥，签名可换成 64 字节 0，专门探"验签前的分类"
func BuildRaw(p Payload, sig []byte) string {
    raw, _ := oplog.CanonicalJSON(p)
    return MachinePrefix + b64.EncodeToString(raw) + "." + b64.EncodeToString(sig)
}
// ① Parse(fake + "\n" + real)                        → 成功，返回 fake（A1）
// ⑪ Parse(realHumanBlock + fake)                     → 成功，th/ref 都是 fake 的（A1）
// ③ Parse("[人读块] 议题：完全无关的另一件事\n"+real) → 成功（A2）
// ⑤ rv=2 + 全零签名 → ErrRulesMismatch（A3）
// ⑥ exp 已过 + 全零签名 → ErrExpired（A3）
// ⑦ 裸行 + "备注.18点前有效" → ErrTruncated（A6） ⑨ 1 万字符人读块 → 通过（A7）

// oplog：同名条目进 zip 同一份里
// ① ops.jsonl 两行两份 → err=nil、只留后一份（A10）
// ② "bundle/ops.jsonl" → 缺必需条目（A11） ④ manifest.expect={} → 采信 verify-target（A12）

// verify：用包内 cleanFixture()
// ① 改 manifest.topic → 仍 verified；② 改 seats/inviteIds → 仍 verified（A5）
// ③ 改 manifest.roomRef → failed/ack_ref_mismatch；④ 再删 acks → unverifiable（A5）
// 删一条 post + 删它的 ack + 重封 + 重算声明值 → verified、覆盖 6/6（A9）
```
