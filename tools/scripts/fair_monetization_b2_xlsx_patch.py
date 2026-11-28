#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""不肝不氪增收体系 V2 · 批次 B2（docs/planning/fair-monetization-expansion-v2.md）xlsx 幂等补丁脚本。

用法（仓库根目录执行）：
    python tools/scripts/fair_monetization_b2_xlsx_patch.py tables           # ① 生成 CosmeticShowcase.xlsx，追加 Item 3417..3420 与 SpiritShopGoods 305..308
    python tools/scripts/fair_monetization_b2_xlsx_patch.py tip-codes        # ② 在 //appearance_error 末尾追加 B2 社交同乐与赠礼错误码
    python tools/scripts/fair_monetization_b2_xlsx_patch.py message-limiter  # ③ 跑完 proto-gen 后为 4 条 B2 C2S RPC 追加 MessageLimiter 行
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
try:
    import openpyxl
    from equip_xlsx_patch import (
        DATA_BEGIN_ROW,
        DATA_DIR,
        HEADER_ROW,
        REPO_ROOT,
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
    )
except ImportError as exc:  # pragma: no cover
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


COSMETIC_SHOWCASE_SHEET = "CosmeticShowcase"
COSMETIC_SHOWCASE_ROWS = [
    # slot_kind = 1: 五行阵法脚底阵纹（5 款可券换 + #13 创始支持者限定）
    dict(
        id=1, cosmetic_key="formation_skin_metal", slot_kind=1, name="金芒八卦阵纹",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/formation_skin_metal", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=10,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉脚底阵纹：进战后全场展示金行八卦辉光，敌我与全服观战可见，零数值加成。",
    ),
    dict(
        id=2, cosmetic_key="formation_skin_wood", slot_kind=1, name="青灵生息阵纹",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/formation_skin_wood", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=20,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉脚底阵纹：进战后全场展示木行青莲流纹，敌我与全服观战可见，零数值加成。",
    ),
    dict(
        id=3, cosmetic_key="formation_skin_water", slot_kind=1, name="玄澜潮涌阵纹",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/formation_skin_water", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=30,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉脚底阵纹：进战后全场展示水行沧澜波纹，敌我与全服观战可见，零数值加成。",
    ),
    dict(
        id=4, cosmetic_key="formation_skin_fire", slot_kind=1, name="赤霄焚天阵纹",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/formation_skin_fire", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=40,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉脚底阵纹：进战后全场展示火行赤焰印纹，敌我与全服观战可见，零数值加成。",
    ),
    dict(
        id=5, cosmetic_key="formation_skin_earth", slot_kind=1, name="厚土镇岳阵纹",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/formation_skin_earth", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=50,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉脚底阵纹：进战后全场展示土行山岳玄纹，敌我与全服观战可见，零数值加成。",
    ),
    # slot_kind = 2: 擂台/会武入场与胜利横幅（3 款）
    dict(
        id=6, cosmetic_key="battle_banner_entrance_01", slot_kind=2, name="青锋破阵旗",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/battle_banner_entrance_01", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=60,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉入场与胜利演出横幅：在生死擂、论剑会武与问道赛登场及制胜时展示，零数值加成。",
    ),
    dict(
        id=7, cosmetic_key="battle_banner_entrance_02", slot_kind=2, name="丹砂点将旌",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/battle_banner_entrance_02", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=70,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉入场与胜利演出横幅：丹砂朱红将旗演出，兼容全服观战视角，零数值加成。",
    ),
    dict(
        id=8, cosmetic_key="battle_banner_entrance_03", slot_kind=2, name="紫微问鼎幡",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/battle_banner_entrance_03", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=80,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉入场与胜利演出横幅：星纹紫微礼幡演出，兼容全服观战视角，零数值加成。",
    ),
    # slot_kind = 3: 聊天气泡样式（2 款）
    dict(
        id=9, cosmetic_key="chat_bubble_skin_01", slot_kind=3, name="竹影清笺气泡",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/chat_bubble_skin_01", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=90,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉聊天气泡：世界、帮会与私聊发言呈现青竹宣纸底纹，由服务端权威校验透传。",
    ),
    dict(
        id=10, cosmetic_key="chat_bubble_skin_02", slot_kind=3, name="鎏金云纹气泡",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/chat_bubble_skin_02", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=100,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉聊天气泡：世界、帮会与私聊发言呈现鎏金卷云边饰，由服务端权威校验透传。",
    ),
    # slot_kind = 4: 战报/残局分享卡片底纹（2 款）
    dict(
        id=11, cosmetic_key="share_card_skin_01", slot_kind=4, name="墨韵宣纸战报帖",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/share_card_skin_01", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=110,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉分享卡片底纹：在聊天发送 #TJC 约战帖与 #PUZ 残局卡时呈现墨韵宣纸帖面。",
    ),
    dict(
        id=12, cosmetic_key="share_card_skin_02", slot_kind=4, name="赤金卷轴战报帖",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/share_card_skin_02", secondary_asset_key="",
        card_item_id=3417, card_item_count=1, spirit_shop_goods_id=307, enabled=1, sort_order=120,
        acquire_hint="百宝阁购「排面装饰券」解锁（80灵石）",
        desc="纯视觉分享卡片底纹：在聊天发送 #TJC 约战帖与 #PUZ 残局卡时呈现赤金卷轴帖面。",
    ),
    # id = 13: B5 Steam 创始支持者典藏包专属限定阵纹（card_item_id=0 不可券换）
    dict(
        id=13, cosmetic_key="formation_skin_starlight", slot_kind=1, name="星河初启阵纹",
        asset_key="UI/Ugui/ShowcaseCosmeticsV1/formation_skin_metal", secondary_asset_key="",
        card_item_id=0, card_item_count=0, spirit_shop_goods_id=0, enabled=1, sort_order=130,
        acquire_hint="留灯院创始支持者典藏包限定",
        desc="创始支持者纪念脚底阵纹：星河初启纯视觉特效，零数值加成，永久收藏。",
    ),
]


