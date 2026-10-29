"""ActivityVitalityReward 的外键跨表查询助手（自动生成，请勿手改）。

DO NOT EDIT — regenerate from Excel via Data Table Exporter.

这里只做一件事：把 ActivityVitalityReward 行里存的**目标表 id** 解析成目标表的行。
解析结果和源行一样属于「当前快照」，同样的热更契约：**只存 id，不存行**。

放在独立模块而不是塞进 activityvitalityreward_table.py，是因为它要 import 目标表的
管理器；写在一起时两张互相引用的表就会在 import 期成环。
"""

from __future__ import annotations

import activityvitalityreward_table_pb2 as _pb
import activityvitalityrule_table_pb2 as _pb_activityvitalityrule
import reward_table_pb2 as _pb_reward

from .activityvitalityreward_table import ActivityVitalityRewardTableManager
from .activityvitalityrule_table import ActivityVitalityRuleTableManager
from .reward_table import RewardTableManager

Row = _pb.ActivityVitalityRewardTable


def get_rule_id_row(row: Row) -> _pb_activityvitalityrule.ActivityVitalityRuleTable | None:
    """解析 ActivityVitalityReward.rule_id -> ActivityVitalityRule 行；解析不到返回 ``None``。"""
    return ActivityVitalityRuleTableManager.instance().find_by_id(row.rule_id)


def get_rule_id_row_by_id(table_id: int) -> _pb_activityvitalityrule.ActivityVitalityRuleTable | None:
    """同上，入参换成 ActivityVitalityReward 自己的 id。"""
    row = ActivityVitalityRewardTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_rule_id_row(row)


def get_reward_id_row(row: Row) -> _pb_reward.RewardTable | None:
    """解析 ActivityVitalityReward.reward_id -> Reward 行；解析不到返回 ``None``。"""
    return RewardTableManager.instance().find_by_id(row.reward_id)


def get_reward_id_row_by_id(table_id: int) -> _pb_reward.RewardTable | None:
    """同上，入参换成 ActivityVitalityReward 自己的 id。"""
    row = ActivityVitalityRewardTableManager.instance().find_by_id(table_id)
    if row is None:
        return None
    return get_reward_id_row(row)


# ---- 反向外键（HasMany）：按外键列的值反查源表的行 ----


def find_rows_by_rule_id(key: int) -> tuple[Row, ...]:
    """反查：全部 rule_id == key 的 ActivityVitalityReward 行。走的是已建好的二级索引。"""
    return ActivityVitalityRewardTableManager.instance().get_by_rule_id(key)


def find_rows_by_reward_id(key: int) -> tuple[Row, ...]:
    """反查：全部 reward_id == key 的 ActivityVitalityReward 行。走的是已建好的二级索引。"""
    return ActivityVitalityRewardTableManager.instance().get_by_reward_id(key)
