#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""角色形象切换（10 元角色形象卡）与一小时免费试用（bit 存储）的 xlsx 幂等补丁脚本。

改动范围(全部幂等,重复执行无副作用,发现现状与预期冲突则中止且绝不覆盖已有数据):
  1. new-tables:       新建 CharacterAppearance.xlsx(8 个现役形象,保留原永久 id 2..5,7..10)与 AppearanceRule.xlsx(单行规则)
  2. card-item-row:    Item.xlsx 追加 3402 角色形象卡
  3. tip-codes:        Tip.xlsx 文末新开 //appearance_error base=33000 width=1000(7 个提示码)

用法(仓库根目录执行):
    python tools/scripts/appearance_switch_xlsx_patch.py all
    python tools/scripts/appearance_switch_xlsx_patch.py all --dry-run
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
        ITEM_SHEET,
        ITEM_XLSX,
        TIP_STATE,
        TIP_XLSX,
        Patch,
        apply_patches_and_verify,
        bind,
        fail,
        first_difference,
        header_cells,
        item_row_cells,
        only_sheet,
        rel,
        schema_layout,
        sheet_matrix,
        tip_parse,
    )
except ImportError as exc:
    sys.exit(f"cannot load dependencies ({exc}): run  pip install pyyaml openpyxl  first")


SERIES_SHEET = "AppearanceSeries"
SERIES_ROWS = [
    dict(
        id=1,
        series_key="lantern_courtyard",
        name="留灯院旧友",
        sort_order=1,
        enabled=1,
        desc="归元城面铺与月汐湾渡口间，总有一盏留到深夜的灯。",
        completion_badge_name="留灯同袍",
        epilogue_title="灯下三碗面",
        epilogue_body=(
            "雨停后的留灯院，檐水顺着竹槽滴在青石板上。阿禾把三只热腾腾的粗瓷碗端上木桌，特意给停云多加了半勺辣油，又把葱花碟子挪到听雨够不着的那一侧。\n\n"
            "停云把擦干净的红缨枪靠在柱旁，笑道：“今儿摆渡收工早，再不用拿面汤抵工钱了。”听雨从竹筒里抽出一张折得方正的新船票，轻轻压在碗边：“这回没让给别人，等风小了，一起去八景岛看鸟。”\n\n"
            "阿禾替两人把院灯拨亮了些，难得没急着收拾碗筷，也坐下来端起自己的那一只：“好。出门前记得叫我锁院门。”"
        ),
    ),
    dict(
        id=2,
        series_key="elemental_blades",
        name="水火冰锋",
        sort_order=2,
        enabled=1,
        desc="霜刃、赤符、沧澜三系灵法同出玄冥，各守一脉气象。",
        completion_badge_name="三灵同契",
        epilogue_title="霜火沧澜试",
        epilogue_body=(
            "八仙岛演武台清晨起雾，冰剑先落三尺寒霜，赤符紧随其后化作一圈暖焰，将台面冻气逼成细碎水汽。\n\n"
            "水龙书生收起折扇，抬手引散半空残雾，温声评点：“霜势太直，火势太急；若是同行对阵，倒恰好替彼此补了破绽。”\n\n"
            "火符少年嘿嘿一笑，收起朱砂笔；冰剑少女还剑入鞘，只留下一句：“明日卯时，再来一局。”"
        ),
    ),
    dict(
        id=3,
        series_key="lotus_melody",
        name="清音济世",
        sort_order=3,
        enabled=1,
        desc="青莲抚伤，瑶琴涤尘，乱世中守住一方安宁。",
        completion_badge_name="清音杏林",
        epilogue_title="青莲闻琴",
        epilogue_body=(
            "归元城药庐后院刚晒好新采的莲心，天音少女便抱着瑶琴坐在廊下试弦。清越的泛音散开，连檐下打盹的雀儿都歪头听了一会儿。\n\n"
            "莲花医者分装完最后一包安神散，递过一杯温茶：“方才送来的几位伤员心神不宁，你这一曲比我半副汤药还管用。”\n\n"
            "天音少女浅笑颔首：“那以后你煎药时，我便在廊下多弹两段，权当抵你的茶钱。”"
        ),
    ),
]