B2_ITEM_ROWS = [
    dict(
        id=3417, max_stack_size=99, equip_kind=0, battle_usable=0, battle_heal_hp=0, battle_heal_mp=0,
        name="排面装饰券",
        description="仅自用绑定外观券：可在衣柜「排面装饰」页永久解锁一款阵纹、入场横幅、聊天气泡或分享卡底纹（纯视觉零数值）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_friend_cosmetic_gift",
        equip_level=0, equip_class=0, affix_pool=0, affix_rule=0, equip_profile=0,
        forge_material_id=0, forge_material_count=0, artifact_id=0,
    ),
    dict(
        id=3418, max_stack_size=99, equip_kind=0, battle_usable=0, battle_heal_hp=0, battle_heal_mp=0,
        name="天墉灵灯",
        description="仅自用绑定主城同乐气氛道具：在归元城/天墉城和平分线施放，全图赏灯并邀≥10级道友沾喜气领150绑定银两（周封顶3000绑银）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_city_lantern",
        equip_level=0, equip_class=0, affix_pool=0, affix_rule=0, equip_profile=0,
        forge_material_id=0, forge_material_count=0, artifact_id=0,
    ),
    dict(
        id=3419, max_stack_size=99, equip_kind=0, battle_usable=0, battle_heal_hp=0, battle_heal_mp=0,
        name="庆功烟花",
        description="仅自用绑定主城同乐气氛道具：在归元城/天墉城和平分线燃放烟花，全图欢庆并邀≥10级道友领300绑定银两（周封顶3000绑银）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_city_firework",
        equip_level=0, equip_class=0, affix_pool=0, affix_rule=0, equip_profile=0,
        forge_material_id=0, forge_material_count=0, artifact_id=0,
    ),
    dict(
        id=3420, max_stack_size=99, equip_kind=0, battle_usable=0, battle_heal_hp=0, battle_heal_mp=0,
        name="仙茶同乐席",
        description="仅自用绑定主城同乐气氛道具：在归元城/天墉城和平分线摆开仙茶席，全图共饮并邀≥10级道友领400绑定银两（周封顶3000绑银）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_city_tea_table",
        equip_level=0, equip_class=0, affix_pool=0, affix_rule=0, equip_profile=0,
        forge_material_id=0, forge_material_count=0, artifact_id=0,
    ),
]


