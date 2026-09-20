# 服务端发布打包标准(对标 luyuan-go/xuanming-server)

**Created:** 2026-09-14
**状态:** P1–P4 已落码(2026-09-16/17),**未编译、未运行、未按 §6 验证**——按 AGENTS §10.1 由 Codex 执行。
§5 的四个待拍板项用户未逐项回答,**按推荐项默认执行**(GitHub Actions / 四批全做 / 不做 SBOM 签名扫描 / 不引入 GoReleaser·MinIO·Harbor·Argo CD)。
实施状态、评审结论与遗留缺口见 §7。
**关联:** [docs/ops/release-checklist.md](../ops/release-checklist.md)、`tools/scripts/lib/release_common.ps1`、`tools/scripts/release_preflight.ps1`、[xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) D-14(迁移 Job)

> 术语:**A 仓** = GitHub `luyuan-go/xuanming-server`(Pandora,Go/Kratos,go.work 多模块,UE 客户端在 SVN);
> **B 仓** = 本仓库。A 仓证据路径写作 `A:<路径>:<行>`。

---

## TL;DR

1. A 仓的"大厂发布标准"本质不是某个工具,而是**四层分离 + 三条铁律**:
   版本库只放源码 → CI 构建 → 版本库外的**不可变**制品目录 → 按 **release manifest** 发布与回滚。
2. B 仓已经有"不可变 tag + 配置预检 + 部署生成器契约测试",但**没有制品目录、没有离线包、没有 release manifest、没有 CHANGELOG**,
   而且**版本号实际上没有进二进制**(Go 的 `-X main.buildVersion` 目标变量不存在;gate 的 `BUILD_GIT_SHA` 无人注入;Java 是 `0.0.1-SNAPSHOT`)。
3. 不照抄 A 的工具选型:A 选 Jenkins 是因为 UE 引擎 + SVN;B 服务端代码在 GitHub、已有 7 条 Actions,A 自己的文档也写明这种情况 GitHub Actions 是默认选择(`A:tools/devops/README.md:250-254`)。
4. A 自身也有缺口(Windows exe 不注入版本、预检没接流水线、基础镜像不钉 digest、无 SBOM/签名),落地时**取 A 最严的部分并补上这些缺口**。
5. 分四批落地(§4),每批 < 30 文件,可独立审阅、回滚。

---

## 1. A 仓用了什么

| 环节 | 工具 / 规范 | 版本 / 钉死方式 | 证据 |
|---|---|---|---|
| CI | Jenkins(JCasC 声明式初始化,controller `numExecutors=0`,宿主机 agent) | `jenkins/jenkins:lts` 未钉;插件未钉版本 | `A:tools/devops/jenkins/*` |
| 流水线 | 两条 Jenkinsfile:dev 快照轨(pollSCM 自动)/ release 版本轨(手动传 `VERSION`) | — | `A:Jenkinsfile`、`A:Jenkinsfile.release` |
| 裸二进制 | GoReleaser(配置 schema `version: 2`),24 个 build 靠 `dir:` 适配 go.work;`release:`/`changelog:` 关闭,只出 tar.gz + `checksums.txt` | 工具版本未钉 | `A:.goreleaser.yaml` |
| 镜像构建 | Docker 多阶段;宿主交叉编译(prebuilt,默认)/ 容器内编译 两种模式产出同名镜像 | Go `1.26.5` 三处钉死并构建时断言 | `A:deploy/services/Dockerfile:16-44` |
| 离线交付 | `docker save` 出 `pandora-images.tar` + `sha256sums.txt` + `build-info.json` + `images-manifest.json` | — | `A:tools/scripts/publish_offline_images.ps1` |
| 制品库 | 本地目录 `PANDORA_ARTIFACT_ROOT`(权威)+ MinIO(分发副本,`mc mirror`)+ `registry:2`(Harbor 轻量替代) | — | `A:docs/design/release-pipeline.md` §2、§7 |
| 部署 | kustomize overlay 按 **digest** 改写镜像;Argo CD 只同步 base、手动同步、不 prune | Argo CD `v3.4.5` 钉死 | `A:deploy/k8s/argocd/*` |
| 迁移 | golang-migrate,`//go:embed` 进迁移器二进制;K8s Job `backoffLimit: 0` 成功后才滚动业务 | `v4.18.2` | `A:tools/migrate/*`、`A:deploy/k8s/migrate/job.yaml` |
| 版本与说明 | semver + Keep a Changelog 1.1.0 | — | `A:CHANGELOG.md` |
| 防回流 | git pre-receive 钩子(拒 `*.tar` 与 >50MB 单文件);托管平台用 push ruleset 替代 | — | `A:tools/vcs-hooks/git-pre-receive.sh` |
| 门禁测试 | 纯 pwsh 契约测试,登记进 `ci_backend.ps1` 数组才算门禁 | — | `A:tools/scripts/ci_backend.ps1:188-276` |
| **没用** | cosign / syft / grype / trivy、buildx 多架构、Dockerfile `HEALTHCHECK`(交给 K8s probe)、Dockerfile 内 `LABEL` | — | 全仓 grep 零命中 |

