#!/usr/bin/env python3
"""封存当前网关源码，离线测试打包，并生成带源码收据的本地 Java 镜像。"""

from __future__ import annotations

import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import uuid
import xml.etree.ElementTree as ET
import zipfile


# tools/ 也有局部 AGENTS.md；仓库根由本脚本固定的 tools/scripts 层级确定。
REPO_ROOT = Path(__file__).resolve().parents[2]
GATEWAY_ROOT = REPO_ROOT / "java/gateway_node"
REACTOR_MODULES = ("authority_jdbc", "gateway_node")
PROTO_ROOT = "proto/common/database/rollback_database_table.proto"
DESCRIPTOR_RESOURCE = "META-INF/sql-authority/route-authority.desc"
MAVEN_PROFILE = "-f java/pom.xml -pl gateway_node -am -o -B -T1 clean package"
# 现存仓库 protoc 的明确 pin；正式入口无需新增参数，覆盖时必须同时指定路径和摘要。
DEFAULT_PROTOC = REPO_ROOT / "third_party/protobuf/install_vs2026_dbg/bin/protoc.exe"
DEFAULT_PROTOC_SHA256 = "a90e35b4a7675c024f81c0d07d7752f4321e7c46384a859cbd8cc7b19b4053c3"
DEFAULT_JDK = Path("D:/luyuan/tools/jdk23/jdk-23.0.2+7")
DEFAULT_MAVEN = Path("D:/luyuan/tools/apache-maven-3.9.9/bin/mvn.cmd")
# 仅接受源码已经通过 JUnit 条件/Assumptions 声明的外部集成用例中断；不增加 Maven 排除参数。
OPTIONAL_GATEWAY_TESTS = {
    ("com.game.gateway.repository.ZoneConfigUpsertLockOrderMySqlTest", "upsertBehindPendingDeleteOnlyQueues"),
    ("com.game.gateway.repository.ZoneConfigUpsertLockOrderMySqlTest", "legacyMergeInsertBehindPendingDeleteDeadlocks"),
    ("com.game.gateway.repository.ZoneWhitelistUpsertLockOrderMySqlTest", "upsertBehindPendingDeleteOnlyQueues"),
    ("com.game.gateway.repository.ZoneWhitelistUpsertLockOrderMySqlTest", "legacyPlainInsertBehindPendingDeleteLosesOneWriter"),
    ("com.game.gateway.repository.GatewaySchemaContractTest", "formalSchemaValidatesOnRealTiDbAndRoundTripsUnsignedZoneRange"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "freshBucketIsFull"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "emptyBucket_refillsAtRps"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "take_writesRemainingTokensAndTime_rejectionWritesNothing"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "zoneEmpty_returnsCeilWait_andDoesNotSpendIpToken"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "ipEmpty_doesNotTouchZone"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "ok_takesOneTokenFromEachBucket"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "keyExpiresWhenBucketWouldBeFull"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "newThresholds_applyToExistingBucketImmediately"),
    ("com.game.gateway.ratelimit.RedisRateLimitStoreIntegrationTest", "serverTime_isUsedWhenNoTimeGiven"),
}
BREAKPOINT_CLASSES = (
    "com.game.gateway.controller.LoginController",
    "com.game.gateway.controller.ServerListController",
)


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def write_json(path: Path, value: object) -> None:
    # 独占创建，失败证据和之前的构建结果都不覆盖。
    with path.open("x", encoding="utf-8", newline="\n") as target:
        json.dump(value, target, ensure_ascii=False, indent=2)
        target.write("\n")


def create_workspace(output: Path, details: dict[str, object]) -> tuple[Path, Path]:
    """Reserve a short, fresh workspace independently of the evidence path.

    The protobuf plugin copies the protoc URL into target; nesting both that URL
    and target below a long --output can exceed Windows process path limits.
    Keep every workspace on success and failure for inspection; never adopt an
    existing directory or its target/JAR files.
    """
    base = REPO_ROOT / "build/jv"
    base.mkdir(parents=True, exist_ok=True)
    if (not base.resolve().is_relative_to(REPO_ROOT.resolve()) or
            any(path.is_symlink() or path.is_junction() for path in (REPO_ROOT / "build", base))):
        raise RuntimeError("Java workspace must stay in the repository's ignored build directory")
    workspace = base / uuid.uuid4().hex[:12]
    workspace.mkdir(exist_ok=False)
    snapshot, toolchain = workspace / "s", workspace / "t"
    details.update(workspace=str(workspace), snapshot=str(snapshot), toolchain=str(toolchain),
                   workspaceLayout="repo-build-jv-v1", workspaceRetained=True)
    provenance = {"layout": "repo-build-jv-v1", "sourceRoot": str(REPO_ROOT),
                  "evidence": str(output), "workspace": str(workspace),
                  "snapshot": str(snapshot), "toolchain": str(toolchain), "retained": True}
    write_json(output / "workspace.json", provenance)
    write_json(workspace / "owner.json", provenance)
    snapshot.mkdir()
    toolchain.mkdir()
    return snapshot, toolchain