APPEARANCE_SHEET = "CharacterAppearance"
APPEARANCE_ROWS = [
    dict(
        id=2,
        appearance_id="00_reference_topright_boy",
        name="金发带Q道童",
        enabled=1,
        sort_order=1,
        desc="金冠束发，葫芦傍身的青玉小道童",
        starter_creatable=1,
        card_unlockable=1,
        series_id=1,
        archive_name="陆照禾",
        nickname="阿禾",
        biography_summary="母亲在归元城开面铺，祖母膝盖疼，把留灯院的钥匙交给阿禾。金葫芦里装的是井水。他记得谁吃面不放葱，却老忘了自己也能开口求人。",
        story_title="第四只碗",
        story_body=(
            "屋顶又漏了。阿禾一手举着碗，一手还端着给巷口送的面。\n\n"
            "停云扶住门框：“你还要走？”\n\n"
            "他迟疑了一下：“能替我看一会儿院子吗？”\n\n"
            "她把第四只碗往左挪了半寸：“成。回来带葱。”"
        ),
    ),
    dict(
        id=3,
        appearance_id="01_ice_sword_girl",
        name="冰剑少女",
        enabled=1,
        sort_order=2,
        desc="寒霜凝刃，清冷出尘",
        starter_creatable=1,
        card_unlockable=1,
        series_id=2,
        archive_name="霜见雪",
        nickname="",
        biography_summary="出身北境玄霜剑阁，剑鞘上系着半截冻裂的旧流苏。性子清冷少言，练剑从不避风雪，却会悄悄把演武台边结冰的石阶扫净，免得旁人滑倒。",
        story_title="霜阶扫雪",
        story_body=(
            "卯时的演武台结着一层薄霜，霜见雪收剑归鞘，顺手抄起廊角那把秃头竹帚。\n\n"
            "许炽揉着眼走上石阶：“你每日练完剑，还要替杂役扫地？”\n\n"
            "她头也不抬，把最后一道暗冰扫进排水沟：“昨日你踩滑，险些撞翻沈澄的茶壶。”\n\n"
            "许炽噎了一下，抓起火符烘暖石面：“那明日我早来半刻，烤干了省得你扫。”她停了停帚：“别把青石烤裂。”"
        ),
    ),
    dict(
        id=4,
        appearance_id="02_fire_talisman_boy",
        name="火符少年",
        enabled=1,
        sort_order=3,
        desc="朱砂赤符，烈焰破邪",
        starter_creatable=0,
        card_unlockable=1,
        series_id=2,
        archive_name="许炽",
        nickname="小炽",
        biography_summary="归元城南朱砂坊的学徒，衣袖总沾着洗不净的红墨。画符讲究一气呵成，常因性子太急烧焦符角，偏偏烤栗子与暖炉火候拿捏得极准。",
        story_title="焦边朱砂",
        story_body=(
            "案头摞着半尺画废的赤符，边缘全卷着焦黑的边。许炽盯着最后一笔叹气：“又收不住火。”\n\n"
            "沈澄递过一碗放凉的井水：“你下笔只想着破邪，没留半寸回旋的余地。”\n\n"
            "许炽蘸了点墨，重新压稳腕力，这一回朱砂顺着黄纸一气贯通，符尾只腾起一缕温热的细烟。\n\n"
            "他咧嘴笑起来，转身把废符拢进小炉：“不白费，正好留着给你们烤一兜糖栗子。”"
        ),
    ),
    dict(
        id=5,
        appearance_id="03_lotus_healer_girl",
        name="莲花医者",
        enabled=1,
        sort_order=4,
        desc="青莲济世，妙手回春",
        starter_creatable=0,
        card_unlockable=1,
        series_id=3,
        archive_name="苏青莲",
        nickname="",
        biography_summary="归元城西药庐的掌勺医者，案头常年养着一钵清露青莲。诊脉细心、记性极好，谁怕苦药、谁爱忘按时换敷，她都在药包绳结上打了暗记。",
        story_title="药包双结",
        story_body=(
            "暮色沉下来，药庐最后一位扭伤脚踝的脚夫刚被送出门。苏青莲解开围裙，顺手往桌上多放了一颗话梅。\n\n"
            "林知音抱着瑶琴凑过来：“这副安神散怎么系了两道活结？”\n\n"
            "“那位阿婆眼神不好，摸见双结便知是睡前那一包。”苏青莲吹熄药炉余火，“单结的才是晨起喝的。”\n\n"
            "林知音替她拢起晒干的莲心：“难怪街坊都说，你开的药还没入口，心就先安了一半。”"
        ),
    ),
    dict(
        id=7,
        appearance_id="05_celestial_musician_girl",
        name="天音少女",
        enabled=1,
        sort_order=5,
        desc="瑶琴仙乐，清音涤尘",
        starter_creatable=0,
        card_unlockable=1,
        series_id=3,
        archive_name="林知音",
        nickname="",
        biography_summary="游历九州的抚琴弟子，随身一床七弦松木瑶琴。不爱在酒楼高台炫技，更常坐到城西药庐的竹廊下试弦，说是檐风与药香最衬清越泛音。",
        story_title="廊下试弦",
        story_body=(
            "夏雨初歇，琴弦受潮略松。林知音坐在药庐长廊下，一寸一寸调着徽位，拨出三声散音。\n\n"
            "里屋正熬药的苏青莲探出头：“方才那段慢了一拍，是换了新弦？”\n\n"
            "“不是新弦。”林知音指尖轻按七徽，看向檐下避雨的一排灰雀，“雨声太急，我放慢些，免得把它们惊飞。”\n\n"
            "廊外水珠顺着瓦当落进青莲钵，正好接上她下一记清脆的泛音。"
        ),
    ),
    dict(
        id=8,
        appearance_id="09_bamboo_archer_girl",
        name="竹弓少女",
        enabled=1,
        sort_order=6,
        desc="青竹穿云，百步穿杨",
        starter_creatable=0,
        card_unlockable=1,
        series_id=1,
        archive_name="桑听雨",
        nickname="",
        biography_summary="月汐湾竹器人家的女儿，想去八景岛画潮汐鸟谱。家里早备好行李，她却把船票让给了别人。竹筒里还留着旧票角，和一张没落过笔的纸。",
        story_title="没有画的那一页",
        story_body=(
            "阿禾调好了青色：“岛上的鸟脚也是青的吗？”\n\n"
            "听雨看看他摊开的画纸：“岸边这只青。岛上的……替我空一页吧。”\n\n"
            "饭后她独自走到渡口，问船家，一个人带行李该坐哪一边。"
        ),
    ),
    dict(
        id=9,
        appearance_id="10_crimson_spear_girl",
        name="赤枪少女",
        enabled=1,
        sort_order=7,
        desc="红缨长枪，飒爽凌云",
        starter_creatable=0,
        card_unlockable=1,
        series_id=1,
        archive_name="祝停云",
        nickname="",
        biography_summary="月汐湾摆渡人家的女儿。父亲掌舵，母亲修船，她把旧船绳编进枪穗，想挣来修船的钱，也想堂堂正正赢一场。败局册里记给自己的问题，字总是特别小。",
        story_title="纸也赢过一回",
        story_body=(
            "她连输三场回院，枪穗往砚台里滴水。\n\n"
            "沈澄写“惜败”，她说：“去掉惜字。”\n\n"
            "自己把“对手拖”划成“我又急了”。\n\n"
            "吃到半碗面，她拿筷子比了两步：“什么时候该等？”\n\n"
            "他抬眼：“昨天谁说我纸上谈兵？”\n\n"
            "“今天纸赢了。你讲。”"
        ),
    ),
    dict(
        id=10,
        appearance_id="15_water_dragon_scholar_boy",
        name="水龙书生",
        enabled=1,
        sort_order=8,
        desc="沧澜化龙，文武兼修",
        starter_creatable=0,
        card_unlockable=1,
        series_id=2,
        archive_name="沈澄",
        nickname="",
        biography_summary="常驻八仙岛与归元城之间的沧澜院书生，一柄素面折扇能引水势也能记战局。待人温和有礼，爱把各家招式的破绽抄进袖珍册页，却最怕别人在茶凉前催他下棋。",
        story_title="半盏未凉",
        story_body=(
            "八仙岛石亭风大，沈澄拿镇纸压住刚画好的阵势图，顺手替对面续了半盏清茶。\n\n"
            "霜见雪瞄了一眼图上密密麻麻的批注：“你连许炽收势慢半息都记下来了？”\n\n"
            "“好记性不如烂笔头。”沈澄合扇轻敲掌心，“昨日在归元城留灯院，祝姑娘拿筷子比步法，倒让我又看清两处变招。”\n\n"
            "亭外水汽漫过栏杆，他端起茶盏浅啜一口：“趁热喝吧，茶凉了，棋局就不好看了。”"
        ),
    ),
    dict(
        id=11,
        appearance_id="20_star_formation_master_girl",
        name="星阵宗师",
        enabled=1,
        sort_order=9,
        desc="星图流转，破阵问道；五行试炼志永久战法印达 18 枚解锁的零属性玩法专属形象。",
        starter_creatable=0,
        card_unlockable=0,
        series_id=0,
        archive_name="星璇",
        nickname="",
        biography_summary="观星台守阵人，袖中星盘刻满五行生克轨迹。不以重金易道，唯认破阵历练之实；凡在五行试炼志积满十八枚战法印者，方得她亲授观星同游之约。",
        story_title="十八星印",
        story_body=(
            "观星台夜露微凉，星璇俯身拨正铜盘上最后一枚星子，抬眸望向石阶尽头。\n\n"
            "“破阵、御障、回春、易势——十八枚战法印，倒真叫你一枚不落地集齐了。”\n\n"
            "她合上星图卷轴，并未赠出半件神兵，只将一枚素净的玉星挂回腰间：“阵法在心，不在器物。这一路你走过的每一局，自己已是最好的凭据。”\n\n"
            "星盘轻转半圈，檐外星河倒映在台面水镜中：“既已过关，今夜归元城的风，便由你换副模样去看看。”"
        ),
    ),
]

