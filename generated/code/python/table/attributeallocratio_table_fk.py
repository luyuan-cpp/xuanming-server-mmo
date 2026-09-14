"""AttributeAllocRatio 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 AttributeAllocRatio 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 attributeallocratio_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import attributeallocratio_table_pb2 as _pb
import attributedimension_table_pb2 as _pb_attributedimension

from .attributeallocratio_table import AttributeAllocRatioTableManager
from .attributedimension_table import AttributeDimensionTableManager

Row = _pb.AttributeAllocRatioTable


def get_dimension_id_row(row: Row) -> _pb_attributedimension.AttributeDimensionTable | None:
    """解析 AttributeAllocRatio.dimension_id -> AttributeDimension 行；解析不到返回 ``None``。"""
    return AttributeDimensionTableManager.instance().find_by_id(row.dimension_id)


def get_dimension_id_row_by_id(table_id: int) -> _pb_attributedimension.AttributeDimensionTable | None:
    """同上，入参换成 AttributeAllocRatio 自己的 id。"""
    row = AttributeAllocRatioTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_dimension_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_dimension_id(key: int) -> tuple[Row, ...]:
    """反查：全部 dimension_id == key 的 AttributeAllocRatio 行。走的是已建好的二级索引。"""
    return AttributeAllocRatioTableManager.instance().get_by_dimension_id(key)
