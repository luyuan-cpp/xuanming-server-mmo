#!/usr/bin/env python3
"""在沙盒里跑一次完整导表,并把产物与仓内现有产物逐字节比对。

这是所有导表器改造的**验收判据**:改之前跑一次拿基线,改之后再跑一次,
除了 manifest 的 version / generated_at / source_rev(设计上就排除在内容指纹外),
其余产物必须逐字节相同。

为什么要沙盒:``data/*.xlsx`` 是二进制,``git diff`` 只会说 "Binary files differ";
而直接跑 ``dev.bat gen`` 会把结果写进 ``generated/`` 与三端部署目录,
和别人正在进行的改动混在一起就再也分不清了。沙盒跑法对仓库是**只读**的。

沙盒里被重定向的东西::

    output.*                 -> <out>/generated/
    languages.*.code_dir 等  -> <out>/generated/code/...
    languages.*.deploy[].dst -> <out>/deploy/...
    output.state_dir         -> <out>/state/  (先从仓里整树复制;位序与 tip 码轴读真权威、写只写副本)
    templates/               -> <out>/templates/  (config_loader 按配置文件所在目录找模板)

仍然读真实内容的:``data/`` 下的全部源表。

用法::

    py tools/data_table_exporter/tools/sandbox_export.py --out baseline      # 跑一次
    py tools/data_table_exporter/tools/sandbox_export.py --out after --compare
    py tools/data_table_exporter/tools/sandbox_export.py --out after --compare --against baseline
"""

from __future__ import annotations

import argparse
import hashlib
import os
import shutil
import subprocess
import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent                       # tools/data_table_exporter
_REPO = _ROOT.parent.parent                # 仓根
if str(_ROOT) not in sys.path:
    sys.path.insert(0, str(_ROOT))

import yaml                                                        # noqa: E402

# 仓内自带的 protoc。dev.bat :prepare_protoc 要求恰好 libprotoc 35.1,这里沿用同一个。
_REPO_PROTOC = _REPO / "third_party" / "grpc" / "install_vs2026_dbg" / "bin" / "protoc.exe"

# manifest 里这几个字段本来就与内容无关(见 core/manifest.py 的 content_digest 口径),
# 每次跑都会变,不能算差异。比对时改为核对 content_digest 与 tables 段。
_MANIFEST_VOLATILE = ("version", "generated_at", "source_rev")


# ---------------------------------------------------------------------------
# 沙盒配置
# ---------------------------------------------------------------------------

def build_sandbox_config(out_dir: Path, protoc: Path | None = None) -> Path:
    src = (_ROOT / "exporter_config.yaml").read_text(encoding="utf-8")
    raw = yaml.safe_load(src)
    orig = yaml.safe_load(src)

    def absolute(rel) -> str:
        return str((_ROOT / str(rel)).resolve())

    for key in ("data_dir", "operator_file", "tip_file"):
        if raw["excel"].get(key):
            raw["excel"][key] = absolute(raw["excel"][key])

    state_copy = out_dir / "state"
    if state_copy.exists():
        shutil.rmtree(state_copy)
    shutil.copytree((_ROOT / raw["output"]["state_dir"]).resolve(), state_copy)

    gen = out_dir / "generated"
    raw["output"] = {
        "generated_dir": str(gen),
        "json_dir": str(gen / "tables"),
        "binary_dir": str(gen / "tables"),
        "proto_dir": str(gen / "code" / "proto"),
        "proto_python_output_dir": str(gen / "code" / "proto" / "python"),
        "state_dir": str(state_copy),
    }

    exe = protoc or (_REPO_PROTOC if _REPO_PROTOC.exists() else None)
    if exe:
        raw["protoc"]["command"] = str(exe)
    raw["protoc"]["extra_includes"] = [absolute(p)
                                       for p in raw["protoc"].get("extra_includes", [])]

    deploy_root = out_dir / "deploy"
    # 按 yaml 里实际有哪些语言来重定向,不写死 —— 写死过一次:加了 csharp/python 之后
    # 沙盒会把这两门语言的产物写进**真实仓库目录**,直接破坏「对仓库只读」这条契约。
    for lang in sorted(orig.get("languages", {})):
        cur, old = raw["languages"].get(lang), orig["languages"].get(lang)
        if not cur:
            continue
        for key in ("code_dir", "proto_output_dir", "constants_dir",
                    "table_id_dir", "bit_index_dir"):
            rel = old.get(key)
            if rel:
                cur[key] = str(gen / Path(str(rel)).as_posix().split("generated/", 1)[-1])
        for i, item in enumerate(cur.get("deploy") or []):
            osrc = Path(str(old["deploy"][i]["src"])).as_posix()
            odst = Path(str(old["deploy"][i]["dst"])).as_posix().replace("../", "")
            item["src"] = str(gen / osrc.split("generated/", 1)[-1])
            item["dst"] = str(deploy_root / odst)

    # config_loader 用「配置文件所在目录/templates」找模板,所以模板要跟着搬。
    tpl = out_dir / "templates"
    if tpl.exists():
        shutil.rmtree(tpl)
    shutil.copytree(_ROOT / "templates", tpl)

    cfg = out_dir / "exporter_config.sandbox.yaml"
    cfg.parent.mkdir(parents=True, exist_ok=True)
    cfg.write_text(yaml.safe_dump(raw, allow_unicode=True, sort_keys=False), encoding="utf-8")
    return cfg