LEGACY_APPEARANCE_DESCRIPTION = "使用后可永久切换一次角色形象，切换后将替换当前形象（商城售价 10 元）。"

RULE_SHEET = "AppearanceRule"
RULE_ROWS = [
    dict(
        id=1,
        card_item_id=3402,
        card_item_count=1,
        trial_duration_seconds=3600,
        mall_sku_id=3402,
        mall_price_fen=1000,
        desc="角色形象切换与一小时免费试用全局规则(单行)",
    ),
]

APPEARANCE_CARD_ITEM = dict(
    id=3402,
    max_stack_size=99,
    equip_kind=0,
    battle_usable=0,
    battle_heal_hp=0,
    battle_heal_mp=0,
    name="角色形象卡",
    description="使用后可永久解锁并切换一款支持兑换的角色形象，已拥有的形象可随时免费切换（商城售价 10 元）。",
    icon_key="UI/Icons/item_appearance_card",
    equip_level=0,
    equip_class=0,
    affix_pool=0,
    affix_rule=0,
    base_attr=[],
    equip_profile=0,
    forge_material_id=0,
    forge_material_count=0,
    artifact_id=0,
)

TIP_GROUP = "appearance_error"
TIP_BASE = 33000
TIP_WIDTH = 1000
TIP_CODES = [
    ("AppearanceInvalid", "所选角色形象不可用", False),
    ("AppearanceAlreadyCurrent", "当前已是该角色形象，无需重复切换", False),
    ("AppearanceNoCard", "背包中没有可用的角色形象卡，请前往商城购买", False),
    ("AppearanceTrialAlreadyUsed", "该角色形象已使用过免费试用机会", False),
    ("AppearanceNoActiveTrial", "当前没有进行中的形象试用", False),
    ("AppearanceFrozen", "当前角色状态暂时不能切换形象，请稍后再试", False),
    ("AppearanceUnavailable", "角色形象服务暂时不可用", True),
    ("AppearanceNotCardUnlockable", "该角色形象需通过专属玩法或活动解锁，暂不可使用形象卡兑换", False),
    ("AppearanceAlreadyOwned", "已拥有该角色形象，可直接免费切换，无需试用", False),
    ("AppearanceNotOwned", "尚未拥有该角色形象，请先解锁后再切换", False),
]