B2_SPIRIT_SHOP_GOODS = [
    dict(
        id=307, tab_id=3, sort_order=70, enabled=1, name="排面装饰券",
        description="仅自用绑定外观券：可在衣柜「排面装饰」页永久解锁一款阵纹、入场横幅、聊天气泡或战报分享卡底纹（纯视觉零数值）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_friend_cosmetic_gift",
        currency_type=1, price=80, max_buy_per_request=10,
        item_id_1=3417, item_count_1=1, item_bound_1=1,
        item_id_2=0, item_count_2=0, item_bound_2=0,
        limit_reset_kind=0, limit_count=0, limit_group_id=0, limit_group_weight=0,
        badge_text="排面装饰", start_time_ms=0, end_time_ms=0, grant_mode=0,
        item_id_3=0, item_count_3=0, item_bound_3=0,
        item_id_4=0, item_count_4=0, item_bound_4=0,
        original_price=0, min_level=0, max_level=0,
    ),
    dict(
        id=308, tab_id=3, sort_order=80, enabled=1, name="天墉灵灯",
        description="主城和平分线全城同乐气氛道具：放飞一盏天墉灵灯，全图路人赏灯并各领150绑定银两（周上限3000，0流通通胀）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_city_lantern",
        currency_type=1, price=20, max_buy_per_request=10,
        item_id_1=3418, item_count_1=1, item_bound_1=1,
        item_id_2=0, item_count_2=0, item_bound_2=0,
        limit_reset_kind=0, limit_count=0, limit_group_id=0, limit_group_weight=0,
        badge_text="全城同乐", start_time_ms=0, end_time_ms=0, grant_mode=0,
        item_id_3=0, item_count_3=0, item_bound_3=0,
        item_id_4=0, item_count_4=0, item_bound_4=0,
        original_price=0, min_level=0, max_level=0,
    ),
    dict(
        id=309, tab_id=3, sort_order=90, enabled=1, name="庆功烟花",
        description="主城和平分线全城同乐气氛道具：燃放漫天庆功烟花，全图路人观礼并各领300绑定银两（周上限3000，0流通通胀）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_city_firework",
        currency_type=1, price=50, max_buy_per_request=10,
        item_id_1=3419, item_count_1=1, item_bound_1=1,
        item_id_2=0, item_count_2=0, item_bound_2=0,
        limit_reset_kind=0, limit_count=0, limit_group_id=0, limit_group_weight=0,
        badge_text="全城同乐", start_time_ms=0, end_time_ms=0, grant_mode=0,
        item_id_3=0, item_count_3=0, item_bound_3=0,
        item_id_4=0, item_count_4=0, item_bound_4=0,
        original_price=0, min_level=0, max_level=0,
    ),
    dict(
        id=310, tab_id=3, sort_order=100, enabled=1, name="仙茶同乐席",
        description="主城和平分线全城同乐气氛道具：摆开一桌仙茶同乐席，全图路人共饮并各领400绑定银两（周上限3000，0流通通胀）。",
        icon_key="UI/Ugui/ShowcaseCosmeticsV1/icon_city_tea_table",
        currency_type=1, price=60, max_buy_per_request=10,
        item_id_1=3420, item_count_1=1, item_bound_1=1,
        item_id_2=0, item_count_2=0, item_bound_2=0,
        limit_reset_kind=0, limit_count=0, limit_group_id=0, limit_group_weight=0,
        badge_text="全城同乐", start_time_ms=0, end_time_ms=0, grant_mode=0,
        item_id_3=0, item_count_3=0, item_bound_3=0,
        item_id_4=0, item_count_4=0, item_bound_4=0,
        original_price=0, min_level=0, max_level=0,
    ),
]


