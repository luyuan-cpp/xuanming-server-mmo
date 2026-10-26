#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""邮件 M1a(docs/design/mail-system.md §9)在**全量 proto-gen 之后**要做的三件收尾,写成可重复执行的脚本。

为什么要有它:
  - 生成器不会把新增 proto 的 C++ 产物登记进工程文件(CMakeLists.txt / *.vcxproj / *.vcxproj.filters),
    漏登记的后果是链接期才报 LNK2001 / LNK2019,而且只在 msbuild 全量编译时才暴露。
  - robot 走 vendor(-mod=vendor):go/proto/mail 是新包,不同步进 robot/vendor 并在 modules.txt 登记,
    生成器给 robot 出的 6 个 handler 桩就 import 不到它。
  - 生成结果有几条硬性验收(恰好 8 个消息号、路由表 6 条客户端协议 + 2 条内部协议、目标节点 MailNodeService),
    人工逐条看容易漏,这里机械核对。

四个子命令都**幂等**,都支持 --dry-run:

    python tools/scripts/mail_postgen.py verify          # 只读:核对 proto-gen 的产物
    python tools/scripts/mail_postgen.py register-cpp    # 把 mail 的 C++ 产物登记进 table / proto / grpc_client / rpc 四个工程
    python tools/scripts/mail_postgen.py vendor-robot    # go/proto/mail 与 mail_error_tip.pb.go 按字节同步进 robot/vendor
    python tools/scripts/mail_postgen.py all             # 依次跑上面三个

登记规则照 friend(同为 Go 实现的全局服务,但生成器对每个域都照常产出 C++ 的 pb / grpc_client / service_metadata,
rpc_event_registry 与 grpc_init_client 会引用它们):每个工程里 friend 有哪类条目,mail 就登记对应的条目;
mail_table 的 pb 与 friend_table 一样**不登记**(C++ 没有任何代码引用它)。

register-cpp 是两阶段的:先在内存里算出全部登记,缺文件、出现不认识的 mail 产物、找不到 friend 锚点时
在写盘之前中止(非 0 退出、点名);全部通过才统一写。每一条按"是否已登记"单独判断,中途失败后重跑不会写出重复条目。
合并后已有的等价重复登记也会收敛为第一条；带条件或不同编译元数据的重复项拒绝自动处理，保留原工程供人工核对。
--allow-missing 只登记已存在的文件,给"导表已跑、proto-gen 还没跑"的阶段用(那时只有 table 工程的 mail_error_tip 存在);
--allow-unknown 在确认不认识的产物不需要登记之后才用。

