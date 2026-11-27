"""封存当前依赖源码并重建、校验、原子发布本机 Linux C++ 独立 SDK。"""

from __future__ import annotations

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import subprocess
import sys

import build_docker_cpp_debug as build_cpp
from docker_cpp_sdk_guard import dependency_files, git as git_bytes, guard_files

REPO = build_cpp.REPO
DOCKER = build_cpp.DOCKER
TOOLCHAIN = build_cpp.TOOLCHAIN
RECIPE = REPO / "deploy/k8s/Dockerfile.cpp"
VERIFIER = REPO / "tools/scripts/verify_docker_cpp_sdk.sh"
SDK_REPOSITORY = "local/xuanming-independent-sdk"
INDEPENDENT_REPOS = (
    "third_party/protobuf",
    "third_party/abseil-cpp",
    "third_party/boringssl",
)
REQUIRED_RECIPES = (
    "tools/scripts/third_party/build_linux_sdk.sh",
    "tools/archived/setup_dependencies.sh",
    "deploy/k8s/Dockerfile.cpp",
)
PROFILES = ("debug-compile-only", "validated")
DEFAULT_UNTRACKED_POLICY = {
    "ignored_directories": [".git", ".vs", "CMakeFiles", "__pycache__", "build"],
    "ignored_suffixes": [],
}
DEFAULT_GUARD_POLICY = {
    "business_roots": ["cpp", "proto", "generated"],
    "check_stamp_producers": [
        "Directory.Build.targets → tools/scripts/check_no_raw_pointer_member.ps1",
        "tools/scripts/lib/no_raw_pointer_project.ps1",
        "tools/archived/setup_dependencies.sh（.build_linux/.mmorpg-source.stamp）",
    ],
    "docker_context_alignment": "对应本轮封存的 deps COPY 输入；保留第三方跟踪源码，不运行其测试。",
    "expected_transforms": {
        "third_party/librdkafka/src/rdkafka_cgrp.c": "setup 的 CRLF→LF 变换；保留原始宿主哈希供审计",
        "third_party/librdkafka/src/rdkafka_ssl.c": "setup 的 CRLF→LF 变换；保留原始宿主哈希供审计",
        "third_party/librdkafka/src/rdrand.c": "setup 的 CRLF→LF 变换；保留原始宿主哈希供审计",
        "third_party/muduo-linux": "以 tools/archived/muduo_linux_overlay 对应文件作为容器期望字节",
    },
    "generated_cmake_exclusions": [],
    "ignored_build_check_stamps": [],
    "ignored_directories": [],
    "ignored_path_prefixes": [
        "third_party/curl/.git",
        "third_party/librdkafka/.git",
        "third_party/muduo-linux/.git",
        "third_party/nghttp2/.git",
        "third_party/nghttp2/install_vs2026_dbg",
        "third_party/nghttp2/tests/munit",
        "third_party/nghttp2/third-party/mruby",
        "third_party/nghttp2/third-party/neverbleed",
        "third_party/nghttp2/third-party/urlparse",
        "third_party/redis/.git",
        "third_party/redis/utils/install_server.sh",
        "third_party/yaml-cpp/.git",
        "third_party/zlib/.git",
    ],
    "ignored_suffixes": [],
    "limits": [
        "只证明独立 Linux 依赖镜像及临时消费者；不证明应用源码构建或业务测试。",
        "没有消费主仓 cpp、generated、proto；后续业务构建必须独立封存完整一致来源。",
    ],
    "other_exclusions": [
        ".git 文件",
        "*.so 及 *.so.*",
        "muduo-linux/muduo/contrib 构建生成链接",
    ],
}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def write_json(path: Path, value: dict) -> None:
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2, sort_keys=True)
        stream.write("\n")


def write_json_atomic(target: Path, value: dict) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    tmp = target.with_name(f"{target.name}.tmp.{os.getpid()}")
    with tmp.open("w", encoding="utf-8", newline="\n") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(tmp, target)


