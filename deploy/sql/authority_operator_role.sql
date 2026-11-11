-- 唯一控制身份权限叶；runtime/registrar/maintenance/schema主体必须四者不同。
-- 账号创建由独立本地部署者完成；此文件不存任何秘密，也不对runtime授注册或DDL。
-- 在迁移之后执行：下面都是表级GRANT，表不存在时MySQL、TiDB都报1146。schema账号的授权在迁移之前，由maintenance_drain.ps1的render-schema-role渲染，不在这里。
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`route_meta` TO {{REGISTRAR_ACCOUNT}};
-- 注册按槽位换代:新一代在同一事务里把同账号的上一代置 REVOKED,所以注册账号对这两张表要有 UPDATE(docs/design/writer-roster-exhaustion-20261106.md §5.4)。
-- 它本来就能插入新的 ACTIVE 写者,UPDATE 多出来的只是退休,不比插入更危险。
GRANT SELECT,INSERT,UPDATE ON {{AUTHORITY_SCHEMA}}.`writer_registry` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,INSERT,UPDATE ON {{AUTHORITY_SCHEMA}}.`writer_slot` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`writer_lifecycle_receipt` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_drain_run` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT ON {{AUTHORITY_SCHEMA}}.`sql_writer_maintenance_receipt` TO {{REGISTRAR_ACCOUNT}};
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`route_meta` TO {{MAINTENANCE_ACCOUNT}};
-- 合服工具 tools/merge_zone 的操作员身份:只写这一张映射表;route_meta 的 UPDATE 见上一行(工具 FOR UPDATE 串行化,不改它的值)。只有目录第 0 片有这张表。
{{MERGE_ZONE_GRANT}}
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`writer_registry` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,UPDATE ON {{AUTHORITY_SCHEMA}}.`writer_slot` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`writer_lifecycle_receipt` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT,UPDATE ON {{AUTHORITY_SCHEMA}}.`sql_writer_drain_run` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT,UPDATE ON {{AUTHORITY_SCHEMA}}.`sql_writer_freeze_ticket` TO {{MAINTENANCE_ACCOUNT}};
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`sql_writer_maintenance_receipt` TO {{MAINTENANCE_ACCOUNT}};
-- 搬库工具 tools/dbmove 的哨兵行:冻结之后写一行,目标能读到才算复制追上。
GRANT SELECT,INSERT ON {{AUTHORITY_SCHEMA}}.`db_move_sentinel` TO {{MAINTENANCE_ACCOUNT}};
-- 不授DROP/GRANT OPTION，runtime仍无DDL；旧账号宽授权另行撤销核验。
