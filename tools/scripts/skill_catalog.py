#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""61 项正式技能首测目录与只读验证。xlsx 仍是运行配置事实源。

参数来源见 docs/design/skill-planning/first-test-parameters.md；除已确认边界外均待平衡。
本脚本只生成增量计划/文档或验证源表，写表由 skill_catalog_xlsx.mjs 完成。
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path

import openpyxl

ROOT = Path(__file__).resolve().parents[2]
DESIGN = ROOT / "docs/design/skill-planning"
BASES = {"A": 10000, "B": 11000, "P": 12000, "T": 13000, "O": 14000}
SHARED = {"A02", "A06", "A10", "A14", "A16", "A19", "A25", "A26", "A27", "A33", "A34", "P04", "P06"}
PET_ONLY = {"A18", "P14", "P15", "P16", "P17", "P18", "T01", "T05", "T06", "O03"}
RETIRED_SKILL_IDS = {
    int(skill_id) for skill_id in
    (ROOT / "cpp/libs/services/battle/constants/retired_skill_ids.inc").read_text(encoding="ascii").split(",")
}
RETIRED_BUFF_IDS = {100070, 100071, 100170, 100280, 100320, 100370, 120100, 120101, 130020, 130030}
BUFFS: list[dict] = []
SKILLS: list[dict] = []


def definitions() -> dict[str, dict]:
    result = {}
    for prefix, file, effect_col in [
        ("A", "active-character.md", 3),
        ("A", "active-pet.md", 4),
        ("B", "active-pet.md", 4),
        ("P", "passive.md", 4),
        ("T", "pursuit-opening.md", 4),
        ("O", "pursuit-opening.md", 4),
    ]:
        for line in (DESIGN / file).read_text(encoding="utf-8").splitlines():
            if re.match(r"^\| " + prefix + r"\d\d \|", line):
                cells = [cell.strip() for cell in line.strip("|").split("|")]
                if len(cells) <= effect_col:
                    continue
                result[cells[0]] = {"name": cells[1], "effect": cells[effect_col], "source": file}
    assert len(result) == 61, f"Expected 61 formal prototypes, got {len(result)}"
    return result


def skill(code: str, team=1, count=1, mana=.06, cd=1, tags=8, **kw) -> dict:
    entry = CATALOG[code]
    kind = {"A": 1, "B": 1, "P": 2, "T": 3, "O": 4}[code[0]]
    row = dict(id=BASES[code[0]] + int(code[1:]), battle_model=1,
               caster_mask=3 if code in SHARED else 2 if code[0] == "B" or code in PET_ONLY else 1,
               ability_kind=kind, prototype_code=code, name=entry["name"],
               description=entry["effect"] + "；首测参数，待平衡。", category={1: "主动", 2: "被动", 3: "追击", 4: "开场"}[kind],
               unlock_level=1, max_rank=10, target_team=team, max_targets=count,
               pet_max_targets=(2 if code in {"A02", "A06", "A18", "A19", "B12"} else 1),
               mana_percent=mana if kind == 1 else 0, cooldown_rounds=cd if kind == 1 else 0,
               action_tags=tags, skill_type=[1] if kind == 1 else [], targeting_mode=[1], require_target=1,
               effect=[], init_effect=[])
    row.update(kw)
    SKILLS.append(row)
    return row


