# 服务端发布清单

**Updated:** 2026-09-16(重组:新增 §1–§5 发布流水线;原 2026-05-09 版"HTTP /api/login 上线 Checklist"全文移入文末附录,内容未删);2026-10-16 增补 §6「发版后一次性项」,§2.1 ⑦ 加指针
**适用范围:** 服务端运行镜像(C++ 节点 `mmorpg-node`、Go 服务、Java 网关)的出包、发版、版本追溯、回滚与离线交付
**设计依据:** [release-packaging-standard-20260914.md](../design/release-packaging-standard-20260914.md)(P1–P4)、[CHANGELOG.md](../../CHANGELOG.md)
**脚本入口:** `tools/scripts/publish_images.ps1` → `make_release.ps1` → `release_preflight.ps1`;离线交付 `fetch_images.ps1` → `import_images.ps1`;快照清理 `artifacts_retention.ps1`;公共库 `tools/scripts/lib/release_common.ps1`、`tools/scripts/lib/artifacts_lib.ps1`;GitHub 干净出包 `.github/workflows/release.yml`

> **协作边界(AGENTS.md §9 / §10.1):** 标"人工"的步骤(`git tag`、`git push`、`docker push`、部署到集群)一律由人执行,AI 与 CI 流水线都不做;构建、测试命令由 Codex 或人执行。
>
> **历史引用:** 仓库里既有的 `release-checklist.md §A.2 / §C / §D.1 / §E.2 / §F.B-2 / #B-1` 等引用(`release_preflight.ps1`、`release_common.ps1`、`k8s_image.ps1`、`k8s_deploy.ps1` 的注释)均指文末附录里的同名小节。

---

## 1. 发布流水线总览

### 1.1 两条轨

| 轨 | 用途 | 版本号 | 镜像 tag | 制品版本目录 |
|---|---|---|---|---|
| snapshot | 日常联调、内测包 | 无 | `<12位commit>`;脏树 `<12位commit>-dirty`(需显式 `-AllowDirty`) | `snapshots/images/g<12位commit>/`;脏树 `g<12位commit>-dirty-yyyyMMdd-HHmmss/` |
| release | 正式版本 | `vX.Y.Z` 或 `vX.Y.Z-<预发布标识>` | `vX.Y.Z-<12位commit>` | `releases/images/vX.Y.Z/` |

- **版本号**:必须小写 `v`,数字段不得有前导 0(`v1.2.3`、`v1.2.3-rc.1`)。规则只在 `release_common.ps1` 的 `Test-ReleaseVersion` 定义一次,三个镜像脚本、`publish_images` / `make_release` / `release_preflight`、`release.yml` 都调它。
- **镜像 tag**:由 `Get-ReleaseImageTag` 生成。release tag 带 commit,同一版本号换了提交重打也不会复用 tag;`latest` 等可变 tag 在 staging/prod 被 `Test-ImmutableImageTag` 拒绝。
- **release 轨拒绝脏工作树**,`-AllowDirty` 对 release 轨无效:脏树产物无法从 commit 还原。
- **版本号四处一致**:git tag `vX.Y.Z` = 各脚本的 `-Version vX.Y.Z` = `CHANGELOG.md` 的 `## [X.Y.Z]` 段(方括号内不带 `v`)= 镜像自报版本(OCI label `org.opencontainers.image.version`、`/app/BUILD_INFO`、启动版本行)。`make_release.ps1` 交叉校验 `build-info.app_version`,`release_preflight.ps1 -ReleaseVersion` 发布前再查一遍。
- **部署以 digest 为准**:tag 只是别名,registry 上可能被覆盖;一个版本真正的身份是推送后 registry 上的 `repo@sha256:…`。推送时用镜像脚本的 `-DigestsOut` 记下(`Get-PushedImageDigest` 只认与镜像同 repo 的 digest),经 `make_release.ps1 -ImageDigestsFile` 写进 release manifest 的 `images.digests`;部署前、回滚前、排障时都按 digest 核对(§3、§4)。
  - 现状限制:`k8s_deploy.ps1` 的 Go / Java 镜像参数仍是 tag(`-GoSvcTag` / `-JavaSvcTag`),渲染进 Deployment 的是 `repo:tag`;digest 目前是**核验依据**,还不是渲染进清单的镜像引用。

### 1.2 制品目录(版本库外)

制品根优先级:`-ArtifactRoot` > 环境变量 `MMORPG_ARTIFACT_ROOT` > `<仓库父目录>/artifacts`(本机即 `E:\work\artifacts`)。刻意放在仓库外,GB 级 tar 不会被 `git add -A` 带进版本库。

```text
<制品根>/
├── snapshots/images/<g+sha12>/            快照轨;artifacts_retention.ps1 按保留数清理(默认 dry-run)
├── snapshots/images/latest.json           指针 {version, channel, published_at},制品目录里唯一可变的文件
├── releases/images/<vX.Y.Z>/              发布轨,永不清理
│   ├── images/<镜像 ref 中 / 与 : 换成 _>.tar   每个镜像单独 docker save
│   ├── images-manifest.json               [ { family, service, ref, image_id, revision, file } ]
│   ├── build-info.json                    version / channel / app_version / vcs / source_rev(g<sha12>) / commit /
│   │                                      dirty / image_tag / families / image_count / tables_sha256 /
│   │                                      tables_file_count / published_at / machine / publisher
│   ├── symbols/*.debug                    可选:C++ 分离符号(来自 bin/symbols)
│   └── sha256sums.txt                     "<64位小写hex>  <相对路径>",与 sha256sum -c 兼容
├── releases/images/latest.json
├── releases/manifests/<vX.Y.Z>.json       release manifest,最后落盘,是"已发布"哨兵
└── releases/manifests/<vX.Y.Z>.md         release notes
```

三条铁律(由 `artifacts_lib.ps1` 强制,调用方不要绕开):

1. **不可变**:版本目录 / manifest 已存在即拒绝覆盖,改内容只能提升版本号(快照轨提交新 commit)。`publish_images.ps1 -SkipIfExists` 只在已发布目录出自同一 commit 时静默成功。
2. **原子发布**:先写同级 `.tmp-<名>-<PID>` staging,整目录 rename 上线;期间被他人抢先发布则丢弃 staging 并报"发布竞争"。
3. **可追溯**:`sha256sums.txt` 覆盖版本目录内全部文件,缺文件、哈希不符、清单外多出文件都算失败。**不要往已发布的版本目录里放任何文件**(包括 digest 记录)。

### 1.3 全流程

```text
① 改 CHANGELOG ─► ② 干净工作树 ─► ③ publish_images.ps1 -Version ─► ④(人工)推送镜像 -DigestsOut
                                                                              │
                                   ⑤ make_release.ps1 -Version [-ImageDigestsFile] ◄┘
                                                    │
                                   ⑥ release_preflight.ps1 -ReleaseVersion -ImageTag
                                                    │
                                   ⑦(人工)git tag / git push tag / 部署 / 按 digest 核对
```

GitHub Actions 的 `release.yml` 覆盖其中 ③⑤(外加版本号、CHANGELOG、契约测试门禁),见 §2.2。

---

## 2. 发版步骤

### 2.1 在构建机上发版(完整路径)

前置:构建机有 docker 与 pwsh 7;发 C++ 镜像时 `deploy/k8s/runtime/linux` 已由 `build_linux.sh` + `k8s_stage_runtime.ps1` 填好(`publish_images.ps1` 构建前会跑 `k8s_image.ps1 -Command preflight` 检查);发 Go 镜像时 go.mod 里 replace 到仓库外的目录(当前是 proto2mysql)已检出到 replace 期望的位置。

