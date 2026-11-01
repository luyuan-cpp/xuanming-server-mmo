-- 唯一runtime权限叶；由maintenance_drain.ps1结构化renderer绑定具名表和每boot独立主体。
-- 不含CREATE USER/口令/GRANT OPTION/DDL；不能把schema.*作为业务表清单。
-- {{AUTHORITY_SCHEMA}}是本账号要写的那个逻辑库：闸门跟着库走，写几个库就按库各渲染一遍。不需要别的库的闸门表权限。
-- 业务表只在本账号自己的库里：go/db选库走data_service的GetPlayerRoute，match读合服映射走GetMergedZones，都不连目录。
-- 唯一的例外是data_service，它在存档库写player_store，读player_centre_database和user_accounts：这些授权由计划的tables给出，见docs/ops/k8s-open-server-runbook.md。
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`route_meta` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`writer_registry` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`writer_slot` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`writer_lifecycle_receipt` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_drain_run` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_freeze_ticket` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_maintenance_receipt` TO {{RUNTIME_ACCOUNT}};
{{LOCK_TOUCH_GRANT}}
{{BUSINESS_TABLE_GRANTS}}
