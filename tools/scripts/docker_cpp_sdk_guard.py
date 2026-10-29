"""枚举明确的 SDK 源码边界；只读文件、Git 索引与字节，不构建依赖。"""

from __future__ import annotations

import hashlib
import os
from pathlib import Path, PurePosixPath
import stat
import subprocess


def sha(path: Path, *, link_contents: bool = False) -> str:
    if link_contents and path.is_symlink():
        return hashlib.sha256(os.readlink(path).encode("utf-8")).hexdigest()
    # 依赖源码可含超过传统 MAX_PATH 的路径；不修改系统长路径设置。
    native = Path("\\\\?\\" + str(path.absolute())) if os.name == "nt" else path
    value = hashlib.sha256()
    with native.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def checked(root: Path, relative: str) -> Path:
    name = PurePosixPath(relative)
    if not relative or name.is_absolute() or ".." in name.parts or "\\" in relative:
        raise RuntimeError("SDK 源码路径非法：" + relative)
    path = root.joinpath(*name.parts)
    if not path.resolve().is_relative_to(root):
        raise RuntimeError("SDK 源码路径越界：" + relative)
    return path


def physical_files(root: Path, prefix: str, excluded, *, allow_links: bool = False) -> dict[str, Path]:
    """聚合 Boost 头允许仓库内链接；按祖先真实路径检测环，保留逻辑 include 路径。"""
    result = {}

    def visit(path: Path, ancestors: frozenset[Path]) -> None:
        relative = path.relative_to(root).as_posix()
        if excluded(relative, path.is_dir()):
            return
        actual = path.resolve(strict=True)
        if not actual.is_relative_to(root):
            raise RuntimeError("SDK 源码链接越界：" + relative)
        info = path.lstat()
        linked = stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & 0x400
        if linked and not allow_links:
            raise RuntimeError("SDK 源码含未许可链接：" + relative)
        if actual.is_dir():
            if actual in ancestors:
                raise RuntimeError("SDK 源码链接成环：" + relative)
            for child in sorted(path.iterdir(), key=lambda item: item.name):
                visit(child, ancestors | {actual})
        elif actual.is_file():
            result[relative] = path
        else:
            raise RuntimeError("SDK 源码不是普通文件：" + relative)

    top = checked(root, prefix)
    if not top.exists():
        raise RuntimeError("SDK 源码输入缺失：" + prefix)
    visit(top, frozenset())
    return result


def guard_files(root: Path, roots: list[str], policy: dict) -> dict[str, Path]:
    """沿用冻结 SDK 审计的明确目录、后缀和生成 CMake 排除规则。"""
    directories = set(policy["ignored_directories"])
    suffixes = set(policy["ignored_suffixes"])
    stamps = set(policy["ignored_build_check_stamps"])
    cmakes = set(policy["generated_cmake_exclusions"])

    def excluded(relative: str, directory: bool) -> bool:
        parts = PurePosixPath(relative).parts
        if any(relative == prefix or relative.startswith(prefix + "/")
               for prefix in policy.get("ignored_path_prefixes", [])):
            return True
        if relative == "third_party/muduo-linux/muduo/contrib" or any(p in directories for p in parts):
            return True
        if directory:
            return False
        name = parts[-1]
        return (name == ".git" or name in stamps or relative in cmakes or
                PurePosixPath(name).suffix.lower() in suffixes or name.endswith(".so") or ".so." in name)

    result = {}
    for prefix in roots:
        result.update(physical_files(root, prefix, excluded,
                      allow_links=prefix == "third_party/boost/boost"))
    return dict(sorted(result.items()))


def git(root: Path, *arguments: str) -> bytes:
    result = subprocess.run(["git", "-C", str(root), *arguments], capture_output=True, timeout=180)
    if result.returncode:
        raise RuntimeError("SDK 源码 Git 只读检查失败：" + root.name)
    return result.stdout


def dependency_files(root: Path, inventory: dict) -> dict[str, str]:
    """复核独立 SDK 仓库；索引外的新源码也进入集合，忽略构建输出。"""
    result = {}
    policy = inventory["untrackedPolicy"]
    expected_roots = {"third_party/protobuf", "third_party/abseil-cpp", "third_party/boringssl"}
    if {repo["path"] for repo in inventory["repositories"]} != expected_roots:
        raise RuntimeError("SDK 来源必须完整覆盖独立 Protobuf、Abseil 和 BoringSSL")
    if len(inventory["repositories"]) != len(expected_roots):
        raise RuntimeError("SDK 来源仓库重复")
    for repository in inventory["repositories"]:
        prefix = repository["path"]
        source = checked(root, prefix)
        if git(source, "rev-parse", "HEAD").decode().strip() != repository["head"]:
            raise RuntimeError("SDK 独立仓库提交已变化：" + prefix)
        entries, links = {}, {}
        for record in git(source, "ls-files", "--stage", "-z").split(b"\0"):
            if not record:
                continue
            spec, name = record.split(b"\t", 1)
            mode, blob, stage = spec.decode("ascii").split()
            if stage != "0":
                raise RuntimeError("SDK 索引存在未合并项")
            relative = name.decode("utf-8")
            if mode == "160000":
                links[relative] = blob
            else:
                entries[relative] = mode
        if links != repository["gitlinks"]:
            raise RuntimeError("SDK 子模块清单已变化：" + prefix)
        for value in git(source, "ls-files", "--others", "--exclude-standard", "-z").split(b"\0"):
            if value:
                relative = value.decode("utf-8")
                parts = PurePosixPath(relative).parts
                # Windows 安装树未必在上游 .gitignore 中；只过滤非跟踪构建产物。
                if (any(p in policy["ignored_directories"] or p.startswith((".build", ".tmp.", "install_")) for p in parts)
                        or PurePosixPath(relative).suffix.lower() in policy["ignored_suffixes"]
                        or relative.endswith(".so") or ".so." in parts[-1]):
                    continue
                entries[relative] = "untracked"
        for relative, mode in entries.items():
            logical = prefix + "/" + relative
            path = checked(root, logical)
            if not path.is_file():
                raise RuntimeError("SDK 源码文件缺失：" + logical)
            result[logical] = sha(path, link_contents=mode == "120000")
    return dict(sorted(result.items()))