def proto_inputs(root: Path) -> dict[str, Path]:
    """只解析 import 图来封存输入，descriptor 内容仍由正式 protoc 生成。"""
    pending, found = [PROTO_ROOT], {}
    while pending:
        name = pending.pop()
        if name in found:
            continue
        if not re.fullmatch(r"[A-Za-z0-9_./-]+[.]proto", name) or ".." in Path(name).parts:
            raise RuntimeError("Invalid descriptor import path")
        path = root / name if name.startswith("proto/") else root / "third_party/protobuf/src" / name
        if not path.is_file() or path.is_symlink() or not path.resolve().is_relative_to(root.resolve()):
            raise RuntimeError("Descriptor import is missing or outside the source root")
        found[name] = path
        # 保留字符串，去掉注释，避免注释中的 import 造成伪闭包。
        text = re.sub(r'"(?:\\.|[^"\\])*"|//[^\n]*|/\*[\s\S]*?\*/',
                      lambda match: match[0] if match[0].startswith('"') else " ", path.read_text("utf-8-sig"))
        imports = re.findall(r'\bimport\s+(?:(?:public|weak)\s+)?"([^"\n]+)"\s*;', text)
        if len(imports) != len(re.findall(r"\bimport\s", text)):
            raise RuntimeError("Unsupported descriptor import syntax")
        pending.extend(imports)
    return found


def collect_sources(root: Path) -> dict[str, str]:
    namespace = {"m": "http://maven.apache.org/POM/4.0.0"}
    reactor = root / "java/pom.xml"
    modules = [item.text for item in ET.parse(reactor).findall("m:modules/m:module", namespace)]
    if not set(REACTOR_MODULES) <= set(modules):
        raise RuntimeError("Required reactor modules are missing")
    candidates = [reactor]
    for module in modules:
        if not module or not re.fullmatch(r"[A-Za-z0-9_-]+", module):
            raise RuntimeError("Unsupported reactor module path")
        candidates.append(root / "java" / module / "pom.xml")
    for module in REACTOR_MODULES:
        directory = root / "java" / module / "src"
        if not directory.is_dir():
            raise RuntimeError("Required reactor source directory is missing")
        candidates.extend(directory.rglob("*"))
    for base in (root, root / "java", *(root / "java" / name for name in REACTOR_MODULES)):
        for name in ("mvnw", "mvnw.cmd"):
            if (base / name).exists():
                candidates.append(base / name)
        if (base / ".mvn").exists():
            candidates.extend((base / ".mvn").rglob("*"))
    candidates.extend(proto_inputs(root).values())
    files = {}
    for path in sorted(set(candidates)):
        if path.is_symlink() or not path.resolve().is_relative_to(root.resolve()):
            raise RuntimeError("Maven input contains a symlink or escaped path")
        if path.is_dir():
            continue
        if not path.is_file():
            raise RuntimeError("Maven input file is missing")
        files[path.relative_to(root).as_posix()] = sha256(path)
    return files


def source_files() -> dict[str, str]:
    return collect_sources(REPO_ROOT)


def snapshot_files(snapshot: Path) -> dict[str, str]:
    return collect_sources(snapshot)


def assert_inputs(expected: dict[str, str], snapshot: Path) -> None:
    if source_files() != expected:
        raise RuntimeError("Local Maven inputs changed during the build")
    if snapshot_files(snapshot) != expected:
        raise RuntimeError("Sealed Maven inputs changed")


def capture(command: list[str], *, timeout: int = 30) -> str:
    completed = subprocess.run(
        command, cwd=REPO_ROOT, capture_output=True, text=True,
        encoding="utf-8", errors="replace", timeout=timeout, check=False,
    )
    if completed.returncode:
        # 不把 docker inspect 或其他命令的整个输出带入终端。
        raise RuntimeError(f"Read-only prerequisite command failed with exit code {completed.returncode}")
    return completed.stdout.strip()


