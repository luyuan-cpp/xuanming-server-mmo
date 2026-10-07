#!/usr/bin/env python3
"""生成 docs/README.md 里的文档索引(两个标记行之间的部分)。

用法(在仓库根目录):
    python tools/scripts/gen_docs_index.py           # 重写索引
    python tools/scripts/gen_docs_index.py --check   # 只比较;索引过期时退出码 1

标题取每篇文档的第一个 `#` 标题。docs/design 按文件名规则归入下面的分类,第一条命中的规则生效;
没有命中任何规则的文档列在“未分类”里——给它补一条规则,或者换一个符合既有前缀的文件名。
`x_en.md` / `x_zh.md` / `x-zh.md` 视为 `x.md` 的语言版本,合并在同一行。
"""
import fnmatch
import os
import re
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
DOCS = os.path.join(REPO_ROOT, "docs")
README = os.path.join(DOCS, "README.md")
BEGIN = "<!-- BEGIN GENERATED INDEX: tools/scripts/gen_docs_index.py -->"
END = "<!-- END GENERATED INDEX -->"

# docs/design 的分类规则:(分类名, 文件名通配)。自上而下第一条命中的生效,
# 所以更具体的规则要排在更宽的规则前面(例如 login-test-* 要先于 login-*)。
DESIGN_RULES = [
    ("总览与入门", [
        "ARCH.md", "onboarding.md", "architecture-current-state-vs-gaps-*", "ecs*.md",
        "table-api-reference*", "script_directory_rules*", "root_level_cleanup_findings*",
    ]),
    ("构建、部署与发布", [
        "docker_k8s_build_deploy*", "cpp_image_optimization*", "release-packaging-standard-*",
        "rolling-update-restart-resilience-tests.md", "vs2026_cross_machine_compatibility*",
        "scene_build_pdb_note*", "gateway-k8s-deployment.md",
    ]),
    ("测试与机器人", [
        "robot-*", "login-test-anti-stuck-system.md", "data-consistency-stress-testing.md",
    ]),
    ("玩家数据与存储", [
        "player-async-save-loss-windows.md", "player-storage-placement.md",
        "player_live_data_export_runbook.md", "data_service_role_and_scope.md", "db-*", "db_*",
        "global-data-layer-tidb-decision.md", "proto-compare-dirty-save.md",
        "async-load-disconnect-reconnect-race.md", "redis-async-client-retry-fix.md",
    ]),
    ("协议与配表工具链", [
        "proto-*", "proto3_*", "pbgen_notes*", "excel-6row-header-format*", "tip-code-axis.md",
        "generated-table-header-decoupling-plan.md",
    ]),
    ("ID、节点身份与寻址", [
        "snowflake-*", "node-*", "node_id_*", "id-routing-session-status-*",
        "routing-identity-audit-*", "grpc_node_entity_id_collision.md",
        "control-plane-topic-partitioning-*",
    ]),
    ("跨服、合服与回档", [
        "cross-server-*", "cross-zone-*", "cross_server_*", "mmo_cross_server_*",
        "server_merge_design.md", "server-merge-gap-fixes.md", "merge-zone-overhaul-*",
        "single_player_rollback*", "zone_data_rollback*", "microservice-zone-contract-*",
    ]),
    ("战斗", ["turn-*", "battle-*", "session-extractability-mmo-slg.md"]),
    ("接入层:网关、gate 与登录", [
        "gate-*", "gate_*", "client-access-band-routing.md", "client-rpc-router.md",
        "k8s-client-entry.md", "k8s_gate_exposure_guidance*", "login-*", "login_*",
        "player_login_flow*", "auth-provider-framework.md", "dual-token-authentication.md",
        "third-party-login-*", "hmac-message-signing.md", "open-server-rate-limit-design.md",
        "serverlist-static-publish-*", "java-gateway-portal-decision.md",
    ]),
    ("场景、AOI 与分线", [
        "scene-*", "aoi_*", "world-channel-*", "agones-scene-node-high-density.md",
        "enter-scene-zone-routing.md", "cross_scene_player_messaging*", "nav-spawn-fix-*",
        "broadcast-message-size-analysis.md", "afk-detection-design.md",
    ]),
    ("玩法系统", [
        "bag-*", "guild*", "friend-*", "team-system.md", "mail-system.md",
        "leaderboard-system.md", "jubaozhai-market.md", "chat-sensitive-word-filter.md",
        "character-appearance-identity.md", "activity_maintenance_auto_shift.md",
        "exploit_loss_prevention*", "player-attribute-allocation.md", "player-pet.md",
        "player-features-ui.md",
    ]),
    ("基础设施与中间件", [
        "kafka-*", "infra-reconnect-overview.md", "go-zero-rpc-timeout-fix.md",
        "grpc-client-deadline-failure-callback.md", "muduo-timer-cancellation-hazards.md",
        "hashed-timing-wheel.md", "double-buffer-queue-optimizations.md",
        "distributed-tracing.md", "error-reporting.md", "traffic-statistics-design.md",
        "thread-count-monitoring*",
    ]),
    ("移植记录", ["xuanming-port-*"]),
]
# 索引里各分类的展示顺序(与上面的匹配顺序无关)
DESIGN_ORDER = [
    "总览与入门", "接入层:网关、gate 与登录", "场景、AOI 与分线", "战斗", "玩法系统",
    "玩家数据与存储", "跨服、合服与回档", "ID、节点身份与寻址", "基础设施与中间件",
    "协议与配表工具链", "构建、部署与发布", "测试与机器人", "移植记录", "未分类",
]
OPS_RULES = [
    ("事故复盘", ["incident-*"]),
    ("运维手册", ["*runbook*", "release-checklist.md", "log-management.md",
                  "run-directory.md", "grafana-loki-local-logs.md",
                  "k8s-docker-desktop-troubleshooting*", "online-debug-data-fetch.md"]),
]
VARIANT_RE = re.compile(r"^(?P<base>.+?)(?P<sep>[_-])(?P<lang>en|zh)$")
LANG_LABEL = {"en": "EN", "zh": "中文"}


