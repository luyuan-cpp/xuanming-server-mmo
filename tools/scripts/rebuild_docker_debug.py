"""本机 Docker 调试入口：构建成功后更新指定服务，附加前核对源码。"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys


REPO = Path(__file__).resolve().parents[2]
STATE = REPO / "build/docker-debug-current"
CPP = ("gate", "scene", "battle")
GO = ("login", "db", "data-service", "scene-manager", "player-locator", "client-rpc-router", "match", "player-inbox", "sql-boot", "db-migrate")
IMAGE_KEYS = {
    **dict.fromkeys(CPP, "NODE_DEBUG_IMAGE"),
    "login": "LOGIN_IMAGE", "db": "DB_IMAGE", "data-service": "DATA_SERVICE_IMAGE",
    "scene-manager": "SCENE_MANAGER_IMAGE", "player-locator": "PLAYER_LOCATOR_IMAGE",
    "client-rpc-router": "CLIENT_RPC_ROUTER_IMAGE", "match": "MATCH_IMAGE", "player-inbox": "PLAYER_INBOX_IMAGE",
    "gateway": "GATEWAY_IMAGE",
    "sql-boot": "SQL_BOOT_IMAGE",
    "db-migrate": "DB_MIGRATE_IMAGE",
}
# 写者的单次启动授权启动即消费：换容器要连注册步骤（compose 的 <服务>-sql-boot）一起重建，只重建服务会被拒启。
SQL_WRITERS = ("login", "db", "data-service", "match", "player-inbox", "gateway")
# 一次性容器没有运行态可核对：重编只换 .env 里的镜像，下一次 up 生效。
ONE_SHOT = ("sql-boot", "db-migrate")
DOCKER = ["docker", "--context", "desktop-linux"]
COMPOSE = DOCKER + ["compose", "--env-file", str(STATE / ".env"),
                    "-f", str(REPO / "deploy/debug/compose.game.yml"),
                    "-f", str(REPO / "deploy/debug/compose.debug.yml")]


def execute(args: list[str], *, capture: bool = True, timeout: int = 90) -> str:
    result = subprocess.run(args, cwd=REPO, capture_output=capture, text=True,
                            encoding="utf-8", errors="replace", timeout=timeout)
    if result.returncode:
        # Compose 展开内容含密钥；失败时只报告命令类型，不打印捕获的数据。
        raise RuntimeError(f"{Path(args[0]).name} 执行失败，退出码 {result.returncode}")
    return result.stdout if capture else ""


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def compose_config() -> dict:
    return json.loads(execute(COMPOSE + ["config", "--format", "json"]))


def current_sources(language: str, service: str) -> dict[str, str]:
    if language == "cpp":
        from build_docker_cpp_debug import inputs, validate_sdk
        validate_sdk()
        return {name: digest(path) for name, path in inputs().items()}
    if language == "go":
        from build_docker_go_debug import BuildFailure, CATALOGUE, snapshot
        if service not in CATALOGUE:
            raise RuntimeError("Go 服务与清单不匹配")
        try:
            return snapshot(CATALOGUE[service][0])
        except BuildFailure as error:
            raise RuntimeError("Go 输入检查未通过：" + str(error)) from error
    if language == "java" and service == "gateway":
        from build_docker_java_debug import source_files
        return source_files()
    raise RuntimeError("调试清单语言与服务不匹配")


def runtime_sources(language: str, files: dict[str, str]) -> dict[str, str]:
    # 普通 go build 不编译 _test.go；其它任务修改单测不能使运行服务的源码检查误报。
    return {name: value for name, value in files.items()
            if not (language == "go" and name.endswith("_test.go"))}


def check_config_inputs(services: list[str]) -> None:
    from build_docker_go_debug import CATALOGUE
    from prepare_docker_debug import GO_CONFIGS, SQL_BOOT_SOURCES
    recorded = json.loads((STATE / "source-manifest.json").read_text("utf-8-sig"))
    names = set()
    for service in services:
        if service in SQL_WRITERS or service in ONE_SHOT:
            names.update(SQL_BOOT_SOURCES)
        if service in CPP:
            names.update(("bin/etc/base_deploy_config.yaml", "bin/etc/game_config.yaml"))
        elif service in GO:
            module = CATALOGUE[service][0]
            names.add(f"go/{module}/etc/{GO_CONFIGS[module]}")
        elif service == "gateway":
            names.add("java/gateway_node/src/main/resources/application.yaml")
    for name in names:
        if recorded.get(name) != digest(REPO / name):
            raise RuntimeError("运行配置源已变化：" + name
                + "；请用 python -X utf8 -B tools/scripts/rebuild_all_docker_debug.py "
                  "保留原栈设置、刷新配置并重编更新整套服务（Java 真库测试前置仍须具备）")


TABLE_TARGETS = {**dict.fromkeys(CPP, "/app/generated/generated_tables"),
                 **dict.fromkeys(("login", "scene-manager", "player-locator"), "/generated/tables")}


def load_runtime_data_manifest(path: Path) -> dict:
    try:
        return _load_runtime_data_manifest(path)
    except (KeyError, TypeError, AttributeError, OSError, ValueError) as error:
        raise RuntimeError("Invalid runtime data manifest: " + str(error)) from error


def _load_runtime_data_manifest(path: Path) -> dict:
    """仅接受来源及全部摘要均锁定的显式开发表，不改镜像编译身份。"""
    data = json.loads(path.read_text("utf-8-sig"))
    if data.get("schemaVersion") != 1 or data.get("developmentOnly") is not True or data.get("productionTravelEnabled") is not False:
        raise RuntimeError("Runtime data manifest is not explicitly development-only")
    for field in ("exportDelivery", "sourceExport"):
        item = data[field]
        if digest(Path(item["path"])) != item["sha256"]:
            raise RuntimeError("Runtime data source binding changed: " + field)
    report = json.loads(Path(data["sourceExport"]["path"]).read_text("utf-8-sig"))
    if (report.get("developmentOnly") is not True or report.get("productionTravelEnabled") is not False
            or report.get("exportDeliverySha256") != data["exportDelivery"]["sha256"]):
        raise RuntimeError("Table export development/source identity mismatch")
    books = data.get("sourceWorkbooks", {})
    if set(books) != {"BaseScene.xlsx", "World.xlsx"}:
        raise RuntimeError("Expected the two copied source workbooks")
    for name, item in books.items():
        if (Path(item["path"]).name != name or digest(Path(item["path"])) != item["sha256"]
                or report.get("sourceWorkbookSha256", {}).get(name) != item["sha256"]):
            raise RuntimeError("Copied source workbook changed: " + name)
    directory = Path(data["tableDirectory"]).resolve()
    if directory != Path(report["generatedTables"]).resolve():
        raise RuntimeError("Exported table directory identity mismatch")
    table_manifest = directory / "manifest.json"
    manifest_sha = digest(table_manifest)
    if (table_manifest.resolve().parent != directory or manifest_sha != data["officialTableManifestSha256"]
            or manifest_sha != report["officialTableManifestSha256"]):
        raise RuntimeError("Official table manifest changed")
    official = json.loads(table_manifest.read_text("utf-8-sig"))
    if official["table_count"] != len(official["tables"]):
        raise RuntimeError("Official table manifest count mismatch")
    recorded_sources = {table.get("source", {}).get("file"): table.get("source", {}).get("sha256")
                        for table in official["tables"]}
    if any(recorded_sources.get(name) != item["sha256"] for name, item in books.items()):
        raise RuntimeError("Official table manifest does not bind the copied source workbooks")
    expected = {"manifest.json": manifest_sha}
    for table in official["tables"]:
        for artifact in table["artifacts"]:
            name = artifact["file"]
            if (not re.fullmatch(r"[a-z0-9_]+\.(json|pb)", name) or name in expected
                    or (directory / name).resolve().parent != directory
                    or (directory / name).stat().st_size != artifact["size"]):
                raise RuntimeError("Invalid official table artifact path/size")
            expected[name] = artifact["sha256"]
    # 正式导表器还输出未列入表 manifest 的 Tip 文案；只允许保持 baseline 摘要不变。
    supplemental = report.get("supplementalTableSha256", {})
    if (set(supplemental) - {"tip_text.json"}
            or any(report.get("unchangedTableSha256", {}).get(n) != sha for n, sha in supplemental.items())):
        raise RuntimeError("Unsupported or changed supplemental table artifact")
    expected.update(supplemental)
    if expected != data["tableSha256"] or expected != report["tableSha256"]:
        raise RuntimeError("Official table artifact set/SHA differs from runtime data receipt")
    if {p.name for p in directory.iterdir() if p.is_file()} != set(expected):
        raise RuntimeError("Runtime table directory has unrecorded files")
    if any(digest(directory / name) != value for name, value in expected.items()):
        raise RuntimeError("Runtime table SHA mismatch")
    services = data.get("services", {})
    if not services or not set(services).issubset(TABLE_TARGETS):
        raise RuntimeError("Unsupported runtime table consumer")
    for service, mount in services.items():
        if (mount.get("target") != TABLE_TARGETS[service] or mount.get("readOnly") is not True
                or Path(mount["source"]).resolve() != directory):
            raise RuntimeError("Runtime table mount is outside the allowed table directory")
    data["manifestSha256"] = digest(path)
    data["supplementalTableSha256"] = supplemental
    return data


def normalized_bind_source(value: str) -> str:
    value = value.replace("\\", "/").rstrip("/")
    for prefix in ("/run/desktop/mnt/host/", "/host_mnt/"):
        if value.startswith(prefix):
            rest = value[len(prefix):]
            if re.match(r"[a-zA-Z]/", rest):
                value = rest[0] + ":/" + rest[2:]
    return value.casefold() if re.match(r"[a-zA-Z]:/", value) else value


def runtime_table_artifacts(service: str, container: str, artifacts: dict, data: dict | None) -> dict:
    if data is None or service not in data["services"]:
        return artifacts
    mount = data["services"][service]
    mounts = json.loads(execute(DOCKER + ["inspect", "--format", "{{json .Mounts}}", container]))
    matches = [m for m in mounts if m.get("Destination") == mount["target"]]
    if (len(matches) != 1 or matches[0].get("Type") != "bind" or matches[0].get("RW") is not False
            or normalized_bind_source(matches[0].get("Source", "")) != normalized_bind_source(str(Path(mount["source"]).resolve()))):
        raise RuntimeError("Runtime tables must use the recorded read-only bind mount")
    for name, sha in data.get("supplementalTableSha256", {}).items():
        original = artifacts.get(mount["target"] + "/" + name)
        if service not in CPP and original != sha:
            raise RuntimeError("Supplemental table differs from the compiled image artifact")
    # 白名单只替换表文件摘要；二进制、符号及其余原始产物仍逐项校验。
    return {**artifacts, **{mount["target"] + "/" + name: sha for name, sha in data["tableSha256"].items()}}


def check(services: list[str], output: Path | None = None, runtime_data_manifest: Path | None = None) -> bool:
    runtime_data = load_runtime_data_manifest(runtime_data_manifest) if runtime_data_manifest else None
    success = True
    receipts = []
    # 三个 C++ 节点使用同一批源码；一次检查内共享本次扫描，避免反复遍历 SDK。
    source_scans = {}
    for service in services:
        record = {"service": service, "success": False, "differences": []}
        try:
            check_config_inputs([service])
            ids = execute(COMPOSE + ["ps", "--all", "-q", service]).splitlines()
            if len(ids) != 1:
                raise RuntimeError("服务容器不存在或数量不为一")
            container = ids[0].strip()
            identity = json.loads(execute(DOCKER + ["inspect", "--format",
                '{"Image":{{json .Image}},"State":{"Running":{{json .State.Running}}}}', container]))
            if not identity["State"]["Running"]:
                raise RuntimeError("服务容器未运行")
            record["imageId"] = identity["Image"]
            raw = execute(DOCKER + ["exec", container, "cat", "/app/DEBUG_SOURCE.json"])
            manifest = json.loads(raw)
            if manifest.get("schema") != 1 or not isinstance(manifest.get("files"), dict) or not manifest["files"]:
                raise RuntimeError("镜像源码清单缺失或不完整，请先重新构建该服务")
            expected_files = runtime_sources(manifest.get("language"), manifest["files"])
            scan_key = (manifest.get("language"), "nodes" if manifest.get("language") == "cpp" else service)
            if scan_key not in source_scans:
                source_scans[scan_key] = runtime_sources(manifest.get("language"),
                    current_sources(manifest.get("language"), service))
            current = source_scans[scan_key]
            for name, expected in expected_files.items():
                relative = PurePosixPath(name)
                if relative.is_absolute() or ".." in relative.parts or "\\" in name:
                    raise RuntimeError("镜像源码清单路径非法")
                path = REPO.joinpath(*relative.parts)
                if not path.resolve().is_relative_to(REPO):
                    raise RuntimeError("本地源码路径越界")
                if current.get(name) != expected:
                    record["differences"].append(name)
            for name in sorted(set(current) - set(expected_files)):
                record["differences"].append(name + " (new)")
            artifacts = manifest.get("artifacts", {})
            if not artifacts or any((not p.startswith(("/app/", "/generated/tables/")) and p != "/usr/local/bin/dlv")
                or ".." in PurePosixPath(p).parts for p in artifacts):
                raise RuntimeError("镜像产物摘要缺失或路径非法")
            artifacts = runtime_table_artifacts(service, container, artifacts, runtime_data)
            if runtime_data is not None and service in runtime_data["services"]:
                record["runtimeDataManifestSha256"] = runtime_data["manifestSha256"]
                record["officialTableManifestSha256"] = runtime_data["officialTableManifestSha256"]
            hashes = execute(DOCKER + ["exec", container, "sha256sum", "--", *artifacts]).splitlines()
            actual = {line[66:]: line[:64] for line in hashes if len(line) > 66}
            if actual != artifacts:
                raise RuntimeError("容器二进制或调试符号已偏离构建清单")
            record["sourceCount"] = len(expected_files)
            record["success"] = not record["differences"]
            print(f"{service}: " + (f"sources match the built artifacts ({record['sourceCount']} files)" if record["success"]
                else f"{len(record['differences'])} source files changed; rebuild required"))
            for name in record["differences"][:8]:
                print("  " + name)
        except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as error:
            record["error"] = str(error)
            print(f"{service}: check failed: {error}")
        success = success and record["success"]
        receipts.append(record)
    if output is not None:
        output.write_text(json.dumps({"success": success, "services": receipts}, ensure_ascii=False, indent=2), encoding="utf-8")
    return success


def update_images(images: dict) -> None:
    path = STATE / ".env"
    original = path.read_bytes()
    text = original.decode("utf-8-sig")
    replacements = {}
    for service, image in images.items():
        if service not in IMAGE_KEYS or not re.fullmatch(r"local/[a-z0-9._/-]+:[a-z0-9._-]+", image["image"]):
            raise RuntimeError("构建结果包含非法镜像引用")
        actual = execute(DOCKER + ["image", "inspect", "--format", "{{.Id}}", image["image"]]).strip()
        if actual != image["imageId"]:
            raise RuntimeError("本地镜像标签与构建结果不同，未更新配置")
        key = IMAGE_KEYS[service]
        if key in replacements and replacements[key] != image["image"]:
            raise RuntimeError("C++ 节点必须来自同一镜像")
        replacements[key] = image["image"]
    for key, image in replacements.items():
        pattern = re.compile(r"^" + re.escape(key) + r"=.*$", re.MULTILINE)
        count = len(pattern.findall(text))
        if count > 1:
            raise RuntimeError("配置镜像键重复，未更新配置：" + key)
        if count:
            text = pattern.sub(key + "=" + image, text)
        else:
            # 初始配置可沿用 Compose 镜像默认值；首次重编后才写显式的新镜像引用。
            text = text.rstrip("\r\n") + "\n" + key + "=" + image + "\n"
    if path.read_bytes() != original:
        raise RuntimeError("开发配置被其他任务更新，未覆盖配置")
    temporary = path.with_name(".env.debug-update.tmp")
    with temporary.open("x", encoding="utf-8", newline="") as stream:
        stream.write(text)
    os.replace(temporary, path)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("language", choices=("cpp", "go", "java", "check"))
    parser.add_argument("--services", help="逗号分隔；C++ 默认三个节点，Go 默认 login，Java 默认 gateway")
    parser.add_argument("--jobs", type=int, default=1, choices=range(1, 5))
    parser.add_argument("--build-only", action="store_true", help="仅构建，不修改配置或更新容器")
    parser.add_argument("--runtime-data-manifest", type=Path, help="仅 check：来源与 SHA 锁定的本机开发表挂载清单")
    args = parser.parse_args()
    if args.runtime_data_manifest is not None and args.language != "check":
        parser.error("--runtime-data-manifest 仅允许 check")
    defaults = {"cpp": CPP, "go": ("login",), "java": ("gateway",), "check": ("scene",)}
    services = args.services.split(",") if args.services else list(defaults[args.language])
    allowed = {"cpp": CPP, "go": GO, "java": ("gateway",), "check": tuple(name for name in IMAGE_KEYS if name not in ONE_SHOT)}
    if len(set(services)) != len(services) or not services or any(s not in allowed[args.language] for s in services):
        parser.error("服务名不属于选择的语言，或包含重复项")
    if args.language == "cpp" and set(services) != set(CPP):
        parser.error("C++ 共享库统一重编后须一起更新 gate,scene,battle")
    if args.language == "go" and args.jobs != 1:
        parser.error("Go 调试构建固定串行 --jobs 1")
    if args.language == "check":
        return 0 if check(services, runtime_data_manifest=args.runtime_data_manifest) else 1
    check_config_inputs(services)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    root = REPO / "build/verification" / ("docker-rebuild-" + args.language + "-" + stamp)
    root.mkdir(parents=True, exist_ok=False)
    config = compose_config()
    bases = {s: config["services"][s]["image"] for s in services}
    base_file = root / "base-images.json"
    base_file.write_text(json.dumps(bases, indent=2), encoding="utf-8")
    command = [sys.executable, "-X", "utf8", str(REPO / f"tools/scripts/build_docker_{args.language}_debug.py"),
               "--output", str(root / "build"), "--base-images", str(base_file)]
    if args.language != "java":
        command += ["--services", ",".join(services), "--jobs", str(args.jobs)]
    print("Evidence for this run: " + str(root), flush=True)
    # 子构建器自行保存日志；失败立即返回，不替换正在运行的服务。
    result = subprocess.run(command, cwd=REPO)
    if result.returncode:
        print("Build failed; running containers were not updated. Check this run's build directory.")
        return result.returncode
    build = json.loads((root / "build/result.json").read_text("utf-8-sig"))
    if not build.get("success") or set(build.get("images", {})) != set(services):
        raise RuntimeError("构建结果不完整，未更新容器")
    if args.build_only:
        print("Build finished; as requested, configuration and containers were not changed.")
        return 0
    update_images(build["images"])
    running = [s for s in services if s not in ONE_SHOT]
    if not running:
        print("Image updated in .env; the next compose up uses it.")
        return 0
    targets = [name for s in running for name in ((s + "-sql-boot", s) if s in SQL_WRITERS else (s,))]
    with (root / "recreate.log").open("x", encoding="utf-8") as log:
        result = subprocess.run(COMPOSE + ["up", "-d", "--no-build", "--pull", "never", "--no-deps",
            "--force-recreate", "--wait", "--wait-timeout", "180", *targets], cwd=REPO,
            stdout=log, stderr=subprocess.STDOUT, text=True, encoding="utf-8")
    if result.returncode:
        print("Container update failed; configuration and state were kept. Check " + str(root / "recreate.log"))
        return result.returncode
    if not check(running, root / "verification.json"):
        return 1
    print("Build, container update and source check finished; press F5 in VS Code to attach.")
    return 0


if __name__ == "__main__":
    try:
        # VS Code 与 API 进程可能继承 CP1252 控制台；提示和子构建统一使用 UTF-8。
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8")
        raise SystemExit(main())
    except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        print("Stopped: " + str(error), file=sys.stderr)
        raise SystemExit(1)