def self_check() -> None:
    seen_series_ids = set()
    seen_series_keys = set()
    for row in SERIES_ROWS:
        if row["id"] <= 0 or row["id"] in seen_series_ids:
            fail(f"invalid or duplicate AppearanceSeries id: {row['id']}")
        if not row["series_key"] or row["series_key"] in seen_series_keys:
            fail(f"invalid or duplicate series_key: {row['series_key']}")
        seen_series_ids.add(row["id"])
        seen_series_keys.add(row["series_key"])
    seen_ids = set()
    seen_app = set()
    for row in APPEARANCE_ROWS:
        if row["id"] <= 0 or row["id"] in seen_ids:
            fail(f"invalid or duplicate CharacterAppearance id: {row['id']}")
        if not row["appearance_id"] or row["appearance_id"] in seen_app:
            fail(f"invalid or duplicate appearance_id: {row['appearance_id']}")
        if row["series_id"] != 0 and row["series_id"] not in seen_series_ids:
            fail(f"CharacterAppearance id={row['id']} references unknown series_id={row['series_id']}")
        bio_core = (row["archive_name"], row["biography_summary"], row["story_title"], row["story_body"])
        has_any_bio = any(bool(s.strip()) for s in bio_core) or bool(row["nickname"].strip())
        if has_any_bio and not all(bool(s.strip()) for s in bio_core):
            fail(f"CharacterAppearance id={row['id']} has incomplete biography columns")
        if "闻砚生" in (row["biography_summary"] + row["story_body"]):
            fail(f"CharacterAppearance id={row['id']} still references removed character 闻砚生")
        seen_ids.add(row["id"])
        seen_app.add(row["appearance_id"])
    rule = RULE_ROWS[0]
    if rule["id"] != 1 or rule["card_item_id"] != APPEARANCE_CARD_ITEM["id"]:
        fail("AppearanceRule card_item_id must match APPEARANCE_CARD_ITEM.id")
    if not 1 <= rule["card_item_count"] <= 99:
        fail("AppearanceRule card_item_count out of range")
    if not 60 <= rule["trial_duration_seconds"] <= 86400:
        fail("AppearanceRule trial_duration_seconds out of range")
    if len({name for name, _, _ in TIP_CODES}) != len(TIP_CODES):
        fail("TIP_CODES has duplicate code names")