---

## 2. A 仓的标准条目

### 2.1 版本库只放源码
- 镜像 tar、exe、UE 包一律不进版本库;`.gitignore` 忽略 `deploy/offline-images/*.tar`。
- 强制力在服务端:pre-receive 钩子拒 `*.tar` 与 >50MB 单文件(`A:tools/vcs-hooks/git-pre-receive.sh:27-45`)。

### 2.2 两轨 CI
| 轨 | 触发 | 版本号 | 落点 |
|---|---|---|---|
| snapshot | 自动 | `g<12位 sha>`;脏树 `-dirty-yyyyMMdd-HHmmss`(默认拒绝) | `<root>\snapshots\` |
| release | 手动传 `VERSION`(必填) | `vX.Y.Z`,正则 `^[vV]?\d+(\.\d+){1,3}([.-][0-9A-Za-z][0-9A-Za-z.-]*)?$` | `<root>\releases\` |

dev 流水线浅克隆不拉 tag,所以 release 不复用 dev job(`A:docs/design/decision-revisit-backend-ci-source.md:30-31`)。

### 2.3 制品目录三条铁律(脚本强制)
1. **不可变**:版本目录已存在即拒绝覆盖;CI 重跑用 `-SkipIfExists` 静默成功。
2. **原子发布**:先写 `.tmp-<名>-<PID>` staging,整目录 rename 上线;rename 前目标被他人抢先发布则丢弃 staging 并报"发布竞争"(`A:tools/scripts/artifacts_lib.ps1:74-97`)。
3. **可追溯**:每个版本目录带 `build-info.json` + `sha256sums.txt`(每行 `<64位hex>  <相对路径>`,与 `sha256sum -c` 兼容)。

布局:
```
<ARTIFACT_ROOT>\
├── snapshots\images\<g+sha12>\{images.tar, images-manifest.json, build-info.json, sha256sums.txt}
├── snapshots\images\latest.json          ← 唯一可变文件(指针)
├── releases\images\<vX.Y.Z>\...
├── releases\images\latest.json
└── releases\manifests\<vX.Y.Z>.json / .md
```
`build-info.json` 字段:`version / channel / app_version / vcs / source_rev / dirty / image_count / registry / pushed_images / published_at / machine / publisher`。
清理脚本**只清 snapshots**(每流保留最近 N,默认 dry-run),`releases\` 永不触碰(`A:tools/scripts/artifacts_retention.ps1`)。

### 2.4 版本号与镜像 tag
- 线上 tag 形如 `v1.2.3-b5a5a95`:必须含 7~40 位小写 git SHA;禁止 `dev|latest|stable|prod|production|test`(`A:tools/scripts/lib/online_manifest_contract.ps1:662-687`)。
- **tag 只是别名,运行身份是 digest**:发布时 `docker buildx imagetools inspect` 回读 digest,overlay 改写成 `repo@sha256:…`,渲染后逐个断言;digest 经 Pod 注解 + downward API 进容器环境变量。
- 只收单平台 manifest(拒 image index)。
- 版本号四处一致:`git tag` = `make_release -Version` = `CHANGELOG` 段落 = 镜像自报 `app_version`。

### 2.5 版本注入二进制
- `pkg/version` 三个包级 var `Version / Commit / BuildTime`,默认 `dev / unknown`(`A:pkg/version/version.go:23-32`)。
- `-ldflags "-s -w -X <mod>/pkg/version.Version=… .Commit=… .BuildTime=…"`;值来自 `git describe --tags --always --dirty`、`git rev-parse --short HEAD`、UTC RFC3339;`PANDORA_RELEASE_VERSION` 显式覆盖(`A:tools/scripts/start.ps1:7451-7481`)。
- 服务启动首行日志 `service starting version= commit= build_time= go_version=`(`A:pkg/log/log.go:76-85`)。
- 镜像构建命令行加 `--label org.opencontainers.image.revision=<commit>`,发布时回读必须等于当前 commit。

### 2.6 镜像构建
- builder:`golang:${GO_VERSION}-bookworm`,`RUN test "$(go env GOVERSION)" = "go${GO_VERSION}"` 断言编译器版本。
- 层顺序:`go mod download` 单独一层,之后才声明每次变化的 `BUILD_TIME` 等 ARG,避免击穿依赖缓存。
- `CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath` + `-s -w`。
- runtime:`FROM scratch`,只拷 CA 证书与 zoneinfo;`USER 10001:10001`;`ENTRYPOINT` exec 形式,业务进程是 PID 1。
- `GOPROXY` 分隔符用 `|`(任何错误都回退),不用 `,`(只在 404/410 回退)。
- 不用 `# syntax=` / `RUN --mount`(内网构建不去 Docker Hub 拉 frontend)。
- 无 `HEALTHCHECK`,健康检查交给 K8s gRPC readinessProbe。

