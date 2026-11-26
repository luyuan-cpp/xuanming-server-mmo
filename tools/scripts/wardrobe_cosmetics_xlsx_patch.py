#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""衣柜灵宠幻化与技能特效 V1(docs/planning/wardrobe-pet-morph-and-skill-vfx-v1.md)的 xlsx 幂等补丁脚本。

用法(仓库根目录执行):
    python tools/scripts/wardrobe_cosmetics_xlsx_patch.py new-tables       # ① 生成/更新 PetCosmetic.xlsx 与 SkillVfxCosmetic.xlsx
    python tools/scripts/wardrobe_cosmetics_xlsx_patch.py voucher-closure  # ② 追加 3411/3412 外观券至 Item.xlsx 与 303/304 至 SpiritShopGoods.xlsx
    python tools/scripts/wardrobe_cosmetics_xlsx_patch.py message-limiter  # ③ 跑完 proto-gen 后为 2 条新 RPC 追加 MessageLimiter 行
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


PET_COSMETIC_SHEET = "PetCosmetic"
PET_COSMETIC_ROWS = [
    dict(
        id=1,
        cosmetic_key="pet_morph_cangzhanglin",
        name="苍嶂麟",
        target_pet_table_id=0,
        visual_pet_table_id=12,
        visual_art_key="07-cangzhanglin",
        enabled=1,
        sort_order=10,
        mall_sku_id=0,
        acquire_hint="百宝阁购「灵宠幻化符」解锁（120灵石）",
        desc="幻化仅改变外观，将当前灵宠化作苍嶂麟仪态，不影响资质、技能与任何战斗数值。",
        card_item_id=3411,
        card_item_count=1,
        spirit_shop_goods_id=303,
    ),
    dict(
        id=2,
        cosmetic_key="pet_morph_feierling",
        name="绯耳灵",
        target_pet_table_id=0,
        visual_pet_table_id=18,
        visual_art_key="14-feierling",
        enabled=1,
        sort_order=20,
        mall_sku_id=0,
        acquire_hint="百宝阁购「灵宠幻化符」解锁（120灵石）",
        desc="幻化仅改变外观，将当前灵宠化作绯耳灵仪态，不影响资质、技能与任何战斗数值。",
        card_item_id=3411,
        card_item_count=1,
        spirit_shop_goods_id=303,
    ),
    dict(
        id=3,
        cosmetic_key="pet_morph_yuexianshi",
        name="月弦师",
        target_pet_table_id=0,
        visual_pet_table_id=16,
        visual_art_key="12-yuexianshi",
        enabled=1,
        sort_order=30,
        mall_sku_id=0,
        acquire_hint="赛季荣誉或活动专属解锁",
        desc="幻化仅改变外观，将当前灵宠化作月弦师仪态，不影响资质、技能与任何战斗数值。",
        card_item_id=0,
        card_item_count=0,
        spirit_shop_goods_id=0,
    ),
]

SKILL_VFX_SHEET = "SkillVfxCosmetic"
SKILL_VFX_ROWS = [
    dict(
        id=1,
        vfx_key="vfx_xuanfu_gilded",
        name="鎏金符纹",
        skill_table_id=1201,
        fx_id="vfx_xuanfu_alt_impact",
        projectile_fx_id="vfx_xuanfu_alt_projectile",
        telegraph_fx_id="",
        tint_hex="",
        enabled=1,
        sort_order=10,
        mall_sku_id=0,
        acquire_hint="百宝阁购「技能特效卷」解锁（180灵石）",
        desc="特效仅改变战斗画面，为玄符落曜赋予专属符纹替换特效，不改变伤害、范围与目标规则。",
        card_item_id=3412,
        card_item_count=1,
        spirit_shop_goods_id=304,
    ),
    dict(
        id=2,
        vfx_key="vfx_zixiao_amethyst",
        name="紫霄惊雷",
        skill_table_id=1202,
        fx_id="vfx_zixiao_alt_strike",
        projectile_fx_id="",
        telegraph_fx_id="vfx_zixiao_alt_telegraph",
        tint_hex="",
        enabled=1,
        sort_order=20,
        mall_sku_id=0,
        acquire_hint="百宝阁购「技能特效卷」解锁（180灵石）",
        desc="特效仅改变战斗画面，为紫霄雷阵赋予专属雷阵替换特效，不改变伤害、范围与目标规则。",
        card_item_id=3412,
        card_item_count=1,
        spirit_shop_goods_id=304,
    ),
    dict(
        id=3,
        vfx_key="vfx_hanxing_aurora",
        name="寒星流华",
        skill_table_id=1203,
        fx_id="vfx_hanxing_alt_impact",
        projectile_fx_id="vfx_hanxing_alt_projectile",
        telegraph_fx_id="",
        tint_hex="",
        enabled=1,
        sort_order=30,
        mall_sku_id=0,
        acquire_hint="赛季荣誉或活动专属解锁",
        desc="特效仅改变战斗画面，为寒星凝魄赋予专属寒星替换特效，不改变伤害、范围与目标规则。",
        card_item_id=0,
        card_item_count=0,
        spirit_shop_goods_id=0,
    ),
]