def first_heading(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                s = line.strip()
                if s.startswith("#"):
                    return s.lstrip("#").strip()
    except OSError:
        pass
    return ""


def md_files(rel_dir):
    """rel_dir 下一层的 .md 文件名(不含 README.md),按文件名排序。"""
    full = os.path.join(DOCS, rel_dir)
    if not os.path.isdir(full):
        return []
    return sorted(n for n in os.listdir(full)
                  if n.lower().endswith(".md") and n != "README.md"
                  and os.path.isfile(os.path.join(full, n)))


def group_variants(names):
    """把语言版本并到主文档上,返回 [(主文件名, {lang: 文件名})]。"""
    stems = {os.path.splitext(n)[0]: n for n in names}
    groups = {}
    for stem, name in stems.items():
        m = VARIANT_RE.match(stem)
        if m and (m.group("base") in stems or
                  any(m.group("base") + sep + other in stems
                      for sep in "_-" for other in LANG_LABEL if other != m.group("lang"))):
            groups.setdefault(m.group("base"), {})[m.group("lang")] = name
        else:
            groups.setdefault(stem, {})[""] = name
    out = []
    for base in sorted(groups, key=str.lower):
        langs = groups[base]
        # 没有不带后缀的主文档时(NOTES_en / NOTES_zh),中文版当主文档
        primary = langs.get("") or langs.get("zh") or langs.get("en")
        variants = {k: v for k, v in langs.items() if k and v != primary}
        out.append((primary, variants))
    return out


def escape(text):
    return text.replace("[", "\\[").replace("]", "\\]")


def entry(rel_dir, primary, variants):
    title = first_heading(os.path.join(DOCS, rel_dir, primary))
    line = f"- [{primary}]({rel_dir}/{primary})"
    if title:
        line += f" — {escape(title)}"
    for lang in ("en", "zh"):
        if lang in variants:
            line += f" · [{LANG_LABEL[lang]}]({rel_dir}/{variants[lang]})"
    return line


def classify(name, rules, default):
    for category, patterns in rules:
        if any(fnmatch.fnmatchcase(name, p) for p in patterns):
            return category
    return default


def section(lines, heading, rel_dir, rules=None, order=None, default="其他"):
    groups = group_variants(md_files(rel_dir))
    if not groups:
        return
    lines.append(f"### {heading}")
    lines.append("")
    if not rules:
        lines.extend(entry(rel_dir, p, v) for p, v in groups)
        lines.append("")
        return
    buckets = {}
    for primary, variants in groups:
        buckets.setdefault(classify(primary, rules, default), []).append((primary, variants))
    names = order or [c for c, _ in rules] + [default]
    for category in names:
        items = buckets.get(category)
        if not items:
            continue
        lines.append(f"#### {category}")
        lines.append("")
        lines.extend(entry(rel_dir, p, v) for p, v in items)
        lines.append("")


def subdirs(rel_dir):
    full = os.path.join(DOCS, rel_dir)
    return sorted(n for n in os.listdir(full) if os.path.isdir(os.path.join(full, n)))


def build():
    lines = []
    section(lines, "设计文档(`design/`)", "design", DESIGN_RULES, DESIGN_ORDER, "未分类")
    # 多篇成套的设计放在子目录里,索引只指向它的 README
    sets = [d for d in subdirs("design")
            if os.path.isfile(os.path.join(DOCS, "design", d, "README.md"))]
    if sets:
        lines.append("#### 成套设计")
        lines.append("")
        for d in sets:
            title = first_heading(os.path.join(DOCS, "design", d, "README.md"))
            lines.append(f"- [{d}/](design/{d}/README.md) — {escape(title)}")
        lines.append("")
    section(lines, "运维手册与事故复盘(`ops/`)", "ops", OPS_RULES, None, "其他")
    section(lines, "压测复盘(`stress/`)", "stress")
    section(lines, "交接说明(`handoff/`)", "handoff")
    section(lines, "笔记(`notes/`)", "notes")
    for d in subdirs("notes"):
        if md_files(f"notes/{d}"):
            section(lines, f"笔记(`notes/{d}/`)", f"notes/{d}")
    section(lines, "历史归档(`archive/`)", "archive")
    return "\n".join(lines).rstrip() + "\n"


def main():
    check = "--check" in sys.argv[1:]
    with open(README, encoding="utf-8", newline="") as f:
        text = f.read()
    if BEGIN not in text or END not in text:
        sys.exit(f"{README} 里找不到索引标记行,拒绝改写")
    head, rest = text.split(BEGIN, 1)
    _, tail = rest.split(END, 1)
    new_text = f"{head}{BEGIN}\n\n{build()}\n{END}{tail}"
    if new_text == text:
        print("docs/README.md 索引已是最新")
        return
    if check:
        sys.exit("docs/README.md 索引已过期: 请运行 python tools/scripts/gen_docs_index.py")
    with open(README, "w", encoding="utf-8", newline="") as f:
        f.write(new_text)
    print("已更新 docs/README.md 索引")


if __name__ == "__main__":
    main()