def parse_deps_recipe(recipe_path: Path) -> tuple[str, list[str]]:
    """解析 Dockerfile.cpp 的 deps 阶段基础镜像与全部 COPY 输入边界。"""
    require(recipe_path.is_file(), f"SDK Dockerfile is missing: {recipe_path}")
    raw = recipe_path.read_text(encoding="utf-8")
    require("FROM deps AS builder" in raw, "SDK Dockerfile is missing 'FROM deps AS builder' boundary")
    deps_section = raw.split("FROM deps AS builder", 1)[0]
    joined = re.sub(r"\\\r?\n", " ", deps_section)
    base_image = None
    copied: set[str] = set()
    for line in joined.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("FROM ") and base_image is None:
            parts = shlex.split(stripped)
            require(len(parts) >= 4 and parts[2].upper() == "AS" and parts[3] == "deps",
                    "Unexpected deps stage FROM line in SDK Dockerfile")
            base_image = parts[1]
        elif stripped.startswith("COPY ") and not stripped.startswith("COPY --from="):
            parts = shlex.split(stripped)
            require(len(parts) >= 3, f"Invalid COPY instruction in SDK Dockerfile: {stripped}")
            for source in parts[1:-1]:
                if not source.startswith("--"):
                    copied.add(source.rstrip("/"))
    require(base_image is not None, "Deps base image not found in SDK Dockerfile")
    missing_independent = [repo for repo in INDEPENDENT_REPOS if repo not in copied]
    require(not missing_independent, "Deps stage is missing independent repositories: " + ", ".join(missing_independent))
    guard_roots = sorted((copied - set(INDEPENDENT_REPOS)) | {"deploy/k8s/Dockerfile.cpp", ".dockerignore"})
    for required in REQUIRED_RECIPES:
        require(required in guard_roots, f"Deps guard boundary is missing required recipe: {required}")
    return base_image, guard_roots


def collect_repositories(repo: Path) -> list[dict]:
    """收集独立依赖仓库的 HEAD 与子模块 gitlink，拒绝未合并索引。"""
    repositories = []
    for relative in INDEPENDENT_REPOS:
        root = repo / relative
        require(root.is_dir(), f"Independent dependency repository is missing: {relative}")
        head = git_bytes(root, "rev-parse", "HEAD").decode("ascii").strip()
        require(re.fullmatch(r"[0-9a-f]{40}", head) is not None, f"Invalid Git HEAD for {relative}")
        gitlinks = {}
        for record in git_bytes(root, "ls-files", "--stage", "-z").split(b"\0"):
            if not record:
                continue
            spec, name = record.split(b"\t", 1)
            mode, blob, stage = spec.decode("ascii").split()
            require(stage == "0", f"Unmerged Git index entry in {relative}")
            if mode == "160000":
                gitlinks[name.decode("utf-8")] = blob
        repositories.append({"path": relative, "head": head, "gitlinks": dict(sorted(gitlinks.items()))})
    return repositories


def load_existing_toolchain(path: Path = TOOLCHAIN) -> tuple[dict | None, str | None]:
    if not path.is_file():
        return None, None
    raw = path.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    try:
        parsed = json.loads(raw.decode("utf-8-sig"))
    except (UnicodeDecodeError, ValueError):
        return None, digest
    return (parsed if isinstance(parsed, dict) else None), digest


def resolve_policies(existing: dict | None, guard_roots: list[str]) -> tuple[dict, dict]:
    guard_policy = copy.deepcopy(existing["guardPolicy"]) if existing and isinstance(existing.get("guardPolicy"), dict) else copy.deepcopy(DEFAULT_GUARD_POLICY)
    guard_policy["guard_roots"] = list(guard_roots)
    untracked_policy = (
        copy.deepcopy(existing["dependencySource"]["untrackedPolicy"])
        if existing
        and isinstance(existing.get("dependencySource"), dict)
        and isinstance(existing["dependencySource"].get("untrackedPolicy"), dict)
        else copy.deepcopy(DEFAULT_UNTRACKED_POLICY)
    )
    return guard_policy, untracked_policy


def collect_current_inputs(
    repo: Path, guard_roots: list[str], guard_policy: dict, untracked_policy: dict
) -> tuple[dict[str, Path], dict[str, str], dict]:
    guard_paths = guard_files(repo, guard_roots, guard_policy)
    dependency_source = {
        "repositories": collect_repositories(repo),
        "untrackedPolicy": untracked_policy,
    }
    dep_hashes = dependency_files(repo, dependency_source)
    overlap = sorted(guard_paths.keys() & dep_hashes.keys())
    require(not overlap, "Guard and independent dependency boundaries overlap: " + overlap[0] if overlap else "")
    return guard_paths, dep_hashes, dependency_source


def compute_drift(existing: dict | None, guard_hashes: dict[str, str], dep_hashes: dict[str, str]) -> list[str]:
    if not existing or existing.get("schema") != 2:
        return ["<missing-or-invalid-cpp-toolchain.json>"]
    recorded_guard = existing.get("guardFiles")
    recorded_dep = (existing.get("dependencySource") or {}).get("files")
    if not isinstance(recorded_guard, dict) or not isinstance(recorded_dep, dict):
        return ["<incomplete-cpp-toolchain.json>"]
    differences = sorted(
        name for name in recorded_guard.keys() | guard_hashes.keys() if recorded_guard.get(name) != guard_hashes.get(name)
    )
    differences.extend(
        sorted(name for name in recorded_dep.keys() | dep_hashes.keys() if recorded_dep.get(name) != dep_hashes.get(name))
    )
    return differences


