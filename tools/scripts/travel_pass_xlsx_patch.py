#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""游历手记（不肝不逼氪双轨活动，docs/design/activity-travel-pass-and-showcase-v1.md）xlsx 幂等补丁脚本。

用法（仓库根目录执行）：
    python tools/scripts/travel_pass_xlsx_patch.py new-tables       # ① 建 TravelPassSeason.xlsx 与 TravelPassTier.xlsx
    python tools/scripts/travel_pass_xlsx_patch.py tip-codes        # ② Tip.xlsx 新开 //travel_pass_error (38000 段)
    python tools/scripts/travel_pass_xlsx_patch.py message-limiter  # ③ 跑完 proto-gen 后为 4 条新 RPC 追加 MessageLimiter 行
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    import openpyxl
    from equip_xlsx_patch import (
        COMMENT_ROW,
        DATA_BEGIN_ROW,
        DATA_DIR,
        HEADER_ROW,
        REPO_ROOT,
        TIP_STATE,
        TIP_XLSX,
        Patch,
        apply_patches_and_verify,
        bind,
        fail,
        first_difference,
        only_sheet,
        rel,
        schema_layout,
        sheet_matrix,
        tip_parse,
    )
except ImportError as exc:  # pragma: no cover
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


SEASON_SHEET = "TravelPassSeason"
SEASON_ROWS = [
    dict(
        id=1,
        enabled=1,
        name="青云游历手记 · 第一期",
        description="零每日打卡、正常打试炼/会武/瑞兽自动涨游历印；免费轨领满绑定实用礼，可先玩满再自愿解锁 ¥18 典藏外观轨，解锁后永不过期。",
        icon_key="UI/Ugui/TravelPassV1/travel_pass_card_icon",
        banner_asset_key="UI/Ugui/TravelPassV1/travel_pass_banner",
        starts_at_ms=0,
        ends_at_ms=0,
        max_points=300,
        pve_win_points=20,
        hard_or_pvp_win_points=30,
        pvp_qualified_other_points=15,
        premium_currency_type=1,
        premium_price=180,
        config_revision=1,
    ),
]

TIER_SHEET = "TravelPassTier"
TIER_ROWS = [
    dict(
        id=101,
        pass_id=1,
        tier_index=1,
        required_points=30,
        free_item_id=0,
        free_item_count=0,
        free_gold=5000,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3405,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="frame_travel_qingyun_s1",
        premium_cosmetic_name="青云游历头像框",
    ),
    dict(
        id=102,
        pass_id=1,
        tier_index=2,
        required_points=60,
        free_item_id=3403,
        free_item_count=2,
        free_gold=0,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=0,
        premium_item_count=0,
        premium_gold=10000,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=103,
        pass_id=1,
        tier_index=3,
        required_points=90,
        free_item_id=3406,
        free_item_count=10,
        free_gold=0,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3401,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=104,
        pass_id=1,
        tier_index=4,
        required_points=120,
        free_item_id=0,
        free_item_count=0,
        free_gold=8000,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3403,
        premium_item_count=3,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=105,
        pass_id=1,
        tier_index=5,
        required_points=150,
        free_item_id=3405,
        free_item_count=1,
        free_gold=0,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3402,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=106,
        pass_id=1,
        tier_index=6,
        required_points=180,
        free_item_id=3403,
        free_item_count=2,
        free_gold=0,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3405,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=107,
        pass_id=1,
        tier_index=7,
        required_points=210,
        free_item_id=0,
        free_item_count=0,
        free_gold=10000,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3407,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=108,
        pass_id=1,
        tier_index=8,
        required_points=240,
        free_item_id=3406,
        free_item_count=15,
        free_gold=0,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3403,
        premium_item_count=3,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=109,
        pass_id=1,
        tier_index=9,
        required_points=270,
        free_item_id=3403,
        free_item_count=2,
        free_gold=0,
        free_cosmetic_key="",
        free_cosmetic_name="",
        premium_item_id=3408,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="",
        premium_cosmetic_name="",
    ),
    dict(
        id=110,
        pass_id=1,
        tier_index=10,
        required_points=300,
        free_item_id=0,
        free_item_count=0,
        free_gold=15000,
        free_cosmetic_key="title_travel_free_s1",
        free_cosmetic_name="游历初成",
        premium_item_id=3402,
        premium_item_count=1,
        premium_gold=0,
        premium_cosmetic_key="title_travel_qingyun_s1",
        premium_cosmetic_name="青云雅客",
    ),
]