# ---------------------------------------------------------------------------
# 快照与比对
# ---------------------------------------------------------------------------

def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def snapshot(root: Path) -> dict[str, str]:
    out: dict[str, str] = {}
    if not root.exists():
        return out
    for dirpath, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if d != "__pycache__"]
        for fn in files:
            p = Path(dirpath) / fn
            out[p.relative_to(root).as_posix()] = sha256(p)
    return out


def _manifest_equivalent(a: Path, b: Path) -> bool:
    """manifest 只比内容相关字段:content_digest 与 tables 段。"""
    import json
    try:
        ja = json.loads(a.read_text(encoding="utf-8"))
        jb = json.loads(b.read_text(encoding="utf-8"))
    except Exception:
        return False
    for k in _MANIFEST_VOLATILE:
        ja.pop(k, None)
        jb.pop(k, None)
    return ja == jb


def compare(sandbox_gen: Path, repo_gen: Path, limit: int = 30) -> int:
    """比对沙盒产出与仓内产物。

    只比对**沙盒里也存在同名目录**的那些路径 —— 这样既能自动排除别的生成器写在
    ``generated/`` 下的树(``proto/_unified/``、``proto/db/``、``data/``),
    又不会放过导表器自己目录里的陈旧残留。
    """
    sb = snapshot(sandbox_gen)
    repo_all = snapshot(repo_gen)
    sandbox_dirs = {str(Path(k).parent.as_posix()) for k in sb}
    repo = {k: v for k, v in repo_all.items()
            if str(Path(k).parent.as_posix()) in sandbox_dirs}

    only_repo = sorted(set(repo) - set(sb))
    only_sb = sorted(set(sb) - set(repo))
    diff = []
    for k in sorted(set(repo) & set(sb)):
        if repo[k] == sb[k]:
            continue
        if Path(k).name == "manifest.json" and _manifest_equivalent(repo_gen / k, sandbox_gen / k):
            print("  manifest.json:仅 %s 不同(设计如此),content_digest 与 tables 段相同"
                  % "/".join(_MANIFEST_VOLATILE))
            continue
        diff.append(k)

    print("\n=== 产物对拍 ===")
    print("  纳入比对 %d 个文件(已按沙盒目录范围过滤,忽略 __pycache__)"
          % len(set(repo) | set(sb)))
    print("  相同 %d / 仅仓内(疑似陈旧残留) %d / 仅沙盒(新增) %d / 内容不同 %d"
          % (len(set(repo) & set(sb)) - len(diff), len(only_repo), len(only_sb), len(diff)))
    for tag, items in (("陈旧残留?", only_repo), ("新增", only_sb), ("内容不同", diff)):
        for k in items[:limit]:
            print("    [%s] %s" % (tag, k))
        if len(items) > limit:
            print("    [%s] ...(还有 %d 个)" % (tag, len(items) - limit))
    return len(only_repo) + len(only_sb) + len(diff)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="沙盒导表 + 产物对拍(对仓库只读)")
    ap.add_argument("--out", required=True,
                    help="沙盒目录。相对路径按当前工作目录解析")
    ap.add_argument("--compare", action="store_true", help="跑完后与仓内 generated/ 比对")
    ap.add_argument("--against", default=None,
                    help="改为与另一个沙盒目录比对(给出该目录,不是它下面的 generated/)")
    ap.add_argument("--protoc", default=None, help="protoc 路径,缺省用仓内自带的那个")
    ap.add_argument("--skip-run", action="store_true", help="不跑导表,只比对已有产物")
    args = ap.parse_args(argv)

    out_dir = Path(args.out).resolve()
    protoc = Path(args.protoc) if args.protoc else None
    if not args.skip_run:
        cfg = build_sandbox_config(out_dir, protoc)
        print("沙盒配置:", cfg)
        print("protoc  :", protoc or _REPO_PROTOC)
        rc = subprocess.run([sys.executable, str(_ROOT / "run.py"), str(cfg)]).returncode
        if rc != 0:
            print("导表失败,退出码 %d" % rc)
            return rc

    if args.compare or args.against:
        base = (Path(args.against).resolve() / "generated") if args.against \
            else (_REPO / "generated")
        n = compare(out_dir / "generated", base)
        print("\n差异合计:%d" % n)
        return 1 if n else 0
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
