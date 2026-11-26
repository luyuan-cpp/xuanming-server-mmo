#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""官方灵石百宝阁(docs/design/spirit-shop-and-starter-packs-v1.md)要对 xlsx 做的全部改动,可幂等重复执行。

用法(仓库根目录执行):
    python tools/scripts/spirit_shop_xlsx_patch.py new-tables       # ① 建 SpiritShopLimitGroup.xlsx 与 SpiritShopGoods.xlsx
    python tools/scripts/spirit_shop_xlsx_patch.py tip-codes        # ② Tip.xlsx 新开 //spirit_shop_error (37000 段)
    python tools/scripts/spirit_shop_xlsx_patch.py message-limiter  # ③ 跑完 proto-gen 后为 2 条新 RPC 追加 MessageLimiter 行
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


LIMIT_GROUP_SHEET = "SpiritShopLimitGroup"
LIMIT_GROUP_ROWS = [
    dict(
        id=1,
        name="首套修行提速终身限额",
        reset_kind=0,
        max_weight=300,
    ),
]

GOODS_SHEET = "SpiritShopGoods"
GOODS_ROWS = [
    dict(
        id=101,
        tab_id=1,
        sort_order=10,
        enabled=1,
        name="初阶修行提速包",
        description="仅自用绑定材料包：含 30 份打造材料 + 30 份百炼石（共 60 份），计入首套终身 300 份共享限额，不增加战力上限。",
        icon_key="UI/Ugui/SpiritShopV1/pack_starter_60",
        currency_type=1,
        price=60,
        max_buy_per_request=5,
        item_id_1=3001,
        item_count_1=30,
        item_bound_1=1,
        item_id_2=3002,
        item_count_2=30,
        item_bound_2=1,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=1,
        limit_group_weight=60,
        badge_text="6元破冰",
    ),
    dict(
        id=102,
        tab_id=1,
        sort_order=20,
        enabled=1,
        name="中阶修行提速包",
        description="仅自用绑定材料包：含 90 份打造材料 + 90 份百炼石（共 180 份），计入首套终身 300 份共享限额，不增加战力上限。",
        icon_key="UI/Ugui/SpiritShopV1/pack_starter_180",
        currency_type=1,
        price=180,
        max_buy_per_request=1,
        item_id_1=3001,
        item_count_1=90,
        item_bound_1=1,
        item_id_2=3002,
        item_count_2=90,
        item_bound_2=1,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=1,
        limit_group_weight=180,
        badge_text="超值省时",
    ),
    dict(
        id=103,
        tab_id=1,
        sort_order=30,
        enabled=1,
        name="半套毕业提速包",
        description="仅自用绑定材料包：含 150 份打造材料 + 150 份百炼石（共 300 份），一次占满首套终身 300 份共享限额，不增加战力上限。",
        icon_key="UI/Ugui/SpiritShopV1/pack_starter_300",
        currency_type=1,
        price=300,
        max_buy_per_request=1,
        item_id_1=3001,
        item_count_1=150,
        item_bound_1=1,
        item_id_2=3002,
        item_count_2=150,
        item_bound_2=1,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=1,
        limit_group_weight=300,
        badge_text="半套直达",
    ),
    dict(
        id=201,
        tab_id=2,
        sort_order=10,
        enabled=1,
        name="定星保底符",
        description="仅自用绑定配套符箓：打磨 V1 装备词条时额外消耗 1 张，本次 100% 必达满值极品品相，不突破原上限。",
        icon_key="UI/Ugui/CraftQualitySkill/Icons/item_star_guarantee_charm",
        currency_type=1,
        price=60,
        max_buy_per_request=20,
        item_id_1=3405,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="保底极品",
    ),
    dict(
        id=202,
        tab_id=2,
        sort_order=20,
        enabled=1,
        name="天工秘墨",
        description="仅自用绑定制书秘墨：配合玩法产出的绝学残卷，100% 定向合成指定稀世绝学书（成品仅自用，永久保值）。",
        icon_key="UI/Ugui/CraftQualitySkill/Icons/item_celestial_ink",
        currency_type=1,
        price=180,
        max_buy_per_request=10,
        item_id_1=3408,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="稀世制书",
    ),
    dict(
        id=301,
        tab_id=3,
        sort_order=10,
        enabled=1,
        name="角色形象卡",
        description="仅自用绑定形象卡：可在易容衣柜永久解锁一款支持兑换的角色形象，已拥有形象随时免费切换（0 数值加成）。",
        icon_key="UI/Icons/item_appearance_card",
        currency_type=1,
        price=100,
        max_buy_per_request=10,
        item_id_1=3402,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="永久衣柜",
    ),
    dict(
        id=302,
        tab_id=3,
        sort_order=20,
        enabled=1,
        name="角色改名卡",
        description="仅自用绑定改名卡：使用后可修改一次角色名（冷却 7 天，旧名保留保护 7 天）。",
        icon_key="",
        currency_type=1,
        price=100,
        max_buy_per_request=5,
        item_id_1=3401,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="社交便利",
    ),
    dict(
        id=303,
        tab_id=3,
        sort_order=30,
        enabled=1,
        name="灵宠幻化符",
        description="仅自用绑定幻化符：可在衣柜永久解锁一款支持兑换的灵宠幻化，已拥有随时免费穿脱（纯外观零数值）。",
        icon_key="UI/Ugui/WardrobeCosmetics/item_pet_morph_charm",
        currency_type=1,
        price=120,
        max_buy_per_request=10,
        item_id_1=3411,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="永久衣柜",
    ),
    dict(
        id=304,
        tab_id=3,
        sort_order=40,
        enabled=1,
        name="技能特效卷",
        description="仅自用绑定特效卷：可在衣柜永久解锁一款支持兑换的技能特效，已拥有随时免费切换（纯画面零数值）。",
        icon_key="UI/Ugui/WardrobeCosmetics/item_skill_vfx_scroll",
        currency_type=1,
        price=180,
        max_buy_per_request=10,
        item_id_1=3412,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="永久衣柜",
    ),
]

