#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""不肝不氪增收体系 V2 · 批次 B3（docs/planning/fair-monetization-expansion-v2.md）xlsx 幂等补丁脚本。

用法（仓库根目录执行）：
    python tools/scripts/fair_monetization_b3_xlsx_patch.py tables           # ① 追加 SkillScrollCraft 8/9、Skill 16001..16011、Buff 160010..160111
    python tools/scripts/fair_monetization_b3_xlsx_patch.py tip-codes        # ② 追加 bag/pet/season/guild 错误码
    python tools/scripts/fair_monetization_b3_xlsx_patch.py message-limiter  # ③ 跑完 proto-gen 后为 6 条 B3 C2S RPC 追加 MessageLimiter 行
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
        fail,
        only_sheet,
        rel,
        sheet_matrix,
    )
except ImportError as exc:  # pragma: no cover
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


B3_SCROLL_ROWS = [
    dict(
        id=8,
        name="五行合纵卷",
        description="横向协作与反制常规战法卷宗（10 项：断盾/降速/回蓝/群净/联防/毒爆/反震/破防追击/承伤回蓝/灵宠反击），0 纯倍率上位替代、不稀释老卷宗，支持随机拓印与定向保底。",
        rarity_tier=0,
        fragment_item_id=3406,
        random_fragment_count=20,
        random_gold_cost=50,
        targeted_fragment_count=40,
        targeted_gold_cost=100,
        dismantle_fragment_count=16,
        rare_scroll_item_id=0,
        rare_scroll_count=0,
        rare_ink_item_id=0,
        rare_ink_count=0,
        rare_gold_cost=0,
        enabled=1,
    ),
    dict(
        id=9,
        name="合纵绝学卷",
        description="横向合纵开场稀世绝学卷宗（合纵星阵，本批稀世占比 9.1% <= 10%），消耗玩法绝学残卷与绑定天工秘墨 100% 定向合成。",
        rarity_tier=1,
        fragment_item_id=3406,
        random_fragment_count=0,
        random_gold_cost=0,
        targeted_fragment_count=0,
        targeted_gold_cost=0,
        dismantle_fragment_count=0,
        rare_scroll_item_id=3407,
        rare_scroll_count=1,
        rare_ink_item_id=3408,
        rare_ink_count=1,
        rare_gold_cost=100,
        enabled=1,
    ),
]

