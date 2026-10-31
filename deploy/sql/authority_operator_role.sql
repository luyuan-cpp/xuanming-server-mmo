-- 唯一控制身份权限叶；runtime/registrar/maintenance/schema主体必须四者不同。
-- 账号创建由独立本地部署者完成；此文件不存任何秘密，也不对runtime授注册或DDL。
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`route_meta` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`writer_registry` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`writer_slot` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`writer_lifecycle_receipt` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_drain_run` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_maintenance_receipt` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`route_meta` TO {{MAINTENANCE_ACCOUNT}};
-- 合服工具 tools/merge_zone 的操作员身份:只写这一张映射表;route_meta 的 UPDATE 见上一行(工具 FOR UPDATE 串行化,不改它的值)。
GRANT SELECT,INSERT,DELETE ON {{AUTHORITY_SCHEMA}}.`zone_merged_into` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`writer_registry` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`writer_slot` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`writer_lifecycle_receipt` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT,UPDATE ON {{AUTHORITY_SCHEMA}}.`sql_writer_drain_run` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT,UPDATE ON {{AUTHORITY_SCHEMA}}.`sql_writer_freeze_ticket` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`sql_writer_maintenance_receipt` TO {{MAINTENANCE_ACCOUNT}};
-- schemaDatabases是独立审批的fresh prestart具名库清单，不从runtime业务表推断。
-- schema身份仅供LoadSchemaPermission确认的fresh prestart；冻结run内不执行DDL。
{{SCHEMA_DDL_GRANTS}}
-- 元数据点表能力用于fresh prestart种子与schema_migrations台账，不授权业务schema.*的DML。
{{SCHEMA_METADATA_GRANTS}}
-- 不授DROP/GRANT OPTION，runtime仍无DDL；旧账号宽授权另行撤销核验。
