# Development Scripts

PowerShell and shell scripts for common development tasks.

## Directory Rules

- [scripts](../../scripts) is for thin entrypoints and bootstrap tasks only, such as environment setup, submodule sync, or simple one-shot launch commands.
- [tools/scripts](.) is the canonical home for maintained engineering scripts.
- [tools/scripts/third_party](third_party) stores third-party build and maintenance scripts, such as gRPC or future protobuf/redis builds.
- If a top-level convenience command is needed, keep it as a thin wrapper that forwards into [tools/scripts](.) instead of copying logic.
- Do not place project-maintained scripts under vendored directories such as [third_party](../../third_party); treat those trees as upstream-owned whenever possible.

## Available Scripts

### start_game.ps1（本机游戏一键启动）

电脑重启后双击仓库根目录 `启动服务器.cmd`；双击 `启动服务器并打开游戏.cmd` 可同时打开客户端。
脚本复用现有 `dev_tools.ps1` 和编译产物，依次启动本机 Docker、数据库依赖、六个 Go 服务、Java 网关、一区 gate / scene / battle，并等待就绪。关闭启动窗口后服务继续运行；再次双击会复用已有实例。

- 网关：`http://127.0.0.1:8081`，一区；默认客户端：同级工作目录 `tmp/showcase_player/mmorpg.exe`。
- 指定客户端：`pwsh -File tools/scripts/start_game.ps1 -OpenClient -ClientPath "E:/其他目录/mmorpg.exe"`。
- 只检查运行文件：`pwsh -File tools/scripts/start_game.ps1 -CheckOnly`。
- 前置：PowerShell 7、Java 21+、本机 Docker Desktop、已缓存镜像与游戏编译产物；日常启动不执行构建或镜像下载。
- 日志：`run/logs/game-launcher/<时间>/`；服务日志仍位于 `run/logs/go_services` 和 `run/logs/cpp_nodes`。
- 旧 PID 会核对程序完整路径，失效记录先备份。Docker 停止时，已知遗留通信目录与独立 socket 会重命名备份；不删除数据卷、账号或角色。
- 失败时显示原因与日志位置；已经启动的服务保留，解决原因后可重新双击。

### dev_tools.ps1

Main entry point for tool commands on Windows.

Supported commands include:

- `help`
- `proto-gen-build` (alias: `pbgen-build`)
- `proto-gen-run` (alias: `pbgen-run`)
- `tree`
- `naming-audit`
- `naming-apply`
- `third-party-grpc-build`
- `no-raw-pointer-setup`
- `iwyu-run`
- `k8s-*`

Proto generator naming references:

- Current naming-state snapshot: `../docs/proto_gen_naming_audit.md`
- Compatibility boundary and migration rules: `../docs/proto_gen_naming_migration.md`

### no-raw-pointer-setup（裸指针成员检查器）

在仓库根目录执行：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command no-raw-pointer-setup
```

自动下载固定版本 LLVM/Clang 23.1.1 Windows x64 **开发包**，校验固定 SHA256，
解压到 `build/deps/llvm-23.1.1`，然后用本机 Visual Studio 和 CMake 串行构建
`cpp/plugin/build/Release/no_raw_ptr_check.exe`。现有 MSBuild 钩子会自动发现该文件。
开发包约 860 MiB，首次下载和解压需要时间与数 GB 磁盘空间；重跑复用开发库并增量编译。
下载中断保留 `.part` 文件，下次续传。校验失败会明确报错，不运行未校验的开发包。
下载及构建产物均在已忽略的构建目录，不安装系统软件、不修改全局 PATH。
LLVM 官方静态库还需要 zlib、zstd、libxml2；入口同样按固定版本和 SHA256 准备它们，
编译到 `build/deps/llvm-23.1.1-support`，已有完整产物则复用。DIA SDK 自动取自本机 Visual Studio。
编译后自动验证合法成员通过、裸指针成员拒绝、解析失败拒绝三个行为。
同时验证真实构建钩子的含空格路径、成功缓存和失败时清除旧缓存。

- 预览路径与下载地址：增加 `-DryRun`。
- 只准备开发库：增加 `-DownloadOnly`。
- 已有完整开发库：增加 `-LlvmRoot 'E:/llvm-sdk'`，跳过下载。
  未指定时也会检查 `LLVM_ROOT` 环境变量、`Program Files/LLVM` 和原工程的 `D:/game/llvm`。
- 前置：PowerShell 7、Windows x64、Visual Studio C++ 工具、CMake、系统 `curl.exe` 与 `tar.exe`。
  CMake 不在 PATH 时自动查找 Visual Studio 内置版本。
- 普通 LLVM 安装器不能替代此开发包。官方包说明见
  [LLVM 23.1.1 Release](https://github.com/llvm/llvm-project/releases/tag/llvmorg-23.1.1)。
- 构建检查器成功不等于项目静态检查通过；项目扫描结果以之后的 MSBuild 检查输出为准。

### iwyu_run.ps1 / iwyu_run.sh

Cross-platform include hygiene entrypoint.

- Generates `compile_commands.json` under `<node>/build_iwyu`
- Uses `include-what-you-use` when available
- Falls back to `clang-tidy` with `misc-include-cleaner`
- Supports dry-run and fix mode
- Supports fast mode with changed files only (`-ChangedOnly`)
- Supports batch node scan (`-NodePath` accepts multiple values)
- In `-ChangedOnly` mode, if no C/C++ files changed under a node, that node is skipped without full-scan fallback

Windows:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene
pwsh -File tools/scripts/dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/gate -IwyuTool clang-tidy -FixIncludes
pwsh -File tools/scripts/dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly
```

