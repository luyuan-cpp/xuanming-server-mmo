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
| `librdkafka-utf8.patch` | `third_party/librdkafka` | `confluentinc/librdkafka`（**上游，无法推送**） | 9 个 `win32/*.vcxproj` 加 `/utf-8` 编译选项 + BOM |
| `ue5navmesh-utf8.patch` | `third_party/ue5navmesh` | `luyuancpp/ue5navmesh`（自有 fork） | `Navmesh.vcxproj` 加 `/utf-8`；`DetourNavMeshBuilder.cpp` 补 `#include <cassert>` |

`/utf-8` 是必需的：这两个工程里有中文注释，MSVC 默认按本地代码页解析会报错或乱码。

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

```bash
cd third_party/librdkafka && git diff > ../patches/librdkafka-utf8.patch
```

`ue5navmesh` 是自有 fork，长期正解是**在 fork 里提交并推送，再回主仓 bump 指针**，
那样就不需要补丁了。在此之前先用补丁兜着。

`librdkafka` 属上游，除非自己 fork 一份并改 `.gitmodules` 指过去，否则只能靠补丁。