B3_BUFF_ROWS = [
    # 16001 断盾剑气: 物理攻击 + 驱散 1 增益
    dict(id=160010, designer="H01断盾剑气伤害", level=1, max_layer=1, battle_op=1),
    dict(id=160011, designer="H01断盾驱益", level=1, max_layer=1, battle_op=5, effect_count=1),
    # 16002 破咒焚心: 法术双攻 + 降速 15% 3 回合
    dict(id=160020, designer="H02破咒焚心伤害", level=1, max_layer=1, battle_op=1),
    dict(id=160021, designer="H02破咒降速", tag=["Debuff"], level=1, max_layer=1, battle_op=6,
         ratio=-0.15, duration_rounds=3, modifier_stat=4, refresh_policy=1, tick_on_apply=1),
    # 16003 灵犀共济: 双友小疗 + 回蓝 15%
    dict(id=160030, designer="H03灵犀共济治疗", level=1, max_layer=1, battle_op=2, per_level=14, ratio=0, value_basis=0),
    dict(id=160031, designer="H03灵犀共济回蓝", level=1, max_layer=1, battle_op=3, ratio=0.15, value_basis=4),
    # 16004 清流洗尘: 友方至多 3 目标净化 1 减益
    dict(id=160040, designer="H04清流洗尘净化", level=1, max_layer=1, battle_op=4, effect_count=1),
    # 16005 守御联璧: 友方至多 3 目标 2 回合联防护盾
    dict(id=160050, designer="H05守御联璧护盾", tag=["Buff", "Shield"], level=1, max_layer=1, battle_op=9,
         per_level=14, ratio=0, value_basis=0, duration_rounds=2, refresh_policy=2, effect_priority=20, tick_on_apply=1),
    # 16006 引火毒爆: 法术单体 + 立刻引爆中毒/灼烧 1 次
    dict(id=160060, designer="H06引火毒爆伤害", level=1, max_layer=1, battle_op=1),
    dict(id=160061, designer="H06引火毒爆结算", level=1, max_layer=1, battle_op=14,
         required_tag="Poison", second_required_tag="Burn", effect_count=1),
    # 16007 借势反震: 友方双目标 2 回合 20% 物理反震
    dict(id=160070, designer="H07借势反震", tag=["Buff"], level=1, max_layer=1, battle_op=12,
         trigger_event=3, damage_filter=1, duration_rounds=2, secondary_ratio=0.2, effect_priority=50, tick_on_apply=1),
    # 16008 伏兵协击 (追击): 普攻命中附加 2 回合 -12% 防御
    dict(id=160080, designer="H08伏兵协击追击", level=1, max_layer=1, infinite_duration=1,
         target_sub_buff=160081, battle_op=21, trigger_event=1, per_round_limit=1),
    dict(id=160081, designer="H08伏兵破防", tag=["Break", "Debuff"], level=1, max_layer=1, battle_op=6,
         ratio=-0.12, duration_rounds=2, modifier_stat=3, refresh_policy=1, tick_on_apply=1),
    # 16009 灵脉回响 (被动): 受直接伤害回复 4% 法力
    dict(id=160090, designer="H09灵脉回响", level=1, max_layer=1, infinite_duration=1, battle_op=23,
         trigger_event=3, ratio=0.04, value_basis=4, per_round_limit=1),
    # 16010 护主灵契 (灵宠被动): 受物理直伤 22% 概率反击
    dict(id=160100, designer="H10护主灵契", level=1, max_layer=1, infinite_duration=1, battle_op=19,
         trigger_event=3, ratio=0.9, damage_filter=1, trigger_chance=0.22, per_round_limit=1),
    # 16011 合纵星阵 (稀世开场绝学): 开场为友方至多 5 名附加 3 回合星阵护盾 + 8% 减伤
    dict(id=160110, designer="H11合纵星阵护盾", tag=["Buff", "Shield"], level=1, max_layer=1, battle_op=9,
         per_level=12, ratio=0, value_basis=0, duration_rounds=3, refresh_policy=2, effect_priority=20, tick_on_apply=1),
    dict(id=160111, designer="H11合纵星阵稳守", tag=["Buff"], level=1, max_layer=1, battle_op=6,
         ratio=-0.08, duration_rounds=3, modifier_stat=16, refresh_policy=1, tick_on_apply=1),
]