TIP_GROUP = "travel_pass_error"
TIP_BASE = 38000
TIP_WIDTH = 1000
TIP_CODES = [
    ("TravelPassUnavailable",        "游历手记暂未开放，请稍后再试",                     True),
    ("TravelPassNotOpen",            "当前游历手记未开放或已结束",                       False),
    ("TravelPassAlreadyUnlocked",    "已解锁本期典藏游历手记",                           False),
    ("TravelPassPriceChanged",       "游历手记解锁价格或配置已更新，请刷新后再试",       False),
    ("TravelPassCurrencyNotEnough",  "灵石不足，无法解锁典藏游历手记",                   False),
    ("TravelPassNotReached",         "游历印点数不足，暂不可领取该档奖励",               False),
    ("TravelPassPremiumRequired",    "需先解锁典藏游历手记才可领取该档典藏奖励",         False),
    ("TravelPassNothingToClaim",     "当前没有可领取的游历手记奖励",                     False),
    ("TravelPassBagFull",            "背包空间不足，请清理后再领取游历手记奖励",         False),
    ("TravelPassItemBlocked",        "当前账号暂不可获取物品",                           False),
    ("TravelPassFrozen",             "角色当前状态无法操作游历手记，请稍后再试",         False),
    ("TravelPassInBattle",           "战斗中无法操作游历手记，请待战斗结束后再试",       False),
    ("TravelPassCosmeticNotOwned",   "尚未解锁该纪念头像框或称谓",                       False),
]

MESSAGE_LIMITER_XLSX = DATA_DIR / "MessageLimiter.xlsx"
MESSAGE_ID_TXT = REPO_ROOT / "proto" / "message_id.txt"
MESSAGE_LIMITER_SPECS = [
    ("SceneActivityClientPlayerGetTravelPassState", 10, 1, 1000),
    ("SceneActivityClientPlayerUnlockTravelPassPremium", 5, 1, 1000),
    ("SceneActivityClientPlayerClaimTravelPassReward", 5, 1, 1000),
    ("SceneActivityClientPlayerEquipTravelPassCosmetic", 10, 1, 1000),
]


def self_check() -> None:
    season_ids = {r["id"] for r in SEASON_ROWS}
    if len(season_ids) != len(SEASON_ROWS):
        fail("SEASON_ROWS has duplicate ids")
    for s in SEASON_ROWS:
        if s["id"] == 0 or s["max_points"] < 1 or not 1 <= s["premium_price"] <= 1_000_000_000:
            fail(f"invalid season row: {s}")
        if s["premium_currency_type"] not in (0, 1, 2):
            fail(f"invalid premium_currency_type in season {s['id']}")

    tier_ids = {r["id"] for r in TIER_ROWS}
    if len(tier_ids) != len(TIER_ROWS):
        fail("TIER_ROWS has duplicate ids")
    seen_indices: set[tuple[int, int]] = set()
    for t in TIER_ROWS:
        if t["pass_id"] not in season_ids:
            fail(f"tier {t['id']} references unknown pass_id {t['pass_id']}")
        if not 1 <= t["tier_index"] <= 64:
            fail(f"tier {t['id']} tier_index out of 1..64")
        key = (t["pass_id"], t["tier_index"])
        if key in seen_indices:
            fail(f"duplicate (pass_id, tier_index)={key}")
        seen_indices.add(key)
        if t["required_points"] < 1:
            fail(f"tier {t['id']} required_points < 1")

    names = [name for name, _text, _fault in TIP_CODES]
    if len(set(names)) != len(names) or len(TIP_CODES) > TIP_WIDTH:
        fail("TIP_CODES invalid")


