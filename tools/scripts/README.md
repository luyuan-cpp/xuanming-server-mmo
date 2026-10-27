# Development Scripts

PowerShell and shell scripts for common development tasks.

## Directory Rules

- 仓库根保留 `dev.bat` 开发菜单，以及转调本目录的 `start-server.cmd` / `start-game.cmd` 薄启动器；维护中的独立工程脚本放在本目录，不把菜单本身误称为无逻辑的薄包装。
- [tools/scripts](.) is the canonical home for maintained engineering scripts.
- One-off or retired scripts go to [tools/archived](../archived) instead of staying here.
- [tools/scripts/third_party](third_party) stores third-party build and maintenance scripts, 包括独立 Protobuf、BoringSSL、GoogleTest 与 Redis 构建。
- If a top-level convenience command is needed, keep it as a thin wrapper that forwards into [tools/scripts](.) instead of copying logic.
- Do not place project-maintained scripts under vendored directories such as [third_party](../../third_party); treat those trees as upstream-owned whenever possible.

## Available Scripts

### prepare_docker_debug.py（独立 Docker 断点调试环境）

在仓库根执行 `python tools/scripts/prepare_docker_debug.py`，从当前 10 份源配置派生容器配置、初始化 SQL 与 topic 契约，输出到 Git 忽略的 `build/docker-debug-current/`。需要本机 Python、PyYAML 和运行中的 Docker；脚本只读检查现有 Docker 子网，不构建、不启动容器、不删除数据。首次生成随机密钥，后续保留原值，不把密钥写进受管文件或输出。

预先用当前源码构建镜像后，运行 `docker compose --env-file build/docker-debug-current/.env -f deploy/debug/compose.game.yml -f deploy/debug/compose.debug.yml up -d`。镜像构建沿用 `deploy/k8s/Dockerfile.cpp`、`Dockerfile.go-svc`（上下文 `go/`、命名上下文 `tables=generated/tables`）及 `Dockerfile.java-svc`；C++ 调试层见 `deploy/debug/Dockerfile`。Compose 禁止自动拉取，初始化任务成功后才启动 15 个常驻容器。

默认客户端入口为 `http://127.0.0.1:8081`，同一网关另保留 `http://127.0.0.1:18081`；gate 为 `127.0.0.1:28000`，battle 为 `127.0.0.1:28010`。可用 `--gateway-port` 调整附加 HTTP 端口、用 `--gate-port` / `--battle-port` 调整对应入口；`8081` 保留给客户端默认网关。VS Code 附加到固定名称的三个调试容器。此栈只启用登录、建角、主世界进场与战斗所需服务，未启动域的请求按现有路由契约失败。校验配置时使用 `docker compose ... config --quiet`，不要把展开后的配置与 `.env` 打印或上传。

### start_game.ps1（本机游戏一键启动）

电脑重启后双击仓库根目录 `start-server.cmd`；双击 `start-game.cmd` 可同时打开客户端。
脚本复用现有 `dev_tools.ps1` 和编译产物，依次启动本机 Docker、数据库依赖、已配置并具备产物的 Go 服务（含 client_rpc_router；可选服务按前置检查决定）、Java 网关、一区 gate / scene / battle，并等待就绪。关闭启动窗口后服务继续运行；再次双击会复用已有实例。

- 网关：`http://127.0.0.1:8081`，一区；默认客户端：同级工作目录 `tmp/showcase_player/mmorpg.exe`。
- 指定客户端：`pwsh -File tools/scripts/start_game.ps1 -OpenClient -ClientPath "E:/其他目录/mmorpg.exe"`。
- 只检查运行文件：`pwsh -File tools/scripts/start_game.ps1 -CheckOnly`。
- 前置：PowerShell 7、Java 23+、本机 Docker Desktop、已缓存镜像与游戏编译产物；日常启动不执行构建或镜像下载。
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
- `third-party-lock-check`（默认只读校验，`-ApplyPatches` 恢复登记补丁）
- `third-party-zlib-build`（隔离构建 Debug/MDd zlib 并执行上游测试）
- `third-party-protobuf-build`
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
编译后自动验证合法成员、裸指针拒绝、解析失败拒绝和第三方源码排除边界。
同时验证真实构建钩子的含空格路径、成功缓存和失败时清除旧缓存。

