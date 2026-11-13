package com.game.authority;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.*;
import java.sql.*;
import java.util.List;
import org.junit.jupiter.api.Test;

/**
 * 物理连接档位(AuthorityDataSource.Backend):配置怎么解析成档位,mysql 档怎么核对端。
 * mysql 档的判据与 Go 的 shared/authoritysql(checkMySQLServer)一致;面向 JDBC 接口用 Mockito,不连真库。
 */
class AuthorityDataSourceBackendTest {
    @Test void resolveKeepsTheTiDbRuleAndAddsMySqlPrimary(){
        assertEquals(AuthorityDataSource.Backend.TIDB, AuthorityDataSource.Backend.resolve("tidb",true,"prod"));
        assertEquals(AuthorityDataSource.Backend.TIDB, AuthorityDataSource.Backend.resolve(null,true,"prod"),"不配 backend 就是 tidb");
        assertEquals(AuthorityDataSource.Backend.ANY, AuthorityDataSource.Backend.resolve("tidb",false,"dev"),"本机库与 H2 夹具");
        assertEquals(AuthorityDataSource.Backend.ANY, AuthorityDataSource.Backend.resolve("tidb",false,"test"));
        assertThrows(IllegalStateException.class, ()->AuthorityDataSource.Backend.resolve("tidb",false,"prod"),"生产不能关掉 TiDB 强核验");
        assertEquals(AuthorityDataSource.Backend.MYSQL, AuthorityDataSource.Backend.resolve("mysql",false,"prod"),"MySQL 主从省钱档可以进生产");
        assertEquals(AuthorityDataSource.Backend.MYSQL, AuthorityDataSource.Backend.resolve("MySQL",true,"prod"),"require-tidb 只对 tidb 档有意义");
        assertThrows(IllegalStateException.class, ()->AuthorityDataSource.Backend.resolve("postgres",true,"prod"));
    }

    @Test void mySqlServerCheckRejectsTiDbAndReadOnlyInstances()throws Exception{
        AuthorityDataSource.checkMySqlServer("8.0.36","MySQL Community Server - GPL",0,0);
        assertThrows(SQLException.class, ()->AuthorityDataSource.checkMySqlServer("8.0.11-TiDB-v8.5.2","TiDB Server",0,0));
        assertThrows(SQLException.class, ()->AuthorityDataSource.checkMySqlServer("8.0.36","TiDB Server (Apache License 2.0)",0,0),
                "TiDB 配了 server-version 时只有 version_comment 露馅");
        assertThrows(SQLException.class, ()->AuthorityDataSource.checkMySqlServer("8.0.36","MySQL Community Server - GPL",1,0),"从库");
        assertThrows(SQLException.class, ()->AuthorityDataSource.checkMySqlServer("8.0.36","MySQL Community Server - GPL",0,1),"super_read_only");
    }

    private static Connection mySql(String version,String comment,long readOnly,long superReadOnly,String isolation)throws Exception{
        Connection raw=mock(Connection.class);
        when(raw.getAutoCommit()).thenReturn(true);
        DatabaseMetaData metadata=mock(DatabaseMetaData.class);
        when(metadata.getDatabaseProductName()).thenReturn("MySQL");
        when(raw.getMetaData()).thenReturn(metadata);
        Statement statement=mock(Statement.class);
        when(raw.createStatement()).thenReturn(statement);
        ResultSet server=mock(ResultSet.class);
        when(server.next()).thenReturn(true);
        when(server.getString(1)).thenReturn(version);
        when(server.getString(2)).thenReturn(comment);
        when(server.getLong(3)).thenReturn(readOnly);
        when(server.getLong(4)).thenReturn(superReadOnly);
        ResultSet session=mock(ResultSet.class);
        when(session.next()).thenReturn(true);
        when(session.getString(1)).thenReturn(isolation);
        when(statement.executeQuery(AuthorityDataSource.MYSQL_SERVER_SQL)).thenReturn(server);
        when(statement.executeQuery("SELECT @@SESSION.transaction_isolation")).thenReturn(session);
        return raw;
    }

    @Test void mySqlPrimaryIsPinnedToReadCommittedWithoutTiDbStatements()throws Exception{
        Connection raw=mySql("8.0.36","MySQL Community Server - GPL",0,0,"READ-COMMITTED");
        AuthorityDataSource.initialize(raw,AuthorityDataSource.Backend.MYSQL,List.of(AuthorityDataSource.STRONG_SESSION_SQL));
        Statement statement=raw.createStatement();
        verify(statement).execute(AuthorityDataSource.MYSQL_SESSION_SQL);
        verify(statement,never()).execute(AuthorityDataSource.STRONG_SESSION_SQL);
    }

