#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""《五行奇谈》反「上班打卡」整治 V1 源配表幂等补丁脚本。

设计文档：docs/planning/anti-chore-remediation-v1.md。
严格从磁盘实时加载各 .xlsx，仅精准改写目标行/单元格，绝不整表覆写并行会话未提交改动。
"""

from __future__ import annotations

import argparse
from pathlib import Path

import openpyxl

REPO_ROOT = Path(__file__).resolve().parents[2]
DATA_DIR = REPO_ROOT / "data"
DATA_BEGIN_ROW = 6


def col_map(ws) -> dict[str, int]:
    return {
        ws.cell(1, c).value: c
        for c in range(1, ws.max_column + 1)
        if ws.cell(1, c).value is not None
    }


def save_if_changed(wb, path: Path, changed: bool, dry_run: bool) -> None:
    rel = path.relative_to(REPO_ROOT)
    if not changed:
        print(f"[OK/UNCHANGED] {rel}")
        return
    if dry_run:
        print(f"[DRY-RUN] would update {rel}")
        return
    wb.save(path)
    print(f"[UPDATED] {rel}")


def patch_guild_donate(dry_run: bool) -> None:
    path = DATA_DIR / "GuildDonate.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    target = {
        "id": 1,
        "name": "随缘银两添香",
        "currency_type": 0,
        "cost_amount": 10000,
        "contribution_gain": 20,
        "funds_gain": 2000,
        "daily_limit": 3,
        "min_guild_level": 1,
    }
    changed = False
    # 删除 id=2(大捐) 与 id=3(灵石捐献)，仅保留单一轻量自愿银两周限档
    for r in range(ws.max_row, DATA_BEGIN_ROW - 1, -1):
        rid = ws.cell(r, cols["id"]).value
        if rid != 1:
            ws.delete_rows(r, 1)
            changed = True
    # 更新 id=1 行
    for k, v in target.items():
        cell = ws.cell(DATA_BEGIN_ROW, cols[k])
        if cell.value != v:
            cell.value = v
            changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_guild_activity(dry_run: bool) -> None:
    path = DATA_DIR / "GuildActivity.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        rid = ws.cell(r, cols["id"]).value
        if rid in (1, 2, 3):
            cell = ws.cell(r, cols["enabled"])
            if cell.value not in (False, 0, "false", "FALSE"):
                cell.value = False
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_guild_level(dry_run: bool) -> None:
    path = DATA_DIR / "GuildLevel.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    costs = {
        1: 5000,
        2: 12000,
        3: 25000,
        4: 45000,
        5: 75000,
        6: 115000,
        7: 170000,
        8: 240000,
        9: 320000,
        10: 0,
    }
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        rid = ws.cell(r, cols["id"]).value
        if rid in costs:
            cell = ws.cell(r, cols["upgrade_cost_funds"])
            if cell.value != costs[rid]:
                cell.value = costs[rid]
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_guild_shop(dry_run: bool) -> None:
    path = DATA_DIR / "GuildShop.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    # 彻底清零每日限购(1)；珍稀/外观项改终身限购(3)；常规补给不限购(0)；周耗材温和减负(2)
    limits = {
        101: (0, 0),  # 培元丹：不限购
        102: (0, 0),  # 回灵散：不限购
        103: (0, 0),  # 精炼石：不限购
        104: (2, 3),  # 修行秘录残页：每周 3
        105: (2, 5),  # 重铸砂：每周 5
        201: (0, 0),  # 帮会令牌：不限购
        202: (3, 1),  # 玄铁护符：终身 1
        203: (0, 0),  # 灵兽口粮：不限购
        204: (3, 1),  # 藏经阁手札：终身 1
        205: (2, 6),  # 悟道残页：每周 6
        206: (2, 1),  # 定星保底符：每周 1
        301: (0, 0),  # 花灯：不限购
        302: (0, 0),  # 月饼礼盒：不限购
        303: (0, 0),  # 同心结：不限购
        304: (3, 2),  # 自用法宝拓印：终身 2
        305: (3, 1),  # 角色形象卡：终身 1（修正误配周限）
        306: (3, 1),  # 天工秘墨：终身 1
    }
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        rid = ws.cell(r, cols["id"]).value
        if rid in limits:
            period, count = limits[rid]
            c_period = ws.cell(r, cols["limit_period"])
            c_count = ws.cell(r, cols["limit_count"])
            if c_period.value != period:
                c_period.value = period
                changed = True
            if c_count.value != count:
                c_count.value = count
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_guild_war_milestone(dry_run: bool) -> None:
    path = DATA_DIR / "GuildWarMilestone.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    merits = {1: 200, 2: 600, 3: 1200, 4: 2000}
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        rid = ws.cell(r, cols["id"]).value
        if rid in merits:
            cell = ws.cell(r, cols["required_guild_merit"])
            if cell.value != merits[rid]:
                cell.value = merits[rid]
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_guild_war_rule(dry_run: bool) -> None:
    path = DATA_DIR / "GuildWarRule.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        if ws.cell(r, cols["id"]).value == 1:
            cell = ws.cell(r, cols["dividend_min_personal_merit"])
            if cell.value != 15:
                cell.value = 15
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_rewarded_ad_slot(dry_run: bool) -> None:
    path = DATA_DIR / "RewardedAdSlot.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        if ws.cell(r, cols["id"]).value == 1:
            # 严守零拒审合规红线：enabled 必须保持 0，currency_type 必须保持 3
            assert ws.cell(r, cols["enabled"]).value == 0, "RewardedAdSlot.enabled MUST remain 0!"
            assert ws.cell(r, cols["currency_type"]).value == 3, "RewardedAdSlot.currency_type MUST remain 3!"
            cell = ws.cell(r, cols["daily_limit"])
            if cell.value != 14:
                cell.value = 14
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_tournament_rule(dry_run: bool) -> None:
    path = DATA_DIR / "TournamentRule.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    cols = col_map(ws)
    changed = False
    for r in range(DATA_BEGIN_ROW, ws.max_row + 1):
        if ws.cell(r, cols["id"]).value == 1:
            cell = ws.cell(r, cols["knockout_round_timeout_seconds"])
            if cell.value != 14400:
                cell.value = 14400
                changed = True
    save_if_changed(wb, path, changed, dry_run)


def patch_tip_xlsx(dry_run: bool) -> None:
    path = DATA_DIR / "tip" / "Tip.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    updates = {
        "GuildDonateLimit": "本周该项捐献次数已用完",
        "RewardedAdDailyLimitReached": "本周观影小礼已领完，下周一 05:00 重置",
    }
    found = set()
    changed = False
    for r in range(1, ws.max_row + 1):
        key = ws.cell(r, 1).value
        if isinstance(key, str) and key.strip() in updates:
            k = key.strip()
            found.add(k)
            cell = ws.cell(r, 2)
            if cell.value != updates[k]:
                cell.value = updates[k]
                changed = True
    missing = set(updates.keys()) - found
    if missing:
        raise RuntimeError(f"Tip.xlsx missing expected keys: {missing}")
    save_if_changed(wb, path, changed, dry_run)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    patch_guild_donate(args.dry_run)
    patch_guild_activity(args.dry_run)
    patch_guild_level(args.dry_run)
    patch_guild_shop(args.dry_run)
    patch_guild_war_milestone(args.dry_run)
    patch_guild_war_rule(args.dry_run)
    patch_rewarded_ad_slot(args.dry_run)
    patch_tournament_rule(args.dry_run)
    patch_tip_xlsx(args.dry_run)


if __name__ == "__main__":
    main()
