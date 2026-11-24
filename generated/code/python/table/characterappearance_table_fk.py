"""CharacterAppearance 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 CharacterAppearance 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 characterappearance_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import characterappearance_table_pb2 as _pb
import appearanceseries_table_pb2 as _pb_appearanceseries

from .characterappearance_table import CharacterAppearanceTableManager
from .appearanceseries_table import AppearanceSeriesTableManager

Row = _pb.CharacterAppearanceTable


def get_series_id_row(row: Row) -> _pb_appearanceseries.AppearanceSeriesTable | None:
    """解析 CharacterAppearance.series_id -> AppearanceSeries 行；解析不到返回 ``None``。"""
    return AppearanceSeriesTableManager.instance().find_by_id(row.series_id)


def get_series_id_row_by_id(table_id: int) -> _pb_appearanceseries.AppearanceSeriesTable | None:
    """同上，入参换成 CharacterAppearance 自己的 id。"""
    row = CharacterAppearanceTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_series_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_series_id(key: int) -> tuple[Row, ...]:
    """反查：全部 series_id == key 的 CharacterAppearance 行。走的是已建好的二级索引。"""
    return CharacterAppearanceTableManager.instance().get_by_series_id(key)