def logged(command: list[str], log: Path, *, cwd: Path,
           env: dict[str, str], timeout: int) -> None:
    with log.open("x", encoding="utf-8", newline="\n") as output:
        process = subprocess.Popen(
            command, cwd=cwd, env=env, stdout=output, stderr=subprocess.STDOUT,
        )
        try:
            exit_code = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired as error:
            # PowerShell→mvn.cmd→JVM 是同一构建进程树，只结束本次创建的子进程。
            # 不按 java/docker 等进程名清理，避免影响正在运行的服务器。
            if os.name == "nt":
                subprocess.run(
                    ["taskkill.exe", "/PID", str(process.pid), "/T", "/F"],
                    stdout=output, stderr=subprocess.STDOUT, timeout=30, check=False,
                )
            if process.poll() is None:
                process.kill()
            process.wait(timeout=15)
            raise RuntimeError(f"Command exceeded {timeout} seconds; inspect {log.name}") from error
    if exit_code:
        raise RuntimeError(f"Command failed with exit code {exit_code}; inspect {log.name}")


def test_summary(snapshot: Path) -> dict[str, object]:
    if any(not list((snapshot / "java" / module / "target/surefire-reports").glob("TEST-*.xml"))
           for module in REACTOR_MODULES):
        raise RuntimeError("Each selected reactor module must produce test reports")
    reports = sorted(path for module in REACTOR_MODULES
                     for path in (snapshot / "java" / module / "target/surefire-reports").glob("TEST-*.xml"))
    cases: dict[tuple[str, str, str], str] = {}
    duplicates = 0
    for report in reports:
        root = ET.parse(report).getroot()
        for case in root.iter("testcase"):
            key = (report.parent.parent.parent.name, case.get("classname", ""), case.get("name", ""))
            status = "passed"
            for candidate in ("error", "failure", "skipped"):
                if case.find(candidate) is not None:
                    status = {"error": "errors", "failure": "failures"}.get(candidate, candidate)
                    break
            if key in cases:
                duplicates += 1
                if cases[key] != status:
                    raise RuntimeError("Conflicting test outcomes in XML reports")
            cases[key] = status
    summary = dict(reports=len(reports), tests=len(cases), passed=0,
                   failures=0, errors=0, skipped=0, duplicateCases=duplicates)
    summary["unexpectedSkipped"] = 0
    summary["unverifiedExternalTests"] = []
    summary["modules"] = {module: dict(tests=0, passed=0, failures=0, errors=0, skipped=0)
                          for module in REACTOR_MODULES}
    for (module, class_name, method), status in sorted(cases.items()):
        summary[status] += 1
        summary["modules"][module]["tests"] += 1
        summary["modules"][module][status] += 1
        if status == "skipped":
            if module == "gateway_node" and (class_name, method) in OPTIONAL_GATEWAY_TESTS:
                summary["unverifiedExternalTests"].append({"module": module, "class": class_name, "method": method})
            else:
                summary["unexpectedSkipped"] += 1
    return summary


def authority_test_evidence(snapshot: Path) -> dict:
    reports = snapshot / "java/authority_jdbc/target/surefire-reports"
    matches = [case for report in reports.glob("TEST-*.xml") for case in ET.parse(report).getroot().iter("testcase")
               if case.get("classname") == "com.game.authority.AuthorityHikariTiDbTest"
               and case.get("name") == "realHikariKeepsProtectedStandardLoopAndDatabaseLeastPrivilege"]
    if len(matches) != 1 or any(matches[0].find(kind) is not None for kind in ("error", "failure", "skipped")):
        raise RuntimeError("The real NORMAL authority TiDB test did not pass exactly once")
    return {"class": "com.game.authority.AuthorityHikariTiDbTest",
            "method": "realHikariKeepsProtectedStandardLoopAndDatabaseLeastPrivilege", "passed": True, "skipped": False}