### 2.7 离线包
- `docker save -o images.tar <镜像清单>`;导出后读 tar 内 `manifest.json` 的 `RepoTags` 与期望集合比对**缺失 / 多余 / 数量**,必须完全一致(`A:tools/scripts/export_images.ps1:372-390`)。
- **坑**:两个镜像层链完全相同时批量 `docker save` 会丢一个;先比 `{{join .RootFS.Layers ","}}`,撞了逐个 save 再合并 manifest(`:344-366`)。
- 过期守卫:镜像 Created 早于源码最新改动即拒绝,无绕过开关。
- 拉取:先 `Test-Sha256Sums`,再拷到 `.fetching` 临时名后 rename。

### 2.8 release manifest(`make_release.ps1`)
- 修复内容优先级 `-Notes` > `-NotesFile` > `CHANGELOG.md` 对应 `## [X.Y.Z]` 段;找不到段落直接 throw。
- 交叉校验镜像 `build-info.app_version == -Version`,为空或不一致一律拒绝。
- 引用 dirty 来源制品默认拒绝(`-AllowDirty` 仅内测)。
- 先写 `.md`,**最后**才落 `.json`(JSON 是"已发布"哨兵),中途失败可直接重跑。

### 2.9 干净树门禁(二进制批次)
构建前后各查一次 `git rev-parse HEAD` 与 `git status --porcelain=v1 --untracked-files=all`;构建期间 HEAD 变化或变脏即拒绝;产物用 `go version -m -json` 回读 `vcs.revision == HEAD`、`vcs.modified=false`(`A:tools/scripts/build_release_binaries.ps1:65-79,108-118,222-232`)。