def write_scalar_table(sheet: str, rows: list[dict], dry_run: bool) -> None:
    xlsx_path = DATA_DIR / f"{sheet}.xlsx"
    layout = schema_layout(sheet)
    if any(f.width != 1 for f in layout):
        fail(f"the {sheet} schema has non-scalar columns")
    names = [f.name for f in layout]
    for idx, row in enumerate(rows):
        if list(row.keys()) != names:
            fail(f"{sheet} row {idx} keys {list(row.keys())} != schema fields {names}")

    wanted = [[None] * len(names) for _ in range(DATA_BEGIN_ROW - 1 + len(rows))]
    wanted[HEADER_ROW - 1] = list(names)
    wanted[COMMENT_ROW - 1] = [f.comment or None for f in layout]
    for offset, row in enumerate(rows):
        wanted[DATA_BEGIN_ROW - 1 + offset] = [None if row[n] == "" else row[n] for n in names]

    if xlsx_path.exists():
        ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path)
        existing_matrix = sheet_matrix(ws)
        if ws.title == sheet and first_difference(existing_matrix, wanted) is None:
            print(f"[ALREADY CURRENT] {rel(xlsx_path)} exists with identical contents; file unchanged.")
            return
        prev_cols = len(existing_matrix[HEADER_ROW - 1]) if len(existing_matrix) >= HEADER_ROW else 0
        can_upgrade_columns = (
            ws.title == sheet
            and 0 < prev_cols < len(names)
            and len(existing_matrix) == len(wanted)
            and existing_matrix[HEADER_ROW - 1] == wanted[HEADER_ROW - 1][:prev_cols]
            and all(
                existing_matrix[r][:prev_cols] == wanted[r][:prev_cols]
                and all(v is None for v in existing_matrix[r][prev_cols:])
                for r in range(DATA_BEGIN_ROW - 1, len(wanted))
            )
        )
        if not can_upgrade_columns:
            fail(f"{rel(xlsx_path)} already exists with different contents -- refusing to overwrite.")
        print(f"Will upgrade {rel(xlsx_path)}: {prev_cols} -> {len(names)} columns x {len(rows)} rows")
    else:
        print(f"Will create {rel(xlsx_path)}: sheet={sheet!r}, {len(names)} columns x {len(rows)} rows")
    if dry_run:
        return
    wb = openpyxl.Workbook()
    ws = wb.active
    ws.title = sheet
    for r, values in enumerate(wanted, start=1):
        for c, value in enumerate(values, start=1):
            if value is not None:
                ws.cell(row=r, column=c, value=value)
    wb.save(xlsx_path)
    check_ws = only_sheet(openpyxl.load_workbook(xlsx_path), xlsx_path, sheet)
    diff = first_difference(sheet_matrix(check_ws), wanted)
    if diff:
        fail(f"after saving, {xlsx_path.name} read back differs ({diff})")
    bind(sheet, xlsx_path)
    print(f"[DONE] {rel(xlsx_path)}")


