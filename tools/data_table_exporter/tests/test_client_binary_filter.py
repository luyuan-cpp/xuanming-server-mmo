"""客户端专用二进制配置表按 owner 裁剪测试。

验证 ``generate_client_binary`` 在序列化前清除所有 ``owner`` 不属于
``client_owner_types``（如 ``server``）的标量、数组、map 与子消息字段，
且全列均为 ``server`` 的私密表导出 0 行空表，保证服务端敏感列不会以明文
字节落进客户端包体，同时服务端 ``binary_dir`` 原样保留。
"""

from __future__ import annotations

import importlib
import sys
from pathlib import Path

import pytest

from conftest import Field, TableSpec, write_xlsx
from core.excel_reader import read_all_tables
from core.generators.binary_gen import generate_binary, generate_client_binary
from core.generators.json_gen import generate_json
from core.generators.proto_gen import compile_proto_python, generate_proto_files


def _repo_protoc() -> str | None:
    repo_root = Path(__file__).resolve().parents[3]
    cand = repo_root / "third_party" / "protobuf" / "install_vs2026_dbg" / "bin" / "protoc.exe"
    return str(cand) if cand.exists() else None


def _compile_and_export(cfg):
    protoc = _repo_protoc()
    if protoc:
        cfg.protoc_command = protoc
    cfg.client_binary_dir = cfg.generated_dir / "client_tables"
    tables = read_all_tables(cfg)
    generate_json(cfg, tables)
    generate_proto_files(cfg, tables)
    compile_proto_python(cfg)
    generate_binary(cfg, tables)
    generate_client_binary(cfg, tables)
    return tables


def test_client_binary_strips_server_only_fields_on_wire(cfg):
    write_xlsx(cfg.data_dir, TableSpec(
        name="ClientFilterMixed",
        fields=[
            Field(name="id", type="uint32", owner="common", options="key"),
            Field(name="display_name", type="string", owner="common"),
            Field(name="secret_weight", type="uint32", owner="server"),
            Field(name="secret_token", type="string", owner="server"),
            Field(name="secret_weights", type="repeated uint32", owner="server", span=2),
            Field(name="secret_rates", type="map<string,uint32>", owner="server", span=2),
            Field(name="secret_drop", type="repeated { uint32 item_id; uint32 rate }", owner="server", span=2),
        ],
        rows=[
            [101, "青莲剑诀", 9999, "TOP_SECRET_SALT", 77, 88, "ssr", 5, 5001, 250],
        ],
    ))

    _compile_and_export(cfg)

    pb2_dir = str(cfg.proto_python_output_dir)
    if pb2_dir not in sys.path:
        sys.path.insert(0, pb2_dir)
    mod = importlib.import_module("clientfiltermixed_table_pb2")

    srv_bytes = (cfg.binary_dir / "clientfiltermixed.pb").read_bytes()
    cli_bytes = (cfg.client_binary_dir / "clientfiltermixed.pb").read_bytes()

    # 敏感字符串不得以任何形式出现在客户端 .pb 原始字节中
    assert b"TOP_SECRET_SALT" in srv_bytes
    assert b"TOP_SECRET_SALT" not in cli_bytes
    assert b"ssr" in srv_bytes
    assert b"ssr" not in cli_bytes

    srv_msg = mod.ClientFilterMixedTableData()
    srv_msg.ParseFromString(srv_bytes)
    assert len(srv_msg.data) == 1
    assert srv_msg.data[0].secret_weight == 9999
    assert list(srv_msg.data[0].secret_weights) == [77, 88]
    assert dict(srv_msg.data[0].secret_rates) == {"ssr": 5}
    assert len(srv_msg.data[0].secret_drop) == 1

    cli_msg = mod.ClientFilterMixedTableData()
    cli_msg.ParseFromString(cli_bytes)
    assert len(cli_msg.data) == 1
    row = cli_msg.data[0]
    assert row.id == 101
    assert row.display_name == "青莲剑诀"
    assert row.secret_weight == 0
    assert row.secret_token == ""
    assert list(row.secret_weights) == []
    assert dict(row.secret_rates) == {}
    assert len(row.secret_drop) == 0


def test_client_binary_empties_fully_server_private_table(cfg):
    write_xlsx(cfg.data_dir, TableSpec(
        name="ClientFilterServerOnly",
        fields=[
            Field(name="id", type="uint32", owner="server", options="key"),
            Field(name="anti_cheat_key", type="string", owner="server"),
        ],
        rows=[
            [1, "PRIVATE_RULE_ALPHA"],
            [2, "PRIVATE_RULE_BETA"],
        ],
    ))

    _compile_and_export(cfg)

    srv_bytes = (cfg.binary_dir / "clientfilterserveronly.pb").read_bytes()
    cli_bytes = (cfg.client_binary_dir / "clientfilterserveronly.pb").read_bytes()

    assert b"PRIVATE_RULE_ALPHA" in srv_bytes
    assert cli_bytes == b""