### 2.10 迁移
已发布迁移文件永不改写/删除/重编号,修正只加新版本;迁移 Job 不自动 force,dirty 或库版本高于镜像内版本即拒;Job 成功后才滚动业务。

### 2.11 A 自身的缺口(**不照抄**)
1. 给策划机出的 24 个 Windows exe 不走 ldflags,启动日志版本显示 `dev`。
2. `release_preflight.ps1` 没接进任何 Jenkinsfile,只能手动跑。
3. 基础镜像只钉 tag 不钉 digest;Dockerfile 内无 OCI `LABEL`(只有命令行 revision 一项)。
4. 无 SBOM、签名、漏洞扫描。
5. 按 digest 部署的 online 链写好了,但被 `start.ps1:7050` 零写阻断,未在真实集群跑通。

---

## 3. B 仓现状与差距

| 维度 | A 的标准 | B 现状 | 差距 | B 证据 |
|---|---|---|---|---|
| 版本注入 | `pkg/version` + ldflags + 启动首行 | Go:`-X main.buildVersion` 目标变量**不存在**(Dockerfile 注释自认);gate:`BUILD_GIT_SHA` 与 `GATE_BUILD_*` 均无人注入,实际输出 `unknown`;Java `0.0.1-SNAPSHOT` 无 build-info | **缺(核心)** | `deploy/k8s/Dockerfile.go-svc:83-92`、`cpp/nodes/gate/gate_version.h:42-73`、`java/gateway_node/pom.xml:16` |
| 服务端本地构建 | `-trimpath` + 版本 | `go_services.ps1` 裸 `go build`,无 trimpath / ldflags | 缺 | `tools/scripts/go_services.ps1:805-850` |
| 镜像 tag | 含 SHA、禁可变 tag、按 digest 部署 | 已有:git sha12(+`-dirty`)、可变 tag 黑名单、prod 拒 dirty;**push 后不记录 digest** | 部分有 | `tools/scripts/lib/release_common.ps1:190-252` |
| 语义版本 / CHANGELOG | semver + Keep a Changelog | 无 | 缺 | — |
| 制品目录 | 两轨、不可变、原子、sha256sums | 无 | 缺 | — |
| 离线包 | `docker save` + RepoTags 核对 + 校验和 | 无(只有 README 里 kind 手敲 `docker save`) | 缺 | `deploy/k8s/README.md:497-522` |
| release manifest | `make_release.ps1` | 无 | 缺 | — |
| 符号文件归档 | 随版本归档 | `build_linux.sh` 产出 `bin/symbols/*.debug`,不归档 | 缺 | `tools/scripts/build_linux.sh:185-217` |
| 镜像加固 | 非 root、revision label | 全部无 `USER`;OCI label 仅 go-svc 3 项 + java 命令行;基础镜像全部只钉 tag | 缺 | `deploy/k8s/Dockerfile.*` |
| 发布 CI | 两轨流水线 | 7 条 workflow 无发布、无制品、无 upload-artifact | 缺 | `.github/workflows/*` |
| 发布预检 | 配置 + 证书(A) | 已有配置/密钥/tag 预检且接进 `k8s_image`/`k8s_deploy`;**不查制品本身** | 部分有(比 A 接线好) | `tools/scripts/release_preflight.ps1:238-428` |
| 发布清单文档 | 分项 + 流程顺序 + 版本追溯 | 只针对 2026-05 HTTP 登录链路,已陈旧 | 过时 | `docs/ops/release-checklist.md:1-17` |
| 契约测试 | 进 CI 清单 | 纯 ps1 自研骨架已进 `deploy-config-tests`;**触发路径不含 `deploy/k8s/Dockerfile*`** | 部分有 | `.github/workflows/deploy-config-tests.yml:32-47` |
| 迁移打包 | 迁移器进发布物,Job 门禁 | trade 有 `-migrate` Job;db 迁移入口不在镜像;data_service 无 Job | 部分(另走 D-14) | `tools/scripts/k8s_deploy.ps1:1559,2141` |
| 防回流 | 钩子 / push ruleset | `.gitignore` 无 `*.tar` 规则;GitHub ruleset 未知 | 缺 | `.gitignore` |

