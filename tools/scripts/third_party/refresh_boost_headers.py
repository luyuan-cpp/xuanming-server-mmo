#!/usr/bin/env python3
"""按 Boost 子模块的直属 include/boost 复制聚合头，不创建符号链接或目录联接。

示例：
    python tools/scripts/third_party/refresh_boost_headers.py \
        --archive build/verification/boost-copy-refresh

Archive 必须是仓库内、Boost 源码树外的全新目录。复制和校验完成后才归档旧聚合树，
最后用同盘重命名发布新树；任何失败均保留现场，不删除文件，不修改模块源码。
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import sys
from typing import Any


EXPECTED_BOOST_VERSION = 109200
COPY_CHUNK_SIZE = 1024 * 1024


def absolute_path(path: Path) -> Path:
    # 保留词法路径，不能先 resolve() 把目录联接隐藏掉再检查。
    return Path(os.path.abspath(path))


def reject_reparse(path: Path) -> os.stat_result:
    info = path.lstat()
    reparse_flag = getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0x400)
    if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & reparse_flag:
        raise RuntimeError(f"路径不能是符号链接或重解析点：{path}")
    return info


def workspace_path(workspace: Path, path: Path, *, check_leaf: bool = True) -> Path:
    result = absolute_path(path)
    try:
        relative = result.relative_to(workspace)
    except ValueError as error:
        raise RuntimeError(f"路径超出当前工作区：{result}") from error
    if not relative.parts:
        raise RuntimeError(f"操作目标不能等于工作区根目录：{result}")
    # Windows 的 junction 可能出现在任一父目录；逐层 lstat，不追随链接。
    current = result if check_leaf else result.parent
    while True:
        if os.path.lexists(current):
            reject_reparse(current)
        if current == workspace:
            break
        current = current.parent
    return result


def git_bytes(root: Path, *arguments: str) -> bytes:
    environment = os.environ.copy()
    environment["GIT_OPTIONAL_LOCKS"] = "0"
    process = subprocess.run(
        ["git", "-C", str(root), *arguments],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=environment,
        check=False,
    )
    if process.returncode:
        detail = process.stderr.decode("utf-8", errors="replace").strip()
        raise RuntimeError(f"Git 只读核查失败（{root}）：{detail}")
    return process.stdout


def git_root_head(root: Path) -> str:
    actual_root = absolute_path(Path(git_bytes(root, "rev-parse", "--show-toplevel").decode().strip()))
    if actual_root != root:
        raise RuntimeError(f"模块并非独立 Git 工作树：期望 {root}，实际 {actual_root}")
    head = git_bytes(root, "rev-parse", "HEAD").decode("ascii").strip()
    if not re.fullmatch(r"[0-9a-f]{40,64}", head):
        raise RuntimeError(f"模块 HEAD 格式不合法：{root}")
    return head


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(COPY_CHUNK_SIZE):
            digest.update(chunk)
    return digest.hexdigest()


def safe_module_path(raw_path: str) -> PurePosixPath:
    path = PurePosixPath(raw_path)
    if (
        not raw_path
        or path.is_absolute()
        or "\\" in raw_path
        or ":" in raw_path
        or any(part in (".", "..") for part in path.parts)
        or raw_path != path.as_posix()
    ):
        raise RuntimeError(f".gitmodules 中的模块路径不合法：{raw_path!r}")
    return path


def inspect_modules(workspace: Path, boost_root: Path) -> tuple[str, str, list[dict[str, Any]]]:
    boost_head = git_root_head(boost_root)
    gitmodules = workspace_path(workspace, boost_root / ".gitmodules")
    declared_paths: list[str] = []
    # Git 自己解析配置语法；只使用 .gitmodules 声明的 path，不扫描 tests 等目录。
    records = git_bytes(
        boost_root, "config", "--null", "--file", str(gitmodules),
        "--get-regexp", r"^submodule\..*\.path$",
    )
    for record in records.split(b"\0"):
        if not record:
            continue
        key, separator, raw_path = record.partition(b"\n")
        if not separator or not key.endswith(b".path"):
            raise RuntimeError("无法解析 .gitmodules 的模块路径输出。")
        declared_paths.append(safe_module_path(raw_path.decode("utf-8")).as_posix())
    if not declared_paths or len(declared_paths) != len(set(declared_paths)):
        raise RuntimeError("Boost 模块列表为空或存在重复路径。")

    gitlinks: dict[str, str] = {}
    for record in git_bytes(boost_root, "ls-tree", "-r", "-z", "HEAD").split(b"\0"):
        if not record:
            continue
        metadata, _, raw_path = record.partition(b"\t")
        mode, object_type, commit = metadata.split()
        if mode == b"160000" and object_type == b"commit":
            gitlinks[raw_path.decode("utf-8")] = commit.decode("ascii")
    if set(declared_paths) != set(gitlinks):
        raise RuntimeError(
            ".gitmodules 与 Boost HEAD 中的 gitlink 集合不一致："
            f"仅配置={sorted(set(declared_paths) - set(gitlinks))}；"
            f"仅 gitlink={sorted(set(gitlinks) - set(declared_paths))}"
        )

    modules: list[dict[str, Any]] = []
    for module_path in sorted(declared_paths):
        module_root = workspace_path(workspace, boost_root.joinpath(*PurePosixPath(module_path).parts))
        if not module_root.is_dir():
            raise RuntimeError(f"模块未初始化或目录缺失：{module_root}")
        head = git_root_head(module_root)
        if head != gitlinks[module_path]:
            raise RuntimeError(f"模块 HEAD 与父 gitlink 不一致：{module_path}，HEAD={head}，gitlink={gitlinks[module_path]}")
        include_root = workspace_path(workspace, module_root / "include" / "boost")
        has_headers = include_root.exists()
        if has_headers and not include_root.is_dir():
            raise RuntimeError(f"模块头目录不是普通目录：{include_root}")
        modules.append({
            "path": module_path,
            "head": head,
            "gitlink": gitlinks[module_path],
            "include_boost_exists": has_headers,
            "header_file_count": 0,
        })
    return boost_head, sha256_file(gitmodules), modules


def ordinary_files(root: Path):
    # 只递归权威 include/boost；每个源条目都拒绝链接，输出也只会创建普通文件。
    for entry in sorted(root.iterdir(), key=lambda item: item.name):
        info = reject_reparse(entry)
        if stat.S_ISDIR(info.st_mode):
            yield from ordinary_files(entry)
        elif stat.S_ISREG(info.st_mode):
            yield entry
        else:
            raise RuntimeError(f"头目录中出现非普通文件：{entry}")


def identical_files(left: Path, right: Path) -> bool:
    with left.open("rb") as first, right.open("rb") as second:
        while True:
            left_chunk = first.read(COPY_CHUNK_SIZE)
            right_chunk = second.read(COPY_CHUNK_SIZE)
            if left_chunk != right_chunk:
                return False
            if not left_chunk:
                return True


def copy_headers(boost_root: Path, new_tree: Path, modules: list[dict[str, Any]]) -> list[dict[str, Any]]:
    files: dict[str, dict[str, Any]] = {}
    for module in modules:
        if not module["include_boost_exists"]:
            continue
        include_root = boost_root / module["path"] / "include" / "boost"
        for source in ordinary_files(include_root):
            relative = source.relative_to(include_root)
            # 在任何宿主上都按 Windows 的大小写规则发现冲突，避免落盘时隐式覆盖。
            key = relative.as_posix().casefold()
            source_name = f"{module['path']}/include/boost/{relative.as_posix()}"
            module["header_file_count"] += 1
            if key in files:
                previous = files[key]
                destination = new_tree / PurePosixPath(previous["path"]).relative_to("boost")
                if not identical_files(source, destination):
                    raise RuntimeError(f"聚合头发生内容冲突：{source_name} 与 {previous['sources'][0]}")
                previous["sources"].append(source_name)
                continue
            destination = new_tree / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            digest = hashlib.sha256()
            size = 0
            # 独占创建，绝不覆盖已复制的路径。
            with source.open("rb") as incoming, destination.open("xb") as outgoing:
                while chunk := incoming.read(COPY_CHUNK_SIZE):
                    outgoing.write(chunk)
                    digest.update(chunk)
                    size += len(chunk)
            # 保留源码时间戳，重复刷新聚合树不会迫使所有 Boost 调用方重新编译。
            source_info = source.stat()
            os.utime(destination, ns=(source_info.st_atime_ns, source_info.st_mtime_ns))
            files[key] = {
                "path": f"boost/{relative.as_posix()}",
                "sha256": digest.hexdigest(),
                "size": size,
                "sources": [source_name],
            }
    return sorted(files.values(), key=lambda item: item["path"])


def write_json_new(path: Path, value: Any) -> None:
    with path.open("x", encoding="utf-8", newline="\n") as output:
        json.dump(value, output, ensure_ascii=False, indent=2)
        output.write("\n")


def validate_new_tree(new_tree: Path, files: list[dict[str, Any]]) -> None:
    actual_paths = {item.relative_to(new_tree).as_posix() for item in ordinary_files(new_tree)}
    expected_paths = {PurePosixPath(item["path"]).relative_to("boost").as_posix() for item in files}
    if actual_paths != expected_paths:
        raise RuntimeError("临时聚合头文件集合发生变化。")
    for item in files:
        path = new_tree / PurePosixPath(item["path"]).relative_to("boost")
        if path.stat().st_size != item["size"] or sha256_file(path) != item["sha256"]:
            raise RuntimeError(f"临时聚合头 SHA256 复核失败：{path}")
    version_header = new_tree / "version.hpp"
    version_text = version_header.read_text(encoding="utf-8")
    version = re.search(r"^\s*#\s*define\s+BOOST_VERSION\s+(\d+)\s*$", version_text, re.MULTILINE)
    if version is None or int(version.group(1)) != EXPECTED_BOOST_VERSION:
        raise RuntimeError(f"聚合头版本不符，要求 BOOST_VERSION={EXPECTED_BOOST_VERSION}：{version_header}")


def rename_new(workspace: Path, source: Path, destination: Path) -> None:
    # 每次实际移动前重新核对最终绝对路径与父目录，不能依赖较早的预检结果。
    source = workspace_path(workspace, source)
    destination = workspace_path(workspace, destination)
    if not source.is_dir():
        raise RuntimeError(f"重命名源目录不存在：{source}")
    if os.path.lexists(destination):
        raise RuntimeError(f"重命名目标已存在，拒绝覆盖：{destination}")
    if source.stat().st_dev != destination.parent.stat().st_dev:
        raise RuntimeError(f"重命名必须在同一文件系统：{source} -> {destination}")
    os.rename(source, destination)


def refresh(workspace: Path, archive_argument: str) -> dict[str, Any]:
    archive_path = Path(archive_argument)
    archive = workspace_path(workspace, archive_path if archive_path.is_absolute() else workspace / archive_path)
    boost_root = workspace_path(workspace, workspace / "third_party" / "boost")
    try:
        archive.relative_to(boost_root)
    except ValueError:
        pass
    else:
        raise RuntimeError(f"Archive 必须位于 Boost 源码树之外：{archive}")
    if os.path.lexists(archive):
        raise RuntimeError(f"Archive 必须是全新目录，拒绝复用：{archive}")
    target = workspace_path(workspace, boost_root / "boost")
    if os.path.lexists(target) and not target.is_dir():
        raise RuntimeError(f"现有聚合头不是普通目录：{target}")
    boost_head, gitmodules_sha256, modules = inspect_modules(workspace, boost_root)
    archive.mkdir(parents=True, exist_ok=False)
    new_tree = workspace_path(workspace, archive / "new-boost")
    previous_tree = workspace_path(workspace, archive / "previous-boost")
    new_tree.mkdir(exist_ok=False)
    old_tree_archived = False
    try:
        print(f"Copying the top-level include/boost of {len(modules)} verified modules -> {new_tree}", flush=True)
        files = copy_headers(boost_root, new_tree, modules)
        validate_new_tree(new_tree, files)
        if git_root_head(boost_root) != boost_head or sha256_file(boost_root / ".gitmodules") != gitmodules_sha256:
            raise RuntimeError("复制期间 Boost HEAD 或 .gitmodules 发生变化。")
        for module in modules:
            if git_root_head(boost_root / module["path"]) != module["gitlink"]:
                raise RuntimeError(f"复制期间模块 HEAD 发生变化：{module['path']}")
        write_json_new(archive / "modules.json", {
            "boost_head": boost_head,
            "gitmodules_sha256": gitmodules_sha256,
            "modules": modules,
        })
        write_json_new(archive / "files.sha256.json", files)
        write_json_new(archive / "prepared.json", {
            "new_tree": str(new_tree), "target": str(target),
            "previous_tree": str(previous_tree), "file_count": len(files),
            "boost_version": EXPECTED_BOOST_VERSION,
        })
        # 所有复制、版本与提交校验均成功后才移动现有 partial；不删除任何旧树。
        if os.path.lexists(target):
            rename_new(workspace, target, previous_tree)
            old_tree_archived = True
        rename_new(workspace, new_tree, target)
        result = {
            "completed_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "boost_head": boost_head, "boost_version": EXPECTED_BOOST_VERSION,
            "module_count": len(modules),
            "header_module_count": sum(module["include_boost_exists"] for module in modules),
            "file_count": len(files),
            "duplicate_identical_paths": sum(len(item["sources"]) - 1 for item in files),
            "target": str(target),
            "previous_tree": str(previous_tree) if old_tree_archived else None,
            "archive": str(archive),
        }
        write_json_new(archive / "result.json", result)
        return result
    except Exception as error:
        write_json_new(archive / "failure.json", {
            "error": str(error), "new_tree": str(new_tree), "target": str(target),
            "previous_tree": str(previous_tree) if old_tree_archived else None,
        })
        raise


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", required=True, help="仓库内、Boost 树外的全新归档目录")
    arguments = parser.parse_args()
    workspace = absolute_path(Path(__file__).parents[3])
    try:
        result = refresh(workspace, arguments.archive)
    except (OSError, RuntimeError, ValueError) as error:
        print(f"Boost umbrella header refresh failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=False, indent=2), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
