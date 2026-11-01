-- 显式运维模板，不由initdb自动执行；mmorpg_chat须先由唯一00_init_zone_dbs.sql预建。
-- 运维预建独立迁移账号并赋本角色/默认角色；凭据只写外部Secret的migration-dsn，不提交到仓库。
-- 首次chat -migrate只建立本库正式两表和闸门表并核信息架构；不发消息、不连Redis/etcd。
CREATE ROLE IF NOT EXISTS 'mmorpg_chat_migrator';
GRANT SELECT, CREATE, ALTER, INDEX ON `mmorpg_chat`.* TO 'mmorpg_chat_migrator';
-- 聊天库自带一份写者闸门:-migrate 建表并种 route_meta 那一行。
GRANT CREATE, INSERT ON `mmorpg_chat`.`route_meta` TO 'mmorpg_chat_migrator';
