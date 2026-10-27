"""用已核对的本机 SDK 增量构建无优化 C++ 调试镜像；不更新运行容器。"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys

from docker_cpp_sdk_guard import guard_files, dependency_files, physical_files


REPO = Path(__file__).resolve().parents[2]
DOCKER = ["docker", "--context", "desktop-linux"]
NODES = ("gate", "scene", "battle")
RECIPE = REPO / "deploy/debug/Dockerfile.cpp"
TOOLCHAIN = REPO / "build/docker-debug-current/cpp-toolchain.json"


def native_path(path: Path) -> Path:
    # 封存目录比原仓库更深；仅修正 Python I/O 表示，不改 Windows 系统长路径策略。
    return Path("\\\\?\\" + str(path.absolute())) if os.name == "nt" else path


def sha(path: Path) -> str:
    value = hashlib.sha256()
    with native_path(path).open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def capture(args: list[str], cwd: Path = REPO) -> str:
    result = subprocess.run(args, cwd=cwd, capture_output=True, text=True,
                            encoding="utf-8", errors="replace", timeout=180)
    if result.returncode:
        raise RuntimeError("只读命令失败，退出码 " + str(result.returncode))
    return result.stdout.strip("\r\n")


def image_id(reference: str) -> str:
    result = capture(DOCKER + ["image", "inspect", "--format", "{{.Id}}", reference])
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", result):
        raise RuntimeError("本机镜像 ID 不合法")
    return result


def pin(reference: str, repository: str) -> tuple[str, str]:
    identity = image_id(reference)
    name = repository + ":pin-" + identity[7:]
    subprocess.run(DOCKER + ["image", "tag", identity, name], cwd=REPO, check=True)
    if image_id(name) != identity:
        raise RuntimeError("固定镜像引用失败")
    return name, identity


def validate_sdk() -> dict:
    if not TOOLCHAIN.is_file():
        raise RuntimeError("缺少本机 cpp-toolchain.json；须先准备与当前依赖匹配的 Linux SDK，不能借用旧 SDK")
    config = json.loads(TOOLCHAIN.read_text("utf-8-sig"))
    files = config.get("guardFiles", {})
    if config.get("schema") != 2 or not files or image_id(config["image"]) != config["imageId"]:
        raise RuntimeError("SDK 来源配置或镜像身份不匹配")
    if not config.get("guardRoots") or not config.get("guardPolicy") or not config.get("dependencySource"):
        raise RuntimeError("SDK 配置缺少完整的源码集合守卫；不能仅凭历史路径或 Git HEAD 放行")
    actual = {name: sha(path) for name, path in
              guard_files(REPO, config["guardRoots"], config["guardPolicy"]).items()}
    for recipe in ("tools/scripts/third_party/build_linux_sdk.sh", "tools/archived/setup_dependencies.sh",
                   "deploy/k8s/Dockerfile.cpp"):
        if recipe not in files or recipe not in actual:
            raise RuntimeError("SDK 来源守卫缺少现役构建入口：" + recipe)
    differences = sorted(name for name in files.keys() | actual.keys() if files.get(name) != actual.get(name))
    dependency_expected = config["dependencySource"]["files"]
    dependency_actual = dependency_files(REPO, config["dependencySource"])
    differences.extend(sorted(name for name in dependency_expected.keys() | dependency_actual.keys()
                              if dependency_expected.get(name) != dependency_actual.get(name)))
    if differences:
        raise RuntimeError("SDK 依赖输入已变化，须先更新 SDK；首项：" + differences[0])
    return config


def inputs() -> dict[str, Path]:
    # Dockerfile 的 COPY 是输入范围的唯一来源，避免维护第二套目录清单。
    paths = []
    for line in RECIPE.read_text("utf-8").splitlines():
        if line.startswith("COPY ") and not line.startswith("COPY --from="):
            words = shlex.split(line)
            paths.extend(words[1:-1])
    result = {}
    for name in paths:
        source = REPO / name
        if not source.resolve().is_relative_to(REPO) or not source.exists():
            raise RuntimeError("构建输入缺失或越界：" + name)
        if source.is_file():
            result[source.relative_to(REPO).as_posix()] = source
            continue
        if name.rstrip("/") == "third_party/boost/boost":
            def boost_excluded(relative: str, directory: bool) -> bool:
                parts = Path(relative).parts
                return (any(p == ".git" or p.startswith((".build", ".tmp.")) or
                            p in ("x64", "CMakeFiles", "__pycache__") for p in parts) or
                        (not directory and Path(relative).suffix.lower() in
                         (".obj", ".pdb", ".exe", ".dll", ".metaproj", ".pyc")))
            result.update(physical_files(REPO, name.rstrip("/"), boost_excluded, allow_links=True))
            continue
        git_root = Path(capture(["git", "-C", str(source), "rev-parse", "--show-toplevel"]))
        relative_root = git_root.resolve().relative_to(REPO).as_posix()
        prefix = "" if relative_root == "." else relative_root + "/"
        names = capture(["git", "-C", str(source), "ls-files", "-z", "--full-name",
                         "--cached", "--others", "--exclude-standard", "--", "."]).split("\0")
        for tracked in set(names) - {""}:
            relative = prefix + tracked
            parts = Path(relative).parts
            if relative.startswith("third_party/fmt/support/.gradle/"):
                continue
            # 测试不参与本次节点构建；CMakeLists 由正式生成器生成，不属于不可变源码。
            if relative.startswith("cpp/tests/") or relative.endswith("/CMakeLists.txt"):
                continue
            if any(part == ".git" or part.startswith((".build", ".tmp.")) or part in ("x64", "CMakeFiles", "__pycache__") for part in parts):
                continue
            path = REPO / relative
            if path.suffix.lower() in (".obj", ".pdb", ".exe", ".dll", ".metaproj", ".pyc"):
                continue
            if path.is_symlink() or not path.resolve().is_relative_to(REPO):
                raise RuntimeError("构建源码不能包含符号链接或越界路径：" + relative)
            if path.is_file():
                result[relative] = path
    if not any(name.endswith(".cpp") for name in result):
        raise RuntimeError("构建输入没有 C++ 源码")
    return dict(sorted(result.items()))


def assert_sources(expected: dict[str, str]) -> None:
    actual = {name: sha(path) for name, path in inputs().items()}
    if actual != expected:
        raise RuntimeError("构建期间本地输入发生变化，镜像未交付；请保存代码后重新构建")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--base-images", type=Path, required=True)
    parser.add_argument("--services", default=",".join(NODES))
    parser.add_argument("--jobs", type=int, choices=range(1, 5), default=1)
    args = parser.parse_args()
    if set(args.services.split(",")) != set(NODES):
        parser.error("三个 C++ 节点必须一起构建")
    output = args.output.resolve()
    if not output.is_relative_to(REPO / "build"):
        parser.error("输出目录须在本仓库 build 下")
    output.mkdir(parents=True, exist_ok=False)
    result = {"success": False, "images": {}, "manifestPaths": {}}
    try:
        sdk = validate_sdk()
        bases = json.loads(args.base_images.read_text("utf-8-sig"))
        if any(node not in bases for node in NODES):
            raise RuntimeError("缺少节点基础镜像")
        base, base_id = pin(bases["scene"], "local/xuanming-cpp-debug-system")
        context = output / "context"
        context.mkdir()
        files = inputs()
        hashes = {}
        for name, path in files.items():
            expected = sha(path)
            target = context / name
            native_path(target.parent).mkdir(parents=True, exist_ok=True)
            shutil.copy2(native_path(path), native_path(target))
            if sha(path) != expected or sha(target) != expected:
                raise RuntimeError("源码封存期间变化：" + name)
            hashes[name] = expected
        assert_sources(hashes)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
        source_sha = hashlib.sha256(json.dumps(hashes, sort_keys=True).encode()).hexdigest()
        commit = capture(["git", "rev-parse", "--short=12", "HEAD"]) + "-dirty.source-" + source_sha[:12]
        meta = output / "source_meta"
        meta.mkdir()
        manifest = {"schema": 1, "language": "cpp", "sourceRoot": "/src", "files": hashes,
                    "artifacts": {}, "build": {"type": "Debug", "sourceSha256": source_sha,
                    "sdkImageId": sdk["imageId"], "commit": commit, "createdUtc": stamp}}
        manifest_path = meta / "source-manifest.json"
        manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
        tag = "local/xuanming-node:debug-" + stamp.lower()
        iid = output / "image.iid"
        command = DOCKER + ["build", "--pull=false", "--network=none", "--progress=plain",
            "--build-arg", "CPP_DEPS_IMAGE=" + sdk["image"],
            "--build-arg", "CPP_DEBUG_BASE_IMAGE=" + base,
            "--build-arg", "CPP_CACHE_KEY=" + sdk["imageId"][7:],
            "--build-arg", "BUILD_JOBS=" + str(args.jobs),
            "--build-arg", "BUILD_VERSION=debug-" + stamp,
            "--build-arg", "BUILD_COMMIT=" + commit,
            "--build-arg", "BUILD_TIME=" + stamp,
            "--build-context", "source_meta=" + str(meta), "--iidfile", str(iid),
            "-f", str(RECIPE), "-t", tag, str(context)]
        print(f"C++ Debug：{len(hashes)} 份输入，jobs={args.jobs}；日志 {output / 'build.log'}", flush=True)
        with (output / "build.log").open("x", encoding="utf-8") as log:
            process = subprocess.run(command, cwd=REPO, stdout=log, stderr=subprocess.STDOUT, timeout=14400)
        if process.returncode:
            raise RuntimeError("C++ 编译失败，退出码 " + str(process.returncode))
        identity = iid.read_text("utf-8").strip()
        if image_id(tag) != identity or image_id(base) != base_id:
            raise RuntimeError("构建前后镜像引用发生变化")
        validate_sdk()
        assert_sources(hashes)
        for node in NODES:
            result["images"][node] = {"image": tag, "imageId": identity}
            result["manifestPaths"][node] = str(manifest_path)
        result["success"] = True
        print("三个 C++ 节点的同批 Debug 镜像构建完成。", flush=True)
        return 0
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
        (output / "failure.txt").write_text(str(error), encoding="utf-8")
        print("C++ 构建停止：" + str(error), file=sys.stderr)
        return 1
    finally:
        (output / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())