TIP_GROUP_ADDITIONS = {
    "//appearance_error": [
        ("CityCelebrationNotSafeCity", "只能在归元城或天墉城和平分线施放全城同乐气氛道具。", False),
        ("CityCelebrationChannelFull", "当前分线同乐演出已满 5 场，请稍候片刻再施放。", False),
        ("CityCelebrationNoItem", "背包中缺少对应同乐气氛道具，可前往灵石百宝阁获取。", False),
        ("CityCelebrationNotFound", "该场全城同乐演出已结束，可期待下一次同乐欢庆。", False),
        ("CityCelebrationAlreadyClaimed", "你已经沾过本场同乐的喜气了，每场演出每人限领一次。", False),
        ("CityCelebrationLevelTooLow", "角色达到 10 级后即可领取全城同乐绑定银两小礼。", False),
        ("CityCelebrationWeeklyCapReached", "本周全城同乐绑定银两已达上限（3000），可继续观赏同乐特效。", False),
        ("CosmeticGiftNotFriend7Days", "需与对方互为好友满 7 天且未拉黑才可赠送永久外观或手记典藏。", False),
        ("CosmeticGiftTargetAlreadyOwned", "对方已拥有该永久外观或已解锁该期游历手记，无需重复赠送。", False),
        ("CosmeticGiftWeeklyLimitReached", "本周赠礼次数已达上限（5 笔），请下周一 05:00 后再试。", False),
        ("CosmeticGiftInvalidTarget", "仅允许使用真实充值灵石赠送可买断永久外观或游历手记典藏，不支持当前目标。", False),
        ("CosmeticGiftCurrencyNotEnough", "真实充值灵石不足，好友赠礼不支持使用绑定灵石抵扣。", False),
        ("CosmeticGiftPriceChanged", "赠礼标价已更新，请刷新后重试。", False),
        ("CosmeticGiftServiceUnavailable", "赠礼校验或投递服务暂不可用，请稍后重试。", True),
    ],
}


MESSAGE_ID_TXT = REPO_ROOT / "proto" / "message_id.txt"
MESSAGE_LIMITER_XLSX = DATA_DIR / "MessageLimiter.xlsx"
MESSAGE_LIMITER_SPECS = [
    ("SceneAppearanceClientPlayerEquipShowcaseCosmetic", 10, 1, 1000),
    ("SceneAppearanceClientPlayerCastCityCelebration", 5, 1, 1000),
    ("SceneAppearanceClientPlayerClaimCityCelebrationGift", 10, 1, 1000),
    ("SceneAppearanceClientPlayerSendCosmeticGift", 5, 1, 1000),
]


def _write_table(sheet: str, rows: list[dict], dry_run: bool) -> None:
    cols = schema_layout(sheet)
    col_names = [c.name for c in cols]
    xlsx_path = DATA_DIR / f"{sheet}.xlsx"

    wanted: list[list] = [
        list(col_names),
        [None] * len(cols),
        [None] * len(cols),
        [None] * len(cols),
        [c.comment for c in cols],
    ]
    for r in rows:
        wanted.append([None if r[name] == "" else r[name] for name in col_names])

    if xlsx_path.exists():
        wb = openpyxl.load_workbook(xlsx_path)
        ws = only_sheet(wb, xlsx_path, sheet)
        current = sheet_matrix(ws)
        if first_difference(current, wanted) is None:
            bind(sheet, xlsx_path)
            print(f"[ALREADY CURRENT] {rel(xlsx_path)}")
            return
        if dry_run:
            print(f"[dry-run] Would rewrite {rel(xlsx_path)} ({len(rows)} rows).")
            return
    elif dry_run:
        print(f"[dry-run] Would create {rel(xlsx_path)} ({len(rows)} rows).")
        return

    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = sheet
    for r_idx, row in enumerate(wanted, start=1):
        for c_idx, val in enumerate(row, start=1):
            if val is not None:
                ws.cell(row=r_idx, column=c_idx, value=val)
    wb.save(xlsx_path)

    check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, sheet)
    diff = first_difference(sheet_matrix(check_ws), wanted)
    if diff:
        fail(f"after saving, {xlsx_path.name} read back differs ({diff})")
    bind(sheet, xlsx_path)
    print(f"[DONE] {rel(xlsx_path)}")