第三方源码按完整目录排除：`third_party/` 与 `cpp/libs/engine/muduo_windows/`，
包括被业务源码包含的库头文件。Windows 路径兼容大小写与两种分隔符；
`third_party_adapter` 等近似目录仍检查。排除依据是成员声明所在源码，
项目类中指向 `muduo::EventLoop`、`protobuf::Service` 等类型的裸指针仍会报告。
独立工具与备用 clang-query 使用相同边界；检查规则更新会让成功缓存失效。
工程检查还会保留 CMake `/external:I` 外部头目录，并拒绝 clang-query 的解析错误，
即使它同时输出 `0 matches`、退出码为 0。

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

### 第三方版本固定与补丁恢复

版本表及补丁清单在 [third_party/releases.lock.json](../../third_party/releases.lock.json)。
当前固定 20 个顶层子模块：17 个正式 release，3 个没有正式 release 的依赖保留当前提交。
父仓的 gitlink 才是真正的版本锁；`.gitmodules` 的分支配置不能代替提交锁。
不要使用 `git submodule update --remote` 或逐库 `git pull`，否则会越过固定提交。

首次检出或按父仓更新依赖后，先运行同步脚本，再恢复已登记的兼容补丁：

```powershell
pwsh -File tools/archived/sync_submodules.ps1
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-lock-check -ApplyPatches

# 日常只读检查，不修改子模块或补丁。
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-lock-check
```

Linux 同步入口为 `bash tools/archived/sync_submodules.sh`，之后执行
`python3 tools/scripts/third_party/verify_release_lock.py --apply-patches`。
校验器检查清单路径、实际提交与已登记补丁；包装入口保留 Python 的原生退出码。
默认校验发现缺少补丁也会失败，不会静默应用。升级依赖须同时复核并更新 gitlink、固定清单和兼容补丁。

Redis 8.10.2 的登记补丁保留 hiredis DNS 测试的错误码与文本检查，Windows 预期文本改由
`gai_strerror(EAI_NONAME)` 生成，适配系统语言；其他平台保留原有断言。
sol2 v3.3.0 的引用 `optional::emplace` 补丁回补上游已有的引用重绑定修复，使用
`std::addressof` 支持重载取地址运算符，并拒绝临时对象，避免 const 引用悬垂。
librdkafka v2.16.0 的补丁同时保留 Windows 工程调整与重入组变参修正：先完成格式化，
再以 `"%s"` 传递原因，避免读取未初始化的 `va_list` 或将它误传给普通变参函数。

Linux 的 curl 与 librdkafka 统一使用独立固定提交的 BoringSSL 头文件及静态归档；C++ 不依赖官方 gRPC SDK。
不要将这些头文件与系统 OpenSSL 库混用。librdkafka 的 BoringSSL 补丁适配密码学随机数、
HMAC 声明及 PKCS#12 证书、私钥和证书链加载；构建将缺失函数声明视为错误。
独立 `third_party/openssl` 固定源码版本，本构建流程不安装或使用它。

版本宏与运行字符串须按上游实际记录，版本来源以清单中的官方 tag 和精确 SHA 为准。
curl 的 `curl-8_23_0` 源码仍标为 `8.23.0-DEV`（数字 `0x081700`）；官方发布脚本在生成
tarball 时才准备发布版本头。直接从该 tag 构建时保留真实字符串，校验数字版本与源码宏一致，
不改头文件伪装输出。sol2 的 `v3.3.0` 源码版本宏仍为 `3.2.3`，同样不能据此改写锁定源码或冒称另一版本。

### third_party/refresh_boost_headers.py（复制聚合头）

Boost 1.92 的头文件分布在直属模块的 `include/boost/`，单独初始化顶层 gitlink 不会生成
工程引用的 `third_party/boost/boost/`。先按 Boost 父提交初始化全部内部子模块，再运行普通复制：

```powershell
git -C third_party/boost submodule update --init --recursive
$boostArchive = "build/verification/boost-copy-$(Get-Date -Format yyyyMMdd-HHmmss)"
python tools/scripts/third_party/refresh_boost_headers.py --archive $boostArchive
```

