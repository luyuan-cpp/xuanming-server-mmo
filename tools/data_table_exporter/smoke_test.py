"""对**仓库真实配置表**的冒烟测试(pytest 风格,文件名匹配 pytest 默认的 ``*_test.py``)。

原来这里是 77 行 print + 结尾一句"✓ All smoke tests passed",不管解析成什么样都打勾,
等于没有测试。现在改成断言:表头能解析、id 列在位、数据行读得出来、
外键结构与取值都成立。

这是唯一一处跑真实数据的测试(tests/ 下的都用临时造的 xlsx),
所以它同时也是「导表器能不能吃下当前 data/」的守门人。
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

_HERE = Path(__file__).resolve().parent
if str(_HERE) not in sys.path:
    sys.path.insert(0, str(_HERE))

from core.config_loader import ExporterConfig, load_config  # noqa: E402
from core.excel_reader import read_all_tables, read_data_rows, read_table  # noqa: E402
from core.foreign_key import validate_foreign_keys  # noqa: E402
from core.schema import TableSchema  # noqa: E402

# 覆盖各种表结构:普通表 / 数组 / map / 位序 / 多主键 / 表达式列
_SAMPLE_TABLES = ["Test.xlsx", "Buff.xlsx", "Reward.xlsx", "Mission.xlsx", "Skill.xlsx"]


@pytest.fixture(scope="module")
def cfg() -> ExporterConfig:
    return load_config()


@pytest.fixture(scope="module")
def all_tables(cfg: ExporterConfig) -> list[TableSchema]:
    tables = read_all_tables(cfg)
    if not tables:
        pytest.skip(f"data_dir 下没有可读的 xlsx:{cfg.data_dir}")
    return tables


@pytest.mark.parametrize("filename", _SAMPLE_TABLES)
def test_sample_table_parses(cfg: ExporterConfig, filename: str):
    path = cfg.data_dir / filename
    if not path.exists():
        pytest.skip(f"{filename} 不存在")

    schema = read_table(path, cfg)

    assert schema is not None, f"{filename} 解析失败"
    assert schema.columns, f"{filename} 没解析出任何列"
    assert schema.columns[0].name == "id", f"{filename} 第一列必须是 id"
    assert schema.id_column is not None


@pytest.mark.parametrize("filename", _SAMPLE_TABLES)
def test_sample_table_has_data_rows(cfg: ExporterConfig, filename: str):
    path = cfg.data_dir / filename
    if not path.exists():
        pytest.skip(f"{filename} 不存在")

    schema = read_table(path, cfg)
    rows = read_data_rows(schema, cfg)

    assert rows, f"{filename} 读不出任何数据行"
    assert "id" in rows[0], f"{filename} 数据行缺 id 字段"


def test_every_table_name_is_unique(all_tables: list[TableSchema]):
    names = [t.name for t in all_tables]
    assert len(names) == len(set(names)), f"表名重复:{names}"


def test_every_table_starts_with_id_column(all_tables: list[TableSchema]):
    bad = [t.name for t in all_tables if not t.columns or t.columns[0].name != "id"]
    assert bad == [], f"这些表第一列不是 id:{bad}"


def test_bit_index_tables_use_a_single_marked_column(all_tables: list[TableSchema]):
    """位序表只能有一个 bit_index 标记列——_find_max_bit 只看第一个,多标一个就会静默取错列。"""
    for table in all_tables:
        marked = {c.name for c in table.bit_index_columns}
        assert len(marked) <= 1, f"{table.name} 标了多个 bit_index 列:{sorted(marked)}"


def test_repository_data_passes_foreign_key_validation(
    cfg: ExporterConfig, all_tables: list[TableSchema]
):
    """仓库现有配表的外键(结构 + 取值)必须全绿,否则 run.py 会直接中止导表。"""
    report = validate_foreign_keys(all_tables, cfg)

    assert report.ok, "外键校验失败:\n" + "\n".join(report.errors)
