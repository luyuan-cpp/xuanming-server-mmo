# third_party 子模块补丁

## 为什么需要这个目录

`third_party/` 下的子模块指向**上游仓库**（如 `confluentinc/librdkafka`）。主仓只存 commit 指针，**存不下对子模块内容的改动**。

本地为了编译与兼容而做的修改（例如给 MSVC `.vcxproj` 加 `/utf-8`、BoringSSL 适配等）如果只留在子模块工作树里，一旦执行 `git submodule update`、重新 clone 或换机就会全部丢失。补丁化把这些改动变成主仓里可版本化、可校验 SHA-256、可审阅的文件。

本目录是全仓**唯一**的第三方子模块补丁目录（原重复的 `third_party/patches/` 已收口并移除）；有效补丁清单与摘要以 [third_party/releases.lock.json](../../third_party/releases.lock.json) 为唯一事实源。

## 当前登记的有效补丁

以下 8 个补丁均登记在 `third_party/releases.lock.json` 中，按子模块固定提交校验并幂等应用：

| 补丁（相对 `tools/patches/`） | 目标子模块 | 上游 | 作用 |
|---|---|---|---|
| `boringssl/windows-x509-name.patch` | `third_party/boringssl` | `google/boringssl` | Windows 下声明 `X509_NAME` 前清除同名宏，避免系统头文件冲突 |
| `librdkafka/v2.16.0-local-windows.patch` | `third_party/librdkafka` | `confluentinc/librdkafka` | 9 个 `win32/*.vcxproj` 加 `/utf-8` 与 BOM（含中文注释，防 MSVC 代码页报错） |
| `librdkafka/v2.16.0-cgrp-rejoin-varargs.patch` | `third_party/librdkafka` | `confluentinc/librdkafka` | 修复消费者重新入组时的可变参数使用（Linux Docker 与 Windows 共用） |
| `librdkafka/v2.16.0-test-selector-ownership.patch` | `third_party/librdkafka` | `confluentinc/librdkafka` | 修复测试参数字符串所有权 |
| `librdkafka/v2.16.0-boringssl-compat.patch` | `third_party/librdkafka` | `confluentinc/librdkafka` | 适配 BoringSSL 随机数、HMAC 与 PKCS#12 加载（Linux Docker 与 Windows 共用） |
| `redis/8.10.2-hiredis-windows-dns-test.patch` | `third_party/redis` | `redis/redis` | hiredis 测试按 Windows 错误码获取本地化域名解析错误文本 |
| `sol2/v3.3.0-optional-reference-emplace.patch` | `third_party/sol2` | `ThePhD/sol2` | 引用 `optional::emplace` 保存左值地址并禁止右值，避免临时对象悬垂 |
| `ue5navmesh/current-local-windows.patch` | `third_party/ue5navmesh` | `lhl-cpp/ue5navmesh`（自有 fork） | `Navmesh.vcxproj` 加 `/utf-8`；`DetourNavMeshBuilder.cpp` 补 `#include <cassert>` |

> 目录中其余 `local-worktree-20260914.*`、`LOCAL_WORKTREE_20260914.md`、`local-worktree-20260929.json`、`boost/restore_local_source.py`、`grpc/` 为早期工作区快照存档，不参与当前构建。

## 校验与应用

统一使用版本锁校验器（幂等：已应用的补丁会自动跳过，同时校验子模块提交与补丁 SHA-256）：

```powershell
# Windows：校验固定版本并幂等应用全部登记补丁
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-lock-check -ApplyPatches

# Windows：只读检查（不修改工作树）
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-lock-check
```

```bash
# Linux / 跨平台：校验并应用全部或指定子模块补丁
python3 tools/scripts/third_party/verify_release_lock.py --apply-patches
python3 tools/scripts/third_party/verify_release_lock.py --path third_party/librdkafka --apply-patches
```

**什么时候需要跑**：首次 clone 后、`git submodule update` 后、换机器后，或出现 MSVC `/utf-8` / 兼容编译报错时。

## 维护与更新补丁

修改子模块源码并重新导出补丁时，使用 `git --output` 直接写出原始字节（避免 PowerShell 管道改写编码或行尾），并保留仓库根目录 `.gitattributes` 中的 `*.patch -text` 规则：

```powershell
git -C third_party/<submodule> diff --binary --no-ext-diff --output=../../tools/patches/<submodule>/<patch-name>.patch -- <files...>
git -C third_party/<submodule> apply --reverse --check ../../tools/patches/<submodule>/<patch-name>.patch
```

导出后同步更新 [third_party/releases.lock.json](../../third_party/releases.lock.json) 中对应补丁的 `path` 与 `sha256`，并执行只读校验确认通过。