Linux 对应使用 `python3`，`--archive` 同样必须指定本轮全新目录。
归档目录必须位于本仓库内、Boost 源码树外且与目标在同一文件系统；不能复用已有目录。
工具按 `.gitmodules` 与父 gitlink 校验模块 HEAD，只复制直属 `include/boost/` 中的普通文件，
不扫描测试或示例，也不创建符号链接、目录联接或硬链接。
完整文件集合、SHA256 与 `BOOST_VERSION=109200` 核查通过后，旧聚合树才移入归档的
`previous-boost/`，再发布新树；失败保留现场，不删除旧树。
归档同时保存 `modules.json`、`files.sha256.json` 和成功或失败回执。
干净 CI 必须在子模块初始化后、Docker 的 Boost 聚合头 `COPY` 前完成这一步。
标准 `tools/scripts/build_linux.sh` 在未指定 `--skip-deps` 时，会在初始化全部依赖后自动执行
固定清单校验、登记补丁恢复和本轮 Boost 聚合头复制；`--dry-run` 只显示计划，不创建归档目录。
Docker builder 保持 `--skip-deps`，复用 CI 已准备的源码和 deps 阶段产物，不读取 `.git`。

### third_party/build_zlib.ps1

只使用已安装的 Visual Studio/MSVC、CMake 与 Ninja，不安装工具。
固定源码构建 Debug/MDd，串行编译，全部上游 ctest 通过后才安装。
`-BuildRoot` 必须是仓库内的全新目录；默认 `build/third_party/zlib-v1.3.2-dbg`，
产物分别位于其 `build/` 与 `install/`，安装目录含 `lib/zlibd.lib`、`lib/zlibstaticd.lib` 与 `bin/zlibd.dll`。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-zlib-build -BuildRoot build/third_party/zlib-v1.3.2-dbg -Jobs 1
```

### 独立 C++ 依赖构建

Protobuf 35.1、Abseil、GoogleTest 1.17.0、BoringSSL 都是独立子模块，提交固定于
`third_party/releases.lock.json`。Protobuf 自带 utf8_range；不再构建官方 gRPC、插件或官方桩。
构建入口只使用已有 MSVC 14.51、CMake 和 VS / Ninja，串行执行，不安装或升级工具。
MSVC 缺少英文诊断包时自动选择已有 VS 生成器，避免 Ninja 的本地化依赖前缀转码失真；
VS / Ninja 使用不同构建缓存目录，安装前缀保持相同。

```powershell
# zlib 安装目录需与目标配置、架构一致；下面复用已有的 Debug 安装。
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-protobuf-build -Configuration Debug -ZlibInstallRoot build/third_party/zlib-v1.3.2-dbg/install
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-boringssl-build -Configuration Debug
pwsh -File tools/scripts/third_party/build_gtest.ps1 -Configuration Debug
```

默认 `x64 Debug` 使用 `/MDd`，安装前缀分别是各子模块内的 `install_vs2026_dbg`；
`-Configuration Release` 使用 `/MD` 与 `install_vs2026_rel`；`-Architecture x86` 使用
`install_vs2026_x86_dbg/rel`。不能跨配置或架构混用库。Protobuf 安装包含同批 Abseil 和 utf8 库。
`-BuildRoot` 可将构建及安装放进仓内 ignored 路径；它不会自动发布到默认前缀。

Linux 由 `tools/archived/setup_dependencies.sh` 先准备 zlib，再调用
`third_party/build_linux_sdk.sh` 构建独立序列化与 TLS 依赖；Docker 使用主仓子模块源码，
不再 clone 官方 gRPC 仓库。SDK Debug 来源清单升级为 schema 2，字段 `dependencySource`
必须完整包含独立 Protobuf、Abseil、BoringSSL 的提交、gitlink 和文件摘要；旧 schema 1 不接受。

#### Commands

```powershell
# Show command help
pwsh -File dev_tools.ps1 -Command help

# Build proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-build

# Run proto-gen with default config
pwsh -File dev_tools.ps1 -Command proto-gen-run

# 构建独立 Protobuf
pwsh -File dev_tools.ps1 -Command third-party-protobuf-build

# Run include cleaner (auto tool selection)
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene

# Force clang-tidy mode and auto-fix include issues
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene -IwyuTool clang-tidy -FixIncludes

# Fast mode: only check changed files in current diff range
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene -ChangedOnly

# Batch scan multiple nodes
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly

# 在 VS Code 中构建独立 Protobuf
# Run Task -> third_party: protobuf build

