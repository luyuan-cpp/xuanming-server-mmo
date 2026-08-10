"""产物批次清单(manifest.json)测试。

清单存在的意义是让 Go / C++ 两端能发现自己加载的表"半新半旧",
所以三件事必须成立:每张表都在册、指纹能对上、版本号只在内容变了时才 +1。
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from conftest import Field, TableSpec, build_tables
from core.generators.json_gen import generate_json
from core.manifest import MANIFEST_FILENAME, MANIFEST_SCHEMA_VERSION, generate_manifest


def _specs() -> list[TableSpec]:
    return [
        TableSpec(
            name="Reward",
            fields=[Field(name="id", options="key"), Field(name="item_id")],
            rows=[[1, 101], [2, 102]],
        ),
        TableSpec(
            name="Mission",
            fields=[Field(name="id", options="key"), Field(name="reward_id")],
            rows=[[1, 1], [2, 2], [3, 1]],
        ),
    ]


def _export(cfg, specs: list[TableSpec]):
    tables = build_tables(cfg, specs)
    generate_json(cfg, tables)
    generate_manifest(cfg, tables)
    return tables


def _read_manifest(cfg) -> dict:
    with open(cfg.json_dir / MANIFEST_FILENAME, "r", encoding="utf-8") as f:
        return json.load(f)


def test_manifest_covers_every_table(cfg):
    _export(cfg, _specs())

    manifest = _read_manifest(cfg)

    assert manifest["schema_version"] == MANIFEST_SCHEMA_VERSION
    assert manifest["table_count"] == 2
    assert [t["name"] for t in manifest["tables"]] == ["Mission", "Reward"]


def test_manifest_records_row_count_and_sha256(cfg):
    _export(cfg, _specs())

    manifest = _read_manifest(cfg)
    mission = next(t for t in manifest["tables"] if t["name"] == "Mission")

    assert mission["rows"] == 3
    assert mission["source"]["file"] == "Mission.xlsx"
    assert len(mission["source"]["sha256"]) == 64

    art = next(a for a in mission["artifacts"] if a["kind"] == "json")
    data = (cfg.json_dir / "Mission.json").read_bytes()
    assert art["sha256"] == hashlib.sha256(data).hexdigest()
    assert art["size"] == len(data)


def test_artifact_paths_are_relative_to_manifest(cfg):
    _export(cfg, _specs())

    manifest = _read_manifest(cfg)
    for table in manifest["tables"]:
        for art in table["artifacts"]:
            assert not Path(art["file"]).is_absolute()
            assert (cfg.json_dir / art["file"]).is_file()


def test_version_stable_when_content_unchanged(cfg):
    tables = _export(cfg, _specs())
    first = _read_manifest(cfg)

    generate_json(cfg, tables)
    generate_manifest(cfg, tables)
    second = _read_manifest(cfg)

    assert second["version"] == first["version"] == 1
    assert second["content_digest"] == first["content_digest"]
    assert second["generated_at"] == first["generated_at"], "内容没变就不该重写清单"


def test_version_bumps_when_content_changes(cfg):
    _export(cfg, _specs())
    assert _read_manifest(cfg)["version"] == 1

    changed = _specs()
    changed[1].rows.append([4, 2])
    _export(cfg, changed)

    manifest = _read_manifest(cfg)
    assert manifest["version"] == 2
    mission = next(t for t in manifest["tables"] if t["name"] == "Mission")
    assert mission["rows"] == 4


def test_version_is_monotonic_across_many_changes(cfg):
    versions = []
    for extra in range(3):
        specs = _specs()
        specs[1].rows.extend([[10 + i, 1] for i in range(extra + 1)])
        _export(cfg, specs)
        versions.append(_read_manifest(cfg)["version"])

    assert versions == sorted(versions)
    assert versions == [1, 2, 3]


def test_corrupt_previous_manifest_does_not_abort(cfg):
    """清单读坏了不该阻断导表——它是元数据,不是位序那种权威状态。"""
    tables = build_tables(cfg, _specs())
    generate_json(cfg, tables)
    (cfg.json_dir / MANIFEST_FILENAME).write_text("not json", encoding="utf-8")

    generate_manifest(cfg, tables)

    assert _read_manifest(cfg)["version"] == 1