def _write_table(sheet: str, rows: list[dict], dry_run: bool) -> None:
    xlsx_path = DATA_DIR / f"{sheet}.xlsx"
    layout = schema_layout(sheet)
    if any(f.width != 1 for f in layout):
        fail(f"{sheet} schema has non-scalar columns")
    names = [f.name for f in layout]
    for row in rows:
        if list(row.keys()) != names:
            fail(f"{sheet}: row keys {list(row.keys())} differ from schema fields {names}")

    wanted = [[None] * len(names) for _ in range(DATA_BEGIN_ROW - 1 + len(rows))]
    wanted[HEADER_ROW - 1] = list(names)
    wanted[COMMENT_ROW - 1] = [f.comment or None for f in layout]
    for i, row in enumerate(rows):
        wanted[DATA_BEGIN_ROW - 1 + i] = [None if row[n] == "" else row[n] for n in names]

    if xlsx_path.exists():
        ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path)
        if ws.title == sheet and first_difference(sheet_matrix(ws), wanted) is None:
            print(f"[ALREADY CURRENT] {rel(xlsx_path)} exists with identical contents; file unchanged.")
            return
        fail(f"{rel(xlsx_path)} already exists with different contents -- refusing to overwrite.")

    print(f"Will create {rel(xlsx_path)}: sheet={sheet!r}, {len(names)} columns x {len(rows)} rows")
    if dry_run:
        print("[dry-run] No files written.")
        return
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = sheet
    for r_idx, values in enumerate(wanted, start=1):
        for c_idx, value in enumerate(values, start=1):
            if value is not None:
                ws.cell(row=r_idx, column=c_idx, value=value)
    wb.save(xlsx_path)

    check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, sheet)
    diff = first_difference(sheet_matrix(check_ws), wanted)
    if diff:
        fail(f"after saving, {xlsx_path.name} read back differs ({diff})")
    bind(sheet, xlsx_path)
    print(f"[DONE] {rel(xlsx_path)}")


def cmd_new_tables(dry_run: bool) -> None:
    _write_table(SEASON_SHEET, SEASON_ROWS, dry_run)
    _write_table(TIER_SHEET, TIER_ROWS, dry_run)


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} row 1 unexpected: {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    header_text = f"//{TIP_GROUP} base={TIP_BASE} width={TIP_WIDTH}"
    wanted = [(name, text, 1 if fault else None) for name, text, fault in TIP_CODES]
    new_names = [name for name, _t, _f in TIP_CODES]

    mine = next((g for g in groups if g["name"] == TIP_GROUP), None)
    if mine is not None:
        if (mine["base"], mine["width"]) == (TIP_BASE, TIP_WIDTH) and mine["codes"][:len(wanted)] == wanted:
            print(f"[ALREADY CURRENT] {header_text} already has these {len(wanted)} codes.")
            return
        fail(f"{TIP_XLSX.name} already has group //{TIP_GROUP} with different contents.")

    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{n} (in {owner[n]})" for n in new_names if n in owner]
    if clash:
        fail(f"duplicate tip code names: {clash}")
    lo, hi = TIP_BASE, TIP_BASE + TIP_WIDTH
    overlap = [f"{g['name']} [{g['base']},{g['base'] + g['width']})" for g in groups
               if g["base"] < hi and lo < g["base"] + g["width"]]
    if overlap:
        fail(f"tip range [{lo},{hi}) overlaps: {overlap}")
    reserved = [f"row {row} {text!r}" for row, text in notes if str(TIP_BASE) in text]
    if reserved:
        fail(f"tip range {TIP_BASE} reserved by note rows: {reserved}")
    if TIP_STATE.exists():
        state_groups = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {})
        tomb = [f"{n} [{g['base']},{g['base'] + g['width']})" for n, g in state_groups.items()
                if n != TIP_GROUP and int(g["base"]) < hi and lo < int(g["base"]) + int(g["width"])]
        if tomb:
            fail(f"tip range [{lo},{hi}) taken in state: {tomb}")

    last_row = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
    header_row = last_row + 2
    patches: list[Patch] = [(header_row, 1, header_text)]
    for offset, (name, text, fault) in enumerate(wanted, start=1):
        row_idx = header_row + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
    if dry_run:
        print(f"[dry-run] Would append {header_text} with {len(wanted)} codes at row {header_row}.")
        return
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, before, patches, bind_schema=False)
    print(f"[DONE] Appended {header_text} ({len(wanted)} codes) to {rel(TIP_XLSX)}")


