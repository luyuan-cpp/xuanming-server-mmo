# TOOLS KNOWLEDGE BASE

## OVERVIEW
`tools/` 放代码生成器、导表器、运维工具和工程脚本。机器人客户端在仓库根的 `robot/`,不在这里。

## STRUCTURE
目录结构与常用入口见同目录 [README.md](README.md),这里不重复维护。

## WHERE TO LOOK
| Task | Location | Notes |
|------|----------|-------|
| Main script entrypoint | `scripts/dev_tools.ps1` | proto-gen (pbgen), k8s, tree, naming audit/apply |
| proto-gen source | `proto_generator/protogen/` | Canonical generator project |
| Compatibility proto-gen bundle | `proto/` | Retained for existing local toolchains |
| Robot data-stress mode | 仓库根 `robot/data_stress.go` | L3 e2e: login→enter→play→logout cycles, publishes expected seq to Redis for verifier |
| **配置表 schema（事实源）** | `data/schema/*.proto` | 类型/owner/键/索引/外键/位序/注释;词表在 `cfg_options.proto` |
| **配置表全表索引** | `data/AGENTS.md` | 哪张表有主键/索引/外键/位序,由 `gen_schema_index.py` 生成 |
| 表怎么被读进来 | `data_table_exporter/core/table_source.py` | 按 sheet 名派发:有权威 schema 走 schema-first |
| 改导表器后的验收 | `data_table_exporter/tools/sandbox_export.py` | 沙盒导一次 + 与仓内产物逐字节比对,对仓库只读 |

## CONVENTIONS
- Keep runnable source projects in dedicated subdirectories; do not scatter standalone scripts across `tools/` root.
- Put reports/dumps/snapshots under `docs/`.
- Keep temp generation output under ignored paths like `generated/` and local binaries.
- `tools/scripts/dev_tools.ps1` is the preferred shell entrypoint for routine commands.
- `tools/proto_generator/protogen` is the canonical proto-gen project; `tools/proto/protogen` exists for compatibility.
- 配置表的 schema 事实源是 `data/schema/*.proto`,**不是 xlsx 表头**(5 行表头格式 2026-09-03 退休)。
  改表流程与禁忌见 `data/AGENTS.md`;导表器自身的说明见 `data_table_exporter/readme.md`。
- Robot client logic assumes one client binds to exactly one goroutine.

## ANTI-PATTERNS
- Committing IDE metadata such as `.idea/`.
- Editing temporary generated outputs as if they were source.
- Renaming broad source trees directly when `naming-audit` / `naming-apply` already exist.
- Replacing `dev_tools.ps1` flows with ad hoc one-off command docs.

## COMMANDS
```bash
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.yaml
pwsh -File tools/scripts/dev_tools.ps1 -Command tree
pwsh -File tools/scripts/dev_tools.ps1 -Command naming-audit
pwsh -File tools/scripts/dev_tools.ps1 -Command naming-apply -MaxChanges 100
```

## NOTES
- `tools/scripts/README.md` is the practical command catalog; keep new tool workflows wired through it.
- High-risk rename exclusions already include generated files, IDE files, `*.vcxproj*`, `*.sln*`, and `*.pb.{h,cc,go}`.