B3_SKILL_ROWS = [
    dict(id=16001, skill_type=1, targeting_mode=1, require_target=1, effect=[160010, 160011],
         damage="2*level", damage_type=1, attack_multiplier=1.15, name="断盾剑气",
         description="单体物理斩击并驱散目标 1 项增益状态，克制厚盾与护阵体系。",
         icon_key="UI/Skills/16001", category="主动", unlock_level=1, max_rank=10,
         target_team=1, max_targets=1, mana_percent=0.08, cooldown_rounds=1,
         battle_model=1, caster_mask=1, ability_kind=1, pet_max_targets=1,
         prototype_code="H01", action_tags=1, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16002, skill_type=1, targeting_mode=1, require_target=1, effect=[160020, 160021],
         damage="2*level", damage_type=0, attack_multiplier=0.95, name="破咒焚心",
         description="法术攻击至多 2 名敌方并降低目标 15% 速度（持续 3 回合），扰乱敌方先手节奏。",
         icon_key="UI/Skills/16002", category="主动", unlock_level=1, max_rank=10,
         target_team=1, max_targets=2, mana_percent=0.10, cooldown_rounds=2,
         battle_model=1, caster_mask=1, ability_kind=1, pet_max_targets=1,
         prototype_code="H02", action_tags=2, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16003, skill_type=1, targeting_mode=1, require_target=1, effect=[160030, 160031],
         name="灵犀共济",
         description="为至多 2 名友方回复适量气血并恢复 15% 法力，支撑拉锯战续航。",
         icon_key="UI/Skills/16003", category="主动", unlock_level=1, max_rank=10,
         target_team=2, max_targets=2, mana_percent=0.08, cooldown_rounds=2,
         battle_model=1, caster_mask=1, ability_kind=1, pet_max_targets=1,
         prototype_code="H03", action_tags=4, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16004, skill_type=1, targeting_mode=1, require_target=1, effect=[160040],
         name="清流洗尘",
         description="为至多 3 名友方驱散 1 项减益状态（人物与灵宠均可研习），反制毒火与控制队。",
         icon_key="UI/Skills/16004", category="主动", unlock_level=1, max_rank=10,
         target_team=2, max_targets=3, mana_percent=0.09, cooldown_rounds=2,
         battle_model=1, caster_mask=3, ability_kind=1, pet_max_targets=2,
         prototype_code="H04", action_tags=8, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16005, skill_type=1, targeting_mode=1, require_target=1, effect=[160050],
         name="守御联璧",
         description="为至多 3 名友方施加持续 2 回合的联防护盾，化解敌方多目标爆发。",
         icon_key="UI/Skills/16005", category="主动", unlock_level=1, max_rank=10,
         target_team=2, max_targets=3, mana_percent=0.10, cooldown_rounds=2,
         battle_model=1, caster_mask=1, ability_kind=1, pet_max_targets=1,
         prototype_code="H05", action_tags=8, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16006, skill_type=1, targeting_mode=1, require_target=1, effect=[160060, 160061],
         damage="2*level", damage_type=0, attack_multiplier=0.90, name="引火毒爆",
         description="法术打击单体目标，并立即结算其身上 1 项中毒或灼烧的剩余持续伤害，协同毒火流派。",
         icon_key="UI/Skills/16006", category="主动", unlock_level=1, max_rank=10,
         target_team=1, max_targets=1, mana_percent=0.09, cooldown_rounds=2,
         battle_model=1, caster_mask=1, ability_kind=1, pet_max_targets=1,
         prototype_code="H06", action_tags=2, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16007, skill_type=1, targeting_mode=1, require_target=1, effect=[160070],
         name="借势反震",
         description="为至多 2 名友方施加 2 回合 20% 物理反震，专门反制高频率物理连击流派。",
         icon_key="UI/Skills/16007", category="主动", unlock_level=1, max_rank=10,
         target_team=2, max_targets=2, mana_percent=0.09, cooldown_rounds=2,
         battle_model=1, caster_mask=1, ability_kind=1, pet_max_targets=1,
         prototype_code="H07", action_tags=8, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16008, targeting_mode=1, require_target=1, init_effect=[160080],
         name="伏兵协击",
         description="追击战法（人物/灵宠共用）：普通攻击命中后每回合至多 1 次使目标防御降低 12%（持续 2 回合），为队友集火铺路。",
         icon_key="UI/Skills/16008", category="追击", unlock_level=1, max_rank=10,
         target_team=3, max_targets=1, mana_percent=0, cooldown_rounds=0,
         battle_model=1, caster_mask=3, ability_kind=3, pet_max_targets=1,
         prototype_code="H08", action_tags=1, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16009, targeting_mode=1, require_target=1, init_effect=[160090],
         name="灵脉回响",
         description="被动战法：每回合首次承受直接伤害时回复 4% 法力，有效反制吸蓝耗竭打法。",
         icon_key="UI/Skills/16009", category="被动", unlock_level=1, max_rank=10,
         target_team=3, max_targets=1, mana_percent=0, cooldown_rounds=0,
         battle_model=1, caster_mask=1, ability_kind=2, pet_max_targets=1,
         prototype_code="H09", action_tags=8, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16010, targeting_mode=1, require_target=1, init_effect=[160100],
         name="护主灵契",
         description="灵宠专属被动：承受敌方物理直接伤害时有 22% 概率反击（每回合限 1 次），丰富防御反击型灵宠构筑。",
         icon_key="UI/Skills/16010", category="被动", unlock_level=1, max_rank=10,
         target_team=3, max_targets=1, mana_percent=0, cooldown_rounds=0,
         battle_model=1, caster_mask=2, ability_kind=2, pet_max_targets=1,
         prototype_code="H10", action_tags=1, forbid_copy=0, season_id=0, scroll_id=8, rarity_tier=0),
    dict(id=16011, targeting_mode=1, require_target=1, init_effect=[160110, 160111],
         name="合纵星阵",
         description="开场绝学（稀世）：开场为友方至多 5 名目标展开持续 3 回合的星阵护盾并降低 8% 所受伤害，稳固全队前期阵脚。",
         icon_key="UI/Skills/16011", category="开场", unlock_level=1, max_rank=10,
         target_team=2, max_targets=5, mana_percent=0, cooldown_rounds=0,
         battle_model=1, caster_mask=1, ability_kind=4, pet_max_targets=1,
         prototype_code="H11", action_tags=8, forbid_copy=0, season_id=0, scroll_id=9, rarity_tier=1),
]

