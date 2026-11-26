#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""归元城原创场景 NPC 与人物系统联动玩法（V1）幂等配表补丁脚本。

仅追加新 ID 行与清理存量人物志旧地名，绝不删除或重排任何存量主键：
  1. data/Monster.xlsx             : 追加演武试招木桩（301..304）、回合残局（311..316）、人物奇谈（321..323），0 掉落 / 0 经验 / 0 银两 / 0 捕宠
  2. data/Dungeon.xlsx             : 追加演武试招木桩（301/302, boss_trial_kind=9）、残局考题（311..313, boss_trial_kind=10）、人物奇谈（321..323, boss_trial_kind=11）
  3. data/PlayMode.xlsx            : 追加对应 PVE_SOLO 入口（451/452、461..463、471..473）
  4. data/CharacterAppearance.xlsx : 清理沈澄（id=10）传记内遗留旧地名「八仙岛」→「八景岛」
"""

from __future__ import annotations

import sys
from pathlib import Path
import openpyxl

REPO_ROOT = Path(__file__).resolve().parents[2]
DATA_DIR = REPO_ROOT / "data"

# Monster 列序（19 列）:
# id, designer, health, strength, armor, resistance, critchance, speed,
# drop(item1, cnt1, rate1, item2, cnt2, rate2),
# exp_reward, gold_reward, capture_pet_table_id, capture_rate, is_boss
MONSTER_ROWS = [
    # 演武试招木桩（单桩高防破甲测试 + 三才群攻控场测试；极低攻、0 掉落/0 经验/0 银两）
    (301, None, 6000, 8,  90, 20, 0,  60,  None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (302, None, 3200, 10, 65, 18, 0,  90,  None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (303, None, 2200, 10, 45, 12, 0,  85,  None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (304, None, 2200, 10, 45, 12, 0,  80,  None, None, None, None, None, None, 0, 0, 0, 0, 0),
    # 演武回合残局 3 关（5 回合快节奏解谜：破甲试锋 / 护主保宠 / 先手断疗）
    (311, None, 1350, 38, 72, 15, 5,  190, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (312, None, 620,  34, 28, 8,  5,  240, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (313, None, 1450, 56, 42, 12, 10, 210, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (314, None, 720,  30, 36, 12, 0,  180, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (315, None, 1280, 42, 48, 18, 5,  220, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (316, None, 860,  32, 40, 16, 0,  195, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    # 归元人物奇谈短案 3 篇（留灯夜话 / 药庐双结 / 月汐渡口）
    (321, None, 1680, 40, 44, 14, 5,  205, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (322, None, 1820, 44, 48, 18, 5,  215, None, None, None, None, None, None, 0, 0, 0, 0, 0),
    (323, None, 1960, 48, 50, 16, 8,  225, None, None, None, None, None, None, 0, 0, 0, 0, 0),
]

# Dungeon 列序（11 列）:
# id, scene_id, max_team_size, time_limit, monster1, monster2, monster3,
# allow_pet_capture, boss_trial_kind, difficulty, round_limit
DUNGEON_ROWS = [
    # 9 = 演武试招木桩（归元城 scene_id=1，强制本人真实养成，0 战损/0 奖励）
    (301, 1, 1, 600, 301, None, None, 0, 9,  1, 8),
    (302, 1, 1, 600, 302, 303,  304,  0, 9,  2, 8),
    # 10 = 回合残局考题（归元城 scene_id=1，可选本人养成或免费 3×3 预设，0 刷取）
    (311, 1, 1, 600, 311, 312,  None, 0, 10, 1, 5),
    (312, 1, 1, 600, 313, 314,  None, 0, 10, 1, 5),
    (313, 1, 1, 600, 315, 316,  None, 0, 10, 1, 5),
    # 11 = 归元人物奇谈短案（归元城 scene_id=1，可选本人养成或免费 3×3 预设试演，0 刷取）
    (321, 1, 1, 600, 321, None, None, 0, 11, 1, 10),
    (322, 1, 1, 600, 322, None, None, 0, 11, 1, 10),
    (323, 1, 1, 600, 323, None, None, 0, 11, 1, 10),
]

# PlayMode 列序（10 列）:
# id, match_mode, battle_config_id, enabled, allowed1..5, team_size_max
PLAYMODE_ROWS = [
    (451, 4, 301, True, None, None, None, None, None, 1),
    (452, 4, 302, True, None, None, None, None, None, 1),
    (461, 4, 311, True, None, None, None, None, None, 1),
    (462, 4, 312, True, None, None, None, None, None, 1),
    (463, 4, 313, True, None, None, None, None, None, 1),
    (471, 4, 321, True, None, None, None, None, None, 1),
    (472, 4, 322, True, None, None, None, None, None, 1),
    (473, 4, 323, True, None, None, None, None, None, 1),
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
    print(f"[guiyuan_npc_gameplay_xlsx_patch] {filename}: added={added}, total_target={len(rows)}")
    return added


def sanitize_appearance_legacy_map_names() -> int:
    path = DATA_DIR / "CharacterAppearance.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    changed = 0
    for r in range(6, ws.max_row + 1):
        for c in (6, 12, 13, 14):
            val = ws.cell(r, c).value
            if isinstance(val, str) and "八仙岛" in val:
                ws.cell(r, c, val.replace("八仙岛", "八景岛"))
                changed += 1
    if changed > 0:
        wb.save(path)
    wb.close()
    print(f"[guiyuan_npc_gameplay_xlsx_patch] CharacterAppearance.xlsx: sanitized_cells={changed}")
    return changed


def main() -> int:
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
    sanitize_appearance_legacy_map_names()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
