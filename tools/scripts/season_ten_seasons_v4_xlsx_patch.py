#!/usr/bin/env python3
"""Idempotent Excel patcher for Season V4 (10 themed seasons S1-S10, 40 seals, 40 skills, buffs, tips, limiter).

Follows docs/planning/season-ten-seasons-infinite-cycle-v4.md and data/AGENTS.md.
Never alters S1 skills (15001..15004) or S1 buffs (150010..150040); upgrades S2 skills/seals and adds S3..S10.
"""
from __future__ import annotations

from pathlib import Path
import openpyxl

REPO = Path(__file__).resolve().parents[2]
DATA = REPO / "data"


def header_col_map(ws) -> dict[str, int]:
    return {
        str(ws.cell(1, c).value).strip(): c
        for c in range(1, ws.max_column + 1)
        if ws.cell(1, c).value is not None and str(ws.cell(1, c).value).strip() != ""
    }


def ensure_columns(ws, cols: list[tuple[str, str]]) -> dict[str, int]:
    hmap = header_col_map(ws)
    next_col = ws.max_column + 1
    for name, comment in cols:
        if name not in hmap:
            ws.cell(1, next_col, name)
            ws.cell(5, next_col, comment)
            hmap[name] = next_col
            next_col += 1
    return hmap


def upsert_by_id(ws, hmap: dict[str, int], row_data: dict[str, object], multi_cols: dict[str, list[object]] | None = None) -> None:
    target_id = int(row_data["id"])
    row_idx = None
    for r in range(6, ws.max_row + 1):
        val = ws.cell(r, 1).value
        if val is not None and str(val).strip() != "" and int(val) == target_id:
            row_idx = r
            break
    if row_idx is None:
        row_idx = ws.max_row + 1
        while row_idx > 6 and all(ws.cell(row_idx - 1, c).value in (None, "") for c in range(1, ws.max_column + 1)):
            row_idx -= 1
    # Clear row first when updating an existing seasonal row so stale columns never linger
    for c in range(1, ws.max_column + 1):
        ws.cell(row_idx, c, None)
    for k, v in row_data.items():
        if k not in hmap:
            raise KeyError(f"Column '{k}' missing in {ws.title}")
        ws.cell(row_idx, hmap[k], v)
    if multi_cols:
        for base_col, vals in multi_cols.items():
            start = hmap[base_col]
            for offset, v in enumerate(vals):
                ws.cell(row_idx, start + offset, v)


def patch_season_rule() -> None:
    path = DATA / "SeasonRule.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    hmap = ensure_columns(
        ws,
        [
            ("cycle_order", "自动循环池顺位（0不入池，1..N严格连续）"),
            ("cycle_duration_days", "自动无限循环单届持续天数（0关闭，[7,180]开启）"),
        ],
    )

    seasons = [
        (1, "破阵之季", True, 101, "破阵宗师", "UI/SeasonHonors/honor_s1_pozhen",
         "首季破阵之季：破盾压制与短回合破甲，4招免费限定技能，单槽赛季印轻属性上限气血+3%/防御+3%。"),
        (2, "玄脉之季", False, 201, "玄脉行者", "UI/SeasonHonors/honor_s2_xuanmai",
         "第二季玄脉之季：易伤聚火、急救稳血与厚盾续流，4招免费限定技能，单槽赛季印零速度零控制。"),
        (3, "离火之季", False, 301, "离火尊者", "UI/SeasonHonors/honor_s3_lihuo",
         "第三季离火之季：灼烧叠势、炎刃引爆与炽幕净秽，4招免费限定技能，单槽赛季印零速度零控制。"),
        (4, "青木之季", False, 401, "青木济世", "UI/SeasonHonors/honor_s4_qingmu",
         "第四季青木之季：毒蔓缠甲、枯荣续航与苍木筑垒，4招免费限定技能，单槽赛季印零速度零控制。"),
        (5, "玄水之季", False, 501, "玄水清流", "UI/SeasonHonors/honor_s5_xuanshui",
         "第五季玄水之季：精准斩残、卸势净尘与潮汐控蓝，4招免费限定技能，单槽赛季印零速度零控制。"),
        (6, "厚土之季", False, 601, "厚土宗师", "UI/SeasonHonors/honor_s6_houtu",
         "第六季厚土之季：穿防重锤、崩势驱敌与铁壁固本，4招免费限定技能，单槽赛季印零速度零控制。"),
        (7, "庚金之季", False, 701, "庚金剑首", "UI/SeasonHonors/honor_s7_gengjin",
         "第七季庚金之季：点杀破核、专挑脆皮与一击蓄锋，4招免费限定技能，单槽赛季印零速度零控制。"),
        (8, "太阴之季", False, 801, "太阴巡夜", "UI/SeasonHonors/honor_s8_taiyin",
         "第八季太阴之季：幽影点弱、延毒绵耗与素月保命，4招免费限定技能，单槽赛季印零速度零控制。"),
        (9, "天雷之季", False, 901, "天雷御使", "UI/SeasonHonors/honor_s9_tianlei",
         "第九季天雷之季：雷枪破盾、群雷压核与雷罡反制，4招免费限定技能，单槽赛季印零速度零控制。"),
        (10, "混元之季", False, 1001, "混元大宗", "UI/SeasonHonors/honor_s10_hunyuan",
         "第十季混元之季：五行合流、吸血反打与两仪均衡，4招免费限定技能，单槽赛季印零速度零控制。"),
    ]

    for sid, name, enabled, default_seal, title, icon, desc in seasons:
        upsert_by_id(
            ws,
            hmap,
            {
                "id": sid,
                "name": name,
                "starts_at_ms": 0,
                "ends_at_ms": 0,
                "enabled": enabled,
                "seal_max_points": 12,
                "pve_win_points": 1,
                "pvp_win_points": 2,
                "pvp_other_points": 1,
                "max_health_bps_cap": 300,
                "defense_bps_cap": 300,
                "catchup_currency_type": 1,
                "catchup_price_per_point": 10,
                "default_seal_id": default_seal,
                "desc": desc,
                "honor_title_name": title,
                "honor_badge_icon_key": icon,
                "cycle_order": sid,
                "cycle_duration_days": 0,
            },
        )
    wb.save(path)


