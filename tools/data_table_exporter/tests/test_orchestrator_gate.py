"""导表闸门测试:外键失配时必须中止,且**产出一个字节都不落盘**。

以前 orchestrator 拿到 FK 结果只 ``logger.warning`` 一句就继续生成,
坏引用的表会先覆盖掉上一批好产物,再让人去查运行时的空指针。
"""

from __future__ import annotations

import json
import subprocess
from types import SimpleNamespace

import pytest

from conftest import Field, TableSpec, build_tables, write_xlsx
from core.excel_reader import TableReadError, read_all_tables
from core.generators.binary_gen import generate_binary
from core.generators.json_gen import generate_json
from core.generators.proto_gen import (
    ProtoCompileError,
    _collect_protos,
    _compile,
    compile_proto_java,
    generate_proto_files,
)
from core.orchestrator import (
    DeployError,
    GeneratedOutputError,
    _deploy,
    _validate_java_code_outputs,
    run,
)


def _seed_bad_data(cfg) -> None:
    write_xlsx(cfg.data_dir, TableSpec(
        name="Reward",
        fields=[Field(name="id", options="key"), Field(name="item_id")],
        rows=[[1, 101], [2, 102]],
    ))
    write_xlsx(cfg.data_dir, TableSpec(
        name="Mission",
        fields=[Field(name="id", options="key"), Field(name="reward_id", options="fk:Reward")],
        rows=[[1, 1], [2, 404]],  # 404 在 Reward 里不存在
    ))


def _disable_languages(cfg) -> None:
    cfg.cpp.enabled = False
    cfg.go.enabled = False
    cfg.java.enabled = False


def test_run_aborts_on_dangling_foreign_key(cfg, caplog):
    _seed_bad_data(cfg)
    _disable_languages(cfg)

    with pytest.raises(SystemExit) as exc:
        run(cfg)

    assert exc.value.code == 1
    assert "404" in caplog.text


def test_run_writes_no_artifacts_when_foreign_key_fails(cfg):
    _seed_bad_data(cfg)
    _disable_languages(cfg)

    with pytest.raises(SystemExit):
        run(cfg)

    produced = [p for p in cfg.json_dir.rglob("*") if p.is_file()] if cfg.json_dir.exists() else []
    assert produced == [], f"外键失败时不应有任何产物落盘,实际有:{produced}"


def test_protoc_failure_is_propagated(cfg, tmp_path, monkeypatch):
    """protoc 失败不能只记日志后继续部署上一批旧产物。"""
    source = tmp_path / "proto"
    output = tmp_path / "out"
    source.mkdir()
    output.mkdir()
    (source / "broken.proto").write_text(
        'syntax = "proto3"; message Broken {}', encoding="utf-8"
    )

    def fail(*args, **kwargs):
        raise subprocess.CalledProcessError(
            returncode=1,
            cmd=args[0],
            stderr="simulated protoc failure",
        )

    monkeypatch.setattr("core.generators.proto_gen.subprocess.run", fail)

    with pytest.raises(ProtoCompileError) as exc:
        _compile(source, output, "cpp_out", cfg)
    assert "simulated protoc failure" in str(exc.value)


def test_non_recursive_proto_collection_excludes_subdirectories(tmp_path):
    source = tmp_path / "proto"
    (source / "tip").mkdir(parents=True)
    (source / "root.proto").write_text('syntax = "proto3";', encoding="utf-8")
    (source / "tip" / "nested.proto").write_text('syntax = "proto3";', encoding="utf-8")

    assert _collect_protos(source, recursive=False) == ["root.proto"]
    assert _collect_protos(source) == ["root.proto", "tip/nested.proto"]


