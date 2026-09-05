"""AttributeDimension 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 AttributeDimension 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 attributedimension_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import attributedimension_table_pb2 as _pb
import attributepool_table_pb2 as _pb_attributepool

from .attributedimension_table import AttributeDimensionTableManager
from .attributepool_table import AttributePoolTableManager

Row = _pb.AttributeDimensionTable


def get_pool_id_row(row: Row) -> _pb_attributepool.AttributePoolTable | None:
    """解析 AttributeDimension.pool_id -> AttributePool 行；解析不到返回 ``None``。"""
    return AttributePoolTableManager.instance().find_by_id(row.pool_id)


def get_pool_id_row_by_id(table_id: int) -> _pb_attributepool.AttributePoolTable | None:
    """同上，入参换成 AttributeDimension 自己的 id。"""
    row = AttributeDimensionTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_pool_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_pool_id(key: int) -> tuple[Row, ...]:
    """反查：全部 pool_id == key 的 AttributeDimension 行。走的是已建好的二级索引。"""
    return AttributeDimensionTableManager.instance().get_by_pool_id(key)
