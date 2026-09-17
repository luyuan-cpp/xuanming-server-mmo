# 更新日志

本文件记录 xuanming-server-mmo 服务端各发布版本的变更,格式遵循
[Keep a Changelog 1.1.0](https://keepachangelog.com/zh-CN/1.1.0/),版本号遵循
[语义化版本 2.0.0](https://semver.org/lang/zh-CN/)。

发版约定(`tools/scripts/make_release.ps1` 依赖这些约定,改格式要同步改脚本和它的契约测试):

1. 发版前把 `## [Unreleased]` 改成 `## [X.Y.Z] - YYYY-MM-DD`(方括号里**不带** `v`),
   再在它上方新开一个空的 `## [Unreleased]`。
2. `make_release.ps1 -Version vX.Y.Z` 读取 `## [X.Y.Z]` 段(到下一个 `## ` 标题为止)写进
   release manifest 与 release notes;找不到该段或段落为空都会拒绝发布。
3. 同一个版本号必须四处一致:git tag `vX.Y.Z`、`publish_images.ps1 -Version vX.Y.Z`、
   本文件的 `[X.Y.Z]` 段、镜像自报版本(OCI label `org.opencontainers.image.version` 与 `/app/BUILD_INFO`)。
4. 每条写"改了什么、对部署/运维有什么影响";过程流水账写 `PROGRESS.md`,不写这里。

## [Unreleased]

### 新增

- 发布版本号规范:`vX.Y.Z[-预发布标识]`(必须小写 `v`);发布轨镜像 tag 为 `vX.Y.Z-<12 位 commit>`,
  快照轨仍为 `<12 位 commit>`。
- 版本库外的制品目录(环境变量 `MMORPG_ARTIFACT_ROOT`,默认仓库同级 `artifacts/`):
  snapshot / release 两轨分仓;版本目录不可变,先写 staging 再整目录 rename 上线;
  每个版本目录带 `build-info.json` 与 `sha256sums.txt`。
- `tools/scripts/publish_images.ps1`:调用既有镜像脚本构建,校验镜像 revision label 等于当前 commit,
  逐个 `docker save` 出离线包并核对 RepoTags,可选归档 C++ 分离符号,原子发布到制品目录。
- `tools/scripts/make_release.ps1`:生成 `releases/manifests/<vX.Y.Z>.json` 与 `.md`;修复内容取自本文件对应段落,
  交叉校验镜像 `app_version`,默认拒绝引用脏树制品,可记录推送后的镜像 digest。
- `tools/scripts/fetch_images.ps1` / `import_images.ps1`:复制前后各校验一次 `sha256sums.txt`,导入后逐个核对镜像 ID;
  `tools/scripts/artifacts_retention.ps1`:只清理快照轨,默认 dry-run,release 轨永不触碰。
- 手动触发的 `.github/workflows/release.yml`:在干净检出上构建并产出离线包与 release manifest,
  只上传为 Actions artifact,不推镜像、不打 tag、不部署。
- `tools/scripts/release_preflight.ps1` 增加发布制品检查(版本目录、校验和、`app_version`、脏树、release manifest)。
- Go 服务版本包 `shared/buildinfo`:版本 / commit / 构建时间经 ldflags 注入,启动首行打印
  `service starting ... version=... commit=...`。
- 运行镜像统一构建参数 `BUILD_VERSION` / `BUILD_COMMIT` / `BUILD_TIME`、OCI label 与 `/app/BUILD_INFO`;
  镜像脚本新增 `-Command list-refs`(机器可读镜像清单)、`-Version`、推送后记录 digest 的 `-DigestsOut`。

### 变更

- 镜像脚本未显式传 tag 时,给了发布版本号就生成 `vX.Y.Z-<commit>`,否则仍用 commit;
  发布版本号不接受脏工作树。
- `deploy/k8s/Dockerfile.*` 运行阶段改为非 root 用户 `10001`;外部基础镜像改为 `tag@sha256` 钉死。

### 修复

- Go 服务镜像的版本号实际没有注入:原 ldflags `-X main.buildVersion` 指向不存在的变量,
  改为注入 `shared/buildinfo`。