def cmd_new_tables(dry_run: bool) -> None:
    write_scalar_table(SERIES_SHEET, SERIES_ROWS, dry_run)
    write_scalar_table(APPEARANCE_SHEET, APPEARANCE_ROWS, dry_run)
    write_scalar_table(RULE_SHEET, RULE_ROWS, dry_run)


def cmd_card_item_row(dry_run: bool) -> None:
    layout = schema_layout(ITEM_SHEET)
    row = APPEARANCE_CARD_ITEM
    if set(row.keys()) != {f.name for f in layout}:
        fail(
            f"APPEARANCE_CARD_ITEM keys differ from Item schema fields: "
            f"extra={sorted(set(row) - {f.name for f in layout})}, "
            f"missing={sorted({f.name for f in layout} - set(row))}"
        )

    wb = openpyxl.load_workbook(ITEM_XLSX)
    ws = only_sheet(wb, ITEM_XLSX, ITEM_SHEET)
    before = sheet_matrix(ws)
    header = header_cells(ws)
    bind(ITEM_SHEET, ITEM_XLSX)

    id_col = header["id"]
    existing: dict[int, int] = {}
    last_data_row = DATA_BEGIN_ROW - 1
    for row_idx in range(DATA_BEGIN_ROW, ws.max_row + 1):
        val = ws.cell(row=row_idx, column=id_col).value
        if val is None or val == "":
            continue
        existing[int(val)] = row_idx
        last_data_row = row_idx

    wanted = item_row_cells(row, layout, header)
    if row["id"] in existing:
        row_idx = existing[row["id"]]
        actual = {c: v for c, v in enumerate(before[row_idx - 1], start=1) if v is not None}
        if actual == wanted:
            print(f"[ALREADY CURRENT] {rel(ITEM_XLSX)} already has appearance card id={row['id']} on row {row_idx}.")
            return
        desc_col = header["description"]
        upgraded = dict(actual)
        if actual.get(desc_col) == LEGACY_APPEARANCE_DESCRIPTION:
            upgraded[desc_col] = wanted[desc_col]
        if upgraded == wanted:
            print(f"{rel(ITEM_XLSX)}: updating appearance card id={row['id']} description at row {row_idx}")
            if dry_run:
                return
            ws.cell(row=row_idx, column=desc_col, value=wanted[desc_col])
            wb.save(ITEM_XLSX)
            expected = [list(r) for r in before]
            expected[row_idx - 1][desc_col - 1] = wanted[desc_col]
            check_ws = only_sheet(openpyxl.load_workbook(ITEM_XLSX), ITEM_XLSX, ITEM_SHEET)
            diff = first_difference(sheet_matrix(check_ws), expected)
            if diff:
                fail(f"after saving, {ITEM_XLSX.name} read back differs ({diff})")
            bind(ITEM_SHEET, ITEM_XLSX)
            print(f"[DONE] {rel(ITEM_XLSX)} updated id={row['id']} description; schema binding passed.")
            return
        fail(f"{rel(ITEM_XLSX)} row {row_idx} already has id={row['id']} with different contents -- refusing to overwrite")

    target_row = last_data_row + 1
    patches: list[Patch] = [(target_row, col, val) for col, val in sorted(wanted.items())]
    print(f"{rel(ITEM_XLSX)}: appending appearance card id={row['id']} ({row['name']}) at row {target_row}")
    if dry_run:
        return
    apply_patches_and_verify(ITEM_XLSX, ITEM_SHEET, wb, ws, before, patches, bind_schema=True)
    print(f"[DONE] {rel(ITEM_XLSX)} appended id={row['id']}; schema binding passed.")


