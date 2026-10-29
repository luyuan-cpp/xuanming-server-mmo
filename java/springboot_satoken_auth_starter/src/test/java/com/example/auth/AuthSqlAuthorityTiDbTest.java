package com.example.auth;

import static org.junit.jupiter.api.Assertions.*;
import javax.sql.DataSource;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;

/** 由独立schema准备真实users/user_oauth/user_password后启动；normal只能validate，不能补表。 */
@SpringBootTest
class AuthSqlAuthorityTiDbTest {
    @Autowired DataSource source;
    @Test void actualAuthDataSourceIsBoundToNormalWriterAndCannotIssueControlSql()throws Exception{
        try(var connection=source.getConnection();var statement=connection.createStatement();var rows=statement.executeQuery("SELECT 1")){
            assertTrue(rows.next());assertEquals(1,rows.getInt(1));assertSame(connection,statement.getConnection());
            assertThrows(java.sql.SQLException.class,()->connection.prepareStatement("ALTER TABLE users ADD COLUMN forbidden_probe INT"));
            assertThrows(java.sql.SQLException.class,()->connection.prepareStatement("UPDATE writer_registry SET state=1"));
        }
    }
}
