#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""不肝不氪增收体系 V2 · 批次 B1（docs/planning/fair-monetization-expansion-v2.md）xlsx 幂等补丁脚本。

用法（仓库根目录执行）：
    python tools/scripts/fair_monetization_b1_xlsx_patch.py tables           # ① 更新 SpiritShopGoods / TravelPassSeason / TravelPassTier / AfkPassRule / AppearanceSeries
    python tools/scripts/fair_monetization_b1_xlsx_patch.py tip-codes        # ② 在 //afk_pass_error 与 //appearance_error 末尾追加错误码
    python tools/scripts/fair_monetization_b1_xlsx_patch.py message-limiter  # ③ 跑完 proto-gen 后为 2 条新 RPC 追加 MessageLimiter 行
"""

from __future__ import annotations

import argparse
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
        TIP_XLSX,
        Patch,
        apply_patches_and_verify,
        fail,
        only_sheet,
        rel,
        schema_layout,
        sheet_matrix,
    )
except ImportError as exc:  # pragma: no cover
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


# ── 1. SpiritShopGoods 新增 203（3409 解封剥离符）与 204（3410 流派副套继承符）──
NEW_SPIRIT_SHOP_GOODS = [
    dict(
        id=203,
        tab_id=2,
        sort_order=30,
        enabled=1,
        name="解封剥离符",
        description="仅自用绑定配套符箓：救回拥有可交易血统（origin_tradeable）的绑定装备原胚，剥离后清空绑定养成并恢复可交易，绝不洗白非原胚。",
        icon_key="UI/Ugui/EconomyTradeV1/icon_unseal_talisman",
        currency_type=1,
        price=60,
        max_buy_per_request=10,
        item_id_1=3409,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="解封原胚",
        start_time_ms=0,
        end_time_ms=0,
        grant_mode=0,
        item_id_3=0,
        item_count_3=0,
        item_bound_3=0,
        item_id_4=0,
        item_count_4=0,
        item_bound_4=0,
        original_price=0,
        min_level=0,
        max_level=0,
    ),
    dict(
        id=204,
        tab_id=2,
        sort_order=40,
        enabled=1,
        name="流派副套继承符",
        description="仅自用绑定配套符箓：将当前高阶装备的强化等级与养成进度平迁继承至同部位副套装备，方便零焦虑切换多流派，不突破战力上限。",
        icon_key="UI/Ugui/EconomyTradeV2/icon_inherit_talisman",
        currency_type=1,
        price=120,
        max_buy_per_request=10,
        item_id_1=3410,
        item_count_1=1,
        item_bound_1=1,
        item_id_2=0,
        item_count_2=0,
        item_bound_2=0,
        limit_reset_kind=0,
        limit_count=0,
        limit_group_id=0,
        limit_group_weight=0,
        badge_text="副套继承",
        start_time_ms=0,
        end_time_ms=0,
        grant_mode=0,
        item_id_3=0,
        item_count_3=0,
        item_bound_3=0,
        item_id_4=0,
        item_count_4=0,
        item_bound_4=0,
        original_price=0,
        min_level=0,
        max_level=0,
    ),
]


# ── 2. TravelPassSeason 永不绝版书架 2..6 期 ──
NEW_TRAVEL_PASS_SEASONS = [
    dict(
        id=2,
        enabled=1,
        name="玄霜问剑手记 · 第二期",
        description="永不绝版书架：零每日打卡、随时切换在研期次；正常战斗自动涨游历印，可先玩满再自愿解锁 ¥18 典藏外观轨，解锁后永不过期。",
        icon_key="UI/Ugui/TravelPassV1/travel_pass_card_icon",
        banner_asset_key="UI/Ugui/TravelPassV2/banner_travel_pass_s2",
        starts_at_ms=0,
        ends_at_ms=0,
        max_points=300,
        pve_win_points=20,
        hard_or_pvp_win_points=30,
        pvp_qualified_other_points=15,
        premium_currency_type=1,
        premium_price=180,
        config_revision=1,
        sort_order=2,
        is_archived=0,
    ),
    dict(
        id=3,
        enabled=1,
        name="炽焰丹心手记 · 第三期",
        description="永不绝版书架：零每日打卡、随时切换在研期次；正常战斗自动涨游历印，可先玩满再自愿解锁 ¥18 典藏外观轨，解锁后永不过期。",
        icon_key="UI/Ugui/TravelPassV1/travel_pass_card_icon",
        banner_asset_key="UI/Ugui/TravelPassV2/banner_travel_pass_s3",
        starts_at_ms=0,
        ends_at_ms=0,
        max_points=300,
        pve_win_points=20,
        hard_or_pvp_win_points=30,
        pvp_qualified_other_points=15,
        premium_currency_type=1,
        premium_price=180,
        config_revision=1,
        sort_order=3,
        is_archived=0,
    ),
    dict(
        id=4,
        enabled=1,
        name="沧澜听雨手记 · 第四期",
        description="永不绝版书架：零每日打卡、随时切换在研期次；正常战斗自动涨游历印，可先玩满再自愿解锁 ¥18 典藏外观轨，解锁后永不过期。",
        icon_key="UI/Ugui/TravelPassV1/travel_pass_card_icon",
        banner_asset_key="UI/Ugui/TravelPassV2/banner_travel_pass_s4",
        starts_at_ms=0,
        ends_at_ms=0,
        max_points=300,
        pve_win_points=20,
        hard_or_pvp_win_points=30,
        pvp_qualified_other_points=15,
        premium_currency_type=1,
        premium_price=180,
        config_revision=1,
        sort_order=4,
        is_archived=0,
    ),
    dict(
        id=5,
        enabled=1,
        name="灵木问道手记 · 第五期",
        description="永不绝版书架：零每日打卡、随时切换在研期次；正常战斗自动涨游历印，可先玩满再自愿解锁 ¥18 典藏外观轨，解锁后永不过期。",
        icon_key="UI/Ugui/TravelPassV1/travel_pass_card_icon",
        banner_asset_key="UI/Ugui/TravelPassV2/banner_travel_pass_s5",
        starts_at_ms=0,
        ends_at_ms=0,
        max_points=300,
        pve_win_points=20,
        hard_or_pvp_win_points=30,
        pvp_qualified_other_points=15,
        premium_currency_type=1,
        premium_price=180,
        config_revision=1,
        sort_order=5,
        is_archived=0,
    ),
    dict(
        id=6,
        enabled=1,
        name="厚土镇岳手记 · 第六期",
        description="永不绝版书架：零每日打卡、随时切换在研期次；正常战斗自动涨游历印，可先玩满再自愿解锁 ¥18 典藏外观轨，解锁后永不过期。",
        icon_key="UI/Ugui/TravelPassV1/travel_pass_card_icon",
        banner_asset_key="UI/Ugui/TravelPassV2/banner_travel_pass_s6",
        starts_at_ms=0,
        ends_at_ms=0,
        max_points=300,
        pve_win_points=20,
        hard_or_pvp_win_points=30,
        pvp_qualified_other_points=15,
        premium_currency_type=1,
        premium_price=180,
        config_revision=1,
        sort_order=6,
        is_archived=0,
    ),
]


def _build_season_tiers(
    pass_id: int,
    slug: str,
    frame_name: str,
    free_title_name: str,
    premium_title_name: str,
    finale_item_id: int,
) -> list[dict]:
    """每期 10 档（30..300 点），零打造/百炼石、全绑定、每期仅 1 张外观券防衣柜送穿。"""
    base = pass_id * 100
    return [
        dict(id=base + 1, pass_id=pass_id, tier_index=1, required_points=30,
             free_item_id=0, free_item_count=0, free_gold=5000, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3405, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key=f"frame_travel_{slug}_s{pass_id}", premium_cosmetic_name=frame_name),
        dict(id=base + 2, pass_id=pass_id, tier_index=2, required_points=60,
             free_item_id=3403, free_item_count=2, free_gold=0, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=0, premium_item_count=0, premium_gold=10000,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 3, pass_id=pass_id, tier_index=3, required_points=90,
             free_item_id=3406, free_item_count=10, free_gold=0, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3401, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 4, pass_id=pass_id, tier_index=4, required_points=120,
             free_item_id=0, free_item_count=0, free_gold=8000, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3403, premium_item_count=3, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 5, pass_id=pass_id, tier_index=5, required_points=150,
             free_item_id=3405, free_item_count=1, free_gold=0, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3409, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 6, pass_id=pass_id, tier_index=6, required_points=180,
             free_item_id=3403, free_item_count=2, free_gold=0, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3405, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 7, pass_id=pass_id, tier_index=7, required_points=210,
             free_item_id=0, free_item_count=0, free_gold=10000, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3407, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 8, pass_id=pass_id, tier_index=8, required_points=240,
             free_item_id=3406, free_item_count=15, free_gold=0, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3403, premium_item_count=3, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 9, pass_id=pass_id, tier_index=9, required_points=270,
             free_item_id=3403, free_item_count=2, free_gold=0, free_cosmetic_key="", free_cosmetic_name="",
             premium_item_id=3408, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key="", premium_cosmetic_name=""),
        dict(id=base + 10, pass_id=pass_id, tier_index=10, required_points=300,
             free_item_id=0, free_item_count=0, free_gold=15000,
             free_cosmetic_key=f"title_travel_free_s{pass_id}", free_cosmetic_name=free_title_name,
             premium_item_id=finale_item_id, premium_item_count=1, premium_gold=0,
             premium_cosmetic_key=f"title_travel_{slug}_s{pass_id}", premium_cosmetic_name=premium_title_name),
    ]


NEW_TRAVEL_PASS_TIERS = (
    _build_season_tiers(2, "xuanshuang", "玄霜问剑头像框", "问剑初成", "玄霜剑客", 3411)
    + _build_season_tiers(3, "chiyan", "炽焰丹心头像框", "丹心初成", "赤焰真君", 3412)
    + _build_season_tiers(4, "canglan", "沧澜听雨头像框", "听雨初成", "沧澜雅士", 3402)
    + _build_season_tiers(5, "lingmu", "灵木问道头像框", "问道初成", "青木隐仙", 3411)
    + _build_season_tiers(6, "houtu", "厚土镇岳头像框", "镇岳初成", "坤岳道宗", 3412)
)


# ── 3. AfkPassRule 多档套餐（id=1 30天月享卡 + id=2 90天季享卡）──
AFK_PASS_ROW1_EXTRA = dict(
    bonus_item_id=3405,
    bonus_item_count=1,
    bonus_frame_key="frame_lightcard_s1",
    plan_name="仙途轻享月卡",
    icon_key="UI/Ugui/AfkPassV2/icon_season_pass_90d",
)
AFK_PASS_ROW2 = dict(
    id=2,
    period_days=90,
    price_currency_type=1,
    price=780,
    max_remaining_days=180,
    remind_before_hours=72,
    desc="90 天仙途季享卡（较 3 次月卡省 120 灵石，与月卡共用 180 天叠加上限；一次性直赠定星保底符×3 + 季享限定头像框）",
    bonus_item_id=3405,
    bonus_item_count=3,
    bonus_frame_key="frame_lightcard_season_s1",
    plan_name="仙途季享卡",
    icon_key="UI/Ugui/AfkPassV2/icon_season_pass_90d",
)


# ── 4. AppearanceSeries 补齐包默认 8.5 折（8500 bps / 100 灵石单价）──
APPEARANCE_SERIES_EXTRA = dict(
    bundle_discount_bps=8500,
    unit_price_diamond=100,
)


# ── 5. Tip.xlsx 在既有组尾追加的错误码 ──
TIP_GROUP_ADDITIONS: dict[str, list[tuple[str, str, bool]]] = {
    "//afk_pass_error": [
        ("AfkPassBagFull", "背包空间不足，请至少留出空位后再开通仙途轻享卡", False),
        ("AfkPassPlanInvalid", "所选轻享卡套餐暂不可用，请刷新后再试", False),
    ],
    "//appearance_error": [
        ("AppearanceSeriesBundleUnavailable", "该收藏系列暂未开放补齐包", False),
        ("AppearanceSeriesAlreadyCompleted", "该收藏系列已全部集齐，无需补齐", False),
        ("AppearanceSeriesBundlePriceChanged", "系列补齐缺口或价格已更新，请刷新后再试", False),
        ("AppearanceSeriesBundleCurrencyNotEnough", "灵石不足，无法购买系列补齐包", False),
    ],
}


# ── 6. MessageLimiter.xlsx 新增 2 条 B1 RPC ──
MESSAGE_LIMITER_XLSX = DATA_DIR / "MessageLimiter.xlsx"
MESSAGE_ID_TXT = REPO_ROOT / "proto" / "message_id.txt"
MESSAGE_LIMITER_SPECS = [
    ("SceneActivityClientPlayerSelectActiveTravelPass", 10, 1, 1000),
    ("SceneAppearanceClientPlayerBuyAppearanceSeriesBundle", 5, 1, 1000),
]


def _ensure_columns_and_rows(
    sheet: str,
    existing_row_updates: dict[int, dict],
    new_rows: list[dict],
    dry_run: bool,
) -> None:
    xlsx_path = DATA_DIR / f"{sheet}.xlsx"
    layout = schema_layout(sheet)
    col_names = [f.name for f in layout]
    col_comments = {f.name: (f.comment or None) for f in layout}
    col_idx_by_name = {name: i + 1 for i, name in enumerate(col_names)}

    wb = openpyxl.load_workbook(xlsx_path)
    ws = only_sheet(wb, xlsx_path, sheet)
    before = sheet_matrix(ws)
    cur_header = [str(v).strip() if v is not None else "" for v in before[HEADER_ROW - 1]]
    while cur_header and cur_header[-1] == "":
        cur_header.pop()

    if cur_header != col_names[:len(cur_header)]:
        fail(f"{xlsx_path.name}: existing header {cur_header} is not a prefix of schema {col_names}")

    patches: list[Patch] = []
    # 追加新列头与第 5 行注释
    for col_name in col_names[len(cur_header):]:
        c = col_idx_by_name[col_name]
        patches.append((HEADER_ROW, c, col_name))
        if col_comments[col_name] is not None:
            patches.append((COMMENT_ROW, c, col_comments[col_name]))

    # 扫描既有数据行（按主键 id）
    id_to_row: dict[int, int] = {}
    last_data_row = DATA_BEGIN_ROW - 1
    for r_idx, row in enumerate(before[DATA_BEGIN_ROW - 1:], start=DATA_BEGIN_ROW):
        if any(v is not None for v in row):
            last_data_row = r_idx
            if row[0] is not None:
                id_to_row[int(row[0])] = r_idx

    # 对既有行补齐新增列或指定列值
    for row_id, updates in existing_row_updates.items():
        if row_id not in id_to_row:
            fail(f"{xlsx_path.name}: expected existing id={row_id} not found")
        r_idx = id_to_row[row_id]
        row_cells = before[r_idx - 1]
        for k, val in updates.items():
            c = col_idx_by_name[k]
            cur_val = row_cells[c - 1] if c - 1 < len(row_cells) else None
            wanted_val = None if val == "" else val
            if cur_val != wanted_val:
                patches.append((r_idx, c, wanted_val))

    # 追加新数据行
    next_row = last_data_row + 1
    for row_dict in new_rows:
        row_id = int(row_dict["id"])
        if set(row_dict.keys()) != set(col_names):
            fail(f"{xlsx_path.name}: new row id={row_id} keys mismatch schema")
        if row_id in id_to_row:
            r_idx = id_to_row[row_id]
            row_cells = before[r_idx - 1]
            mismatch = []
            for k in col_names:
                c = col_idx_by_name[k]
                cur_val = row_cells[c - 1] if c - 1 < len(row_cells) else None
                wanted_val = None if row_dict[k] == "" else row_dict[k]
                if cur_val != wanted_val:
                    mismatch.append((k, cur_val, wanted_val))
            if mismatch:
                fail(f"{xlsx_path.name}: row id={row_id} already exists with different values: {mismatch[:3]}")
            continue
        for k in col_names:
            c = col_idx_by_name[k]
            val = None if row_dict[k] == "" else row_dict[k]
            if val is not None:
                patches.append((next_row, c, val))
        next_row += 1

    if not patches:
        print(f"[ALREADY CURRENT] {rel(xlsx_path)}")
        return
    print(f"Will patch {rel(xlsx_path)}: {len(patches)} cell edits")
    if dry_run:
        return
    apply_patches_and_verify(xlsx_path, sheet, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] {rel(xlsx_path)}")


def cmd_tables(dry_run: bool) -> None:
    _ensure_columns_and_rows("SpiritShopGoods", {}, NEW_SPIRIT_SHOP_GOODS, dry_run)
    _ensure_columns_and_rows(
        "TravelPassSeason",
        {1: dict(sort_order=1, is_archived=0)},
        NEW_TRAVEL_PASS_SEASONS,
        dry_run,
    )
    _ensure_columns_and_rows("TravelPassTier", {}, NEW_TRAVEL_PASS_TIERS, dry_run)
    _ensure_columns_and_rows(
        "AfkPassRule",
        {1: AFK_PASS_ROW1_EXTRA},
        [AFK_PASS_ROW2],
        dry_run,
    )
    _ensure_columns_and_rows(
        "AppearanceSeries",
        {1: APPEARANCE_SERIES_EXTRA, 2: APPEARANCE_SERIES_EXTRA, 3: APPEARANCE_SERIES_EXTRA},
        [],
        dry_run,
    )


def _tip_names_by_row(ws) -> dict[str, int]:
    out: dict[str, int] = {}
    for row in range(1, ws.max_row + 1):
        v = ws.cell(row, 1).value
        if isinstance(v, str) and v.strip() and not v.strip().startswith("//"):
            name = v.strip()
            if name in out:
                fail(f"Tip.xlsx duplicate code {name} at rows {out[name]} and {row}")
            out[name] = row
    return out


def _tip_group_insert_row(ws, group_prefix: str) -> int:
    inside = False
    last_filled = 0
    for row in range(1, ws.max_row + 1):
        v = ws.cell(row, 1).value
        if isinstance(v, str) and v.strip().startswith("//"):
            if v.strip().startswith(group_prefix + " "):
                inside = True
                last_filled = row
                continue
            elif inside:
                return last_filled + 1
        if inside and v is not None and str(v).strip():
            last_filled = row
    if not inside:
        fail(f"Tip.xlsx: group {group_prefix} not found")
    return last_filled + 1


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = wb.active
    header = [str(ws.cell(1, c).value or "").strip().lower() for c in range(1, ws.max_column + 1)]
    if "fault" not in header:
        fail(f"Tip.xlsx missing fault column: {header}")
    fault_col = header.index("fault") + 1

    existing = _tip_names_by_row(ws)
    to_apply: list[tuple[str, list[tuple[str, str, bool]]]] = []
    for group_prefix, specs in TIP_GROUP_ADDITIONS.items():
        present = [n for n, _t, _f in specs if n in existing]
        if len(present) == len(specs):
            print(f"[ALREADY CURRENT] Tip.xlsx {group_prefix}: {', '.join(present)}")
            continue
        if present:
            fail(f"Tip.xlsx {group_prefix}: partial codes present {present}")
        to_apply.append((group_prefix, specs))

    if not to_apply:
        return

    # 按行号逆序插入，避免前面的 insert_rows 偏移后面组的行号
    planned = [(_tip_group_insert_row(ws, grp), grp, specs) for grp, specs in to_apply]
    planned.sort(key=lambda x: x[0], reverse=True)
    for insert_at, grp, specs in planned:
        print(f"Will insert {len(specs)} codes into {grp} at row {insert_at}: {[s[0] for s in specs]}")
    if dry_run:
        print("[dry-run] No files written.")
        return

    for insert_at, _grp, specs in planned:
        if insert_at <= ws.max_row:
            ws.insert_rows(insert_at, len(specs))
        for offset, (name, text, is_fault) in enumerate(specs):
            r = insert_at + offset
            ws.cell(r, 1, name)
            ws.cell(r, 2, text)
            if is_fault:
                ws.cell(r, fault_col, 1)
    wb.save(TIP_XLSX)

    check = openpyxl.load_workbook(TIP_XLSX).active
    after = _tip_names_by_row(check)
    for _grp, specs in to_apply:
        for name, text, is_fault in specs:
            r = after.get(name)
            if r is None or check.cell(r, 2).value != text:
                fail(f"Tip.xlsx read-back mismatch for {name}")
            if bool(check.cell(r, fault_col).value) != is_fault:
                fail(f"Tip.xlsx fault mismatch for {name}")
    print(f"[DONE] Updated {rel(TIP_XLSX)}")


def _parse_message_ids() -> dict[str, int]:
    if not MESSAGE_ID_TXT.exists():
        fail(f"missing {rel(MESSAGE_ID_TXT)}; run dev.bat gen first")
    result: dict[str, int] = {}
    for line in MESSAGE_ID_TXT.read_text(encoding="utf-8").splitlines():
        s = line.strip()
        if not s or s.startswith("#") or "=" not in s:
            continue
        left, right = (p.strip() for p in s.split("=", 1))
        if left.isdigit() and right:
            result[right] = int(left)
        elif right.isdigit() and left:
            result[left] = int(right)
    return result


def cmd_message_limiter(dry_run: bool) -> None:
    msg_ids = _parse_message_ids()
    targets = []
    for name, max_req, window, tip in MESSAGE_LIMITER_SPECS:
        if name not in msg_ids:
            fail(f"{name} not found in {rel(MESSAGE_ID_TXT)}; run dev.bat gen first")
        targets.append((msg_ids[name], max_req, window, tip))

    wb = openpyxl.load_workbook(MESSAGE_LIMITER_XLSX)
    ws = only_sheet(wb, MESSAGE_LIMITER_XLSX)
    before = sheet_matrix(ws)
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
                print(f"[ALREADY CURRENT] MessageLimiter id={mid}")
                continue
            fail(f"MessageLimiter id={mid} exists with different values {existing[mid]}")
        patches += [
            (next_row, 1, mid),
            (next_row, 2, max_req),
            (next_row, 3, window),
            (next_row, 4, tip),
        ]
        print(f"Will append MessageLimiter row {next_row}: id={mid} ({max_req}/{window}/{tip})")
        next_row += 1

    if not patches:
        return
    if dry_run:
        print("[dry-run] No files written.")
        return
    apply_patches_and_verify(MESSAGE_LIMITER_XLSX, "MessageLimiter", wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(MESSAGE_LIMITER_XLSX)}")


COMMANDS = {
    "tables": cmd_tables,
    "tip-codes": cmd_tip_codes,
    "message-limiter": cmd_message_limiter,
}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=tuple(COMMANDS))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    COMMANDS[args.command](args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
