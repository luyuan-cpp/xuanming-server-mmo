#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""世界地图玩法 V2（月汐湾全开放 + 三图野生灵宠栖息地捕捉）增量配表脚本（幂等）。

仅向以下 4 张既有 Excel 表追加新 ID 行，绝不修改或删除任何存量行：
  1. data/MysteryMerchantSpot.xlsx : 追加场景 3（月汐湾）3 个经 navigation.json 连通核验的站位（13..15）
  2. data/Monster.xlsx             : 追加三图专属野生可捕捉灵宠怪（201..209）
  3. data/Dungeon.xlsx             : 追加三图专属野外栖息地捕捉模板（202 / 203 / 205，allow_pet_capture=1）
  4. data/PlayMode.xlsx            : 追加对应 PVE_SOLO 入口（402 / 403 / 405）
"""

from __future__ import annotations

import sys
from pathlib import Path
import openpyxl

REPO_ROOT = Path(__file__).resolve().parents[2]
DATA_DIR = REPO_ROOT / "data"

SPOT_ROWS = [
    # id, scene_config_id, x, y, z, desc
    (13, 3, 39.54375, 41.2875, 0, "月汐湾·渔市栈道(Unity 41.29,39.54)"),
    (14, 3, 44.4375, 59.34375, 0, "月汐湾·北码头(Unity 59.34,44.44)"),
    (15, 3, 32.0625, 28.63125, 0, "月汐湾·海湾神龛(Unity 28.63,32.06)"),
]

# Monster 列序（19 列）:
# id, designer, health, strength, armor, resistance, critchance, speed,
# drop(item1, cnt1, rate1, item2, cnt2, rate2),
# exp_reward, gold_reward, capture_pet_table_id, capture_rate, is_boss
MONSTER_ROWS = [
    # 场景 2 八景岛：仙风高敏 / 法系栖息地（云鹤 Pet4 / 竹风狸 Pet13 / 霞角鹿 Pet19）
    (201, None, 520, 42, 44, 10, 5, 310, 20, 1, 3000, 10, 1, 2500, 45, 24, 4,  3000, 0),
    (202, None, 350, 30, 34, 5,  0, 275, 10, 1, 3000, None, None, None, 24, 12, 13, 4000, 0),
    (203, None, 360, 28, 38, 8,  0, 265, 10, 1, 3000, None, None, None, 24, 12, 19, 4000, 0),
    # 场景 3 月汐湾：海潮物攻 / 爆发栖息地（金猊 Pet3 / 灵狐 Pet1 / 符小虎 Pet22）
    (204, None, 560, 46, 46, 5,  5, 285, 20, 1, 3000, 10, 1, 2500, 45, 24, 3,  3000, 0),
    (205, None, 340, 32, 32, 5,  0, 280, 10, 1, 3000, None, None, None, 24, 12, 1,  4000, 0),
    (206, None, 380, 34, 38, 0,  0, 255, 10, 1, 3000, None, None, None, 24, 12, 22, 4000, 0),
    # 场景 5 碧渊水宫：玄水耐打 / 辅助栖息地（玄潮龟 Pet15 / 汐螺 Pet11 / 露华灵 Pet17）
    (207, None, 430, 26, 54, 10, 0, 210, 20, 1, 3000, None, None, None, 26, 14, 15, 4000, 0),
    (208, None, 390, 28, 46, 10, 0, 230, 10, 1, 3000, None, None, None, 24, 12, 11, 4000, 0),
    (209, None, 370, 29, 40, 12, 0, 248, 10, 1, 3000, None, None, None, 24, 12, 17, 4000, 0),
    # 灵潮珍兽潮（M2-Tide：仅当该野外图处于灵潮涌动窗口可遇，含各图标志性 cult=3 珍稀灵宠与提升捕获率 50%–55%）
    # 场景 2 八景岛灵潮：岚铎仙(Pet10, cult=3) / 砚羽灵(Pet9) / 绯耳灵(Pet18)
    (211, None, 580, 44, 48, 12, 5, 320, 20, 1, 4000, 10, 1, 3500, 56, 30, 10, 5000, 0),
    (212, None, 410, 34, 38, 10, 5, 290, 10, 1, 3500, None, None, None, 32, 18, 9,  5500, 0),
    (213, None, 400, 32, 40, 10, 0, 285, 10, 1, 3500, None, None, None, 32, 18, 18, 5500, 0),
    # 场景 3 月汐湾灵潮：苍嶂麟(Pet12, cult=3) / 赤砂魁(Pet14) / 烛翎(Pet5)
    (214, None, 620, 48, 52, 10, 5, 295, 20, 1, 4000, 10, 1, 3500, 56, 30, 12, 5000, 0),
    (215, None, 430, 38, 42, 6,  5, 275, 10, 1, 3500, None, None, None, 32, 18, 14, 5500, 0),
    (216, None, 390, 36, 36, 8,  5, 300, 10, 1, 3500, None, None, None, 32, 18, 5,  5500, 0),
    # 场景 5 碧渊水宫灵潮：月弦师(Pet16, cult=3) / 霜团貂(Pet7) / 绛铃(Pet6)
    (217, None, 560, 40, 54, 15, 5, 270, 20, 1, 4000, 10, 1, 3500, 56, 30, 16, 5000, 0),
    (218, None, 410, 33, 44, 12, 0, 265, 10, 1, 3500, None, None, None, 32, 18, 7,  5500, 0),
    (219, None, 400, 34, 42, 14, 0, 260, 10, 1, 3500, None, None, None, 32, 18, 6,  5500, 0),
]

# Dungeon 列序（11 列）:
# id, scene_id, max_team_size, time_limit, monster1, monster2, monster3,
# allow_pet_capture, boss_trial_kind, difficulty, round_limit
DUNGEON_ROWS = [
    (202, 2, 1, 900, 201, 202, 203, 1, 0, 0, 15),
    (203, 3, 1, 900, 204, 205, 206, 1, 0, 0, 15),
    (205, 5, 1, 900, 207, 208, 209, 1, 0, 0, 15),
    # 灵潮珍兽潮专属副本（ID 210 + scene_id，服务端叠加校验灵潮窗口）
    (212, 2, 1, 900, 211, 212, 213, 1, 0, 0, 15),
    (213, 3, 1, 900, 214, 215, 216, 1, 0, 0, 15),
    (215, 5, 1, 900, 217, 218, 219, 1, 0, 0, 15),
]

# PlayMode 列序（10 列）:
# id, match_mode, battle_config_id, enabled, allowed1..5, team_size_max
PLAYMODE_ROWS = [
    (402, 4, 202, True, None, None, None, None, None, 1),
    (403, 4, 203, True, None, None, None, None, None, 1),
    (405, 4, 205, True, None, None, None, None, None, 1),
    (412, 4, 212, True, None, None, None, None, None, 1),
    (413, 4, 213, True, None, None, None, None, None, 1),
    (415, 4, 215, True, None, None, None, None, None, 1),
]


def append_missing_rows(filename: str, expected_headers: tuple[str, ...], rows: list[tuple]) -> int:
    path = DATA_DIR / filename
    wb = openpyxl.load_workbook(path)
    if len(wb.sheetnames) != 1:
        sys.exit(f"{filename}: expected 1 sheet, got {wb.sheetnames}")
    ws = wb.active
    actual_headers = tuple(ws.cell(1, c).value for c in range(1, len(expected_headers) + 1))
    if actual_headers != expected_headers:
        sys.exit(f"{filename}: header mismatch\n  actual={actual_headers}\n  expect={expected_headers}")

    existing_ids: dict[int, int] = {}
    last_data_row = 5
    for r in range(6, ws.max_row + 1):
        val = ws.cell(r, 1).value
        if val is not None:
            existing_ids[int(val)] = r
            last_data_row = r

    added = 0
    for row in rows:
        row_id = int(row[0])
        if row_id in existing_ids:
            continue
        last_data_row += 1
        for col_idx, cell_val in enumerate(row, 1):
            ws.cell(last_data_row, col_idx, cell_val)
        added += 1

    if added > 0:
        wb.save(path)
    wb.close()
    print(f"[world_map_habitat_xlsx_patch] {filename}: added={added}, total_target={len(rows)}")
    return added


def main() -> int:
    append_missing_rows(
        "MysteryMerchantSpot.xlsx",
        ("id", "scene_config_id", "x", "y", "z", "desc"),
        SPOT_ROWS,
    )
    append_missing_rows(
        "Monster.xlsx",
        ("id", "designer", "health", "strength", "armor", "resistance", "critchance", "speed",
         "drop", None, None, None, None, None, "exp_reward", "gold_reward",
         "capture_pet_table_id", "capture_rate", "is_boss"),
        MONSTER_ROWS,
    )
    append_missing_rows(
        "Dungeon.xlsx",
        ("id", "scene_id", "max_team_size", "time_limit", "monster", None, None,
         "allow_pet_capture", "boss_trial_kind", "difficulty", "round_limit"),
        DUNGEON_ROWS,
    )
    append_missing_rows(
        "PlayMode.xlsx",
        ("id", "match_mode", "battle_config_id", "enabled",
         "allowed_team_sizes", None, None, None, None, "team_size_max"),
        PLAYMODE_ROWS,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
