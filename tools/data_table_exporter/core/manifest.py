"""产物批次清单(manifest.json)生成器。

``generated/tables/`` 下原本只有表数据、没有任何清单:Go 与 C++ 双端各自扫目录加载,
**没有任何机制能发现两端半新半旧**——一端热加载了新一批表、另一端还是旧的,
两边算出来的掉落 / 任务 / 场景 ID 就悄悄对不上。

manifest.json 给这一批产物一个可比对的身份:单调递增的 ``version`` + 每个文件的 sha256。
加载端只要各自读出 ``version`` / ``content_digest`` 上报,就能一眼看出谁落后了。
(**加载端的校验不在本工具范围内**,这里只负责把清单产出来;字段含义见 readme.md
「产物批次清单」一节。)

设计要点:
- ``content_digest`` 只覆盖内容(表名 / 行数 / 各产物 sha256 / 源表 sha256),
  **不含** ``version`` 与 ``generated_at``。内容没变 → digest 不变 → 版本号不动、
  文件原样不重写,导表不会因为跑了一次就制造 diff。
- ``version`` 单调递增且只在内容变化时 +1,所以「版本号大 = 更新」这个判断是可靠的。
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import subprocess
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from core.config_loader import ExporterConfig
from core.file_utils import sha256_hash, write_file
from core.schema import TableSchema

logger = logging.getLogger(__name__)

MANIFEST_FILENAME = "manifest.json"

# 清单自身的格式版本。字段增删要 +1,加载端据此判断能不能解析。
MANIFEST_SCHEMA_VERSION = 1


def generate_manifest(cfg: ExporterConfig, tables: list[TableSchema]) -> Path:
    """扫描已产出的表数据,写出 ``generated/tables/manifest.json``,返回其路径。"""
    manifest_path = cfg.json_dir / MANIFEST_FILENAME

    entries = [_table_entry(cfg, table, manifest_path.parent) for table in tables]
    entries.sort(key=lambda e: e["name"])

    content_digest = _content_digest(entries)
    previous = _load_previous(manifest_path)

    if previous is not None and previous.get("content_digest") == content_digest:
        logger.info(
            "产物清单未变(version=%s, digest=%s),跳过重写",
            previous.get("version"), content_digest[:12],
        )
        return manifest_path

    version = _next_version(previous)
    manifest: dict[str, Any] = {
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "version": version,
        "content_digest": content_digest,
        "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "source_rev": _source_rev(cfg.data_dir),
        "table_count": len(entries),
        "tables": entries,
    }
    write_file(manifest_path, json.dumps(manifest, indent=2, ensure_ascii=False) + "\n")
    logger.info("Generated %s (version=%d, %d tables)", MANIFEST_FILENAME, version, len(entries))
    return manifest_path


# ---------------------------------------------------------------------------
# Internal
# ---------------------------------------------------------------------------

def _table_entry(cfg: ExporterConfig, table: TableSchema, manifest_dir: Path) -> dict[str, Any]:
    """一张表在清单里的条目:源表指纹 + 行数 + 全部产物指纹。"""
    artifacts: list[dict[str, Any]] = []

    json_path = cfg.json_dir / f"{table.name}.json"
    binary_path = cfg.binary_dir / f"{table.name.lower()}.pb"
    for path, kind in ((json_path, "json"), (binary_path, "binary")):
        info = _artifact_info(path, kind, manifest_dir)
        if info is not None:
            artifacts.append(info)

    return {
        "name": table.name,
        "rows": _row_count(json_path),
        "source": _source_info(table),
        "artifacts": artifacts,
    }


def _artifact_info(path: Path, kind: str, manifest_dir: Path) -> dict[str, Any] | None:
    """产物文件的指纹。文件不存在返回 None——比如整表被 owner 过滤空了就不会有 json。"""
    if not path.is_file():
        return None
    digest = sha256_hash(path)
    if digest is None:
        return None
    return {
        "kind": kind,
        # 相对清单自身的位置:清单和产物一起被拷到别处也不会失效。
        "file": Path(os.path.relpath(path, manifest_dir)).as_posix(),
        "size": path.stat().st_size,
        "sha256": digest,
    }


def _row_count(json_path: Path) -> int:
    """行数直接读产出的 json,而不是重新解析 xlsx——清单描述的是**产物**,不是源表。"""
    if not json_path.is_file():
        return 0
    try:
        with open(json_path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except Exception as exc:
        logger.warning("清单统计行数失败 %s: %s", json_path.name, exc)
        return 0
    rows = data.get("data") if isinstance(data, dict) else None
    return len(rows) if isinstance(rows, list) else 0


def _source_info(table: TableSchema) -> dict[str, Any]:
    src = table.source_path
    if src is None or not Path(src).is_file():
        return {"file": "", "sha256": ""}
    src = Path(src)
    return {
        "file": src.name,
        "size": src.stat().st_size,
        "sha256": sha256_hash(src) or "",
    }


def _content_digest(entries: list[dict[str, Any]]) -> str:
    """内容指纹:只喂"内容相关"字段,不含 version / generated_at / source_rev。

    source_rev 也排除:同一批表在两次 commit 之间重导,内容一样就不该算新批次。
    """
    hasher = hashlib.sha256()
    for entry in entries:
        hasher.update(entry["name"].encode("utf-8"))
        hasher.update(str(entry["rows"]).encode("utf-8"))
        hasher.update(entry["source"].get("sha256", "").encode("utf-8"))
        for art in entry["artifacts"]:
            hasher.update(art["kind"].encode("utf-8"))
            hasher.update(art["file"].encode("utf-8"))
            hasher.update(art["sha256"].encode("utf-8"))
    return hasher.hexdigest()


def _load_previous(path: Path) -> dict[str, Any] | None:
    if not path.is_file():
        return None
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except Exception as exc:
        # 清单读坏了不该阻断导表:它是"这批产物是谁"的元数据,不是位序那种权威状态,
        # 重新从 1 开始最坏是让加载端多报一次不一致,不会损坏任何存量数据。
        logger.warning("旧产物清单无法解析,版本号将从头开始:%s(%s)", path, exc)
        return None
    return data if isinstance(data, dict) else None


def _next_version(previous: dict[str, Any] | None) -> int:
    if not previous:
        return 1
    try:
        return int(previous.get("version", 0)) + 1
    except (TypeError, ValueError):
        return 1


def _source_rev(data_dir: Path) -> dict[str, Any]:
    """源表所在仓库的 git 版本。

    拿不到就老实写 unknown / dirty=true:清单宁可自称"来源不可信",
    也不能给出一个看起来精确、实际对不上的 rev。
    """
    unknown: dict[str, Any] = {"commit": "unknown", "data_dirty": True}
    if not Path(data_dir).exists():
        return unknown
    try:
        commit = subprocess.run(
            ["git", "-C", str(data_dir), "rev-parse", "HEAD"],
            capture_output=True, text=True, timeout=20, check=True,
        ).stdout.strip()
    except Exception as exc:
        logger.warning("清单取 git rev 失败,记为 unknown:%s", exc)
        return unknown
    try:
        status = subprocess.run(
            ["git", "-C", str(data_dir), "status", "--porcelain", "--", str(data_dir)],
            capture_output=True, text=True, timeout=30, check=True,
        ).stdout
        dirty = bool(status.strip())
    except Exception:
        dirty = True
    return {"commit": commit, "data_dirty": dirty}
