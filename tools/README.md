# tools/ —— 工具链

代码生成器、导表器、运维工具和工程脚本。这里的东西都不随服务端部署,只在开发、构建、运维时使用。

## 目录

| 目录 | 语言 | 用途 |
|------|------|------|
| [`proto_generator/`](proto_generator/) | Go | 协议生成器 proto-gen:从 `proto/` 生成 C++、Go、机器人、客户端代码 |
| [`data_table_exporter/`](data_table_exporter/) | Python | 导表器:从 `data/` 生成表数据与各语言的查表代码 |
| [`scripts/`](scripts/) | PowerShell、Shell、Python | 工程脚本:启动、构建、部署、发布、压测 |
| `merge_zone/` | Go | 合服与玩家搬库工具 |
| `data_consistency_check/` | Go | 跨区数据交叉引用巡检 |
| `navmesh_baker/` | C++ | 场景导航网格烘焙(Recast) |
| `battle_art_gen/` | Go | 战斗美术素材的程序化生成 |
| `dev/` | — | [mprocs](https://github.com/pvolok/mprocs) 的进程面板配置 |
| `patches/` | — | 对第三方子模块的本地补丁 |
| `docs/` | — | 生成器命名迁移的审计记录 |
| `archived/` | — | 已退役的脚本,只作参考,不保证能运行 |

`proto/` 下只有一份弃用说明:旧的预编译生成器放在这里,新代码在 `proto_generator/`。

## 常用入口

日常命令统一从 `scripts/dev_tools.ps1` 进,仓库根的 `dev.bat` 是它的菜单封装。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command help              # 全部命令
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run     # 重新生成协议代码
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build      # 编译全部 Go 服务
pwsh -File tools/scripts/dev_tools.ps1 -Command dev-status        # 本地进程状态
```

| 脚本 | 用途 |
|------|------|
| `scripts/start_game.ps1` | 一键启动本地整套服务(根目录 `start-server.cmd` 调它) |
| `scripts/dev_tools.ps1` | 命令总入口:生成、构建、本地进程、Kubernetes、合服 |
| `scripts/go_services.ps1`、`scripts/cpp_nodes.ps1` | 本地 Go 服务与 C++ 节点的启停 |
| `scripts/run_cpp_tests.ps1` | 编译并运行 C++ 测试 |
| `scripts/build_linux.sh` | C++ 节点的 Linux 构建 |
| `scripts/k8s_deploy.ps1`、`scripts/k8s_image.ps1` | Kubernetes 部署与镜像 |
| `scripts/publish_images.ps1`、`scripts/make_release.ps1` | 发布打包 |
| `scripts/stress_snap.ps1`、`scripts/stress_summarize.ps1` | 压测采样与汇总 |
| `scripts/gen_docs_index.py` | 刷新 `docs/README.md` 的文档索引 |

脚本的详细说明见 [scripts/README.md](scripts/README.md);`scripts/tests/` 下是这些脚本的契约测试,
改脚本前先看有没有对应的测试。

## 约定

- 能独立运行的工具各占一个子目录,不在 `tools/` 根下散放脚本。
- 被维护的脚本放 `scripts/`;一次性的或已退役的放 `archived/`。
- 生成过程的临时输出放在已忽略的路径下,不入库。

给 AI 协作者的排查入口见 [AGENTS.md](AGENTS.md)。