### 3.1 顺带发现的现存问题
1. `deploy/k8s/Dockerfile.robot` `COPY robot/`,但根 `.dockerignore:39` 排除了 `robot/`,**推断**该镜像构建不出来(未实跑)。
2. C++ 镜像两条路径并存且文档矛盾:`deploy/k8s/AGENTS.md:21,27` 说生产用 `Dockerfile.runtime`,`deploy/k8s/README.md:43` 说 `Dockerfile.cpp` 是推荐路径;`README.md:53` 引用的 `build_grpc_linux.sh` 不存在。
3. Go 服务目录三处手工同步(`go_services.ps1:114`、`go_svc_image.ps1:71`、`k8s_deploy.ps1:385`),guild / friend 不在镜像目录。

---

## 4. 落地方案(适配 B,不照抄 A)

### 4.0 适配原则
- **CI 用 GitHub Actions,不引入 Jenkins**:B 服务端无 UE cook、代码在 GitHub、已有 7 条 workflow(AGENTS §11.2 KISS)。所有逻辑写在 pwsh 脚本里,workflow 只做薄调用,本机与 CI 同一入口。
- **复用不重写**:不可变 tag 判定沿用 `release_common.ps1` 的 `Get-GitReleaseStamp` / `Test-ImmutableImageTag`;镜像构建沿用 `go_svc_image.ps1` / `k8s_image.ps1` / `java_svc_image.ps1`,新脚本只负责"收集 → 导出 → 校验 → 发布"。
- **暂不引入** GoReleaser / MinIO / Harbor / Argo CD(YAGNI):B 的交付物是镜像,版本注入在 Dockerfile 与 `go_services.ps1` 两个入口补齐即可;制品根先用本地/共享盘目录,A 的文档也确认"迁到 MinIO 时只改根路径语义,脚本契约不变"(`A:docs/design/release-pipeline.md:169-170`)。
- 制品根:环境变量 `MMORPG_ARTIFACT_ROOT`,默认 `<仓库父目录>\artifacts`(本机即 `E:\work\artifacts`),**不在仓库内**。

### P1 版本可追溯(约 18 文件)
| 改动 | 说明 |
|---|---|
| 新增 `go/shared/buildinfo` | 包级 var `Version/Commit/BuildTime`;ldflags 未注入时回落 `debug.ReadBuildInfo()` 的 `vcs.revision` / `vcs.modified`(补 A 缺口 1);提供一行 `StartupLine()` |
| 各 Go 服务 `main` 启动首行打印 | 当前 12 个 module 中有 main 的服务;不改业务逻辑 |
| `deploy/k8s/Dockerfile.go-svc` | ldflags 目标改为 `shared/buildinfo.*`;加 `GOFLAGS=-trimpath`。⚠️ 该文件另一会话有未提交改动(`COPY schemamigrate/`),动手前需确认已提交 |
| `tools/scripts/go_services.ps1` | 本地构建同样注入 ldflags + `-trimpath` |
| gate | `k8s_deploy.ps1` 给 gate/scene 容器注入 `GATE_BUILD_VERSION` / `GATE_BUILD_COMMIT`(`gate_version.h` 已设计该通道;CMakeLists 会被重生成,不走 `-D`) |
| Java | `spring-boot-maven-plugin` 加 `build-info` goal,actuator `/info` 自报版本 |