def seal_effect_template(effect_id: int) -> dict[str, object]:
    if effect_id == 1:
        return {
            "icon_key": "UI/SeasonSeals/101",
            "effect_id": 1,
            "bonus_bps": 1000,
            "cap_bps": 0,
            "per_round_limit": 1,
            "battle_limit": 0,
            "active_rounds": 0,
            "health_threshold_bps": 0,
            "description": "主目标首段物理直伤结算后，额外削减伤害10%的剩余护盾，不伤气血。每回合1次。",
        }
    if effect_id == 2:
        return {
            "icon_key": "UI/SeasonSeals/102",
            "effect_id": 2,
            "bonus_bps": 1000,
            "cap_bps": 0,
            "per_round_limit": 1,
            "battle_limit": 0,
            "active_rounds": 0,
            "health_threshold_bps": 0,
            "description": "主目标首段法术直伤结算后，额外削减伤害10%的剩余护盾，不伤气血。每回合1次。",
        }
    if effect_id == 4:
        return {
            "icon_key": "UI/SeasonSeals/104",
            "effect_id": 4,
            "bonus_bps": 1200,
            "cap_bps": 0,
            "per_round_limit": 1,
            "battle_limit": 0,
            "active_rounds": 0,
            "health_threshold_bps": 5000,
            "description": "主动治疗主目标且其气血不高于50%时，首段治疗量额外提升12%。每回合1次。",
        }
    if effect_id == 5:
        return {
            "icon_key": "UI/SeasonSeals/103",
            "effect_id": 5,
            "bonus_bps": 1200,
            "cap_bps": 0,
            "per_round_limit": 1,
            "battle_limit": 0,
            "active_rounds": 0,
            "health_threshold_bps": 0,
            "description": "主动施放护盾命中主目标首段护盾时，额外提升12%护盾容量。每回合1次。",
        }
    if effect_id == 6:
        return {
            "icon_key": "UI/SeasonSeals/103",
            "effect_id": 6,
            "bonus_bps": 1000,
            "cap_bps": 300,
            "per_round_limit": 0,
            "battle_limit": 1,
            "active_rounds": 2,
            "health_threshold_bps": 0,
            "description": "前两回合首次受到敌方扣血直伤减少10%，最多减免入战最大气血的3%。每场1次。",
        }
    if effect_id == 7:
        return {
            "icon_key": "UI/SeasonSeals/104",
            "effect_id": 7,
            "bonus_bps": 400,
            "cap_bps": 0,
            "per_round_limit": 1,
            "battle_limit": 3,
            "active_rounds": 0,
            "health_threshold_bps": 0,
            "description": "实际执行普通防御后，回复入战最大法力的4%，按缺口封顶。每回合1次，每场3次。",
        }
    raise ValueError(f"Forbidden or unknown season seal effect_id={effect_id}")