def buff(row: dict, op: int, *, init=False, child=False, **kw) -> dict:
    index = sum(b["id"] // 10 == row["id"] for b in BUFFS)
    assert index < 10
    result = dict(id=row["id"] * 10 + index, level=1, max_layer=1, battle_op=op, **kw)
    if not result.get("duration_rounds") and (init or op in {6, 19, 20, 21, 22, 23, 24, 25}):
        result["infinite_duration"] = 1
    BUFFS.append(result)
    if not child:
        row["init_effect" if init else "effect"].append(result["id"])
    return result


def damage(row: dict, physical=False, multiplier=1, **kw):
    row.update(damage_type=1 if physical else 0, attack_multiplier=multiplier, damage="4*level", action_tags=1 if physical else 2)
    return buff(row, 1, **kw)


def mod(row: dict, stat: int, ratio: float, rounds=5, tag="Buff", **kw):
    return buff(row, 6, modifier_stat=stat, ratio=ratio, duration_rounds=rounds, tag=[tag], tick_on_apply=1, **kw)


def make_catalog():
    # 主动：效果按列中顺序结算；伤害后再挂障碍。
    damage(skill("A02", count=5, mana=.05, cd=0), True)
    r = skill("A03", count=5, mana=.08, cd=2); damage(r, True, .7); mod(r, 1, -.20, tag="Debuff")
    damage(skill("A06", count=5, mana=.05, cd=0))
    damage(skill("A08", count=5, mana=.15, cd=4), multiplier=1.6)
    # 独立治疗曲线：以旧首测 Lv30/法攻600/rank1 的强度为锚点，不再读取攻击面板。
    r = skill("A10", team=2, count=7, tags=4, cd=0); buff(r, 2, value_basis=0, per_level=20)
    r = skill("A12", team=2, count=7, mana=.10, cd=2, tags=4, execute_heal_threshold=.35, execute_heal_bonus=.6); buff(r, 2, value_basis=0, per_level=17.5)
    mod(skill("A14", team=2, count=7, cd=2), 3, .25)
    buff(skill("A15", team=3, mana=0, cd=2), 3, value_basis=4, ratio=.25)
    mod(skill("A16", team=2, count=7, cd=2), 4, .25, 3)
    r = skill("A18", count=2, mana=.05, cd=1); damage(r, True, .55); mod(r, 3, -.25, 3, tag="Break", stack_key=100181, refresh_policy=1)
    r = skill("A19", count=5, mana=.08, cd=2, tags=18)
    buff(r, 7, value_basis=2, ratio=.25, per_level=2, state_kind=7, tick_count=3, duration_rounds=3, application_chance=.75, tick_on_apply=1, tag=["Poison", "Debuff"], stack_key=100190, refresh_policy=1)
    for code, state, tag, count, chance, rounds, mana in [
        ("A20", 1, "SealActive", 5, .65, 1, .10), ("A21", 2, "SealMagic", 5, .75, 2, .08),
        ("A22", 3, "SealPhysical", 5, .75, 2, .08), ("A23", 4, "Confusion", 5, .65, 1, .10),
        ("A24", 5, "Sleep", 7, .65, 2, .10), ("A38", 6, "Freeze", 7, .55, 1, .12)]:
        buff(skill(code, count=count, mana=mana, cd=3, tags=16), 13, state_kind=state, duration_rounds=rounds, application_chance=chance, tag=[tag, "Control", "Debuff"], tick_on_apply=0, refresh_policy=1)
    mod(skill("A25", team=2, count=7, cd=2), 15, .20)
    buff(skill("A26", team=2, count=7, mana=.10, cd=3, tags=4, allow_dead_target=1), 8,
         value_basis=0, per_level=6, secondary_ratio=.20, duration_rounds=5, tick_count=5, tick_on_apply=1, keep_on_death=1, tag=["Buff", "Revive"], refresh_policy=1)
    mod(skill("A27", team=2, count=7, cd=2), 7, .15, 3)
    buff(skill("A29", count=5, mana=.06, cd=2, tags=16), 15, required_tag="Poison", effect_count=1)
    r = skill("A30", count=5, mana=.08, cd=2); damage(r, multiplier=.5)
    buff(r, 7, value_basis=2, ratio=.20, per_level=2, state_kind=8, tick_count=3, duration_rounds=3, application_chance=.80, tick_on_apply=1, tag=["Burn", "Debuff"], refresh_policy=1)
    r = skill("A31", count=5, mana=.10, cd=2); damage(r, multiplier=.85); buff(r, 14, required_tag="Poison", second_required_tag="Burn", effect_count=1)
    buff(skill("A33", team=2, count=7, mana=.06, cd=1), 4, effect_count=1)
    buff(skill("A34", count=7, mana=.08, cd=2), 5, effect_count=1)
    mod(skill("A35", count=5, mana=.08, cd=2, tags=16), 6, -.35, tag="Debuff", application_chance=.80)
    buff(skill("A36", team=2, count=7, mana=.08, cd=3), 11, ratio=.25, value_basis=3, secondary_ratio=.30, duration_rounds=3, tick_on_apply=1, require_living_caster=1, tag=["Buff"], effect_priority=40)
    damage(skill("A39", count=5, mana=.15, cd=4), True, 1.6)
    buff(skill("A40", team=2, count=7, mana=.08, cd=3), 9, value_basis=0, per_level=16, duration_rounds=2, tick_on_apply=1, tag=["Buff", "Shield"], refresh_policy=2, effect_priority=20)
    # 宝宝专属：B13/B16费用先校验目标，B12吸血只给施法宝宝。
    for code, damage_filter in [("B05", 2), ("B06", 1)]:
        buff(skill(code, team=2, cd=2), 9, value_basis=2, ratio=1.2, per_level=6, hit_limit=2, damage_filter=damage_filter, duration_rounds=3, tick_on_apply=1, refresh_policy=2, tag=["Buff", "Shield"], effect_priority=10)
    for code, damage_filter in [("B07", 1), ("B08", 2)]:
        buff(skill(code, team=2, cd=2), 12, trigger_event=3, damage_filter=damage_filter, secondary_ratio=.25, duration_rounds=3, tick_on_apply=1, tag=["Buff"], effect_priority=50)
    buff(skill("B09", mana=.10, cd=3, tags=16), 17, application_chance=.50, state_kind=9, duration_rounds=2)
    buff(skill("B11", team=2, mana=.06, cd=2), 20, trigger_event=1, ratio=.5, damage_filter=1, target_count=1, pet_target_count=1, hit_limit=1, duration_rounds=2, tick_on_apply=0, tag=["Buff"])
    damage(skill("B12", count=2, mana=.08, cd=2), False, .8, secondary_ratio=.30)
    buff(skill("B13", mana=0, cd=4, health_cost_mode=1), 16, value_basis=5, ratio=1.25)
    buff(skill("B14", team=2, mana=.08, cd=2), 10, ratio=.5, secondary_ratio=1, duration_rounds=3, tick_on_apply=1, tag=["Buff", "Shield"], effect_priority=30)
    buff(skill("B15", team=3, mana=.25, cd=4), 18, value_basis=6, ratio=2, duration_rounds=3, tick_on_apply=1, tag=["Buff"])
    r = skill("B16", team=2, mana=0, cd=0, tags=4, health_cost_mode=2, max_battle_uses=1)
    buff(r, 2, value_basis=3, ratio=.6); buff(r, 4, effect_count=3)
    # 常驻属性只交给现有scene聚合，不再通过init_effect重复叠加。
    for code, stat, amount in [("P01", "physicalAttack", .12), ("P02", "magicAttack", .12), ("P03", "defense", .12), ("P04", "maxHp", .15), ("P06", "speed", .10)]:
        skill(code, team=3, passive_stat=stat, passive_percent=amount)
    for code, stat, ratio in [("P05", 8, .15), ("P07", 9, -.15), ("P14", 10, .15), ("P15", 11, .25), ("P16", 12, .20), ("P17", 13, .30)]:
        buff(skill(code, team=3), 6, init=True, modifier_stat=stat, ratio=ratio)
    buff(skill("P08", team=3), 22, init=True, trigger_event=4, required_tag="Poison", value_basis=3, ratio=.04, per_round_limit=1)
    for code, filt in [("P09", 1), ("P19", 2)]:
        buff(skill(code, team=3), 19, init=True, trigger_event=3, damage_filter=filt, ratio=1, trigger_chance=.20, per_round_limit=1)
    buff(skill("P18", team=3), 23, init=True, trigger_event=3, value_basis=4, ratio=.05, per_round_limit=1)
    # 派生动作不再触发追击或镜像，所有监听均至多每轮一次。
    buff(skill("T01", team=3), 20, init=True, trigger_event=1, ratio=.5, damage_filter=1, target_count=1, pet_target_count=2, trigger_chance=.35, per_round_limit=1)
    r = skill("T05", team=3); listener = buff(r, 21, init=True, trigger_event=1, per_round_limit=1)
    child = buff(r, 6, child=True, modifier_stat=3, ratio=-.20, duration_rounds=3, tick_on_apply=1, tag=["Break", "Debuff"], stack_key=100181, refresh_policy=1)
    listener["target_sub_buff"] = [child["id"]]
    buff(skill("T06", team=3), 20, init=True, trigger_event=2, ratio=.4, damage_filter=2, target_count=2, pet_target_count=2, trigger_chance=.35, per_round_limit=1)
    buff(skill("O01", team=2, count=7), 25, init=True, trigger_event=6, modifier_stat=5, ratio=.05, start_round=3, stack_limit=3, require_living_caster=1, keep_on_death=1, refresh_policy=1, tag=["Buff"])
    buff(skill("O02", team=2, count=7), 6, init=True, modifier_stat=16, ratio=-.20, duration_rounds=2, tick_on_apply=1, require_living_caster=1, keep_on_death=1, refresh_policy=1, tag=["Buff"])
    buff(skill("O03", team=2), 9, init=True, value_basis=2, ratio=1, per_level=5, duration_rounds=2, tick_on_apply=1, refresh_policy=2, tag=["Buff", "Shield"], effect_priority=20)
    SKILLS.sort(key=lambda row: row["id"])
    BUFFS.sort(key=lambda row: row["id"])


CATALOG = definitions()
make_catalog()


def validate_catalog():
    assert {s["prototype_code"] for s in SKILLS} == set(CATALOG)
    assert len(SKILLS) == 61 and len(BUFFS) == 62 and len({b["id"] for b in BUFFS}) == len(BUFFS)
    assert not (RETIRED_SKILL_IDS & {s["id"] for s in SKILLS})
    assert not (RETIRED_BUFF_IDS & {b["id"] for b in BUFFS})
    ids = {b["id"] for b in BUFFS}
    for s in SKILLS:
        assert s.get("passive_stat") or s["effect"] or s["init_effect"], f"Empty skill: {s['id']}"
        assert all(i in ids for i in s["effect"] + s["init_effect"])
        assert not(s.get("passive_stat") and (s["effect"] or s["init_effect"])), "Duplicate static passive consumers"
    for b in BUFFS:
        assert all(i in ids for i in b.get("sub_buff", []) + b.get("target_sub_buff", []))
        assert 0 < b["battle_op"] < 26
        assert all(0 <= b.get(k, 0) <= 1 for k in ["trigger_chance", "application_chance"])


def table_plan(name, rows):
    """按真实schema解析增量列，不再维护第二份类型/slot映射。"""
    sys.path.insert(0, str(ROOT / "tools/data_table_exporter"))
    from core.schema_proto import parse_schema_proto
    schemas = parse_schema_proto(ROOT / "data/schema" / f"{name.lower()}_table.proto")
    schema = schemas[f"{name}Table"]
    book = openpyxl.load_workbook(ROOT / "data" / f"{name}.xlsx")
    sheet = book[name]
    headers = list(next(sheet.values))
    fields = []
    for field in schema.fields:
        width = field.opts.get("cfg_slots", 1)
        if field.type_expr in schemas:
            width *= len(schemas[field.type_expr].fields)
        fields.append({"name": field.name, "width": width, "comment": field.comment.split("\n")[0]})
    # 本轮落表后的复核修正：蚀骨毒同时是持续法术攻击与障碍，只接受已知旧值16。
    migrations = {"10019": {"action_tags": 16}} if name == "Skill" else {}
    return {"name": name, "source_sha256": hashlib.sha256((ROOT / "data" / f"{name}.xlsx").read_bytes()).hexdigest(), "headers": headers, "fields": fields, "rows": rows, "previous_values": migrations}


def verify_xlsx():
    sys.path.insert(0, str(ROOT / "tools/data_table_exporter"))
    from core.schema_proto import build_table_schema
    for name, expected in [("Skill", SKILLS), ("Buff", BUFFS)]:
        build_table_schema(ROOT / "data/schema" / f"{name.lower()}_table.proto", ROOT / "data" / f"{name}.xlsx")
        sheet = openpyxl.load_workbook(ROOT / "data" / f"{name}.xlsx")[name]
        headers = [c.value for c in sheet[1]]
        indexes = {v: i for i, v in enumerate(headers) if v}
        actual = {r[0]: r for r in list(sheet.values)[5:] if r[0] is not None}
        for row in expected:
            assert row["id"] in actual, f"Missing {name} row {row['id']}"
            for key, value in row.items():
                assert key in indexes, f"Missing {name} column {key}"
                want = value if isinstance(value, list) else [value]
                got = list(actual[row["id"]][indexes[key]:indexes[key] + len(want)])
                assert got == want, f"{name} {row['id']} {key}: {got!r} != {want!r}"
        if name == "Skill":
            assert not (RETIRED_SKILL_IDS & set(actual)), f"Retired Skill rows still present: {sorted(RETIRED_SKILL_IDS & set(actual))}"
            public = [r for r in actual.values() if r[indexes["battle_model"]] == 1]
            assert len(public) == 61, f"Expected 61 model rows, got {len(public)}"
        else:
            assert not (RETIRED_BUFF_IDS & set(actual)), f"Retired Buff rows still present: {sorted(RETIRED_BUFF_IDS & set(actual))}"
    pets = openpyxl.load_workbook(ROOT / "data/Pet.xlsx")["Pet"]
    index = [c.value for c in pets[1]].index("skill")
    legal = {s["id"] for s in SKILLS if s["caster_mask"] & 2}
    for row in list(pets.values)[5:]:
        assert all(v in legal for v in row[index:index+4] if v), f"Illegal pet skill: {row[0]}"
    print(f"Verified 61 skills, {len(BUFFS)} effects and legal pet loadouts")


def documentation() -> str:
    text = """# 61项正式技能首测参数

更新：2026-11-21。本页为当前有效 61 项正式原型及 62 条 Buff 的可调首测配置（2026-11-21 撤回人物单体/普攻系 8 项与 10 条对应 Buff，旧编号与 ID 永久保留不复用；`A18`/`T01` 取消人物资格转为宝宝专属；`A15` 保留；`B12` 修正为 `pet_max_targets=2`）。**表已落地，所有新增倍率、概率、耗蓝、冷却与未明确定案的边界仍待平衡**；不是最终平衡结论。运行实现、导表与测试结论统一见[实施清单](implementation.md)，此页不提前宣称测试通过。

## 1. 事实源与覆盖范围

运行配置来自 `data/Skill.xlsx`、`data/Buff.xlsx`，结构来自同名 `data/schema/*_table.proto`。`tools/scripts/skill_catalog.py` 是本轮可复现的初始化目录与校验器，后续策划调数应同步更新本页及校验基线，生成脚本遇到已存在但不同的值会拒绝覆盖。

正式目录为人物/共享主动28、宝宝专属主动12（含 A18）、被动15、追击3（均为宝宝专属）、开场3，共61。策略S/SP候选、暂缓E候选、撤回项（`P11`、`A07`/`A17`/`A28`/`A32`/`A37`/`P10`/`T02`/`T03`）和所有合并旧编号不生成新原型。旧46条Skill和26条Buff原样保留。新增62条Buff承载真实操作；P01/P02/P03/P04/P06沿既有 `passive_stat` 聚合入口，不再重复挂属性Buff。

Skill ID分别为A10000、B11000、P12000、T13000、O14000加编号。Buff ID为该Skill ID乘10加本原型效果序号。这是编表约定；运行时只读取操作、事件、主体资格和参数，禁止按策划编号或这些ID段分支。

## 2. 本轮首测口径

- 人物和宝宝共用原型只计一次，宝宝限制由 `caster_mask`、`pet_max_targets` 表达。人物技能不锁职业。普通行动每轮一次，反击、追击及开场初始化不额外占普通行动。
- 技能全量解锁入口仍走系统既有学习/快照流程；首测解锁等级1、最高10重，不新增任务/抽取/刷材料门槛。这里不设置人物4＋2或宝宝2/3招装配上限。
- 比例字段全部为小数。主动直接伤害基础为 `4*level` 加对应攻击属性乘 `attack_multiplier`，仍经过正式伤害、减免、暴击等管线。治疗/容量为 `flat_value + level*per_level + value_basis*ratio`，施加时快照。主动伤害及数值效果每增加一重提高基础量2%；scene静态属性被动每重提高基础比例5%。Buff属性比例、命中率、触发概率、限次、持续时间、反弹和追击倍率首测直接读取表值，不暗加另一条全局成长曲线。
- 新模型CD数值表示再次施放的回合偏移，CD3在第1轮施放后最早第4轮再用；旧模型继续原有完整回合CD口径。耗蓝按最大法力比例，普通非零费用向上取整且至少1；零耗蓝反击独立处理。
- 持续轮数的 `tick_on_apply=1` 包含施加轮末，0从下一轮末起扣时。A03/A14/A25/A35均5轮；A16首测3轮，排速只影响下次排序；控制保留至少一个后续完整回合窗口。A26当轮末可结算，最多5次。
- A19毒3次、A30烧3次；毒同名跨来源同强刷新、强者覆盖，弱者不替换。蔓毒每份延长1次且刷新保留已延长标记；毒火联动须同时有毒和烧，各提前1次，消费原状态及原施加者强度，不能在轮末重复跳伤。
- A24实际扣血才唤醒；护盾全吸收、未命中不唤醒。A38冻结隔离伤害及新障碍，旧持续状态时钟继续；友方治疗/保护/净化允许，敌方抽蓝/驱散/劝退阻断。
- A26死亡保留回生印，轮末持续伤害后结算印记再判胜负；复活20%最大气血并消费整印。重新施加新印可再次复活，没有每场一次限制。A33每目标净化1项，优先控制；不附加原A11候选少量治疗。A34每目标驱散1项。
- A10/A12/A26独立治疗基础分别为20/17.5/6×人物等级，再乘技能支持等级系数；不读取攻击面板。初始曲线锚定旧首测Lv30/法攻600/rank1强度，尚非平衡结论。
- A40仅独立施放、七目标。容量为16×人物等级×技能支持等级系数（按A10普通治疗基础的80%编表，但运行时不调用治疗产生护盾）；持续2轮含当轮、耗蓝8%、CD3。同名容量按剩余容量择强，不因弱覆盖延时；目标死亡移除，来源死亡保留；可驱散。只吸收直接伤害，不吸收毒、反射、转伤及资源费用。
- 类型护罩→普通容量盾→法力盾→援护预算→实际气血伤害→反射依次处理，优先级10/20/30/40/50。B05/B06最多吸收2次且有容量；B14受益者用自己的法力吸收50%伤害，每吸收1点耗1法力。A36分担25%单体直接伤害，来源每轮承担最多自身最大气血30%，不循环转伤。
- B12首测法术、两目标（`pet_max_targets=2`）、按实际扣血30%吸血给施法宝宝，不吃治疗修正、不从护罩吸收量获益。B13降至1气血后按已付气血125%削蓝，当前1气血不可施放。B15耗自身25%最大法力，按实际已付法力2倍临时增最大气血并回血，持续3轮，到期回归原上限。B16每战1次、牺牲自身，对其他存活友方治疗其最大气血60%并净化最多3项。
- B09劝退不等于死亡，保留气血与单位身份。首测离场后等待2个完整回合：第1轮劝退，最早第4轮由其人物主人提交召回行动；主人消耗本轮正常行动，召回宝宝本轮不追加行动，下一轮恢复正常指令。不能召回别人的宝宝或死亡宝宝。返回截止为施放轮＋配置轮数＋1，`PET_RETREAT`事件值1表示退场、0表示召回。
- P08原毒施加者每轮首次实际毒伤恢复自身最大气血4%，自身灼烧不能借目标身上其他人的毒触发。P09/P19首测20%概率、每轮最多1次、复制同技能且使用自身属性、零耗蓝；逐段实际扣血后立即触发，群攻的每个受击者各自遵守本轮1次上限，反击释放时来袭者须仍存活。不等待主行动结束；派生动作不再互相触发追击或反击。P18每轮首次实际受敌方直接伤害回5%最大法力。
- T01仅限宝宝、首测35%概率、50%物理倍率、最多2敌，每轮最多1次；T05挂20%破防3轮且不附伤；T06法术行动后35%概率追加40%法术倍率、最多2目标、每轮最多1次，不能每个命中目标重复触发。
- O01第3轮开始每轮加5%直接伤害、最多3层；来源死亡暂停且不补增长。O02开局前2轮直接减伤20%，来源死亡暂停但时钟照走。O03优先主人，单目标盾1倍法攻＋5×等级，前2轮有效，来源死亡不撤回已生成容量。复活不重新执行开场。
- Pet22行保持原名字、模型、品质、资质和商业字段；旧法术14映射共享A06，石灵首测A14/P04，金猊A02/T01，其余已有法术宝宝映射A06。这是初始技能配置，不限制后续合法学习，也不复制已合并B01/B02/B03/P12/T04。

## 3. 完整Skill映射和首测资源

表中“自身”指自动监听宿主；O技能按开场友方选人。Buff参数详见下一节。角色/宝宝目标上限不重复计入原型数量。

| 原型 | Skill ID | 名称 | 主体 | 目标（人物／宝宝） | 耗蓝 | CD | effect / init_effect / 静态属性 |
|---|---:|---|---|---|---:|---:|---|
"""
    for s in SKILLS:
        team = {1: "敌", 2: "友", 3: "自身"}[s["target_team"]]
        target = f"{team}{s['max_targets']}" + (f"／{team}{s['pet_max_targets']}" if s["caster_mask"] == 3 else "")
        effects = ", ".join(map(str, s["effect"] or s["init_effect"])) or f"{s.get('passive_stat')} {s.get('passive_percent'):.0%}"
        text += f"| {s['prototype_code']} | {s['id']} | {s['name']} | { {1:'人物',2:'宝宝',3:'人物、宝宝'}[s['caster_mask']] } | {target} | {s['mana_percent']:.0%} | {s['cooldown_rounds']} | {effects} |\n"
    text += "\n## 4. 全部Buff效果与参数\n\n未列出的数值列为空/0；`infinite_duration=1`为常驻监听，非无限次触发许可。概率0按契约表示确定触发；限次列0表示该列不限，派生链边界仍生效。`tag`支持净化和联动，`stack_key`解决同名跨来源覆盖。每条数据可通过校验脚本和正式schema核对。\n\n| Buff ID | 原型 | 操作 | 事件 | 参数 |\n|---:|---|---|---|---|\n"
    ops = ["旧模型", "直接伤害", "治疗", "回蓝", "净化", "驱散", "属性修正", "持续伤害", "回生印", "容量盾", "法力盾", "援护", "反射", "控制", "催动持续伤害", "延长持续伤害", "抽蓝", "劝退", "临时气血上限", "镜像技能", "追击伤害", "触发子Buff", "条件回血", "条件回蓝", "闪避蓄势", "成长光环"]
    event = ["施放/挂载", "普攻后", "技能后", "受击后", "持续伤害后", "闪避后", "轮开始"]
    codes = {s["id"]: s["prototype_code"] for s in SKILLS}
    for b in BUFFS:
        params = {k:v for k,v in b.items() if k not in {"id", "level", "max_layer", "battle_op", "trigger_event"}}
        text += f"| {b['id']} | {codes[b['id']//10]} | {ops[b['battle_op']]} | {event[b.get('trigger_event',0)]} | `{json.dumps(params, ensure_ascii=False, separators=(',', ':'))}` |\n"
    text += """
## 5. 可复现生成与验证

依赖Python（导表器既有openpyxl）和Node的 `@oai/artifact-tool`。先使用现有官方依赖环境，不安装或升级系统工具。`ARTIFACT_NODE_MODULES`设为当前环境提供的Node依赖目录，`PYTHON`可选指定Python解释器。

```powershell
python tools/scripts/skill_catalog.py plan --output build/verification/skill-catalog/catalog-plan.json
node tools/scripts/skill_catalog_xlsx.mjs build/verification/skill-catalog/catalog-plan.json --preview
node tools/scripts/skill_catalog_xlsx.mjs build/verification/skill-catalog/catalog-plan.json
python tools/scripts/skill_catalog.py check
```

预览文件保存在计划目录。脚本按最新源文件哈希校验并拒绝并行覆盖；写格由artifact-tool完成，再将已校验目标单元格移回原OOXML包，保留原样式、列宽、页签及非目标数据。再次执行相同计划应报告 `already current`，不重写文件。手工改表后需重新生成计划；不同已有值会中止，不能用旧计划覆盖新策划调整。

本页可通过 `python tools/scripts/skill_catalog.py documentation --output docs/design/skill-planning/first-test-parameters.md` 从初始化目录重建。生成/核对源表后，由主流程执行 `dev.bat gen`（或项目现有导表入口），禁止手写generated产物。

首测验收不能只有61行存在：必须通过全部引用闭合、合法主体、目标数、事件顺序、时长、消耗、死亡/复活/净化/派生链和同seed重放用例。数值强弱仍要用实战对局验证；本次目录闭合不代表已经平衡。
"""
    return text


def main():
    parser = argparse.ArgumentParser(description="Plan or verify the formal skill catalog")
    parser.add_argument("mode", choices=["plan", "check", "catalog", "documentation"])
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    validate_catalog()
    if args.mode == "check":
        verify_xlsx(); return
    payload = {"skills": SKILLS, "buffs": BUFFS}
    if args.mode == "plan":
        payload["tables"] = [table_plan("Skill", SKILLS), table_plan("Buff", BUFFS)]
        # 保留宝宝其它列；原法术14映射共享A06，物理/防御品种补本身方向的共用原型。
        payload["pet_skills"] = {str(i): ([10014, 12004, 0, 0] if i == 2 else [10002, 13001, 0, 0] if i == 3 else [10006, 0, 0, 0]) for i in range(1, 23)}
    content = documentation() if args.mode == "documentation" else json.dumps(payload, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(content, encoding="utf-8")
        print(f"Wrote catalog plan: {args.output}")
    else:
        sys.stdout.reconfigure(encoding="utf-8"); print(content)


if __name__ == "__main__":
    main()