def test_java_compiles_each_proto_once_into_package_root(cfg, monkeypatch):
    cfg.java.enabled = True
    cfg.java.proto_output_dir = cfg.generated_dir / "java-proto"
    (cfg.proto_dir / "tip").mkdir(parents=True)
    (cfg.proto_dir / "operator").mkdir()
    calls = []

    def record(source, output, out_flag, config, *, recursive=True):
        calls.append((source, output, out_flag, recursive))
        output.mkdir(parents=True, exist_ok=True)
        (output / f"{source.name}.java").write_text("generated", encoding="utf-8")

    monkeypatch.setattr("core.generators.proto_gen._compile", record)
    compile_proto_java(cfg)

    assert [call[0] for call in calls] == [
        cfg.proto_dir,
        cfg.proto_dir / "tip",
        cfg.proto_dir / "operator",
    ]
    assert [call[2:] for call in calls] == [
        ("java_out", False),
        ("java_out", True),
        ("java_out", True),
    ]
    staging_dirs = {call[1] for call in calls}
    assert len(staging_dirs) == 1
    assert cfg.java.proto_output_dir not in staging_dirs
    assert {path.name for path in cfg.java.proto_output_dir.glob("*.java")} == {
        "proto.java", "tip.java", "operator.java",
    }


def test_java_compile_rejects_stale_output_without_deleting_it(cfg, monkeypatch):
    """删 message 后遗留的旧 Java 类必须阻断部署，且不能静默删除并行改动。"""
    cfg.java.enabled = True
    cfg.java.proto_output_dir = cfg.generated_dir / "java-proto"
    cfg.java.proto_output_dir.mkdir(parents=True)
    stale = cfg.java.proto_output_dir / "StaleMessage.java"
    stale.write_text("old generated class", encoding="utf-8")

    def generate_current(_source, output, _out_flag, _config, *, recursive=True):
        output.mkdir(parents=True, exist_ok=True)
        (output / "CurrentMessage.java").write_text("current", encoding="utf-8")

    monkeypatch.setattr("core.generators.proto_gen._compile", generate_current)

    with pytest.raises(ProtoCompileError, match="StaleMessage.java"):
        compile_proto_java(cfg)
    assert stale.read_text(encoding="utf-8") == "old generated class"
    assert not (cfg.java.proto_output_dir / "CurrentMessage.java").exists()


def test_deploy_failure_is_propagated(cfg, tmp_path, monkeypatch):
    """任一部署失败时不能打印 DONE 并以 0 退出。"""
    _disable_languages(cfg)
    cfg.cpp.enabled = True
    cfg.cpp.deploy = [{"src": tmp_path / "src", "dst": tmp_path / "dst"}]

    def fail(*args, **kwargs):
        raise OSError("simulated deploy failure")

    monkeypatch.setattr("core.orchestrator.md5_copy", fail)

    with pytest.raises(DeployError) as exc:
        _deploy(cfg)
    assert "1 个部署任务失败" in str(exc.value)


def test_deploy_orphan_is_fail_closed_and_not_deleted(cfg, tmp_path):
    """部署目标中的陈旧编译单元不能只告警，也不能未审计就自动删除。"""
    _disable_languages(cfg)
    cfg.cpp.enabled = True
    source = tmp_path / "source"
    target = tmp_path / "target"
    source.mkdir()
    target.mkdir()
    (source / "Current.cpp").write_text("current", encoding="utf-8")
    stale = target / "Stale.cpp"
    stale.write_text("parallel work", encoding="utf-8")
    cfg.cpp.deploy = [{"src": source, "dst": target}]

    with pytest.raises(DeployError, match="1 个陈旧部署产物"):
        _deploy(cfg)
    assert stale.read_text(encoding="utf-8") == "parallel work"


def test_java_code_source_orphan_is_fail_closed(cfg, tmp_path):
    """生成源里的旧 Manager/Comp 不得伪装成当前部署输入。"""
    cfg.java.enabled = True
    cfg.java.code_dir = tmp_path / "java-code"
    cfg.java.code_dir.mkdir(parents=True)
    (cfg.java.code_dir / "AllTable.java").write_text("current", encoding="utf-8")
    stale = cfg.java.code_dir / "RemovedTableManager.java"
    stale.write_text("old generated class", encoding="utf-8")

    with pytest.raises(GeneratedOutputError, match="RemovedTableManager.java"):
        _validate_java_code_outputs(cfg, [])
    assert stale.read_text(encoding="utf-8") == "old generated class"