def patch_season_seal() -> None:
    path = DATA / "SeasonSeal.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    hmap = ensure_columns(
        ws,
        [
            ("resonance_skill_id", "表驱动技印共鸣绑定的当季免费技能ID"),
            ("resonance_kind", "共鸣原语种类（1..8；0无）"),
            ("resonance_param", "共鸣参数（比例百分点/回合/最大法力万分比）"),
            ("resonance_desc", "共鸣文案（客户端直读）"),
        ],
    )

    # (seal_id, season_id, name, effect_id, resonance_skill_id, resonance_kind, resonance_param, resonance_desc)
    seals = [
        # S1 破阵之季
        (101, 1, "破阵锐印", 1, 15001, 1, 30, "共鸣·破阵裂锋：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (102, 1, "破阵玄印", 2, 15002, 2, 1, "共鸣·破阵灵雷：触发削盾后，本招附加的破甲持续回合+1"),
        (103, 1, "固阵守印", 6, 15003, 3, 10, "共鸣·固阵灵障：触发开局减伤后次轮施放固阵灵障，护盾量+10%"),
        (104, 1, "聚息援印", 7, 15004, 4, 50, "共鸣·破阵蓄势：防御回蓝后次轮施放破阵蓄势，法力消耗减半"),
        # S2 玄脉之季
        (201, 2, "玄脉锋印", 1, 15101, 2, 1, "共鸣·玄脉破军：触发削盾后，本招附加的易伤持续回合+1"),
        (202, 2, "玄脉雷印", 2, 15102, 1, 30, "共鸣·玄脉天罚：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (203, 2, "玄脉护印", 5, 15103, 6, 250, "共鸣·玄脉灵幕：触发首盾增容后，自身回复2.5%最大法力"),
        (204, 2, "玄脉息印", 4, 15104, 5, 30, "共鸣·玄脉归元：触发急救增疗后，返还本次30%耗蓝"),
        # S3 离火之季
        (301, 3, "离火刃印", 1, 15202, 1, 30, "共鸣·炎刃引爆：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (302, 3, "离火炎印", 2, 15201, 1, 30, "共鸣·离火焚阵：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (303, 3, "炽幕守印", 6, 15203, 3, 10, "共鸣·炽幕护心：触发开局减伤后次轮施放炽幕护心，护盾量+10%"),
        (304, 3, "薪火疗印", 4, 15204, 5, 30, "共鸣·薪火回生：触发急救增疗后，返还本次30%耗蓝"),
        # S4 青木之季
        (401, 4, "青藤锐印", 1, 15301, 2, 1, "共鸣·青藤缠甲：触发削盾后，本招附加的破甲持续回合+1"),
        (402, 4, "苍木盾印", 5, 15304, 6, 250, "共鸣·苍木灵盖：触发首盾增容后，自身回复2.5%最大法力"),
        (403, 4, "枯荣济印", 4, 15303, 5, 30, "共鸣·枯荣回春：触发急救增疗后，返还本次30%耗蓝"),
        (404, 4, "灵木息印", 7, 15304, 8, 10, "共鸣·苍木灵盖：防御回蓝后次轮施放苍木灵盖，护盾量+10%"),
        # S5 玄水之季
        (501, 5, "寒潮锋印", 1, 15401, 1, 30, "共鸣·寒潮斩浪：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (502, 5, "冥水玄印", 2, 15402, 2, 1, "共鸣·冥水覆涛：触发削盾后，本招附加的降物攻持续回合+1"),
        (503, 5, "静水守印", 6, 15403, 7, 12, "共鸣·碧波净尘：触发开局减伤后次轮施放碧波净尘，治疗量+12%"),
        (504, 5, "潮汐援印", 7, 15404, 4, 50, "共鸣·潮汐聚灵：防御回蓝后次轮施放潮汐聚灵，法力消耗减半"),
        # S6 厚土之季
        (601, 6, "裂地破印", 1, 15501, 1, 30, "共鸣·裂地重击：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (602, 6, "山岳盾印", 5, 15503, 6, 250, "共鸣·山岳承伤：触发首盾增容后，自身回复2.5%最大法力"),
        (603, 6, "磐石守印", 6, 15503, 3, 10, "共鸣·山岳承伤：触发开局减伤后次轮施放山岳承伤，护盾量+10%"),
        (604, 6, "厚土息印", 7, 15503, 8, 10, "共鸣·山岳承伤：防御回蓝后次轮施放山岳承伤，护盾量+10%"),
        # S7 庚金之季
        (701, 7, "庚金锐印", 1, 15601, 2, 1, "共鸣·庚金断锋：触发削盾后，本招附加的破甲持续回合+1"),
        (702, 7, "剑气玄印", 2, 15602, 2, 1, "共鸣·剑雨贯阵：触发削盾后，本招附加的易伤持续回合+1"),
        (703, 7, "金钟护印", 5, 15603, 6, 250, "共鸣·金钟镇岳：触发首盾增容后，自身回复2.5%最大法力"),
        (704, 7, "砺锋援印", 7, 15604, 4, 50, "共鸣·锐金砺刃：防御回蓝后次轮施放锐金砺刃，法力消耗减半"),
        # S8 太阴之季
        (801, 8, "幽影锋印", 1, 15701, 2, 1, "共鸣·幽影袭脉：触发削盾后，本招附加的易伤持续回合+1"),
        (802, 8, "太阴玄印", 2, 15702, 1, 30, "共鸣·太阴蚀魂：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (803, 8, "月华疗印", 4, 15703, 5, 30, "共鸣·月华抚伤：触发急救增疗后，返还本次30%耗蓝"),
        (804, 8, "素月守印", 6, 15703, 7, 12, "共鸣·月华抚伤：触发开局减伤后次轮施放月华抚伤，治疗量+12%"),
        # S9 天雷之季
        (901, 9, "雷枪锐印", 1, 15801, 1, 30, "共鸣·雷枪破盾：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (902, 9, "九霄雷印", 2, 15802, 2, 1, "共鸣·九霄神雷：触发削盾后，本招附加的降法攻持续回合+1"),
        (903, 9, "雷罡盾印", 5, 15803, 6, 250, "共鸣·雷罡护体：触发首盾增容后，自身回复2.5%最大法力"),
        (904, 9, "引雷援印", 7, 15804, 4, 50, "共鸣·引雷蓄能：防御回蓝后次轮施放引雷蓄能，法力消耗减半"),
        # S10 混元之季
        (1001, 10, "混元锐印", 1, 15901, 1, 30, "共鸣·混元一击：削盾将主目标护盾击碎归零时，返还本次30%耗蓝"),
        (1002, 10, "太极玄印", 2, 15902, 2, 1, "共鸣·太极归虚：触发削盾后，本招附加的破甲持续回合+1"),
        (1003, 10, "两仪守印", 6, 15903, 3, 10, "共鸣·两仪灵罩：触发开局减伤后次轮施放两仪灵罩，护盾量+10%"),
        (1004, 10, "混元疗印", 4, 15904, 5, 30, "共鸣·混元回生：触发急救增疗后，返还本次30%耗蓝"),
    ]

    for seal_id, season_id, name, eff_id, res_skill, res_kind, res_param, res_desc in seals:
        tpl = seal_effect_template(eff_id)
        row = {
            "id": seal_id,
            "season_id": season_id,
            "name": name,
            **tpl,
            "resonance_skill_id": res_skill,
            "resonance_kind": res_kind,
            "resonance_param": res_param,
            "resonance_desc": res_desc,
        }
        upsert_by_id(ws, hmap, row)
    wb.save(path)


def buff_dmg(bid: int, lifesteal: float = 0.0) -> tuple[dict[str, object], dict[str, list[object]]]:
    d: dict[str, object] = {"id": bid, "level": 1, "max_layer": 1, "battle_op": 1}
    if lifesteal > 0:
        d["secondary_ratio"] = lifesteal
    return d, {}


def buff_mod(
    bid: int,
    stat: int,
    ratio: float,
    rounds: int,
    tags: list[str],
    effect_target: int = 0,
    tick_on_apply: int = 1,
) -> tuple[dict[str, object], dict[str, list[object]]]:
    d: dict[str, object] = {
        "id": bid,
        "level": 1,
        "max_layer": 1,
        "battle_op": 6,
        "ratio": ratio,
        "duration_rounds": rounds,
        "modifier_stat": stat,
        "stack_key": bid,
        "refresh_policy": 1,
        "tick_on_apply": tick_on_apply,
    }
    if effect_target:
        d["effect_target"] = effect_target
    return d, {"tag": tags}


def buff_shield(bid: int, per_level: float, rounds: int = 2) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {
            "id": bid,
            "level": 1,
            "max_layer": 1,
            "battle_op": 9,
            "per_level": per_level,
            "duration_rounds": rounds,
            "refresh_policy": 2,
            "effect_priority": 20,
            "tick_on_apply": 1,
        },
        {"tag": ["Buff", "Shield"]},
    )


def buff_heal(bid: int, per_level: float) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {"id": bid, "level": 1, "max_layer": 1, "battle_op": 2, "per_level": per_level},
        {},
    )


def buff_mana(bid: int, ratio: float) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {"id": bid, "level": 1, "max_layer": 1, "battle_op": 3, "ratio": ratio, "value_basis": 4},
        {},
    )


def buff_periodic(
    bid: int, state_kind: int, tag_primary: str, per_level: float, ratio: float, rounds: int = 2, chance: float = 0.85
) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {
            "id": bid,
            "level": 1,
            "max_layer": 1,
            "battle_op": 7,
            "per_level": per_level,
            "ratio": ratio,
            "value_basis": 2,
            "duration_rounds": rounds,
            "tick_count": rounds,
            "state_kind": state_kind,
            "application_chance": chance,
            "stack_key": bid,
            "refresh_policy": 1,
            "tick_on_apply": 1,
        },
        {"tag": [tag_primary, "Debuff"]},
    )


def buff_reflect(bid: int, secondary_ratio: float, rounds: int = 2) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {
            "id": bid,
            "level": 1,
            "max_layer": 1,
            "battle_op": 12,
            "trigger_event": 3,
            "damage_filter": 0,
            "duration_rounds": rounds,
            "secondary_ratio": secondary_ratio,
            "effect_priority": 50,
            "tick_on_apply": 1,
        },
        {"tag": ["Buff"]},
    )


def buff_dispel(bid: int, op: int, count: int = 1) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {"id": bid, "level": 1, "max_layer": 1, "battle_op": op, "effect_count": count},
        {},
    )


def buff_consume_periodic(bid: int, req_tag: str, count: int = 1) -> tuple[dict[str, object], dict[str, list[object]]]:
    return (
        {"id": bid, "level": 1, "max_layer": 1, "battle_op": 14, "required_tag": req_tag, "effect_count": count},
        {},
    )


def buff_extend_periodic(
    bid: int, req_tag: str, second_tag: str = "", count: int = 1
) -> tuple[dict[str, object], dict[str, list[object]]]:
    d: dict[str, object] = {
        "id": bid,
        "level": 1,
        "max_layer": 1,
        "battle_op": 15,
        "required_tag": req_tag,
        "effect_count": count,
    }
    if second_tag:
        d["second_required_tag"] = second_tag
    return d, {}


def patch_buffs_and_skills() -> None:
    # --- 1. Buffs (S2..S10, preserving S1 150010..150040 untouched) ---
    buff_specs: list[tuple[dict[str, object], dict[str, list[object]]]] = [
        # S2 玄脉之季
        buff_dmg(151010),
        buff_mod(151011, 16, 0.10, 2, ["Debuff"]),  # 受击易伤 +10%
        buff_dmg(151020),
        buff_mod(151021, 2, -0.12, 2, ["Debuff"]),  # 降敌法攻 -12%
        buff_shield(151030, 17, 2),                 # 2体高额护盾
        buff_heal(151040, 15),                      # 3体群疗+低血急救
        # S3 离火之季
        buff_dmg(152010),
        buff_periodic(152011, 8, "Burn", 2.0, 0.20, 2, 0.85),  # 2回合灼烧
        buff_dmg(152020),
        buff_consume_periodic(152021, "Burn", 1),   # 引爆1跳灼烧
        buff_shield(152030, 14, 2),
        buff_reflect(152031, 0.15, 2),              # 反伤 15%
        buff_heal(152040, 16),
        buff_dispel(152041, 4, 1),                  # 驱散1减益
        # S4 青木之季
        buff_dmg(153010),
        buff_mod(153011, 3, -0.15, 2, ["Break", "Debuff"]),  # 破甲 -15%
        buff_dmg(153020),
        buff_periodic(153021, 7, "Poison", 2.0, 0.22, 2, 0.80),  # 2回合中毒
        buff_heal(153030, 14),
        buff_mod(153031, 6, 0.15, 2, ["Buff"]),     # 受疗 +15%
        buff_shield(153040, 13, 2),
        buff_mod(153041, 3, 0.12, 2, ["Buff"]),     # 防御 +12%
        # S5 玄水之季
        buff_dmg(154010),
        buff_dmg(154020),
        buff_mod(154021, 1, -0.12, 2, ["Debuff"]),  # 降敌物攻 -12%
        buff_dispel(154030, 4, 1),                  # 驱散1减益
        buff_heal(154031, 15),
        buff_mana(154040, 0.18),                    # 回蓝 18%
        buff_mod(154041, 9, -0.15, 2, ["Buff"]),    # 耗蓝 -15%
        # S6 厚土之季
        buff_mod(155010, 26, 0.15, 1, ["Buff"], effect_target=1, tick_on_apply=1),  # 自身当回合穿防 15%
        buff_dmg(155011),
        buff_dmg(155020),
        buff_dispel(155021, 5, 1),                  # 驱散敌方1增益
        buff_shield(155030, 15, 2),
        buff_mod(155031, 16, -0.12, 2, ["Buff"]),   # 受直伤 -12%
        buff_mod(155040, 3, 0.18, 2, ["Buff"]),     # 防御 +18%
        buff_heal(155041, 11),
        # S7 庚金之季
        buff_dmg(156010),
        buff_mod(156011, 3, -0.18, 2, ["Break", "Debuff"]),  # 单体破甲 -18%
        buff_dmg(156020),
        buff_mod(156021, 16, 0.10, 2, ["Debuff"]),  # 易伤 +10%
        buff_shield(156030, 22, 2),                 # 单体厚盾
        buff_mod(156040, 15, 0.20, 2, ["Buff"]),    # 2体双攻 +20%
        # S8 太阴之季
        buff_dmg(157010),
        buff_mod(157011, 16, 0.10, 2, ["Debuff"]),  # 易伤 +10%
        buff_dmg(157020),
        buff_extend_periodic(157021, "Poison", "Burn", 1),  # 延毒/烧 1 跳
        buff_heal(157030, 17),                      # 2体强急救治疗
        buff_shield(157040, 14, 2),
        buff_mod(157041, 10, 0.15, 2, ["Buff"]),    # 受法伤 -15%
        # S9 天雷之季
        buff_dmg(158010),
        buff_mod(158011, 3, -0.15, 2, ["Break", "Debuff"]),  # 破甲 -15%
        buff_dmg(158020),
        buff_mod(158021, 2, -0.12, 2, ["Debuff"]),  # 降敌法攻 -12%
        buff_shield(158030, 14, 2),
        buff_mod(158031, 10, 0.15, 2, ["Buff"]),    # 受法伤 -15%
        buff_mod(158040, 15, 0.15, 2, ["Buff"]),    # 双攻 +15%
        buff_mana(158041, 0.12),                    # 回蓝 12%
        # S10 混元之季
        buff_dmg(159010, lifesteal=0.15),           # 15% 吸血物理直伤
        buff_dmg(159020),
        buff_dispel(159021, 5, 1),                  # 驱散敌方1增益
        buff_mod(159022, 3, -0.12, 2, ["Break", "Debuff"]),  # 降防 -12%
        buff_shield(159030, 14, 2),                 # 3体两仪护盾
        buff_heal(159040, 13),                      # 3体治疗
        buff_mod(159041, 15, 0.10, 2, ["Buff"]),    # 3体双攻 +10%
    ]

    buff_path = DATA / "Buff.xlsx"
    bwb = openpyxl.load_workbook(buff_path)
    bws = bwb.active
    bhmap = header_col_map(bws)
    for row_data, mcols in buff_specs:
        upsert_by_id(bws, bhmap, row_data, mcols)
    bwb.save(buff_path)

    # --- 2. Skills (S2..S10, preserving S1 15001..15004 untouched) ---
    def make_skill(
        sid: int,
        season_id: int,
        proto_code: str,
        name: str,
        desc: str,
        icon_key: str,
        target_team: int,
        max_targets: int,
        mana_pct: float,
        cd: int,
        action_tags: int,
        ban_cat: int,
        effects: list[int],
        damage_expr: str | None = None,
        damage_type: int = 0,
        atk_mult: float | None = None,
        exec_thresh: float | None = None,
        exec_bonus: float | None = None,
        sec_policy: int = 0,
    ) -> tuple[dict[str, object], dict[str, list[object]]]:
        row: dict[str, object] = {
            "id": sid,
            "skill_type": 1,
            "targeting_mode": 1,
            "require_target": 1,
            "name": name,
            "description": desc,
            "icon_key": icon_key,
            "category": "主动",
            "unlock_level": 1,
            "max_rank": 10,
            "target_team": target_team,
            "max_targets": max_targets,
            "mana_percent": mana_pct,
            "cooldown_rounds": cd,
            "battle_model": 1,
            "caster_mask": 1,
            "ability_kind": 1,
            "pet_max_targets": 1,
            "prototype_code": proto_code,
            "action_tags": action_tags,
            "season_id": season_id,
            "scroll_id": 0,
            "rarity_tier": 0,
            "secondary_target_policy": sec_policy,
            "ban_pool_category": ban_cat,
        }
        if damage_expr is not None:
            row["damage"] = damage_expr
            row["damage_type"] = damage_type
            row["attack_multiplier"] = atk_mult if atk_mult is not None else 0.85
        if exec_thresh is not None:
            row["execute_heal_threshold"] = exec_thresh
            row["execute_heal_bonus"] = exec_bonus
        return row, {"effect": effects}

    skill_specs = [
        # S2 玄脉之季
        make_skill(15101, 2, "SS201", "玄脉破军",
                   "【玄脉之季免费】物理攻击敌方最多3个目标并附加2回合易伤（受击伤害+10%）。",
                   "UI/Skills/15001", 1, 3, 0.06, 2, 1, 2, [151010, 151011], "4*level", 1, 0.85),
        make_skill(15102, 2, "SS202", "玄脉天罚",
                   "【玄脉之季免费】法术攻击敌方最多3个目标并降低其2回合法术攻击12%。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [151020, 151021], "4*level", 0, 0.85),
        make_skill(15103, 2, "SS203", "玄脉灵幕",
                   "【玄脉之季免费】为友方最多2个目标施加2回合高额护盾，稳守双核。",
                   "UI/Skills/15003", 2, 2, 0.06, 2, 8, 3, [151030]),
        make_skill(15104, 2, "SS204", "玄脉归元",
                   "【玄脉之季免费】治疗友方最多3个目标，目标气血低于45%时治疗量额外+25%。",
                   "UI/Skills/15004", 2, 3, 0.06, 2, 4, 3, [151040], exec_thresh=0.45, exec_bonus=0.25),
        # S3 离火之季
        make_skill(15201, 3, "SS301", "离火焚阵",
                   "【离火之季免费】法术攻击敌方最多3个目标并有概率附加2回合灼烧。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [152010, 152011], "3.5*level", 0, 0.80),
        make_skill(15202, 3, "SS302", "炎刃引爆",
                   "【离火之季免费】强力物理斩击单个敌方目标，若目标带有灼烧则立即催动1跳灼烧伤害。",
                   "UI/Skills/15001", 1, 1, 0.06, 2, 1, 2, [152020, 152021], "5.5*level", 1, 1.10),
        make_skill(15203, 3, "SS303", "炽幕护心",
                   "【离火之季免费】为友方最多2个目标施加2回合护盾与15%直伤反弹。",
                   "UI/Skills/15003", 2, 2, 0.06, 2, 8, 3, [152030, 152031]),
        make_skill(15204, 3, "SS304", "薪火回生",
                   "【离火之季免费】治疗友方最多2个目标并各自驱散1个减益状态。",
                   "UI/Skills/15004", 2, 2, 0.06, 2, 4, 3, [152040, 152041]),
        # S4 青木之季
        make_skill(15301, 4, "SS401", "青藤缠甲",
                   "【青木之季免费】物理攻击敌方最多3个目标并附加2回合破甲（防御-15%）。",
                   "UI/Skills/15001", 1, 3, 0.06, 2, 1, 2, [153010, 153011], "4*level", 1, 0.85),
        make_skill(15302, 4, "SS402", "碧毒蚀脉",
                   "【青木之季免费】法术攻击敌方最多3个目标并有概率附加2回合中毒。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [153020, 153021], "3.5*level", 0, 0.80),
        make_skill(15303, 4, "SS403", "枯荣回春",
                   "【青木之季免费】治疗友方最多3个目标并提升其2回合受疗效果15%。",
                   "UI/Skills/15004", 2, 3, 0.06, 2, 4, 3, [153030, 153031]),
        make_skill(15304, 4, "SS404", "苍木灵盖",
                   "【青木之季免费】为友方最多3个目标施加2回合护盾并提升防御12%。",
                   "UI/Skills/15003", 2, 3, 0.06, 2, 8, 3, [153040, 153041]),
        # S5 玄水之季
        make_skill(15401, 5, "SS501", "寒潮斩浪",
                   "【玄水之季免费】物理攻击敌方最多2个目标，副目标优先锁定气血比例最低的敌人。",
                   "UI/Skills/15001", 1, 2, 0.06, 2, 1, 2, [154010], "4.5*level", 1, 0.95, sec_policy=4),
        make_skill(15402, 5, "SS502", "冥水覆涛",
                   "【玄水之季免费】法术攻击敌方最多3个目标并降低其2回合攻击力12%。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [154020, 154021], "4*level", 0, 0.85),
        make_skill(15403, 5, "SS503", "碧波净尘",
                   "【玄水之季免费】为友方最多2个目标先驱散1个减益再回复气血。",
                   "UI/Skills/15004", 2, 2, 0.06, 2, 4, 3, [154030, 154031]),
        make_skill(15404, 5, "SS504", "潮汐聚灵",
                   "【玄水之季免费】为友方最多3个目标回复18%法力并降低2回合技能耗蓝15%。",
                   "UI/Skills/15004", 2, 3, 0.05, 2, 8, 3, [154040, 154041]),
        # S6 厚土之季
        make_skill(15501, 6, "SS601", "裂地重击",
                   "【厚土之季免费】自身获得1回合15%无视防御并对敌方最多2个目标造成强力物理伤害。",
                   "UI/Skills/15001", 1, 2, 0.06, 2, 1, 2, [155010, 155011], "4.5*level", 1, 0.95),
        make_skill(15502, 6, "SS602", "岩震崩势",
                   "【厚土之季免费】法术攻击敌方最多2个目标并各自驱散1个增益状态。",
                   "UI/Skills/15002", 1, 2, 0.06, 2, 2, 2, [155020, 155021], "4.5*level", 0, 0.90),
        make_skill(15503, 6, "SS603", "山岳承伤",
                   "【厚土之季免费】为友方最多2个目标施加2回合护盾并降低所受直伤12%。",
                   "UI/Skills/15003", 2, 2, 0.06, 2, 8, 3, [155030, 155031]),
        make_skill(15504, 6, "SS604", "磐石固本",
                   "【厚土之季免费】为友方最多3个目标提升2回合防御18%并回复少量气血。",
                   "UI/Skills/15004", 2, 3, 0.05, 2, 8, 3, [155040, 155041]),
        # S7 庚金之季
        make_skill(15601, 7, "SS701", "庚金断锋",
                   "【庚金之季免费】物理重击单个敌方目标并附加2回合重度破甲（防御-18%）。",
                   "UI/Skills/15001", 1, 1, 0.06, 2, 1, 2, [156010, 156011], "5.5*level", 1, 1.12),
        make_skill(15602, 7, "SS702", "剑雨贯阵",
                   "【庚金之季免费】法术攻击敌方最多3个目标（副目标优先低法防）并附加2回合易伤+10%。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [156020, 156021], "4*level", 0, 0.85, sec_policy=6),
        make_skill(15603, 7, "SS703", "金钟镇岳",
                   "【庚金之季免费】为单个友方目标施加2回合超厚护盾，保核抗集火。",
                   "UI/Skills/15003", 2, 1, 0.06, 2, 8, 3, [156030]),
        make_skill(15604, 7, "SS704", "锐金砺刃",
                   "【庚金之季免费】为友方最多2个目标提升2回合双攻自适应伤害20%。",
                   "UI/Skills/15004", 2, 2, 0.05, 2, 8, 3, [156040]),
        # S8 太阴之季
        make_skill(15701, 8, "SS801", "幽影袭脉",
                   "【太阴之季免费】物理攻击敌方最多2个目标（副目标优先低物防）并附加2回合易伤+10%。",
                   "UI/Skills/15001", 1, 2, 0.06, 2, 1, 2, [157010, 157011], "4.5*level", 1, 0.92, sec_policy=5),
        make_skill(15702, 8, "SS802", "太阴蚀魂",
                   "【太阴之季免费】法术攻击敌方最多3个目标并延长其身上的中毒或灼烧持续1跳。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [157020, 157021], "4*level", 0, 0.85),
        make_skill(15703, 8, "SS803", "月华抚伤",
                   "【太阴之季免费】治疗友方最多2个目标，目标气血低于45%时治疗量额外+30%。",
                   "UI/Skills/15004", 2, 2, 0.06, 2, 4, 3, [157030], exec_thresh=0.45, exec_bonus=0.30),
        make_skill(15704, 8, "SS804", "素月避锋",
                   "【太阴之季免费】为友方最多2个目标施加2回合护盾并降低所受法术伤害15%。",
                   "UI/Skills/15003", 2, 2, 0.06, 2, 8, 3, [157040, 157041]),
        # S9 天雷之季
        make_skill(15801, 9, "SS901", "雷枪破盾",
                   "【天雷之季免费】物理突刺敌方最多2个目标并附加2回合破甲（防御-15%）。",
                   "UI/Skills/15001", 1, 2, 0.06, 2, 1, 2, [158010, 158011], "4.5*level", 1, 0.92),
        make_skill(15802, 9, "SS902", "九霄神雷",
                   "【天雷之季免费】法术雷击敌方最多3个目标（副目标优先高法攻）并降其法攻12%。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [158020, 158021], "4*level", 0, 0.85, sec_policy=2),
        make_skill(15803, 9, "SS903", "雷罡护体",
                   "【天雷之季免费】为友方最多2个目标施加2回合护盾并降低所受法术伤害15%。",
                   "UI/Skills/15003", 2, 2, 0.06, 2, 8, 3, [158030, 158031]),
        make_skill(15804, 9, "SS904", "引雷蓄能",
                   "【天雷之季免费】为友方最多2个目标提升2回合双攻势能15%并回复12%法力。",
                   "UI/Skills/15004", 2, 2, 0.05, 2, 8, 3, [158040, 158041]),
        # S10 混元之季
        make_skill(15901, 10, "SS1001", "混元一击",
                   "【混元之季免费】物理攻击敌方最多2个目标，并将造成直伤的15%转化为自身气血。",
                   "UI/Skills/15001", 1, 2, 0.06, 2, 1, 2, [159010], "4.5*level", 1, 0.92),
        make_skill(15902, 10, "SS1002", "太极归虚",
                   "【混元之季免费】法术攻击敌方最多3个目标，驱散1个增益并附加2回合破甲（防御-12%）。",
                   "UI/Skills/15002", 1, 3, 0.06, 2, 2, 2, [159020, 159021, 159022], "3.8*level", 0, 0.82),
        make_skill(15903, 10, "SS1003", "两仪灵罩",
                   "【混元之季免费】为友方最多3个目标施加2回合均衡护盾。",
                   "UI/Skills/15003", 2, 3, 0.06, 2, 8, 3, [159030]),
        make_skill(15904, 10, "SS1004", "混元回生",
                   "【混元之季免费】治疗友方最多3个目标并提升其2回合双攻势能10%。",
                   "UI/Skills/15004", 2, 3, 0.06, 2, 4, 3, [159040, 159041]),
    ]

    skill_path = DATA / "Skill.xlsx"
    swb = openpyxl.load_workbook(skill_path)
    sws = swb.active
    shmap = header_col_map(sws)
    for row_data, mcols in skill_specs:
        upsert_by_id(sws, shmap, row_data, mcols)
    swb.save(skill_path)


def patch_tips() -> None:
    path = DATA / "tip" / "Tip.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active

    # Check if already present
    existing = set()
    season_unavailable_row = None
    for r in range(1, ws.max_row + 1):
        val = ws.cell(r, 1).value
        if val is not None:
            s = str(val).strip()
            existing.add(s)
            if s == "SeasonUnavailable":
                season_unavailable_row = r
    if season_unavailable_row is None:
        raise RuntimeError("SeasonUnavailable not found in Tip.xlsx")

    to_insert = [
        ("SeasonHonorAlreadyUnlocked", "该赛季永久徽章已经解锁，无需重复追忆", None),
        ("SeasonHonorTargetInvalid", "所选往季徽章追忆目标无效，请刷新页面后重试", None),
    ]
    insert_at = season_unavailable_row + 1
    for name, desc, fault in to_insert:
        if name in existing:
            continue
        ws.insert_rows(insert_at)
        ws.cell(insert_at, 1, name)
        ws.cell(insert_at, 2, desc)
        if fault is not None:
            ws.cell(insert_at, 3, fault)
        insert_at += 1
    wb.save(path)


def patch_message_limiter() -> None:
    path = DATA / "MessageLimiter.xlsx"
    wb = openpyxl.load_workbook(path)
    ws = wb.active
    hmap = header_col_map(ws)
    # 569 = SceneSeasonClientPlayerSetRetroHonorTarget
    upsert_by_id(
        ws,
        hmap,
        {
            "id": 569,
            "max_requests": 5,
            "time_window": 1,
            "tip_message": 1000,
        },
    )
    wb.save(path)


def main() -> None:
    patch_season_rule()
    patch_season_seal()
    patch_buffs_and_skills()
    patch_tips()
    patch_message_limiter()
    print("Season V4 Excel patch complete: SeasonRule(10), SeasonSeal(40), Skill(40), Buff(S2-S10), Tip(+2), MessageLimiter(569).")


if __name__ == "__main__":
    main()
