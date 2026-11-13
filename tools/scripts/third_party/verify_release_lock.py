"""校验子模块的固定提交，并按需恢复经过登记的本地兼容补丁。"""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[3]


def git(repository, *arguments, check=True):
    result = subprocess.run(
        ["git", "-C", str(repository), *arguments], capture_output=True
    )
    if check and result.returncode:
        raise RuntimeError(result.stderr.decode("utf-8", "replace").strip())
    return result


def verify_repository(relative_path, commit):
    repository = ROOT / relative_path
    actual_root = Path(git(repository, "rev-parse", "--show-toplevel").stdout.decode().strip())
    if actual_root.resolve() != repository.resolve():
        raise RuntimeError(f"子模块未初始化：{relative_path}")
    actual = git(repository, "rev-parse", "HEAD").stdout.decode().strip()
    if actual != commit:
        raise RuntimeError(f"提交不匹配：{relative_path}，期望 {commit}，实际 {actual}")
    return repository


def verify_patch(repository, item, apply_patches):
    patch = ROOT / item["path"]
    if hashlib.sha256(patch.read_bytes()).hexdigest() != item["sha256"]:
        raise RuntimeError(f"补丁内容不匹配：{item['path']}")
    if git(repository, "apply", "--reverse", "--check", str(patch), check=False).returncode == 0:
        return
    if not apply_patches:
        raise RuntimeError(f"补丁尚未应用：{item['path']}，请使用 --apply-patches")
    git(repository, "apply", "--check", str(patch))
    git(repository, "apply", str(patch))
    print(f"Applied: {item['path']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply-patches", action="store_true")
    parser.add_argument("--check-index", action="store_true")
    parser.add_argument("--path", action="append", help="仅校验所选构建目标实际初始化的子模块，可重复指定")
    args = parser.parse_args()
    manifest = json.loads((ROOT / "third_party/releases.lock.json").read_text(encoding="utf-8"))
    entries = manifest["dependencies"]
    paths = {entry["path"] for entry in entries}
    if len(paths) != len(entries):
        raise RuntimeError("固定清单含重复子模块")
    configured = git(ROOT, "config", "-f", ".gitmodules", "--get-regexp", r"^submodule\..*\.path$").stdout.decode()
    if {line.split(None, 1)[1] for line in configured.splitlines()} != paths:
        raise RuntimeError(".gitmodules 与固定清单的路径集合不一致")
    if args.path:
        unknown = set(args.path) - paths
        if unknown:
            raise RuntimeError(f"固定清单没有这些子模块：{sorted(unknown)}")
        entries = [entry for entry in entries if entry["path"] in args.path]
    for entry in entries:
        repository = verify_repository(entry["path"], entry["commit"])
        if args.check_index:
            index_entry = git(ROOT, "ls-files", "--stage", "--", entry["path"]).stdout.decode().split()
            if index_entry[:2] != ["160000", entry["commit"]]:
                raise RuntimeError(f"主仓库暂存的 gitlink 不匹配：{entry['path']}")
        for patch in entry.get("patches", []):
            verify_patch(repository, patch, args.apply_patches)
        for nested in entry.get("nested_patches", []):
            nested_repository = verify_repository(nested["repository"], nested["commit"])
            verify_patch(nested_repository, nested, args.apply_patches)
    print(f"Pinned versions and compatibility patches verified: {len(entries)} submodules")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
