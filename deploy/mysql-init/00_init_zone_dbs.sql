-- Create per-zone databases and grant appuser access.
-- Docker MYSQL_USER only auto-grants on MYSQL_DATABASE (mmorpg),
-- so zone databases need explicit grants.

CREATE DATABASE IF NOT EXISTS zone_1_db;
GRANT ALL PRIVILEGES ON `zone_1_db`.* TO 'appuser'@'%';

-- zone_2_db is used by local two-zone stress tests (`dev start-zones`).
CREATE DATABASE IF NOT EXISTS zone_2_db;
GRANT ALL PRIVILEGES ON `zone_2_db`.* TO 'appuser'@'%';

-- testdb is the local "global" database of data_service
-- (go/data_service/etc/data_service.yaml SnapshotMySQL.DBName). data_service
-- creates transaction_log / player_snapshot / rollback_audit_log / id_segment
-- there at startup (Schema.AutoMigrate) or via `data_service -migrate`; if the
-- database is missing or appuser has no grant, startup logs "schema auto-migrate
-- failed" and AllocateIdSegment (login PlayerId segments) is unavailable.
-- K8s does not use this name: k8s_deploy.ps1 generates 02_k8s_global_db.sql for
-- the cluster-global database instead. NOTE: initdb scripts only run on an empty
-- data volume; an already-initialised local MySQL will not pick this up -- run the
-- two statements below by hand or recreate the volume.
CREATE DATABASE IF NOT EXISTS testdb;
GRANT ALL PRIVILEGES ON `testdb`.* TO 'appuser'@'%';

FLUSH PRIVILEGES;
