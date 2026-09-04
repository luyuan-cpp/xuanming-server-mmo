"""重复主键的 fail-closed 回归。

从前重复 id 是静默丢索引:生成的 id 索引是单值 map,后写覆盖先写(Go)或先写胜出(C++),
``FindById`` 只拿得到其中一行、``Count`` 少算,而 ``FindAll`` / JSON / ``.pb`` 里那些行都还在。
数据没丢,丢的是索引,而且零报错 —— 正是最难发现的一类。
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

_PKG_ROOT = Path(__file__).resolve().parent.parent
if str(_PKG_ROOT) not in sys.path:
    sys.path.insert(0, str(_PKG_ROOT))

from conftest import Field, TableSpec, make_config, write_xlsx  # noqa: E402
from core.excel_reader import read_table  # noqa: E402
from core.primary_key import validate_primary_keys  # noqa: E402


def _table(tmp_path: Path, rows, options: str = ""):
    cfg = make_config(tmp_path)
    cfg.data_dir.mkdir(parents=True, exist_ok=True)
    spec = TableSpec(name="Dup", fields=[
        Field(name="id", type="uint32", owner="common", options=options),
        Field(name="value", type="uint32", owner="common"),
    ], rows=rows)
    path = write_xlsx(cfg.data_dir, spec)
    schema = read_table(path, cfg)
    assert schema is not None
    return schema, cfg


def test_unique_ids_pass(tmp_path: Path):
    schema, cfg = _table(tmp_path, [[1, 10], [2, 20], [3, 30]])
    assert validate_primary_keys([schema], cfg).ok


def test_duplicate_id_is_rejected(tmp_path: Path):
    """没声明可重复 -> 重复 id 整批不产出。"""
    schema, cfg = _table(tmp_path, [[1, 10], [1, 11], [2, 20]])
    report = validate_primary_keys([schema], cfg)
    assert not report.ok
    assert "主键 id 重复" in report.errors[0]
    assert "(cfg_multi)" in report.errors[0]      # 报错要说清楚怎么修


def test_duplicate_id_allowed_when_declared(tmp_path: Path):
    """主键列声明了 multi -> 重复 id 合法。"""
    schema, cfg = _table(tmp_path, [[1, 10], [1, 11], [2, 20]], options="multi")
    assert schema.multi_primary_key is True
    assert validate_primary_keys([schema], cfg).ok
