#!/usr/bin/env python3
"""读取实际导出表，检查回合制数值设计；这是公式镜像，不能代替 C++ 或联机验收。"""

from __future__ import annotations

import argparse
import ast
import hashlib
import json
import math
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
LEVELS = (1, 10, 20, 40, 60, 80, 85)
STATS = ("max_health", "max_mana", "physical_attack", "magic_attack", "defense", "speed")
PRIMARY = {"constitution": 101, "spirit": 102, "strength": 103, "agility": 104}
PASSIVE = {"maxHp": "max_health", "maxMp": "max_mana", "physicalAttack": "physical_attack",
           "magicAttack": "magic_attack", "defense": "defense", "speed": "speed"}
SOURCES = (
    "cpp/libs/services/battle/constants/turn_battle_constants.h",
    "cpp/libs/services/battle/system/combat_damage_rules.h",
    "cpp/libs/services/battle/system/class_skill_rules.h",
    "cpp/libs/services/battle/system/turn_battle_engine.cpp",
    "cpp/libs/services/scene/player/system/player_level_rules.h",
    "cpp/libs/services/scene/player/system/attribute_allocation_rules.h",
    "cpp/libs/services/scene/player/system/player_attribute.cpp",
    "cpp/libs/services/scene/player/system/player_skill.cpp",
    "cpp/libs/services/scene/player/system/equip_attribute_rules.h",
)


def expression(text: str, names: dict[str, float]) -> float:
    """只接受纯数值与四则运算；不会执行表或源码里的任意 Python。"""
    def visit(node):
        if isinstance(node, ast.Expression):
            return visit(node.body)
        if isinstance(node, ast.Constant) and type(node.value) in (int, float):
            return float(node.value)
        if isinstance(node, ast.Name) and node.id in names:
            return names[node.id]
        if isinstance(node, ast.UnaryOp) and isinstance(node.op, (ast.USub, ast.UAdd)):
            return (-1 if isinstance(node.op, ast.USub) else 1) * visit(node.operand)
        if isinstance(node, ast.BinOp) and isinstance(node.op, (ast.Add, ast.Sub, ast.Mult, ast.Div)):
            left, right = visit(node.left), visit(node.right)
            if isinstance(node.op, ast.Add):
                return left + right
            if isinstance(node.op, ast.Sub):
                return left - right
            if isinstance(node.op, ast.Mult):
                return left * right
            return left / right
        raise ValueError(f"不支持的数值表达式: {text}")
    value = visit(ast.parse(str(text), mode="eval"))
    if not math.isfinite(value):
        raise ValueError(f"非有限数值: {text}")
    return value


