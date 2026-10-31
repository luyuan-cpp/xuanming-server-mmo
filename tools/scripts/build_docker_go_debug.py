#!/usr/bin/env python3
"""串行编译本机 Go 调试镜像；不修改运行配置、不启动容器、不安装宿主工具。"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import sys
import time
from datetime import datetime, timezone
import uuid


ROOT = Path(__file__).resolve().parents[2]
CATALOGUE = {
    "login": ("login", "login.go"),
    "data-service": ("data_service", "data_service.go"),
    "db": ("db", "."),
    "player-locator": ("player_locator", "player_locator.go"),
    "scene-manager": ("scene_manager", "scene_manager_service.go"),
    "client-rpc-router": ("client_rpc_router", "client_rpc_router_service.go"),
    "match": ("match", "."),
    "player-inbox": ("player_inbox", "."),
    # 写者注册的一次性容器（compose 的 sql-boot、<服务>-sql-boot）：基础镜像是 deploy/debug/Dockerfile.sql-boot，/app/service 是 writer_authority。
    "sql-boot": ("data_service", "./cmd/writer_authority"),
}
DELVE_VERSION = "v1.27.2"
GO_VERSION = "go1.27.2"


class BuildFailure(Exception):
    """只保留固定分类；错误细节在本机独占日志里。"""


def utc() -> str:
    return datetime.now(timezone.utc).isoformat()


def write_json(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def sha256(path: Path) -> str:
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def stop_owned_process(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        return
    if os.name == "nt":
        subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
    else:
        process.kill()
    process.wait(timeout=20)


def run_logged(command: list[str], *, cwd: Path, env: dict[str, str], log: Path,
               timeout: int) -> None:
    with log.open("xb") as stream:
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=stream, stderr=subprocess.STDOUT)
        try:
            code = process.wait(timeout=timeout)
        except BaseException:
            stop_owned_process(process)
            raise
    if code:
        raise BuildFailure("command_failed")


def capture(command: list[str], *, cwd: Path = ROOT, env: dict[str, str] | None = None,
            timeout: int = 30) -> str:
    # Compose 的输出可能含派生密钥，只在内存解析，调用方不得原样输出或写日志。
    result = subprocess.run(command, cwd=cwd, env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, check=False)
    if result.returncode:
        raise BuildFailure("capture_command_failed")
    return result.stdout.decode("utf-8", errors="strict")


def snapshot(module: str) -> dict[str, str]:
    # 服务可执行文件不编译 *_test.go；测试改动不应触发运行镜像来源失配。
    # 其余相关 module 文件仍保守纳入，覆盖生成协议、依赖声明和嵌入资源。
    directories = [f"go/{module}", "go/shared", "go/proto", "go/schemamigrate"]
    names = capture(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard",
                     "--", *directories]).split("\0")
    result: dict[str, str] = {}
    for name in sorted(set(names) - {""}):
        if name.endswith("_test.go"):
            continue
        path = ROOT / name
        if not path.exists():
            continue
        if path.is_symlink() or not path.resolve().is_relative_to(ROOT):
            raise BuildFailure("source_outside_repository")
        if path.is_file():
            result[name.replace("\\", "/")] = sha256(path)
    # 策划表可能由生成器写在 Git 忽略目录，不能仅靠 git ls-files 收集运行输入。
    for path in runtime_resource_plan(module).values():
        result[path.relative_to(ROOT).as_posix()] = sha256(path)
    if not any(name.endswith(".go") for name in result):
        raise BuildFailure("source_manifest_empty")
    return result


def runtime_resource_plan(module: str) -> dict[str, Path]:
    """沿用服务 Dockerfile 的顶层 data* 合并布局，策划表路径统一小写。"""
    roots = [(path, "/app/data", False) for path in sorted((ROOT / "go" / module).iterdir())
             if path.is_dir() and path.name.startswith("data")]
    tables = ROOT / "generated/tables"
    if not tables.is_dir():
        raise BuildFailure("runtime_tables_missing")
    roots.append((tables, "/generated/tables", True))
    result: dict[str, Path] = {}
    destinations = set()
    for root, destination_root, lowercase in roots:
        if root.is_symlink() or not root.resolve().is_relative_to(ROOT):
            raise BuildFailure("runtime_resource_outside_repository")
        for path in sorted(root.rglob("*")):
            if path.is_symlink() or not path.resolve().is_relative_to(ROOT):
                raise BuildFailure("runtime_resource_outside_repository")
            if not path.is_file():
                continue
            relative = path.relative_to(root).as_posix()
            destination = destination_root + "/" + (relative.lower() if lowercase else relative)
            # Windows 冻结目录不区分大小写；任何目标冲突均失败，不猜覆盖顺序。
            key = destination.casefold()
            if key in destinations:
                raise BuildFailure("runtime_resource_destination_collision")
            destinations.add(key)
            result[destination] = path
    return result


def freeze_runtime_resources(module: str, directory: Path, expected: dict[str, str]) -> tuple[dict[str, str], dict[str, str]]:
    (directory / "app/data").mkdir(parents=True)
    (directory / "generated/tables").mkdir(parents=True)
    plan = runtime_resource_plan(module)
    expected_sources = set()
    for name in expected:
        parts = name.split("/")
        if name.startswith("generated/tables/") or (len(parts) > 3 and parts[:2] == ["go", module]
                                                     and parts[2].startswith("data")):
            expected_sources.add(name)
    if {path.relative_to(ROOT).as_posix() for path in plan.values()} != expected_sources:
        raise BuildFailure("runtime_resources_changed_before_freeze")
    artifacts: dict[str, str] = {}
    sources: dict[str, str] = {}
    for destination, source in plan.items():
        source_name = source.relative_to(ROOT).as_posix()
        target = directory / destination.lstrip("/")
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
        actual = sha256(target)
        if actual != expected[source_name]:
            raise BuildFailure("runtime_resource_changed_during_freeze")
        artifacts[destination] = actual
        sources[destination] = source_name
    return artifacts, sources


def check_elf(path: Path) -> None:
    # 流式读取 ELF 头与节名；宿主可用内存较低，不把整个服务二进制读入内存。
    with path.open("rb") as stream:
        header = stream.read(64)
        if len(header) != 64 or header[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", header, 18)[0] != 62:
            raise BuildFailure("artifact_not_linux_amd64")
        section_offset = struct.unpack_from("<Q", header, 40)[0]
        entry_size, count, names_index = struct.unpack_from("<HHH", header, 58)
        if entry_size != 64 or not 0 < names_index < count:
            raise BuildFailure("elf_sections_invalid")
        stream.seek(section_offset + names_index * entry_size)
        names_header = stream.read(entry_size)
        names_offset, names_size = struct.unpack_from("<QQ", names_header, 24)
        if names_size > 1024 * 1024:
            raise BuildFailure("elf_section_names_invalid")
        stream.seek(names_offset)
        names = stream.read(names_size)
        sections = set()
        for index in range(count):
            stream.seek(section_offset + index * entry_size)
            name_offset = struct.unpack("<I", stream.read(4))[0]
            sections.add(names[name_offset:].split(b"\0", 1)[0])
        if b".debug_info" not in sections or b".debug_line" not in sections:
            raise BuildFailure("artifact_missing_dwarf")


def build_environment(*, network: bool) -> dict[str, str]:
    result = os.environ.copy()
    result.update({"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0",
                   "GOTOOLCHAIN": "local", "GOMAXPROCS": "1", "GOMEMLIMIT": "512MiB", "GOWORK": "off",
                   "GOENV": "off", "GOFLAGS": "", "GOPROXY": "https://proxy.golang.org" if network else "off",
                   "GOSUMDB": "sum.golang.org", "GONOPROXY": "", "GONOSUMDB": "", "GOVCS": "*:off"})
    return result


def resolve_base_images(services: list[str], supplied: Path | None) -> dict[str, str]:
    if supplied is not None:
        value = json.loads(supplied.read_text(encoding="utf-8-sig"))
    else:
        raw = capture(["docker", "--context", "desktop-linux", "compose", "--env-file",
                       "build/docker-debug-current/.env", "-f", "deploy/debug/compose.game.yml",
                       "-f", "deploy/debug/compose.debug.yml", "config", "--format", "json"], timeout=60)
        configuration = json.loads(raw)
        value = {name: item.get("image") for name, item in configuration.get("services", {}).items()}
        del configuration, raw
    if not isinstance(value, dict):
        raise BuildFailure("base_images_invalid")
    result = {}
    for service in services:
        image = value.get(service)
        if not isinstance(image, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.:/@-]{0,250}", image):
            raise BuildFailure("base_image_missing_or_invalid")
        result[service] = image
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="仓库 build 下尚不存在的绝对输出目录")
    parser.add_argument("--services", default="login", help="login、逗号分隔服务名，或 all")
    parser.add_argument("--base-images", type=Path, help="可选 JSON 文件，仅包含服务名到本地运行镜像的映射")
    parser.add_argument("--jobs", type=int, choices=[1], default=1, help="固定为 1，串行编译")
    args = parser.parse_args()
    requested = list(CATALOGUE) if args.services == "all" else list(dict.fromkeys(args.services.split(",")))
    if not requested or any(name not in CATALOGUE for name in requested):
        parser.error("services 只能是当前八个常驻 Go 服务或 sql-boot")
    services = [name for name in CATALOGUE if name in requested]
    if not args.output.is_absolute():
        parser.error("output 必须为绝对路径")
    output = args.output.resolve()
    if not output.is_relative_to(ROOT / "build") or output.exists():
        parser.error("output 必须是仓库 build 下尚不存在的新目录")
    output.mkdir(parents=True)
    result = {"success": False, "images": {}, "manifestPaths": {}}
    write_json(output / "result.json", result)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ").lower() + "-" + uuid.uuid4().hex[:8]
    stage = "preflight"
    started = time.monotonic()
    journal = output / "journal.jsonl"

    def progress(event: str, service: str | None = None) -> None:
        record = {"utc": utc(), "elapsedSeconds": round(time.monotonic() - started, 3),
                  "event": event, "service": service}
        with journal.open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(record, ensure_ascii=False) + "\n")
        print(f"{record['utc']} {event}" + (f" {service}" if service else ""), flush=True)

    try:
        offline_environment = build_environment(network=False)
        version = capture(["go", "version"], env=offline_environment).strip()
        if not version.startswith(f"go version {GO_VERSION} "):
            raise BuildFailure("go_version_mismatch")
        bases = resolve_base_images(services, args.base_images)
        dockerfile = ROOT / "deploy/debug/Dockerfile.go"
        dockerfile_hash = sha256(dockerfile)
        commit = capture(["git", "rev-parse", "HEAD"]).strip()
        write_json(output / "preflight.json", {"utc": utc(), "sourceRoot": str(ROOT),
                   "goVersion": version, "services": services, "dockerfileSHA256": dockerfile_hash,
                   "commit": commit, "jobs": 1})

        # 唯一联网步骤：在本轮忽略目录中解析并编译固定版工具，不写宿主 PATH/GOBIN。
        stage = "delve_build"
        tools = output / "tools"
        tools.mkdir()
        (tools / "go.mod").write_text("module local/xuanming-docker-debug-tools\n\ngo 1.27.2\n\n"
                                      f"require github.com/go-delve/delve {DELVE_VERSION}\n", encoding="utf-8")
        delve_directory = output / "delve"
        delve_directory.mkdir()
        delve = delve_directory / "dlv"
        progress("delve_build_started")
        run_logged(["go", "build", "-p=1", "-mod=mod", "-buildvcs=false", "-o", str(delve),
                    "github.com/go-delve/delve/cmd/dlv"], cwd=tools,
                   env=build_environment(network=True), log=output / "delve.build.log", timeout=1200)
        check_elf(delve)
        details = capture(["go", "version", "-m", str(delve)], env=offline_environment)
        (output / "delve.build-info.txt").write_text(details, encoding="utf-8")
        if not re.search(r"\bmod\s+github\.com/go-delve/delve\s+v1\.27\.2\s", details):
            raise BuildFailure("delve_version_mismatch")
        module_cache = Path(capture(["go", "env", "GOMODCACHE"], env=offline_environment).strip())
        compatibility = module_cache / "github.com/go-delve/delve@v1.27.2/pkg/goversion/compat.go"
        compatibility_text = compatibility.read_text(encoding="utf-8")
        if not re.search(r"MaxSupportedVersionOfGoMinor\s*=\s*27\b", compatibility_text):
            raise BuildFailure("delve_go_support_mismatch")
        delve_hash = sha256(delve)
        write_json(output / "delve-receipt.json", {"version": DELVE_VERSION, "sha256": delve_hash,
                   "goVersion": GO_VERSION, "compatibilitySourceSHA256": sha256(compatibility),
                   "goVersionCheckDisabled": False})
        progress("delve_build_completed")

        for service in services:
            module, entry = CATALOGUE[service]
            directory = output / service
            directory.mkdir()
            binary_directory = directory / "binary"
            binary_directory.mkdir()
            metadata_directory = directory / "source_meta"
            metadata_directory.mkdir()
            binary = binary_directory / "service"
            stage = f"{service}_source_before"
            before = snapshot(module)
            write_json(directory / "source-before.json", before)
            source_digest = hashlib.sha256(json.dumps(before, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
            build_time = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            build_version = f"debug-{stamp}"
            build_commit = f"{commit[:12]}-dirty+{source_digest[:12]}"
            stage = f"{service}_build"
            progress("service_build_started", service)
            run_logged(["go", "build", "-p=1", "-mod=readonly", "-buildvcs=false", "-trimpath=false",
                        "-gcflags=all=-N -l", f"-ldflags=-X shared/buildinfo.Version={build_version} -X shared/buildinfo.Commit={build_commit} -X shared/buildinfo.BuildTime={build_time}",
                        "-o", str(binary), entry], cwd=ROOT / "go" / module, env=offline_environment,
                       log=directory / "build.log", timeout=1200)
            after = snapshot(module)
            write_json(directory / "source-after.json", after)
            if before != after:
                write_json(directory / "source-drift.json", {"changedPaths": sorted(
                    name for name in before.keys() | after.keys() if before.get(name) != after.get(name))})
                raise BuildFailure("source_changed_during_build")
            check_elf(binary)
            details = capture(["go", "version", "-m", str(binary)], env=offline_environment)
            (directory / "build-info.txt").write_text(details, encoding="utf-8")
            if not re.search(r'-gcflags="?all=-N -l"?(?:\r?\n|$)', details) or "-trimpath=true" in details:
                raise BuildFailure("service_debug_flags_mismatch")
            binary_hash = sha256(binary)
            stage = f"{service}_runtime_resources"
            runtime_directory = directory / "runtime_assets"
            runtime_artifacts, runtime_sources = freeze_runtime_resources(module, runtime_directory, before)
            manifest = {"schema": 1, "language": "go", "sourceRoot": str(ROOT), "files": before,
                        "artifacts": {"/app/service": binary_hash, "/usr/local/bin/dlv": delve_hash, **runtime_artifacts},
                        "runtimeResources": runtime_sources,
                        "build": {"goVersion": GO_VERSION, "gcflags": "all=-N -l", "trimpath": False,
                                  "stripSymbols": False, "goos": "linux", "goarch": "amd64", "cgoEnabled": False,
                                  "delveVersion": DELVE_VERSION, "goVersionCheckDisabled": False,
                                  "service": service, "module": module, "entry": entry, "commit": build_commit,
                                  "version": build_version, "buildTime": build_time, "sourceDigest": source_digest,
                                  "runtimeAssetsFrozen": True, "tablesLowercase": True,
                                  "dockerfileSHA256": dockerfile_hash}}
            manifest_path = metadata_directory / "source-manifest.json"
            write_json(manifest_path, manifest)
            (metadata_directory / "BUILD_INFO").write_text(
                f"version={build_version}\ncommit={build_commit}\nbuilt_at={build_time}\n", encoding="utf-8")
            result["manifestPaths"][service] = str(manifest_path)

            stage = f"{service}_base_image"
            inspection = capture(["docker", "--context", "desktop-linux", "image", "inspect", bases[service],
                                  "--format", "{{.Id}} {{.Os}} {{.Architecture}}" ]).strip().split()
            if len(inspection) != 3 or inspection[1:] != ["linux", "amd64"] or not re.fullmatch(r"sha256:[0-9a-f]{64}", inspection[0]):
                raise BuildFailure("base_image_platform_invalid")
            pinned_base = f"local/mmorpg-{service}-runtime:pin-{inspection[0].split(':')[1]}"
            run_logged(["docker", "--context", "desktop-linux", "image", "tag", inspection[0], pinned_base],
                       cwd=ROOT, env=os.environ.copy(), log=directory / "base-pin.log", timeout=30)
            if capture(["docker", "--context", "desktop-linux", "image", "inspect", pinned_base,
                        "--format", "{{.Id}}"] ).strip() != inspection[0]:
                raise BuildFailure("base_image_pin_changed")
            image = f"local/mmorpg-{service}:debug-{stamp}"
            if sha256(dockerfile) != dockerfile_hash or sha256(delve) != delve_hash:
                raise BuildFailure("packaging_input_changed")
            stage = f"{service}_image_build"
            progress("image_build_started", service)
            run_logged(["docker", "--context", "desktop-linux", "build", "--pull=false", "--network", "none",
                        "--progress=plain", "-f", str(dockerfile), "--build-arg", f"GO_RUNTIME_IMAGE={pinned_base}",
                        "--build-arg", f"BUILD_VERSION={build_version}", "--build-arg", f"BUILD_COMMIT={build_commit}",
                        "--build-arg", f"BUILD_TIME={build_time}",
                        "--build-context", f"binary={binary_directory}", "--build-context", f"delve={delve_directory}",
                        "--build-context", f"runtime_assets={runtime_directory}",
                        "--build-context", f"source_meta={metadata_directory}", "-t", image, str(metadata_directory)],
                       cwd=ROOT, env=os.environ.copy(), log=directory / "image.log", timeout=300)
            image_id = capture(["docker", "--context", "desktop-linux", "image", "inspect", image,
                                "--format", "{{.Id}}"] ).strip()
            if capture(["docker", "--context", "desktop-linux", "image", "inspect", pinned_base,
                        "--format", "{{.Id}}"] ).strip() != inspection[0]:
                raise BuildFailure("base_image_pin_changed")
            if (not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or sha256(binary) != binary_hash
                    or any(sha256(runtime_directory / path.lstrip("/")) != expected_hash
                           for path, expected_hash in runtime_artifacts.items())):
                raise BuildFailure("packaged_artifact_changed")
            labels = json.loads(capture(["docker", "--context", "desktop-linux", "image", "inspect", image,
                                         "--format", "{{json .Config.Labels}}"] ))
            expected_labels = {"org.opencontainers.image.version": build_version,
                               "org.opencontainers.image.revision": build_commit,
                               "org.opencontainers.image.created": build_time}
            if any(labels.get(name) != value for name, value in expected_labels.items()):
                raise BuildFailure("image_build_labels_mismatch")
            result["images"][service] = {"image": image, "imageId": image_id}
            write_json(directory / "image-receipt.json", {"image": image, "imageId": image_id,
                       "baseImage": bases[service], "baseImageId": inspection[0], "manifestSHA256": sha256(manifest_path),
                       "runtimeResourceFiles": len(runtime_artifacts), "labels": expected_labels})
            write_json(output / "result.json", result)
            progress("image_build_completed", service)

        result["success"] = True
        write_json(output / "result.json", result)
        progress("completed")
        return 0
    except BaseException as error:
        # 不把异常字符串或 Compose 展开值复制到可见输出；首错日志原样保留在本轮目录。
        write_json(output / "failure.json", {"utc": utc(), "stage": stage, "exceptionType": type(error).__name__,
                   "classification": error.args[0] if isinstance(error, BuildFailure) else "execution_failed"})
        write_json(output / "result.json", result)
        progress("failed")
        return 1


if __name__ == "__main__":
    sys.exit(main())
