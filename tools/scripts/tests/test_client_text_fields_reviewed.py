"""玩家能填的文字字段,必须登记「送检」「豁免」或「待接入」三者之一。只读 proto 源码,不编译、不访问网络。

为什么要这道守卫(docs/design/rename-and-content-safety.md §6.6):
屏蔽字是按入口接的 —— 建角名在 login、帮会名与公告在 guild、聊天在 chat、宠物名在 scene。以后谁给客户端请求
加了一个新的文字字段(签名、备注、房间名……)而忘了送检,不会有任何报错,只是多了一个没人管的口子。
这里把「客户端请求里出现的 string 字段」全部列出来,逐个要求表态;新字段不登记,这里判红。

范围:客户端可见服务(名字带 ClientPlayer,外加 EXTRA_CLIENT_SERVICES)里,请求类型以 Request 结尾的 rpc,
沿消息字段往下走到的全部 string 字段。服务器发给客户端的推送(请求类型不以 Request 结尾)不是玩家输入,不在范围内。
这是文本解析,不调 protoc:只认本仓 proto 的常规写法(一行一个字段)。
"""

from pathlib import Path
import re
import unittest


REPO = Path(__file__).resolve().parents[3]
PROTO_ROOT = REPO / "proto"

# 名字里不带 ClientPlayer、但客户端经 gate / 路由服 / 网关直接调得到的服务。
EXTRA_CLIENT_SERVICES = {"GuildService", "MatchService", "GateClientSpectate", "LoginPreGate"}

# 一看名字就不是给人看的文字:请求号、游标、令牌、各种 id。按字段名整体豁免,不逐个登记。
MACHINE_FIELD = re.compile(r"(?:^|_)(?:request_id|cursor|token|ticket|id)$")

# 送检:字段 -> 内容安全场景(proto/content_guard/content_guard.proto 的 TextScene)。
# 调用点:login 的 createplayer_content.go、guild 的 content_check.go、chat 的 authority/content_review.go、
# scene 的 PetSystem::RequestRename。
REVIEWED = {
    "CreatePlayerRequest.name": "TEXT_SCENE_ROLE_NAME",
    "RenamePlayerRequest.name": "TEXT_SCENE_ROLE_NAME",
    "CreateGuildRequest.name": "TEXT_SCENE_GUILD_NAME",
    "RenameGuildRequest.name": "TEXT_SCENE_GUILD_NAME",
    "SetAnnouncementRequest.announcement": "TEXT_SCENE_GUILD_ANNOUNCEMENT",
    "ChatMessage.content": "TEXT_SCENE_CHAT_WORLD / TEXT_SCENE_CHAT_PRIVATE",
    "RenamePetRequest.name": "TEXT_SCENE_PET_NAME",
    "CreateRmbConsignmentRequest.title": "TEXT_SCENE_TRADE_TITLE",
    # 买家评价的文字全服可见:go/trade 的 economy_credit_logic.go 经 checkTradeTitle 送检。
    "SubmitSellerReviewRequest.comment": "TEXT_SCENE_TRADE_TITLE",
    # 赛事战队名别的玩家看得到:go/match 的 tournamentlogic.go 按帮会名的口径送检。
    "CreateTournamentSquadRequest.squad_name": "TEXT_SCENE_GUILD_NAME",
}

