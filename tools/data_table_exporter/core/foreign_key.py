"""外键校验。

分两层:

1. **结构层**:``fk:Table[.column]`` / ``gfk:Table`` 指向的表、列是否存在。
2. **数据层**:引用列里的**每一个取值**是否真的能在目标表对应列里找到。

以前只做第 1 层,而且结果只 ``logger.warning`` 一句、产出照常落盘——等于配表填错
ID 时导出完全静默通过,错误一直漏到运行时(C++/Go 的 ``FindById`` 返回空),
表现是"某任务领不到奖励""某镜像进不去场景"这类查半天的线上问题。

现在数据层失配一律进 ``errors``,由 orchestrator 直接中止、**产出不落盘**。
``0`` / ``-1`` / 空单元格视为"无引用"跳过(与 excel_reader._build_row 的空值哨兵口径一致)。
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from typing import Any, Optional

from core.config_loader import ExporterConfig
from core.excel_reader import open_worksheet
from core.schema import TableSchema

logger = logging.getLogger(__name__)

# 空引用哨兵:与 excel_reader._build_row 对 repeated 列的 ``value not in (0, -1, "")`` 保持一致。
_EMPTY_REFS: frozenset[Any] = frozenset({0, -1, ""})

# 单个引用列最多列出多少个失配取值,避免一张表填错整列时刷屏。
_MAX_REPORTED_VALUES = 10


@dataclass
class ForeignKeyReport:
    """外键校验结果。

    errors 非空 = 必须中止导表;warnings = 记录但放行(多为"无法校验"而非"校验失败")。
    """
    errors: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.errors


def validate_foreign_keys(tables: list[TableSchema], cfg: ExporterConfig) -> ForeignKeyReport:
    """校验所有表的 FK / GFK 引用(结构 + 取值)。"""
    report = ForeignKeyReport()
    table_map: dict[str, TableSchema] = {t.name: t for t in tables}
    col_map: dict[str, set[str]] = {t.name: {c.name for c in t.columns} for t in tables}

    rows_cache: dict[str, list[tuple]] = {}
    key_cache: dict[tuple[str, str], set[Any]] = {}

    for table in tables:
        refs: list[tuple[str, str, str, str]] = []  # (kind, 本表列名, 目标表, 目标列)
        for col in table.foreign_key_columns:
            fk = col.foreign_key
            if fk is not None:
                refs.append(("fk", col.name, fk.target_table, fk.target_column))
        for col in table.group_foreign_key_columns:
            gfk = col.group_foreign_key
            if gfk is not None:
                # gfk 是"这一组列整体引用 TableName",目标列恒为 id
                # (见 templates/*_config_fk.j2 里生成的 FindById 系列函数)。
                refs.append(("gfk", col.name, gfk.target_table, "id"))

        for kind, col_name, target_table, target_column in refs:
            label = f"[{table.name}.{col_name}] {kind.upper()} -> {target_table}.{target_column}"

            if target_table not in table_map:
                report.errors.append(f"{label}:目标表 '{target_table}' 不存在")
                continue
            if target_column not in col_map[target_table]:
                report.errors.append(f"{label}:目标列 '{target_column}' 不存在")
                continue

            # 外键必须解析到**唯一一行**。目标表主键可重复时,「XX.id = 5」指的是哪一行
            # 没有答案 —— 生成的 FK 助手也拿不到 FindById(那种表不生成它)。
            # 外键列的类型必须与目标键列一致。C++/Go/Java 里 int32 与 uint32 映射到同一个
            # 整型所以静默放行,而 C# 跟着 protoc 走(int vs uint),类型不一致会直接编译不过。
            # 这本来就是策划填错类型,不该由某一门语言去替大家发现。
            tgt_cols = {c.name: c for c in table_map[target_table].columns}
            src_col = next((c for c in table.columns if c.name == col_name), None)
            tgt_col = tgt_cols.get(target_column)
            if src_col is not None and tgt_col is not None and src_col.data_type != tgt_col.data_type:
                report.errors.append(
                    f"{label}:类型不一致 —— 本列是 {src_col.data_type},"
                    f"{target_table}.{target_column} 是 {tgt_col.data_type}。"
                    f"外键两端类型必须相同(C# 会因此编译不过,其余语言只是静默通过)。")
                continue

            if target_column == "id" and table_map[target_table].multi_primary_key:
                report.errors.append(
                    f"{label}:目标表 {target_table} 的主键声明了 (cfg_multi)(id 可重复),"
                    f"不能被外键引用 —— 外键要求唯一解析。"
                    f"要么去掉那边的 (cfg_multi),要么把这个引用指向该表的某个唯一键列。")
                continue

            keys = _target_keys(table_map[target_table], target_column, cfg, rows_cache, key_cache)
            if not keys:
                report.warnings.append(f"{label}:目标表没有可校验的取值(空表?),跳过数据层校验")
                continue

            values = _column_values(table, col_name, cfg, rows_cache)
            missing = _missing_values(values, keys)
            if missing:
                shown = missing[:_MAX_REPORTED_VALUES]
                suffix = f" 等 {len(missing)} 个" if len(missing) > len(shown) else ""
                report.errors.append(
                    f"{label}:{len(missing)} 个取值在目标表中不存在:"
                    f"{', '.join(str(v) for v in shown)}{suffix}"
                )

    return report


# ---------------------------------------------------------------------------
# Internal — 取值读取
# ---------------------------------------------------------------------------

def _table_rows(table: TableSchema, cfg: ExporterConfig, cache: dict[str, list[tuple]]) -> list[tuple]:
    """读一次工作表的全部数据行并缓存(openpyxl 打开工作簿是这里最贵的一步)。"""
    rows = cache.get(table.name)
    if rows is None:
        ws = open_worksheet(table)
        rows = list(ws.iter_rows(min_row=cfg.data_begin_row, values_only=True))
        cache[table.name] = rows
    return rows


def _normalize(value: Any) -> Optional[Any]:
    """把单元格值归一到可比较的形态;空值返回 None。

    openpyxl 会把整数读成 float(1 → 1.0),两侧不归一就会全判失配。
    """
    if value is None:
        return None
    if isinstance(value, bool):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    if isinstance(value, str):
        text = value.strip()
        return text or None
    return value


def _column_indices(table: TableSchema, column_name: str) -> list[int]:
    """同名列可能占多列(repeated / set / map_key 展开后名字相同),全都要查。"""
    return [c.excel_index for c in table.columns if c.name == column_name]


def _column_values(
    table: TableSchema,
    column_name: str,
    cfg: ExporterConfig,
    rows_cache: dict[str, list[tuple]],
) -> list[Any]:
    """取某个逻辑列的全部非空取值(含重复,便于报告里体现填错了几处)。"""
    indices = _column_indices(table, column_name)
    if not indices:
        return []
    values: list[Any] = []
    for row in _table_rows(table, cfg, rows_cache):
        for idx in indices:
            if idx >= len(row):
                continue
            v = _normalize(row[idx])
            if v is not None:
                values.append(v)
    return values


def _target_keys(
    target: TableSchema,
    column_name: str,
    cfg: ExporterConfig,
    rows_cache: dict[str, list[tuple]],
    key_cache: dict[tuple[str, str], set[Any]],
) -> set[Any]:
    """目标列的取值集合。

    同时放入原值和它的字符串形式:配表里同一个 ID 在两张表里可能一边存成数字、
    一边存成文本,不做这层兼容会误报。
    """
    cache_key = (target.name, column_name)
    keys = key_cache.get(cache_key)
    if keys is None:
        keys = set()
        for v in _column_values(target, column_name, cfg, rows_cache):
            keys.add(v)
            keys.add(str(v))
        key_cache[cache_key] = keys
    return keys


def _missing_values(values: list[Any], keys: set[Any]) -> list[Any]:
    """返回去重、稳定排序后的失配取值。"""
    missing: set[Any] = set()
    for v in values:
        if v in _EMPTY_REFS:
            continue
        if v in keys or str(v) in keys:
            continue
        missing.add(v)
    return sorted(missing, key=str)