def ps_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def maven_command(pwsh: str, maven: Path, version: str, commit: str,
                  protoc: Path, maven_repo: Path | None) -> list[str]:
    # 使用编码后的固定参数调用 .cmd，不把路径或参数拼入 cmd /c。
    arguments = ["-f", "java/pom.xml", "-pl", "gateway_node", "-am", "-o", "-B", "-T1", "clean", "package", "-DforkCount=1",
                 "-DreuseForks=true", f"-Dmmorpg.build.version={version}",
                 f"-Dmmorpg.build.commit={commit}", "-Dprotoc.spec=" + protoc.as_uri()]
    if maven_repo is not None:
        arguments.append("-Dmaven.repo.local=" + str(maven_repo))
    script = ("$ErrorActionPreference='Stop'; "
              "$PSNativeCommandUseErrorActionPreference=$false; & "
              + ps_literal(str(maven)) + " "
              + " ".join(ps_literal(argument) for argument in arguments)
              + "; exit $LASTEXITCODE")
    encoded = base64.b64encode(script.encode("utf-16-le")).decode("ascii")
    return [pwsh, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded]


def check_line_tables(javap: Path, snapshot: Path, jar: Path,
                      output: Path, env: dict[str, str]) -> None:
    classes = snapshot / "target/classes"
    with zipfile.ZipFile(jar) as archive:
        for class_name in BREAKPOINT_CLASSES:
            relative = class_name.replace(".", "/") + ".class"
            expected = (classes / relative).read_bytes()
            if archive.read("BOOT-INF/classes/" + relative) != expected:
                raise RuntimeError("Packaged breakpoint class differs from fresh target/classes")
    log = output / "javap.log"
    with log.open("x", encoding="utf-8", newline="\n") as target:
        for class_name in BREAKPOINT_CLASSES:
            completed = subprocess.run(
                [str(javap), "-classpath", str(classes), "-v", "-l", class_name],
                cwd=snapshot, env=env, capture_output=True, text=True,
                encoding="utf-8", errors="replace", timeout=30, check=False,
            )
            target.write(completed.stdout)
            target.write(completed.stderr)
            if completed.returncode or "LineNumberTable:" not in completed.stdout:
                raise RuntimeError("Breakpoint class has no usable line table; inspect javap.log")
            if 'SourceFile: "' not in completed.stdout:
                raise RuntimeError("Breakpoint class has no source file attribute; inspect javap.log")