# 豁免:字段 -> 为什么不用送检。
EXEMPT = {
    # 账号、凭据、实名:不展示给别的玩家。
    "LoginRequest.account": "账号标识,不展示给其他玩家",
    "LoginRequest.password": "凭据",
    "LoginRequest.auth_type": "登录方式的固定取值",
    "AccountDeletionRequest.account": "账号标识",
    "AccountDeletionRequest.password": "凭据",
    "AccountDeletionRequest.auth_type": "登录方式的固定取值",
    "AccountDeletionRequest.authorization_code": "第三方授权码",
    "CancelAccountDeletionRequest.account": "账号标识",
    "ServiceCallAuth.caller": "服务间调用方名字,不是玩家输入",
    "ServiceCallAuth.signature_hex": "服务间签名",
    "SubmitRealNameRequest.name": "实名,只送防沉迷系统核验,不展示",
    "SubmitRealNameRequest.id_number": "证件号,只送防沉迷系统核验,不展示",
    "CreatePayIntentRequest.login_code": "平台登录凭证",
    "CreatePayIntentRequest.device_os": "设备系统的固定取值",
    # 只用来查找,不落库、不展示。
    "RecommendFriendsRequest.query": "搜索词,只用来查找",
    "BrowseListingsRequest.search": "搜索词,只用来查找",
    "RankBoardKey.period": "榜单周期的固定取值",
    # 只有自己看得见。
    "CreateAttributeSchemeRequest.name": "加点方案名只有自己可见;送检是异步的,会破坏这个同步接口的契约",
    "RenameAttributeSchemeRequest.name": "同上",
    "SaveEquipLoadoutRequest.name": "装备预设方案名只有自己可见(只经 GetEquipLoadouts 回给本人),与加点方案名同口径",
    "SaveSkillLoadoutRequest.name": "技能装配方案名只有自己可见,同上",
    "SavedBattleBuildBookmarkView.title": "流派收藏是玩家给自己存的书签,标题只回给本人",
    "SavedBattleBuildBookmarkView.source_player_name": "同上:书签里记的来源玩家名,只回给本人",
    "SavedBattleBuildBookmarkView.active_pet_name": "同上:书签里记的宝宝名,只回给本人",
    # 配置键、凭证这类机器取值,不是给人看的文字。
    "ClaimFestivalBeastRewardRequest.reward_identity": "奖励条目的标识,机器取值",
    "ClaimRewardedAdRequest.ssv_proof": "广告平台的服务端校验凭证",
    "EquipTravelPassCosmeticRequest.cosmetic_key": "外观的配置键",
    "RankedCosmeticLoadout.equipped_avatar_pattern_code": "外观的配置码",
    "RankedCosmeticLoadout.equipped_card_bg_code": "外观的配置码",
    "RankedCosmeticLoadout.equipped_talisman_code": "外观的配置码",
    "RankedCosmeticLoadout.equipped_title_code": "外观的配置码",
    # 有别的审核流程。
    "BattleScript.name": "玩家剧本上传后人工审核通过才可用(eBattleScriptOrigin)",
    "BattleScript.description": "同上",
    "BattleScriptRole.name": "同上",
    "BattleScriptRule.note": "同上",
    # 名字以 Request 结尾、其实是服务器下发的。
    "GameKickPlayerRequest.operator": "服务器下发的踢人原因,不是玩家输入",
    "TipInfoMessage.parameters": "随踢人通知下发的提示参数,服务器填的",
    # 聊天举报说明:仅记入举报审计表给运营后台审核,不向其他玩家展示,服务端限长度与UTF-8。
    "ReportChatRequest.detail": "仅入举报审计表,不向其他玩家展示",
    # 聊天撤回原因:内部/GM/后台撤回审计用,CLIENT_RPC_SERVER_ONLY 禁止客户端直调。
    "RetractWorldChatRequest.reason": "服务端内部/GM撤回原因,客户端不可调(CLIENT_RPC_SERVER_ONLY)",
    # 系统世界公告:服务端内部发起(CLIENT_RPC_SERVER_ONLY),带的是已经审过的角色名。
    "PublishSystemWorldNoticeRequest.actor_a_name": "服务端内部发起的系统公告,名字是已审过的角色名,客户端不可调",
    "PublishSystemWorldNoticeRequest.actor_b_name": "同上",
}

# 待接入:别的玩家看得到、现在还没送检的字段。登记在这里是为了不让它悄悄留着;接上之后挪到 REVIEWED。
PENDING = {}

COMMENT = re.compile(r"//[^\n]*|/\*.*?\*/", re.S)
SCALARS = {"bytes", "bool", "double", "float", "string"}
INTEGER_PREFIXES = ("uint", "int", "sint", "fixed", "sfixed")
FIELD = re.compile(r"(?:^|[;{}\n])\s*(?:repeated\s+|optional\s+)?(map\s*<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+")


def block_end(text, open_brace):
    depth = 0
    for index in range(open_brace, len(text)):
        if text[index] == "{":
            depth += 1
        elif text[index] == "}":
            depth -= 1
            if depth == 0:
                return index
    raise ValueError("unbalanced braces")


def parse_messages(text):
    """返回 {消息名: [(类型, 字段名)]}。嵌套消息按自己的名字单独登记,不算进外层的字段。"""
    found = {}
    for match in re.finditer(r"\bmessage\s+(\w+)\s*\{", text):
        end = block_end(text, match.end() - 1)
        body = text[match.end():end]
        nested = []
        for inner in re.finditer(r"\b(?:message|enum)\s+\w+\s*\{", body):
            nested.append((inner.start(), block_end(body, inner.end() - 1) + 1))
        for start, stop in sorted(nested, reverse=True):
            if all(not (s < start and stop <= e) for s, e in nested):
                body = body[:start] + body[stop:]
        found.setdefault(match.group(1), [(m.group(1).strip(), m.group(2)) for m in FIELD.finditer(body)])
    return found


