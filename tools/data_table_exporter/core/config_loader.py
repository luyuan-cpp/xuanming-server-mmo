"""Configuration loader.

Reads ``exporter_config.yaml`` and resolves all paths relative to the config
file's directory so that every other module receives absolute ``Path`` objects.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

logger = logging.getLogger(__name__)


@dataclass
class LangConfig:
    """Per-language generation settings."""
    enabled: bool = False
    code_dir: Path = field(default_factory=Path)
    proto_output_dir: Path = field(default_factory=Path)
    proto_import_path: str = ""
    constants_dir: Path = field(default_factory=Path)
    table_id_dir: Path = field(default_factory=Path)
    bit_index_dir: Path = field(default_factory=Path)
    package: str = ""
    deploy: list[dict[str, Path]] = field(default_factory=list)
    grpc_service_scan_dir: Path = field(default_factory=Path)
    grpc_deploy_base: Path = field(default_factory=Path)


@dataclass
class ExporterConfig:
    """Typed representation of *exporter_config.yaml*."""

    # Excel
    data_dir: Path = field(default_factory=Path)
    operator_file: Path = field(default_factory=Path)
    tip_file: Path = field(default_factory=Path)
    data_begin_row: int = 7
    metadata_rows: dict[str, int] = field(default_factory=dict)
    server_owner_types: list[str] = field(default_factory=lambda: ["server", "common"])

    # Output
    generated_dir: Path = field(default_factory=Path)
    json_dir: Path = field(default_factory=Path)
    binary_dir: Path = field(default_factory=Path)
    proto_dir: Path = field(default_factory=Path)
    proto_python_output_dir: Path = field(default_factory=Path)
    state_dir: Path = field(default_factory=Path)

    # Protoc
    protoc_command: str = "protoc"
    protoc_extra_includes: list[Path] = field(default_factory=list)

    # Languages
    cpp: LangConfig = field(default_factory=LangConfig)
    go: LangConfig = field(default_factory=LangConfig)
    java: LangConfig = field(default_factory=LangConfig)

    # Misc
    constant_tables: list[str] = field(default_factory=list)
    template_dir: Path = field(default_factory=Path)
    config_root: Path = field(default_factory=Path)

    # 位序状态从空初始化的开关。**只能由命令行 --bitindex-bootstrap 打开**,
    # 刻意不放进 yaml:配置文件里一旦写上就等于永久关掉了位序状态的 fail-closed 保护
    # (见 core/generators/bit_index_gen.py 的红线说明)。
    bit_index_bootstrap: bool = False


def _resolve(base: Path, raw: Any) -> Path:
    if raw is None:
        return Path()
    return (base / str(raw)).resolve()


def _build_lang(base: Path, raw: dict) -> LangConfig:
    deploy_list = []
    for item in raw.get("deploy", []):
        deploy_list.append({
            "src": _resolve(base, item.get("src")),
            "dst": _resolve(base, item.get("dst")),
        })
    return LangConfig(
        enabled=raw.get("enabled", False),
        code_dir=_resolve(base, raw.get("code_dir")),
        proto_output_dir=_resolve(base, raw.get("proto_output_dir")),
        proto_import_path=raw.get("proto_import_path", ""),
        constants_dir=_resolve(base, raw.get("constants_dir")),
        table_id_dir=_resolve(base, raw.get("table_id_dir")),
        bit_index_dir=_resolve(base, raw.get("bit_index_dir")),
        package=raw.get("package", ""),
        deploy=deploy_list,
        grpc_service_scan_dir=_resolve(base, raw.get("grpc_service_scan_dir")),
        grpc_deploy_base=_resolve(base, raw.get("grpc_deploy_base")),
    )


def load_config(config_path: str | Path | None = None) -> ExporterConfig:
    """Load and resolve the YAML configuration file."""
    if config_path is None:
        config_path = Path(__file__).resolve().parent.parent / "exporter_config.yaml"
    config_path = Path(config_path).resolve()
    base = config_path.parent

    with open(config_path, "r", encoding="utf-8") as f:
        raw = yaml.safe_load(f)

    excel = raw.get("excel", {})
    output = raw.get("output", {})
    protoc = raw.get("protoc", {})
    langs = raw.get("languages", {})

    cfg = ExporterConfig(
        data_dir=_resolve(base, excel.get("data_dir")),
        operator_file=_resolve(base, excel.get("operator_file")),
        tip_file=_resolve(base, excel.get("tip_file")),
        data_begin_row=excel.get("data_begin_row", 7),
        metadata_rows=excel.get("metadata_rows", {}),
        server_owner_types=excel.get("server_owner_types", ["server", "common"]),

        generated_dir=_resolve(base, output.get("generated_dir")),
        json_dir=_resolve(base, output.get("json_dir")),
        binary_dir=_resolve(base, output.get("binary_dir", output.get("json_dir", ""))),
        proto_dir=_resolve(base, output.get("proto_dir")),
        proto_python_output_dir=_resolve(base, output.get("proto_python_output_dir", "../../generated/code/proto/python")),
        state_dir=_resolve(base, output.get("state_dir")),

        protoc_command=protoc.get("command", "protoc"),
        protoc_extra_includes=[_resolve(base, p) for p in protoc.get("extra_includes", [])],

        cpp=_build_lang(base, langs.get("cpp", {})),
        go=_build_lang(base, langs.get("go", {})),
        java=_build_lang(base, langs.get("java", {})),

        constant_tables=raw.get("constant_tables", []),
        template_dir=(base / "templates").resolve(),
        config_root=base,
    )
    logger.info("Config loaded from %s", config_path)
    return cfg
