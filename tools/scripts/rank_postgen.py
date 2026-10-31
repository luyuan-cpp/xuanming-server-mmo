#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""排行榜 rank(docs/design/leaderboard-system.md §5.3 / §7 R1p)在**全量 proto-gen 之后**要做的收尾,写成可重复执行的脚本。

与 tools/scripts/mail_postgen.py 同一个问题、同一套子命令约定(dev_tools.ps1 的 $ProtoGenPostHooks 按这个约定调用):
  - 生成器不会把新域的 C++ 产物登记进工程文件(CMakeLists.txt / *.vcxproj / *.vcxproj.filters):rank.proto 是新域,
    生成器照常给它出 C++ 的 pb / grpc_client / service_metadata,rpc_event_registry 与 grpc_init_client 会引用它们,
    漏登记的后果是 msbuild 全量链接期才报 LNK2001 / LNK2019;
  - robot 走 vendor(-mod=vendor):go/proto/rank 是新包,不同步进 robot/vendor 并在 modules.txt 登记,生成器给 robot 出的
    ClientPlayerRank handler 桩就 import 不到它;
  - 生成结果有几条硬性验收(方法号齐全且已有方法号不变、路由表里客户端方法与 admin 方法分开准入、目标节点 RankNodeService),机械核对。

通用的工程文件登记机制(两阶段写盘、按原 BOM / 换行写回、逐条判断是否已登记、以 friend 的条目为锚点)在 mail_postgen.py 里,
本文件只描述 rank 域的产物清单,不另写第二份机制。rank 没有 tip 段(v1 不开 21000,设计 §4.5),所以没有 table 工程的登记。

    python tools/scripts/rank_postgen.py verify          # 只读:核对 proto-gen 的产物
    python tools/scripts/rank_postgen.py register-cpp    # 把 rank 的 C++ 产物登记进 proto / grpc_client / rpc 三个工程
    python tools/scripts/rank_postgen.py vendor-robot    # go/proto/rank 按字节同步进 robot/vendor
    python tools/scripts/rank_postgen.py all             # 依次跑上面三个

