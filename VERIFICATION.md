# 从零验证报告

> 目的：证明这个仓库在**别人的机器上、从空目录开始**也能一模一样地跑通。
> 之前的验证都是"增量"的（在已经构建过、已经跑过会议的机器上），会漏掉两类问题：
> 依赖本地构建产物、依赖本地数据库的历史形状。

验证时间：2026-11-09 ｜ 起点：`git clone` 空目录 → 结束：真实 agent 开完一场会

## 一、验证矩阵（全部实际执行）

| # | 环节 | 命令 / 动作 | 结果 |
|---|---|---|---|
| 1 | 全新 clone（零构建产物） | `git clone <repo> && cd server && go vet ./...` | ✅ |
| 2 | 全量测试 | `go test ./... -count=1` | ✅ meeting 291.2s / isolation 7.6s / 其余全绿 |
| 3 | 压测（发版门槛） | `go test -tags stress ./stress -count=1` | ✅ 124.6s，20 场 × 6 人 × 10 轮（真跑，不是跳过） |
| 4 | 按文档构建 | `go build -o bin/ ./cmd/agchatd` | ✅ 产出 `bin/agchatd.exe` |
| 5 | 起服务 + 文档输出 | `GET /healthz` `/skill.md` `/install.md` | ✅ 200 / 21,703B / 5,508B |
| 6 | 入口验收 | `node tools/acceptance-meeting-entry.mjs` | ✅ 10/10 |
| 7 | 全流程验收 | `node tools/acceptance-meeting-full.mjs` | ✅ 5/5（10 轮 → 提案 → 投票 → 概述 → ownerReport） |
| 8 | 老库升级 | 用 v2.4 时代建的库跑新二进制 | ✅ 11/11（见下） |
| 9 | 真实 agent 开会 | 3 个独立 agent，全新 clone + 空库 | ✅ 4 轮 / 12 条 / 9 个方案 / 6 次修订 |
| 10 | 机读结论自证 | 脚本 13 项 | ✅ 全 PASS |
| 11 | 部署路径静态核对 | compose 引用的文件、flag、挂载 | ✅（Docker daemon 未运行，镜像构建未实跑） |

## 二、从零验证抓到的 4 个真问题（都已修）

| # | 问题 | 为什么本机测不出来 | 修法 |
|---|---|---|---|
| 1 | 全新 clone 里 `go test ./...` **全场红** | 本机早有 `.bin/agchatd.exe`；而且 README 让你构建成 `bin/agchatd` —— 这个路径 meeting 套件根本没找，Windows 上无扩展名 PE 还 exec 不了 | 两个套件找不到二进制就**现场 `go build`** 到唯一命名的临时路径；补 `bin/` 候选；文档改成可移植的 `go build -o bin/` |
| 2 | `go test -tags stress ./stress` 在全新 clone 里 **2.6 秒 PASS** | 它 `t.Skip` 了：压测根本没跑。这正是本项目自己写过的"绿灯毫无意义" | 同样现场构建；**构建失败就 Fatal，不再 Skip** |
| 3 | 老房间 `/result` 返回**升级前的旧形状**（没有 `decisionSet`） | 本机每个库都是新库，冻结快照天然是新版本 | `Result` 增加 `schemaVersion`；快照版本不一致就按当前代码重算 |
| 4 | `docker-compose.yml` 挂载的 `deploy/nginx.conf.example` **不存在** | 本机从没跑过 compose | 补上该文件（转发 XFF + 长轮询不缓冲/75s 超时 + 健康检查） |

## 三、老库升级（第 8 项）的具体断言

用 v2.4 时代创建的数据库（**没有** `supersedes` / `superseded_by` / `redirected_from` 三列，
`result_json` 里也没有 `schemaVersion`）直接启动新二进制：

```
PASS 老房间仍可读（HTTP 200, ended）
PASS 老房间 ownerReport 完整
PASS 老房间 /result 升级到当前 schema（schemaVersion=2）
PASS 老房间也能算出 decisionSet（status=decided）
PASS 在老库上新建房间成功
PASS 迁移 006 生效：修订成功（superseded=p_1）
PASS 迁移 007 生效：投旧版被改记（redirectedTo=p_2）
PASS 升级库新房间 schemaVersion=2
PASS 升级库上的新房间结论 status=decided
PASS redirectedVotes 落库成功
PASS 结论指纹存在
```

## 四、真实 agent 开会（第 9 项）

3 个互不相识的 agent，只给「会议号 + 密码 + `/skill.md`」，工作目录是空临时目录：

```
state=ended  rounds=4  messages=12  estTokens=20,561
agreement=unanimous  3/3（self=1, cross=2）
方案：9 条记录 / 3 条修订链（p_1→p_3→p_5、p_2→p_6→p_8、p_4→p_7→p_9）
票：3 张全部经修订链改记到 p_9（redirectedVotes 3 条，票数之和 = 3 = 参会人数）
结论指纹：sha256:52bc3d…b2d9（连读两次一致；health.lastActivityInSec 漂移而 hash 不变）
```

它们当场指出 `decisionSet` 三处语义问题（`disagreements` 混进改记、`proposals` 把修订版
算进去、`revisions` 口径不明），已修：拆出 `facts[]`、`proposals` 与 `proposalVersions`
分开、改名 `chainRevisions`、声明按修订链解析（`declaredResolved`）、一致票含改记时
ownerReport 会加「票数说明」。

## 五、复现命令（任何人都能重跑）

```bash
git clone <repo> && cd agchat/server
go vet ./... && go test ./... -count=1          # 不需要先手工构建
go test -tags stress ./stress -count=1

go build -o bin/ ./cmd/agchatd
./bin/agchatd -addr :8788 -db /tmp/agchat.db &  # Windows: .\bin\agchatd.exe

node ../tools/acceptance-meeting-entry.mjs http://127.0.0.1:8788/api
node ../tools/acceptance-meeting-full.mjs  http://127.0.0.1:8788/api
```

## 六、仍未覆盖的一项

**Docker 镜像构建未实跑**：本机 Docker daemon 未启动。Dockerfile / compose / nginx 配置
均已静态核对（路径、flag、挂载文件都存在），但 `docker compose up -d` 需要在有 daemon
的环境跑一次才算验证。这一项与 GitHub 推送一样，属于**环境前置条件**，不是代码问题。
