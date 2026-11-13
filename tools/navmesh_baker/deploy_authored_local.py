#!/usr/bin/env python3
"""只更新已验证手绘地图的本机六个消费者；默认预检，不启动容器。"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tools/scripts"))
import rebuild_docker_debug as rebuild

CONSUMERS = ("gate", "scene", "battle", "login", "scene-manager", "player-locator")
TARGETS = set(CONSUMERS) | {"login-sql-boot"}
PHASES = (rebuild.CPP, ("login-sql-boot", "scene-manager", "player-locator", "login"))
OUTSIDE_CONTAINERS = ("rea-dkbi-existing-project",)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def read(path):
    return json.loads(path.read_text("utf-8-sig"))


def write(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def compose(overlay):
    return rebuild.COMPOSE + ["-f", str(overlay)]


def snapshot(overlay):
    ids = rebuild.execute(compose(overlay) + ["ps", "--all", "-q"]).splitlines()
    require(ids, "Existing canonical Compose project is empty")
    # 只读取白名单身份字段；绝不保存或打印 Docker Env、完整 inspect 或 Compose 展开内容。
    template = ('{"id":{{json .Id}},"name":{{json .Name}},"imageId":{{json .Image}},'
                '"service":{{json (index .Config.Labels "com.docker.compose.service")}},'
                '"project":{{json (index .Config.Labels "com.docker.compose.project")}},'
                '"oneoff":{{json (index .Config.Labels "com.docker.compose.oneoff")}},'
                '"number":{{json (index .Config.Labels "com.docker.compose.container-number")}},'
                '"workingDir":{{json (index .Config.Labels "com.docker.compose.project.working_dir")}},'
                '"configFiles":{{json (index .Config.Labels "com.docker.compose.project.config_files")}},'
                '"configHash":{{json (index .Config.Labels "com.docker.compose.config-hash")}},'
                '"running":{{json .State.Running}},"startedAt":{{json .State.StartedAt}},'
                '"hostname":{{json .Config.Hostname}}}')
    rows = [json.loads(line) for line in rebuild.execute(rebuild.DOCKER + ["inspect", "--format", template, *ids]).splitlines()]
    return partition_snapshot(rows, overlay)


def partition_snapshot(rows, overlay):
    expected_configs = {(ROOT / "deploy/debug/compose.game.yml").resolve(), (ROOT / "deploy/debug/compose.debug.yml").resolve()}
    result = {}
    for row in rows:
        configs = {Path(p).resolve() for p in row["configFiles"].split(",")}
        require(Path(row["workingDir"]).resolve() == (ROOT / "deploy/debug").resolve() and
                configs in (expected_configs, expected_configs | {overlay.resolve()}) and
                re.fullmatch(r"[a-f0-9]{64}", row["configHash"]), "Container does not belong to the canonical Compose configuration")
        if row["oneoff"] == "True":
            require(row["running"] is False, "Running one-off container requires coordination")
            key = "history:" + row["id"]
        else:
            require(row["oneoff"] == "False" and row["number"] == "1", "Canonical service container identity is ambiguous")
            key = row["service"]
        require(key not in result, "Duplicate canonical service containers in project")
        result[key] = row
    return result


def outside_snapshot():
    result, ids = {}, []
    for name in OUTSIDE_CONTAINERS:
        matches = rebuild.execute(rebuild.DOCKER + ["ps", "--all", "--quiet", "--filter", "name=^/" + name + "$"]).splitlines()
        require(len(matches) <= 1, "Duplicate exact outside-project container name")
        result[name] = dict(name=name, exists=bool(matches))
        ids.extend(matches)
    if not ids:
        return result
    template = ('{"id":{{json .Id}},"name":{{json .Name}},"imageId":{{json .Image}},'
                '"running":{{json .State.Running}},"startedAt":{{json .State.StartedAt}}}')
    rows = [json.loads(line) for line in rebuild.execute(rebuild.DOCKER + ["inspect", "--format", template, *ids]).splitlines()]
    require(len(rows) == len(ids), "Outside-project container identity is incomplete")
    for row in rows:
        name = row["name"].lstrip("/")
        require(name in result and result[name]["exists"], "Unexpected outside-project container identity")
        result[name] = dict(row, exists=True)
    return result


def validate_overlay(path, data):
    require(rebuild.digest(path) == data["composeOverlay"]["sha256"] and
            path.resolve() == Path(data["composeOverlay"]["path"]).resolve(), "Compose overlay binding changed")
    overlay = read(path)
    require(set(overlay) == {"services"} and set(overlay["services"]) == set(CONSUMERS), "Overlay service scope changed")
    navigation = (ROOT / "data/scene_nav_bin/authored_maps").resolve()
    require(Path(data["navigationDirectory"]).resolve() == navigation, "Unexpected authored navigation directory")
    source = data["navigationReceipt"]
    require(rebuild.digest(Path(source["path"])) == source["sha256"], "Installed navigation receipt changed")
    require(read(Path(source["path"])).get("installed") is True, "Authored navigation is not installed")
    nav_files = data["navigationSha256"]
    require(len(nav_files) == 16 and all(re.fullmatch(
        r"(?:26000|26008)/(?:scene\.bin(?:\.authored|\.sha256)?|world-y-up\.obj|navigation\.json|source\.json|server-bake-args\.json|delivery-source\.json)", name)
        for name in nav_files), "Unexpected authored navigation snapshot")
    require({p.relative_to(navigation).as_posix() for p in navigation.rglob("*") if p.is_file()} == set(nav_files),
            "Unrecorded authored navigation file")
    require(all(rebuild.digest(navigation / name) == sha for name, sha in nav_files.items()), "Installed navigation SHA changed")
    for service in CONSUMERS:
        expected = [dict(type="bind", source=Path(data["tableDirectory"]).as_posix(),
                         target=rebuild.TABLE_TARGETS[service], read_only=True)]
        if service in rebuild.CPP:
            expected.append(dict(type="bind", source=navigation.as_posix(),
                                 target="/app/data/scene_nav_bin/authored_maps", read_only=True))
        require(overlay["services"][service] == dict(volumes=expected), "Overlay contains unauthorized service changes")


def validate_build(path, data):
    require(path.resolve().is_relative_to((ROOT / "build").resolve()), "Build receipt must be below repository build/")
    build = read(path)
    require(build.get("success") is True and set(build.get("images", {})) == set(rebuild.CPP), "C++ build did not succeed as one batch")
    image = build["images"]["scene"]
    require(all(build["images"][name] == image for name in rebuild.CPP), "C++ nodes have different build identities")
    actual = rebuild.execute(rebuild.DOCKER + ["image", "inspect", "--format", "{{.Id}}", image["image"]]).strip()
    require(actual == image["imageId"], "Built image tag identity changed")
    manifests = {Path(build["manifestPaths"][name]).resolve() for name in rebuild.CPP}
    require(len(manifests) == 1, "C++ source manifests do not describe one batch")
    manifest_path = manifests.pop()
    require(manifest_path.is_relative_to(path.resolve().parent), "Build source receipt is outside the build batch")
    manifest = read(manifest_path)
    files = manifest["files"]
    require(manifest.get("schema") == 1 and manifest.get("language") == "cpp" and files, "Invalid C++ source receipt")
    aggregate = hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest()
    require(aggregate == manifest["build"]["sourceSha256"], "C++ frozen source receipt changed")
    require(rebuild.current_sources("cpp", "scene") == files, "Local C++ build inputs changed after frozen build")
    for scene in (26000, 26008):
        for name in ("scene.bin", "scene.bin.authored", "scene.bin.sha256"):
            relative = f"data/scene_nav_bin/authored_maps/{scene}/{name}"
            require(files.get(relative) == data["navigationSha256"][f"{scene}/{name}"], "Built image omits current authored navigation")
    return build, dict(path=str(path.resolve()), sha256=rebuild.digest(path), image=image,
                       sourceManifestPath=str(manifest_path), sourceManifestSha256=rebuild.digest(manifest_path), sourceSha256=aggregate)


def preflight(build_path, manifest_path, overlay):
    env_sha = rebuild.digest(rebuild.STATE / ".env")
    data = rebuild.load_runtime_data_manifest(manifest_path)
    require(set(data["services"]) == set(CONSUMERS), "Runtime manifest must name exactly the six table consumers")
    validate_overlay(overlay, data)
    rebuild.check_config_inputs(list(TARGETS))
    print("Validating frozen C++ image, source inputs and authored navigation", flush=True)
    build, frozen = validate_build(build_path, data)
    before = snapshot(overlay)
    require(TARGETS.issubset(before) and all(before[n]["running"] for n in CONSUMERS), "Expected existing six running consumers and login boot")
    require(len({r["project"] for r in before.values()}) == 1 and next(iter(before.values()))["project"] == "xuanming-debug-current",
            "Deployment only supports the existing canonical local project")
    config = json.loads(rebuild.execute(compose(overlay) + ["config", "--format", "json"]))
    reused = {}
    for name in TARGETS - set(rebuild.CPP):
        image = config["services"][name]["image"]
        identity = rebuild.execute(rebuild.DOCKER + ["image", "inspect", "--format", "{{.Id}}", image]).strip()
        require(identity == before[name]["imageId"], "Supporting image/config differs from existing container: " + name)
        reused[name] = dict(image=image, imageId=identity)
    print("Validating reused Go source and compiled artifacts before deployment", flush=True)
    require(rebuild.check(["login", "scene-manager", "player-locator"]),
            "Existing Go source/image artifacts do not match; deployment has not started")
    require(rebuild.digest(rebuild.STATE / ".env") == env_sha, "Canonical configuration changed during preflight")
    return build, data, before, dict(developmentOnly=True, productionTravelEnabled=False, build=frozen,
        runtimeDataManifest=dict(path=str(manifest_path.resolve()), sha256=rebuild.digest(manifest_path)),
        overlay=dict(path=str(overlay.resolve()), sha256=rebuild.digest(overlay)), reusedImages=reused,
        envSha256=env_sha, phases=[list(p) for p in PHASES])


def run_up(overlay, services, log):
    require(tuple(services) in PHASES, "Unauthorized recreation phase")
    command = compose(overlay) + ["up", "-d", "--no-build", "--pull", "never", "--no-deps",
        "--force-recreate", "--wait", "--wait-timeout", "180", *services]
    with log.open("x", encoding="utf-8") as stream:
        result = subprocess.run(command, cwd=ROOT, stdout=stream, stderr=subprocess.STDOUT, timeout=240)
    require(result.returncode == 0, "Compose recreation failed; inspect the phase log; no automatic retry")


def verify_navigation_runtime(data, after):
    source = Path(data["navigationDirectory"]).resolve()
    target = "/app/data/scene_nav_bin/authored_maps"
    expected = {target + "/" + name: sha for name, sha in data["navigationSha256"].items()}
    for service in rebuild.CPP:
        container = after[service]["id"]
        mounts = json.loads(rebuild.execute(rebuild.DOCKER + ["inspect", "--format", "{{json .Mounts}}", container]))
        matches = [item for item in mounts if item.get("Destination") == target]
        require(len(matches) == 1 and matches[0].get("Type") == "bind" and matches[0].get("RW") is False and
                rebuild.normalized_bind_source(matches[0].get("Source", "")) == rebuild.normalized_bind_source(str(source)),
                "Runtime authored navigation is not the recorded read-only bind: " + service)
        output = rebuild.execute(rebuild.DOCKER + ["exec", container, "sha256sum", "--", *expected]).splitlines()
        actual = {line[66:]: line[:64] for line in output if len(line) > 66}
        require(actual == expected, "Runtime authored navigation/source SHA mismatch: " + service)


def scene_navigation_log(overlay, evidence):
    scene = snapshot(overlay)["scene"]
    # Muduo 信息日志写卷内文件；只读新容器 hostname 对应且不早于本次启动的文件，不能拿旧卷日志冒充。
    started = datetime.fromisoformat(scene["startedAt"].replace("Z", "+00:00")).timestamp()
    deadline, lines, sources = time.monotonic() + 45, [], []
    while True:
        result = subprocess.run(rebuild.DOCKER + ["logs", "--since", scene["startedAt"], scene["id"]],
                                cwd=ROOT, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=20)
        require(result.returncode == 0, "Cannot read new scene container logs")
        content = result.stdout + result.stderr
        files = rebuild.execute(rebuild.DOCKER + ["exec", scene["id"], "find", "/app/bin/logs/cpp_nodes",
            "-maxdepth", "1", "-type", "f", "-printf", "%p\n"]).splitlines()
        sources = []
        for path in files:
            match = re.fullmatch(r"/app/bin/logs/cpp_nodes/scene\.(\d{8}-\d{6})\." + re.escape(scene["hostname"]) + r"\.\d+\.log", path)
            if match and datetime.strptime(match[1], "%Y%m%d-%H%M%S").replace(tzinfo=timezone.utc).timestamp() >= started - 2:
                sources.append(path)
                content += rebuild.execute(rebuild.DOCKER + ["exec", scene["id"], "tail", "-c", "2000000", path])
        lines = [line for line in content.splitlines() if re.search(r"nav registered for scene (26000|26008):", line)]
        if all(any(f"nav registered for scene {value}:" in line for line in lines) for value in (26000, 26008)) or time.monotonic() >= deadline:
            break
        time.sleep(1)
    (evidence / "scene-authored-navigation.log").write_text("\n".join(lines) + "\n", encoding="utf-8")
    write(evidence / "scene-log-provenance.json", dict(containerId=scene["id"], startedAt=scene["startedAt"],
          hostname=scene["hostname"], files=sources, stdoutSince=scene["startedAt"]))
    require(all(any(f"nav registered for scene {scene}:" in line for line in lines) for scene in (26000, 26008)),
            "New scene startup logs do not confirm both authored maps; preserve the scene and inspect logs")


def deploy(build_path, manifest_path, overlay, evidence, execute=False):
    evidence = evidence.resolve()
    require(evidence.is_relative_to((ROOT / "build").resolve()) and evidence != ROOT / "build" and not evidence.exists(),
            "Choose a new evidence directory below repository build/")
    subprocess.run(["git", "check-ignore", "--quiet", str(evidence / "result.json")], cwd=ROOT, check=True)
    evidence.mkdir(parents=True)
    result = dict(success=False, executeRequested=execute, configurationUpdated=False, completedPhases=[], attemptedPhases=[], containerUpdateStarted=False)
    try:
        build, data, before, frozen = preflight(build_path, manifest_path, overlay)
        write(evidence / "preflight.json", frozen)
        write(evidence / "containers-before.json", before)
        outside_before = outside_snapshot()
        write(evidence / "outside-containers-before.json", outside_before)
        if not execute:
            result.update(success=True, status="PREFLIGHT_ONLY")
            return result
        # 备份只进入忽略证据目录。CAS 之前再次确认现场与预检一致，不覆盖其它窗口改动。
        env = rebuild.STATE / ".env"
        require(rebuild.digest(env) == frozen["envSha256"] and snapshot(overlay) == before, "Configuration or containers changed during preflight")
        require(outside_snapshot() == outside_before, "Outside-project container changed during preflight")
        require(rebuild.digest(manifest_path) == frozen["runtimeDataManifest"]["sha256"] and
                rebuild.digest(overlay) == frozen["overlay"]["sha256"] and
                rebuild.digest(build_path) == frozen["build"]["sha256"] and
                rebuild.digest(Path(frozen["build"]["sourceManifestPath"])) == frozen["build"]["sourceManifestSha256"],
                "Frozen deployment receipts changed")
        (evidence / ".env.before.private").write_bytes(env.read_bytes())
        rebuild.update_images(build["images"])
        result["configurationUpdated"] = True
        result["envAfterSha256"] = rebuild.digest(env)
        # 旧 scene-manager 此时只认识旧 World；先让新 C++ 装好两图导航，再让新 World 建场。
        for index, phase in enumerate(PHASES):
            require(rebuild.digest(env) == result["envAfterSha256"], "Canonical configuration changed before recreation phase")
            print("Recreating phase: " + ",".join(phase), flush=True)
            result["containerUpdateStarted"] = True
            result["attemptedPhases"].append(list(phase))
            run_up(overlay, phase, evidence / f"phase-{index + 1}.log")
            result["completedPhases"].append(list(phase))
            if index == 0:
                scene_navigation_log(overlay, evidence)
        after = snapshot(overlay)
        write(evidence / "containers-after.json", after)
        outside_after = outside_snapshot()
        write(evidence / "outside-containers-after.json", outside_after)
        require(outside_after == outside_before, "Outside-project container identity changed during deployment")
        require(set(after) == set(before), "Compose project service set changed unexpectedly")
        for name in before:
            if name not in TARGETS:
                require(before[name] == after[name], "Unrelated container identity changed: " + name)
            else:
                require(before[name]["id"] != after[name]["id"], "Target service was not recreated: " + name)
                expected = build["images"][name]["imageId"] if name in rebuild.CPP else frozen["reusedImages"][name]["imageId"]
                require(after[name]["imageId"] == expected, "Unexpected runtime image: " + name)
        verify_navigation_runtime(data, after)
        require(rebuild.check(list(CONSUMERS), evidence / "source-and-runtime-data-check.json", manifest_path),
                "Source/runtime-data verification failed; preserve the deployment and inspect evidence")
        scene_navigation_log(overlay, evidence)
        result.update(success=True, status="DEPLOYED_AND_VERIFIED", beforeImages={n: r["imageId"] for n, r in before.items()},
                      afterImages={n: r["imageId"] for n, r in after.items()})
        return result
    except Exception as error:
        result.update(status="FAILED_PRESERVED", error=str(error), automaticRollback=False, automaticRetry=False)
        try:
            write(evidence / "containers-after-failure.json", snapshot(overlay))
            write(evidence / "outside-containers-after-failure.json", outside_snapshot())
        except Exception:
            pass
        raise
    finally:
        write(evidence / "result.json", result)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--build-result", required=True, type=Path)
    parser.add_argument("--runtime-data-manifest", required=True, type=Path)
    parser.add_argument("--overlay", required=True, type=Path)
    parser.add_argument("--evidence-dir", required=True, type=Path)
    parser.add_argument("--execute", action="store_true", help="完成预检后执行一次部署；失败不自动重试或回滚")
    args = parser.parse_args()
    result = deploy(args.build_result.resolve(), args.runtime_data_manifest.resolve(), args.overlay.resolve(), args.evidence_dir, args.execute)
    print(json.dumps(dict(success=result["success"], status=result["status"], evidence=str(args.evidence_dir.resolve()))))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print("Deployment stopped: " + str(error), file=sys.stderr)
        raise SystemExit(1)