class Balance:
    def __init__(self, table_dir: Path):
        self.fingerprints = {}
        self.tables = {}
        for name in ("class", "attributepool", "attributedimension", "attributeallocratio",
                     "attributerule", "attributeautoplan", "equipattribute", "equipattributecap",
                     "equipaffixpool", "equipaffixrule", "equipslot", "item", "skill", "buff", "monster", "dungeon"):
            path = table_dir / f"{name}.json"
            raw = path.read_bytes()
            self.fingerprints[str(path.relative_to(ROOT)) if path.is_relative_to(ROOT) else str(path)] = hashlib.sha256(raw).hexdigest()
            rows = json.loads(raw.decode("utf-8-sig"))["data"]
            if len({r["id"] for r in rows}) != len(rows):
                raise ValueError(f"重复表主键: {name}")
            self.tables[name] = {r["id"]: r for r in rows}
        self.constants = {}
        self.fingerprints["tools/scripts/verify_turn_balance.py"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        for relative in SOURCES:
            raw = (ROOT / relative).read_bytes()
            self.fingerprints[relative] = hashlib.sha256(raw).hexdigest()
            for name, value in re.findall(r"inline constexpr (?:double|uint32_t)\s+(\w+)\s*=\s*([^;]+);", raw.decode("utf-8-sig")):
                try:
                    self.constants[name] = expression(value, self.constants)
                except (ValueError, SyntaxError, ZeroDivisionError):
                    pass
        for key in ("kBasicAttackBaseDamage", "kPvpDamageScale", "kLevelFactorBase",
                    "kLevelFactorPerLevel", "kMaxPassiveReduction", "kLevelFactorMaxLevel", "kMaxLevel"):
            if key not in self.constants:
                raise ValueError(f"源码常量未解析: {key}")
        self.max_level = int(self.constants["kMaxLevel"])
        self.dimensions = {i: r for i, r in self.tables["attributedimension"].items()
                           if self.tables["attributepool"][r["pool_id"]].get("owner_type", 0) == 0}
        self.rule = self.tables["attributerule"][min(self.tables["attributerule"])]

    def total(self, level: int, pool_id: int = 1) -> int:
        pool = self.tables["attributepool"][pool_id]
        return (pool.get("base_points", 0) + pool["points_per_level"] * (level - pool["unlock_level"] + 1)
                if level >= pool["unlock_level"] else 0)

    def standard(self, level: int, class_id: int) -> dict[int, int]:
        plans = [r for r in self.tables["attributeautoplan"].values()
                 if r["pool_id"] == 1 and r.get("class_id", 0) in (0, class_id)]
        plan = min(plans, key=lambda r: (r.get("class_id", 0) != class_id, r["id"]))
        dims, weights = plan["dimension"], plan["weight"]
        weights = [weights[i] if i < len(weights) else 1 for i in range(len(dims))]
        total, weight_sum = self.total(level), sum(weights)
        if not weight_sum:
            raise ValueError(f"自动方案零权重: {plan['id']}")
        allocated = {dim: total * weight // weight_sum for dim, weight in zip(dims, weights)}
        left = total - sum(allocated.values())
        for dim, weight in zip(dims, weights):
            if left and weight:
                allocated[dim] += 1
                left -= 1
        return allocated

    def rank(self, row: dict, level: int) -> int:
        return min(row.get("max_rank", 0), 1 + (level - row["unlock_level"]) // 2) if level >= row["unlock_level"] else 0

    def ratio(self, dimension_id: int, class_id: int) -> dict:
        rows = [r for r in self.tables["attributeallocratio"].values()
                if r["dimension_id"] == dimension_id and r.get("class_id", 0) in (0, class_id)]
        if not rows:
            return {}
        return min(rows, key=lambda r: (r.get("class_id", 0) != class_id, r["id"]))

    def increment(self, dimension_id: int, class_id: int, stat: str, points: int) -> float:
        dim = self.dimensions[dimension_id]
        ratio = self.ratio(dimension_id, class_id)
        if not ratio:
            return dim.get(stat, 0) * points
        cls = self.tables["class"][class_id]
        base = {"max_health": cls["init_health"], "max_mana": cls["init_mana"], "speed": cls["init_speed"]}.get(stat, 0)
        base += sum(d.get(stat, 0) * d.get("base_per_level", 0) * self.max_level for d in self.dimensions.values())
        pool = self.tables["attributepool"][dim["pool_id"]]
        full = self.total(self.max_level, dim["pool_id"])
        if pool.get("dimension_cap", 0):
            full = min(full, pool["dimension_cap"])
        bonus = self.rule.get("alloc_efficiency_bonus", 0.2)
        return base * ratio.get(stat, 0) * points * (1 + bonus * points / full) / (full * (1 + bonus)) if full else 0

    def range_of(self, attr_id: int, level: int) -> tuple[int, int]:
        rows = [r for r in self.tables["equipattributecap"].values() if r["attr_id"] == attr_id and r["level"] <= level]
        if not rows:
            return 0, 0
        row = max(rows, key=lambda r: r["level"])
        return row["min_value"], row["cap"]

    def worn(self, level: int, class_id: int) -> list[dict]:
        items = self.tables["item"].values()
        return [max(rows, key=lambda r: (r["equip_level"], -r["id"])) for kind in (11, 12, 13, 14)
                if (rows := [r for r in items if r.get("equip_kind") == kind and r.get("equip_level", 0) <= level
                             and r.get("equip_class", 0) in (0, class_id)])]

    def effect(self, attr_id: int, value: int) -> dict[str, float]:
        row = self.tables["equipattribute"][attr_id]
        kind, param = row["effect"], row.get("effect_param", 0)
        if kind in (1, 2):
            dims = [self.dimensions[param]] if kind == 1 else self.dimensions.values()
            return {s: sum(d.get(s, 0) for d in dims) * value for s in STATS}
        if kind == 3:
            stat = {1: "max_health", 2: "max_mana", 3: "physical_attack", 4: "magic_attack", 5: "speed", 6: "defense"}[param]
            return {stat: value}
        if kind == 4:
            return {"physical_attack": value, "magic_attack": value}
        return {}  # 本数值镜像不模拟随机必杀、连击、反震与控制。

    def ordinary_bonus(self, level: int, class_id: int) -> tuple[dict, list]:
        totals, selected = {s: 0 for s in STATS}, []
        primary_attr = {1: 3, 2: 5, 3: 5, 4: 6}[class_id]
        for item in self.worn(level, class_id):
            lines = [(r["base_attr_id"], r["base_attr_value"]) for r in item.get("base_attr", [])]
            pool = {r["attr_id"] for r in self.tables["equipaffixpool"].values()
                    if r["pool_id"] == item.get("affix_pool") and r.get("weight", 0) > 0}
            preferred = [1, primary_attr, 4, 9] if item["equip_kind"] == 11 else [primary_attr, 9, 4, 8]
            affixes = []
            rule = self.tables["equipaffixrule"][item["affix_rule"]]
            for attr_id in dict.fromkeys(preferred):
                low, cap = self.range_of(attr_id, item["equip_level"])
                if attr_id in pool and cap and len(affixes) < min(2, rule["blue_max"]):
                    affixes.append((attr_id, max(low, math.ceil(cap * 0.5))))
            for attr_id, value in lines + affixes:
                for stat, amount in self.effect(attr_id, value).items():
                    totals[stat] += amount
            selected.append({"item": item["id"], "blue": affixes})
        return totals, selected

    def actor(self, level: int, class_id: int, allocated: dict[int, int], gear: bool = False) -> dict:
        cls = self.tables["class"][class_id]
        stats = {s: 0.0 for s in STATS}
        stats.update(max_health=cls["init_health"], max_mana=cls["init_mana"], speed=cls["init_speed"])
        for dim in self.dimensions.values():
            for stat in STATS:
                stats[stat] += dim.get(stat, 0) * dim.get("base_per_level", 0) * level
                stats[stat] += self.increment(dim["id"], class_id, stat, allocated.get(dim["id"], 0))
        bonus, _ = self.ordinary_bonus(level, class_id) if gear else ({}, [])
        for stat in STATS:
            stats[stat] += bonus.get(stat, 0)
        outgoing_heal = 0.0
        passive_bonus = {s: 0.0 for s in STATS}
        for row in self.tables["skill"].values():
            rank = self.rank(row, level) if row.get("class_id") == class_id else 0
            if rank:
                amount = row.get("passive_percent", 0) * (1 + 0.05 * (rank - 1))
                if row.get("passive_stat") in PASSIVE:
                    passive_bonus[PASSIVE[row["passive_stat"]]] += amount
                if row.get("passive_stat") == "outgoingHeal":
                    outgoing_heal += amount
        stats = {s: math.floor(v * (1 + passive_bonus[s])) for s, v in stats.items()}
        stats.update(level=level, class_id=class_id, armor=cls["init_armor"], strength=cls["init_strength"],
                     resistance=cls["init_resistance"], outgoing_heal=outgoing_heal)
        return stats

    def received(self, target: dict) -> float:
        level = min(max(target["level"], 1), self.constants["kLevelFactorMaxLevel"])
        factor = self.constants["kLevelFactorBase"] + self.constants["kLevelFactorPerLevel"] * level
        return max(1 - self.constants["kMaxPassiveReduction"], factor / (target["armor"] + target["defense"] + factor)
                   * (1 - min(max(target["resistance"] / 100, 0), 1)))

    def damage(self, attacker: dict, target: dict, skill: dict | None = None, pvp: bool = False,
               rank: int | None = None) -> int:
        base = expression(skill["damage"], {"level": attacker["level"]}) if skill else self.constants["kBasicAttackBaseDamage"]
        if base <= 0:
            return 0
        attack = attacker["magic_attack"] if skill and skill.get("damage_type", 0) == 0 else attacker["physical_attack"]
        multiplier = (skill.get("attack_multiplier", 0) or 1.0) if skill else 1.0
        damage = (base * (1 + attacker["strength"] * 0.1) + attack * multiplier) * self.received(target)
        if pvp:
            damage *= self.constants["kPvpDamageScale"]
        if skill:
            damage *= 1 + 0.02 * ((self.rank(skill, attacker["level"]) if rank is None else rank) - 1)
        return math.ceil(damage)

    def healing(self, actor: dict, skill_id: int = 1301, rank: int | None = None,
                target_health_ratio: float = 1.0) -> int:
        skill = self.tables["skill"][skill_id]
        amount = ((actor["magic_attack"] * skill["heal_multiplier"] + actor["level"] * skill["heal_per_level"])
                  * (1 + 0.02 * ((self.rank(skill, actor["level"]) if rank is None else rank) - 1))
                  * (1 + actor["outgoing_heal"]))
        if skill.get("execute_heal_threshold", 0) > 0 and target_health_ratio < skill["execute_heal_threshold"]:
            amount *= 1 + skill.get("execute_heal_bonus", 0)
        return math.floor(amount)  # 理论治疗量；引擎还会按目标缺血量封顶。

    def pve(self, level: int, config_id: int, count: int, gear: bool, max_rounds: int,
            class_id: int = 1) -> dict:
        players = [self.actor(level, class_id, self.standard(level, class_id), gear) for _ in range(count)]
        ids = (self.tables["dungeon"][config_id]["monster"] * count)[:int(self.constants["kMaxScaledPveMonsterCount"])]
        monsters = []
        for monster_id in ids:
            row = self.tables["monster"][monster_id]
            monsters.append(dict(level=level, class_id=0, armor=row["armor"], resistance=row["resistance"],
                                 strength=row["strength"], speed=row["speed"], max_health=row["health"],
                                 physical_attack=0, magic_attack=0, defense=0))
        actors = players + monsters
        health = [a["max_health"] for a in actors]
        initial_hp = sum(health[:count])
        order = sorted(range(len(actors)), key=lambda i: (-actors[i]["speed"], i))
        for round_index in range(1, max_rounds + 1):
            for i in order:
                if health[i] <= 0:
                    continue
                enemies = range(count, len(actors)) if i < count else range(count)
                target = next((j for j in enemies if health[j] > 0), None)
                if target is not None:
                    health[target] = max(0, health[target] - self.damage(actors[i], actors[target]))
            if not any(health[:count]) or not any(health[count:]):
                break
        win = any(health[:count]) and not any(health[count:])
        return {"level": level, "config": config_id, "players": count, "ordinary_gear": gear, "monsters": len(monsters),
                "outcome": "win" if win else "loss" if not any(health[:count]) else "ongoing", "rounds": round_index,
                "player_hp_remaining_ratio": round(sum(health[:count]) / initial_hp, 4), "alive_players": sum(h > 0 for h in health[:count])}


def verify(args) -> dict:
    balance = Balance(args.table_dir)
    failures, checks = [], 0

    def check(condition, name, **details):
        nonlocal checks
        checks += 1
        if not condition:
            failures.append(dict(check=name, **details))

    # 逐条检查本脚本消费的外键，坏表必须在数值计算前暴露。
    for row in balance.tables["dungeon"].values():
        check(all(i in balance.tables["monster"] for i in row["monster"]), "dungeon_monster_reference", dungeon=row["id"])
    for row in balance.tables["equipaffixpool"].values():
        check(row["attr_id"] in balance.tables["equipattribute"], "equipment_attribute_reference", row=row["id"])
    for row in balance.tables["equipattributecap"].values():
        check(row["attr_id"] in balance.tables["equipattribute"] and 1 <= row["min_value"] <= row["cap"], "equipment_cap", row=row["id"])
    for row in balance.tables["attributeautoplan"].values():
        check(all(i in balance.tables["attributedimension"] and balance.tables["attributedimension"][i]["pool_id"] == row["pool_id"] for i in row["dimension"]), "allocation_plan_reference", row=row["id"])
    for cls in balance.tables["class"].values():
        check(all(i in balance.tables["skill"] for i in cls.get("skill", [])), "class_skill_reference", class_id=cls["id"])
    for row in balance.tables["attributeallocratio"].values():
        check(row["dimension_id"] in balance.dimensions and row.get("class_id", 0) in (0, *balance.tables["class"]),
              "allocation_ratio_reference", row=row["id"])
    for row in balance.tables["skill"].values():
        check(row.get("class_id", 0) in (0, *balance.tables["class"]) and
              all(i in balance.tables["buff"] for i in row.get("effect", []) if i), "skill_reference", skill=row["id"])
    pool_ids = {r["pool_id"] for r in balance.tables["equipaffixpool"].values()}
    for row in balance.tables["item"].values():
        if not row.get("equip_kind"):
            continue
        check(all(r["base_attr_id"] in balance.tables["equipattribute"] for r in row.get("base_attr", [])) and
              (not row.get("affix_pool") or row["affix_pool"] in pool_ids) and
              (not row.get("affix_rule") or row["affix_rule"] in balance.tables["equipaffixrule"]),
              "equipment_item_reference", item=row["id"])
    if failures:
        return {"ok": False, "checks": checks, "failures": failures, "input_sha256": balance.fingerprints}

    # 十级一档的新装备是本次数值方案的明确契约。缺档不会阻止后面的旧表诊断，
    # 因此这里单独累计失败；不得让缺档的旧输入提前退出而失去数值对比。
    equipment_tiers = []
    for kind in (11, 12, 13, 14):
        tier_ids = {}
        for level in (1, 10, 20, 30, 40, 50, 60, 70, 80):
            rows = [r for r in balance.tables["item"].values() if r.get("equip_kind") == kind
                    and r.get("equip_level") == level and r.get("equip_class", 0) == 0]
            check(len(rows) == 1, "equipment_tier_unique_and_complete", kind=kind, level=level, count=len(rows))
            if len(rows) == 1:
                row = rows[0]
                attrs = [r["base_attr_id"] for r in row.get("base_attr", [])]
                expected = {11: [1], 12: [8], 13: [8], 14: [8, 11]}[kind]
                check(sorted(attrs) == expected and len(attrs) == len(set(attrs)),
                      "equipment_tier_base_attributes", kind=kind, level=level, item=row["id"], attributes=attrs)
                tier_ids[level] = row["id"]
        equipment_tiers.append({"kind": kind, "level_to_item": tier_ids})

    minimum_point_gain = math.inf
    for class_id in range(1, 5):
        for dimension_id, dim in balance.dimensions.items():
            ratio = balance.ratio(dimension_id, class_id)
            for stat in STATS:
                if not (ratio.get(stat, 0) if ratio else dim.get(stat, 0)):
                    continue
                previous = 0
                for points in range(1, balance.total(balance.max_level) + 1):
                    current = math.floor(balance.increment(dimension_id, class_id, stat, points))
                    minimum_point_gain = min(minimum_point_gain, current - previous)
                    check(current - previous >= 1, "each_allocated_point_visible", class_id=class_id, dimension=dimension_id, stat=stat, points=points)
                    previous = current

    growth, extremes, gear_samples = [], [], []
    pvp_ranges = {"basic": [math.inf, 0], "primary": [math.inf, 0]}
    tradeoffs = {"attack_gain": math.inf, "health_gain": math.inf, "speed_gain": math.inf}
    all_states = 0
    for level in LEVELS:
        for class_id in range(1, 5):
            standard = balance.standard(level, class_id)
            check(sum(standard.values()) == balance.total(level), "standard_spends_all_points", level=level, class_id=class_id)
            variants = {"standard": standard, **{name: {dim: balance.total(level)} for name, dim in PRIMARY.items()}}
            states = {(name, gear): balance.actor(level, class_id, points, gear)
                      for name, points in variants.items() for gear in (False, True)}
            all_states += len(states)
            # 所有加点、职业、等级和装备档都计算命中次数。仅对标准方案施加节奏预算，
            # 全体/全敏是有意牺牲输出的极端方案，不能要求它们也落在输出职业的预算内。
            for state in states.values():
                primary = balance.tables["skill"][1000 + class_id * 100 + 1]
                for kind, skill in (("basic", None), ("primary", primary)):
                    damage = balance.damage(state, state, skill, pvp=True)
                    if damage:
                        hits = math.ceil(state["max_health"] / damage)
                        pvp_ranges[kind][0] = min(pvp_ranges[kind][0], hits)
                        pvp_ranges[kind][1] = max(pvp_ranges[kind][1], hits)
            for gear in (False, True):
                attack_stat = "magic_attack" if class_id in (2, 3) else "physical_attack"
                offensive = states[("spirit" if class_id in (2, 3) else "strength", gear)]
                constitution, agility = states[("constitution", gear)], states[("agility", gear)]
                gains = {"attack_gain": offensive[attack_stat] / constitution[attack_stat] - 1,
                         "health_gain": constitution["max_health"] / offensive["max_health"] - 1,
                         "speed_gain": agility["speed"] / offensive["speed"] - 1}
                for key, value in gains.items():
                    tradeoffs[key] = min(tradeoffs[key], value)
                    check(value >= args.min_tradeoff_gain, "visible_build_tradeoff", metric=key, gain=round(value, 4), level=level, class_id=class_id, gear=gear)
            bare, gear = states[("standard", False)], states[("standard", True)]
            primary = balance.tables["skill"][1000 + class_id * 100 + 1]
            normal_damage = balance.damage(gear, gear, pvp=True)
            skill_damage = balance.damage(gear, gear, primary, pvp=True)
            normal_hits = math.ceil(gear["max_health"] / normal_damage) if normal_damage else None
            skill_hits = math.ceil(gear["max_health"] / skill_damage) if skill_damage else None
            check(normal_hits is not None and args.min_pvp_hits <= normal_hits <= args.max_pvp_hits,
                  "pvp_basic_hit_budget", level=level, class_id=class_id, hits=normal_hits)
            # 丹心首招是治疗，不将 0 伤害技能伪装成无限击杀回合。
            if skill_damage:
                check(args.min_pvp_hits <= skill_hits <= args.max_pvp_hits, "pvp_primary_hit_budget", level=level, class_id=class_id, hits=skill_hits)
            bare_basic = balance.damage(bare, bare, pvp=True)
            bare_primary = balance.damage(bare, bare, primary, pvp=True)
            bare_hits = math.ceil(bare["max_health"] / bare_basic) if bare_basic else None
            bare_skill_hits = math.ceil(bare["max_health"] / bare_primary) if bare_primary else None
            # 1级无装备普攻是教学基线：初始生命占主体，治疗/敏捷职业尚未形成输出，
            # 独立允许45次；普通装备及10级以上仍守统一战斗节奏预算。
            bare_max_hits = args.max_intro_bare_pvp_hits if level == 1 else args.max_pvp_hits
            check(bare_hits is not None and args.min_pvp_hits <= bare_hits <= bare_max_hits,
                  "pvp_bare_basic_hit_budget", level=level, class_id=class_id, hits=bare_hits,
                  baseline="intro_without_equipment" if level == 1 else "normal", maximum=bare_max_hits)
            if bare_primary:
                check(args.min_pvp_hits <= bare_skill_hits <= bare_max_hits,
                      "pvp_bare_primary_hit_budget", level=level, class_id=class_id, hits=bare_skill_hits)
            growth.append({"level": level, "class": class_id, "bare": [bare[s] for s in STATS],
                           "ordinary": [gear[s] for s in STATS], "pvp_basic_hits": normal_hits,
                           "pvp_primary_hits": skill_hits, "pvp_bare_basic_hits": bare_hits,
                           "pvp_bare_primary_hits": bare_skill_hits})
            if level == balance.max_level:
                extremes.append({"class": class_id, **{name: [states[(name, False)][s] for s in STATS] for name in PRIMARY}})
                _, selected = balance.ordinary_bonus(level, class_id)
                gear_samples.append({"class": class_id, "items": selected})

    heal_ratios, emergency_healing = [], []
    rank_growth, rank_comparisons = [], 0
    for level in LEVELS:
        for gear in (False, True):
            healer = balance.actor(level, 3, balance.standard(level, 3), gear)
            heal = balance.healing(healer)
            typical_hp = sum(balance.actor(level, cls, balance.standard(level, cls), gear)["max_health"]
                             for cls in range(1, 5)) / 4
            row = {"level": level, "ordinary_gear": gear, "heal": heal, "typical_hp": typical_hp,
                   "heal_to_typical_hp": round(heal / typical_hp, 4),
                   "heal_to_healer_hp": round(heal / healer["max_health"], 4)}
            for class_id, name in ((1, "physical"), (2, "magic")):
                attacker = balance.actor(level, class_id, balance.standard(level, class_id), gear)
                damage = balance.damage(attacker, healer, balance.tables["skill"][1000 + class_id * 100 + 1], pvp=True)
                row[name + "_damage"] = damage
                row["heal_to_" + name] = round(heal / damage, 3) if damage else None
                check(damage > 0 and heal / damage <= args.max_heal_damage_ratio, "heal_damage_budget", level=level, gear=gear, damage_type=name, ratio=row["heal_to_" + name])
            heal_ratios.append(row)
            emergency = balance.tables["skill"][1305]
            if balance.rank(emergency, level):
                threshold = emergency["execute_heal_threshold"]
                normal = balance.healing(healer, 1305, target_health_ratio=threshold)
                below = balance.healing(healer, 1305, target_health_ratio=threshold - 0.0001)
                above = balance.healing(healer, 1305, target_health_ratio=threshold + 0.0001)
                expected = normal * (1 + emergency["execute_heal_bonus"])
                check(normal == above, "emergency_heal_threshold_is_strict", level=level, gear=gear)
                check(below > normal and abs(below - expected) < 2,
                      "emergency_heal_bonus_applies_once", level=level, gear=gear, normal=normal, below=below)
                check(below / typical_hp <= 0.9, "emergency_heal_burst_budget", level=level, gear=gear,
                      ratio=round(below / typical_hp, 4))
                emergency_healing.append({"level": level, "ordinary_gear": gear, "normal": normal,
                                          "below_threshold_nominal": below,
                                          "nominal_to_typical_hp": round(below / typical_hp, 4),
                                          "cooldown_rounds": emergency["cooldown_rounds"]})
            for class_id in range(1, 5):
                actor = balance.actor(level, class_id, balance.standard(level, class_id), gear)
                for skill in balance.tables["skill"].values():
                    rank = balance.rank(skill, level) if skill.get("class_id") == class_id else 0
                    if not rank or skill.get("passive_stat"):
                        continue
                    effects = {}
                    if expression(skill["damage"], {"level": level}) > 0:
                        effects["damage"] = (balance.damage(actor, actor, skill, pvp=True, rank=1),
                                             balance.damage(actor, actor, skill, pvp=True, rank=rank))
                    if skill.get("heal_multiplier", 0) > 0 or skill.get("heal_per_level", 0) > 0:
                        effects["heal"] = (balance.healing(actor, skill["id"], rank=1),
                                           balance.healing(actor, skill["id"], rank=rank))
                    for effect, (first, maximum) in effects.items():
                        rank_comparisons += 1
                        check(maximum > first if rank > 1 else maximum == first,
                              "active_rank_increases_effect", level=level, class_id=class_id,
                              skill=skill["id"], effect=effect, gear=gear, rank=rank,
                              first=first, maximum=maximum)
                        if level == balance.max_level and gear:
                            rank_growth.append({"skill": skill["id"], "effect": effect, "rank": rank,
                                                "rank1": first, "rank_max": maximum})

    # 上界检查：每件普通装备为某一属性选择收益最高的两条互异蓝词条，而非只查展示样例。
    affix_ratios = {}
    for stat, dim_id in (("physical_attack", 103), ("magic_attack", 102), ("max_health", 101), ("defense", 101), ("speed", 104)):
        worst = 0.0
        for class_id in range(1, 5):
            affix_gain = 0
            for item in balance.worn(balance.max_level, class_id):
                values = []
                for row in balance.tables["equipaffixpool"].values():
                    low, cap = balance.range_of(row["attr_id"], item["equip_level"])
                    if row["pool_id"] == item["affix_pool"] and row.get("weight", 0) and cap:
                        values.append(balance.effect(row["attr_id"], max(low, math.ceil(cap * 0.5))).get(stat, 0))
                slots = min(2, balance.tables["equipaffixrule"][item["affix_rule"]]["blue_max"])
                affix_gain += sum(sorted(values, reverse=True)[:slots])
            invested = balance.increment(dim_id, class_id, stat, balance.total(balance.max_level))
            ratio = affix_gain / invested if invested else math.inf
            worst = max(worst, ratio)
            check(ratio <= args.max_affix_allocation_ratio, "ordinary_affixes_vs_full_allocation", stat=stat, class_id=class_id, ratio=round(ratio, 4))
        affix_ratios[stat] = round(worst, 4)

    pve = [balance.pve(level, config_id, count, gear, args.max_pve_rounds)
           for level, config_id, count in ((1, 1, 1), (10, 2, 1), (30, 3, 3)) for gear in (False, True)]
    for row in pve:
        # 30级首领的裸装纯普攻用于展示装备差距；要求普通装备能通关，
        # 不把“无装备也必须过首领”变成削弱首领或抹平装备收益的隐含设计。
        row["acceptance"] = "equipment_gap_reference" if row["config"] == 3 and not row["ordinary_gear"] else "must_win"
        if row["acceptance"] == "must_win":
            check(row["outcome"] == "win", "pve_basic_attack_completion", **row)
        check(row["rounds"] >= 2, "pve_not_one_round", **row)

    # 教学和早期普通关必须支持四种职业用普通装备通过；不只用物理输出职业代表所有玩家。
    intro_classes = []
    for class_id in range(1, 5):
        for level, config_id in ((1, 1), (10, 2)):
            row = balance.pve(level, config_id, 1, True, args.max_pve_rounds, class_id)
            row["class"] = class_id
            check(row["outcome"] == "win", "intro_all_classes_complete", **row)
            check(2 <= row["rounds"] <= 12, "intro_all_classes_round_budget", **row)
            intro_classes.append(row)

    return {"ok": not failures, "checks": checks, "failures": failures,
            "scope": "实际导出 JSON 的独立数值设计验收；非 C++、真实进程链、随机性或联机验收",
            "assumptions": {"skill_training": "所有已解锁职业技能练至角色等级许可最高重",
                            "ordinary_gear": "各部位可穿最高等级档基础；最多两条互异蓝词条取50%上限且不低于下限；无粉黄强化宝石",
                            "pvp_hits": "同职业同标准加点，对等满血靶；仅计算无暴击普攻或首主动重复命中次数，非完整对战回合；1级裸装为独立教学基线",
                            "pve": "实际副本整组按玩家数重复；三关主基准为破军，教学/早期关另查四职业标准加点；速度序、集中首个存活敌方、仅普攻，无暴击宠物技能道具；30级首领裸装仅作装备差距参照",
                            "rank_comparison": "固定相同角色属性及最高重被动，只比较主动技能1重与当前等级许可最高重；非修习全过程模拟",
                            "typical_hp": "同等级同装备档四职业标准方案气血上限的算术平均；治疗报告为不计溢出的理论单奶量",
                            "excluded": "随机连击/反击/反震、状态与冷却轮转、部分治疗溢出、经验经济、客户端"},
            "stat_order": STATS, "states_checked": all_states, "minimum_point_gain": minimum_point_gain,
            "minimum_build_tradeoffs": {k: round(v, 4) for k, v in tradeoffs.items()},
            "ordinary_affix_to_full_allocation_max": affix_ratios, "growth": growth,
            "level_cap_extremes_bare": extremes, "ordinary_gear_samples_at_cap": gear_samples,
            "healing_vs_pvp_damage": heal_ratios, "emergency_healing": emergency_healing,
            "pve_basic_attack": pve, "intro_pve_all_classes": intro_classes,
            "rank_comparisons_checked": rank_comparisons, "rank_growth_at_cap_ordinary": rank_growth,
            "equipment_tiers": equipment_tiers,
            "budgets": {k: v for k, v in vars(args).items() if k not in ("output", "table_dir")},
            "pvp_hit_ranges_all_builds": pvp_ranges,
            "constants": {k: balance.constants[k] for k in ("kBasicAttackBaseDamage", "kPvpDamageScale",
                           "kLevelFactorBase", "kLevelFactorPerLevel", "kMaxPassiveReduction", "kMaxLevel")},
            "input_sha256": balance.fingerprints}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--table-dir", type=Path, default=ROOT / "generated/tables")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--min-tradeoff-gain", type=float, default=0.10)
    parser.add_argument("--max-affix-allocation-ratio", type=float, default=0.75)
    parser.add_argument("--max-heal-damage-ratio", type=float, default=2.5)
    parser.add_argument("--min-pvp-hits", type=int, default=2)
    parser.add_argument("--max-pvp-hits", type=int, default=30)
    parser.add_argument("--max-intro-bare-pvp-hits", type=int, default=45)
    parser.add_argument("--max-pve-rounds", type=int, default=40)
    args = parser.parse_args()
    try:
        if any(not math.isfinite(v) or v < 0 for v in (args.min_tradeoff_gain,
                args.max_affix_allocation_ratio, args.max_heal_damage_ratio)) or not (
                1 <= args.min_pvp_hits <= args.max_pvp_hits and
                args.max_intro_bare_pvp_hits >= args.min_pvp_hits and args.max_pve_rounds >= 1):
            raise ValueError("预算参数必须为有限非负数，命中次数及回合数必须为正整数")
        report = verify(args)
    except (ValueError, KeyError, TypeError, OSError, ZeroDivisionError, SyntaxError) as exc:
        report = {"ok": False, "error": str(exc), "scope": "数值设计镜像输入或计算失败"}
    encoded = json.dumps(report, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded + "\n", encoding="utf-8")
    print(encoded)
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
