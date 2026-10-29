# third_party 子模块补丁

## 为什么需要这个目录

`third_party/` 下的子模块指向**上游仓库**（如 `confluentinc/librdkafka`）。
主仓只存一个 commit 指针，**存不下对子模块内容的改动**。

于是本地为了编译而做的修改（给 MSVC 加 `/utf-8` 等）只能躺在子模块的工作树里，
处于「未提交」状态。任何人执行下面任一操作，这些修改**全部消失**：

- `git submodule update`（会强制检出到钉住的 commit）
- 重新 clone 仓库
- 换一台机器

而且丢失之后的表现是**编译错误**，不是静默问题 —— 但排查的人不会想到是子模块被重置了。

补丁化就是把这些改动变成主仓里可版本化、可审阅的文件，`submodule update` 冲不掉。

## 目录内容

| 补丁 | 目标子模块 | 上游 | 内容 |
|---|---|---|---|
| `tools/patches/boringssl/windows-x509-name.patch` | `third_party/boringssl` | `google/boringssl` | Windows 下在声明 `X509_NAME` 类型前清除同名宏，避免系统头文件冲突；应用入口复用此唯一来源 |
| `librdkafka-runtime-compat.patch` | `third_party/librdkafka` | `confluentinc/librdkafka` | 修复消费者重新入组时可变参数使用、BoringSSL 证书与随机数兼容、测试参数字符串所有权 |
| `librdkafka-utf8.patch` | `third_party/librdkafka` | `confluentinc/librdkafka`（**上游，无法推送**） | 9 个 `win32/*.vcxproj` 加 `/utf-8` 编译选项 + BOM |
| `redis-windows-hiredis-test.patch` | `third_party/redis` | `redis/redis` | hiredis 测试按 Windows 错误码获取本地化域名解析错误文本 |
| `sol2-optional-reference.patch` | `third_party/sol2` | `ThePhD/sol2` | 引用 optional 的 `emplace` 保存左值地址并禁止右值，避免临时对象悬垂 |
| `ue5navmesh-utf8.patch` | `third_party/ue5navmesh` | `lhl-cpp/ue5navmesh`（自有 fork） | `Navmesh.vcxproj` 加 `/utf-8`；`DetourNavMeshBuilder.cpp` 补 `#include <cassert>` |

`/utf-8` 是必需的：这两个工程里有中文注释，MSVC 默认按本地代码页解析会报错或乱码。

补丁基于以下钉住的子模块提交；升级依赖后须重新检查能否应用：

- 独立 BoringSSL：`2b44a3701a4788e1ef866ddc7f143060a3d196c9`，补丁相对该子模块根目录应用，不依赖官方 gRPC 仓库。
- `librdkafka`：`c6c9973cf1b25394c58e0cade13a36f386856f77`；运行时补丁只包含四个 C 源码与测试文件，与原编码补丁互不重叠。
- `redis`：`498ecd0d6d007db11ddb3aea9428552598a78622`。
- `sol2`：`eba86625b707e3c8c99bbfc4624e51f42dc9e561`。
- `ue5navmesh`：`aacf2f82af2e24c56474855fce057018928735bb`。

这些补丁保存源码改动，不包含 `install_vs2026_dbg/` 等本机构建安装产物；不需要修改子模块提交指针或向第三方上游推送。

## 用法

```powershell
# 应用全部补丁（幂等：已应用的会跳过）
pwsh -File third_party/patches/apply.ps1

# 只看会做什么，不改文件
pwsh -File third_party/patches/apply.ps1 -DryRun
```

**什么时候需要跑**：clone 之后、`git submodule update` 之后、换机器之后，
以及任何一次 C++ 编译报「无法将字符转换」「非法字符」之类的错误时。

## 维护

改了子模块内容之后重新导出补丁：

```powershell
git -C third_party/librdkafka diff --binary --no-ext-diff --output=../patches/librdkafka-runtime-compat.patch -- src/rdkafka_cgrp.c src/rdkafka_ssl.c src/rdrand.c tests/test.c
git -C third_party/librdkafka apply --reverse --check ../patches/librdkafka-runtime-compat.patch
```

使用 Git 的 `--output` 直接保存补丁字节，避免 PowerShell 管道或重定向改变编码；保留 `.gitattributes` 的 `*.patch -text` 规则。新增本目录补丁按 `<子模块名>-<用途>.patch` 命名，脚本会自动发现。BoringSSL 补丁维护在 `tools/patches/boringssl/`，同时更新固定清单中的摘要，不重复导出到本目录。

`ue5navmesh` 是自有 fork，长期正解是**在 fork 里提交并推送，再回主仓 bump 指针**，
那样就不需要补丁了。在此之前先用补丁兜着。

`librdkafka` 属上游，除非自己 fork 一份并改 `.gitmodules` 指过去，否则只能靠补丁。
