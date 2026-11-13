package com.game.authority;

import static org.junit.jupiter.api.Assertions.*;
import com.zaxxer.hikari.HikariDataSource;
import java.sql.*;
import java.util.*;
import org.junit.jupiter.api.Test;

/** 真实受限perboot主体与预先由schema/operator准备的隔离库；不把H2/宽权限root当成I3证据。 */
class AuthorityHikariTiDbTest {
    private static String env(String name){String value=System.getenv(name);if(value==null||value.isBlank())throw new IllegalStateException("缺少真实fixture环境："+name);return value;}
    @Test void realHikariKeepsProtectedStandardLoopAndDatabaseLeastPrivilege()throws Exception{
        String url=env("MMORPG_AUTHORITY_IT_JDBC_URL");if(!url.matches("jdbc:mysql://127[.]0[.]0[.]1:[0-9]{1,5}/[A-Za-z0-9_]*_it_[a-f0-9]{8,32}([?].*)?"))throw new IllegalArgumentException("仅接显式loopback隔离库");
        Properties credentials=new Properties();credentials.setProperty("user",env("MMORPG_SQL_USERNAME"));credentials.setProperty("password",env("MMORPG_SQL_PASSWORD"));
        var boot=WriterAuthority.consumeEnvironment();
        try(var pool=new HikariDataSource()){
            pool.setDataSource(new AuthorityDataSource(url,credentials,boot,true,List.of()));pool.setMaximumPoolSize(2);pool.setMinimumIdle(0);pool.setConnectionTimeout(3000);
            try(var connection=pool.getConnection()){
                connection.setAutoCommit(false);
                try(var statement=connection.prepareStatement("SELECT 1");var rows=statement.executeQuery()){
                    assertTrue(rows.next());assertEquals(1,rows.getInt(1));assertSame(connection,statement.getConnection());assertSame(statement,rows.getStatement());
                    assertThrows(SQLException.class,()->connection.unwrap(Class.forName("com.mysql.cj.jdbc.JdbcConnection")));
                }
                connection.commit();assertThrows(SQLException.class,()->connection.prepareStatement("UPDATE writer_registry SET state=1"));
            }
            try(var raw=DriverManager.getConnection(url,credentials);var statement=raw.createStatement()){
                assertThrows(SQLException.class,()->statement.executeUpdate("DELETE FROM writer_registry WHERE 1=0"));
                assertThrows(SQLException.class,()->statement.executeUpdate("DELETE FROM sql_writer_drain_run WHERE 1=0"));
                assertThrows(SQLException.class,()->statement.executeUpdate("UPDATE route_meta SET writable=writable WHERE 1=0"));
            }
            boot.seal();assertTrue(boot.isSealed());assertThrows(SQLException.class,()->{try(var connection=pool.getConnection();var statement=connection.createStatement()){statement.executeQuery("SELECT 1");}});
        }
    }
}