# Run proto-gen with custom config
pwsh -File dev_tools.ps1 -Command proto-gen-run -ConfigPath "<path-to-config>"

# Generate project tree report
pwsh -File dev_tools.ps1 -Command tree

# Audit recursive naming issues (default snake_case)
pwsh -File dev_tools.ps1 -Command naming-audit

# Apply recursive naming normalization
pwsh -File dev_tools.ps1 -Command naming-apply

# Use kebab-case style with a capped batch size
pwsh -File dev_tools.ps1 -Command naming-audit -Style kebab -MaxChanges 200

# Open one k8s zone
pwsh -File dev_tools.ps1 -Command k8s-zone-up -ZoneName yesterday -ZoneId 101 -OpsProfile managed-cloud -NodeImage "<image>" -WaitReady

# Open all zones from JSON/YAML config
pwsh -File dev_tools.ps1 -Command k8s-all-up -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml -OpsProfile managed-cloud -NodeImage "<image>" -WaitReady

# Check/close k8s zones
pwsh -File dev_tools.ps1 -Command k8s-zone-status -ZoneName yesterday
pwsh -File dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday
pwsh -File dev_tools.ps1 -Command k8s-all-status -ZonesConfigPath deploy/k8s/zones.yaml
pwsh -File dev_tools.ps1 -Command k8s-all-down -ZonesConfigPath deploy/k8s/zones.yaml

# 独立 Release 构建需匹配的 Release zlib
pwsh -File dev_tools.ps1 -Command third-party-protobuf-build -Configuration Release -Clean -Jobs 1 -ZlibInstallRoot "<Release zlib prefix>"

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
- 按 infra / zone 职责部署基础设施、Go 服务、Java 网关和 C++ gate/scene；battle 为全局池，Centre 已退役。Go/Java 部署需显式提供各自镜像仓库参数。
- 集群外入口显式使用 `-ClientEntryMode external`，为 gate 创建 StatefulSet 与每序号 Service；默认 podip 只面向集群内测试，不能用生产暴露 profile 代替入口模式。生产还须显式设置 `-ReleaseProfile prod`。
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

### 本机 Docker 修改后重编与断点

在仓库根执行 `python tools/scripts/rebuild_docker_debug.py cpp`、`python tools/scripts/rebuild_docker_debug.py go --services login` 或 `python tools/scripts/rebuild_docker_debug.py java`，分别更新 C++ 三节点、Go 登录服或 Java 网关。入口先构建独占镜像，核对当前源码与产物，再只重建对应服务；现有数据卷、账号和随机开发密码保留。`--build-only` 只构建，不更新配置或容器。

`build_docker_cpp_debug.py` 使用本机已验证 SDK 和 `Debug/-O0`，SDK 来源及依赖守卫位于忽略目录 `build/docker-debug-current/cpp-toolchain.json`；依赖源码变化时停止，不复用不匹配 SDK。`build_docker_go_debug.py` 使用当前 Go 1.27.2、`all=-N -l` 与固定 Delve v1.27.2，逐服务串行构建。`build_docker_java_debug.py` 使用已有 JDK 23/Maven 离线单线程执行测试、打包并核验行号表。三个底层构建器只输出本地镜像及 `--output/result.json`，不自行更新运行栈。

`docker_cpp_sdk_guard.py` 收集 SDK 及独立依赖的实际文件集合；`docker_debug_manifest.py` 在容器构建中核验冻结源码、维护按 SHA 变化失效的对象缓存，并登记最终编译参数、节点与调试符号摘要。

VS Code 的“终端 → 运行任务”提供对应的“Docker：重编并更新”任务。“运行和调试”提供 C++ 三节点、Go 七服务及 Java Gateway 的附加配置，每项附加前执行源码检查。也可执行 `python tools/scripts/rebuild_docker_debug.py check --services scene` 单独检查。Go 普通服务不编译的 `*_test.go` 不参与运行源码一致性判定。完整操作与端口见 `docs/design/docker_k8s_build_deploy.md`。

- 维护中的 PowerShell 入口使用 PowerShell 7（`pwsh`）；不要假设 Windows PowerShell 5.1 兼容。
- Use `-Verbose` flag for detailed output
- Scripts are designed to be cross-platform compatible where possible
- For large repositories, prefer iterative rename batches with `-MaxChanges` to keep refactors reviewable and reduce breakage risk
