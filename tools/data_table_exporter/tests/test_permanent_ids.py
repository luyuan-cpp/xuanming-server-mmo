"""表行 id 只增不删(``(cfg_ids_permanent)``)的门禁回归。

scene 把「本节点表里查不到的 id」判成更新版本或尚未热更到的行、回 RETRY 等它出现,前提是表行永不删除;
否则引用已删行的资产通道指令会永远卡住(混版本持久字段保护设计 §13 第 10 条)。这条前提由导表器守:
表里少了台账里的 id 就整批不产出。
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
from core.permanent_ids import (  # noqa: E402
    PermanentIdsError,
    ledger_path,
    load_ledger,
    record_permanent_ids,
    validate_permanent_ids,
)


def _table(tmp_path: Path, ids, permanent: bool = True):
    cfg = make_config(tmp_path)
    cfg.data_dir.mkdir(parents=True, exist_ok=True)
    spec = TableSpec(name="Item", fields=[
        Field(name="id", type="uint32", owner="common"),
        Field(name="value", type="uint32", owner="common"),
    ], rows=[[i, i * 10] for i in ids])
    schema = read_table(write_xlsx(cfg.data_dir, spec), cfg)
    assert schema is not None
    schema.ids_permanent = permanent   # 旧表头路径不读表级 option,这里直接置位;解析见 test_schema_proto
    return schema, cfg


def _write_ledger(cfg, schema, lines: str) -> Path:
    path = ledger_path(cfg, schema)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(lines, encoding="utf-8")
    return path


def test_missing_ledger_is_initialized_from_the_current_table(tmp_path: Path):
    schema, cfg = _table(tmp_path, [3, 1, 2])
    assert validate_permanent_ids([schema], cfg).ok

    record_permanent_ids([schema], cfg)
    path = ledger_path(cfg, schema)
    assert load_ledger(path) == {1, 2, 3}
    lines = path.read_text(encoding="utf-8").splitlines()
    assert lines[0].startswith("#")
    assert [l for l in lines if not l.startswith("#")] == ["1", "2", "3"], "升序、每行一个,diff 才稳定"
    assert b"\r" not in path.read_bytes(), "台账固定 LF"


def test_new_ids_are_appended_and_existing_ones_kept(tmp_path: Path):
    schema, cfg = _table(tmp_path, [1, 2, 3])
    _write_ledger(cfg, schema, "# header\n1\n2\n")
    assert validate_permanent_ids([schema], cfg).ok

    record_permanent_ids([schema], cfg)
    assert load_ledger(ledger_path(cfg, schema)) == {1, 2, 3}


def test_unchanged_table_leaves_the_ledger_untouched(tmp_path: Path):
    schema, cfg = _table(tmp_path, [1, 2])
    path = _write_ledger(cfg, schema, "# 手写的表头不被改写\n1\n2\n")
    record_permanent_ids([schema], cfg)
    assert path.read_text(encoding="utf-8") == "# 手写的表头不被改写\n1\n2\n"


def test_removed_row_fails_the_export_and_says_how_to_fix(tmp_path: Path):
    schema, cfg = _table(tmp_path, [1, 3])
    _write_ledger(cfg, schema, "1\n2\n3\n")
    report = validate_permanent_ids([schema], cfg)
    assert not report.ok
    message = report.errors[0]
    assert "[Item]" in message and ": 2." in message, "要点名被删的 id"
    assert "cfg_ids_permanent" in message
    assert "remove its references" in message, "报错要说清楚该怎么做"


def test_tables_without_the_option_are_not_checked_or_recorded(tmp_path: Path):
    schema, cfg = _table(tmp_path, [1], permanent=False)
    _write_ledger(cfg, schema, "1\n2\n")  # 没声明选项:这份台账与它无关
    assert validate_permanent_ids([schema], cfg).ok
    record_permanent_ids([schema], cfg)
    assert load_ledger(ledger_path(cfg, schema)) == {1, 2}


@pytest.mark.parametrize("content,needle", [
    ("1\nabc\n", "not an integer"),
    ("1\n1\n", "duplicate row id 1"),
])
def test_malformed_ledger_fails_closed(tmp_path: Path, content: str, needle: str):
    schema, cfg = _table(tmp_path, [1])
    _write_ledger(cfg, schema, content)
    report = validate_permanent_ids([schema], cfg)
    assert not report.ok
    assert needle in report.errors[0]
    with pytest.raises(PermanentIdsError):
        record_permanent_ids([schema], cfg)
