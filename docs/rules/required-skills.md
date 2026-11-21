# 必装 AI Agent Skills 清单（新电脑 / 新 Agent 必做）

> 对应根索引 [AGENTS.md 会话启动门禁 / §10](../../AGENTS.md#会话启动门禁每次必做)。**每次配置新电脑或接入新 AI Agent（Claude Code / Codex / Gemini / Cursor / Copilot 等）前，必须先确认下方必装 Skills 已安装。**

选型标准与本仓技术选型一致：**明确指定的核心工程 Skill + GitHub ≥ 2 万 Star 的高口碑代码工程 Skill**（聚焦代码质量、精简设计、架构理解与工程工作流，不盲装与后端/游戏开发无关的营销/求职/纯视觉类 Skill）。

> **安全与权限提醒**：遵循 [AGENTS.md §9](naming-and-boundaries.md#2-禁止事项与权限边界9)，AI Agent 不擅自安装工具或修改系统环境；在新机器初始化或发现缺失必装 Skill 时，由开发者授权后按本清单执行安装。

## 1. 必装核心清单（负责人指定 + ≥ 2 万 Star 精选）

> Star 数为 2026-11-21 通过 GitHub API 实测值。

| 序号 | 仓库 | GitHub 地址 | Stars（2026-11） | 核心用途 |
|---:|---|---|---:|---|
| 1 | **mattpocock/skills**（指定必装） | https://github.com/mattpocock/skills | ~32.9 万 | 实战型软件工程工作流 Skill 集（规划、拆解、代码评审、TDD 等） |
| 2 | **DietrichGebert/ponytail**（指定必装） | https://github.com/DietrichGebert/ponytail | ~19.6 万 | 懒惰资深工程师模式：最小改动完整解决问题、反过度工程与 bloat、代码审计与评审 |
| 3 | **obra/superpowers** | https://github.com/obra/superpowers | ~32.3 万 | 完整软件工程 Skill 框架（头脑风暴、分步计划、系统化调试、严格 TDD、代码评审） |
| 4 | **affaan-m/ECC**（Everything Claude Code） | https://github.com/affaan-m/ECC | ~30.8 万 | 多 Agent 工程效能体系（编码规范、安全审查、记忆与研究优先开发流程） |
| 5 | **anthropics/skills** | https://github.com/anthropics/skills | ~19.8 万 | Anthropic 官方公共 Agent Skills 仓库（标准 `SKILL.md` 参考与核心能力集） |
| 6 | **Graphify-Labs/graphify** | https://github.com/Graphify-Labs/graphify | ~15.1 万 | 本地确定性 AST 解析，将代码库/文档/Schema 转为可查询知识图谱（支持 Claude / Codex / Cursor / Gemini CLI） |
| 7 | **addyosmani/agent-skills** | https://github.com/addyosmani/agent-skills | ~11.9 万 | 生产级代码工程 Skills（五维代码评审、需求拷问、TDD、架构与性能排查） |
| 8 | **wshobson/agents** | https://github.com/wshobson/agents | ~4.3 万 | 跨工具多语言工程 Plugins/Agents/Skills 合集（原生适配 Claude Code / Codex / Cursor / Antigravity / Copilot） |

### 按需可选（同样 ≥ 2 万 Star，按需增装）
- **Egonex-AI/Understand-Anything**（https://github.com/Egonex-AI/Understand-Anything ，~9.3 万 star）：代码库交互式知识图谱，适合新人/新会话快速上手复杂模块。
- **JuliusBrussee/caveman**（https://github.com/JuliusBrussee/caveman ，~12.2 万 star）：压缩 Agent 冗余输出与工具上下文消耗。
- **garrytan/gstack**（https://github.com/garrytan/gstack ，~14.2 万 star）：多角色虚拟工程团队工作流（需 Bun 环境）。
- **sickn33/agentic-awesome-skills**（https://github.com/sickn33/agentic-awesome-skills ，~5.0 万 star） / **VoltAgent/awesome-agent-skills**（https://github.com/VoltAgent/awesome-agent-skills ，~3.7 万 star） / **github/awesome-copilot**（https://github.com/github/awesome-copilot ，~4.1 万 star）：大型 Skill 检索目录，按单技能挑选安装，**不要整仓全量盲装**以免撑爆上下文。

## 2. 跨 Agent 安装方式（新电脑初始化指南）

### 方式 A：通过开放 `skills` CLI 一键同步到多 Agent（推荐）
适用于同时使用 Claude Code、Codex、Gemini / Antigravity、Cursor、Copilot 的开发机（需 Node.js；Windows PowerShell 若禁用 `.ps1` 执行策略，用 `cmd /c` 包裹）：

```powershell
# 1. 安装 / 同步标准 Agent Skills（支持 -g 全局安装与指定目标 Agent）
cmd /c "npx --yes skills add mattpocock/skills -g -y"
cmd /c "npx --yes skills add DietrichGebert/ponytail -g -y"
cmd /c "npx --yes skills add obra/superpowers -g -y"
cmd /c "npx --yes skills add anthropics/skills -g -y"
cmd /c "npx --yes skills add addyosmani/agent-skills -g -y"
cmd /c "npx --yes skills add affaan-m/ECC -g -y"
cmd /c "npx --yes skills add wshobson/agents -g -y"

# 2. 代码库知识图谱 CLI（需 uv 或 pipx）
uv tool install graphifyy
graphify install                     # Claude Code
graphify install --platform codex    # Codex
graphify cursor install              # Cursor
```

> **注意**：`wshobson/agents`、`affaan-m/ECC`、`anthropics/skills` 仓库内 Skill 数量较多，建议按需挑选核心工程类 Skill 或通过插件机制按需加载（渐进式披露），避免一次性把上百个无关 Skill 全量灌入同一全局目录。

### 方式 B：各 Agent 原生插件命令

- **Claude Code**：
  ```text
  /plugin install mattpocock-skills@claude-plugins-official
  /plugin marketplace add DietrichGebert/ponytail
  /plugin install ponytail@ponytail
  /plugin marketplace add obra/superpowers-marketplace
  /plugin install superpowers@superpowers-marketplace
  /plugin marketplace add anthropics/skills
  /plugin marketplace add affaan-m/ECC
  /plugin install ecc@ecc
  ```
- **Codex**：
  ```powershell
  codex plugin marketplace add mattpocock/skills
  codex plugin add mattpocock-skills@mattpocock
  ```
  其余标准 `SKILL.md` 仓库可直接通过 `npx skills add <owner>/<repo> -g -a codex` 安装到 `~/.codex/skills/`。
- **Gemini / Antigravity**：
  - 全局插件放在 `~/.gemini/config/plugins/<name>/`（含 `plugin.json`、`rules/`、`skills/`），全局单技能放在 `~/.gemini/config/skills/<name>/SKILL.md`；
  - 可用 `npx skills add <owner>/<repo> -g` 同步到 `~/.agents/skills/` / `~/.gemini/`，或按 Antigravity `plugin` 机制打包接入。

## 3. 新电脑 / 新 Agent 验收检查

初始化完成后核对以下路径：
- Claude Code：`~/.claude/plugins/installed_plugins.json` 或 `~/.claude/skills/`
- Codex：`~/.codex/skills/`
- Gemini / Antigravity：`~/.gemini/config/plugins/` 与 `~/.gemini/config/skills/`
- 公共 Agent 目录：`~/.agents/skills/`
- 确认 `ponytail`、`mattpocock/skills`、`superpowers`、`addyosmani/agent-skills` 等核心 Skill 均已就位，且未引入与仓库 `AGENTS.md` 冲突的破坏性规则（任何外部 Skill 与本仓 `AGENTS.md` 冲突时，一律以本仓 `AGENTS.md` 为准）。