def build(args: argparse.Namespace, output: Path,
          result: dict[str, object], details: dict[str, object]) -> None:
    protoc = args.protoc.resolve()
    if not re.fullmatch(r"[0-9a-fA-F]{64}", args.protoc_sha256) or not protoc.is_file() or sha256(protoc) != args.protoc_sha256.lower():
        raise RuntimeError("Explicit protoc executable SHA256 does not match")
    maven_repo = args.maven_repo.resolve() if args.maven_repo else None
    if maven_repo is not None and not maven_repo.is_dir():
        raise RuntimeError("Explicit private Maven repository must already exist")
    fixture_names = ("MMORPG_AUTHORITY_IT_JDBC_URL", "MMORPG_SQL_USERNAME", "MMORPG_SQL_PASSWORD",
                     "MMORPG_SQL_WRITER_GRANT_FILE", "MMORPG_SQL_WRITER_GRANT_SHA256", "MMORPG_SQL_DEPLOYMENT_ID",
                     "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256", "MMORPG_SQL_WRITER_CONSUME_DIR")
    if os.name == "nt":
        fixture_names += ("MMORPG_SQL_PRIVATE_FILE_HELPER", "MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256")
    if any(not os.environ.get(name) for name in fixture_names):
        raise RuntimeError("A provisioned real NORMAL authority TiDB fixture is required; no fixture is created by this builder")
    with args.base_images.resolve().open(encoding="utf-8-sig") as source:
        bases = json.load(source)
    if not isinstance(bases, dict) or not all(
        isinstance(key, str) and isinstance(value, str) and value.strip()
        for key, value in bases.items()
    ) or "gateway" not in bases:
        raise RuntimeError("Base image mapping must contain the gateway service")
    java = args.jdk.resolve() / "bin/java.exe"
    javap = args.jdk.resolve() / "bin/javap.exe"
    maven = args.maven.resolve()
    if not all(path.is_file() for path in (java, javap, maven)):
        raise RuntimeError("Existing JDK23 or Maven3.9.9 tools are unavailable")
    pwsh = shutil.which("pwsh")
    docker = shutil.which("docker")
    if not pwsh or not docker:
        raise RuntimeError("Existing PowerShell7 and Docker CLI are required")
    docker_command = [docker, "--context", args.docker_context]
    base_id = capture(docker_command + ["image", "inspect", "--format", "{{.Id}}", bases["gateway"]])
    if not base_id.startswith("sha256:") or len(base_id) != 71:
        raise RuntimeError("Gateway base image ID is invalid")
    base_user = capture(docker_command + ["image", "inspect", "--format", "{{.Config.User}}", base_id])
    base_entrypoint = json.loads(capture(docker_command + [
        "image", "inspect", "--format", "{{json .Config.Entrypoint}}", base_id,
    ]))
    if base_user != "10001:10001" or base_entrypoint != ["java", "-jar", "/app/service.jar"]:
        raise RuntimeError("Gateway base user or JVM entrypoint contract mismatch")
    commit = capture(["git", "rev-parse", "HEAD"])
    dirty = bool(capture(["git", "status", "--porcelain"]))
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ").lower()
    version = f"debug-{stamp}-{uuid.uuid4().hex[:8]}"
    image = f"local/mmorpg-gateway:{version}"
    details.update(baseImage=bases["gateway"], baseImageId=base_id,
                   image=image, commit=commit, dirty=dirty, version=version)

    before = source_files()
    source_hash = hashlib.sha256(json.dumps(before, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()
    revision = commit + ("-dirty" if dirty else "") + "-source." + source_hash[:12]
    details.update(sourceSHA256=source_hash, revision=revision)
    write_json(output / "source-before.json", before)
    snapshot, tool_directory = create_workspace(output, details)
    for relative in before:
        source = REPO_ROOT / relative
        target = snapshot / source.relative_to(REPO_ROOT)
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
    assert_inputs(before, snapshot)
    details["sourceCount"] = len(before)

    # 仅拷贝现成 protoc 与相邻 DLL；不下载、安装或借用未钉摘要的编译器。
    tool_inputs = [protoc, *sorted(protoc.parent.glob("*.dll"))]
    tool_hashes = {str(path): sha256(path) for path in tool_inputs}
    for path in tool_inputs:
        shutil.copy2(path, tool_directory / path.name)
        if sha256(tool_directory / path.name) != tool_hashes[str(path)]:
            raise RuntimeError("Copied protoc toolchain changed")
    sealed_protoc = tool_directory / protoc.name
    details["protoc"] = {"sha256": args.protoc_sha256.lower(), "source": str(protoc), "toolchainFiles": tool_hashes}

    env = os.environ.copy()
    env["JAVA_HOME"] = str(args.jdk.resolve())
    env["PATH"] = str(java.parent) + os.pathsep + str(tool_directory) + os.pathsep + env.get("PATH", "")
    env["MAVEN_OPTS"] = "-Xmx768m -XX:ActiveProcessorCount=1"
    # 同样约束 Surefire 子 JVM；只修改子进程环境，不修改宿主环境。
    env["JAVA_TOOL_OPTIONS"] = "-Xmx768m -XX:ActiveProcessorCount=1"
    env.pop("MAVEN_ARGS", None)
    env.pop("JDK_JAVA_OPTIONS", None)
    env.pop("_JAVA_OPTIONS", None)
    command = maven_command(pwsh, maven, version, revision, sealed_protoc, maven_repo)
    maven_error: Exception | None = None
    try:
        logged(command, output / "maven.log", cwd=snapshot, env=env, timeout=1800)
    except Exception as error:
        maven_error = error
    try:
        summary = test_summary(snapshot)
    except Exception:
        # Maven 可能在生成 XML 前失败，保留原构建错误，避免被报告缺失覆盖。
        if maven_error:
            raise maven_error
        raise
    write_json(output / "test-summary.json", summary)
    details["tests"] = summary
    if maven_error:
        raise maven_error
    if summary["tests"] == 0 or summary["failures"] or summary["errors"] or summary["unexpectedSkipped"]:
        raise RuntimeError("Maven test reports are empty or contain failures, errors or unexpected skips")
    authority_test = authority_test_evidence(snapshot)
    write_json(output / "authority-tidb-test.json", authority_test)
    assert_inputs(before, snapshot)
    gateway_snapshot = snapshot / "java/gateway_node"
    jars = sorted((gateway_snapshot / "target").glob("*.jar"))
    if len(jars) != 1:
        raise RuntimeError("Exactly one fresh executable gateway JAR is required")
    binary = output / "binary"
    binary.mkdir()
    jar = binary / "service.jar"
    shutil.copy2(jars[0], jar)
    jar_hash = sha256(jar)
    if jar_hash != sha256(jars[0]):
        raise RuntimeError("Delivered JAR hash differs from the fresh Maven artifact")
    check_line_tables(javap, gateway_snapshot, jar, output, env)

    # 用同一个封存 protoc 对同一份闭包重新生成，核 Maven descriptor 与最终嵌套 JAR。
    descriptor_reference = output / "authority-reference.desc"
    logged([str(sealed_protoc), "-I.", "-Ithird_party/protobuf/src", "--include_imports",
            "--descriptor_set_out=" + str(descriptor_reference), PROTO_ROOT],
           output / "descriptor-verify.log", cwd=snapshot, env=env, timeout=60)
    descriptor = snapshot / "java/authority_jdbc/target/generated-resources/sql-authority/route-authority.desc"
    reference_bytes = descriptor_reference.read_bytes()
    if not reference_bytes or descriptor.read_bytes() != reference_bytes:
        raise RuntimeError("Maven authority descriptor differs from the pinned protoc result")
    authority_jars = sorted((snapshot / "java/authority_jdbc/target").glob("*.jar"))
    if len(authority_jars) != 1:
        raise RuntimeError("Exactly one fresh authority JAR is required")
    import io
    with zipfile.ZipFile(jar) as archive:
        embedded = [name for name in archive.namelist() if name.startswith("BOOT-INF/lib/authority-jdbc-") and name.endswith(".jar")]
        if len(embedded) != 1 or archive.read(embedded[0]) != authority_jars[0].read_bytes():
            raise RuntimeError("Gateway does not embed the fresh reactor authority JAR")
        with zipfile.ZipFile(io.BytesIO(archive.read(embedded[0]))) as authority_archive:
            if authority_archive.read(DESCRIPTOR_RESOURCE) != reference_bytes:
                raise RuntimeError("Packaged authority descriptor differs from the verified fresh descriptor")
    descriptor_evidence = {"sha256": sha256(descriptor), "authorityJarSha256": sha256(authority_jars[0]),
        "protoInputs": {name: before[path.relative_to(snapshot).as_posix()] for name, path in proto_inputs(snapshot).items()},
        "protocSha256": args.protoc_sha256.lower()}
    write_json(output / "authority-descriptor.json", descriptor_evidence)

    source_meta = output / "source_meta"
    source_meta.mkdir()
    built_utc = utc_now()
    build_info = source_meta / "BUILD_INFO"
    build_info.write_text(
        f"version={version}\ncommit={revision}\nbuilt_at={built_utc}\nsource_sha256={source_hash}\n",
        encoding="utf-8", newline="\n",
    )
    manifest = source_meta / "source-manifest.json"
    manifest_value = dict(
        schema=1, language="java", sourceRoot=str(REPO_ROOT), files=before,
        artifacts={"/app/service.jar": jar_hash, "/app/BUILD_INFO": sha256(build_info)},
        build=dict(version=version, commit=commit, dirty=dirty,
                   revision=revision, sourceSHA256=source_hash,
                   startedUtc=details["startedUtc"], builtUtc=built_utc,
                   baseImageId=base_id, tests=summary,
                   maven=MAVEN_PROFILE, maxHeapMiB=768, sourceLayout="repository-reactor-v1",
                   reactorModules=list(REACTOR_MODULES), authorityTiDbTest=authority_test,
                   authorityDescriptor=descriptor_evidence, protoc=details["protoc"]),
    )
    write_json(manifest, manifest_value)
    result["manifestPaths"] = {"gateway": str(manifest)}
    recipe = REPO_ROOT / "deploy/debug/Dockerfile.java"
    shutil.copy2(recipe, output / "Dockerfile.java")
    # BuildKit 的 FROM 不接受裸 config ID，独占本地标签只指向已解析的基础镜像。
    pinned = "local/mmorpg-gateway-runtime:pin-" + uuid.uuid4().hex
    capture(docker_command + ["tag", base_id, pinned])
    details["pinnedBaseImage"] = pinned
    if capture(docker_command + ["image", "inspect", "--format", "{{.Id}}", pinned]) != base_id:
        raise RuntimeError("Pinned base image tag does not match its resolved ID")
    build_args = docker_command + [
        "build", "--pull=false", "--progress=plain", "--file", str(output / "Dockerfile.java"),
        "--tag", image, "--build-arg", f"GATEWAY_IMAGE={pinned}",
        "--label", f"org.opencontainers.image.version={version}",
        "--label", f"org.opencontainers.image.revision={revision}",
        "--label", f"org.opencontainers.image.created={utc_now()}",
        "--label", "org.opencontainers.image.title=mmorpg-gateway", str(output),
    ]
    # Docker 上下文只保留新 JAR 与收据，避免上传 Maven 日志、源码快照和缓存。
    (output / ".dockerignore").write_text(
        "*\n!binary/\nbinary/*\n!binary/service.jar\n"
        "!source_meta/\nsource_meta/*\n!source_meta/source-manifest.json\n!source_meta/BUILD_INFO\n",
        encoding="utf-8", newline="\n",
    )
    assert_inputs(before, snapshot)
    if any(sha256(Path(path)) != value or sha256(tool_directory / Path(path).name) != value for path, value in tool_hashes.items()):
        raise RuntimeError("Pinned protoc toolchain changed during the build")
    logged(build_args, output / "image.log", cwd=REPO_ROOT, env=os.environ.copy(), timeout=600)
    if capture(docker_command + ["image", "inspect", "--format", "{{.Id}}", pinned]) != base_id:
        raise RuntimeError("Pinned base image changed during packaging")
    assert_inputs(before, snapshot)
    if (sha256(jar) != jar_hash
            or sha256(build_info) != manifest_value["artifacts"]["/app/BUILD_INFO"]
            or json.loads(manifest.read_text(encoding="utf-8")) != manifest_value):
        raise RuntimeError("JAR or source receipt changed during packaging")
    write_json(output / "source-after.json", source_files())
    image_id = capture(docker_command + ["image", "inspect", "--format", "{{.Id}}", image])
    if not image_id.startswith("sha256:") or len(image_id) != 71:
        raise RuntimeError("Gateway debug image ID could not be verified")
    details.update(jarSHA256=jar_hash, imageId=image_id)
    result["images"] = {"gateway": {"image": image, "imageId": image_id}}
    result["success"] = True


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8")
    parser = argparse.ArgumentParser(description="Build a fresh gateway and authority reactor with all default tests")
    parser.add_argument("--protoc", type=Path, help="Existing protoc executable; supply together with --protoc-sha256")
    parser.add_argument("--protoc-sha256", help="Explicit expected SHA256 of protoc")
    parser.add_argument("--maven-repo", type=Path, help="Existing private Maven repository for offline resolution")
    parser.add_argument("--output", required=True, type=Path, help="Exclusive new evidence directory")
    parser.add_argument("--base-images", required=True, type=Path, help="Service-to-image JSON including gateway")
    parser.add_argument("--jdk", type=Path, default=DEFAULT_JDK, help="Existing JDK23 directory")
    parser.add_argument("--maven", type=Path, default=DEFAULT_MAVEN, help="Existing Maven3.9.9 mvn.cmd")
    parser.add_argument("--docker-context", default="desktop-linux")
    args = parser.parse_args()
    if (args.protoc is None) != (args.protoc_sha256 is None):
        parser.error("--protoc and --protoc-sha256 must be supplied together")
    if args.protoc is None:
        args.protoc, args.protoc_sha256 = DEFAULT_PROTOC, DEFAULT_PROTOC_SHA256
    output = args.output.resolve()
    try:
        output.mkdir(parents=True, exist_ok=False)
    except FileExistsError:
        print("Output directory already exists; refusing overwrite", file=sys.stderr)
        return 2
    result: dict[str, object] = dict(success=False, images={}, manifestPaths={})
    details: dict[str, object] = dict(startedUtc=utc_now())
    try:
        build(args, output, result, details)
    except Exception as error:
        details["errorType"] = type(error).__name__
        details["error"] = str(error) if isinstance(error, RuntimeError) else "Build failed; raw exception details suppressed"
        print(f"Java gateway build failed; evidence directory: {output}", file=sys.stderr)
    finally:
        details["endedUtc"] = utc_now()
        write_json(output / "build-details.json", details)
        write_json(output / "result.json", result)
    if result["success"]:
        unverified = len(details["tests"]["unverifiedExternalTests"])
        print(f"Java gateway build passed; optional external tests unverified: {unverified}; result: {output / 'result.json'}")
        return 0
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