### P2 制品目录 + 离线包 + release manifest(约 12 文件)
| 新增 | 对标 A | B 的适配 |
|---|---|---|
| `tools/scripts/lib/artifacts_lib.ps1` | `artifacts_lib.ps1` | 原样语义:root 解析、两轨、sha256sums 生成/校验、原子 staging |
| `tools/scripts/publish_images.ps1` | `publish_offline_images.ps1` + `export_images.ps1` | 调既有三个镜像脚本构建 → `docker save` → RepoTags 核对 + 同层链逐个 save → 写 `images-manifest.json` / `build-info.json` / `sha256sums.txt` → 原子发布;同时归档 C++ `bin/symbols/*.debug` |
| `tools/scripts/fetch_images.ps1` / `import_images.ps1` | 同名 | 校验后落地 / `docker load` |
| `tools/scripts/artifacts_retention.ps1` | 同名 | 只清 snapshots,默认 dry-run |
| `tools/scripts/make_release.ps1` | 同名 | 去掉 UE 包与 configtable 引用,改为引用 `generated/tables` 摘要 |
| `CHANGELOG.md` | Keep a Changelog 1.1.0 | 首段 `## [Unreleased]` |
| `.gitignore` | `*.tar` 不入库 | 加 `*.tar`、`deploy/offline-images/` |
| `tools/scripts/tests/artifacts_publish_contract.tests.ps1` 等 3 个 | `release_artifact_publish_contract_test.ps1` | 沿用 B 的 `test_harness.ps1` 骨架;守:不可变、原子、校验和、脏树拒绝、app_version 交叉校验、CHANGELOG 段缺失拒绝 |

### P3 镜像加固(约 10 文件)
- 所有运行镜像 `USER 10001`(C++ 节点需确认日志/数据目录可写,必须实跑验证)。
- 统一 OCI label:`version / revision / created / source`(补 A 缺口 3)。
- 基础镜像按 digest 钉死(`image:tag@sha256:…`),Dockerfile 头部登记更新方式(补 A 缺口 3)。
- 修 `Dockerfile.robot` 与 `.dockerignore` 冲突;`deploy-config-tests.yml` 触发路径加 `deploy/k8s/Dockerfile*`、`.dockerignore`。
- 修 `deploy/k8s/AGENTS.md` / `README.md` 的 C++ 镜像路径矛盾(文档)。

### P4 发布流水线与门禁(约 8 文件,依赖 §5 拍板)
- `.github/workflows/release.yml`:`workflow_dispatch` 必填 `version` → 构建测试 → `publish_images.ps1 -Version` → `make_release.ps1` → `upload-artifact`;**默认不推 registry**。
- 推送后回读并记录 digest(`release_common.ps1` 新增 `Get-ImageDigest`),写进 manifest。
- `release_preflight.ps1` 增加制品检查:版本目录存在、`sha256sums` 通过、`app_version` 一致、镜像 digest 已记录(补 A 缺口 2:B 的预检已接线,只需扩检查项)。
- 重写 `docs/ops/release-checklist.md`:补版本追溯、制品、迁移、回滚章节,删过时的 05 月专项。
- 可选:SBOM(syft)/ 签名(cosign)/ 漏洞扫描(trivy)(补 A 缺口 4,需装工具)。

### 不在本方案内(另立任务)
- db / data_service 迁移入口进镜像与 Job 化 → 随 D-14 首个建表服务同批。
- 三处 Go 服务目录合并为单一事实源 → 触及 `k8s_deploy.ps1` 主干,单独评审。
- GitHub push ruleset(拒 `*.tar` / >50MB)→ 需仓库管理员在 GitHub 设置里配,AI 不登录远端(AGENTS §9)。

---

## 5. 待拍板

| # | 问题 | 推荐 |
|---|---|---|
| 1 | CI 平台:沿用 GitHub Actions,还是照 A 上 Jenkins | GitHub Actions(§4.0) |
| 2 | 本轮做到哪一批(全部约 48 文件,超过 AGENTS §10.2 的 30 文件线) | 先 P1 + P2 |
| 3 | 是否做 SBOM / 签名 / 漏洞扫描(需装 syft / cosign / trivy) | 暂缓,P4 再议 |
| 4 | 是否引入 GoReleaser / MinIO / Harbor / Argo CD | 暂不引入 |

