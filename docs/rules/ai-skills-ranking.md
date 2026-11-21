# AI Agent Skills 梯队清单（618 个必装技能精选排序）

> **记录日期**：2026-11-21  
> **关联安装清单**：全量 8 仓 618 个技能明细与一键复原脚本见 [required-skills.md](required-skills.md)  
> **安装位置说明**：618 个技能本体均通过 `-g` 安装在**本机用户全局目录**（`~/.agents/skills/<技能名>/SKILL.md`，并同步到 `~/.claude/skills/`、`~/.gemini/config/skills/`、`~/.codex/skills/`、`~/.copilot/skills/`），不随技能文件本身提交进业务 Git 仓库；业务仓库只保留安装脚本与本梯队清单。  
> **排序依据**：对 AI 工程产出的杠杆效应、机制独特性、实战不可替代性。任何外部 Skill 与本仓 `AGENTS.md` 冲突时，一律以本仓 `AGENTS.md` 为准。

---

## S+ 级：镇仓神器（最建议先看这 6 个）

1. **`graphify`**（`Graphify-Labs/graphify`）— **唯一带本地 AST 解析 CLI 的知识图谱引擎**  
   把代码/文档/多仓库转成可查询知识图谱（社区聚类、God Nodes、`query/path/explain`、交互式 HTML、MCP）。对多仓跨语言服务端/客户端架构对账是降维打击。
2. **`ponytail`** + **`ponytail-audit`** / **`ponytail-review`**（`DietrichGebert/ponytail`）— **专治 AI 过度设计与整仓体检**  
   日常强压最小完整改动、拒写废抽象；`audit/review` 按「正确性 → 安全 → 并发 → 测试 → 性能 → 精简」全链路追调用方，不做表面 diff 评审。
3. **`verification-before-completion`**（`obra/superpowers`）— **反 AI 嘴炮/幻觉铁律**  
   强制“未在本轮亲手跑完验证命令并看完完整输出，严禁宣称通过/修好”，把各种“应该没问题”的借口直接拉黑。实测降返工率最强。
4. **`systematic-debugging`**（`obra/superpowers`）— **四阶段根因排障**  
   铁律是“没查到根因前禁止提修复”，强迫先复现、查近期变更、跨组件边界埋诊断日志，专治 AI 猜着改越改越烂。
5. **`subagent-driven-development`**（`obra/superpowers`）— **子 Agent 流水线 + 双门禁验收**  
   每个任务派全新上下文子 Agent 做，做完立刻过「规范合规 + 代码质量」两道审查，中途不频繁停下来问“要不要继续”，遇小歧义自主裁决并留档。
6. **`grilling`**（`mattpocock/skills`，别名 `grill-me`）— **设计树式无情拷问**  
   把需求拆成决策前沿树分轮追问，并附默认推荐答案；凡是能自己从代码库查到的事实派子 Agent 去查，绝不拿能查的事烦人。

---

## S 级：高杠杆架构 / 对抗验证 / 多 Agent 编排

| 排名 | 技能 | 来源 | 厉害在哪 |
|---:|---|---|---|
| 7 | **`doubt-driven-development`** | `addyosmani` | 写关键代码或定架构**前**，拉一个“以证伪为目标”的新鲜上下文审查者挑刺，打破长会话自我催眠 |
| 8 | **`improve-codebase-architecture`** + **`codebase-design`** | `mattpocock` | 按 `git log` 热点扫“浅模块/烂接缝”，直接产出带 Mermaid 前后对比图的 HTML 报告再逐项讨论重构 |
| 9 | **`santa-method`** | `ECC` | 双独立审查 Agent（无共享上下文、同量表）各自打分，全过才放行，否则自动返工收敛 |
| 10 | **`gan-style-harness`** | `ECC` | 复现 Anthropic 的 Generator-Evaluator 对抗闭环，彻底拆分“干活的”和“挑刺的” |
| 11 | **`council`** + **`council-multi-model`** | `ECC` | 重大权衡时拉怀疑派/务实派/批评派圆桌辩论，还支持把结论丢给外部模型（Codex）刻意找漏洞 |
| 12 | **`ai-debt-detector`** | `wshobson` | 专查 AI 写代码特有暗病：只写 happy path、吞异常、孤儿资源未释放、虚构依赖/API |
| 13 | **`parallel-debugging`** | `wshobson` | 疑难 Bug 用竞争假设分析（ACH），按逻辑/数据/状态等 6 类并发起并行排查仲裁 |
| 14 | **`orch-pipeline`**（`orch-*` 家族） | `ECC` | 按改动影响面自动分级，把 Research → Plan → TDD → Review → Commit 串成统一流水线 |

