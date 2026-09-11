"""ActivitySchedule 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 ActivitySchedule 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 activityschedule_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import activityschedule_table_pb2 as _pb
import mission_table_pb2 as _pb_mission

from .activityschedule_table import ActivityScheduleTableManager
from .mission_table import MissionTableManager

Row = _pb.ActivityScheduleTable


def get_id_row(row: Row) -> _pb_mission.MissionTable | None:
    """解析 ActivitySchedule.id -> Mission 行；解析不到返回 ``None``。"""
    return MissionTableManager.instance().find_by_id(row.id)


def get_id_row_by_id(table_id: int) -> _pb_mission.MissionTable | None:
    """同上，入参换成 ActivitySchedule 自己的 id。"""
    row = ActivityScheduleTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_id(key: int) -> tuple[Row, ...]:
    """反查：全部 id == key 的 ActivitySchedule 行。走的是已建好的二级索引。"""
    return ActivityScheduleTableManager.instance().get_by_id(key)
