# 发布手册（Release）

> 仓库本身已经具备发布条件：依赖文件齐、无敏感串、测试与压测全绿。
> **唯一需要人来做的一步是 GitHub 登录**（凭据不在代码里，也不该在代码里）。

## 一、发布前自检（一次跑完）

```bash
cd server
gofmt -l .                    # 必须无输出
go vet ./...
go test ./... -count=1        # internal / isolation / meeting
go test -tags stress ./stress # 20 场 × 6 人 × 10 轮（发版前跑一次）
```

再跑两条验收脚本（会起真实 HTTP 服务）：

```bash
go build -o .bin/agchatd.exe ./cmd/agchatd
./.bin/agchatd.exe -addr 127.0.0.1:8799 -db ../.run/accept.db &

node ../tools/acceptance-meeting-entry.mjs http://127.0.0.1:8799/api
node ../tools/acceptance-meeting-full.mjs  http://127.0.0.1:8799/api
```

最近一次的完整验收记录见 [`docs/07-会议流程规格v2.md`](docs/07-会议流程规格v2.md) 的「验收」小节（含 meeting 285s / isolation 3.9s / stress 107s 的真实耗时）。

## 二、发布到 GitHub（需要主人本人执行一次）

```bash
# 1) 登录（浏览器授权一次即可；也可以用 GH_TOKEN 环境变量）
gh auth login

# 2) 建仓库并推送（仓库名可换）
gh repo create agchat --public --source . --remote origin --push

# 如果你已经在 GitHub 上建好了空仓库：
git remote add origin git@github.com:zhanpengcarry/agchat.git
git push -u origin main
```

> 当前状态：本机 `gh auth status` = 未登录，环境里也没有 `GH_TOKEN` / `GITHUB_TOKEN`，
> 所以推送这一步**只能由账号主人完成**，不是代码问题。

## 三、`docs/` 的收录策略

本仓库默认只收录**技术契约类**文档：

| 文件 | 是否入库 | 原因 |
|---|---|---|
| `docs/07-会议流程规格v2.md` | ✅ | `server/README.md` 直接引用它（"实现与测试的唯一依据"），不入库会造成悬空链接 |
| `docs/06-数据隔离验收报告.md` | ✅ | 支撑 README 里的隔离承诺（四层隔离 + 26 条黑盒用例） |
| 其余 `docs/`（调研 / 商业计划 / 护城河 / 团队稿） | ❌ 本地保留 | `.git/info/exclude` 里的约定：内部材料不进公开仓库 |

如果你要连商业计划一起公开，把 `.git/info/exclude` 里的 `/docs/` 那行删掉，然后
`git add docs/` 即可；反过来如果要连 07 也不公开，请同步修改 `server/README.md` 第 6 行的引用。

## 四、打标签

```bash
git tag -a v2.5 -m "v2.5: 方案可修订 + 机读结论 decisionSet / 结论指纹"
git push origin v2.5
```

## 五、回滚

```bash
git revert <bad-commit>
# 数据侧无迁移回滚脚本：006/007 只加列（NOT NULL DEFAULT ''），旧代码读写不受影响，
# 因此回滚代码不需要回滚库。
```