---

## 6. 验证(交 Codex,按 AGENTS §10.1)

每批落码后在交付说明里给出完整命令;通用口径:
- **P1**:受影响 Go module 逐个 `go build ./... && go test ./...`;构建一个 go-svc 镜像后 `docker run --rm <img> 2>&1 | head -1` 首行必须含 `version=` 且 commit 不为 `unknown`;本地 `go_services.ps1` 构建的 exe 用 `go version -m <exe>` 看到 `-trimpath` 与 ldflags。
- **P2**:`pwsh -File tools/scripts/tests/<新测试>.tests.ps1` 全绿;在临时 `MMORPG_ARTIFACT_ROOT` 下连跑两次 `publish_images.ps1`,第二次必须被不可变规则拒绝(带 `-SkipIfExists` 则 exit 0);篡改 tar 1 字节后 `fetch_images.ps1` 必须报"哈希不符"。
- **P3**:全部镜像 `docker build` 成功;`docker inspect --format '{{.Config.User}}'` 为 `10001`;C++ 节点容器启动后能写日志。
- **P4**:在 fork 或 `workflow_dispatch` 干跑 release.yml,产物页可下载 manifest 与 sha256sums。

---

## 7. 实施状态(2026-09-17)

### 7.1 已落码

| 批次 | 落点 |
|---|---|
| P1 Go | `go/shared/buildinfo`(+单测);11 个服务 main 启动首行 `service starting …` 与 `-version`;`Dockerfile.go-svc`(ldflags 指向 buildinfo、`-trimpath`、`GOTOOLCHAIN=local`、Go 版本断言、依赖层与版本 ARG 分层、基础镜像钉 digest、非 root 10001、OCI label);`go_svc_image.ps1`(`-Version` / `list-refs` / `-DigestsOut` / revision label / 跨平台路径);`go_services.ps1` 本地构建注入 ldflags + `-trimpath` |
| P1 C++/Java | `Dockerfile.runtime` / `Dockerfile.cpp` 版本 ENV + label + 非 root + digest;`build_linux.sh` 的 `MMORPG_STAMP_BUILD=1` 编译期注入(默认关,避免开发机每次提交全量重编);`build_info.h` 环境变量回落;`k8s_image.ps1` / `java_svc_image.ps1` 按接口 C;`pom.xml` build-info |
| P2 制品 | `lib/artifacts_lib.ps1`(两轨、不可变、原子 staging、sha256sums、latest 指针、配置表摘要);`publish_images.ps1`(逐镜像 `docker save` + 归档自证 + revision/version label 核对 + 构建前后复查版本戳);`make_release.ps1`;`fetch_images.ps1` / `import_images.ps1` / `artifacts_retention.ps1`;`CHANGELOG.md`;`.gitignore` |
| P3 镜像加固 | robot / sandbox-mock 镜像;`.dockerignore` 修掉排除 COPY 源(robot/、runtime/linux)的问题;`deploy/k8s/AGENTS.md` 与 `README.md` 的 C++ 镜像路径矛盾 |
| P4 门禁 | `release_common.ps1` 新增 `Test-ReleaseVersion` / `Get-ReleaseImageTag` / `Get-PushedImageDigest` / `Write-ImageDigestRecord`;`release_preflight.ps1` 制品检查;`.github/workflows/release.yml`;`deploy-config-tests.yml` 触发路径;本文档与 `docs/ops/release-checklist.md` |
| 契约测试 | `artifacts_lib` / `artifacts_fetch_import_retention` / `publish_images` / `make_release` / `release_common_version`,均走 `test_harness.ps1`,自动进 `deploy-config-tests` 与 release.yml 门禁 |

流程:7 个工作包并行实现(文件所有权互斥)→ 每包 3 视角评审(静态正确性 / 标准符合度 / 回归与安全)→ 修复者逐条核实后修。
评审共 60+ 条发现,**修复者对 artifact-core、misc 两包跑完;go、cppjava、gate、artifact-release 四包的修复与跨包集成审查、完整性审查因额度中断未跑**,主会话据评审结论手工补了下列高优先项。