TIP_GROUP_ADDITIONS: dict[str, list[tuple[str, str, bool]]] = {
    "//bag_error": [
        ("BagExpandMaxReached", "仓库格子已达扩展上限", False),
        ("BagExpandPriceChanged", "仓库扩容报价或当前容量已变化，请刷新后重试", False),
    ],
    "//pet_error": [
        ("PetSquadSlotLocked", "该灵宠编队槽位尚未解锁", False),
        ("PetSquadMaxSlotsReached", "灵宠编队预设槽位已达上限", False),
        ("PetSquadPriceChanged", "灵宠编队槽位数量或报价已变化，请刷新后重试", False),
    ],
    "//season_error": [
        ("SeasonBuildBookmarkMaxExpanded", "战报配装收藏夹容量已达上限", False),
        ("SeasonBuildBookmarkPriceChanged", "收藏夹容量或报价已变化，请刷新后重试", False),
    ],
    "//guild_error": [
        ("GuildEmblemInvalid", "无效的帮会徽记款式", False),
        ("GuildEmblemAlreadyUnlocked", "本帮已解锁该款帮会徽记，无需重复购买", False),
        ("GuildEmblemNotUnlocked", "本帮尚未解锁该款帮会徽记", False),
        ("GuildEmblemPriceChanged", "帮会徽记报价已变化，请刷新后重试", False),
    ],
}

MESSAGE_LIMITER_SERVICES = [
    ("SceneBagClientPlayerExpandBagCapacity", 5, 1, 1000),
    ("ScenePetClientPlayerSavePetSquadPreset", 10, 1, 1000),
    ("ScenePetClientPlayerUnlockPetSquadPresetSlot", 5, 1, 1000),
    ("SceneSeasonClientPlayerExpandSavedBuildSlots", 5, 1, 1000),
    ("GuildServiceUnlockGuildEmblem", 5, 1, 1000),
    ("GuildServiceEquipGuildEmblem", 10, 1, 1000),
]


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
                fail(f"{xlsx_path.name} missing column {k!r}")
            base_col = col_by_name[k]
            values = v if isinstance(v, (list, tuple)) else [v]
            for offset, item in enumerate(values):
                col = base_col + offset
                cur = before[target_row - 1][col - 1] if target_row <= len(before) and col <= len(before[target_row - 1]) else None
                want = None if item == "" else item
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
    _append_rows_by_header(DATA_DIR / "SkillScrollCraft.xlsx", "SkillScrollCraft", B3_SCROLL_ROWS, dry_run)
    _append_rows_by_header(DATA_DIR / "Buff.xlsx", "Buff", B3_BUFF_ROWS, dry_run)
    _append_rows_by_header(DATA_DIR / "Skill.xlsx", "Skill", B3_SKILL_ROWS, dry_run)


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
    print(f"[DONE] Updated {rel(TIP_XLSX)}")


def _parse_message_ids() -> dict[str, int]:
    path = REPO_ROOT / "proto" / "message_id.txt"
    out: dict[str, int] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or "=" not in line:
            continue
        lhs, rhs = line.split("=", 1)
        out[rhs.strip()] = int(lhs.strip())
    return out


def cmd_message_limiter(dry_run: bool) -> None:
    msg_ids = _parse_message_ids()
    targets = []
    for name, max_req, window, tip in MESSAGE_LIMITER_SERVICES:
        mid = msg_ids.get(name)
        if mid is None:
            fail(f"proto/message_id.txt missing {name!r} (run dev.bat proto first)")
        targets.append((mid, max_req, window, tip))

    xlsx_path = DATA_DIR / "MessageLimiter.xlsx"
    wb = openpyxl.load_workbook(xlsx_path)
    ws = only_sheet(wb, xlsx_path, "MessageLimiter")
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
        patches += [(next_row, 1, mid), (next_row, 2, max_req), (next_row, 3, window), (next_row, 4, tip)]
        next_row += 1
    if not patches:
        return
    if dry_run:
        print(f"[dry-run] Would append {len(patches) // 4} MessageLimiter rows")
        return
    apply_patches_and_verify(xlsx_path, "MessageLimiter", wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] Updated {rel(xlsx_path)}")


def main() -> int:
    parser = argparse.ArgumentParser(description="Fair monetization V2 Batch B3 xlsx patcher")
    parser.add_argument("command", choices=["tables", "tip-codes", "message-limiter"])
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if args.command == "tables":
        cmd_tables(args.dry_run)
    elif args.command == "tip-codes":
        cmd_tip_codes(args.dry_run)
    elif args.command == "message-limiter":
        cmd_message_limiter(args.dry_run)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
