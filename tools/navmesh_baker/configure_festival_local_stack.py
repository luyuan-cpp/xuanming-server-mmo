#!/usr/bin/env python3
"""将已验证的节庆表、导航和同批节点镜像绑定到独立本机 Compose 配置。"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import shutil
import subprocess
from pathlib import Path

from prepare_festival_test_tables import EXPECTED, REPO
from sync_festival_maps import HANDOFF_SHA


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def configure(run: Path, tables_report: Path, node_build: Path, image_env: Path) -> None:
    run = run.resolve()
    if not run.is_relative_to((REPO / "build").resolve()) or not (run / ".env").is_file():
        raise ValueError("Run directory must be a prepared private directory below build")
    values = {}
    for line in (run / ".env").read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            key, value = line.split("=", 1)
            values[key] = value
    if not values.get("COMPOSE_PROJECT_NAME", "").startswith("festival-maps-"):
        raise ValueError("Refusing to modify an existing non-festival stack")
    if Path(values.get("DEBUG_RUN_DIR", "")).resolve() != run:
        raise ValueError("Configuration directory must belong to this festival run")
    if values.get("DEBUG_DATA_VOLUME_PREFIX", values["COMPOSE_PROJECT_NAME"]) != values["COMPOSE_PROJECT_NAME"]:
        raise ValueError("Data volumes must belong to this festival project")
    subprocess.run(["git", "check-ignore", "--quiet", str(run / ".env")], cwd=REPO, check=True)
    # 只复用本机已构建镜像的引用；账号、口令、端口、网络和数据卷全部保持新栈自己的值。
    image_ids = {}
    for line in image_env.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            if key.endswith("_IMAGE") and key != "NODE_DEBUG_IMAGE":
                image_ids[key] = subprocess.check_output(
                    ["docker", "image", "inspect", "--format", "{{.Id}}", value], text=True).strip()
                values[key] = value
    if (run / "tables").exists() or (run / "navigation").exists():
        raise ValueError("Immutable runtime snapshot already exists; prepare a new run directory")
    tables = json.loads(tables_report.read_text(encoding="utf-8"))
    if not tables.get("developmentOnly") or tables.get("productionTravelEnabled"):
        raise ValueError("Only development table exports can be mounted")
    if tables.get("handoffSha256") != HANDOFF_SHA:
        raise ValueError("Table export frozen handoff mismatch")
    if len(tables["maps"]) != 9 or {m["paddedMapId"] for m in tables["maps"]} != set(EXPECTED):
        raise ValueError("Table export map set mismatch")
    if set(tables["tableSha256"]) != {"basescene.json", "basescene.pb", "world.json", "world.pb", "manifest.json"}:
        raise ValueError("Table export file set mismatch")
    table_dir = Path(tables["generatedTables"])
    for name, expected in tables["tableSha256"].items():
        if digest(table_dir / name) != expected:
            raise ValueError(f"Table output changed: {name}")
    table_manifest = json.loads((table_dir / "manifest.json").read_text(encoding="utf-8"))
    if table_manifest["table_count"] != len(table_manifest["tables"]):
        raise ValueError("Table manifest count mismatch")
    for table in table_manifest["tables"]:
        for artifact in table["artifacts"]:
            path = table_dir / artifact["file"]
            if (path.resolve().parent != table_dir.resolve() or path.stat().st_size != artifact["size"]
                    or digest(path) != artifact["sha256"]):
                raise ValueError(f"Table manifest artifact mismatch: {table['name']}")
    build = json.loads(node_build.read_text(encoding="utf-8"))
    if build.get("success") is not True:
        raise ValueError("Node build did not succeed")
    images = build["images"]
    image = images["scene"]["image"]
    image_id = images["scene"]["imageId"]
    if any(images[k] != images["scene"] for k in ("gate", "battle")):
        raise ValueError("Three node images must belong to the same build")
    actual = subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", image], text=True).strip()
    if actual != image_id:
        raise ValueError("Built node image identity changed")
    nav_root = REPO / "data/scene_nav_bin"
    batch = json.loads((nav_root / "festival_maps_20261109.json").read_text(encoding="utf-8"))
    if batch.get("handoffSha256") != HANDOFF_SHA or batch.get("status") != "DEVELOPMENT_NAVIGATION_ONLY":
        raise ValueError("Navigation frozen handoff mismatch")
    if len(batch["maps"]) != 9 or {m["paddedMapId"] for m in batch["maps"]} != set(EXPECTED):
        raise ValueError("Navigation map set mismatch")
    by_id = {m["paddedMapId"]: m for m in batch["maps"]}
    table_maps = {m["paddedMapId"]: m for m in tables["maps"]}
    hashes = {}
    for key in EXPECTED:
        folder = nav_root / "festival_maps" / key
        nav = folder / "scene.bin"
        item, mapping = by_id[key], table_maps[key]
        if (item["sceneId"] != int(key) or mapping["sceneConfigId"] != int(key) or mapping["worldId"] != int(key)
                or item["appearances"] != list(EXPECTED[key]) or mapping["appearances"] != item["appearances"]
                or item["navPath"] != mapping["navBinFile"]
                or item["navPath"] != f"data/scene_nav_bin/festival_maps/{key}/scene.bin"
                or mapping["spawnServer"] != [item["spawnServer"][axis] for axis in ("x", "y", "z")]
                or mapping["packedMaskSha256"] != item["packedSha256"]
                or mapping["walkableCells"] != item["walkableCells"]
                or mapping["components"] != len(item["componentSizes"])
                or any(record["clientOnly"] is not True or record["productionTravelEnabled"] is not False
                       for record in (item, mapping))):
            raise ValueError(f"Table/navigation contract mismatch: {key}")
        packed = base64.b64decode((folder / "walkmask.txt").read_bytes().strip(), validate=True)
        if (digest(folder / "source.json") != item["sourceSha256"]
                or digest(nav) != item["navSha256"]
                or digest(folder / "scene.bin.mask") != item["maskSha256"]
                or hashlib.sha256(packed).hexdigest() != item["packedSha256"]
                or sum(value.bit_count() for value in packed) != item["walkableCells"]):
            raise ValueError(f"Navigation manifest mismatch: {key}")
        expected = (folder / "scene.bin.sha256").read_text(encoding="ascii").split()
        if expected != [digest(nav), digest(folder / "scene.bin.mask")]:
            raise ValueError(f"Navigation/checksum mismatch: {key}")
        for path in folder.iterdir():
            if path.is_file():
                hashes[path.relative_to(nav_root).as_posix()] = digest(path)
    # 快照包含旧图导航；一起登记并核对，避免把未记录的旧图漂移带入本次服务。
    hashes.update({p.relative_to(nav_root).as_posix(): digest(p) for p in nav_root.rglob("*") if p.is_file()})
    table_hashes = {p.name: digest(p) for p in table_dir.iterdir() if p.is_file()}
    shutil.copytree(table_dir, run / "tables")
    shutil.copytree(nav_root, run / "navigation")
    if table_hashes != {p.name: digest(p) for p in (run / "tables").iterdir() if p.is_file()}:
        raise ValueError("Table files changed during snapshot")
    if any(digest(run / "navigation" / name) != expected for name, expected in hashes.items()):
        raise ValueError("Navigation files changed during snapshot")
    values["NODE_DEBUG_IMAGE"] = image
    (run / ".env").write_text("# Private local development configuration. Do not publish.\n" +
        "".join(f"{k}={v}\n" for k, v in sorted(values.items())), encoding="utf-8")
    table_mount = {"type": "bind", "source": (run / "tables").as_posix(), "target": "/generated/tables", "read_only": True}
    services = {name: {"volumes": [table_mount]} for name in ("login", "scene-manager", "player-locator")}
    for name in ("gate", "scene", "battle"):
        services[name] = {"volumes": [
            dict(table_mount, target="/app/generated/generated_tables"),
            {"type": "bind", "source": (run / "navigation").as_posix(), "target": "/app/data/scene_nav_bin", "read_only": True}]}
    # 调试镜像默认入口可能是 dlv；一次性迁移必须直接退出，沿用正式 debug overlay 的约定。
    services["match-migrate"] = {"entrypoint": ["/app/service"]}
    services["player-inbox-migrate"] = {"entrypoint": ["/app/service"]}
    (run / "compose.festival.json").write_text(json.dumps({"services": services}, indent=2) + "\n", encoding="utf-8")
    receipt = dict(developmentOnly=True, project=values["COMPOSE_PROJECT_NAME"], nodeImage=image,
                   nodeImageId=image_id, tablesReport=str(tables_report.resolve()),
                   supportingImageIds=image_ids,
                   tableSha256=table_hashes, navigationSha256=hashes,
                   gateway="http://127.0.0.1:" + values["GATEWAY_PORT"],
                   clientOnly=True, liveVerified=False)
    (run / "festival-runtime-manifest.json").write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"result": "PASS", "project": receipt["project"], "gateway": receipt["gateway"]}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-dir", required=True, type=Path)
    parser.add_argument("--tables-report", required=True, type=Path)
    parser.add_argument("--node-build", required=True, type=Path)
    parser.add_argument("--image-env", type=Path, default=REPO / "build/docker-debug-current/.env")
    args = parser.parse_args()
    configure(args.run_dir, args.tables_report, args.node_build, args.image_env)