def _append_rows_by_header(xlsx_path: Path, sheet: str, rows_to_ensure: list[dict], dry_run: bool) -> None:
    wb = openpyxl.load_workbook(xlsx_path)
    ws = only_sheet(wb, xlsx_path, sheet)
    before = sheet_matrix(ws)
    header = [str(v).strip() if v is not None else "" for v in before[HEADER_ROW - 1]]
    col_by_name = {name: idx + 1 for idx, name in enumerate(header) if name}
    existing_ids: dict[int, int] = {}
    last_row = DATA_BEGIN_ROW - 1
    for r_idx, row in enumerate(before[DATA_BEGIN_ROW - 1:], start=DATA_BEGIN_ROW):
        if any(v is not None for v in row):
            last_row = r_idx
            if row[0] is not None:
                existing_ids[int(row[0])] = r_idx

    patches: list[Patch] = []
    next_row = last_row + 1
    for spec in rows_to_ensure:
        rid = int(spec["id"])
        target_row = existing_ids.get(rid)
        if target_row is None:
            target_row = next_row
            next_row += 1
        for k, v in spec.items():
            if k not in col_by_name:
                continue
            col = col_by_name[k]
            cur = before[target_row - 1][col - 1] if target_row <= len(before) and col <= len(before[target_row - 1]) else None
            want = None if v == "" else v
            if cur != want:
                patches.append((target_row, col, want))

    if not patches:
        print(f"[ALREADY CURRENT] {rel(xlsx_path)}")
        return
    if dry_run:
        print(f"[dry-run] Would patch {len(patches)} cells in {rel(xlsx_path)}")
        return
    apply_patches_and_verify(xlsx_path, sheet, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(xlsx_path)} ({len(patches)} cells)")


def cmd_tables(dry_run: bool) -> None:
    _write_table(COSMETIC_SHOWCASE_SHEET, COSMETIC_SHOWCASE_ROWS, dry_run)
    _append_rows_by_header(DATA_DIR / "Item.xlsx", "Item", B2_ITEM_ROWS, dry_run)
    _append_rows_by_header(DATA_DIR / "SpiritShopGoods.xlsx", "SpiritShopGoods", B2_SPIRIT_SHOP_GOODS, dry_run)


def _tip_names_by_row(ws) -> dict[str, int]:
    out: dict[str, int] = {}
    for r in range(1, ws.max_row + 1):
        v = ws.cell(r, 1).value
        if isinstance(v, str) and v.strip():
            out[v.strip()] = r
    return out


def _tip_group_insert_row(ws, group_prefix: str) -> int:
    start = None
    for r in range(1, ws.max_row + 1):
        v = ws.cell(r, 1).value
        if isinstance(v, str) and v.strip().startswith(group_prefix):
            start = r
            break
    if start is None:
        fail(f"Tip.xlsx missing group header starting with {group_prefix!r}")
    r = start + 1
    while r <= ws.max_row:
        v = ws.cell(r, 1).value
        if isinstance(v, str) and v.strip().startswith("//"):
            return r
        r += 1
    return ws.max_row + 1


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = wb.active
    header = [str(ws.cell(1, c).value or "").strip() for c in range(1, ws.max_column + 1)]
    if "fault" not in header:
        fail(f"Tip.xlsx missing fault column: {header}")
    fault_col = header.index("fault") + 1
    existing = _tip_names_by_row(ws)

    to_apply: list[tuple[str, list[tuple[str, str, bool]]]] = []
    for grp, specs in TIP_GROUP_ADDITIONS.items():
        missing = [s for s in specs if s[0] not in existing]
        if not missing:
            print(f"[ALREADY CURRENT] Tip.xlsx {grp}")
            continue
        to_apply.append((grp, missing))

    if not to_apply:
        return

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