tools/scripts/dev_tools.ps1 的 proto-gen-run 在生成成功后会自动跑 register-cpp 与 vendor-robot($ProtoGenPostHooks),
做统一重生成的人不必记得这一步;verify 是验收用的,仍手工跑。
"""

from __future__ import annotations

import argparse
import filecmp
import io
import ntpath
import re
import shutil
import subprocess
import sys
from pathlib import Path
import xml.etree.ElementTree as ET

REPO = Path(__file__).resolve().parents[2]
BS = chr(92)

CLIENT_METHODS = ("ListMails", "ReadMail", "DeleteMails", "ClearReadMails", "ClaimMailAttachments", "NotifyMailChanged")
ADMIN_METHODS = ("SendPersonalMail", "SendSystemMail", "SendGuildMail", "ImportMailRecipients", "SealMailCampaign")
GO_PROTO_FILES = ("mail.pb.go", "mail_grpc.pb.go", "mail_admin.pb.go", "mail_admin_grpc.pb.go", "mail_table.pb.go")
TIP_GO_FILE = "mail_error_tip.pb.go"


def fail(message: str) -> "NoReturn":
    sys.exit(f"[中止] {message}")


def rel(path: Path) -> str:
    return path.relative_to(REPO).as_posix()


# ════════════════════════════════════════════════════════════════════════════
# verify
# ════════════════════════════════════════════════════════════════════════════

def parse_message_ids(text: str, where: str) -> dict[str, int]:
    ids: dict[str, int] = {}
    for line_no, line in enumerate(text.splitlines(), start=1):
        line = line.strip()
        if not line:
            continue
        number, sep, key = line.partition("=")
        if not sep or not number.isdigit():
            fail(f"{where}:{line_no} 不是 `<号>=<服务名+方法名>` 形状:{line!r}")
        if key in ids:
            fail(f"{where}:{line_no} 的 {key!r} 出现了两次")
        ids[key] = int(number)
    return ids


def load_message_ids() -> dict[str, int]:
    path = REPO / "proto" / "message_id.txt"
    if not path.is_file():
        fail(f"找不到 {rel(path)} —— 消息号表由全量 proto-gen 生成,先跑 proto-gen")
    return parse_message_ids(path.read_text(encoding="utf-8"), rel(path))


def committed_message_ids() -> dict[str, int] | None:
    """HEAD 里的 proto/message_id.txt(生成之前的号);拿不到(不是 git 工作树、文件不在 HEAD 里)返回 None。"""
    try:
        out = subprocess.run(["git", "-C", str(REPO), "show", "HEAD:proto/message_id.txt"],
                             capture_output=True, check=True)
    except (OSError, subprocess.CalledProcessError):
        return None
    return parse_message_ids(out.stdout.decode("utf-8"), "HEAD:proto/message_id.txt")


def cmd_verify(_dry_run: bool) -> None:
    problems: list[str] = []
    ids = load_message_ids()
    wanted = ["ClientPlayerMail" + m for m in CLIENT_METHODS] + ["MailAdmin" + m for m in ADMIN_METHODS]
    missing = [k for k in wanted if k not in ids]
    if missing:
        problems.append(f"proto/message_id.txt 缺 {missing}(含 mail 的全量 proto-gen 没跑或没跑完)")
    extra = sorted(k for k in ids if (k.startswith("ClientPlayerMail") or k.startswith("MailAdmin")) and k not in wanted)
    if extra:
        problems.append(f"proto/message_id.txt 多出 {extra}(mail.proto / mail_admin.proto 的方法与本脚本的清单不一致)")
    # 不要求号从 0 连续:生成器(service_register_info.go 的 InitMessageId)会把空出来的号分给新方法、
    # WriteMessageIdFile 跳过没人用的号,所以有方法被删的那次生成里 mail 可能拿到中间的旧号,文件里也可能留空洞。
    # 真正的不变量是:号不重复;生成之前就有的方法号不变(否则线上客户端与新服务端错解包)。
    numbers = sorted(ids.values())
    seen: dict[int, str] = {}
    for key, number in ids.items():
        if number in seen:
            problems.append(f"proto/message_id.txt 的号 {number} 同时分给了 {seen[number]} 与 {key}")
        seen[number] = key
    before = committed_message_ids()
    if before is None:
        print("[SKIP] 拿不到 HEAD:proto/message_id.txt,跳过\"已有方法号不变\"的核对")
    else:
        moved = sorted(f"{k}: {before[k]} -> {ids[k]}" for k in before if k in ids and ids[k] != before[k])
        if moved:
            problems.append(f"生成之前就有的方法号变了(会让已发布的客户端错解包):{moved}")

    route = REPO / "go" / "client_rpc_router" / "generated" / "pb" / "game" / "route_table.go"
    route_text = route.read_text(encoding="utf-8") if route.is_file() else ""
    for key in wanted:
        line = next((l for l in route_text.splitlines() if l.strip().startswith(key + "MessageId:")), None)
        if line is None:
            problems.append(f"{rel(route)} 没有 {key} 这一行")
            continue
        want_client = "true" if key.startswith("ClientPlayerMail") else "false"
        if "ENodeType_MailNodeService" not in line:
            problems.append(f"{rel(route)} 的 {key} 目标节点不是 MailNodeService:{line.strip()}")
        if f"ClientProtocol: {want_client}" not in line:
            problems.append(f"{rel(route)} 的 {key} 应为 ClientProtocol: {want_client}:{line.strip()}")

    go_dir = REPO / "go" / "proto" / "mail"
    for name in GO_PROTO_FILES:
        if not (go_dir / name).is_file():
            problems.append(f"缺 {rel(go_dir / name)}")
    for name in ("message_id.go", "event_id.go"):
        p = REPO / "go" / "mail" / "generated" / "pb" / "game" / name
        if not p.is_file():
            problems.append(f"缺 {rel(p)}")
    tip_go = REPO / "go" / "shared" / "generated" / "pb" / "table" / TIP_GO_FILE
    if not tip_go.is_file():
        problems.append(f"缺 {rel(tip_go)}(导表没跑?)")

    registry_h = REPO / "cpp" / "generated" / "rpc" / "service_metadata" / "rpc_event_registry.h"
    if not registry_h.is_file():
        problems.append(f"缺 {rel(registry_h)}(全量 proto-gen 没跑完?),无法核对 kMaxRpcMethodCount")
    else:
        m = re.search(r"kMaxRpcMethodCount\s*=\s*(\d+)", registry_h.read_text(encoding="utf-8"))
        if not m or int(m.group(1)) != max(numbers) + 1:
            problems.append(f"{rel(registry_h)} 的 kMaxRpcMethodCount 应为 {max(numbers) + 1}(最大消息号 + 1),"
                            f"实际 {m.group(1) if m else '找不到'} —— 生成器中途退出过,重跑一次 proto-gen")

    if problems:
        print("[FAIL] proto-gen output check:")
        for p in problems:
            print("  - " + p)
        sys.exit(1)
    print(f"[OK] mail message IDs: " + ", ".join(f"{k}={ids[k]}" for k in wanted))
    print("[OK] route table: 6 client protocols + 2 internal protocols, all targeting MailNodeService;"
          " Go bindings, message_id.go / event_id.go and the tip enum are present; registry capacity matches.")


# ════════════════════════════════════════════════════════════════════════════
# register-cpp
# ════════════════════════════════════════════════════════════════════════════

SRC, HDR = "源文件", "头文件"


def win(path: str) -> str:
    return path.replace("/", BS)


class Entry:
    """要登记的一条:lines 是不带换行符的若干行;key 判断它是否已经登记过(逐条判断,不靠首条代表整块)。"""

    def __init__(self, lines: list[str], key: str, cmake_path: str | None = None):
        self.lines = lines
        self.key = key
        self.cmake_path = cmake_path   # CMakeLists 里的条目按"整行等于该路径"判断,避免子串误判
        if cmake_path is None:
            item = ET.fromstring("\n".join(lines))
            self.tag = item.tag
            self.include = ntpath.normpath(item.attrib["Include"]).casefold()

    def xml_items(self, text: str) -> tuple[ET.Element, list[ET.Element]]:
        root = ET.fromstring(text)
        items = [item for item in root.iter()
                 if item.tag.rsplit("}", 1)[-1] == self.tag
                 and "Include" in item.attrib
                 and ntpath.normpath(item.attrib["Include"]).casefold() == self.include]
        return root, items

    def present(self, text: str) -> bool:
        if self.cmake_path is not None:
            return re.search(r"(?m)^[ \t]*" + re.escape(self.cmake_path) + r"[ \t]*\r?$", text) is not None
        return bool(self.xml_items(text)[1])


class ProjectEdit:
    """一个工程文件里"插在 friend 条目之后"的若干处登记。

    两阶段:plan() 只在内存里改文本,任何问题都在写盘之前暴露;save() 才写。文件按原有 BOM 写回,
    插入的行用锚点所在处的换行符(混合换行的文件也不会被改成另一种)。
    """

    def __init__(self, rel_path: str):
        self.path = REPO / rel_path
        self.rel = rel_path
        data = self.path.read_bytes()
        self.bom = data.startswith(b"\xef\xbb\xbf")
        self.text = data.decode("utf-8-sig")
        self.changed = 0
        self.removed = 0

    def _deduplicate(self, entry: Entry) -> None:
        if entry.cmake_path is not None:
            lists = list(re.finditer(r"(?ms)^set\(SOURCE_FILES[ \t]*\r?\n(.*?)^\)", self.text))
            if len(lists) != 1:
                fail(f"{self.rel}: SOURCE_FILES 必须恰好一段，不能安全去重")
            source_list = lists[0]
            pattern = r"(?m)^[ \t]*" + re.escape(entry.cmake_path) + r"[ \t]*(?:\r?\n|$)"
            matches = list(re.finditer(pattern, self.text[source_list.start(1):source_list.end(1)]))
            spans = [(source_list.start(1) + item.start(), source_list.start(1) + item.end()) for item in matches]
        else:
            root, items = entry.xml_items(self.text)
            if len(items) < 2:
                return
            parents = {child: parent for parent in root.iter() for child in parent}
            for item in items:
                current = item
                while current is not None:
                    if "Condition" in current.attrib:
                        fail(f"{self.rel}: 重复登记带 Condition，不能自动去重: {entry.key}")
                    current = parents.get(current)
                children = list(item)
                default_filter = SRC if entry.tag == "ClCompile" else HDR
                safe_filter = (self.rel.endswith(".filters") and len(children) == 1
                               and children[0].tag.rsplit("}", 1)[-1] == "Filter"
                               and not children[0].attrib and not list(children[0])
                               and children[0].text == default_filter)
                if set(item.attrib) != {"Include"} or (children and not safe_filter):
                    fail(f"{self.rel}: 重复登记的元数据不等价，不能自动去重: {entry.key}")
            # 只剪掉重复项所在的原始片段，不重新序列化 XML，保留其余字节与换行。
            pattern = (r"(?ms)^[ \t]*<(?P<tag>ClCompile|ClInclude)\b[^>]*?"
                       r"(?:/>|>.*?</(?P=tag)>)[ \t]*(?:\r?\n|$)")
            spans = []
            for match in re.finditer(pattern, self.text):
                item = ET.fromstring(match.group().strip())
                if (item.tag == entry.tag and "Include" in item.attrib
                        and ntpath.normpath(item.attrib["Include"]).casefold() == entry.include):
                    spans.append(match.span())
            if len(spans) != len(items):
                fail(f"{self.rel}: 重复登记不是独立条目，不能安全去重: {entry.key}")
        for start, end in reversed(spans[1:]):
            self.text = self.text[:start] + self.text[end:]
            self.changed += 1
            self.removed += 1

    def _find_anchor(self, anchor_lines: list[str]) -> tuple[int, str]:
        for eol in ("\r\n", "\n"):
            anchor = "".join(line + eol for line in anchor_lines)
            count = self.text.count(anchor)
            if count == 1:
                return self.text.index(anchor) + len(anchor), eol
            if count > 1:
                fail(f"{self.rel}: friend 的登记锚点出现了 {count} 次(应恰好 1 次):{anchor_lines[0].strip()[:100]!r}")
        fail(f"{self.rel}: 找不到 friend 的登记锚点(换行符试过 CRLF 与 LF):{anchor_lines[0].strip()[:100]!r}"
             f" —— friend 的登记被人改过,先看清再改本脚本")

    def plan(self, anchor_lines: list[str], entries: list[Entry]) -> None:
        # 合并可能同时保留域钩子与通用生成器的登记；先收敛已有重复，再补缺项。
        for entry in entries:
            self._deduplicate(entry)
        pending = [e for e in entries if not e.present(self.text)]
        if not pending:
            return
        pos, eol = self._find_anchor(anchor_lines)
        # 紧跟在锚点后面、已经登记好的条目原样跳过,新条目接在它们后面,保持与清单相同的相对顺序。
        for e in entries:
            block = "".join(line + eol for line in e.lines)
            if e.present(self.text) and self.text.startswith(block, pos):
                pos += len(block)
        added = "".join(line + eol for e in pending for line in e.lines)
        self.text = self.text[:pos] + added + self.text[pos:]
        self.changed += len(pending)

    def save(self, dry_run: bool) -> None:
        if self.changed and not dry_run:
            self.path.write_bytes((b"\xef\xbb\xbf" if self.bom else b"") + self.text.encode("utf-8"))
        print(f"  {self.rel:<52} {'planned' if dry_run else 'applied'}: "
              f"inserted {self.changed - self.removed}, removed duplicates {self.removed}")


def compile_line(path: str) -> str:
    return f'    <ClCompile Include="{win(path)}" />'


def include_line(path: str) -> str:
    return f'    <ClInclude Include="{win(path)}" />'


def compile_entry(path: str) -> Entry:
    return Entry([compile_line(path)], f'<ClCompile Include="{win(path)}"')


def include_entry(path: str) -> Entry:
    return Entry([include_line(path)], f'<ClInclude Include="{win(path)}"')


def filter_lines(tag: str, path: str, name: str) -> list[str]:
    return [f'    <{tag} Include="{win(path)}">', f"      <Filter>{name}</Filter>", f"    </{tag}>"]


def filter_entry(tag: str, path: str, name: str) -> Entry:
    return Entry(filter_lines(tag, path, name), f'<{tag} Include="{win(path)}"')


def cmake_entry(path: str) -> Entry:
    return Entry(["    " + path], path, cmake_path=path)


def present_files(base: str, files: list[str], missing: list[str]) -> list[str]:
    out = []
    for f in files:
        if (REPO / base / f).is_file():
            out.append(f)
        else:
            missing.append(f"{base}/{f}")
    return out


def plan_table(edits: list[ProjectEdit], missing: list[str]) -> None:
    base = "cpp/generated/table"
    files = present_files(base, ["proto/tip/mail_error_tip.pb.cc", "proto/tip/mail_error_tip.pb.h"], missing)
    cc = [f for f in files if f.endswith(".cc")]
    h = [f for f in files if f.endswith(".h")]
    e = ProjectEdit(base + "/CMakeLists.txt")
    e.plan(["    proto/tip/friend_error_tip.pb.cc"], [cmake_entry(p) for p in cc])
    edits.append(e)
    e = ProjectEdit(base + "/table.vcxproj")
    e.plan([include_line("proto/tip/friend_error_tip.pb.h")], [include_entry(p) for p in h])
    e.plan([compile_line("proto/tip/friend_error_tip.pb.cc")], [compile_entry(p) for p in cc])
    edits.append(e)
    e = ProjectEdit(base + "/table.vcxproj.filters")
    e.plan(filter_lines("ClInclude", "proto/tip/friend_error_tip.pb.h", HDR), [filter_entry("ClInclude", p, HDR) for p in h])
    e.plan(filter_lines("ClCompile", "proto/tip/friend_error_tip.pb.cc", SRC), [filter_entry("ClCompile", p, SRC) for p in cc])
    edits.append(e)


def plan_proto(edits: list[ProjectEdit], missing: list[str]) -> None:
    base = "cpp/generated/proto"
    stems = [f"mail/{name}{suffix}" for name in ("mail", "mail_admin", "mail_table") for suffix in (".pb",)]
    files = present_files(base, [s + ".cc" for s in stems] + [s + ".h" for s in stems], missing)
    cc = [f for f in files if f.endswith(".cc")]
    h = [f for f in files if f.endswith(".h")]
    friend_cc = ["friend/friend.pb.cc"]
    friend_h = [p[:-3] + ".h" for p in friend_cc]
    e = ProjectEdit(base + "/CMakeLists.txt")
    e.plan(["    " + p for p in friend_cc], [cmake_entry(p) for p in cc])
    edits.append(e)
    e = ProjectEdit(base + "/proto.vcxproj")
    e.plan([compile_line(p) for p in friend_cc], [compile_entry(p) for p in cc])
    e.plan([include_line(p) for p in friend_h], [include_entry(p) for p in h])
    edits.append(e)
    e = ProjectEdit(base + "/proto.vcxproj.filters")
    e.plan([l for p in friend_cc for l in filter_lines("ClCompile", p, SRC)], [filter_entry("ClCompile", p, SRC) for p in cc])
    e.plan([l for p in friend_h for l in filter_lines("ClInclude", p, HDR)], [filter_entry("ClInclude", p, HDR) for p in h])
    edits.append(e)


def plan_grpc_client(edits: list[ProjectEdit], missing: list[str]) -> None:
    base = "cpp/generated/grpc_client"
    stems = ["mail/mail_grpc_client", "mail/mail_admin_grpc_client"]
    files = present_files(base, [s + ".cpp" for s in stems] + [s + ".h" for s in stems], missing)
    cpp = [f for f in files if f.endswith(".cpp")]
    h = [f for f in files if f.endswith(".h")]
    e = ProjectEdit(base + "/CMakeLists.txt")
    e.plan(["    friend/friend_grpc_client.cpp"], [cmake_entry(p) for p in cpp])
    edits.append(e)
    for name in ("grpc_client.vcxproj", "grpc_client.vcxproj.filters"):   # 这个工程的 filters 也是不带 Filter 子元素的单行写法
        e = ProjectEdit(base + "/" + name)
        e.plan([compile_line("friend/friend_grpc_client.cpp")], [compile_entry(p) for p in cpp])
        e.plan([include_line("friend/friend_grpc_client.h")], [include_entry(p) for p in h])
        edits.append(e)


def plan_rpc(edits: list[ProjectEdit], missing: list[str]) -> None:
    base = "cpp/generated/rpc"
    h = present_files(base, ["service_metadata/mail_service_metadata.h", "service_metadata/mail_admin_service_metadata.h"],
                      missing)
    e = ProjectEdit(base + "/rpc.vcxproj")
    e.plan([include_line("service_metadata/friend_service_metadata.h")], [include_entry(p) for p in h])
    edits.append(e)
    e = ProjectEdit(base + "/rpc.vcxproj.filters")
    e.plan(filter_lines("ClInclude", "service_metadata/friend_service_metadata.h", HDR), [filter_entry("ClInclude", p, HDR) for p in h])
    edits.append(e)


def unexpected_mail_outputs() -> list[str]:
    """cpp/generated 下名字含 mail 的产物里,本脚本不认识的(生成器模板变了才会出现)。"""
    known = {
        "cpp/generated/table/proto/tip/mail_error_tip.pb.cc", "cpp/generated/table/proto/tip/mail_error_tip.pb.h",
        "cpp/generated/rpc/service_metadata/mail_service_metadata.h", "cpp/generated/rpc/service_metadata/mail_admin_service_metadata.h",
    }
    for stem in ("mail", "mail_admin", "mail_table"):
        for suffix in (".pb.cc", ".pb.h"):
            known.add(f"cpp/generated/proto/mail/{stem}{suffix}")
    for stem in ("mail", "mail_admin"):
        for suffix in ("_grpc_client.cpp", "_grpc_client.h"):
            known.add(f"cpp/generated/grpc_client/mail/{stem}{suffix}")
    found = []
    for p in (REPO / "cpp" / "generated").rglob("*"):
        if not p.is_file():
            continue
        parts = rel(p).split("/")
        # 只认 mail 目录下的文件与 mail_ / mail. 开头的文件名;bag_quest_mail_data 这类别的域里带 mail 字样的不算。
        is_mail = "mail" in parts[:-1] or p.name.startswith(("mail_", "mail."))
        if is_mail and rel(p) not in known:
            found.append(rel(p))
    return sorted(found)


def cmd_register_cpp(dry_run: bool, allow_missing: bool = False, allow_unknown: bool = False) -> None:
    """两阶段:先在内存里算出全部登记、核对缺文件 / 未知产物 / 锚点,全部通过才写盘 —— 中途失败不会留下半套登记。"""
    missing: list[str] = []
    edits: list[ProjectEdit] = []
    plan_table(edits, missing)
    plan_proto(edits, missing)
    plan_grpc_client(edits, missing)
    plan_rpc(edits, missing)

    stray = unexpected_mail_outputs()
    if stray:
        print("[WARN] cpp/generated contains mail outputs this script does not know (generator templates changed?):")
        for s in stray:
            print("  - " + s)
        if not allow_unknown:
            fail("有本脚本不认识的 mail 生成物(见上)—— 生成器换了产物形状;看清是否要登记、改好本脚本再跑。"
                 "确认它们不需要登记时加 --allow-unknown。未写任何文件。")
    if missing:
        msg = "以下期望的生成文件不存在:" + ", ".join(missing)
        if not allow_missing:
            fail(msg + " —— 全量 proto-gen 没跑,或生成器换了产物形状;看清后再改本脚本,不要手补文件。未写任何文件。")
        print("[SKIP] " + msg + "(--allow-missing:只登记已存在的文件)")

    print("register-cpp:")
    for e in edits:
        e.save(dry_run)


# ════════════════════════════════════════════════════════════════════════════
# vendor-robot
# ════════════════════════════════════════════════════════════════════════════

def cmd_vendor_robot(dry_run: bool) -> None:
    """先核对、后写:源目录与 vendor 目录的文件集合双向比对,任何一边有清单外的文件都在复制之前中止。"""
    src_dir = REPO / "go" / "proto" / "mail"
    dst_dir = REPO / "robot" / "vendor" / "proto" / "mail"
    pairs = [(src_dir / n, dst_dir / n) for n in GO_PROTO_FILES]
    pairs.append((REPO / "go" / "shared" / "generated" / "pb" / "table" / TIP_GO_FILE,
                  REPO / "robot" / "vendor" / "shared" / "generated" / "pb" / "table" / TIP_GO_FILE))
    for src, _dst in pairs:
        if not src.is_file():
            fail(f"缺 {rel(src)} —— 先跑导表与全量 proto-gen。未写任何文件。")
    src_extra = sorted(p.name for p in src_dir.glob("*.go") if p.name not in GO_PROTO_FILES)
    if src_extra:
        fail(f"go/proto/mail 里有本脚本清单之外的文件 {src_extra}(生成器多出了产物?)—— 先把它们加进 GO_PROTO_FILES"
             f" 再同步,否则 robot/vendor 会缺文件。未写任何文件。")
    dst_extra = sorted(p.name for p in dst_dir.glob("*.go") if p.name not in GO_PROTO_FILES) if dst_dir.is_dir() else []
    if dst_extra:
        fail(f"robot/vendor/proto/mail 里有 go/proto/mail 没有的文件 {dst_extra} —— 先查清再删。未写任何文件。")

    modules = REPO / "robot" / "vendor" / "modules.txt"
    text = modules.read_bytes().decode("utf-8")
    eol = "\r\n" if "\r\n" in text else "\n"
    lines = text.split(eol)
    added = False
    if "proto/mail" not in lines:
        try:
            start = next(i for i, l in enumerate(lines) if l.startswith("# proto v0.0.0"))
        except StopIteration:
            fail("robot/vendor/modules.txt 里找不到 `# proto v0.0.0 => ../go/proto` 模块段。未写任何文件。")
        i = start + 1
        while i < len(lines) and lines[i].startswith("## "):
            i += 1
        while i < len(lines) and lines[i].startswith("proto/") and lines[i] < "proto/mail":
            i += 1
        lines.insert(i, "proto/mail")
        added = True

    copied = 0
    for src, dst in pairs:
        if dst.is_file() and filecmp.cmp(src, dst, shallow=False):
            continue
        copied += 1
        if not dry_run:
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(src, dst)
    if added and not dry_run:
        modules.write_bytes(eol.join(lines).encode("utf-8"))
    print(f"vendor-robot: {'would copy' if dry_run else 'copied'} {copied} file(s);"
          f" modules.txt {'gets proto/mail' if added else 'already lists proto/mail'}.")


COMMANDS = ("verify", "register-cpp", "vendor-robot", "all")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=COMMANDS)
    parser.add_argument("--dry-run", action="store_true", help="只打印将要做什么,不写文件")
    parser.add_argument("--allow-missing", action="store_true",
                        help="register-cpp:期望的生成文件不存在时只登记已存在的(proto-gen 之前的阶段用)")
    parser.add_argument("--allow-unknown", action="store_true",
                        help="register-cpp:cpp/generated 下有本脚本不认识的 mail 产物时仍继续(确认它们不需要登记之后才用)")
    args = parser.parse_args()
    if args.command in ("verify", "all"):
        cmd_verify(args.dry_run)
    if args.command in ("register-cpp", "all"):
        cmd_register_cpp(args.dry_run, args.allow_missing, args.allow_unknown)
    if args.command in ("vendor-robot", "all"):
        cmd_vendor_robot(args.dry_run)


if __name__ == "__main__":
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")
    main()