def parse_services(text):
    """返回 [(服务名, [(方法名, 请求类型)])]。"""
    found = []
    for match in re.finditer(r"\bservice\s+(\w+)\s*\{", text):
        body = text[match.end():block_end(text, match.end() - 1)]
        found.append((match.group(1), re.findall(r"\brpc\s+(\w+)\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)", body)))
    return found


def is_string_type(field_type):
    compact = field_type.replace(" ", "")
    return compact == "string" or (compact.startswith("map<") and "string" in compact[4:-1].split(","))


def client_text_fields(sources):
    """sources: {相对路径: proto 文本}。返回 {"消息.字段": "服务.方法"}(第一个走到它的入口)。"""
    messages, services = {}, []
    for text in sources.values():
        stripped = COMMENT.sub("", text)
        for name, fields in parse_messages(stripped).items():
            messages.setdefault(name, fields)
        services.extend(parse_services(stripped))

    reached = {}

    def walk(type_name, entry, seen):
        simple = type_name.split(".")[-1]
        if simple in seen or simple not in messages:
            return
        seen.add(simple)
        for field_type, field_name in messages[simple]:
            if is_string_type(field_type):
                reached.setdefault(f"{simple}.{field_name}", entry)
            elif field_type not in SCALARS and not field_type.startswith(INTEGER_PREFIXES):
                walk(field_type, entry, seen)

    for service, rpcs in services:
        if "ClientPlayer" not in service and service not in EXTRA_CLIENT_SERVICES:
            continue
        for method, request in rpcs:
            if request.split(".")[-1].endswith("Request"):
                walk(request, f"{service}.{method}", set())
    return reached


def needs_decision(field_key):
    return not MACHINE_FIELD.search(field_key.split(".", 1)[1])


class ClientTextFieldsTest(unittest.TestCase):
    def test_every_player_text_field_is_reviewed_exempt_or_pending(self):
        sources = {path.relative_to(REPO).as_posix(): path.read_text(encoding="utf-8") for path in sorted(PROTO_ROOT.rglob("*.proto"))}
        self.assertTrue(sources, "no proto sources found")
        reached = client_text_fields(sources)
        for known in REVIEWED:
            self.assertIn(known, reached, f"{known} is registered as reviewed but no client request reaches it any more")

        registry = {**REVIEWED, **EXEMPT, **PENDING}
        self.assertEqual(len(registry), len(REVIEWED) + len(EXEMPT) + len(PENDING), "a field is registered in more than one table")
        undecided = sorted(f"{key} (via {entry})" for key, entry in reached.items() if needs_decision(key) and key not in registry)
        self.assertEqual(
            undecided, [],
            "client requests carry text fields nobody has decided on. Send them to the content guard and add them to REVIEWED, "
            "or add them to EXEMPT with the reason (tools/scripts/tests/test_client_text_fields_reviewed.py)",
        )
        stale = sorted(key for key in registry if key not in reached)
        self.assertEqual(stale, [], "registered fields that no client request reaches any more: remove them from the registry")
        self.assertTrue(all(reason.strip() for reason in registry.values()), "every registered field needs a scene or a reason")

    def test_parser_follows_nested_messages_and_ignores_server_pushes(self):
        sources = {
            "a.proto": """
                // string hidden = 9; 注释里的不算
                message Inner { string note = 1; uint64 owner_id = 2; }
                message SayRequest { string request_id = 1; Inner inner = 2; repeated string tags = 3; map<string, string> extra = 4; }
                message PushS2C { string text = 1; }
                message Outer { message Nested { string deep = 1; } Nested nested = 1; uint32 count = 2; }
                message WrapRequest { Outer outer = 1; }
                service ChatClientPlayer {
                  rpc Say (SayRequest) returns (Empty);
                  rpc NotifyPush (PushS2C) returns (Empty);
                  rpc Wrap (WrapRequest) returns (Empty);
                }
                service Internal { rpc Do (SayRequest) returns (Empty); }
            """,
        }
        reached = client_text_fields(sources)
        self.assertEqual(
            sorted(reached),
            ["Inner.note", "Nested.deep", "SayRequest.extra", "SayRequest.request_id", "SayRequest.tags"],
        )
        self.assertEqual(reached["Inner.note"], "ChatClientPlayer.Say")
        self.assertFalse(needs_decision("SayRequest.request_id"))
        self.assertFalse(needs_decision("X.target_player_id"))
        self.assertFalse(needs_decision("X.access_token"))
        self.assertTrue(needs_decision("Inner.note"))
        self.assertTrue(needs_decision("X.identity"), "a name that merely ends in the letters 'id' is still free text")


if __name__ == "__main__":
    unittest.main()