def stage_context(
    repo: Path, context: Path, guard_paths: dict[str, Path], dep_hashes: dict[str, str]
) -> dict[str, str]:
    """将全部受守卫与独立依赖源码复制到冻结目录，逐文件比对复制前后字节。"""
    guard_hashes: dict[str, str] = {}
    for relative, source in guard_paths.items():
        expected = build_cpp.sha(source)
        target = context / relative
        build_cpp.native_path(target.parent).mkdir(parents=True, exist_ok=True)
        shutil.copy2(build_cpp.native_path(source), build_cpp.native_path(target))
        require(build_cpp.sha(source) == expected and build_cpp.sha(target) == expected,
                f"SDK guard source mutated during snapshot: {relative}")
        guard_hashes[relative] = expected
    for relative, expected in dep_hashes.items():
        source = repo / relative
        require(not source.is_symlink(), f"Unsupported symlink in independent dependency source: {relative}")
        target = context / relative
        build_cpp.native_path(target.parent).mkdir(parents=True, exist_ok=True)
        shutil.copy2(build_cpp.native_path(source), build_cpp.native_path(target))
        require(build_cpp.sha(source) == expected and build_cpp.sha(target) == expected,
                f"Independent dependency source mutated during snapshot: {relative}")
    return dict(sorted(guard_hashes.items()))


def assert_host_unchanged(
    repo: Path,
    guard_roots: list[str],
    guard_policy: dict,
    dependency_source: dict,
    expected_guard: dict[str, str],
    expected_dep: dict[str, str],
) -> None:
    actual_guard = {name: build_cpp.sha(path) for name, path in guard_files(repo, guard_roots, guard_policy).items()}
    actual_dep = dependency_files(repo, dependency_source)
    require(actual_guard == expected_guard and actual_dep == expected_dep,
            "Host SDK inputs changed during build; candidate image was not published")


def build_sdk_image(context: Path, tag: str, iid_file: Path, log_path: Path, profile: str, jobs: int) -> str:
    command = DOCKER + [
        "build",
        "--target",
        "deps",
        "--pull=false",
        "--progress=plain",
        "--build-arg",
        f"BUILD_JOBS={jobs}",
        "--build-arg",
        f"SDK_BUILD_PROFILE={profile}",
        "--iidfile",
        str(iid_file),
        "-f",
        str(context / "deploy/k8s/Dockerfile.cpp"),
        "-t",
        tag,
        str(context),
    ]
    with log_path.open("x", encoding="utf-8") as log:
        process = subprocess.run(command, cwd=REPO, stdout=log, stderr=subprocess.STDOUT, timeout=14400)
    require(process.returncode == 0, f"SDK Docker build failed with exit code {process.returncode}; inspect {log_path}")
    identity = iid_file.read_text(encoding="utf-8").strip()
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", identity) is not None, "Built SDK image ID is invalid")
    require(build_cpp.image_id(tag) == identity, "Built SDK tag does not resolve to recorded image ID")
    return identity


def qualify_sdk_image(identity: str, profile: str, verifier_path: Path, log_path: Path) -> str:
    require(verifier_path.is_file(), f"SDK install verifier script is missing: {verifier_path}")
    script_bytes = verifier_path.read_bytes()
    require(bool(script_bytes) and b"\r" not in script_bytes,
            f"SDK install verifier must be non-empty and use LF line endings: {verifier_path}")
    script_sha = hashlib.sha256(script_bytes).hexdigest()
    command = DOCKER + [
        "run",
        "--rm",
        "--network=none",
        "--read-only",
        "--tmpfs",
        "/tmp:rw,nosuid,nodev",
        "-e",
        f"SDK_BUILD_PROFILE={profile}",
        "--entrypoint",
        "bash",
        "-i",
        identity,
        "-s",
    ]
    result = subprocess.run(
        command,
        cwd=REPO,
        input=script_bytes,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=600,
    )
    with log_path.open("xb") as log:
        log.write(result.stdout)
    require(result.returncode == 0,
            f"SDK install qualification failed with exit code {result.returncode}; inspect {log_path}")
    return script_sha


