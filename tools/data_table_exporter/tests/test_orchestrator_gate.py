"""导表闸门测试:外键失配时必须中止,且**产出一个字节都不落盘**。

以前 orchestrator 拿到 FK 结果只 ``logger.warning`` 一句就继续生成,
坏引用的表会先覆盖掉上一批好产物,再让人去查运行时的空指针。
"""

from __future__ import annotations

import pytest

from conftest import Field, TableSpec, write_xlsx
from core.orchestrator import run


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