    @Test void mySqlBackendRefusesReplicasTiDbH2AndAWrongIsolation()throws Exception{
        assertThrows(SQLException.class, ()->AuthorityDataSource.initialize(
                mySql("8.0.36","MySQL Community Server - GPL",1,1,"READ-COMMITTED"),AuthorityDataSource.Backend.MYSQL,List.of()),"从库");
        assertThrows(SQLException.class, ()->AuthorityDataSource.initialize(
                mySql("8.0.11-TiDB-v8.5.2","TiDB Server",0,0,"READ-COMMITTED"),AuthorityDataSource.Backend.MYSQL,List.of()),"TiDB");
        assertThrows(SQLException.class, ()->AuthorityDataSource.initialize(
                mySql("8.0.36","MySQL Community Server - GPL",0,0,"REPEATABLE-READ"),AuthorityDataSource.Backend.MYSQL,List.of()),"隔离级别没固定住");
        Connection h2=mySql("2.2.224","",0,0,"READ-COMMITTED");
        when(h2.getMetaData().getDatabaseProductName()).thenReturn("H2");
        assertThrows(SQLException.class, ()->AuthorityDataSource.initialize(h2,AuthorityDataSource.Backend.MYSQL,List.of()),"H2 夹具不是 MySQL 主库");
    }

    /** 只有 mysql 档在每个事务进入前复核对端;复核与建连时的核对是同一个函数,带语句超时。 */
    @Test void onlyTheMySqlBackendRechecksTheServerPerTransaction()throws Exception{
        assertNull(AuthorityDataSource.serverCheck(AuthorityDataSource.Backend.TIDB),"tidb 档的对端没有会随时间变化的角色");
        assertNull(AuthorityDataSource.serverCheck(AuthorityDataSource.Backend.ANY));
        AuthorityConnection.ServerCheck check=AuthorityDataSource.serverCheck(AuthorityDataSource.Backend.MYSQL);
        assertNotNull(check);
        Connection primary=mySql("8.0.36","MySQL Community Server - GPL",0,0,"READ-COMMITTED");
        check.recheck(primary);
        Statement statement=primary.createStatement();
        verify(statement).setQueryTimeout(intThat(seconds->seconds>0));
        verify(statement,never()).execute(anyString());
        assertThrows(SQLException.class, ()->check.recheck(mySql("8.0.36","MySQL Community Server - GPL",1,1,"READ-COMMITTED")),
                "主从切换后这条连接连着的是已降成只读的旧主库");
    }

    private static final String TIDB_COMMENT="TiDB Server (Apache License 2.0) Community Edition, MySQL 8.0 compatible";

    /** 一条会对 tidb / ANY 档的识别与强会话读回作答的物理连接;version 与 comment 由用例给。 */
    private static Connection sqlServer(String version,String comment)throws Exception{
        Connection raw=mock(Connection.class);
        when(raw.getAutoCommit()).thenReturn(true);
        DatabaseMetaData metadata=mock(DatabaseMetaData.class);
        when(metadata.getDatabaseProductName()).thenReturn("MySQL");
        when(raw.getMetaData()).thenReturn(metadata);
        Statement statement=mock(Statement.class);
        when(raw.createStatement()).thenReturn(statement);
        ResultSet identity=mock(ResultSet.class);
        when(identity.next()).thenReturn(true);
        when(identity.getString(1)).thenReturn(version);
        when(identity.getString(2)).thenReturn(comment);
        when(statement.executeQuery(AuthorityDataSource.SERVER_IDENTITY_SQL)).thenReturn(identity);
        ResultSet session=mock(ResultSet.class);
        when(session.next()).thenReturn(true);
        when(session.getString(1)).thenReturn("");
        when(session.getString(4)).thenReturn("strict");
        when(session.getString(5)).thenReturn("leader");
        when(session.getString(6)).thenReturn("pessimistic");
        when(session.getString(7)).thenReturn("READ-COMMITTED");
        when(statement.executeQuery(startsWith("SELECT @@SESSION.tidb_snapshot"))).thenReturn(session);
        return raw;
    }

    @Test void tiDbIsRecognisedByVersionCommentWhenServerVersionIsOverridden()throws Exception{
        assertTrue(AuthorityDataSource.isTiDb("8.0.11-TiDB-v8.5.2",TIDB_COMMENT));
        assertTrue(AuthorityDataSource.isTiDb("8.0.36",TIDB_COMMENT),"配了 server-version 的 TiDB 只有 version_comment 认得出");
        assertFalse(AuthorityDataSource.isTiDb("8.0.36","MySQL Community Server - GPL"));
        assertFalse(AuthorityDataSource.isTiDb(null,null));

        Connection overridden=sqlServer("8.0.36",TIDB_COMMENT);
        AuthorityDataSource.initialize(overridden,AuthorityDataSource.Backend.TIDB,List.of());
        Statement pinned=overridden.createStatement();
        verify(pinned).execute(AuthorityDataSource.STRONG_SESSION_SQL);

        Connection devTier=sqlServer("8.0.36",TIDB_COMMENT);
        AuthorityDataSource.initialize(devTier,AuthorityDataSource.Backend.ANY,List.of());
        Statement devPinned=devTier.createStatement();
        verify(devPinned).execute(AuthorityDataSource.STRONG_SESSION_SQL);
    }

    @Test void tiDbBackendStillRefusesAPlainMySqlServer()throws Exception{
        Connection plain=sqlServer("8.0.36","MySQL Community Server - GPL");
        assertThrows(SQLException.class, ()->AuthorityDataSource.initialize(plain,AuthorityDataSource.Backend.TIDB,List.of()),"tidb 档接到 MySQL");
        Statement statement=plain.createStatement();
        verify(statement,never()).execute(anyString());
    }
}