Linux/macOS:

```bash
tools/scripts/iwyu_run.sh -NodePath cpp/nodes/scene -Tool auto
tools/scripts/iwyu_run.sh -NodePath cpp/nodes/gate -Tool clang-tidy -Fix
tools/scripts/iwyu_run.sh -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly
```

Dependencies:

- `pwsh`
- `cmake`
- `clang-tidy` (required)
- `include-what-you-use` (optional but preferred when available)

### third_party/build_grpc.ps1

Canonical Windows PowerShell entrypoint for building vendored gRPC from [third_party/grpc](../../third_party/grpc).

```powershell
pwsh -File tools/scripts/third_party/build_grpc.ps1
```

Features:

- Auto-detects Visual Studio / MSVC with `vswhere`
- Auto-installs or upgrades `cmake` and `ninja` through `winget` when needed
- Builds both Release and Debug into `third_party/grpc/install_vs2026` and `third_party/grpc/install_vs2026_dbg`

Legacy batch launcher is also available at [tools/scripts/third_party/build_grpc_vs2026_v145.bat](third_party/build_grpc_vs2026_v145.bat).

Legacy thin wrapper moved to: [tools/archived/build_grpc.ps1](../archived/build_grpc.ps1)

VS Code tasks are available in [/.vscode/tasks.json](../../.vscode/tasks.json):

- `third_party: grpc build`
- `third_party: grpc build release`
- `third_party: grpc build debug`

#### Commands

```powershell
# Show command help
pwsh -File dev_tools.ps1 -Command help

# Build proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-build

# Run proto-gen with default config
pwsh -File dev_tools.ps1 -Command proto-gen-run

# Build vendored gRPC via the canonical third-party script
pwsh -File dev_tools.ps1 -Command third-party-grpc-build

# Run include cleaner (auto tool selection)
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene

# Force clang-tidy mode and auto-fix include issues
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene -IwyuTool clang-tidy -FixIncludes

# Fast mode: only check changed files in current diff range
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene -ChangedOnly

# Batch scan multiple nodes
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly

# Build vendored gRPC from VS Code Tasks
# Run Task -> third_party: grpc build

# Run proto-gen with custom config
pwsh -File dev_tools.ps1 -Command proto-gen-run -ConfigPath <path-to-config>

# Generate project tree report
pwsh -File dev_tools.ps1 -Command tree

# Audit recursive naming issues (default snake_case)
pwsh -File dev_tools.ps1 -Command naming-audit

# Apply recursive naming normalization
pwsh -File dev_tools.ps1 -Command naming-apply

# Use kebab-case style with a capped batch size
pwsh -File dev_tools.ps1 -Command naming-audit -Style kebab -MaxChanges 200

# Open one k8s zone
pwsh -File dev_tools.ps1 -Command k8s-zone-up -ZoneName yesterday -ZoneId 101 -OpsProfile managed-cloud -NodeImage <image> -WaitReady

# Open all zones from JSON/YAML config
pwsh -File dev_tools.ps1 -Command k8s-all-up -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml -OpsProfile managed-cloud -NodeImage <image> -WaitReady

# Check/close k8s zones
pwsh -File dev_tools.ps1 -Command k8s-zone-status -ZoneName yesterday
pwsh -File dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday
pwsh -File dev_tools.ps1 -Command k8s-all-status -ZonesConfigPath deploy/k8s/zones.yaml
pwsh -File dev_tools.ps1 -Command k8s-all-down -ZonesConfigPath deploy/k8s/zones.yaml

# gRPC release-only rebuild with explicit flags
pwsh -File dev_tools.ps1 -Command third-party-grpc-build -BuildDebug:$false -Clean -Jobs 8

# Preflight/build/push/release runtime image for k8s
pwsh -File dev_tools.ps1 -Command k8s-stage-runtime -BinarySourceRoot D:/linux-build/bin -ZoneInfoSource bin/zoneinfo -TableSource generated/tables
pwsh -File dev_tools.ps1 -Command k8s-image-preflight
pwsh -File dev_tools.ps1 -Command k8s-build-image -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
pwsh -File dev_tools.ps1 -Command k8s-push-image -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
pwsh -File dev_tools.ps1 -Command k8s-release-zone -ZoneName yesterday -ZoneId 101 -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1 -WaitReady
```