def test_unreadable_xlsx_aborts_table_scan(cfg):
    """坏表不能从 read_all_tables 结果中静默消失并继续沿用旧产物。"""
    cfg.data_dir.mkdir(parents=True, exist_ok=True)
    (cfg.data_dir / "Broken.xlsx").write_bytes(b"not an xlsx")

    with pytest.raises(TableReadError, match="Broken.xlsx"):
        read_all_tables(cfg)


def test_excel_lock_file_is_not_treated_as_input_table(cfg):
    build_tables(cfg, [TableSpec(
        name="Valid",
        fields=[Field(name="id")],
        rows=[[1]],
    )])
    (cfg.data_dir / "~$Valid.xlsx").write_bytes(b"Excel lock placeholder")

    assert [table.name for table in read_all_tables(cfg)] == ["Valid"]


def test_parallel_json_generation_failure_is_propagated(cfg, monkeypatch):
    tables = build_tables(cfg, [TableSpec(
        name="BrokenJson",
        fields=[Field(name="id")],
        rows=[[1]],
    )])

    def fail(*args, **kwargs):
        raise ValueError("simulated JSON failure")

    monkeypatch.setattr("core.generators.json_gen.read_data_rows", fail)
    with pytest.raises(RuntimeError, match="BrokenJson"):
        generate_json(cfg, tables)


def test_parallel_proto_generation_failure_is_propagated(cfg, monkeypatch):
    tables = build_tables(cfg, [TableSpec(
        name="BrokenProto",
        fields=[Field(name="id")],
        rows=[[1]],
    )])

    def fail(*args, **kwargs):
        raise RuntimeError("simulated proto generation failure")

    monkeypatch.setattr("core.generators.proto_gen._gen_one_proto", fail)
    with pytest.raises(RuntimeError, match="simulated proto generation failure"):
        generate_proto_files(cfg, tables)


def test_missing_json_aborts_binary_generation(cfg):
    tables = build_tables(cfg, [TableSpec(
        name="MissingJson",
        fields=[Field(name="id")],
        rows=[[1]],
    )])

    with pytest.raises(RuntimeError, match="MissingJson"):
        generate_binary(cfg, tables)


def test_binary_serialization_is_deterministic(cfg, monkeypatch):
    """PB 会进逐字节漂移检查，含 map 的消息必须请求 deterministic 编码。"""
    tables = build_tables(cfg, [TableSpec(
        name="Deterministic",
        fields=[Field(name="id")],
        rows=[[1]],
    )])
    cfg.json_dir.mkdir(parents=True, exist_ok=True)
    (cfg.json_dir / "Deterministic.json").write_text(
        json.dumps({"data": [{"id": 1}]}), encoding="utf-8"
    )

    class RecordingMessage:
        def SerializeToString(self, *, deterministic=False):
            if not deterministic:
                raise AssertionError("deterministic=True was not requested")
            return b"stable"

    monkeypatch.setattr(
        "core.generators.binary_gen.json_format.Parse",
        lambda _text, _message: None,
    )
    monkeypatch.setattr(
        "core.generators.binary_gen.importlib.import_module",
        lambda _name: SimpleNamespace(DeterministicTableData=RecordingMessage),
    )

    generate_binary(cfg, tables)
    assert (cfg.binary_dir / "deterministic.pb").read_bytes() == b"stable"


def test_missing_deploy_source_is_failure(cfg, tmp_path):
    _disable_languages(cfg)
    cfg.cpp.enabled = True
    cfg.cpp.deploy = [{
        "src": tmp_path / "missing-source",
        "dst": tmp_path / "dst",
    }]

    with pytest.raises(DeployError, match="1 个部署任务失败"):
        _deploy(cfg)
