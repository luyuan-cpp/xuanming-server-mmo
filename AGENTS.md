# mmorpg AI 协作守则

> 给本项目所有 AI Agent(Claude Code / Cursor / Copilot 等)的工作守则,人类开发者同样遵守。

## 1. 第一原则

**AI 没有跨会话记忆**。每次新会话动手前必须按序读完:

1. `PROGRESS.md` —— 当前进度
2. `CLAUDE.md` —— 项目规范(宪法)
3. `.github/copilot-instructions.md` —— 架构/构建/编码细则
4. `docs/design/<相关服务>.md` —— 任务相关设计
5. `git log -20 --oneline` —— 最近改动
6. 当前打开的 PR / Issue

**没读懂就动手 = 失忆人改代码**,会出大问题。

## 2. AI 能做的

写代码(C++ / Go / Java / proto / yaml / shell / ps1)/ 文档 / 测试;跑本地 docker-compose / dev_tools.ps1;建议 commit message 与 PR 描述;代码审查 / 设计评审;分析 `stress_summarize` 输出表。

**编译除外 —— 见 §3 与 §4.1:Claude 只改代码,构建/测试由 Codex 执行。**

## 3. AI 不能做的

- ❌ **Claude 不执行编译/测试**:`msbuild` / `dotnet build` / `go build` / `go test` / `go vet` / `mvn` / `cmake` / `build.bat` / `build_linux.sh` 等一律交给 Codex(细则见 §4.1)
- ❌ `git push` / `git tag`(人手动推);`git commit`(除非用户明说"帮我 commit")
- ❌ 登录任何远端账号(GitHub / k8s / 云厂商 / 注册表);改 CI 凭证 / secrets
- ❌ 写 secret / token / 密码到 git 跟踪文件
- ❌ `kubectl apply` 到生产(只能本地 / 指定 dev 集群);`docker push` 到 registry
- ❌ 读 `client/` 目录
- ❌ 用 `--no-verify`、注释断言、跳 test 等方式绕过安全检查

## 4. AI 执行方式

默认 **直接执行**:读 §1 → 改代码/proto/yaml/脚本/文档 → 跑 build/test/lint → 汇报改动范围 + 验证 + 剩余风险。需 commit 时等人发话。

遇 §3 禁令、§6 红线,或要装/升级工具、改系统环境、写 secrets、碰生产、push/tag、改 30+ 文件 → **立刻停止报告**,等授权。

### 4.1 编译 / 压测协作

**分工:Claude 只改代码,Codex 执行编译 / 测试 / 压测。**

Claude 改完后必须给出 Codex 可直接执行的细节:目标工程或服务、具体命令、工作目录、环境变量、前置清理、期望产物、通过标准、失败时要保留的日志或摘要。多工程要注明先后顺序;**C++ MSBuild 必须串行 `/m:1`**(并发会报假的 C1041 / LNK1104)。

Codex 按这些细节执行,不自行脑补压测口径或宣称性能结论。

Claude 在拿到 Codex 的编译结果之前,**不得声称「编译通过」「测试绿」「已验证」**;交付说明必须如实写明"未编译,待 Codex 验证"。这条与 §5「不假装成功」是同一条纪律的两面。

## 5. 失败时怎么办

不"假装成功"(老实报错)、不自动重试 5 次(报错后等决策)、不绕过失败(注释断言/跳 test)、不擦屁股式 `git reset / checkout --` 销毁进度。

## 6. 触碰红线 → 立刻停止 + 报告

任务范围明显扩大或漏关键文件 / 规范文档自相矛盾 / 要改 30+ 文件 / 要写 secrets 进 git / 要改系统环境 / 要关防火墙 / build 改坏别的服务 / 即将 push 远端。

## 7. 压测纪律

以 `CLAUDE.md §6` 为准。**核心**:跑测前必有 `prev-summary.txt` 且清空 redis/mysql/etcd/kafka offset;至少 3 次 prom snapshot(ramp 完/稳态中/稳态末);只读 `stress_summarize.ps1` 五段二维表,不手 grep raw prom;没对比表不准声明"性能提升";压期不上传日志;登录压测前清空 redis/mysql/etcd 数据。

## 8. 决策记录

- 大决策 → `docs/design/`(架构总图)
- 服务级 → `docs/design/<service>.md`
- 压测结果 → `docs/design/stress-<round>-*.md`
- 进度 → `PROGRESS.md`(只追加,不删旧的)

**没写文档 = 没说过**。

## 9. 中文回复

继承 `CLAUDE.md §2`:所有对话产出、注释、commit、文档全中文。
