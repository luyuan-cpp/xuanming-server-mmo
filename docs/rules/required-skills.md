# 必装 AI Agent Skills 清单（新电脑 / 新 Agent 必做）

> 对应根索引 [AGENTS.md 会话启动门禁 / §10](../../AGENTS.md#会话启动门禁每次必做)。**每次换新电脑或接入新 AI Agent（Claude Code / Codex / Gemini / Cursor / Copilot 等）前，必须读本文件并把下方已登记的全部 Skills 安装到位。**

选型标准与本仓技术选型一致：**负责人指定核心 Skill + GitHub ≥ 2 万 Star 的高口碑代码工程 Skill**。任何外部 Skill 与本仓 `AGENTS.md` 冲突时，一律以本仓 `AGENTS.md` 为准。

> **权限提醒**：遵循 [AGENTS.md §9](naming-and-boundaries.md#2-禁止事项与权限边界9)，新机器初始化或发现缺失必装 Skill 时，由开发者确认/授权后按本文件脚本一键安装。

## 1. 本机实装的必装仓库总表（8 个仓库，共 618 个 Skills + CLI）

> Star 数与实装数量核对日期：2026-11-21。新电脑按第 2 节脚本跑完即可 1:1 复原全部 618 个技能（按厉害程度精选梯队见 [ai-skills-ranking.md](ai-skills-ranking.md)）。

| 序号 | 仓库 / 工具 | GitHub 地址 | Stars（2026-11） | 实装数量 | 核心用途 |
|---:|---|---|---:|---:|---|
| 1 | **mattpocock/skills**（指定必装） | https://github.com/mattpocock/skills | ~32.9 万 | 39 | 实战型软件工程工作流 Skill 集（规划、拆解、代码评审、TDD 等） |
| 2 | **DietrichGebert/ponytail**（指定必装） | https://github.com/DietrichGebert/ponytail | ~19.6 万 | 6 | 懒惰资深工程师模式：最小改动完整解决问题、反过度工程与 bloat、代码审计与评审 |
| 3 | **obra/superpowers** | https://github.com/obra/superpowers | ~32.3 万 | 14 | 完整软件工程 Skill 框架（头脑风暴、分步计划、系统化调试、严格 TDD、代码评审） |
| 4 | **affaan-m/ECC** | https://github.com/affaan-m/ECC | ~30.8 万 | 327 | 多 Agent 工程效能体系（编码规范、安全审查、记忆与研究优先开发流程） |
| 5 | **anthropics/skills** | https://github.com/anthropics/skills | ~19.8 万 | 20 | Anthropic 官方公共 Agent Skills 仓库（标准 SKILL.md 参考与核心能力集） |
| 6 | **addyosmani/agent-skills** | https://github.com/addyosmani/agent-skills | ~11.9 万 | 26 | 生产级代码工程 Skills（五维代码评审、需求拷问、TDD、架构与性能排查） |
| 7 | **wshobson/agents** | https://github.com/wshobson/agents | ~4.3 万 | 185 | 跨工具多语言工程 Plugins/Agents/Skills 合集（原生适配 Claude / Codex / Cursor / Antigravity / Copilot） |
| 8 | **Graphify-Labs/graphify**（`graphifyy`） | https://github.com/Graphify-Labs/graphify | ~15.1 万 | 1（CLI + Skill） | 本地确定性 AST 解析，将代码库/文档/Schema 转为可查询知识图谱（支持 Claude / Codex / Cursor / Gemini / Antigravity / Copilot） |

### 附：其他按需可选（≥ 2 万 Star，本机未全量安装）
- **Egonex-AI/Understand-Anything**（https://github.com/Egonex-AI/Understand-Anything ，~9.3 万 star）：代码库交互式知识图谱。
- **JuliusBrussee/caveman**（https://github.com/JuliusBrussee/caveman ，~12.2 万 star）：压缩 Agent 冗余输出与工具上下文消耗。
- **garrytan/gstack**（https://github.com/garrytan/gstack ，~14.2 万 star）：多角色虚拟工程团队工作流（需 Bun 环境）。
- **sickn33/agentic-awesome-skills**（~5.0 万 star）/ **VoltAgent/awesome-agent-skills**（~3.7 万 star）/ **github/awesome-copilot**（~4.1 万 star）：大型索引目录，按需单技能挑选，不整仓盲装。

## 2. 新电脑 / 新 Agent 一键复原安装脚本（Windows PowerShell 实测通过）

前置依赖：已安装 `git`、`node`（含 `npx`）、`python`（含 `pip` 或 `uv`）。Windows PowerShell 若禁用 `.ps1` 执行策略，用 `cmd /c` 调用 `npx`（注意 `cmd` 里不要给 `*` 加单引号）。

```powershell
# ============================================================
# 1. 直接在线安装中小型核心仓库（同步 Claude Code / Codex / Gemini CLI / Antigravity / Cursor / Copilot）
# ============================================================
cmd /c "npx --yes skills add mattpocock/skills -g -a claude-code codex gemini-cli antigravity cursor github-copilot -s * -y --copy"
cmd /c "npx --yes skills add DietrichGebert/ponytail -g -a claude-code codex gemini-cli antigravity cursor github-copilot -s * -y --copy"
cmd /c "npx --yes skills add obra/superpowers -g -a claude-code codex gemini-cli antigravity cursor github-copilot -s * -y --copy"
cmd /c "npx --yes skills add addyosmani/agent-skills -g -a claude-code codex gemini-cli antigravity cursor github-copilot -s * -y --copy"

# ============================================================
# 2. 大型仓库先浅克隆再本地安装，避免默认 300s 克隆超时
# ============================================================
foreach ($r in @("anthropics/skills", "affaan-m/ECC", "wshobson/agents")) {
    $dir = Join-Path $env:TEMP ("skill-" + ($r -replace "/", "-"))
    if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
    git clone --depth 1 --filter=blob:none "https://github.com/$r.git" $dir
    cmd /c "npx --yes skills add `"$dir`" -g -a claude-code codex gemini-cli antigravity cursor github-copilot -s * -y --copy"
    Remove-Item -Recurse -Force $dir
}

# ============================================================
# 3. 安装 Graphify CLI 及各 Agent Skill
# ============================================================
pip install --user graphifyy   # 或: uv tool install graphifyy
# Windows --user 安装目录通常在 %APPDATA%\Python\Python3xx\Scripts\graphify.exe，建议加入 PATH
graphify install --platform claude
graphify install --platform codex
graphify install --platform antigravity-windows
graphify install --platform copilot
graphify install --platform agents
# 注：--platform gemini / cursor 会在当前目录生成 GEMINI.md / .cursor/rules/graphify.mdc，请按需在具体仓库根目录内执行
```

### 各 Agent 原生插件补充（可选，用于启用 Claude Code / Antigravity 原生插件 Hook 与 Rules）
- **Claude Code**（在会话内执行）：
  ```text
  /plugin marketplace add DietrichGebert/ponytail
  /plugin install ponytail@ponytail
  /plugin install mattpocock-skills@claude-plugins-official
  /plugin marketplace add obra/superpowers-marketplace
  /plugin install superpowers@superpowers-marketplace
  /plugin marketplace add affaan-m/ECC
  /plugin install ecc@ecc
  ```
- **Antigravity**：`ponytail` 规则插件放在 `~/.gemini/config/plugins/ponytail/`（含 `plugin.json`、`rules/AGENTS.md`、`skills/`），其余技能已由上方脚本同步到 `~/.gemini/config/skills/`。

## 3. 新电脑安装验收检查

执行以下命令核对每个 Agent 目录均已同步到位（预期 `~/.agents/skills`、`~/.claude/skills`、`~/.gemini/config/skills` 各为 618 个）：

```powershell
@(
  "$env:USERPROFILE\.agents\skills",
  "$env:USERPROFILE\.claude\skills",
  "$env:USERPROFILE\.gemini\config\skills",
  "$env:USERPROFILE\.codex\skills",
  "$env:USERPROFILE\.copilot\skills"
) | ForEach-Object {
  $cnt = if (Test-Path $_) { (Get-ChildItem $_ -Directory).Count } else { 0 }
  Write-Output ("{0,4}  {1}" -f $cnt, $_)
}
```

## 4. 已安装全部 Skills 完整明细（按来源仓库登记，共 618 个）

> 注：`~/.agents/skills/` 中极少数跨仓同名技能按后安装顺序覆盖并归属到最后写入的仓库，总计唯一技能目录 618 个（含 `graphify`）。

### 4.1 `mattpocock/skills`（39 个）
- 仓库：https://github.com/mattpocock/skills
- 技能列表：`ask-matt`、`chief-of-staff`、`claude-handoff`、`code-review`、`codebase-design`、`diagnosing-bugs`、`domain-modeling`、`git-guardrails-claude-code`、`grill-me`、`grill-with-docs`、`grilling`、`handoff`、`implement`、`implement-spec`、`improve-codebase-architecture`、`loop-me`、`migrate-to-shoehorn`、`pr`、`prototype`、`research`、`retro`、`scaffold-exercises`、`setup-matt-pocock-skills`、`setup-pre-commit`、`setup-ts-deep-modules`、`software-factory`、`tdd`、`teach`、`to-questionnaire`、`to-spec`、`to-tickets`、`triage`、`wait-what`、`wayfinder`、`wizard`、`writing-beats`、`writing-for-agents`、`writing-fragments`、`writing-shape`

### 4.2 `DietrichGebert/ponytail`（6 个）
- 仓库：https://github.com/DietrichGebert/ponytail
- 技能列表：`ponytail`、`ponytail-audit`、`ponytail-debt`、`ponytail-gain`、`ponytail-help`、`ponytail-review`

### 4.3 `obra/superpowers`（14 个）
- 仓库：https://github.com/obra/superpowers
- 技能列表：`brainstorming`、`diagnosing-superpowers`、`dispatching-parallel-agents`、`executing-plans`、`finishing-a-development-branch`、`receiving-code-review`、`requesting-code-review`、`subagent-driven-development`、`systematic-debugging`、`using-git-worktrees`、`using-superpowers`、`verification-before-completion`、`writing-plans`、`writing-skills`

### 4.4 `affaan-m/ECC`（327 个）
- 仓库：https://github.com/affaan-m/ECC
- 技能列表：`accessibility`、`agent-architecture-audit`、`agent-eval`、`agent-harness-construction`、`agent-introspection-debugging`、`agent-payment-x402`、`agent-runtime-gateway-smoke-test`、`agent-security-hardening`、`agent-self-evaluation`、`agent-sort`、`agentic-engineering`、`agentic-os`、`ai-first-engineering`、`ai-regression-testing`、`android-clean-architecture`、`angular-developer`、`api-connector-builder`、`api-design`、`article-writing`、`automation-audit-ops`、`autonomous-agent-harness`、`autonomous-loops`、`backend-patterns`、`benchmark`、`benchmark-methodology`、`benchmark-optimization-loop`、`bengali-nlp`、`blender-motion-state-inspection`、`blueprint`、`brand-discovery`、`brand-voice`、`brownfield-retrospective`、`browser-qa`、`bun-runtime`、`canary-watch`、`carrier-relationship-management`、`change-impact`、`cisco-ios-patterns`、`ck`、`click-path-audit`、`clickhouse-io`、`code-tour`、`codebase-onboarding`、`codehealth-mcp`、`coding-standards`、`competitive-platform-analysis`、`competitive-report-structure`、`compose-multiplatform-patterns`、`config-gc`、`config-health`、`configure-ecc`、`connections-optimizer`、`content-engine`、`content-hash-cache-pattern`、`context-and-decisions`、`context-budget`、`continuous-agent-loop`、`continuous-learning`、`continuous-learning-v2`、`contract-first`、`conventional-commit-review`、`cost-aware-llm-pipeline`、`cost-tracking`、`council`、`council-multi-model`、`counterparty-channel-discipline`、`cpp-coding-standards`、`cpp-testing`、`crosspost`、`csharp-testing`、`customer-billing-ops`、`customs-trade-compliance`、`dart-flutter-patterns`、`dashboard-builder`、`data-scraper-agent`、`data-throughput-accelerator`、`database-migrations`、`deep-research`、`deepseek-harness-setup`、`defi-amm-security`、`delivery-gate`、`deployment-patterns`、`design-system`、`dev-team`、`devfleet`、`django-celery`、`django-patterns`、`django-security`、`django-tdd`、`django-verification`、`dmux-workflows`、`docker-patterns`、`docs-governance`、`documentation-lookup`、`dotnet-patterns`、`dynamic-workflow-mode`、`e2e-testing`、`ecc-contribution-review`、`ecc-conventions`、`ecc-guide`、`ecc-recipes`、`ecc-tools-cost-audit`、`email-ops`、`energy-procurement`、`enterprise-agent-ops`、`error-handling`、`esign-field-placement`、`eval-harness`、`evm-token-decimals`、`exa-search`、`fal-ai-media`、`fastapi-patterns`、`finance-billing-ops`、`flox-environments`、`flutter-dart-code-review`、`foundation-models-on-device`、`frontend-a11y`、`frontend-design-direction`、`frontend-patterns`、`frontend-slides`、`fsharp-testing`、`gan-style-harness`、`gateguard`、`generating-python-installer`、`git-workflow`、`github-ops`、`godot-patterns`、`godot-testing`、`golang-patterns`、`golang-testing`、`google-workspace-ops`、`growth-log`、`healthcare-cdss-patterns`、`healthcare-emr-patterns`、`healthcare-eval-harness`、`healthcare-phi-compliance`、`hermes-imports`、`hexagonal-architecture`、`hipaa-compliance`、`homelab-network-readiness`、`homelab-network-setup`、`homelab-pihole-dns`、`homelab-vlan-segmentation`、`homelab-wireguard-vpn`、`hookify-rules`、`i18n-sync`、`inherit-legacy-style`、`intent-driven-development`、`inventory-demand-planning`、`investor-materials`、`investor-outreach`、`ios-icon-gen`、`issue-review`、`iterative-retrieval`、`ito-baskets`、`ito-compute`、`ito-inference`、`ito-training`、`java-coding-standards`、`jira-integration`、`jpa-patterns`、`knowledge-ops`、`kotlin-coroutines-flows`、`kotlin-exposed-patterns`、`kotlin-ktor-patterns`、`kotlin-patterns`、`kotlin-testing`、`kubernetes-patterns`、`laravel-patterns`、`laravel-plugin-discovery`、`laravel-security`、`laravel-tdd`、`laravel-verification`、`latency-critical-systems`、`lead-intelligence`、`leanrea`、`liquid-glass-design`、`living-docs-governance`、`llm-trading-agent-security`、`logistics-exception-management`、`loop-design-check`、`mailtrap-email-integration`、`make-interfaces-feel-better`、`manim-video`、`market-research`、`marketing-campaign`、`master-agreement-generator`、`mcp-dependency-review`、`mcp-server-patterns`、`messages-ops`、`ml-adoption-playbook`、`mle-workflow`、`module-governance`、`module-regression`、`motion-advanced`、`motion-foundations`、`motion-patterns`、`mybatis-patterns`、`mysql-patterns`、`nanoclaw-repl`、`nasiko-control-plane`、`nestjs-patterns`、`netmiko-ssh-automation`、`network-bgp-diagnostics`、`network-config-validation`、`network-interface-health`、`nextjs-turbopack`、`nodejs-keccak256`、`nutrient-document-processing`、`nuxt4-patterns`、`openclaw-persona-forge`、`opensource-pipeline`、`operator-approval-loop`、`orch-add-feature`、`orch-build-mvp`、`orch-change-feature`、`orch-fix-defect`、`orch-pipeline`、`orch-refine-code`、`osint-investigation`、`parallel-execution-optimizer`、`perl-patterns`、`perl-security`、`perl-testing`、`plan-canvas`、`plan-orchestrate`、`plankton-code-quality`、`platform-factory`、`postgres-patterns`、`prediction-market-oracle-research`、`prediction-market-risk-review`、`prisma-patterns`、`product-capability`、`product-lens`、`production-audit`、`production-scheduling`、`project-flow-ops`、`prompt-caching-strategy`、`prompt-optimizer`、`python-patterns`、`python-testing`、`pytorch-patterns`、`quality-nonconformance`、`quarkus-patterns`、`quarkus-security`、`quarkus-tdd`、`quarkus-verification`、`rails-patterns`、`rails-security`、`ralphinho-rfc-pipeline`、`react-native-patterns`、`react-patterns`、`react-performance`、`react-testing`、`recursive-decision-ledger`、`redis-patterns`、`regex-vs-llm-structured-text`、`remotion-video-creation`、`repo-scan`、`research-ops`、`returns-reverse-logistics`、`roundhouse-x402`、`rules-distill`、`rust-patterns`、`rust-testing`、`safety-guard`、`santa-method`、`scientific-db-openalex-database`、`scientific-db-pubmed-database`、`scientific-db-uspto-database`、`scientific-pkg-gget`、`scientific-thinking-literature-review`、`scientific-thinking-scholar-evaluation`、`search-first`、`security-bounty-hunter`、`security-review`、`security-scan`、`seo`、`short-video-distribution`、`skill-comply`、`skill-host-compat`、`skill-scout`、`skill-stocktake`、`social-graph-ranker`、`social-publisher`、`springboot-patterns`、`springboot-security`、`springboot-tdd`、`springboot-verification`、`story-lifecycle`、`strategic-compact`、`swift-actor-persistence`、`swift-concurrency-6-2`、`swift-protocol-di-testing`、`swiftui-patterns`、`taste`、`taste-application`、`taste-distillation`、`tasteforge-video`、`tdd-workflow`、`team-agent-orchestration`、`team-builder`、`terminal-opener`、`terminal-ops`、`terraform-patterns`、`test-collaboration`、`tinystruct-patterns`、`token-budget-advisor`、`token-card`、`typescript-patterns`、`ui-canvas`、`ui-demo`、`ui-to-vue`、`uncloud`、`unified-memory`、`unified-notifications-ops`、`verification-loop`、`video-editing`、`videodb`、`visa-doc-translate`、`vite-patterns`、`vue-patterns`、`web-perimeter-owasp-2025-audit`、`windows-desktop-e2e`、`workspace-surface-audit`、`x-api`、`yoruba-localization-testing`

### 4.5 `anthropics/skills`（20 个）
- 仓库：https://github.com/anthropics/skills
- 技能列表：`academy-guide`、`algorithmic-art`、`brand-guidelines`、`canvas-design`、`claude-api`、`discernment-nudge`、`doc-coauthoring`、`docx`、`frontend-design`、`internal-comms`、`mcp-builder`、`pdf`、`pptx`、`skill-creator`、`slack-gif-creator`、`template-skill`、`theme-factory`、`web-artifacts-builder`、`webapp-testing`、`xlsx`

### 4.6 `addyosmani/agent-skills`（26 个）
- 仓库：https://github.com/addyosmani/agent-skills
- 技能列表：`accessibility-engineering`、`api-and-interface-design`、`browser-testing-with-devtools`、`ci-cd-and-automation`、`code-review-and-quality`、`code-simplification`、`constraint-driven-development`、`context-engineering`、`debugging-and-error-recovery`、`deprecation-and-migration`、`documentation-and-adrs`、`doubt-driven-development`、`frontend-ui-engineering`、`git-workflow-and-versioning`、`idea-refine`、`incremental-implementation`、`interview-me`、`observability-and-instrumentation`、`performance-optimization`、`planning-and-task-breakdown`、`security-and-hardening`、`shipping-and-launch`、`source-driven-development`、`spec-driven-development`、`test-driven-development`、`using-agent-skills`

### 4.7 `wshobson/agents`（185 个）
- 仓库：https://github.com/wshobson/agents
- 技能列表：`accessibility-compliance`、`ai-debt-detector`、`airflow-dag-patterns`、`angular-migration`、`anti-reversing-techniques`、`api-design-principles`、`architecture-decision-records`、`architecture-patterns`、`async-python-patterns`、`attack-tree-construction`、`auth-implementation-patterns`、`avoid-ai-writing`、`backtesting-frameworks`、`bash-defensive-patterns`、`bats-testing-patterns`、`bazel-build-optimization`、`before-you-build`、`billing-automation`、`binary-analysis-patterns`、`block-no-verify-hook`、`brand-landingpage`、`changelog-automation`、`checkpoint-promotion`、`code-review-excellence`、`competitive-landscape`、`connectivity-triage`、`context-driven-development`、`cost-optimization`、`cqrs-implementation`、`data-quality-frameworks`、`data-storytelling`、`database-migration`、`dataset-curation`、`dbt-transformation-patterns`、`debugging-strategies`、`defi-protocol-templates`、`dependency-upgrade`、`deployment-pipeline-design`、`design-system-patterns`、`distributed-tracing`、`dotnet-backend-patterns`、`e2e-testing-patterns`、`embedding-strategies`、`employment-contract-templates`、`error-handling-patterns`、`eval-harness-first`、`evaluation-methodology`、`event-store-design`、`fastapi-templates`、`file-conversion`、`finetuning-method-selection`、`gdpr-data-handling`、`git-advanced-workflows`、`github-actions-templates`、`gitlab-ci-patterns`、`gitops-workflow`、`go-concurrency-patterns`、`godot-gdscript-patterns`、`grafana-dashboards`、`grounded-vault`、`grpo-rlvr-training`、`hads`、`helm-chart-scaffolding`、`hermes-tweet`、`hybrid-cloud-networking`、`hybrid-search-implementation`、`incident-runbook-templates`、`interaction-design`、`istio-traffic-management`、`javascript-testing-patterns`、`k8s-manifest-generator`、`k8s-security-policies`、`kpi-dashboard-design`、`langchain-architecture`、`linkerd-patterns`、`llm-evaluation`、`lora-qlora-recipes`、`market-sizing-analysis`、`memory-forensics`、`memory-safety-patterns`、`microservices-patterns`、`ml-pipeline-workflow`、`mobile-android-design`、`mobile-ios-design`、`modern-javascript-patterns`、`monorepo-management`、`mtls-configuration`、`multi-cloud-architecture`、`multi-reviewer-patterns`、`nextjs-app-router-patterns`、`nft-standards`、`nodejs-backend-patterns`、`nx-workspace-patterns`、`on-call-handoff-patterns`、`openapi-spec-generation`、`parallel-debugging`、`parallel-feature-development`、`paypal-integration`、`pci-compliance`、`postgresql-table-design`、`postmortem-writing`、`pptx-deck-context`、`pptx-quality-gates`、`pptx-reference-deck-analysis`、`pptx-slide-specification`、`pptx-visual-assets`、`preference-optimization`、`projection-patterns`、`prometheus-configuration`、`prompt-engineering-patterns`、`protect-mcp-setup`、`protocol-reverse-engineering`、`python-anti-patterns`、`python-background-jobs`、`python-code-style`、`python-configuration`、`python-design-patterns`、`python-error-handling`、`python-observability`、`python-packaging`、`python-performance-optimization`、`python-project-structure`、`python-resilience`、`python-resource-management`、`python-testing-patterns`、`python-type-safety`、`quantized-export`、`rag-implementation`、`react-modernization`、`react-native-architecture`、`react-native-design`、`react-state-management`、`recsys-pipeline-architect`、`responsive-design`、`review-agent-setup`、`risk-metrics-calculation`、`rust-async-patterns`、`saga-orchestration`、`sast-configuration`、`scan`、`screen-reader-testing`、`secrets-management`、`security-requirement-extraction`、`service-mesh-observability`、`session-guard`、`shellcheck-configuration`、`signed-audit-trails-recipe`、`similarity-search-patterns`、`slo-implementation`、`social-evidence-research`、`social-publishing`、`solidity-security`、`spark-environment-setup`、`spark-memory-thermal-ops`、`spark-optimization`、`spark-training-gotchas`、`sql-optimization-patterns`、`startup-financial-modeling`、`startup-metrics-framework`、`stride-analysis-patterns`、`stripe-integration`、`superself`、`tailwind-design-system`、`task-coordination-strategies`、`team-communication-protocols`、`team-composition-analysis`、`team-composition-patterns`、`temporal-python-testing`、`terraform-module-library`、`threat-mitigation-mapping`、`trace-to-training-data`、`track-management`、`turborepo-caching`、`typescript-advanced-types`、`unity-ecs-patterns`、`uv-package-manager`、`vector-index-tuning`、`vision-sft`、`visual-design-foundations`、`visual-edit-precision`、`wcag-audit-patterns`、`web-component-design`、`web3-testing`、`workflow-orchestration-patterns`、`workflow-patterns`

### 4.8 `Graphify-Labs/graphify`（1 个）
- 仓库：https://github.com/Graphify-Labs/graphify
- 技能列表：`graphify`（通过 `graphifyy` CLI 分发到 `~/.agents/skills/graphify`、`~/.claude/skills/graphify`、`~/.codex/skills/graphify`、`~/.gemini/config/skills/graphify`、`~/.gemini/skills/graphify`、`~/.copilot/skills/graphify`）
