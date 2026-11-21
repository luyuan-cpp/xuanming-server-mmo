# AI 协作约定与模型档位规定

> 对应根索引 [AGENTS.md §10](../../AGENTS.md#10-ai-协作约定)。新电脑或新 AI Agent 启动前请先完成必装 Skills 安装 → [required-skills.md](required-skills.md)。

在 [权限边界](naming-and-boundaries.md#2-禁止事项与权限边界9) 内，AI 可写代码（C++ / Go / Java / proto / yaml / shell / ps1）、文档和测试；可跑本地 docker-compose / `dev_tools.ps1`，建议 commit message / PR 描述，执行代码与设计评审，分析 `stress_summarize` 输出。不同 Agent 按 §1 分工。

## 1. 编译 / 测试 / 压测协作（§10.1）

**Claude 可修改代码、测试和文档，但不执行编译、测试或压测命令；这些命令由 Codex 执行。**

- Claude ❌ 不跑 `msbuild` / `dotnet build` / `go build` / `go test` / `go vet` / `mvn` / `cmake` / `build.bat` / `build_linux.sh` 等任何构建、测试或压测命令，也不为“验证一下”而跑。
- Claude ✅ 改完代码后，给出 Codex 可直接执行的细节：**目标工程/服务、具体命令、工作目录、环境变量、前置清理、期望产物、通过标准、失败时要保留的日志或摘要**。多个工程要说明先后顺序；C++ MSBuild 必须串行 `/m:1`，并发会报假的 C1041 / LNK1104。
- Claude ✅ 改了 C++ 源码时，交付前先在仓库根跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。它是只读的源码文本预检（几秒，不编译、不起服务），不在上面禁跑之列。类 / 结构体里不得有裸指针成员，违例会让 MSBuild 的 no-raw-pointer-member 门禁在首错处停下，整个工程不编不测；写法见 [cpp-runtime-and-build.md](cpp-runtime-and-build.md#3-c-类成员不放裸指针no-raw-pointer-member-门禁)。命中自己的文件就改，命中别的会话正在写的文件只告知对方；结论以门禁为准。
- Codex 按上述细节执行本地编译、测试、压测与汇总，不自行脑补压测口径或宣称性能结论。
- Claude 在没有 Codex 运行结果前，**不得声称“编译通过”“测试绿”“已验证”**；交付说明必须如实写明“未编译，待 Codex 验证”。

## 2. 执行与交付（§10.2）

- 默认直接执行：完成会话启动门禁 → 明确范围 → 改代码/proto/yaml/脚本/文档 → 由有权限的执行者跑受影响的 build/test/lint → 汇报改动范围、验证证据、未验证项和剩余风险。需要 commit 时等用户明确发话。
- 动手前先检查工作区和相关文件的既有改动；不覆盖、回退或顺手整理他人的工作，不夹带任务外改动。
- **写回主工作区（负责人 2026-11-10 定）**：会话自己的 worktree 已删、桌面应用的守卫仍拦对主工作区的编辑时，在草稿目录改副本；写回前核对主仓文件与复制时逐字节相同，不同就用 `git merge-file` 三方合并，有冲突即停下；`docs/PROGRESS.md` 只纯追加。写回后用 `git diff` 逐文件核对只多了自己的改动，交付说明写明是这样写回的。不要跳过核对，直接用重定向、`sed -i` 或移动文件覆盖主仓文件。
- 失败时不假装成功；命令失败后不连续自动重试，不注释断言或跳过测试，不用 `git reset` / `git checkout --` 销毁进度。保留关键错误与日志摘要并报告，等待决策。
- 请求触犯权限禁令，或任务范围明显扩大、遗漏关键文件、规范文档冲突、预计改动 30+ 文件、需要安装/升级工具、修改系统环境、关闭防火墙、写 secrets、触碰生产、build 损坏其他服务或即将 push 时，立即停止并报告，等待授权或决策。

## 3. 模型档位规定（§10.3，强制，负责人 2026-11-20 定）

- **修改代码必须使用各软件当时的最高档模型**，不得在低档模型上改代码：
  - Claude Code：`Fable 5.2 Max`
  - Gemini：`Gemini 4 Neon Extra`
  - Codex：`GPT 6.2 Astral Ultra`
- **编译、生成、跑测试与冒烟使用低档模型**（发现需改代码的问题时先保留首错原文停下，切回最高档模型再改）：
  - Gemini：`Carbon Medium`
  - Codex：`6.1 Sol Ultra`
  - Claude Code：`Opus 5 High`（按 §1 默认不跑构建命令，仅在负责人明确指定时用此档）
