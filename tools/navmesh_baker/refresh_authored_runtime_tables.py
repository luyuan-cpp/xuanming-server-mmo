#!/usr/bin/env python3
"""从当前正式源表重新导出本机定制地图表集，保留两份已验收的地图源表与旧批原件。"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import shutil
import sys

import deploy_authored_local as deployment
from install_authored_maps import export_tables

rebuild = deployment.rebuild
ROOT = deployment.ROOT
MAP_BOOKS = ("BaseScene.xlsx", "World.xlsx")
MAP_ARTIFACTS = {f"{name}.{suffix}" for name in ("basescene", "world") for suffix in ("json", "pb")}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def read(path):
    return json.loads(path.read_text("utf-8-sig"))


def write(path, value):
    with path.open("x", encoding="utf-8") as stream:
        stream.write(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


def table_hashes(directory):
    # Windows 的现有部署副本可能保留大写文件名；正式导出文件名及清单均为小写。
    entries = [path for path in directory.iterdir() if path.is_file()]
    result = {path.name.lower(): rebuild.digest(path) for path in entries}
    require(len(result) == len(entries), "Case-colliding table artifacts")
    return result


def formal_inputs():
    paths = list((ROOT / "data").glob("*.xlsx"))
    paths += list((ROOT / "data/schema").glob("*.proto"))
    paths += list((ROOT / "data/operator").glob("*.xlsx"))
    paths += list((ROOT / "data/tip").glob("*.xlsx"))
    return {path.relative_to(ROOT).as_posix(): rebuild.digest(path) for path in sorted(paths)}


def compare_artifacts(previous, official, candidate, allowed):
    require(previous.keys() <= official.keys() and official.keys() == candidate.keys(),
            "Table artifact set changed: previous table removed or official export incomplete")
    require(MAP_ARTIFACTS.issubset(candidate), "Authored map artifacts are incomplete")
    for name in MAP_ARTIFACTS:
        require(candidate[name] == previous[name], "Authored map artifact changed: " + name)
    for name in candidate.keys() - MAP_ARTIFACTS - {"manifest.json"}:
        require(candidate[name] == official[name], "Export does not match current official table: " + name)
    changed = {name for name in candidate if candidate[name] != previous.get(name)}
    approved = {f"{name.lower()}.{suffix}" for name in allowed for suffix in ("json", "pb")}
    require(changed <= approved | {"manifest.json"}, "Unapproved table changes: " + ", ".join(sorted(changed - approved - {"manifest.json"})))
    return sorted(changed)


def refresh(previous_path, output, allowed):
    previous_path, output = previous_path.resolve(), output.resolve()
    require(output.parent == (ROOT / "build/verification").resolve() and
            re.fullmatch(r"authored-festival-[A-Za-z0-9_.-]+", output.name), "Unexpected authored output path")
    require(not output.exists(), "Output batch already exists; preserve it and choose a new batch")
    require(allowed and all(re.fullmatch(r"[A-Z][A-Za-z0-9]*", name) for name in allowed) and
            not set(allowed) & {"BaseScene", "World"}, "Explicit non-map table names are required")
    previous = rebuild.load_runtime_data_manifest(previous_path)
    previous_sha = rebuild.digest(previous_path)
    deployment.validate_overlay(Path(previous["composeOverlay"]["path"]), previous)
    original_report = read(Path(previous["sourceExport"]["path"]))
    formal_before = formal_inputs()
    official_directory = ROOT / "generated/tables"
    official_before = table_hashes(official_directory)
    official_manifest = read(official_directory / "manifest.json")
    known = {table["name"] for table in official_manifest["tables"]}
    require(set(allowed) <= known, "Approved table is absent from official manifest")
    for table in official_manifest["tables"]:
        name = table["source"]["file"]
        require(formal_before.get("data/" + name) == table["source"]["sha256"], "Official export source is stale: " + name)
        for artifact in table["artifacts"]:
            require(official_before.get(artifact["file"]) == artifact["sha256"], "Official artifact digest mismatch: " + artifact["file"])

    output.mkdir()
    staged = output / "source-data"
    staged.mkdir()
    for source in (ROOT / "data").glob("*.xlsx"):
        shutil.copy2(source, staged / source.name)
    shutil.copytree(ROOT / "data/schema", staged / "schema")
    for name in MAP_BOOKS:
        shutil.copy2(Path(previous["sourceWorkbooks"][name]["path"]), staged / name)
    write(output / "refresh-inputs.json", dict(previousManifest=dict(path=str(previous_path), sha256=previous_sha),
        formalInputSha256=formal_before, officialTableSha256=official_before, allowedTables=sorted(allowed)))

    generated = export_tables(staged, output / "tables", output / "tables-export.log")
    after = table_hashes(generated)
    changed = compare_artifacts(previous["tableSha256"], official_before, after, allowed)
    require(formal_inputs() == formal_before and table_hashes(official_directory) == official_before,
            "Formal inputs or official output changed during export")
    require(rebuild.digest(previous_path) == previous_sha, "Previous runtime manifest changed during export")
    require(rebuild.load_runtime_data_manifest(previous_path)["tableSha256"] == previous["tableSha256"], "Previous runtime data changed during export")
    deployment.validate_overlay(Path(previous["composeOverlay"]["path"]), previous)

    workbook_sha = {name: rebuild.digest(staged / name) for name in MAP_BOOKS}
    unchanged = {name: sha for name, sha in after.items() if previous["tableSha256"].get(name) == sha}
    supplemental = {name: after[name] for name in original_report.get("supplementalTableSha256", {})}
    report = dict(schemaVersion=1, developmentOnly=True, productionTravelEnabled=False,
        refreshedAtUtc=datetime.now(timezone.utc).isoformat(),
        predecessorRuntimeManifest=dict(path=str(previous_path), sha256=previous_sha),
        exportDeliverySha256=previous["exportDelivery"]["sha256"],
        formalSourceSha256={Path(name).name: sha for name, sha in formal_before.items() if Path(name).parent == Path("data")},
        sourceWorkbookSha256=workbook_sha, generatedTables=str(generated), changedTableFiles=changed,
        tableSha256=after, officialTableManifestSha256=after["manifest.json"],
        unchangedTableSha256=unchanged, supplementalTableSha256=supplemental,
        maps=original_report["maps"], liveServerVerified=False,
        javaFullServerAuthoredSupport=original_report.get("javaFullServerAuthoredSupport"),
        refreshedTables=sorted(allowed))
    report_path = output / "authored-test-tables.json"
    write(report_path, report)
    overlay = read(Path(previous["composeOverlay"]["path"]))
    for service in overlay["services"]:
        for mount in overlay["services"][service]["volumes"]:
            if mount["target"] == rebuild.TABLE_TARGETS[service]:
                mount["source"] = generated.as_posix()
    overlay_path = output / "compose.authored.json"
    write(overlay_path, overlay)
    data = {key: value for key, value in previous.items() if key not in {"manifestSha256", "supplementalTableSha256"}}
    data.update(sourceExport=dict(path=str(report_path), sha256=rebuild.digest(report_path)),
        sourceWorkbooks={name: dict(path=str(staged / name), sha256=sha) for name, sha in workbook_sha.items()},
        tableDirectory=str(generated), tableSha256=after, officialTableManifestSha256=after["manifest.json"],
        services={service: dict(mount, source=str(generated)) for service, mount in previous["services"].items()},
        composeOverlay=dict(path=str(overlay_path), sha256=rebuild.digest(overlay_path)))
    manifest_path = output / "runtime-data-manifest.json"
    write(manifest_path, data)
    validated = rebuild.load_runtime_data_manifest(manifest_path)
    deployment.validate_overlay(overlay_path, validated)
    write(output / "refresh-verification.json", dict(success=True, runtimeDataManifest=str(manifest_path),
        runtimeDataManifestSha256=rebuild.digest(manifest_path), composeOverlay=str(overlay_path),
        changedTableFiles=changed, authoredMapArtifactsUnchanged=True, currentOfficialTablesMatched=True,
        previousBatchUnchanged=True, liveServerVerified=False))
    return manifest_path, overlay_path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--previous-manifest", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--allow-table", action="append", required=True)
    args = parser.parse_args()
    manifest, overlay = refresh(args.previous_manifest, args.output, args.allow_table)
    print(json.dumps(dict(result="PASS", runtimeDataManifest=str(manifest), composeOverlay=str(overlay))))


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, ValueError) as error:
        print("error: " + str(error), file=sys.stderr)
        raise SystemExit(1)