- [ ] **① 改 CHANGELOG**:把 `## [Unreleased]` 下的条目移到新段 `## [X.Y.Z] - YYYY-MM-DD`(方括号内不带 `v`),在它上方重开一个空的 `## [Unreleased]`。同一版本只能有一段,且不能为空。提交。
- [ ] **② 干净工作树**:切到要发布的提交,`git status --porcelain` 输出为空。
- [ ] **③ 出包**。`-Registry` 填最终要推送的 registry 前缀:镜像名里带着它,④ 推送时才对得上;不填则用各镜像脚本的默认值(当前 `ghcr.io/luyuancpp`)。
  ```powershell
  pwsh -File tools/scripts/publish_images.ps1 -Version vX.Y.Z -Registry <registry>
  # 本机没有 Linux 运行时 staging 时只发 Go 与 Java(默认 -Families 是 cpp,go,java):
  pwsh -File tools/scripts/publish_images.ps1 -Version vX.Y.Z -Registry <registry> -Families go,java
  ```
  产出 `<制品根>/releases/images/vX.Y.Z/`;日志里的 `tag=` 即本版本镜像 tag `vX.Y.Z-<12位commit>`。
- [ ] **④(人工)推送镜像并记录 digest**。要按 digest 部署就必须在 ⑤ 之前做:manifest 生成后不可变,补不进 digest。记录文件放在版本目录**之外**,在同一提交的干净检出上执行,`<registry>` 与 ③ 一致:
  ```powershell
  # 写在仓库**外**(当前目录 = 仓库根):它不被 .gitignore 忽略,留在仓库里会让工作树变脏,
  # 下一次 ③ publish_images.ps1 的发布轨与 release.yml 的"工作树必须干净"就会拒绝发布。
  # 与 deploy/k8s/README.md、deploy/k8s/AGENTS.md 同一写法。
  $digests = Join-Path (Split-Path $PWD.Path -Parent) 'image-digests-vX.Y.Z.json'
  pwsh -File tools/scripts/k8s_image.ps1      -Command push-image -ImageRepository <registry>/mmorpg-node -Version vX.Y.Z -DigestsOut $digests
  pwsh -File tools/scripts/go_svc_image.ps1   -Command push-all   -Registry <registry> -Version vX.Y.Z -DigestsOut $digests
  pwsh -File tools/scripts/java_svc_image.ps1 -Command push       -Registry <registry> -Version vX.Y.Z -DigestsOut $digests
  ```
  只推 ③ 实际发布了的镜像族:`make_release.ps1 -ImageDigestsFile` 要求记录**恰好**覆盖 manifest 里的全部镜像。同一 ref 被记成不同 digest 时 `Write-ImageDigestRecord` 会拒绝——说明同名 tag 被推成了另一份内容,查清再继续。
- [ ] **⑤ 生成 release manifest**:
  ```powershell
  pwsh -File tools/scripts/make_release.ps1 -Version vX.Y.Z -ImageDigestsFile $digests   # $digests 同 ④,在仓库外
  # 纯离线交付、不推 registry 时省略 -ImageDigestsFile
  ```
  修复内容优先级 `-Notes` > `-NotesFile` > CHANGELOG 段落;校验制品 `sha256sums`、`build-info.app_version == -Version`、`dirty=false`;先写 `.md`,最后写 `releases/manifests/vX.Y.Z.json`。结尾打印打 tag 的命令。
- [ ] **⑥ 发布预检(配置 + 制品),必须 exit 0**:
  ```powershell
  pwsh -File tools/scripts/release_preflight.ps1 -ReleaseProfile prod -ReleaseVersion vX.Y.Z -ImageTag vX.Y.Z-<12位commit>
  # 制品根不在默认位置时追加 -ArtifactRoot <制品根>
  ```
  `-ReleaseVersion` 追加的 H 节检查项:`release.version`、`release.changelog`、`release.artifact.dir`、`release.artifact.sha256sums`、`release.buildinfo.version`、`release.buildinfo.clean`、`release.manifest`、`release.imagetag.commit`(给了 `-ImageTag` 时),以及只告警的 `release.buildinfo.commit`——当前工作树不在制品 commit 上时,A–G 节读到的配置不代表该版本,切到发布提交重跑。
  注意:`k8s_image.ps1` / `k8s_deploy.ps1` 在 staging/prod 自动调用的预检**不带** `-ReleaseVersion`,只查配置与 tag;制品检查只靠这一步手动跑。