### 7.2 主会话手工修复(2026-09-17)

- `.gitignore`:忽略 `/bin/{gate,scene,battle}`、`/bin/symbols/`、`**/.build/`、`/deploy/k8s/runtime/linux/`。**三份评审独立报的阻断项**:这些是 `build_linux.sh` 与 `k8s_stage_runtime.ps1` 的产物,不忽略则凡在 Linux 上编译过的机器工作树必脏,而 C++ 发布轨拒绝脏树 —— "能构建"与"能发布"互斥。
- `release.yml`:`ext` 步骤补按**目录名**比对 `ExternalReplaceStages`。原来只比模块路径最后一段,而 `go_svc_image.ps1` 比的是 replace 目标目录名;`../../../../proto2mysql-v0.1.0` 能过前者、过不了后者,结果是流水线放行、几十分钟构建后才失败。
- `publish_images.ps1`:① 新增 `Assert-SourceUnchanged`,构建结束后与上线前各复查一次 HEAD/脏树(A §2.9,原来只在开头取一次);② 镜像核对增加 `org.opencontainers.image.version` 与本次发布版本一致(原来只核对 revision,挡不住"包名 v1.2.3、镜像自报另一版本")。
- `docs/ops/release-checklist.md`:digest 记录文件从仓库根改到仓库外 —— 它不被忽略,留在仓库里会让下一次发布因脏树被拒(两份 deploy/k8s 文档已由 misc 包改对)。

### 7.3 遗留缺口(未修,按优先级)

1. **db / data_service 镜像构建不了**(阻断 P1 验收与 go 族发布):`go/db`、`go/data_service` 的 `go.mod` 把 proto2mysql replace 到仓库外 `../../../../proto2mysql-v0.1.0`,目录名与 Dockerfile 占位 stage、`ExternalReplaceStages` 都对不上。根因修法:仿 `go/schemamigrate/go.mod` 改为 require 已发布 tag 并删掉本地 replace(D-14),随后删掉白名单与占位 stage。属另一会话的 proto2mysql 版本治理任务。
2. **C++ 镜像的 revision label 证明不了二进制出自该提交**:`Dockerfile.runtime` 打包的是 `deploy/k8s/runtime/linux` 里预编译的二进制,label 与 ENV 写的却是打包时的仓库 commit。修法:`build_linux.sh`(`MMORPG_STAMP_BUILD=1`)把 `STAMP_SHA` 写进 `bin/BUILD_STAMP`,`k8s_image.ps1` 在带 `-Version` 构建时核对 staging 里的戳与当前 commit 一致,不一致 fail-closed。
3. **release.yml 没有单元测试门禁**:只跑了 `tools/scripts/tests` 契约测试,缺 A 仓 `Jenkinsfile.release` 那段 `go build + go test`(可复用 `go-modules-ci.yml` 的发现口径)与 Java `mvn test`。
4. **`release_preflight.ps1` 缺"digest 已记录"检查**(§4 P4 明列):现在只查 manifest 文件存在且是合法 JSON,`images.digests` 为空也 PASS。
5. `k8s_image.ps1` / `java_svc_image.ps1` 的"带版本号即拒绝脏树"对 `list-refs`、`push` 这类不构建的命令也生效,与 `go_svc_image.ps1` 只在构建命令上拒绝的口径不一致。
6. 跨包接口一致性审查与对照本文档的完整性审查**未执行**(额度中断),§7.1 的"已落码"未经这两道交叉核对。
7. `build_linux.sh` 的 `LIB_PROJECTS` 含 `cpp/libs/services/battle`,但该目录没有 `CMakeLists.txt`(只有 vcxproj,从未生成提交)。

**当前状态:代码与文档已落盘,未编译、未运行、未按 §6 验证。**