VOUCHER_ITEM_ROWS = [
    dict(
        id=3411,
        max_stack_size=99,
        equip_kind=0,
        battle_usable=0,
        battle_heal_hp=0,
        battle_heal_mp=0,
        name="灵宠幻化符",
        description="仅自用绑定幻化符：可在衣柜「灵宠」页永久解锁一款支持兑换的灵宠幻化，已拥有幻化随时免费穿脱（纯外观零数值）。",
        icon_key="UI/Ugui/WardrobeCosmetics/item_pet_morph_charm",
        equip_level=0,
        equip_class=0,
        affix_pool=0,
        affix_rule=0,
        equip_profile=0,
        forge_material_id=0,
        forge_material_count=0,
        artifact_id=0,
    ),
    dict(
        id=3412,
        max_stack_size=99,
        equip_kind=0,
        battle_usable=0,
        battle_heal_hp=0,
        battle_heal_mp=0,
        name="技能特效卷",
        description="仅自用绑定特效卷：可在衣柜「特效」页永久解锁一款支持兑换的技能特效，已拥有特效随时免费切换（纯画面零数值）。",
        icon_key="UI/Ugui/WardrobeCosmetics/item_skill_vfx_scroll",
        equip_level=0,
        equip_class=0,
        affix_pool=0,
        affix_rule=0,
        equip_profile=0,
        forge_material_id=0,
        forge_material_count=0,
        artifact_id=0,
    ),
]

SPIRIT_SHOP_VOUCHER_ROWS = [
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

MESSAGE_ID_TXT = REPO_ROOT / "proto" / "message_id.txt"
MESSAGE_LIMITER_XLSX = DATA_DIR / "MessageLimiter.xlsx"
MESSAGE_LIMITER_SPECS = [
    ("SceneAppearanceClientPlayerEquipPetCosmetic", 15, 60, 4),
    ("SceneAppearanceClientPlayerEquipSkillVfx", 15, 60, 4),
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
    for item in rows_to_ensure:
        rid = int(item["id"])
        if rid in existing_ids:
            row_idx = existing_ids[rid]
            cur_row = before[row_idx - 1]
            mismatch = []
            for k, v in item.items():
                c = col_by_name[k]
                cur_val = cur_row[c - 1] if c - 1 < len(cur_row) else None
                want_val = None if v == "" else v
                if cur_val != want_val:
                    mismatch.append((k, cur_val, want_val))
            if not mismatch:
                print(f"[ALREADY CURRENT] {xlsx_path.name} id={rid}")
                continue
            fail(f"{xlsx_path.name} id={rid} already exists with different values: {mismatch}")
        for k, v in item.items():
            if k not in col_by_name:
                fail(f"{xlsx_path.name} missing header column {k!r}")
            patches.append((next_row, col_by_name[k], None if v == "" else v))
        next_row += 1

    if not patches:
        return
    if dry_run:
        print(f"[dry-run] Would append {len(rows_to_ensure)} rows to {rel(xlsx_path)}.")
        return
    apply_patches_and_verify(xlsx_path, sheet, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(xlsx_path)}")


def cmd_voucher_closure(dry_run: bool) -> None:
    _append_rows_by_header(DATA_DIR / "Item.xlsx", "Item", VOUCHER_ITEM_ROWS, dry_run)
    _append_rows_by_header(DATA_DIR / "SpiritShopGoods.xlsx", "SpiritShopGoods", SPIRIT_SHOP_VOUCHER_ROWS, dry_run)
    cmd_new_tables(dry_run)


def cmd_new_tables(dry_run: bool) -> None:
    _write_table(PET_COSMETIC_SHEET, PET_COSMETIC_ROWS, dry_run)
    _write_table(SKILL_VFX_SHEET, SKILL_VFX_ROWS, dry_run)


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
            fail(f"{name} not found in {rel(MESSAGE_ID_TXT)}; run proto-gen first")
        targets.append((msg_ids[name], max_req, window, tip))

    wb = openpyxl.load_workbook(MESSAGE_LIMITER_XLSX)
    ws = only_sheet(wb, MESSAGE_LIMITER_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:4] != ["id", "max_requests", "time_window", "tip_message"]:
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
        next_row += 1

    if not patches:
        return
    if dry_run:
        print(f"[dry-run] Would append {len(patches) // 4} MessageLimiter rows.")
        return
    apply_patches_and_verify(MESSAGE_LIMITER_XLSX, "MessageLimiter", wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(MESSAGE_LIMITER_XLSX)}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=("new-tables", "voucher-closure", "message-limiter"))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if args.command == "new-tables":
        cmd_new_tables(args.dry_run)
    elif args.command == "voucher-closure":
        cmd_voucher_closure(args.dry_run)
    else:
        cmd_message_limiter(args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