def _parse_message_ids() -> dict[str, int]:
    if not MESSAGE_ID_TXT.exists():
        fail(f"missing {rel(MESSAGE_ID_TXT)}; run proto-gen first")
    result: dict[str, int] = {}
    for line in MESSAGE_ID_TXT.read_text(encoding="utf-8").splitlines():
        s = line.strip()
        if not s or s.startswith("#") or "=" not in s:
            continue
        left, right = s.split("=", 1)
        left, right = left.strip(), right.strip()
        if left.isdigit() and right:
            result[right] = int(left)
        elif right.isdigit() and left:
            result[left] = int(right)
    return result


def cmd_message_limiter(dry_run: bool) -> None:
    msg_ids = _parse_message_ids()
    targets: list[tuple[int, int, int, int]] = []
    for name, max_req, window, tip in MESSAGE_LIMITER_SPECS:
        if name not in msg_ids:
            fail(f"{name} not found in {rel(MESSAGE_ID_TXT)}; run dev.bat gen / proto-gen first")
        targets.append((msg_ids[name], max_req, window, tip))

    wb = openpyxl.load_workbook(MESSAGE_LIMITER_XLSX)
    ws = only_sheet(wb, MESSAGE_LIMITER_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip() if v is not None else "" for v in before[HEADER_ROW - 1]]
    expected_cols = ["id", "max_requests", "time_window", "tip_message"]
    if header[:4] != expected_cols:
        fail(f"{MESSAGE_LIMITER_XLSX.name} header unexpected: {header}")

    existing: dict[int, tuple[int, int, int]] = {}
    last_row = DATA_BEGIN_ROW - 1
    for r_idx, row in enumerate(before[DATA_BEGIN_ROW - 1:], start=DATA_BEGIN_ROW):
        if any(v is not None for v in row):
            last_row = r_idx
            if row[0] is not None:
                existing[int(row[0])] = (int(row[1]), int(row[2]), int(row[3]))

    patches: list[Patch] = []
    next_row = last_row + 1
    for mid, max_req, window, tip in targets:
        if mid in existing:
            if existing[mid] == (max_req, window, tip):
                print(f"[ALREADY CURRENT] MessageLimiter id={mid} already matches.")
                continue
            fail(f"MessageLimiter id={mid} already exists with different values {existing[mid]}")
        patches += [
            (next_row, 1, mid),
            (next_row, 2, max_req),
            (next_row, 3, window),
            (next_row, 4, tip),
        ]
        print(f"Will append MessageLimiter row {next_row}: id={mid} max_requests={max_req} time_window={window} tip_message={tip}")
        next_row += 1

    if not patches:
        return
    if dry_run:
        print("[dry-run] No files written.")
        return
    apply_patches_and_verify(MESSAGE_LIMITER_XLSX, "MessageLimiter", wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(MESSAGE_LIMITER_XLSX)}")


COMMANDS = {
    "new-tables": cmd_new_tables,
    "tip-codes": cmd_tip_codes,
    "message-limiter": cmd_message_limiter,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    self_check()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