def remove_tree(path: Path) -> None:
    def onerror(func, target_path, _exc):
        os.chmod(target_path, stat.S_IWRITE | stat.S_IREAD)
        func(target_path)

    if path.exists():
        shutil.rmtree(build_cpp.native_path(path), onerror=onerror)


def build_toolchain_payload(
    *,
    pinned_ref: str,
    identity: str,
    profile: str,
    guard_roots: list[str],
    guard_policy: dict,
    guard_hashes: dict[str, str],
    dependency_source: dict,
    dep_hashes: dict[str, str],
    source_manifest_path: Path,
    evidence_paths: dict[str, Path],
    previous_sha: str | None,
) -> dict:
    dep_record = {
        "files": dep_hashes,
        "repositories": dependency_source["repositories"],
        "sourceManifest": str(source_manifest_path),
        "sourceManifestScope": "本轮 Debug SDK 完整封存输入",
        "sourceManifestSha256": build_cpp.sha(source_manifest_path),
        "untrackedPolicy": dependency_source["untrackedPolicy"],
    }
    evidence = {
        name: {"path": str(path), "sha256": build_cpp.sha(path)}
        for name, path in sorted(evidence_paths.items())
    }
    qualification = {
        "buildProfile": profile,
        "generatedSourcesConsumed": False,
        "sdkOnly": True,
        "thirdPartyTestsRun": profile == "validated",
    }
    if previous_sha:
        qualification["previousToolchainSha256"] = previous_sha
    return {
        "schema": 2,
        "image": pinned_ref,
        "imageId": identity,
        "guardRoots": list(guard_roots),
        "guardPolicy": guard_policy,
        "guardFiles": guard_hashes,
        "dependencySource": dep_record,
        "evidence": evidence,
        "qualification": qualification,
    }