TIP_GROUP = "spirit_shop_error"
TIP_BASE = 37000
TIP_WIDTH = 1000
TIP_CODES = [
    ("SpiritShopUnavailable",       "灵石百宝阁暂不可用，请稍后再试",         True),
    ("SpiritShopGoodsOffShelf",     "该商品已下架或暂不出售",                 False),
    ("SpiritShopPriceChanged",      "商品价格或内容已更新，请刷新后再试",     False),
    ("SpiritShopInvalidCount",      "购买数量不合法",                         False),
    ("SpiritShopLimitReached",      "该商品已达限购上限",                     False),
    ("SpiritShopGroupLimitReached", "该类提速包已达终身限购总额",             False),
    ("SpiritShopCurrencyNotEnough", "灵石不足，请先充值灵石",                 False),
    ("SpiritShopBagFull",           "背包空间不足，请清理后再试",             False),
    ("SpiritShopFrozen",            "角色当前状态无法购买，请稍后再试",       False),
    ("SpiritShopInBattle",          "战斗中无法购买，请待战斗结束后再试",     False),
    ("SpiritShopItemBlocked",       "该物品当前被禁止获取",                   False),
]

MESSAGE_LIMITER_XLSX = DATA_DIR / "MessageLimiter.xlsx"
MESSAGE_ID_TXT = REPO_ROOT / "proto" / "message_id.txt"
MESSAGE_LIMITER_SPECS = [
    ("SceneSpiritShopClientPlayerGetSpiritShopInfo", 10, 1, 1000),
    ("SceneSpiritShopClientPlayerBuySpiritShopGoods", 5, 1, 1000),
]


def self_check() -> None:
    group_ids = {r["id"] for r in LIMIT_GROUP_ROWS}
    if len(group_ids) != len(LIMIT_GROUP_ROWS):
        fail("LIMIT_GROUP_ROWS has duplicate ids")
    for g in LIMIT_GROUP_ROWS:
        if g["id"] == 0 or g["reset_kind"] not in (0, 1) or g["max_weight"] < 1:
            fail(f"invalid limit group row: {g}")

    goods_ids = {r["id"] for r in GOODS_ROWS}
    if len(goods_ids) != len(GOODS_ROWS):
        fail("GOODS_ROWS has duplicate ids")
    for r in GOODS_ROWS:
        if r["currency_type"] not in (0, 1, 2):
            fail(f"invalid currency_type in row {r['id']}")
        if not 1 <= r["price"] <= 1_000_000_000:
            fail(f"invalid price in row {r['id']}")
        if not 1 <= r["max_buy_per_request"] <= 99:
            fail(f"invalid max_buy_per_request in row {r['id']}")
        if r["item_id_1"] == 0 or r["item_count_1"] < 1:
            fail(f"invalid item_1 in row {r['id']}")
        if r["item_id_2"] != 0 and r["item_count_2"] < 1:
            fail(f"invalid item_2 in row {r['id']}")
        if r["limit_group_id"] != 0:
            if r["limit_group_id"] not in group_ids or r["limit_group_weight"] < 1:
                fail(f"invalid limit_group in row {r['id']}")

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
    _write_table(LIMIT_GROUP_SHEET, LIMIT_GROUP_ROWS, dry_run)
    _write_table(GOODS_SHEET, GOODS_ROWS, dry_run)


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
