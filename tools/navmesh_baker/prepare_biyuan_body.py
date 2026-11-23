#!/usr/bin/env python3
"""校验客户端碧渊水宫双节正式交付并烘焙服务端场景 5 严格导航（MSET + FMASK + SHA256）。"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[2]
MAP_KEY = "biyuan-water-palace"
SCENE_ID = 5
REVISION = "biyuan-body-20261123"
NAV_REL = Path(f"data/scene_nav_bin/current_maps/{MAP_KEY}/{REVISION}/scene.bin")
NAV_FIELDS = (
    "worldRect",
    "effectiveWorldRect",
    "imageRect",
    "cellSize",
    "walkmaskColumns",
    "walkmaskRows",
    "spawnWorld",
)


def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha_file(path: Path) -> str:
    return sha_bytes(path.read_bytes())


def read_json(path: Path) -> dict:
    return json.loads(path.read_bytes().decode("utf-8-sig"))


def csv(values) -> str:
    return ",".join(str(v) for v in values)


def vec(point: dict) -> list[float]:
    return [point[axis] for axis in ("x", "y", "z")]


def rect(value: dict) -> list[float]:
    return [value[field] for field in ("x", "y", "width", "height")]


def check_client_source(client: Path, appearance: str):
    resources = client / "Assets/Resources"
    catalog = read_json(resources / "World/QdaoMaps20261102/catalog.json")
    entry = next((m for m in catalog["maps"] if m["sceneId"] == SCENE_ID and m["key"] == MAP_KEY), None)
    require(entry is not None and entry["revision"] == REVISION, "Client biyuan scene 5 revision mismatch")
    ap = next((a for a in entry["appearances"] if a["id"] == appearance), None)
    require(ap is not None, f"Missing client appearance: {appearance}")
    folder = (resources / ap["resourceRoot"]).resolve()
    require(folder.is_relative_to(resources.resolve()), "Client asset path escapes Assets/Resources")
    source = read_json(folder / "source.json")
    require(
        source["schemaVersion"] == 2
        and source["key"] == MAP_KEY
        and source["sceneId"] == SCENE_ID
        and source["revision"] == REVISION
        and source["appearance"] == appearance
        and source["resourceRoot"] == ap["resourceRoot"]
        and source["layoutSha256"] == ap["layoutSha256"],
        "Client source identity mismatch",
    )
    for name, field in (("layout.png", "layoutSha256"), ("walkmask.txt", "walkmaskSha256"), ("navigation.json", "navigationSha256")):
        require(sha_file(folder / name) == source[field], f"Client digest mismatch: {appearance}/{name}")
    plan = (client / source["navigationPlanPath"]).resolve()
    require(plan.is_relative_to(client.resolve()) and sha_file(plan) == source["navigationPlanSha256"], "Navigation plan digest mismatch")
    packed = base64.b64decode((folder / "walkmask.txt").read_bytes().strip(), validate=True)
    total = source["walkmaskColumns"] * source["walkmaskRows"]
    require(
        sha_bytes(packed) == source["walkmaskPackedSha256"]
        and len(packed) == (total + 7) // 8
        and (total % 8 == 0 or not (packed[-1] & ((1 << (8 - total % 8)) - 1))),
        "Client packed walkmask mismatch",
    )
    nav = read_json(folder / "navigation.json")
    require(
        all(nav["source"][f] == source[f] for f in NAV_FIELDS)
        and nav["navigationPlanSha256"] == source["navigationPlanSha256"]
        and nav["stats"]["maskSha256"] == source["walkmaskPackedSha256"]
        and sum(b.bit_count() for b in packed) == nav["stats"]["walkableCells"],
        "Client navigation review mismatch",
    )
    return folder, source, nav


def install_nav(client: Path, baker: Path, repo: Path = ROOT, cell_cs: float = 0.25) -> dict:
    spring_folder, spring_src, spring_nav = check_client_source(client, "spring")
    autumn_folder, autumn_src, _ = check_client_source(client, "mid-autumn")
    for field in (*NAV_FIELDS, "walkmaskSha256", "walkmaskPackedSha256", "navigationSha256", "navigationPlanSha256"):
        require(spring_src[field] == autumn_src[field], f"Festival navigation mismatch: {field}")

    destination = repo / NAV_REL.parent
    destination.mkdir(parents=True, exist_ok=True)
    for name in ("source.json", "walkmask.txt", "navigation.json"):
        shutil.copy2(spring_folder / name, destination / name)

    scene_bin = destination / NAV_REL.name
    columns, rows = spring_src["walkmaskColumns"], spring_src["walkmaskRows"]
    world = rect(spring_src["worldRect"])
    effective = rect(spring_src["effectiveWorldRect"])
    spawn = vec(spring_src["spawnWorld"])
    stats = spring_nav["stats"]
    command = [
        str(baker),
        "--mask-base64", str(destination / "walkmask.txt"),
        "--mask-grid", csv([columns, rows, spring_src["cellSize"], *world]),
        "--effective-rect", csv(effective),
        "--cs", str(cell_cs),
        "--min-region-area", "0",
        "--probe", csv(spawn),
    ]
    for name, record in stats["landmarks"].items():
        if name != "spawn":
            command.extend(["--probe", csv(vec(record["world"]))])
    command.extend(["--out", str(scene_bin)])

    result = subprocess.run(command, capture_output=True, text=True, encoding="utf-8", timeout=180)
    output = result.stdout + result.stderr
    require(result.returncode == 0, f"navmesh_baker failed ({result.returncode}):\n{output}")
    expected_cells = f"mask cells checked={columns * rows} mismatches=0"
    expected_conn = (
        f"mask connectivity components={stats['componentCount']} "
        f"reachable_cells={stats['walkableCells']} cross_component_bridges=0"
    )
    require(expected_cells in output and expected_conn in output, f"Baker contract check missing:\n{output}")

    nav_sha = sha_file(scene_bin)
    mask_sha = sha_file(destination / "scene.bin.mask")
    (destination / "scene.bin.sha256").write_bytes(f"{nav_sha}\n{mask_sha}\n".encode("ascii"))

    receipt = {
        "schemaVersion": 1,
        "key": MAP_KEY,
        "sceneId": SCENE_ID,
        "revision": REVISION,
        "navPath": NAV_REL.as_posix(),
        "springSourceSha256": sha_file(spring_folder / "source.json"),
        "midAutumnSourceSha256": sha_file(autumn_folder / "source.json"),
        "walkmaskPackedSha256": spring_src["walkmaskPackedSha256"],
        "navSha256": nav_sha,
        "maskSha256": mask_sha,
        "navBytes": scene_bin.stat().st_size,
        "maskBytes": (destination / "scene.bin.mask").stat().st_size,
        "bakeCellSize": cell_cs,
        "walkmaskColumns": columns,
        "walkmaskRows": rows,
        "walkableCells": stats["walkableCells"],
        "componentCount": stats["componentCount"],
        "probedLandmarks": len(stats["landmarks"]),
        "spawnUnity": spring_src["spawnWorld"],
        "spawnServer": {"x": spawn[2], "y": spawn[0], "z": spawn[1]},
        "bakerSummary": [line for line in output.splitlines() if line.strip()],
    }
    (destination / "import-receipt.json").write_text(
        json.dumps(receipt, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n"
    )
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client-root", type=Path, default=ROOT.parent / "mmorpg-client")
    parser.add_argument("--baker", type=Path, default=ROOT / "tools/navmesh_baker/build/Release/navmesh_baker.exe")
    parser.add_argument("--cs", type=float, default=0.25)
    args = parser.parse_args()
    receipt = install_nav(args.client_root.resolve(), args.baker.resolve(), ROOT, args.cs)
    print(json.dumps({k: v for k, v in receipt.items() if k != "bakerSummary"}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