### normalize_names.ps1

Recursively normalizes file and directory names to `snake_case` or `kebab-case`.

- Excludes `third_party` by default
- Also skips tooling/build artifact roots by default: `.git`, `.vs`, `.idea`, `.vscode`, `_copilot_session_transfer`, `x64`, `bin`, `generated`, `cpp/generated`, `go/generated`, `cpp/bin`
- Skips embedded muduo vendor trees under `cpp/libs/*/muduo_windows`
- By default only scans source roots: `cpp`, `go`, `java`, `proto`, `docs`, `tools`, `scripts`, `data`, `deploy`, `robot`, `test`, `etc`
- Skips dotfiles and dot-directories to avoid renaming repository metadata files
- Skips high-risk generated/IDE files: `*.vcxproj*`, `*.sln*`, `*.tlog`, `*.recipe`, `*.pb.{h,cc,go}`
- Supports conflict detection (duplicate targets / existing target path)
- Uses deep-first rename order to avoid parent path conflicts

Recommended workflow:

```powershell
# 1) Dry run first
pwsh -File tools/scripts/dev_tools.ps1 -Command naming-audit

# 2) Apply in small batches for safer rollout
pwsh -File tools/scripts/dev_tools.ps1 -Command naming-apply -MaxChanges 100

# 3) Build and run tests after each batch

# Optional: run against all non-excluded paths (advanced)
pwsh -File tools/scripts/normalize_names.ps1 -Mode audit -IncludeRelativePaths @()
```

### tree.ps1

Generates a tree structure report of the project directory.

```powershell
pwsh -File tree.ps1
```

### k8s_deploy.ps1

Kubernetes-only deployment entrypoint used by `dev_tools.ps1`.

- Supports single-zone and multi-zone one-click open-server flows.
- Applies infra (`etcd`, `redis`, `kafka`) and game node workloads (`centre`, `gate`, `scene`) per namespace.
- Exposes `gate` through a per-zone Kubernetes Service and can wait for deployment readiness.
- Reads multi-zone definitions from `deploy/k8s/zones.json` or `deploy/k8s/zones.yaml` (or custom path).
- Do not assume `LoadBalancer` across all environments: managed cloud K8s usually prefers `LoadBalancer`, while self-hosted / bare metal K8s should usually prefer `NodePort` plus an external L4 load balancer.
- Prefer passing `-OpsProfile managed-cloud` or `-OpsProfile bare-metal` so the exposure choice is explicit in commands and change records.
- In `custom` mode, the default `-GateServiceType` baseline is `NodePort` (safer for clusters without mature LB support).

Quick exposure preflight (dry-run + assertions):

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-exposure-preflight
```

See `deploy/k8s/README.md` for full usage and behavior details.
Ops runbook: `docs/ops/k8s-open-server-runbook.md`.

### k8s_image.ps1

Kubernetes runtime image and release entrypoint used by `dev_tools.ps1`.

- Verifies staged Linux runtime files before image build.
- Builds and pushes the dedicated K8s runtime image from `deploy/k8s/Dockerfile.runtime`.
- Supports one-command release flows: build + push + deploy.

### k8s_stage_runtime.ps1

Stages Linux node binaries, `zoneinfo`, and generated tables into `deploy/k8s/runtime/linux` for the dedicated K8s runtime image.

## Usage Examples

### From Project Root

```powershell
# Generate all proto code
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run

# Generate project tree
pwsh -File tools/scripts/dev_tools.ps1 -Command tree
```

### From tools/scripts Directory

```powershell
# Build proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-build

# Run proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-run -ConfigPath ../proto_generator/protogen/etc/proto_gen.yaml
```

## Notes

- All scripts are Windows PowerShell 5.1+ compatible
- Use `-Verbose` flag for detailed output
- Scripts are designed to be cross-platform compatible where possible
- For large repositories, prefer iterative rename batches with `-MaxChanges` to keep refactors reviewable and reduce breakage risk
