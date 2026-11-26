#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""战前技能禁选（每方 3 Ban · 方案 A）配表与 Tip 幂等增量补丁脚本。

改动目标（docs/planning/battle-prep-skill-ban-v1.md）：
1. data/Skill.xlsx：末尾追加 ban_pool_category 列（0=豁免，1=控场障碍，2=爆发与毒火，3=辅助/护盾/绝学，4=灵宠策略）并标定 51 常驻 + 8 赛季主动技。
2. data/RankedRule.xlsx：末尾追加 skill_ban_slots_per_team(=3) 与 skill_ban_min_rating_centi(=160000)。
3. data/TournamentRule.xlsx：末尾追加 skill_ban_slots_per_team(=3)。
4. data/tip/Tip.xlsx：在 //skill_error 末尾追加 SkillBannedInBattle。
"""

from __future__ import annotations

from pathlib import Path
import openpyxl

REPO_ROOT = Path(__file__).resolve().parents[2]
DATA_DIR = REPO_ROOT / "data"

BAN_POOL_CATEGORY_BY_SKILL_ID: dict[int, int] = {
    # 1 = 控场障碍（10）
    10020: 1, 10021: 1, 10022: 1, 10023: 1, 10024: 1, 10038: 1,
    10101: 1, 10102: 1, 10103: 1, 10104: 1,
    # 2 = 爆发与毒火（15 常驻 + S1/S2 各 2）
    10003: 2, 10008: 2, 10039: 2, 10019: 2, 10029: 2, 10030: 2, 10031: 2,
    10105: 2, 10106: 2, 10111: 2, 10112: 2, 10113: 2, 10114: 2, 10115: 2, 10116: 2,
    15001: 2, 15002: 2, 15101: 2, 15102: 2,
    # 3 = 辅助、护盾与绝学（16 常驻 + S1/S2 各 2）
    10012: 3, 10014: 3, 10016: 3, 10025: 3, 10026: 3, 10027: 3,
    10033: 3, 10034: 3, 10035: 3, 10036: 3, 10040: 3, 10041: 3,
    10107: 3, 10108: 3, 10109: 3, 10110: 3,
    15003: 3, 15004: 3, 15103: 3, 15104: 3,
    # 4 = 灵宠策略（10）
    11005: 4, 11006: 4, 11007: 4, 11008: 4, 11009: 4,
    11013: 4, 11014: 4, 11016: 4, 11101: 4, 11102: 4,
}


def find_or_append_column(ws, col_name: str, comment_text: str) -> tuple[int, bool]:
    headers = {str(ws.cell(1, c).value).strip(): c for c in range(1, ws.max_column + 1) if ws.cell(1, c).value is not None}
    if col_name in headers:
        return headers[col_name], False
    col_idx = ws.max_column + 1
    ws.cell(1, col_idx, col_name)
    ws.cell(5, col_idx, comment_text)
    return col_idx, True


def patch_skill_xlsx() -> None:
    path = DATA_DIR / "Skill.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    col_idx, created = find_or_append_column(
        ws, "ban_pool_category", "竞技战前禁选分类：0豁免，1控场，2爆发毒火，3辅助绝学，4灵宠策略"
    )
    updated = 0
    seen: set[int] = set()
    for row_idx in range(6, ws.max_row + 1):
        raw_id = ws.cell(row_idx, 1).value
        if raw_id is None or raw_id == "":
            continue
        skill_id = int(raw_id)
        desired = BAN_POOL_CATEGORY_BY_SKILL_ID.get(skill_id, 0)
        if skill_id in BAN_POOL_CATEGORY_BY_SKILL_ID:
            seen.add(skill_id)
        current = ws.cell(row_idx, col_idx).value
        if desired != 0:
            if current != desired:
                ws.cell(row_idx, col_idx, desired)
                updated += 1
        elif current not in (None, "", 0):
            ws.cell(row_idx, col_idx, 0)
            updated += 1
    missing = sorted(set(BAN_POOL_CATEGORY_BY_SKILL_ID) - seen)
    if missing:
        raise RuntimeError(f"Skill.xlsx missing expected skill IDs: {missing}")
    if created or updated > 0:
        wb.save(path)
        print(f"[DONE] Skill.xlsx: col={col_idx}, created={created}, updated={updated}, marked={len(seen)}")
    else:
        print(f"[ALREADY CURRENT] Skill.xlsx: marked={len(seen)}")


def patch_single_row_rule(filename: str, columns: list[tuple[str, str, int]]) -> None:
    path = DATA_DIR / filename
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    changed = False
    for col_name, comment_text, value in columns:
        col_idx, created = find_or_append_column(ws, col_name, comment_text)
        if created:
            changed = True
        if ws.cell(6, col_idx).value != value:
            ws.cell(6, col_idx, value)
            changed = True
    if changed:
        wb.save(path)
        print(f"[DONE] {filename}: updated {[c[0] for c in columns]}")
    else:
        print(f"[ALREADY CURRENT] {filename}")


def patch_tip_xlsx() -> None:
    path = DATA_DIR / "tip" / "Tip.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    target_key = "SkillBannedInBattle"
    target_msg = "该技能本局已被禁用"
    for row_idx in range(1, ws.max_row + 1):
        val = ws.cell(row_idx, 1).value
        if isinstance(val, str) and val.strip() == target_key:
            print(f"[ALREADY CURRENT] Tip.xlsx: {target_key} at row {row_idx}")
            return
    insert_at = None
    in_skill_section = False
    for row_idx in range(1, ws.max_row + 1):
        val = ws.cell(row_idx, 1).value
        if isinstance(val, str):
            text = val.strip()
            if text.startswith("//skill_error"):
                in_skill_section = True
            elif in_skill_section and text.startswith("//"):
                insert_at = row_idx
                break
    if insert_at is None:
        raise RuntimeError("Tip.xlsx: failed to locate end of //skill_error section")
    ws.insert_rows(insert_at, 1)
    ws.cell(insert_at, 1, target_key)
    ws.cell(insert_at, 2, target_msg)
    wb.save(path)
    print(f"[DONE] Tip.xlsx: inserted {target_key} at row {insert_at}")


RANKED_RULE_COLUMNS: list[tuple[str, str, int]] = [
    ("skill_ban_slots_per_team", "正式论剑每队战前禁选槽数(3)", 3),
    ("skill_ban_min_rating_centi", "正式论剑开启3Ban全员最低段位分×100(160000云章)", 160000),
]

TOURNAMENT_RULE_COLUMNS: list[tuple[str, str, int]] = [
    ("skill_ban_slots_per_team", "锦标赛每队战前禁选槽数(3)", 3),
]


def main() -> None:
    patch_skill_xlsx()
    patch_single_row_rule("RankedRule.xlsx", RANKED_RULE_COLUMNS)
    patch_single_row_rule("TournamentRule.xlsx", TOURNAMENT_RULE_COLUMNS)
    patch_tip_xlsx()


if __name__ == "__main__":
    main()
