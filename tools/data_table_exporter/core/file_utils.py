"""File I/O utilities: write, MD5-based copy, directory helpers."""

from __future__ import annotations

import hashlib
import logging
import os
import shutil
from pathlib import Path

logger = logging.getLogger(__name__)


def write_file(path: Path | str, content: str) -> None:
    """Write *content* to *path*, creating parent directories as needed."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="") as f:
        f.write(content)


def write_file_bytes(path: Path | str, data: bytes) -> None:
    """Write binary *data* to *path*, creating parent directories as needed."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)


def md5_hash(file_path: Path | str, block_size: int = 2 ** 20) -> str | None:
    """Return the hex MD5 digest of a file, or ``None`` on error."""
    try:
        md5 = hashlib.md5()
        with open(file_path, "rb") as f:
            while chunk := f.read(block_size):
                md5.update(chunk)
        return md5.hexdigest()
    except Exception as exc:
        logger.error("MD5 error for %s: %s", file_path, exc)
        return None


def md5_copy_file(src: Path, dst: Path) -> None:
    """Copy *src* → *dst* only when content differs (by MD5)."""
    dst.parent.mkdir(parents=True, exist_ok=True)
    if dst.exists() and md5_hash(src) == md5_hash(dst):
        return
    shutil.copyfile(str(src), str(dst))
    logger.debug("Copied %s → %s", src, dst)


def md5_copy_dir(src_dir: Path, dst_dir: Path) -> None:
    """Recursively copy *src_dir* → *dst_dir*, skipping unchanged files."""
    src_dir = Path(src_dir)
    dst_dir = Path(dst_dir)
    if not src_dir.exists():
        return
    dst_dir.mkdir(parents=True, exist_ok=True)
    for item in src_dir.iterdir():
        dst_item = dst_dir / item.name
        if item.is_dir():
            md5_copy_dir(item, dst_item)
        elif item.is_file():
            md5_copy_file(item, dst_item)


def md5_copy(src: str | Path, dst: str | Path) -> None:
    """Smart dispatcher: copy file or directory with MD5 change detection."""
    src, dst = Path(src), Path(dst)
    if src.is_file():
        md5_copy_file(src, dst)
    elif src.is_dir():
        md5_copy_dir(src, dst)
    else:
        logger.warning("md5_copy: source does not exist: %s", src)


# 手写文件白名单:这些文件本就该只存在于部署目标树、源树里没有,不算陈旧产物。
# 路径是相对目标目录的 posix 形式。新增手写文件必须登记在这里,否则每次导表都会告警。
_DEPLOY_KEEP: frozenset[str] = frozenset({
    "table/table_test.go",  # 手写的表加载冒烟测试
})

# 只对这些后缀查陈旧产物(编译单元;.json/.pb 等数据产物由 manifest 与业务自行管理)。
_ORPHAN_SUFFIXES: frozenset[str] = frozenset({".go", ".h", ".hpp", ".cpp", ".java"})


def report_orphans(src_dirs: list[Path] | tuple[Path, ...], dst_dir: Path | str) -> list[Path]:
    """报告部署目标树里「所有源树都已经没有、但目标树还留着」的生成产物。

    src_dirs 必须是**部署到同一个 dst_dir 的全部源目录**,不能逐对调用:
    exporter_config.yaml 里 Java 就有两个不同的 src 部署到同一个
    `java/config_node/src/main/java/com/game/table`(代码树 + proto 产物树),
    逐对判定会把另一对的产物全报成陈旧。

    md5_copy_dir 只覆盖同名文件、**从不删除多余文件**。这带来一类会反复发生的事故:
    导表器一改输出布局(重命名、换目录、某张表下线),旧产物就永久留在目标树里,
    而且看起来和新产物一模一样。真实案例——Go 位序常量曾从平铺改为按包分子目录,
    平铺副本没人删,导致 `go/shared` 整模块 `go build ./...` 直接失败
    (一个目录两个 package),而各服务模块因为没 import 它反而是绿的,坑一直藏到 CI 全模块构建。

    **只报告不删除**:目标树里合法地混着手写文件(见 _DEPLOY_KEEP),自动删除的爆炸半径
    远大于收益。返回可疑文件列表供调用方打日志 / 在 CI 里升级为失败。
    """
    dst_dir = Path(dst_dir)
    sources = [Path(s) for s in src_dirs if Path(s).is_dir()]
    if not sources or not dst_dir.is_dir():
        return []

    orphans: list[Path] = []
    for dst_file in sorted(dst_dir.rglob("*")):
        if not dst_file.is_file() or dst_file.suffix not in _ORPHAN_SUFFIXES:
            continue
        rel = dst_file.relative_to(dst_dir)
        if rel.as_posix() in _DEPLOY_KEEP:
            continue
        if not any((src / rel).exists() for src in sources):
            orphans.append(dst_file)
    return orphans


def ensure_dirs(*dirs: Path | str) -> None:
    """Create directories if they do not already exist."""
    for d in dirs:
        Path(d).mkdir(parents=True, exist_ok=True)


def list_xlsx(directory: Path) -> list[Path]:
    """Return all *.xlsx* files directly under *directory*."""
    if not directory.exists():
        return []
    return sorted(f for f in directory.iterdir() if f.is_file() and f.suffix == ".xlsx")