def refresh_sdk(*, profile: str, jobs: int, check_only: bool, force: bool, keep_context: bool) -> int:
    base_image, guard_roots = parse_deps_recipe(RECIPE)
    existing, previous_sha = load_existing_toolchain(TOOLCHAIN)
    guard_policy, untracked_policy = resolve_policies(existing, guard_roots)
    guard_paths, dep_hashes, dependency_source = collect_current_inputs(REPO, guard_roots, guard_policy, untracked_policy)
    guard_hashes_preview = {name: build_cpp.sha(path) for name, path in guard_paths.items()}
    drift = compute_drift(existing, guard_hashes_preview, dep_hashes)

    valid_existing = False
    if not drift and existing is not None:
        try:
            build_cpp.validate_sdk()
            valid_existing = True
        except RuntimeError:
            valid_existing = False

    existing_profile = (existing or {}).get("qualification", {}).get("buildProfile")
    if check_only:
        if valid_existing and existing_profile == profile:
            print(f"SDK is up to date (profile={profile}): {existing['image']}", flush=True)
            return 0
        if drift:
            print(f"SDK refresh required: {len(drift)} differing inputs; first={drift[0]}", flush=True)
            for item in drift[:20]:
                print(f"  - {item}", flush=True)
        elif not valid_existing:
            print("SDK refresh required: local toolchain validation failed (image missing or policy mismatch)", flush=True)
        else:
            print(f"SDK refresh required: profile mismatch (current={existing_profile}, requested={profile})", flush=True)
        return 1

    if valid_existing and existing_profile == profile and not force:
        print(f"SDK is already up to date (profile={profile}): {existing['image']}; use --force to rebuild", flush=True)
        return 0

    require(VERIFIER.is_file(), f"SDK install verifier script is missing: {VERIFIER}")
    base_id = build_cpp.image_id(base_image)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    output = REPO / "build/verification" / f"docker-sdk-refresh-{stamp}"
    output.mkdir(parents=True, exist_ok=False)
    print(f"Evidence directory: {output}", flush=True)

    result = {
        "success": False,
        "phase": "preflight",
        "profile": profile,
        "jobs": jobs,
        "baseImage": base_image,
        "baseImageId": base_id,
        "changedInputsCount": len(drift),
        "changedInputsPreview": drift[:20],
        "published": False,
    }
    try:
        result["phase"] = "prepare"
        context = output / "sdk-context"
        context.mkdir()
        print(f"Staging {len(guard_paths) + len(dep_hashes)} audited SDK input files...", flush=True)
        guard_hashes = stage_context(REPO, context, guard_paths, dep_hashes)
        require(guard_hashes == guard_hashes_preview, "Guard inputs changed before staging completed")

        source_manifest_path = output / "sdk-source.json"
        write_json(
            source_manifest_path,
            {
                "buildProfile": profile,
                "changedGuardPaths": drift,
                "dependencyFiles": dep_hashes,
                "guardFiles": guard_hashes,
                "guardRoots": guard_roots,
                "repositories": dependency_source["repositories"],
            },
        )
        snapshot_result_path = output / "sdk-snapshot-result.json"
        manifest_sha = build_cpp.sha(source_manifest_path)
        write_json(
            snapshot_result_path,
            {"files": len(guard_hashes) + len(dep_hashes), "manifestSha256": manifest_sha, "passed": True},
        )

        result["phase"] = "build"
        prefix = "debug" if profile == "debug-compile-only" else "validated"
        tag = f"{SDK_REPOSITORY}:{prefix}-{stamp.lower()}"
        iid_file = output / "sdk.iid"
        build_log = output / "sdk-build.log"
        print(f"Building SDK image {tag} (profile={profile}, jobs={jobs}); log={build_log}", flush=True)
        identity = build_sdk_image(context, tag, iid_file, build_log, profile, jobs)
        build_result_path = output / "sdk-build-result.json"
        write_json(
            build_result_path,
            {"buildProfile": profile, "imageId": identity, "manifestSha256": manifest_sha, "passed": True},
        )
        result["tag"] = tag
        result["imageId"] = identity

        result["phase"] = "qualify"
        install_log = output / "sdk-install.log"
        print(f"Qualifying SDK image {identity} offline (--network=none --read-only)...", flush=True)
        script_sha = qualify_sdk_image(identity, profile, VERIFIER, install_log)
        install_result_path = output / "sdk-install-result.json"
        write_json(
            install_result_path,
            {
                "exitCode": 0,
                "imageId": identity,
                "logSha256": build_cpp.sha(install_log),
                "passed": True,
                "scriptSha256": script_sha,
            },
        )

        result["phase"] = "verify-host-unchanged"
        assert_host_unchanged(REPO, guard_roots, guard_policy, dependency_source, guard_hashes, dep_hashes)

        result["phase"] = "publish"
        pinned_ref, pinned_id = build_cpp.pin(identity, SDK_REPOSITORY)
        require(pinned_id == identity, "Pinned SDK image ID mismatch")
        payload = build_toolchain_payload(
            pinned_ref=pinned_ref,
            identity=identity,
            profile=profile,
            guard_roots=guard_roots,
            guard_policy=guard_policy,
            guard_hashes=guard_hashes,
            dependency_source=dependency_source,
            dep_hashes=dep_hashes,
            source_manifest_path=source_manifest_path,
            evidence_paths={
                "sdk-build-result.json": build_result_path,
                "sdk-install-result.json": install_result_path,
                "sdk-install.log": install_log,
                "sdk-snapshot-result.json": snapshot_result_path,
            },
            previous_sha=previous_sha,
        )
        candidate_path = output / "cpp-toolchain.candidate.json"
        write_json(candidate_path, payload)
        write_json_atomic(TOOLCHAIN, payload)

        result["phase"] = "validate-published"
        validated = build_cpp.validate_sdk()
        require(validated["imageId"] == identity and validated["image"] == pinned_ref,
                "Published cpp-toolchain.json did not match newly built SDK")

        result["published"] = True
        result["pinnedImage"] = pinned_ref
        result["phase"] = "complete"
        result["success"] = True
        if not keep_context:
            remove_tree(context)
        print(f"SDK refreshed and verified: {pinned_ref} ({identity})", flush=True)
        return 0
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
        result["errorType"] = type(error).__name__
        result["error"] = str(error)
        print(f"Stopped [{result['phase']}]: {error}", file=sys.stderr, flush=True)
        return 1
    finally:
        write_json_atomic(output / "result.json", result)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", choices=PROFILES, default="debug-compile-only",
                        help="SDK_BUILD_PROFILE passed to deploy/k8s/Dockerfile.cpp (default: debug-compile-only)")
    parser.add_argument("--jobs", type=int, choices=range(1, 9), default=4,
                        help="Parallel compile jobs inside the deps build container (default: 4)")
    parser.add_argument("--check", action="store_true",
                        help="Read-only drift check against build/docker-debug-current/cpp-toolchain.json")
    parser.add_argument("--force", action="store_true",
                        help="Rebuild SDK even if current cpp-toolchain.json already matches host inputs")
    parser.add_argument("--keep-context", action="store_true",
                        help="Keep the staged sdk-context directory after a successful refresh")
    args = parser.parse_args(argv)
    try:
        return refresh_sdk(
            profile=args.profile,
            jobs=args.jobs,
            check_only=args.check,
            force=args.force,
            keep_context=args.keep_context,
        )
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"Stopped [preflight]: {error}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
