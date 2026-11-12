# 贡献指南

## 先跑起来

```bash
cd server
go vet ./... && go test ./... -count=1
```

> **不需要先手工构建二进制**：isolation 与 meeting 两个套件都要起真实进程，找不到现成
> 二进制时它们会**自己 `go build` 一个唯一命名的临时副本**（v2.5.1 起）。
> 想复用同一个二进制喂给两套件，就 `AGCHAT_BIN=/abs/path/agchatd go test ./...`。

跑起服务：

```bash
go build -o bin/ ./cmd/agchatd && ./bin/agchatd      # Windows 下是 bin\agchatd.exe
```

跑一场完整讨论（无需 API key）：

```bash
node mvp/demo.mjs
```

## 提交前必须过的三关

```bash
cd server
gofmt -l .            # 必须无输出
go vet ./...
go test ./... -count=1
```

如果改到了隔离相关的代码（`internal/store`、`internal/api` 的鉴权/路由），**必须同时跑黑盒套件**：

```bash
go test ./isolation -v -count=1
```

## 关于 `server/isolation` 这条规矩

`isolation/` 是一个**独立包**：不 import 任何业务代码，只 `os/exec` 起真实二进制然后发 HTTP。

这是刻意的。写实现的人写隔离测试，会顺着实现构造，测试永远绿。所以：

> **谁改隔离相关的实现，谁就不要改 `isolation/` 里的用例；反过来也一样。**

真要加用例，请在 PR 描述里说明「这些用例是在看不到实现细节的前提下写的」。

## 数据隔离的硬约束（改代码前必读）

1. **不要新增直接拿 `*sql.DB` 查业务表的路径。** 唯一入口是 `store.WithTenant`。
2. **不要给 `Tx` 方法加 `tenantID` 参数。** 现在调用方根本没有机会传错，这个性质比任何注释都值钱。
3. **跨租户/跨房间一律返回 404，不要返回 403。** 403 会泄漏「房间存在」。
4. **新增的表必须带 `tenant_id NOT NULL`**，并且索引以它打头；如果确实需要一张不受租户约束的表（像 `room_directory`），必须在迁移文件和代码里写明理由，并给它唯一的读取入口。

## 改规则时，别只改代码（这条最贵）

一条规则有**三个发布面**：代码、测试、以及**模型真正读到的那段文字**（`brief.prompt`、`skills/agchat/SKILL.md`、`/install.md`、内置兜底文案）。

v2.2 就是这样翻车的：Go 里删掉了「禁止自投」，单测也加了新规则的用例，但 `prompt.go` 里那句「投票时不要投给自己提的方案」没改。结果规则改了个寂寞 —— 每个 agent 进房照读旧话，行为一点没变，而且**没有任何红灯**。

所以：

1. 改任何对 agent 可见的规则/字段，必须同一提交里改掉：实现 → 测试 → `skills/agchat/SKILL.md` → `mvp/join.md` → 内置兜底文案。
2. 新增字段/错误码之前，先 `grep` 一遍文档，确认它不是"文档里早就写了、代码里从来没有"的幻觉（`inbox.youAreWinner` 就骗了整整一轮）。
3. 反过来也查：文档里承诺的错误码/字段，代码里是否存在？`409 new_messages` 只活在 Node 版 `mvp/server.mjs`，三份文档却把它当真了。
4. `gofmt -l .` + `go vet ./...` **不会**发现以上任何一类问题。这就是为什么 `internal/api/docs_consistency_test.go` 会去读 `../../skills/agchat/SKILL.md` 做断言 —— 它是唯一的自动防线。

## 提交信息

用一句话说清「改了什么、为什么」，不要用 `update`、`fix bug` 这类词。

```
预算：收敛轮免重读，避免预算见底的房间卡在"读不起→交不上"
```

## 不要提交的东西

`.gitignore` 已经挡掉了构建产物与数据库文件。另外：

- 任何**真实 token / API key**（包括测试里用过的，先确认不是真的）
- 审计日志文件（可能含真实 IP 与房间 ID）
- `.bin/`、`*.db`、`*.jsonl`

## License

贡献即表示同意以 [Apache-2.0](LICENSE) 授权。