都幂等、都支持 --dry-run;register-cpp 的 --allow-missing / --allow-unknown 语义与 mail_postgen.py 相同。
"""

from __future__ import annotations

import argparse
import filecmp
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from mail_postgen import (  # noqa: E402  通用机制,见文件头
    HDR,
    REPO,
    SRC,
    ProjectEdit,
    cmake_entry,
    committed_message_ids,
    compile_entry,
    compile_line,
    fail,
    filter_entry,
    filter_lines,
    include_entry,
    include_line,
    load_message_ids,
    present_files,
    rel,
)

CLIENT_METHODS = ("GetRankPage", "GetRankAround")
ADMIN_METHODS = ("OpenRankGeneration", "SubmitRankScores", "FreezeRankGeneration", "RemoveRankEntry", "DeleteRankGeneration")
PROTO_STEMS = ("rank", "rank_admin", "rank_table")
GRPC_CLIENT_STEMS = ("rank", "rank_admin")
GO_PROTO_FILES = ("rank.pb.go", "rank_grpc.pb.go", "rank_admin.pb.go", "rank_admin_grpc.pb.go", "rank_table.pb.go")
TIP_GO_FILE = "rank_error_tip.pb.go"


# ════════════════════════════════════════════════════════════════════════════
# verify
# ════════════════════════════════════════════════════════════════════════════

def cmd_verify(_dry_run: bool) -> None:
    problems: list[str] = []
    ids = load_message_ids()
    wanted = ["ClientPlayerRank" + m for m in CLIENT_METHODS] + ["RankAdmin" + m for m in ADMIN_METHODS]
    missing = [k for k in wanted if k not in ids]
    if missing:
        problems.append(f"proto/message_id.txt 缺 {missing}(含 rank 的全量 proto-gen 没跑或没跑完)")
    extra = sorted(k for k in ids if k.startswith("ClientPlayerRank") and k not in wanted)
    if extra:
        problems.append(f"proto/message_id.txt 多出 {extra}(rank.proto 的方法与本脚本的清单不一致)")
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
        if "ENodeType_RankNodeService" not in line:
            problems.append(f"{rel(route)} 的 {key} 目标节点不是 RankNodeService(proto 目录必须叫 proto/rank/,设计 §4.1):{line.strip()}")
        client_protocol = "true" if key.startswith("ClientPlayerRank") else "false"
        if f"ClientProtocol: {client_protocol}" not in line:
            problems.append(f"{rel(route)} 的 {key} 应为 ClientProtocol: {client_protocol}:{line.strip()}")

    go_dir = REPO / "go" / "proto" / "rank"
    for name in GO_PROTO_FILES:
        if not (go_dir / name).is_file():
            problems.append(f"缺 {rel(go_dir / name)}")
    if (REPO / "go" / "rank" / "go.mod").is_file():
        p = REPO / "go" / "rank" / "generated" / "pb" / "game" / "message_id.go"
        if not p.is_file():
            problems.append(f"缺 {rel(p)}")

    if problems:
        print("[FAIL] proto-gen output check (rank):")
        for p in problems:
            print("  - " + p)
        sys.exit(1)
    print("[OK] rank message IDs: " + ", ".join(f"{k}={ids[k]}" for k in wanted))
    print("[OK] route table: client and admin methods target RankNodeService with separate admission; Go bindings present.")


# ════════════════════════════════════════════════════════════════════════════
# register-cpp
# ════════════════════════════════════════════════════════════════════════════

def plan_table(edits: list[ProjectEdit], missing: list[str]) -> None:
    base = "cpp/generated/table"
    cc = present_files(base, ["proto/tip/rank_error_tip.pb.cc"], missing)
    h = present_files(base, ["proto/tip/rank_error_tip.pb.h"], missing)
    e = ProjectEdit(base + "/CMakeLists.txt")
    e.plan(["    proto/tip/friend_error_tip.pb.cc"], [cmake_entry(p) for p in cc])
    edits.append(e)
    e = ProjectEdit(base + "/table.vcxproj")
    e.plan([compile_line("proto/tip/friend_error_tip.pb.cc")], [compile_entry(p) for p in cc])
    e.plan([include_line("proto/tip/friend_error_tip.pb.h")], [include_entry(p) for p in h])
    edits.append(e)
    e = ProjectEdit(base + "/table.vcxproj.filters")
    e.plan(filter_lines("ClCompile", "proto/tip/friend_error_tip.pb.cc", SRC), [filter_entry("ClCompile", p, SRC) for p in cc])
    e.plan(filter_lines("ClInclude", "proto/tip/friend_error_tip.pb.h", HDR), [filter_entry("ClInclude", p, HDR) for p in h])
    edits.append(e)


def plan_proto(edits: list[ProjectEdit], missing: list[str]) -> None:
    base = "cpp/generated/proto"
    stems = [f"rank/{name}{suffix}" for name in PROTO_STEMS for suffix in (".pb",)]
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
    stems = [f"rank/{name}_grpc_client" for name in GRPC_CLIENT_STEMS]
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
    h = present_files(base, [f"service_metadata/{name}_service_metadata.h" for name in GRPC_CLIENT_STEMS], missing)
    e = ProjectEdit(base + "/rpc.vcxproj")
    e.plan([include_line("service_metadata/friend_service_metadata.h")], [include_entry(p) for p in h])
    edits.append(e)
    e = ProjectEdit(base + "/rpc.vcxproj.filters")
    e.plan(filter_lines("ClInclude", "service_metadata/friend_service_metadata.h", HDR), [filter_entry("ClInclude", p, HDR) for p in h])
    edits.append(e)


def unexpected_rank_outputs() -> list[str]:
    """cpp/generated 下属于 rank 域、但本脚本不认识的产物(生成器模板变了才会出现)。"""
    known = {f"cpp/generated/rpc/service_metadata/{name}_service_metadata.h" for name in GRPC_CLIENT_STEMS}
    known.update({"cpp/generated/table/proto/tip/rank_error_tip.pb.cc", "cpp/generated/table/proto/tip/rank_error_tip.pb.h"})
    for name in PROTO_STEMS:
        for suffix in (".pb.cc", ".pb.h"):
            known.add(f"cpp/generated/proto/rank/{name}{suffix}")
    for name in GRPC_CLIENT_STEMS:
        for suffix in ("_grpc_client.cpp", "_grpc_client.h"):
            known.add(f"cpp/generated/grpc_client/rank/{name}{suffix}")
    found = []
    for p in (REPO / "cpp" / "generated").rglob("*"):
        if not p.is_file():
            continue
        parts = rel(p).split("/")
        # 只认 rank 目录下的文件与 rank_ / rank. 开头的文件名;guild_rank 这类别的域里带 rank 字样的不算。
        is_rank = "rank" in parts[:-1] or p.name.startswith(("rank_", "rank."))
        if is_rank and rel(p) not in known:
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

    stray = unexpected_rank_outputs()
    if stray:
        print("[WARN] cpp/generated contains rank outputs this script does not know (generator templates changed?):")
        for s in stray:
            print("  - " + s)
        if not allow_unknown:
            fail("有本脚本不认识的 rank 生成物(见上)—— 生成器换了产物形状;看清是否要登记、改好本脚本再跑。"
                 "确认它们不需要登记时加 --allow-unknown。未写任何文件。")
    if missing:
        msg = "以下期望的生成文件不存在:" + ", ".join(missing)
        if not allow_missing:
            fail(msg + " —— 全量 proto-gen 没跑,或生成器换了产物形状;看清后再改本脚本,不要手补文件。未写任何文件。")
        print("[SKIP] " + msg + "(--allow-missing:只登记已存在的文件)")

    print("register-cpp (rank):")
    for e in edits:
        e.save(dry_run)


# ════════════════════════════════════════════════════════════════════════════
# vendor-robot
# ════════════════════════════════════════════════════════════════════════════

def cmd_vendor_robot(dry_run: bool) -> None:
    """先核对、后写:源目录与 vendor 目录的文件集合双向比对,任何一边有清单外的文件都在复制之前中止。"""
    src_dir = REPO / "go" / "proto" / "rank"
    dst_dir = REPO / "robot" / "vendor" / "proto" / "rank"
    pairs = [(src_dir / n, dst_dir / n) for n in GO_PROTO_FILES]
    pairs.append((REPO / "go" / "shared" / "generated" / "pb" / "table" / TIP_GO_FILE,
                  REPO / "robot" / "vendor" / "shared" / "generated" / "pb" / "table" / TIP_GO_FILE))
    for src, _dst in pairs:
        if not src.is_file():
            fail(f"缺 {rel(src)} —— 先跑全量 proto-gen。未写任何文件。")
    src_extra = sorted(p.name for p in src_dir.glob("*.go") if p.name not in GO_PROTO_FILES)
    if src_extra:
        fail(f"go/proto/rank 里有本脚本清单之外的文件 {src_extra}(生成器多出了产物?)—— 先把它们加进 GO_PROTO_FILES"
             f" 再同步,否则 robot/vendor 会缺文件。未写任何文件。")
    dst_extra = sorted(p.name for p in dst_dir.glob("*.go") if p.name not in GO_PROTO_FILES) if dst_dir.is_dir() else []
    if dst_extra:
        fail(f"robot/vendor/proto/rank 里有 go/proto/rank 没有的文件 {dst_extra} —— 先查清再删。未写任何文件。")

    modules = REPO / "robot" / "vendor" / "modules.txt"
    text = modules.read_bytes().decode("utf-8")
    eol = "\r\n" if "\r\n" in text else "\n"
    lines = text.split(eol)
    added = False
    if "proto/rank" not in lines:
        try:
            start = next(i for i, l in enumerate(lines) if l.startswith("# proto v0.0.0"))
        except StopIteration:
            fail("robot/vendor/modules.txt 里找不到 `# proto v0.0.0 => ../go/proto` 模块段。未写任何文件。")
        i = start + 1
        while i < len(lines) and lines[i].startswith("## "):
            i += 1
        while i < len(lines) and lines[i].startswith("proto/") and lines[i] < "proto/rank":
            i += 1
        lines.insert(i, "proto/rank")
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
    print(f"vendor-robot (rank): {'would copy' if dry_run else 'copied'} {copied} file(s);"
          f" modules.txt {'gets proto/rank' if added else 'already lists proto/rank'}.")


COMMANDS = ("verify", "register-cpp", "vendor-robot", "all")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", choices=COMMANDS)
    parser.add_argument("--dry-run", action="store_true", help="只打印将要做什么,不写文件")
    parser.add_argument("--allow-missing", action="store_true",
                        help="register-cpp:期望的生成文件不存在时只登记已存在的(proto-gen 之前的阶段用)")
    parser.add_argument("--allow-unknown", action="store_true",
                        help="register-cpp:cpp/generated 下有本脚本不认识的 rank 产物时仍继续(确认它们不需要登记之后才用)")
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