def cmd_tip_codes(dry_run: bool) -> None:
    wb = openpyxl.load_workbook(TIP_XLSX)
    ws = only_sheet(wb, TIP_XLSX)
    before = sheet_matrix(ws)
    header = [str(v).strip().lower() if v is not None else "" for v in before[HEADER_ROW - 1]]
    if header[:2] != ["name", "commen"] or header.count("fault") != 1:
        fail(f"{TIP_XLSX.name} row 1 header mismatch: {before[HEADER_ROW - 1]}")
    fault_col = header.index("fault") + 1

    groups, notes = tip_parse(before, fault_col)
    header_text = f"//{TIP_GROUP} base={TIP_BASE} width={TIP_WIDTH}"
    wanted = [(name, text, 1 if fault else None) for name, text, fault in TIP_CODES]
    new_names = [name for name, _t, _f in TIP_CODES]

    mine = next((g for g in groups if g["name"] == TIP_GROUP), None)
    if mine is not None:
        if (mine["base"], mine["width"]) == (TIP_BASE, TIP_WIDTH) and mine["codes"][:len(wanted)] == wanted:
            print(f"[ALREADY CURRENT] {header_text} (row {mine['row']}) already has these {len(wanted)} codes; file unchanged.")
            return
        existing_count = len(mine["codes"])
        last_non_empty = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
        group_end_row = mine["row"] + existing_count
        if (
            (mine["base"], mine["width"]) == (TIP_BASE, TIP_WIDTH)
            and existing_count < len(wanted)
            and mine["codes"] == wanted[:existing_count]
            and group_end_row == last_non_empty
        ):
            extra = wanted[existing_count:]
            owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
            clash = [f"{n} (in {owner[n]})" for n, _t, _f in extra if n in owner]
            if clash:
                fail(f"duplicate tip code names: {clash}")
            patches: list[Patch] = []
            for offset, (name, text, fault) in enumerate(extra, start=1):
                row_idx = group_end_row + offset
                patches += [(row_idx, 1, name), (row_idx, 2, text)]
                if fault is not None:
                    patches.append((row_idx, fault_col, fault))
            print(f"Will append {len(extra)} codes to {header_text} in {rel(TIP_XLSX)} starting at row {group_end_row + 1}")
            if dry_run:
                return
            apply_patches_and_verify(TIP_XLSX, None, wb, ws, before, patches, bind_schema=False)
            print(f"[DONE] {rel(TIP_XLSX)} appended {len(extra)} codes to {header_text} ({TIP_BASE + existing_count}..{TIP_BASE + len(wanted) - 1})")
            return
        fail(f"{TIP_XLSX.name} row {mine['row']} already has group //{TIP_GROUP} with different contents")

    owner = {code: g["name"] for g in groups for code, _t, _f in g["codes"]}
    clash = [f"{n} (in {owner[n]})" for n in new_names if n in owner]
    if clash:
        fail(f"duplicate tip code names: {clash}")
    lo, hi = TIP_BASE, TIP_BASE + TIP_WIDTH
    overlap = [f"{g['name']} [{g['base']},{g['base'] + g['width']})" for g in groups if g["base"] < hi and lo < g["base"] + g["width"]]
    if overlap:
        fail(f"range [{lo},{hi}) is already taken: {overlap}")
    reserved = [f"row {row} {text!r}" for row, text in notes if str(TIP_BASE) in text]
    if reserved:
        fail(f"range {TIP_BASE} is reserved by note rows: {reserved}")
    if TIP_STATE.exists():
        state_groups = json.loads(TIP_STATE.read_text(encoding="utf-8")).get("groups", {})
        tomb = [f"{n} [{g['base']},{g['base'] + g['width']})" for n, g in state_groups.items()
                if n != TIP_GROUP and int(g["base"]) < hi and lo < int(g["base"]) + int(g["width"])]
        if tomb:
            fail(f"range [{lo},{hi}) already in {rel(TIP_STATE)}: {tomb}")

    last_row = max((r for r, row in enumerate(before, start=1) if any(v is not None for v in row)), default=0)
    header_row = last_row + 2
    patches: list[Patch] = [(header_row, 1, header_text)]
    for offset, (name, text, fault) in enumerate(wanted, start=1):
        row_idx = header_row + offset
        patches += [(row_idx, 1, name), (row_idx, 2, text)]
        if fault is not None:
            patches.append((row_idx, fault_col, fault))
    print(f"Will append {header_text} ({len(wanted)} codes) to {rel(TIP_XLSX)} at row {header_row}")
    if dry_run:
        return
    apply_patches_and_verify(TIP_XLSX, None, wb, ws, before, patches, bind_schema=False)
    print(f"[DONE] {rel(TIP_XLSX)} {header_text} ({lo}..{lo + len(wanted) - 1})")


def cmd_all(dry_run: bool) -> None:
    cmd_new_tables(dry_run)
    cmd_card_item_row(dry_run)
    cmd_tip_codes(dry_run)


COMMANDS = {
    "new-tables": cmd_new_tables,
    "card-item-row": cmd_card_item_row,
    "tip-codes": cmd_tip_codes,
    "all": cmd_all,
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
