"""Mission 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 Mission 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 mission_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import mission_table_pb2 as _pb
import condition_table_pb2 as _pb_condition
import reward_table_pb2 as _pb_reward

from .mission_table import MissionTableManager
from .condition_table import ConditionTableManager
from .reward_table import RewardTableManager

Row = _pb.MissionTable


def get_reward_id_row(row: Row) -> _pb_reward.RewardTable | None:
    """解析 Mission.reward_id -> Reward 行；解析不到返回 ``None``。"""
    return RewardTableManager.instance().find_by_id(row.reward_id)


def get_reward_id_row_by_id(table_id: int) -> _pb_reward.RewardTable | None:
    """同上，入参换成 Mission 自己的 id。"""
    row = MissionTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_reward_id_row(row)


def get_condition_id_rows(row: Row) -> tuple[_pb_condition.ConditionTable, ...]:
    """解析组外键 Mission.condition_id[] -> Condition 行。"""
    manager = ConditionTableManager.instance()
    result: list[_pb_condition.ConditionTable] = []
    for fk_id in row.condition_id:
        target_row = manager.find_by_id(fk_id)
        if target_row is not None:
            result.append(target_row)
    return tuple(result)


def get_condition_id_rows_by_id(table_id: int) -> tuple[_pb_condition.ConditionTable, ...]:
    """同上，入参换成 Mission 自己的 id。源行不存在时返回空元组。"""
    row = MissionTableManager.instance().find_by_id(table_id)
    if row is None:
        return ()
    return get_condition_id_rows(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_reward_id(key: int) -> tuple[Row, ...]:
    """反查：全部 reward_id == key 的 Mission 行。走的是已建好的二级索引。"""
    return MissionTableManager.instance().get_by_reward_id(key)
