#!/usr/bin/env python3
"""核验手绘图片地图的同源导出，烘焙并安装本机开发导航；不启动容器。"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
MAPS = {"26000-mid-autumn": 26000, "26008-spring": 26008}
SOURCE_FILES = ("world-y-up.obj", "navigation.json", "source.json", "server-bake-args.json")
INSTALLED_FILES = SOURCE_FILES + ("scene.bin", "scene.bin.authored", "scene.bin.sha256", "delivery-source.json")
OPTION_KEYS = {"--obj", "--out", "--profile", "--cs", "--ch", "--agent-radius", "--agent-height",
               "--agent-climb", "--agent-slope", "--tile-size", "--min-region-area", "--filter-ledges", "--probe"}


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def document(path: Path):
    return json.loads(path.read_text(encoding="utf-8-sig"))


def finite(value, label: str) -> float:
    require(isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value),
            f"Invalid finite number: {label}")
    return float(value)


def near(a, b) -> bool:
    return math.isclose(float(a), float(b), rel_tol=1e-7, abs_tol=1e-7)


def contained(path: Path, parent: Path) -> Path:
    resolved = path.resolve()
    require(resolved.is_relative_to(parent.resolve()) and resolved != parent.resolve(),
            "Path escapes its owned directory")
    return resolved


def validate_package(export_root: Path, item: dict) -> dict:
    key = item.get("mapKey")
    require(key in MAPS, f"Unsupported authored map: {key}")
    scene = MAPS[key]
    for field in ("sceneConfigId", "sceneId"):
        if field in item:
            require(item[field] == scene, f"Scene identity mismatch: {key}")
    folder = contained(export_root / key, export_root)
    require(folder.is_dir(), f"Missing export directory: {key}")
    require(Path(item["exportDirectory"]).resolve() == folder and item.get("isBakeCurrent") is True,
            f"Delivery export identity or baked state mismatch: {key}")
    for name, field in (("world-y-up.obj", "objSha256"), ("navigation.json", "authoringJsonSha256"),
                        ("source.json", "sourceJsonSha256"), ("server-bake-args.json", "serverBakeArgumentsSha256")):
        require(digest(folder / name) == item.get(field), f"Export source SHA mismatch: {key}/{name}")
    source = document(folder / "source.json")
    for name, field in (("world-y-up.obj", "geometrySha256"), ("navigation.json", "authoringSha256"),
                        ("server-bake-args.json", "serverBakeArgumentsSha256")):
        require(digest(folder / name) == source.get(field), f"Export source SHA mismatch: {key}/{name}")
    nav = document(folder / "navigation.json")
    require(nav.get("format") == "hand-authored-navmesh-v2" and nav.get("mapKey") == key and
            source.get("mapKey") == key, f"Authoring identity mismatch: {key}")
    require(nav.get("spawnSet") is True, f"Missing explicit spawn: {key}")
    rect = nav["worldRect"]
    bounds = [finite(rect[name], f"{key}.worldRect.{name}") for name in ("x", "z", "width", "depth")]
    require(0 < bounds[2] <= 100000 and 0 < bounds[3] <= 100000, f"Invalid world bounds: {key}")
    spawn = nav["spawn"]
    require(isinstance(spawn, list) and len(spawn) == 3, f"Invalid spawn vector: {key}")
    spawn = [finite(value, f"{key}.spawn") for value in spawn]
    require(spawn[1] == 0 and bounds[0] <= spawn[0] <= bounds[0] + bounds[2] and
            bounds[1] <= spawn[2] <= bounds[1] + bounds[3], f"Spawn outside original ground bounds: {key}")
    contract = nav["bakeContract"]
    require(contract["revision"] == "authored-flat-v2" and contract["serverProfile"] == "authored-flat" and
            contract["tileSizeVoxels"] == 256 and contract["minRegionAreaWorld"] == 0 and
            contract["unity"]["heightMesh"] is True, f"Unsupported shared bake contract: {key}")
    require(source.get("agent") == nav["agent"] and source.get("bakeContract") == contract,
            f"Source parameter metadata mismatch: {key}")
    require(item["agent"] == nav["agent"] and item["bakeContract"] == contract and
            item["worldRect"] == rect and item["spawnUnity"] == spawn,
            f"Delivery parameter/spawn/bounds mismatch: {key}")
    args = document(folder / "server-bake-args.json")
    require(isinstance(args, list) and len(args) % 2 == 0 and all(isinstance(v, str) for v in args),
            f"Invalid baker argument array: {key}")
    options = dict(zip(args[::2], args[1::2]))
    require(len(options) * 2 == len(args) and set(options) == OPTION_KEYS,
            f"Unexpected or duplicate baker options: {key}")
    require(options["--obj"] == "world-y-up.obj" and options["--out"] == "scene-nav.bin" and
            options["--profile"] == "authored-flat" and options["--filter-ledges"] == "1",
            f"Unsafe output/input/profile options: {key}")
    for option, field in (("--cs", "voxelSize"), ("--agent-radius", "radius"), ("--agent-height", "height"),
                          ("--agent-climb", "climb"), ("--agent-slope", "slope")):
        actual = float(options[option])
        require(math.isfinite(actual) and near(actual, finite(nav["agent"][field], field)),
                f"Shared argument mismatch: {key}/{option}")
    require(float(options["--ch"]) == .2 and options["--tile-size"] == "256" and
            float(options["--min-region-area"]) == 0, f"Cell/tile/region mismatch: {key}")
    probe = options["--probe"].split(",")
    require(len(probe) == 3 and all(near(a, b) for a, b in zip(probe, spawn)),
            f"Original spawn probe changed: {key}")
    # OBJ 仅能包含已扣洞的水平地面；不得借助 profile 把三维建筑压平。
    vertices, faces = [], []
    for line in (folder / "world-y-up.obj").read_text(encoding="utf-8").splitlines():
        words = line.split()
        if not words or words[0].startswith("#"):
            continue
        if words[0] == "v":
            require(len(words) == 4, f"Invalid OBJ vertex: {key}")
            point = [float(value) for value in words[1:]]
            require(all(math.isfinite(v) for v in point) and point[1] == 0 and
                    bounds[0] - 1e-5 <= point[0] <= bounds[0] + bounds[2] + 1e-5 and
                    bounds[1] - 1e-5 <= point[2] <= bounds[1] + bounds[3] + 1e-5,
                    f"OBJ is not original flat ground inside world bounds: {key}")
            vertices.append(point)
        elif words[0] == "f":
            require(len(words) == 4, f"OBJ must contain triangles: {key}")
            faces.append([int(value) for value in words[1:]])
        else:
            raise ValueError(f"Unexpected OBJ record: {key}/{words[0]}")
    require(vertices and faces and all(0 < i <= len(vertices) for face in faces for i in face),
            f"Empty or invalid OBJ topology: {key}")
    return dict(mapKey=key, sceneConfigId=scene, folder=folder, navigation=nav, source=source,
                args=args, spawnUnity=spawn, bounds=bounds, delivery=item,
                sourceFileSha256={name: digest(folder / name) for name in SOURCE_FILES})


def load_delivery(export_root: Path) -> tuple[dict, list[dict]]:
    delivery = document(export_root / "delivery.json")
    items = delivery["maps"]
    require(isinstance(items, list) and len(items) == len(MAPS) and
            {item.get("mapKey") for item in items} == set(MAPS), "Expected exactly two explicitly supported maps")
    return delivery, [validate_package(export_root, item) for item in items]


def run(command: list[str], cwd: Path, log: Path, timeout: int = 180) -> str:
    result = subprocess.run(command, cwd=cwd, capture_output=True, text=True, encoding="utf-8", timeout=timeout)
    log.write_text(result.stdout + result.stderr, encoding="utf-8")
    require(result.returncode == 0, f"Command failed ({result.returncode}); see {log}")
    return result.stdout


def prepare(export_root: Path, baker: Path, query: Path, evidence: Path) -> dict:
    require(not evidence.exists(), "Evidence output already exists; choose a new directory")
    delivery, packages = load_delivery(export_root)
    evidence.mkdir(parents=True)
    receipt = dict(schemaVersion=1, status="DEVELOPMENT_AUTHORED_NAVIGATION", installed=False,
                   exportDeliverySha256=digest(export_root / "delivery.json"), bakerSha256=digest(baker),
                   querySha256=digest(query), maps=[])
    try:
        for package in packages:
            key, scene = package["mapKey"], package["sceneConfigId"]
            folder = evidence / "candidate" / str(scene)
            folder.mkdir(parents=True)
            for name in SOURCE_FILES:
                shutil.copyfile(package["folder"] / name, folder / name)
            run([str(baker), *package["args"]], folder, evidence / f"{scene}-bake.log")
            shutil.copyfile(folder / "scene-nav.bin", folder / "scene.bin")
            spawn = package["spawnUnity"]
            coordinates = ",".join(format(value, ".17g") for value in spawn)
            result = json.loads(run([str(query), "--nav", str(folder / "scene.bin"), "--point", coordinates],
                                    folder, evidence / f"{scene}-spawn-query.json"))
            point = result["points"][0]
            require(point["onMesh"] is True and all(abs(a - b) <= .02 for a, b in zip(point["nearest"], spawn)),
                    f"Original spawn is not on the final MSET: {key}")
            authored = "ANAV 1\n" + " ".join(format(value, ".17g") for value in package["bounds"] + [0.0]) + "\n"
            (folder / "scene.bin.authored").write_text(authored, encoding="ascii", newline="\n")
            nav_sha, bounds_sha = digest(folder / "scene.bin"), digest(folder / "scene.bin.authored")
            (folder / "scene.bin.sha256").write_text(nav_sha + "\n" + bounds_sha + "\n", encoding="ascii", newline="\n")
            (folder / "delivery-source.json").write_text(json.dumps(package["delivery"], ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
            receipt["maps"].append(dict(mapKey=key, sceneConfigId=scene,
                navBinFile=f"data/scene_nav_bin/authored_maps/{scene}/scene.bin",
                spawnUnity=spawn, spawnServer=[spawn[2], spawn[0], spawn[1]],
                worldRect=package["navigation"]["worldRect"], boundsConfirmedByAuthor=package["navigation"].get("boundsConfirmedByAuthor", False),
                sourceAsset=package["delivery"], sourceFileSha256=package["sourceFileSha256"],
                navigationSha256=nav_sha, boundsSha256=bounds_sha, spawnQuery=point,
                candidateDirectory=str(folder.resolve())))
        # 安装前再次核验导出输入，不能混入烘焙期间用户刚修改的第二版数据。
        _, current_packages = load_delivery(export_root)
        require({p["mapKey"]: p["sourceFileSha256"] for p in current_packages} ==
                {p["mapKey"]: p["sourceFileSha256"] for p in packages}, "Source files changed during bake")
        require(digest(export_root / "delivery.json") == receipt["exportDeliverySha256"], "Delivery changed during bake")
        receipt["result"] = "PASS"
    except Exception as error:
        receipt.update(result="FAIL", error=str(error))
        raise
    finally:
        (evidence / "navigation-install.json").write_text(json.dumps(receipt, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return receipt


def atomic_bytes(path: Path, data: bytes, owned_root: Path) -> None:
    contained(path, owned_root)
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(prefix=".authored-install-", dir=path.parent)
    try:
        with os.fdopen(handle, "wb") as stream:
            stream.write(data)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def install_navigation(receipt: dict, evidence: Path, output: Path) -> None:
    require(receipt["result"] == "PASS" and not receipt["installed"], "Candidate is not ready for installation")
    output = output.resolve()
    snapshots = {}
    for item in receipt["maps"]:
        candidate = contained(Path(item["candidateDirectory"]), evidence / "candidate")
        expected = dict(item["sourceFileSha256"], **{"scene.bin": item["navigationSha256"],
                        "scene.bin.authored": item["boundsSha256"]})
        require(all(digest(candidate / name) == value for name, value in expected.items()),
                "Candidate changed after validation")
        require((candidate / "scene.bin.sha256").read_text(encoding="ascii").split() ==
                [item["navigationSha256"], item["boundsSha256"]], "Candidate checksum changed")
        require(document(candidate / "delivery-source.json") == item["sourceAsset"],
                "Candidate source receipt changed")
        target = contained(output / str(item["sceneConfigId"]), output)
        require(not (target / "scene.bin.mask").exists(), "Authored navigation must not inherit a legacy mask")
        for name in INSTALLED_FILES:
            path = contained(target / name, output)
            snapshots[path] = path.read_bytes() if path.is_file() else None
    manifest_path = contained(output.parent / "authored_maps_20261112.json", output.parent)
    snapshots[manifest_path] = manifest_path.read_bytes() if manifest_path.is_file() else None
    backup = evidence / "previous-navigation"
    backup.mkdir()
    for index, (path, value) in enumerate(snapshots.items()):
        if value is not None:
            (backup / str(index)).write_bytes(value)
    (backup / "paths.json").write_text(json.dumps([str(path) for path in snapshots], indent=2), encoding="utf-8")
    written = []
    try:
        for item in receipt["maps"]:
            candidate = Path(item["candidateDirectory"])
            target = output / str(item["sceneConfigId"])
            for name in INSTALLED_FILES:
                destination = target / name
                atomic_bytes(destination, (candidate / name).read_bytes(), output)
                written.append(destination)
        receipt["installed"] = True
        encoded = (json.dumps(receipt, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
        atomic_bytes(manifest_path, encoded, output.parent)
        written.append(manifest_path)
    except Exception:
        receipt["installed"] = False
        for path in reversed(written):
            previous = snapshots[path]
            if previous is None:
                contained(path, output.parent).unlink()
            else:
                atomic_bytes(path, previous, output.parent)
        raise
    finally:
        (evidence / "navigation-install.json").write_text(json.dumps(receipt, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def table_rows(receipt: dict) -> dict:
    return {"BaseScene.xlsx": [[item["sceneConfigId"], item["navBinFile"], *item["spawnServer"]] for item in receipt["maps"]],
            "World.xlsx": [[item["sceneConfigId"], item["sceneConfigId"]] for item in receipt["maps"]]}


def export_tables(source_data: Path, output: Path, log: Path) -> Path:
    require(not output.exists(), "Table output already exists")
    helper = ROOT / "tools/data_table_exporter/tools/sandbox_export.py"
    spec = importlib.util.spec_from_file_location("authored_map_sandbox_export", helper)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    import yaml
    config = module.build_sandbox_config(output)
    values = yaml.safe_load(config.read_text(encoding="utf-8"))
    values["excel"]["data_dir"] = str(source_data.resolve())
    config.write_text(yaml.safe_dump(values, allow_unicode=True, sort_keys=False), encoding="utf-8")
    with log.open("w", encoding="utf-8") as stream:
        result = subprocess.run([sys.executable, "-X", "utf8", "-B", str(ROOT / "tools/data_table_exporter/run.py"), str(config)],
                                cwd=ROOT, stdout=stream, stderr=subprocess.STDOUT)
    require(result.returncode == 0, f"Official table exporter failed; see {log}")
    return output / "generated/tables"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--export-root", required=True, type=Path)
    parser.add_argument("--evidence-dir", required=True, type=Path)
    parser.add_argument("--baker", type=Path, default=ROOT / "tools/navmesh_baker/build/Release/navmesh_baker.exe")
    parser.add_argument("--query", type=Path, default=ROOT / "tools/navmesh_baker/build/Release/navmesh_query.exe")
    parser.add_argument("--install", action="store_true")
    args = parser.parse_args()
    evidence = args.evidence_dir.resolve()
    require(evidence.is_relative_to((ROOT / "build").resolve()) and evidence != ROOT / "build", "Evidence must stay below repository build/")
    receipt = prepare(args.export_root.resolve(), args.baker.resolve(), args.query.resolve(), evidence)
    (evidence / "table-rows.json").write_text(json.dumps(table_rows(receipt), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    if args.install:
        install_navigation(receipt, evidence, ROOT / "data/scene_nav_bin/authored_maps")
    print(json.dumps({"result": "PASS", "maps": len(receipt["maps"]), "installed": receipt["installed"], "evidence": str(evidence)}))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print(f"error: {error}", file=sys.stderr)
        raise SystemExit(1)
