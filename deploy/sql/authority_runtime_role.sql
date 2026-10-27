-- 唯一runtime权限叶；由maintenance_drain.ps1结构化renderer绑定具名表和每boot独立主体。
-- 不含CREATE USER/口令/GRANT OPTION/DDL；不能把schema.*作为业务表清单。
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`route_meta` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`writer_registry` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`writer_slot` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`writer_lifecycle_receipt` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_drain_run` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_freeze_ticket` TO {{RUNTIME_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_maintenance_receipt` TO {{RUNTIME_ACCOUNT}};
{{LOCK_TOUCH_GRANT}}
{{BUSINESS_TABLE_GRANTS}}