---

## A 级：日常高频主力（每类挑出最强，不用重复看）

- **需求与拆解**：`brainstorming`（先对齐思路再写设计）、`spec-driven-development`（能力地图 + 门禁式 Spec）、`writing-plans`（拆到每步可独立验证）、`search-first`（造轮子前先搜现成库/MCP）
- **测试与排障**：`tdd`（`mattpocock` 版，核心讲垂直切片与 seam 边界测试，反脆弱 mock）、`diagnosing-bugs`（强制先搭最小红绿复现闭环）
- **评审与上线**：`code-review-and-quality`（五维评审标尺）、`receiving-code-review`（禁止讨好式“你说得对”，必须先核实再改或反驳）、`production-audit`（上线前本地证据体检）、`security-review`（安全底线清单）
- **并行与防降智**：`using-git-worktrees`、`dispatching-parallel-agents`、`context-engineering`、`strategic-compact`
- **元技能（造/管技能本身）**：`skill-creator`（Anthropic 官方带 eval 评测）、`writing-skills`（用 TDD 写 Skill）、`continuous-learning-v2`（分项目沉淀原子本能并演化新 Skill）、`skill-stocktake`（审计现有技能库质量与冗余）

---

## 贴合当前游戏多仓技术栈的专项强力技能

- **C++ / 低延迟服务端**：`cpp-coding-standards`、`memory-safety-patterns`、`latency-critical-systems`
- **Go 微服务**：`go-concurrency-patterns`、`golang-patterns`
- **Unity 客户端**：`unity-ecs-patterns`
- **二进制/协议/资产轻量逆向**：`leanrea`
- **通用加分（UI / 工具）**：`frontend-design`（去 AI 模板味最强）、`mcp-builder`

---

## B / C 级（其余 ~560 个，直接省时间）

- **B 级（~250 个，工具书型）**：各语言/框架手册（Java/SpringBoot、Python、Postgres、Redis、K8s、Playwright、PDF/PPTX/DOCX/XLSX 等）及特定工程流程（如 `ralphinho-rfc-pipeline`、`multi-reviewer-patterns`、`agent-harness-construction`、`delivery-gate`）。不用专门读，用到再调。
- **C 级（~300+ 个，重叠或垂直长尾）**：跨仓重复的 TDD / Review / Debug 变体，以及医疗 HIPAA、DeFi/NFT、Perl、小语种本地化、法务/市场调研等，还有强依赖外部 MCP 才生效的（如 `deep-research` 需 Firecrawl/Exa）。

---

## 盘点范围与使用风险提示

- **盘点范围**：本次仅实读了约 35 个头部技能正文与 [required-skills.md](required-skills.md) 全量清单，未逐个通读全部 618 个文件。
- **上下文预算截断风险**：一次性安装 618 个技能容易触发部分 AI Agent 的上下文预算截断（例如 Antigravity 会截出 330+ 个技能，包含 `graphify`、`ponytail`、`verification-before-completion`、`doubt-driven-development`、`santa-method` 等核心项）；平时建议在对话中**显式点名调用**目标技能，而不是指望 Agent 全自动触发。