- [ ] **⑦(人工)打 tag、部署、核对**:
  ```powershell
  git tag vX.Y.Z <12位commit>
  git push origin vX.Y.Z
  ```
  部署沿用 `k8s_deploy.ps1` / `k8s_image.ps1` 的既有命令,`-ReleaseProfile prod`,镜像参数统一用本版本 tag:`-NodeImage <registry>/mmorpg-node:vX.Y.Z-<12位commit>`、`-GoSvcRegistry <registry> -GoSvcTag vX.Y.Z-<12位commit>`、`-JavaSvcRegistry <registry> -JavaSvcTag vX.Y.Z-<12位commit>`。部署前后按 §3.2 核对 digest。
  部署核对完成后,查看 §6 有没有本版本对应的一次性项(2026-10-16 增补)。
  带新持久字段的版本(2026-10-20 增补,审计 r2-ps#1):首次上线之后,发布前先跑 `git diff <线上版本提交> <本版本提交> -- proto/persist_schema_gen.txt`。出现新增字段行(`<世代>=<消息>#<字段号> …`)时,台账的 `state` 行必须已是 `state=active`(§6.2);仍是 `state=bootstrap` 就停止发布,先做完 §6.2。原因:引导期内新增字段都记世代 1,旧 go/db 与新 scene 世代号相同,go/db 的世代校验拦不住,旧 go/db 会静默丢掉新列或读成旧值,见 [mixed-version-persist-field-protection.md](../design/mixed-version-persist-field-protection.md) §8.1 第 6 条。首次上线本身不受此限。
  首次发布含 B0-4(C++ 节点 fd 上限不够就拒启)的版本之前,先按 [gate-kernel-tuning-runbook.md](gate-kernel-tuning-runbook.md) §3.1「上线前置」第 1 步,读各集群各节点池的容器 nofile 硬限并记进 PROGRESS;硬限不够时先处理再发布。拒启的影响面按工作负载形态不同(单副本 battle hostPort 会整体停服),回滚另有 external gate 删 Pod 等额外步骤,见同节「紧急回退」(2026-10-18 增补)。
  共享 Redis 单实例 → 高可用(2026-10-19 增补;部署脚本改动未运行、待 Codex 验证,尚未在任何真实 staging / prod 集群上切换过):staging / prod 首次以缺省档跑 `infra-up`(或不带 `-SkipInfra` 的 `all-up`)时,`-RedisReplicas` 缺省按档位解析为 3,而线上还是单实例,脚本判定 1 → 3(`[RedisTopology]Upgrade:`)。这是一次全服登录中断:在维护窗口里做,先停写,带 `-ConfirmRedisCoreRestart`(没带就在任何写操作之前拒绝),口令不得含空白或单引号;步骤与回退见 [deploy/k8s/README.md](../../deploy/k8s/README.md)「共享 Redis:高可用形态」→「切换与回退」。切完对**每个 zone** 重跑 `zone-up`(WAITAOF 副本份数随之变为 1,入口行 `Redis topology: ... waitaof_replicas=1`),并在各 zone namespace 里 `kubectl rollout restart deploy/db` 与 `deploy/data-service`(只改 ConfigMap 不会滚动它们)。还没切的环境照常发 zone 版本:zone 写路径没给 `-RedisReplicas` 时跟随线上 `sts/redis` 的副本数,显式值与线上不同会被拒绝。

### 2.2 在 GitHub Actions 上出包(`.github/workflows/release.yml`)

用途:在全新检出的托管 runner(ubuntu-latest,linux/amd64)上完成 ③⑤,得到可下载的发布制品。**不推镜像、不打 tag、不部署、不使用任何 secret。**

- 触发:Actions → "服务端版本发布" → Run workflow,选要发布的分支(其 HEAD 上 CHANGELOG 已有 `## [X.Y.Z]` 段)。输入:
  - `version`(必填):`vX.Y.Z`;
  - `families`(默认 `go,java`):`cpp` 需要 Linux 运行时 staging,托管 runner 上没有,会被 `publish_images.ps1` 在构建前拒绝;
  - `proto2mysql_ref`:go.mod 仍把 proto2mysql replace 到仓库外本地目录、且 families 含 go 时必填,40 位 commit sha(tag 与分支可被移动,不接受)。缺了流程会在 step summary 写明原因并失败,不会猜。
- 流程:释放磁盘 → 检出(全量历史)→ `Test-ReleaseVersion` + `Test-ChangelogReleaseSection` + git tag 未被别的提交占用 → `tools/scripts/tests` 全部契约测试 → 按 go.mod 定位仓库外 replace 模块,检出到位并核对模块名与提交 → 工作树干净检查 → `publish_images.ps1 -Version -Families`(`MMORPG_ARTIFACT_ROOT` = runner 临时目录)→ `make_release.ps1 -Version -ImagesVersion` → 上传 → step summary(版本、镜像 tag、镜像数、commit、配置表摘要、镜像清单)。
- 产物:Actions artifact `mmorpg-release-vX.Y.Z`,保留 90 天,内部布局 `images/vX.Y.Z/…` + `manifests/vX.Y.Z.json|.md`。**解压到 `<制品根>/releases/` 下**即可被本文其它脚本直接使用。
- 之后(人工):解压 → §2.1 ⑥ 预检 → §5 离线导入 → 推送 → ⑦。CI 出包未传 `-Registry`,镜像名用镜像脚本的默认前缀;推送用 §2.1 ④ 的命令时须在同一提交的干净检出上执行。
  - CI 出的 manifest 里 `images.digests` 为空(流程不推镜像),且 manifest 不可变:按 digest 部署时,以推送时 `-DigestsOut` 写出的记录文件为 digest 依据,与 manifest 的 `images.image_list[].ref` 逐个对账,记录文件归档在版本目录之外。

---

## 3. 版本追溯排障:"线上跑的到底是哪一版"

### 3.1 五个证据点

| 证据 | 怎么看 | 应当看到 |
|---|---|---|
| Go 服务启动首行 | `kubectl logs <pod> \| head -1`;或 `kubectl exec <pod> -- /app/service -version`、`docker run --rm <ref> -version`(各 Go 服务的 `-version` 参数:打印同一行后退出) | `service starting service=<服务> version=vX.Y.Z commit=<12位> build_time=<UTC> go_version=<go版本>`;二进制含未提交改动时 commit 带 `-dirty`。先于读配置打印,进程在配置阶段崩溃也留得下。`version=dev` 说明没经发布构建注入版本 |
| gate 版本行 | `kubectl logs <gate pod> \| grep '\[gate_version\]'` | `[gate_version] version=… commit=… build_time=… node_type=GATE node_id=… zone_id=…`;启动打两次(第一次 node_id / zone_id 为 `pending`)。version / commit 来自镜像环境变量 `GATE_BUILD_VERSION` / `GATE_BUILD_COMMIT`;gate 没有 `-version` 参数 |
| `/app/BUILD_INFO` | `kubectl exec <pod> -- cat /app/BUILD_INFO` | `version=`、`commit=`、`built_at=` 三项,不依赖进程自己打印 |
| OCI label | `docker image inspect --format '{{json .Config.Labels}}' <ref>` | `org.opencontainers.image.version / revision / created / source / title`;`revision` 是 12 位 commit(`publish_images.ps1` 出包时校验它等于当前 commit) |
| registry digest | `docker buildx imagetools inspect <ref>` 的 `Digest:`;Pod 实际运行的镜像:`kubectl get pod <pod> -o jsonpath='{.status.containerStatuses[*].imageID}'` | 两边的 `sha256:…` 等于 release manifest `images.digests["<ref>"]` |

### 3.2 闭环核对顺序

1. Pod `imageID` 的 digest → 在 `<制品根>/releases/manifests/*.json` 的 `images.digests` 里反查是哪个版本。查不到 = 跑的不是任何已发布版本(同名 tag 被手工推过,或部署了快照)。
2. 该 manifest 的 `source.commit` 与 `images.image_tag` 里的 commit 一致。
3. 容器内 `/app/BUILD_INFO`、启动版本行(Go 首行 / gate `[gate_version]`)的 version 与 commit 等于 manifest;镜像 OCI label `revision` 同值。
4. `git rev-parse vX.Y.Z^{commit}` 指向同一提交,`CHANGELOG.md` 有 `## [X.Y.Z]` 段。

任一环不一致:停止发布 / 扩容,按 §4 回滚到上一个能闭环的版本,再查是哪一步被绕过。

---

## 4. 回滚(按上一版本 manifest)

- [ ] **定位回滚目标**:`<制品根>/releases/manifests/` 里上一个验证过的 `<vX.Y.Z>.json`:
  - `images.image_tag`:各族镜像统一的 tag;
  - `images.image_list[].ref`:完整镜像引用;
  - `images.digests`:推送时记录的 digest(为空说明当时没记录,只能按 tag 回滚,无法证明 tag 未被覆盖)。
- [ ] **核对 registry 上的 tag 仍指向记录的 digest**:对每个 ref 跑 `docker buildx imagetools inspect <ref>`,`Digest:` 必须等于 `images.digests["<ref>"]`。不一致说明 tag 被覆盖过:**不要用这个 tag 回滚**,停下查清是谁推的、推了什么。
- [ ] **部署上一版本**:沿用 `k8s_deploy.ps1` 既有部署命令,`-ReleaseProfile prod`,镜像参数换成上一版:`-NodeImage <registry>/mmorpg-node:<image_tag>`、`-GoSvcTag <image_tag>`、`-JavaSvcTag <image_tag>`。
- [ ] **快速回滚单个 Deployment**:`kubectl rollout undo`(附录 E.2 三级回滚的第 3 级)。它的前提是新旧 revision 的 tag 不同且不可变,本流水线的 `vX.Y.Z-<commit>` tag 满足。
  - **gateway 例外(2026-10-16 补注)**:gateway 的上一个 revision 早于 2026-10-15「探针改走独立管理端口 8082」时,**不要**对 gateway 做 `kubectl rollout undo`。undo 只退 Pod 模板(探针回到 8081),`java-svc-gateway-config` 仍是 `management.server.port: 8082`,三个探针全部失败,startupProbe 约 600s 后杀容器并反复重启,rollout 卡到 progressDeadline,回滚不生效。改用旧 commit 重跑 `k8s_deploy.ps1 zone-up`(ConfigMap 与 Pod 模板同批生成);详见 [deploy/k8s/README.md](../../deploy/k8s/README.md)「gateway 探针、管理端口与回退口径」。更一般地,`rollout undo` 还隐含「旧 revision 与当前 ConfigMap 兼容」这一前提,ConfigMap 不随 undo 回退。
- [ ] **go/db 回滚到 S1(重试只交给分区属主)之前的版本**(2026-10-17 补;已落码,未编译、未测试,待 Codex 验证):先按 [db-retry-owner-routing.md](../design/db-retry-owner-routing.md) §10 切回 RetryRouting=shared 并 rollout restart,用 `go/db/cmd/retry_requeue -check` 确认所有 `kafka:retry:queue:{topic}:p*` 为 0,再回滚镜像。旧版本只认共享收件箱 `kafka:retry:queue:{topic}`、看不到 p*,直接回滚会让其中的存盘滞留(它们在 Redis 里只有这一份);已经回滚的,立即执行 retry_requeue 搬回(告警 DbRetryPartitionedStranded)。上面「部署上一版本」(`-GoSvcTag <上一版>`)与「快速回滚单个 Deployment」(`kubectl rollout undo deploy/db`)两条路径都适用;步骤:
  1. **切回 shared 并等滚动完成**(`<zone ns>` 即 `mmorpg-zone-<z>`):配置里是 owner 时,把 `go/db/etc/db.yaml` 的 `RetryRouting` 改回 `shared` → `k8s_deploy.ps1 zone-up` → `kubectl -n <zone ns> rollout restart deploy/db` → `kubectl -n <zone ns> rollout status deploy/db`(只改 ConfigMap 不会滚动 db);金丝雀留下的环境变量用 `kubectl -n <zone ns> set env deploy/db DB_RETRY_ROUTING-` 去掉(同样触发滚动,也要等 rollout status)。`kubectl set env` 加的变量不在 last-applied 里,`k8s_deploy` 的 apply 不会删它,且环境变量优先于配置键,必须单独去掉。
  2. **三项人工核对**(retry_requeue 的退出码 5 只看得见会登记模式的 go/db,更早的 S1 镜像不登记,所以这三项不能省):
     - 每个 db Pod 的 `db_retry_routing_mode{mode="owner"}` 都是 0(Prometheus 查 `db_retry_routing_mode{namespace="<zone ns>", mode="owner"}`;S1 之前的 Pod 没有这条序列,不算 owner);
     - `kubectl -n <zone ns> set env deploy/db --list` 里没有 `DB_RETRY_ROUTING=owner`;
     - `kubectl -n <zone ns> rollout status deploy/db` 已完成。
  3. **只读盘点,按退出码判断**(工作目录 `go/db`;`-zone` 必填:R7 之前缺省取配置文件里的 ZoneId,漏了会盘点别的 zone 的 topic 并以 0 退出(fail-open);现在不给 `-zone` 直接以退出码 1 退出,一条不动;K8s 上 go/db 的 `RedisClient` 是 `redis.<InfraNamespace>:6379`、DB 0;口令只经环境变量传入,名字由 `-redis-password-env` 指定,不上命令行):
     ```powershell
     kubectl -n <InfraNamespace> port-forward svc/redis 16379:6379
     go run ./cmd/retry_requeue -f etc/db.yaml -zone <z> -redis 127.0.0.1:16379 -redis-db 0 -redis-password-env REDIS_PASSWORD -check
     ```
     `REDIS_PASSWORD` 是执行者自己设的环境变量(名字可换);Redis 不设口令的档位(dev)去掉 `-redis-password-env`,口令取 `-f` 配置里的值(仓库自带的为空)。
     topic 由 `-f` 配置的 `ServerConfig.Kafka.TopicGeneration` 推导,该 zone 的代次与本地配置不同时加 `-topic-generation <N>`。**只有退出码 0 才允许回滚**:
     - 0:p* 全 0,且登记表里没有 owner 实例、目标在用;
     - 3:p* 还有存盘。shared 实例每拍兜底排空 p*,稍后重跑;一直不降时看 DbRetryPartitionedNoProgress、本 zone 主库是否在冷却;
     - 4:目标看起来没在用(登记表为空,共享收件箱、死信、旧共享 processing、全部 p* 都是 0),多半是 `-f` / `-zone` / `-topic-generation` / `-redis` / `-redis-db` 指错了,核对后重跑;只有该 zone 的 go/db 已全部停机时才加 `-allow-empty-target`;
     - 5:登记表里还有登记为 owner 模式的实例(输出逐行列出实例名与模式),回到第 1 步;
     - 1:参数、配置、Redis 连接或盘点出错,修好原因后重跑。
  4. **回滚镜像时认准 revision**:第 1 步的 `rollout restart` / `set env` 本身会生成新的 revision,之后不带参数的 `kubectl rollout undo deploy/db` 回到的是切换前那个 S1 revision,不是 S1 之前的镜像。先 `kubectl -n <zone ns> rollout history deploy/db` 找到 S1 之前镜像的 revision,用 `kubectl -n <zone ns> rollout undo deploy/db --to-revision=<N>`;或者走「部署上一版本」。ConfigMap 里的 `RetryRouting` / `WaitAOF*` 不用先改:go/db 用的 go-zero v1.10.0 忽略未知键(`go/db/internal/config/config_test.go` 的 `TestConfigLoaderIgnoresUnknownKeys` 守住)。回滚完成后建议再跑一次第 3 步确认 p* 仍为 0(回到的镜像早于按实例 processing 列表、不往登记表里写时,共享收件箱、死信、旧共享 processing(`kafka:retry:processing:{topic}`,这类镜像的在途收据就在这里)也都为 0 时以 4 退出,参数核对无误即可;旧共享 processing 非 0、p* 为 0 时以 0 退出)。
  5. **已经回滚了(没做上面几步,或紧急回滚)**:约 10 分钟内会响 DbRetryPartitionedStranded。立即按第 2 步核对(此时 db 应已全是 S1 之前的镜像),用第 3 步同一组参数先跑 `-check`,再去掉 `-check` 执行搬回(把 p* 搬回共享收件箱;同样过退出码 4 / 5 两道门,一条不动就退出;可重入,中途失败修好原因后重跑)。搬完它会再盘点一次,退出码 0 才算搬完,3 表示还有实例在把收件箱分拣回 p*。搬回完成之前不要清理任何 Redis 键,也不要重启或清空这台 Redis。告警按 Prometheus 里 6h 内的最后样本判定,搬回之后窗口内仍会响,以 `-check` 退出码 0 为准并静默。
- [ ] **写过落点记录之后,禁止把 go/db 回滚到不认落点记录的版本**(2026-10-23 补;规则本身来自 [player-storage-placement.md](../design/player-storage-placement.md) §13 与 [merge-zone-runbook.md](merge-zone-runbook.md) §14「上线顺序红线」第 5、8 条,本清单原来没有)。旧版 go/db 按本进程 zone 选库,会把已经 pin 合服、relocate 或建角钉过落点的玩家写进 `zone_<z>_db`,与真源分叉,全程零报错。上面「部署上一版本」与 `kubectl rollout undo deploy/db` 两条路径都受这条约束。
  - **怎么判断「写过」**:任一玩家已有落点记录,即全局库 `player_route` 里有 `storage_id IS NOT NULL` 的行(`SELECT 1 FROM player_route WHERE storage_id IS NOT NULL LIMIT 1`),或 mapping Redis 里有 `player:placement:*` 键。拿不准就按「写过」处理。
  - **还没写过**:可以照常回滚,但要等满 90s 或手工 `DEL db:capability:zone:<Z>` 之后,才能做 pin 合服、relocate、打开 `PinOnCreate`(红线第 7 条)。
  - **任一 go/db 开过 `Placement.Required=true`**:另外不许把 login 的 `PinOnCreate` 改回 false,不许把 login / data_service 回滚到不支持 storage_id 的版本(红线第 8 条)。要回滚这些组件,先关掉全部 go/db 的 `Required`。
- [ ] **回滚后核对**:按 §3.1 确认 Pod `imageID` digest、启动版本行、`/app/BUILD_INFO` 都已是上一版本;再做附录 E.3"回滚后必做"。
- **数据不随代码回滚**:库结构迁移只前进(`go/schemamigrate` 只有 `Up`;trade 的迁移 Job 以 `-migrate` 运行),回滚前确认上一版本能在当前库结构上运行。需要把数据回到某个时间点时走 `tools/scripts/k8s_zone_rollback.ps1`(`-NodeImage` 必填,且必须是不可变引用)与 [zone_data_rollback.md](../design/zone_data_rollback.md)。

---

## 5. 离线交付(目标机无 registry)

- [ ] **取包**(在能访问制品根的机器上;共享盘挂载路径不同就设 `MMORPG_ARTIFACT_ROOT` 或传 `-ArtifactRoot`):
  ```powershell
  pwsh -File tools/scripts/fetch_images.ps1 -Channel release -Version vX.Y.Z -OutDir <落地目录>
  ```
  先校验源目录 `sha256sums.txt`,复制到同级 `.fetching-*` 临时目录后再校验一次,通过才 rename 成落地目录。`-OutDir` 缺省为 `<仓库>/deploy/offline-images/vX.Y.Z`(已在 .gitignore 里);落地目录已存在时需加 `-Force`。
- [ ] **拷到目标机后导入**:
  ```powershell
  pwsh -File tools/scripts/import_images.ps1 -Dir <落地目录>
  ```
  有 `sha256sums.txt` 就先校验;按 `images-manifest.json` 逐个 `docker load`,每个导入后 `docker image inspect` 的镜像 ID 必须等于清单里的 `image_id`。
- [ ] **导入后核对**:抽查 §3.1 的 OCI label 与 `/app/BUILD_INFO`。离线环境没有 registry digest,以 `images-manifest.json` 的 `image_id` 为镜像身份依据。
- 快照包同理:`fetch_images.ps1` 不带参数时取 `snapshots/images/latest.json` 指向的版本。快照清理:`pwsh -File tools/scripts/artifacts_retention.ps1`(只预览)/ `pwsh -File tools/scripts/artifacts_retention.ps1 -KeepLast 5 -Force`(真删),只动 `snapshots/`,`releases/` 永不触碰。

---

## 6. 发版后一次性项

只在**第一次**部署含对应改动的版本之后做一次,做完在 `docs/PROGRESS.md` 记一条(环境、时间、读数),之后的发版不再重复。在 staging / prod 执行属于触碰共享环境,**须负责人明确指定环境并授权**(AGENTS.md §9),AI 与 CI 都不执行。本机开发 / 压测环境每轮都 `FLUSHALL`,不用单独做。

### 6.1 清理共享核心 Redis 里的存量 `dirty_keys_set`(2026-10-16 增补)

- **背景**:含 2026-10-16 落码提交(提交号待提交后回填;2026-10-20 回填:`e76f6f747`,用 `git log -S dirty_keys_set -- cpp/libs/engine/infra/storage/redis_client/redis_client.h` 核实)的 C++ scene 镜像,两段存盘 Lua 不再 `SADD` 全服单键 `dirty_keys_set`;没换镜像的 scene 实例仍在写。这个键全仓没有读者、没有 TTL,只随「累计存过盘的玩家数」增长;新版本不再写它,旧版本写下的存量要一次性删掉。新旧版本混跑与回滚都无害(旧二进制只是继续写一个没人读的键),所以清理不是发版的前置条件,但必须在下面的门禁之后做。依据见 [capacity-model-hok-scale.md](../design/capacity-model-hok-scale.md) A0-4、[shared-redis-split-by-purpose.md](../design/shared-redis-split-by-purpose.md) §2.5 第 1 条。
- **对象**:C++ `zoneredis` 指向的共享核心 Redis 的 DB 0(C++ 不发 `SELECT`)。K8s 上是 `<InfraNamespace>` 里的 StatefulSet `redis`(Pod `redis-0`,Service `redis.<InfraNamespace>:6379`);届时若已换成别的实例,以 `k8s_deploy.ps1` 生成的 zoneredis 配置为准。若届时已切到 `deploy/k8s/manifests/infra/redis-ha.yaml`(1 主 2 从 + 哨兵 + haproxy 代理;2026-10-15 时尚未接入部署路径,2026-10-16 起由 `k8s_deploy.ps1 -RedisReplicas` 接入,staging / prod 档缺省即此形态,接入改动未运行、待 Codex 验证),`redis-0` 不一定是主:在从库上 `UNLINK` 会报 `READONLY`,`OBJECT IDLETIME` 也只反映那个从库上的访问。所以进 Pod 后先跑下面的 ⓪ `ROLE`,不是 master 就换到 `ROLE` 为 master 的那个 `redis-N` 上重来(两份清单里 Redis 容器都叫 `redis`)。
- [ ] **门禁:版本盘点**(必须先过,不能用 `OBJECT IDLETIME` 代替,原因见下一条)。确认**所有** scene 实例都已换成含该改动的镜像,包括 Agones 里仍是 `Allocated` 的旧 GameServer(它们不随 Fleet 滚动立即退出,会继续写这个键):
  ```powershell
  # scene 的 Pod:Deployment 与 Agones 两种编排的 Pod 都带 app=<scene 池名>;每个容器按「容器名=imageID」列出
  kubectl get pods -A -l 'app in (scene,scene-world,scene-instance)' -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}app={.metadata.labels.app}{"\t"}{range .status.containerStatuses[*]}{.name}={.imageID}{" "}{end}{"\n"}{end}'
  # Agones GameServer,重点看 STATE=Allocated 的那些;GameServer 的 Pod 与它同名,版本以上一条里该 Pod 业务容器的 imageID 为准
  kubectl get gameservers -A -l 'app in (scene,scene-world,scene-instance)' -o custom-columns='NS:.metadata.namespace,GS:.metadata.name,STATE:.status.state,IMAGE:.spec.template.spec.containers[*].image'
  ```
  - **只核对业务容器**:容器名与 Pod 的 `app` 标签同值(`k8s_deploy.ps1` 里 Deployment 模板是 `- name: $NodeName`,Fleet 模板是 `- name: $FleetName`,都与 app 标签同值)。Agones 注入的 SDK sidecar `agones-gameserver-sidecar`(以普通容器还是原生 sidecar 注入取决于 Agones 的特性门,未核实)和 C++ 日志 sidecar(原生 sidecar,在 `initContainerStatuses` 里,上面的命令本来就不列)都不核对,它们的 digest 不会出现在任何 release manifest 里。
  - **逐个确定业务容器的源码提交 `<commit>`**,再跑 `git merge-base --is-ancestor <落码提交> <commit>`,返回 0 才算已含改动:
    1. 正式发布的镜像:`imageID` 的 digest 按 §3.2 第 1 步反查到 release manifest,取它的 `source.commit`。
    2. 反查不到 manifest 的(快照部署,staging 常见;CI 出包的 manifest `images.digests` 为空,见 §2.2):改用 §3.1 的 `/app/BUILD_INFO`,即 `kubectl -n <ns> exec <pod> -c <app 值> -- cat /app/BUILD_INFO` 里的 `commit=`;或镜像 OCI label `org.opencontainers.image.revision`。commit 带 `-dirty` 的镜像出自未提交的工作树,commit 证明不了内容,按「无法确定」处理。
  - **按判定结果处理**:
    - 全部返回 0:门禁通过。
    - 确认不含(返回 1):这是仍会写这个键的旧实例。Deployment 的 Pod 等滚动换完;`Allocated` 的 GameServer 上有在线玩家,等它自然结束退出,不要为清理这个键删它(确需提前,只在维护窗口、确认没有在线玩家后再删)。处理完回来重做盘点。
    - 无法确定(两种来源都拿不到 commit,或带 `-dirty`):门禁不过。停下按 §3.2 查清它跑的是什么,**不要**据此删实例。
- [ ] **辅助观测(不作门禁)**:`OBJECT IDLETIME dirty_keys_set` 必须是对这个键执行的**第一条**命令(其他读命令会刷新 LRU,`OBJECT` 不会);隔至少一个完整的周期存盘(默认 300s,建议 10 分钟以上)再读一次。读数归零说明还有写者或读者,停下排查。读数一直增大**不能**证明没有旧写者:旧二进制只在真写时才 SADD,快照相等的存盘会被跳过;`BGSAVE` / `BGREWRITEAOF` 子进程运行期间,读写也不刷新 LRU。
- [ ] **按序执行**(口令不上命令行:进 Pod 后用容器自带的 `REDIS_PASSWORD` 导出成 `REDISCLI_AUTH`;dev 档没有口令时这一步什么都不做):
  ```sh
  kubectl -n <InfraNamespace> exec -it redis-0 -c redis -- sh
  # 以下在 Pod 内执行
  [ -n "$REDIS_PASSWORD" ] && export REDISCLI_AUTH="$REDIS_PASSWORD"
  redis-cli ROLE                             # ⓪ 第一行必须是 master,否则退出换 Pod(见上面「对象」);它不碰下面的键,不影响 ① 的读数
  redis-cli OBJECT IDLETIME dirty_keys_set   # ① 必须是对这个键的第一条命令
  redis-cli TYPE dirty_keys_set              # ② 期望 set;none = 没有存量,直接做 24 小时复查
  redis-cli SCARD dirty_keys_set             # ③ O(1),记下规模
  redis-cli MEMORY USAGE dirty_keys_set      # ④ 默认 SAMPLES 5 抽样;不要带 SAMPLES 0(O(N),阻塞),也不要 SMEMBERS
  redis-cli UNLINK dirty_keys_set            # ⑤ 主线程只摘键,内存由后台 lazyfree 释放;禁止 DEL(大 set 在主线程逐个释放成员,阻塞数秒)
  redis-cli EXISTS dirty_keys_set            # ⑥ 期望 0
  ```
- [ ] **24 小时后复查**:再执行一次 `EXISTS dirty_keys_set`,期望仍为 0。被建回说明还有旧二进制在跑:回到版本盘点找到并处理掉它,再重复 ⑤⑥。
- **兜底**:托管 Redis 禁用或改名了 `UNLINK` 时,循环执行 `SPOP dirty_keys_set 1000`,批与批之间短暂停顿,直到 `SCARD` 为 0;不要用 `SSCAN` + `SREM` 全量遍历。
- **恢复后重做**:之后若从清理前的 AOF / RDB 备份恢复了这个 Redis(K8s 上是 PVC + appendonly),存量键会跟着回来,恢复后重做 ⓪、②~⑥ 与 24 小时复查。scene 镜像回滚到不含改动的版本也会把键重新建起来(无害),再次升级后同样重做版本盘点与 ②~⑥。
- **预期与记录**:`INFO memory` 的 `used_memory` 下降;RSS 不一定立刻回落(jemalloc)。把环境、时间、③④ 的读数和两次 `EXISTS` 结果记进 `docs/PROGRESS.md`。此前的规模与内存数字都是推算,以这里的读数为准。

### 6.2 首次上线后:关闭持久 schema 引导期,台账切到 `active`(2026-10-20 增补,审计 r2-ps#1)

- **背景**:台账 `proto/persist_schema_gen.txt` 的 `state=bootstrap` 期间,生成器把新增持久字段一律记为世代 1。上线后台账若仍是 `bootstrap`,第一次带新持久列的滚动发布里,新旧二进制世代号相同、认识的字段集合不同,go/db 的 C1 / C1b / C2 都拦不住:旧 go/db 会把新列静默丢掉或读成旧值,scene 随后把它写成零值或旧值。新列若是限购或幂等账本,就会重复发放。推导见 [mixed-version-persist-field-protection.md](../design/mixed-version-persist-field-protection.md) §8.1 第 6 条。
- **时机**:首次上线(go/db 以 dual + 严格提交屏障整批起步)之后、**第一次发布带新持久字段的版本之前**。只做一次;台账不能从 `active` 改回 `bootstrap`,生成器会拒绝。
- [ ] **① 逐个 zone 关闭引导期**:按 [deploy/k8s/README.md](../../deploy/k8s/README.md)「玩家持久 schema 世代」的「关闭引导期」执行(负责人执行;先确认全部 go/db Pod 上 `db_persist_schema_unstamped_write_total` 持续一个完整存盘周期不再增长,再写 `db:persist_schema:bootstrap_closed:zone:<zone>` = `closed`,`db_persist_schema_bootstrap_closed` 变为 1 即生效)。
- [ ] **② 主干上把台账切到 `active`**:全部 zone 都做完 ① 之后,由负责人在主干提交里把 `proto/persist_schema_gen.txt` 的 `state=bootstrap` 改为 `state=active`(只改这一行),再按 AGENTS.md §4 跑一次 proto 生成(Codex 执行),确认生成通过、台账其余行不变。此后每次出现新增字段的生成运行都会追加一个新世代。
- [ ] **③ 记录**:在 `docs/PROGRESS.md` 记一条:各 zone 关闭引导期的时间、`state=active` 的提交号。
- **门禁**:② 完成之前,不得发布任何新增持久字段的版本(§2.1 ⑦ 的台账比对)。机器门禁暂缺(生成器或发布预检都还不检查台账状态与上线状态的关系),目前只靠这一条人工核对。

---

# 附录:2026-05 HTTP 登录链路专项(历史)

> **时效说明:** 以下是 2026-05-09 为"新登录链路(HTTP `/api/login`)从灰度到全量上线"写的专项清单,2026-09-16 重组本文档时整体原样移到这里(仅标题下调一级),未随之后的代码与部署方式更新。其中的测试计数、commit 数、T+0~T+3 时间线、单机阈值都是当时的状态;通用条目(配置审计、内核参数、ulimit、监控基线、回滚 SOP)引用前请对照当前代码与 `release_preflight.ps1` 核实。
> 仓库里既有的 `§A.2 / §C / §D.1 / §E.2 / §F.B-2 / #B-1` 引用都指本附录。

## 上线前 Checklist (HTTP /api/login 路径)

**Date:** 2026-05-09
**目的:** 把"新登录链路"从灰度到全量上线的所有动作 / 阈值 / 回滚 SOP 沉淀成一张清单。
**前置阅读:** [ARCH.md §11-12](../design/ARCH.md), [open-server-rate-limit-design.md](../design/open-server-rate-limit-design.md), [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md)

---

### A. 部署前(代码已合,准备发布镜像)

#### A.1 代码 / 测试

- [ ] (2026-10-19 增补)开服前由 AI 复核「登录取逻辑服 / 物理服是不是瓶颈、能不能水平扩展」:见 `docs/design/tidb-phase1-and-launch-readiness.md` §7 L-18 与 `player-route-mapping-durability-proposal.md` §12.2,结论写回文档后才算过这一项。(2026-10-20:静态部分已复核,见提案 §12.3;第 5 步压测未做,前置观测见拍板清单 X-3,本项保持未勾。)

- [ ] `mvn test` Java Gateway 全绿(当前 34/34)
- [ ] `go build ./...` go-zero login + player_locator + scene_manager + data_service + db 全 EXIT=0
- [ ] cpp gate / scene MSBuild Debug + Release 全过
- [ ] robot 单二进制可产出(`go build -o robot.exe .` ~26 MB)
- [ ] `git log --oneline` 包含本轮 11 个 commit(自 `4d6c70482 docs: onboarding...` 起)

#### A.2 配置审计

- [ ] `java/gateway_node/src/main/resources/application.yaml`
  - `gate.rate-limit.enabled: false` (上线**默认关**,T+0 灰度才开)
  - `login.grpc.endpoints` 指向真实 login.rpc(staging 51000;prod 50000)
  - `gate.token-secret` 与 `bin/etc/base_deploy_config.yaml.gate_token_secret` 一致(不一致时 cpp gate 验签全 fail)
- [ ] `go/login/etc/login.yaml`
  - `GateTokenSecret` 与上同步
  - `AuthProviders.WeChat` / `QQ` 真实 AppId/AppSecret 通过环境变量注入
  - `TokenConfig.AccessTokenTTL=2h` / `RefreshTokenTTL=720h`
- [ ] `go/player_locator/etc/player_locator.yaml`
  - `LeaseTTL` ≥ 30s(否则 30s 重连窗口失效)
- [ ] `bin/etc/base_deploy_config.yaml`
  - `Kafka.Brokers` 至少 3 个 broker(单 broker 见 §F.B-2)
  - `etcd_hosts` 与服务一致

#### A.3 Linux 内核(gate 机器)

固化到 `/etc/sysctl.d/99-gate.conf`,然后 `sysctl --system`:

```ini
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse        = 1
net.ipv4.tcp_max_tw_buckets  = 1048576
net.ipv4.tcp_fin_timeout     = 15
net.core.somaxconn           = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.netfilter.nf_conntrack_max = 1048576
net.ipv4.tcp_keepalive_time   = 300
net.ipv4.tcp_keepalive_intvl  = 30
net.ipv4.tcp_keepalive_probes = 3
```

⚠️ **不开** `tcp_tw_recycle`(NAT 环境会丢)。
完整说明: [docs/ops/gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md)

#### A.4 服务 ulimit(gate / scene)

- [ ] `LimitNOFILE=1048576` 写入 systemd unit
- [ ] `LimitNPROC=65535` 写入 systemd unit
- [ ] `/etc/security/limits.d/gate.conf` 同步配置

---

### B. 监控基线(发布前布点)

#### B.1 必须 dashboard

| 面板 | 数据源 | 关键阈值 |
|---|---|---|
| 登录成功率 (HTTP) | gateway log `code:0 / 100 / 401 / 429 / 500` | 95% 持续 5 分钟 → 告警 |
| 登录 latency p99 | gateway access log | > 500 ms 持续 1 分钟 → 告警 |
| Login.Login QPS / decision 分布 | go-zero login stat + decision counter | decision=2 (REPLACE) 占比 > 5% → 告警 |
| Refresh-token rotation QPS | gateway `/api/refresh-token` | 突涨 10x → 怀疑攻击 |
| Bucket4j 限流命中率 | Java JMX | rate limited count |
| **legacy_login_count vs new_login_count** | go-zero login deprecation 计数器 | T+0 灰度期 legacy 单调下降 |
| gate ephemeral port | `node_exporter` `node_sockstat_TCP_*` | TIME_WAIT > 50% port range → 告警 |
| gate accept queue 溢出 | `nstat TcpExtListenOverflows` | > 0 → 严重告警 |
| Kafka `gate-{gateId}` lag | kafka-ui / cmak | > 1000 → 告警 |
| player_locator session ONLINE 数 | redis ZCARD player_locator:lease_zset + SCAN player:session: | 偏差 > 20% → 怀疑 #B-1 类 leak |

> **2026-10-18 更正(B0-1 会话对账按 gate 建索引)**:上表最后一行的数据源写错了,而且不能照做:
> `player_locator:lease_zset` 这个键不存在(断线租约 ZSET 是 `player:leases`);生产核心 Redis 上也**不要**跑 `SCAN player:session:*` 全键扫描 —— 目标规模下一遍要数小时,和业务抢同一个单主。改为:
> - 在线数上界:指标 `player_locator_gate_index_members`(只由会话对账 leader 产出,按 `player_locator_reconcile_is_leader == 1` 过滤后取 max)。它是**上界**,不是精确在线数:存活 gate 索引里的脏成员要等日审计清掉;
> - 断线中(DISCONNECTING、等租约到期)的人数:`ZCARD player:leases`;
> - 抽查单个 gate:`SMEMBERS player:gate_index:gates` 列出登记过的 gate 实例 uuid,再 `SCARD player:gate_index:sessions:<uuid>`;
> - 「偏差 > 20%」的对照基线改为 gateway / login 侧的在线计数;对账本身有没有在跑,看 `deploy/k8s/player-locator-alerts.yaml` 的 NoLeader / Stalled。
> - 单个玩家卡在 ONLINE 的定向补救见本附录 F.B-1 末尾「定向补救」。机制见 [player-locator-gate-session-index.md](../design/player-locator-gate-session-index.md)。

#### B.2 日志关键词

错误关键字(grep + 告警):
- `[DEPRECATION]` → 知道老路径还在用,不告警
- `kLoginAccountNotFound` `kLoginInProgress` → 用户体感
- `Failed to parse NodeInfo` → 已修(allocated 过滤),如再出现说明 etcd schema 变更
- `Connect to ipv4#.*:9092 failed` → kafka 挂了
- `bind: An attempt was made to access a socket in a way forbidden` → 端口冲突(Windows)/ 占用
- `[disconnect] get account failed` → session 残留(参考 #B-1)
- `panic:` `fatal:` `OOM` `Killed` → P0

---

### C. 灰度切换步骤(T+0 → T+3,详见 ARCH §12)

#### T+0(2026-05):内测灰度

- [ ] robot 全量用 `use_http_login: true`(已有 `robot/etc/robot.stress-*.yaml`)
- [ ] 内测客户端(version >= X)启用 `/api/login` 路径
- [ ] Gateway `rate-limit.enabled: false`(生产观察日志再开)
- [ ] 退出条件:**新路径成功率 ≥ 老路径** 且 **`[DEPRECATION]` 计数稳定**

#### T+1(2026-06):全量切换

- [ ] 线上客户端默认走 `/api/login`
- [ ] Gateway 按 zone 灰度开 limiter:
  ```yaml
  gate.rate-limit:
    enabled: true
    zone-default-rps: 500
    zone-default-burst: 1000
    zone-overrides:
      1: { rps: 2000, burst: 5000 }   # 主力 zone
  ```
- [ ] 老路径保留兜底,但日志 `[DEPRECATION]` 监控降到 < 1%
- [ ] 退出条件:全量 24 小时稳定,refresh-token 走 HTTP 比例 > 90%

#### T+2(2026-07):老路径下线

- [ ] go-zero login 加 feature flag `legacy-gate-login-enabled: false`
- [ ] 老路径计数 0 持续 2 周
- [ ] 退出条件:零误伤客户端

#### T+3(2026-08):代码移除

- [ ] cpp gate `HandleGrpcNodeMessage` 删除对 `ClientPlayerLoginLoginMessageId` 的路由(只删 Login,**保留** EnterGame/CreatePlayer/LeaveGame/Disconnect/RefreshToken)
- [ ] go-zero `deprecation.go` 删除
- [ ] `proto/message_id.txt` 标记 #48 deprecated

---

### D. 开服日 SOP

#### D.1 开服前 1 小时

- [ ] redis FLUSHDB(测试库)+ 真实库做 `BGSAVE` 备份
- [ ] kafka topic 提前创建:`gate-0` ~ `gate-{N}` partition=8,retention 5min
- [ ] etcd 空间:CronJob `etcd-compact` 最近一次成功、`etcdctl alarm list` 为空、各成员 DB SIZE 低于配额一半
      (命令见 [etcd-maintenance-runbook.md](etcd-maintenance-runbook.md) §4.3、§5.1)。需要 defrag 时按该手册 §5 在更早的低峰窗口逐成员做,
      不要压在开服前 1 小时
- [ ] Gateway 配置波次 schedule:
  ```yaml
  gate.rate-limit.wave:
    enabled: true
    start-epoch-sec: <开服时刻 unix>
    schedule:
      - { offset-sec: 0,   allow-zones: [1, 2] }
      - { offset-sec: 30,  allow-zones: [3, 4] }
      - { offset-sec: 120, allow-zones: [-1] }   # 全开
  ```
- [ ] 客户端公告热更:开服时间 / 排队提示 UI / 重试策略

#### D.2 开服瞬间(T0 ~ T+5min)

监控值班(必须 2 人):
1. p99 登录延迟突涨? → 看 Gateway/login QPS 是否打满
2. 排队队列深度? → `gate_assign_queue_depth` 跌 0 = 限流没生效
3. cpp gate ephemeral port? → 接近 50% → 准备扩 IP / 横向加 gate
4. kafka lag 突涨? → 加分区或加 broker

#### D.3 开服后(T+5min ~ T+1h)

- [ ] 全 zone wave 已开
- [ ] Gateway QPS 平稳 < 单实例 1万
- [ ] login QPS 平稳 < 单实例 1.5万
- [ ] gate 长连数 < 单进程 5 万

---

### E. 回滚 SOP

#### E.1 触发条件(任一即立即回滚)

- 新路径登录成功率 < 90%(老路径 99%+)
- p99 > 2s 持续 5 分钟
- Gateway/login OOM 或 panic
- 数据完整性问题(玩家进游戏看到错乱数据)

#### E.2 三级回滚

**1 级 - 关限流**(30 秒生效):
```bash
# Gateway 配置中心改 gate.rate-limit.enabled=false
# 或环境变量:GATE_RATE_LIMIT_ENABLED=false
kubectl rollout restart deploy/gateway-node -n mmorpg
```

**2 级 - 切回老路径**(2 分钟生效):
```yaml
# 客户端配置中心(或灰度服务):
useHttpLogin: false     # 客户端走老的 gate Login RPC 路径
```
go-zero login 不动,因为它对两条路径都兼容。

**3 级 - 完全回滚版本**(5-10 分钟):
```bash
# k8s rollback 到上一个 tag
kubectl rollout undo deploy/gateway-node -n mmorpg
kubectl rollout undo deploy/login -n mmorpg
# cpp gate 是 statefulset,谨慎滚动
```

> **2026-10-16 补注**(上面与第 1 级代码块原文保留):`deploy/gateway-node -n mmorpg` 是 2026-05 的旧名。当前 K8s 上 Java 网关的 Deployment 叫 `gateway`,位于各 zone namespace(`<NamespacePrefix>-<zone>`,如 `mmorpg-zone-<zone>`);第 1 级的 `rollout restart` 同样要换成 `kubectl -n <zone ns> rollout restart deploy/gateway`。gateway 的上一个 revision 早于 2026-10-15 探针改端口时,不要对它做 `rollout undo`(原因见 §4「快速回滚单个 Deployment」的 gateway 例外),改用旧 commit 重跑 `k8s_deploy.ps1 zone-up`。第 1 级「Gateway 配置中心改 `gate.rate-limit.enabled=false`」是 2026-05 的口径,在当前 K8s 部署下是否仍有效未核对。

#### E.3 回滚后必做

- [ ] 抓 5 分钟 Gateway / login / gate 日志归档(给事后复盘)
- [ ] dump redis `player:session:*` 样本 100 个
- [ ] dump etcd `--prefix LoginNodeService.rpc/`
- [ ] 写一份 RCA(根因分析),最迟下个工作日

---

### F. 已知风险 & 应对

#### F.B-1: dev 重启 robot 后 player_locator session 残留 ONLINE

- **状态:** 治标 (E) + 治本 (G) 都已合
- **治标:** robot main `defer sendDisconnectBestEffort(gc)` 主动通知 login.Disconnect (commit `0f4712996` 之后)
- **治本:** cpp gate `HandleConnectionDisconnection` 把 `SessionInfo.playerId` 填进 `SessionDetails`,login `markPlayerSessionDisconnecting` 真的能跑(本轮 commit)
- **生产影响:** 玩家正常 TCP 断会触发 OS-level RST → gate 同样路径,本来就走治本逻辑,所以**生产无症状**;dev 重跑机器人时遇到

**定向补救(2026-10-18 增补):单个玩家的会话卡在 ONLINE**

login 的 `markPlayerSessionDisconnecting` 退避重试 4 次仍失败时打 CRITICAL 日志 `CRITICAL: session stuck ONLINE — SetSessionDisconnecting exhausted retries player=<player_id> session=<session_id> gate_instance="<uuid>" ...`(日志里写的「#B-1 sweep」就指这里)。这类会话所在的 gate 还活着,会话对账**看不到**它(对账只处理 etcd 里已消失的 gate,机制见 [player-locator-gate-session-index.md](../design/player-locator-gate-session-index.md));gate 已死的那一类不用人工处理,对账会在 2~3 拍(约 2~3 分钟)内补投 DISCONNECTING。处置按玩家逐个来,**不要**在生产核心 Redis 上全键 SCAN:

1. 从 CRITICAL 日志取 `player`、`session`、`gate_instance`。
2. 转发到该 zone 的 player-locator(不要把服务暴露到集群外),在仓库根目录执行(proto 的 import 路径以仓库根为准;staging / prod 不开 gRPC 反射,必须带 `-proto`):
   ```bash
   kubectl -n <zone namespace> port-forward svc/player-locator 50100:50100
   grpcurl -plaintext -import-path . -proto proto/player_locator/player_locator.proto \
     -d '{"player_id": <player_id>}' 127.0.0.1:50100 playerlocator.PlayerLocator/GetSession
   ```
3. 只有 `found=true`、`state=SESSION_STATE_ONLINE`、`session_id` 等于日志里的 session(日志的 gate_instance 非空时还要与 `gate_instance_id` 相同)才继续。已经是 DISCONNECTING、已不存在、或 session_id 变了,说明玩家已重连 / 重登或已被清理,什么都不做。
4. 补投断线(与 login 原本要发的那一次完全相同;会话在这之间被改写时 CAS 自动失败,不会踢掉活人):
   ```bash
   grpcurl -plaintext -import-path . -proto proto/player_locator/player_locator.proto \
     -d '{"player_id": <player_id>, "session_id": <session_id>, "gate_instance_id": "<GetSession 回的 gate_instance_id>"}' \
     127.0.0.1:50100 playerlocator.PlayerLocator/SetDisconnecting
   ```
5. 再 GetSession 一次应为 `SESSION_STATE_DISCONNECTING`;断线租约(缺省 30s)到期后 LeaseMonitor 发踢人与 LeaveScene,随后 `found=false`。
- 文档债(登记,不在本次改):`go/login/internal/logic/clientplayerlogin/session_lifecycle.go` 的注释与 CRITICAL 日志文案仍写「#B-1 SCAN 巡检 / sweep」,与 `disconnectlogic.go` 注释里过时的「Redis key TTL expires」并列,随 login 下一次改动更正。

#### F.B-2: docker kafka 单 broker 偶发 Exited (255)

- **状态:** 仅 dev,生产 3+ broker 不会触发
- **dev 应对:** `docker start kafka && dev.bat stop && dev.bat start`(必须重启 cpp gate 让 rdkafka 干净 bootstrap)
- **生产应对:** 依赖 kafka 集群高可用 + ISR 监控

#### F.B-3: 1k+ 阶梯压测在 Windows dev 跑不动

- **状态:** 仅 dev,生产 Linux + 多 broker kafka 不会触发
- **应对:** **1k/2k/5k 必须在 Linux staging 跑**,直接用 `tools/scripts/stress-linux-tier.sh` 跑出 csv,把结果追加到 [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md)
  ```bash
  # staging 上(redis-cli 在 PATH,gateway/login/gate 已起):
  ./tools/scripts/stress-linux-tier.sh                  # 默认 1000/2000/5000
  ./tools/scripts/stress-linux-tier.sh 100 500 1000     # 自定义阶梯
  ```
- **当前基线:** Windows dev 单实例 50/100/200/500 全 0 失败,延迟 69-101ms

---

### G. 上线后 1 周观察项

- [ ] `legacy_login_count / new_login_count` 比例曲线(T+1 退出条件)
- [ ] refresh token rotation 成功率(应 > 99.9%)
- [ ] Bucket4j 限流误伤率(用户上报 + log 误差 < 0.01%)
- [ ] cpp gate→login gRPC channel 复用(`netstat -tan | grep 51000` 应只有少数 ESTAB,不上千)
- [ ] WeChat / QQ provider 端到端登录漏斗(若灰度中开启了第三方登录)

---

### 关联文档
- [ARCH.md](../design/ARCH.md) §11-12 决策表 + deprecation 计划
- [onboarding.md](../design/onboarding.md) 5 分钟上手
- [open-server-rate-limit-design.md](../design/open-server-rate-limit-design.md) 限流配置参考
- [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md) 压测基线
- [gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md) sysctl 完整清单
