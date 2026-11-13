package com.game.authority;

import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.*;
import java.math.BigInteger;
import java.sql.*;
import org.junit.jupiter.api.Test;
import org.mockito.stubbing.Answer;

/** 面向JDBC接口验证；测试不改变真实大updateCount，也不把keys行数当受影响行数。 */
class AuthorityConnectionTest {
    private record Fixture(Connection raw,Connection managed,WriterAuthority authority){}
    private Fixture fixture()throws Exception{
        Connection raw=mock(Connection.class);when(raw.getAutoCommit()).thenReturn(true);
        WriterAuthority authority=mock(WriterAuthority.class);
        return new Fixture(raw,AuthorityConnection.wrap(raw,authority),authority);
    }
    private ResultSet keys(BigInteger... values)throws Exception{
        ResultSet keys=mock(ResultSet.class);ResultSetMetaData metadata=mock(ResultSetMetaData.class);
        when(keys.getMetaData()).thenReturn(metadata);when(metadata.getColumnCount()).thenReturn(1);
        when(metadata.getColumnType(1)).thenReturn(Types.BIGINT);when(metadata.getColumnTypeName(1)).thenReturn("BIGINT UNSIGNED");
        when(metadata.getColumnName(1)).thenReturn("GENERATED_KEY");when(metadata.getColumnLabel(1)).thenReturn("GENERATED_KEY");
        when(metadata.getColumnClassName(1)).thenReturn(BigInteger.class.getName());when(metadata.isNullable(1)).thenReturn(ResultSetMetaData.columnNoNulls);
        when(metadata.getSchemaName(1)).thenReturn("");when(metadata.getTableName(1)).thenReturn("");when(metadata.getCatalogName(1)).thenReturn("");
        when(keys.getType()).thenReturn(ResultSet.TYPE_FORWARD_ONLY);when(keys.getHoldability()).thenReturn(ResultSet.HOLD_CURSORS_OVER_COMMIT);
        int[] at={-1};when(keys.next()).thenAnswer(call->++at[0]<values.length);
        when(keys.getObject(1)).thenAnswer(call->values[at[0]]);
        when(keys.getString(1)).thenAnswer(call->values[at[0]].toString());
        when(keys.getLong(1)).thenThrow(new SQLDataException("原driver高半区溢出","22003",1264));
        return keys;
    }
    @Test void updateCountAndUnsignedKeysReturnOnlyAfterCommit()throws Exception{
        var f=fixture();PreparedStatement statement=mock(PreparedStatement.class);ResultSet original=keys(new BigInteger("18446744073709551615"));
        when(f.raw.prepareStatement("INSERT INTO business(v) VALUES (?)",Statement.RETURN_GENERATED_KEYS)).thenReturn(statement);
        when(statement.executeUpdate()).thenReturn(Integer.MAX_VALUE);when(statement.getGeneratedKeys()).thenReturn(original);
        var managed=f.managed.prepareStatement("INSERT INTO business(v) VALUES (?)",Statement.RETURN_GENERATED_KEYS);
        assertEquals(Integer.MAX_VALUE,managed.executeUpdate());
        var order=inOrder(statement,original,f.raw);order.verify(statement).executeUpdate();order.verify(statement).getGeneratedKeys();order.verify(original).close();order.verify(f.raw).commit();
        var cached=managed.getGeneratedKeys();assertTrue(cached.next());assertInstanceOf(BigInteger.class,cached.getObject(1));
        assertEquals(new BigInteger("18446744073709551615"),cached.getObject("GENERATED_KEY"));
        assertEquals("22003",assertThrows(SQLException.class,()->cached.getLong(1)).getSQLState());
        assertEquals(BigInteger.class.getName(),cached.getMetaData().getColumnClassName(1));assertSame(managed,cached.getStatement());
        managed.close();assertTrue(cached.isClosed());verify(statement,times(1)).executeUpdate();
    }
    @Test void commitReplyLossIsOpaqueUnknownAndDiscardsPhysical()throws Exception{
        var f=fixture();PreparedStatement statement=mock(PreparedStatement.class);when(f.raw.prepareStatement("UPDATE business SET v=1")).thenReturn(statement);
        ResultSet generated=keys(BigInteger.ONE);
        when(statement.executeUpdate()).thenReturn(7);when(statement.getGeneratedKeys()).thenReturn(generated);
        doThrow(new SQLRecoverableException("应答丢失","08S01",3024)).when(f.raw).commit();
        var managed=f.managed.prepareStatement("UPDATE business SET v=1");
        SQLException unknown=assertThrows(SQLException.class,managed::executeUpdate);assertEquals("08007",unknown.getSQLState());assertNull(unknown.getCause());
        assertEquals("08007",assertThrows(SQLException.class,managed::executeUpdate).getSQLState());verify(statement,times(1)).executeUpdate();verify(f.raw,atLeastOnce()).abort(any());verify(f.raw,never()).rollback();
    }
    @Test void secondAutoStatementCannotRunBeforeFirstEof()throws Exception{
        var f=fixture();Statement first=mock(Statement.class),second=mock(Statement.class);when(f.raw.createStatement()).thenReturn(first,second);
        ResultSet rows=mock(ResultSet.class);when(first.executeQuery("SELECT v FROM business")).thenReturn(rows);
        Statement a=f.managed.createStatement(),b=f.managed.createStatement();ResultSet result=a.executeQuery("SELECT v FROM business");
        SQLException blocked=assertThrows(SQLException.class,()->b.executeUpdate("UPDATE business SET v=2"));assertEquals("HY010",blocked.getSQLState());verify(second,never()).executeUpdate(anyString());
        assertSame(a,result.getStatement());assertSame(f.managed,a.getConnection());assertThrows(SQLException.class,()->a.unwrap(first.getClass()));
        result.close();verify(f.raw).commit();
    }
    @Test void explicitBatchPreservesDefinite1062PrefixAndCallerDecision()throws Exception{
        var f=fixture();PreparedStatement statement=mock(PreparedStatement.class);when(f.raw.prepareStatement("INSERT INTO business(v) VALUES (?)")).thenReturn(statement);
        BatchUpdateException duplicate=new BatchUpdateException("重复键","23000",1062,new int[]{1,1});when(statement.executeBatch()).thenThrow(duplicate);
        f.managed.setAutoCommit(false);var managed=f.managed.prepareStatement("INSERT INTO business(v) VALUES (?)");managed.addBatch();
        assertSame(duplicate,assertThrows(BatchUpdateException.class,managed::executeBatch));verify(f.raw,never()).commit();verify(f.raw,never()).rollback();
        f.managed.commit();verify(f.raw).commit();verify(f.authority,times(1)).begin(f.raw);
    }
    @Test void autoNativeBatchRejectsBeforeDelegate()throws Exception{
        var f=fixture();PreparedStatement statement=mock(PreparedStatement.class);when(f.raw.prepareStatement("INSERT INTO business(v) VALUES (?)")).thenReturn(statement);
        var managed=f.managed.prepareStatement("INSERT INTO business(v) VALUES (?)");managed.addBatch();assertThrows(SQLFeatureNotSupportedException.class,managed::executeBatch);verify(statement,never()).executeBatch();
    }
    /** 冻结时事务一条业务语句都没发：错误原样到调用方、物理连接不隔离，解冻或换连接之后重试能成。 */
    @Test void frozenGateIsRetryableAndKeepsPhysical()throws Exception{
        var f=fixture();PreparedStatement statement=mock(PreparedStatement.class);when(f.raw.prepareStatement("UPDATE business SET v=1")).thenReturn(statement);
        when(statement.executeUpdate()).thenReturn(1);
        doThrow(new WriterFrozenException()).doNothing().when(f.authority).begin(f.raw);
        var managed=f.managed.prepareStatement("UPDATE business SET v=1");
        SQLException frozen=assertThrows(SQLException.class,managed::executeUpdate);
        assertInstanceOf(WriterFrozenException.class,frozen);assertInstanceOf(SQLTransientException.class,frozen);assertEquals("40001",frozen.getSQLState());
        verify(statement,never()).executeUpdate();verify(f.raw,never()).abort(any());
        assertEquals(1,managed.executeUpdate());verify(f.raw).commit();
    }
    /** 进入事务前的对端复核不过：一条业务语句都没发、写者闸门没进、物理连接丢弃；错误可重试，不是结局未知。 */
    @Test void failedServerRecheckDiscardsPhysicalBeforeAnyBusinessStatement()throws Exception{
        Connection raw=mock(Connection.class);when(raw.getAutoCommit()).thenReturn(true);
        WriterAuthority authority=mock(WriterAuthority.class);
        SQLException demoted=new SQLException("read_only=1");
        Connection managed=AuthorityConnection.wrap(raw,authority,physical->{throw demoted;});
        PreparedStatement statement=mock(PreparedStatement.class);when(raw.prepareStatement("UPDATE business SET v=1")).thenReturn(statement);
        var prepared=managed.prepareStatement("UPDATE business SET v=1");
        SQLException failure=assertThrows(SQLException.class,prepared::executeUpdate);
        assertInstanceOf(ServerRecheckFailedException.class,failure);assertInstanceOf(SQLTransientException.class,failure);
        assertEquals("08S01",failure.getSQLState());assertSame(demoted,failure.getCause());
        verify(authority,never()).begin(raw);verify(statement,never()).executeUpdate();verify(raw).abort(any());
        assertTrue(managed.isClosed());assertFalse(managed.isValid(1));
        assertEquals("08003",assertThrows(SQLException.class,prepared::executeUpdate).getSQLState(),"丢弃之后是连接已关闭，不是结局未知");
    }
    /** 复核通过时先复核、后进写者闸门；同一个事务只复核一次，下一个事务再复核。 */
    @Test void serverRecheckRunsOncePerTransactionBeforeTheWriterGate()throws Exception{
        Connection raw=mock(Connection.class);when(raw.getAutoCommit()).thenReturn(true);
        WriterAuthority authority=mock(WriterAuthority.class);
        AuthorityConnection.ServerCheck check=mock(AuthorityConnection.ServerCheck.class);
        Connection managed=AuthorityConnection.wrap(raw,authority,check);
        PreparedStatement statement=mock(PreparedStatement.class);when(raw.prepareStatement("UPDATE business SET v=1")).thenReturn(statement);
        when(statement.executeUpdate()).thenReturn(1);
        managed.setAutoCommit(false);
        var prepared=managed.prepareStatement("UPDATE business SET v=1");
        prepared.executeUpdate();prepared.executeUpdate();
        var order=inOrder(check,authority,statement);
        order.verify(check).recheck(raw);order.verify(authority).begin(raw);order.verify(statement,times(2)).executeUpdate();
        verify(check,times(1)).recheck(raw);
        managed.commit();prepared.executeUpdate();
        verify(check,times(2)).recheck(raw);verify(authority,times(2)).begin(raw);
    }
    /** 连接池探活带上对端复核：空闲连接复核不过就判无效并丢弃；事务进行中不往里插复核语句。 */
    @Test void validityProbeRechecksIdleConnectionsOnly()throws Exception{
        Connection raw=mock(Connection.class);when(raw.getAutoCommit()).thenReturn(true);when(raw.isValid(anyInt())).thenReturn(true);
        WriterAuthority authority=mock(WriterAuthority.class);
        AuthorityConnection.ServerCheck check=mock(AuthorityConnection.ServerCheck.class);
        Connection managed=AuthorityConnection.wrap(raw,authority,check);
        assertTrue(managed.isValid(2));verify(check,times(1)).recheck(raw);
        Statement statement=mock(Statement.class);when(raw.createStatement()).thenReturn(statement);
        managed.setAutoCommit(false);managed.createStatement().executeUpdate("UPDATE business SET v=1");
        verify(check,times(2)).recheck(raw);
        assertTrue(managed.isValid(2));verify(check,times(2)).recheck(raw);
        managed.rollback();
        doThrow(new SQLException("read_only=1")).when(check).recheck(raw);
        assertFalse(managed.isValid(2));verify(raw).abort(any());assertTrue(managed.isClosed());
    }
    /** 没给对端复核的档位（tidb、本机夹具）探活只问驱动，进入事务前也不多发语句。 */
    @Test void withoutServerCheckTheProbeOnlyAsksTheDriver()throws Exception{
        var f=fixture();when(f.raw.isValid(3)).thenReturn(true,false);
        assertTrue(f.managed.isValid(3));assertFalse(f.managed.isValid(3));
        verify(f.raw,never()).abort(any());verify(f.raw,never()).createStatement();
    }
    /** route_meta.writable=0 映射成 WriterFrozenException；meta 形状不对仍是不可重试的普通错误。 */
    @Test void frozenMetaBecomesWriterFrozenAndBadMetaDoesNot()throws Exception{
        var mapper=new com.fasterxml.jackson.databind.ObjectMapper();
        var grant=mapper.createObjectNode();
        var encoder=java.util.Base64.getEncoder();byte[] sixteen=new byte[16],thirtyTwo=new byte[32];java.util.Arrays.fill(sixteen,(byte)1);java.util.Arrays.fill(thirtyTwo,(byte)2);
        grant.put("format","SQL_WRITER_BOOT_V1").put("permission","NORMAL").put("generation","ab").put("authoritySchema","mmorpg").put("deploymentId","boot-1").put("grantedMs","1").put("lockPrimitive","SELECT_ONLY");
        grant.putArray("protectedSchemas").add("mmorpg");
        for(String field:java.util.List.of("writerKey","instanceId","grantId"))grant.put(field,encoder.encodeToString(sixteen));
        for(String field:java.util.List.of("registrationDigest","physicalAuthoritySha256","lockQualificationSha256"))grant.put(field,encoder.encodeToString(thirtyTwo));
        grant.put("sqlPrincipal",encoder.encodeToString("writer@%".getBytes()));
        var constructor=WriterAuthority.class.getDeclaredConstructor(com.fasterxml.jackson.databind.JsonNode.class);constructor.setAccessible(true);
        WriterAuthority authority=constructor.newInstance(grant);
        Connection raw=mock(Connection.class);when(raw.getAutoCommit()).thenReturn(true);
        PreparedStatement read=mock(PreparedStatement.class);ResultSet meta=mock(ResultSet.class);
        when(raw.prepareStatement(anyString())).thenReturn(read);when(read.executeQuery()).thenReturn(meta);
        when(meta.next()).thenReturn(true);when(meta.getString(1)).thenReturn("ab");when(meta.getObject(2)).thenReturn(7L);when(meta.getObject(4)).thenReturn(9L);when(meta.getBytes(5)).thenReturn(new byte[0]);
        when(meta.getInt(3)).thenReturn(0);
        assertThrows(WriterFrozenException.class,()->authority.begin(raw));
        verify(raw,never()).setAutoCommit(false);
        when(meta.getInt(3)).thenReturn(2);
        SQLException bad=assertThrows(SQLException.class,()->authority.begin(raw));
        assertFalse(bad instanceof SQLTransientException);
    }
    @Test void sqlControlAndUnwrapCannotEscape()throws Exception{
        var f=fixture();assertThrows(SQLException.class,()->f.managed.prepareStatement("UPDATE writer_registry SET state=1"));
        assertThrows(SQLException.class,()->f.managed.prepareStatement("SELECT 1; DELETE FROM business"));
        assertThrows(SQLException.class,()->f.managed.prepareStatement("SET SESSION tidb_snapshot='old'"));
        assertThrows(SQLException.class,()->f.managed.unwrap(f.raw.getClass()));verify(f.raw,never()).prepareStatement(anyString());
    }

    @Test void heldCursorReentersFenceAfterCommitAndRollback()throws Exception{
        for(boolean commit:new boolean[]{true,false}){
            var f=fixture();Statement raw=mock(Statement.class);ResultSet cursor=mock(ResultSet.class);
            when(f.raw.createStatement()).thenReturn(raw);when(raw.executeQuery("SELECT v FROM business")).thenReturn(cursor);
            f.managed.setAutoCommit(false);ResultSet rows=f.managed.createStatement().executeQuery("SELECT v FROM business");
            if(commit)f.managed.commit();else f.managed.rollback();
            rows.updateRow();rows.insertRow();rows.deleteRow();
            var order=inOrder(f.authority,f.raw,cursor);
            order.verify(f.authority).begin(f.raw);
            if(commit)order.verify(f.raw).commit();else order.verify(f.raw).rollback();
            order.verify(f.authority).begin(f.raw);order.verify(cursor).updateRow();order.verify(cursor).insertRow();order.verify(cursor).deleteRow();
            verify(f.authority,times(2)).begin(f.raw);
        }
    }
    @Test void retiredFenceBlocksHeldCursorBeforeAnyMutation()throws Exception{
        var f=fixture();Statement raw=mock(Statement.class);ResultSet cursor=mock(ResultSet.class);
        when(f.raw.createStatement()).thenReturn(raw);when(raw.executeQuery("SELECT v FROM business")).thenReturn(cursor);
        f.managed.setAutoCommit(false);ResultSet rows=f.managed.createStatement().executeQuery("SELECT v FROM business");f.managed.commit();
        doThrow(new SQLException("本boot已退役","25000")).when(f.authority).begin(f.raw);
        assertThrows(SQLException.class,rows::updateRow);assertThrows(SQLException.class,rows::insertRow);assertThrows(SQLException.class,rows::deleteRow);
        assertThrows(SQLException.class,()->rows.updateString(1,"blocked"));
        verify(cursor,never()).updateRow();verify(cursor,never()).insertRow();verify(cursor,never()).deleteRow();verify(cursor,never()).updateString(anyInt(),anyString());
    }
    @Test void lobLocatorCannotWriteAfterFenceWasRetired()throws Exception{
        var f=fixture();Statement raw=mock(Statement.class);ResultSet cursor=mock(ResultSet.class);Blob blob=mock(Blob.class);
        when(f.raw.createStatement()).thenReturn(raw);when(raw.executeQuery("SELECT v FROM business")).thenReturn(cursor);when(cursor.getBlob(1)).thenReturn(blob);
        f.managed.setAutoCommit(false);Blob managed=f.managed.createStatement().executeQuery("SELECT v FROM business").getBlob(1);f.managed.rollback();
        doThrow(new SQLException("本boot已退役","25000")).when(f.authority).begin(f.raw);
        assertThrows(SQLException.class,()->managed.setBytes(1,new byte[]{1}));assertThrows(SQLException.class,()->managed.truncate(0));
        verify(blob,never()).setBytes(anyLong(),any(byte[].class));verify(blob,never()).truncate(anyLong());
    }
    @Test void writableLobStreamKeepsFenceUntilCloseAndRollbackInvalidatesIt()throws Exception{
        var f=fixture();Blob blob=mock(Blob.class);java.io.OutputStream stream=mock(java.io.OutputStream.class);when(f.raw.createBlob()).thenReturn(blob);when(blob.setBinaryStream(1)).thenReturn(stream);
        Blob managed=f.managed.createBlob();f.managed.setAutoCommit(false);java.io.OutputStream output=managed.setBinaryStream(1);output.write(7);
        assertEquals("HY010",assertThrows(SQLException.class,f.managed::commit).getSQLState());verify(f.raw,never()).commit();
        f.managed.rollback();var order=inOrder(f.authority,stream,f.raw);order.verify(f.authority).begin(f.raw);order.verify(stream).write(7);order.verify(stream).close();order.verify(f.raw).rollback();
        assertThrows(java.io.IOException.class,()->output.write(9));verify(stream,never()).write(9);
    }
    @Test void writableLobStreamFailurePoisonsPhysicalWithoutReplay()throws Exception{
        var f=fixture();Blob blob=mock(Blob.class);java.io.OutputStream stream=mock(java.io.OutputStream.class);when(f.raw.createBlob()).thenReturn(blob);when(blob.setBinaryStream(1)).thenReturn(stream);
        doThrow(new java.io.IOException("应答未知")).when(stream).write(7);
        java.io.OutputStream output=f.managed.createBlob().setBinaryStream(1);
        assertThrows(java.io.IOException.class,()->output.write(7));assertEquals("08007",assertThrows(SQLException.class,f.managed::createStatement).getSQLState());verify(stream,times(1)).write(7);verify(f.raw,atLeastOnce()).abort(any());
    }
    @Test void replayPropertiesAcceptOnlyExplicitFalse()throws Exception{
        for(String key:java.util.List.of("allowMultiQueries","autoReconnect","autoReconnectForPools","reconnectAtTxEnd","createDatabaseIfNotExist")){
            for(String value:java.util.List.of("true","YES","yes","1","unknown","","false ")){
                java.util.Properties properties=new java.util.Properties();properties.setProperty(key,value);
                assertThrows(SQLException.class,()->AuthorityDataSource.validateProperties(properties));
                assertThrows(SQLException.class,()->AuthorityDataSource.validateUrl("jdbc:mysql://127.0.0.1/test?"+key+"="+value));
            }
            java.util.Properties properties=new java.util.Properties();properties.setProperty(key,"FALSE");AuthorityDataSource.validateProperties(properties);AuthorityDataSource.validateUrl("jdbc:mysql://127.0.0.1/test?"+key+"=false");
        }
        assertThrows(SQLException.class,()->AuthorityDataSource.validateUrl("jdbc:mysql://127.0.0.1/test?auto%52econnect=%59ES"));
        assertThrows(SQLException.class,()->AuthorityDataSource.validateUrl("jdbc:mysql://127.0.0.1/test?autoReconnect=false&autoReconnect=YES"));
        java.util.Properties defaults=new java.util.Properties();defaults.setProperty("autoReconnect","YES");
        assertThrows(SQLException.class,()->AuthorityDataSource.validateProperties(new java.util.Properties(defaults)));
    }

    @Test void nestedArrayReferenceStillRequiresCurrentFence()throws Exception{
        var f=fixture();java.sql.Array array=mock(java.sql.Array.class);Ref ref=mock(Ref.class);
        when(f.raw.createArrayOf(eq("business"),any(Object[].class))).thenReturn(array);when(array.getArray()).thenReturn(new Object[]{ref});
        f.managed.setAutoCommit(false);Ref managed=(Ref)((Object[])f.managed.createArrayOf("business",new Object[0]).getArray())[0];
        managed.setObject("first");f.managed.commit();doThrow(new SQLException("已退役","25000")).when(f.authority).begin(f.raw);
        assertThrows(SQLException.class,()->managed.setObject("blocked"));verify(ref).setObject("first");verify(ref,never()).setObject("blocked");
    }
    @Test void helperAclKeepsOwnerAndReplacementGuardsWhileAllowingAncestorCreation()throws Exception{
        java.nio.file.attribute.UserPrincipal current=()->"current",installer=()->"NT SERVICE\\TrustedInstaller",other=()->"other";
        var trusted=java.util.Set.of(current,installer);
        var createOnly=java.nio.file.attribute.AclEntry.newBuilder().setType(java.nio.file.attribute.AclEntryType.ALLOW).setPrincipal(other)
                .setPermissions(java.nio.file.attribute.AclEntryPermission.WRITE_DATA,java.nio.file.attribute.AclEntryPermission.APPEND_DATA).build();
        // 真实C盘根的创建目录ACE不得误判为替换现有Users路径；同样的权限在实际EXE上仍拒绝。
        assertDoesNotThrow(()->WriterAuthority.requireHelperPathAcl(false,installer,java.util.List.of(createOnly),trusted));
        assertEquals("helper可被非受信主体替换",assertThrows(java.io.IOException.class,()->WriterAuthority.requireHelperPathAcl(true,current,java.util.List.of(createOnly),trusted)).getMessage());
        assertEquals("helper owner未取得资格",assertThrows(java.io.IOException.class,()->WriterAuthority.requireHelperPathAcl(false,other,java.util.List.of(createOnly),trusted)).getMessage());
        for(var permission:java.util.List.of(java.nio.file.attribute.AclEntryPermission.WRITE_NAMED_ATTRS,java.nio.file.attribute.AclEntryPermission.DELETE_CHILD,
                java.nio.file.attribute.AclEntryPermission.WRITE_ATTRIBUTES,java.nio.file.attribute.AclEntryPermission.DELETE,java.nio.file.attribute.AclEntryPermission.WRITE_ACL,java.nio.file.attribute.AclEntryPermission.WRITE_OWNER)){
            var replace=java.nio.file.attribute.AclEntry.newBuilder().setType(java.nio.file.attribute.AclEntryType.ALLOW).setPrincipal(other).setPermissions(permission).build();
            assertEquals("helper可被非受信主体替换",assertThrows(java.io.IOException.class,()->WriterAuthority.requireHelperPathAcl(false,installer,java.util.List.of(replace),trusted)).getMessage());
            assertEquals("helper可被非受信主体替换",assertThrows(java.io.IOException.class,()->WriterAuthority.requireHelperPathAcl(true,current,java.util.List.of(replace),trusted)).getMessage());
        }
        var readOnly=java.nio.file.attribute.AclEntry.newBuilder().setType(java.nio.file.attribute.AclEntryType.ALLOW).setPrincipal(other)
                .setPermissions(java.nio.file.attribute.AclEntryPermission.READ_DATA,java.nio.file.attribute.AclEntryPermission.EXECUTE).build();
        assertDoesNotThrow(()->WriterAuthority.requireHelperPathAcl(true,current,java.util.List.of(readOnly),trusted));
        var inheritedOnly=java.nio.file.attribute.AclEntry.newBuilder(createOnly).setFlags(java.nio.file.attribute.AclEntryFlag.INHERIT_ONLY).build();
        assertDoesNotThrow(()->WriterAuthority.requireHelperPathAcl(true,current,java.util.List.of(inheritedOnly),trusted));
    }

    @Test void boundedSelectDeadlineHintsKeepExistingProducerSqlAndDriverBytes()throws Exception{
        // PlayerProfiles.readBatch原列/占位，以及ListingSql.COUNT_SELLER_LISTINGS经原期限hint生成的字面SQL。
        for(String sql:java.util.List.of(
                "SELECT /*+ MAX_EXECUTION_TIME(800) */ player_id, name, level, class_id, gender, appearance_id, zone_id FROM player WHERE player_id IN (?,?)",
                "SELECT /*+ MAX_EXECUTION_TIME(1000) */ COUNT(*) FROM trade_listing WHERE `seller_player_id` = ?")){
            var f=fixture();PreparedStatement driver=mock(PreparedStatement.class);
            when(f.raw.prepareStatement(sql)).thenReturn(driver);
            try(var managed=f.managed.prepareStatement(sql)){
                assertNotSame(driver,managed);
                assertEquals(AuthoritySql.Kind.READ,AuthoritySql.validate(sql));
                verify(f.raw).prepareStatement(sql); // 不strip、不重写实际交driver的原SQL。
                verify(driver,never()).executeQuery();
            }
        }
        assertEquals(AuthoritySql.Kind.READ,AuthoritySql.validate("SELECT /*+ MAX_EXECUTION_TIME(1) */ 1"));
        assertEquals(AuthoritySql.Kind.READ,AuthoritySql.validate("SELECT /*+ MAX_EXECUTION_TIME("+Integer.MAX_VALUE+") */ 1"));
        assertArrayEquals(new long[]{Integer.MAX_VALUE,Integer.MAX_VALUE},AuthoritySql.deadlineCaps(
                "SET SESSION MAX_EXECUTION_TIME="+Integer.MAX_VALUE+",TIDB_DML_MAX_EXECUTION_TIME="+Integer.MAX_VALUE));
        assertArrayEquals(new long[]{0,0},AuthoritySql.deadlineCaps("SET SESSION MAX_EXECUTION_TIME=0,TIDB_DML_MAX_EXECUTION_TIME=0"));
    }

    @Test void selectDeadlineHintDoesNotAdmitOtherHintsLocationsOrControlSql()throws Exception{
        for(String sql:java.util.List.of(
                "SELECT /*+ SET_VAR(sql_mode='ANSI_QUOTES') */ 1",
                "SELECT /*!50000 SQL_NO_CACHE */ 1",
                "SELECT /*T! SET_VAR(tidb_snapshot='old') */ 1",
                "SELECT /* + MAX_EXECUTION_TIME(1) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(0) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(-1) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(01) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1+1) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(?) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1e3) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(2147483648) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(999999999999999999999999) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1) SET_VAR(sql_mode='ANSI_QUOTES') */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1) MAX_EXECUTION_TIME(2) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1) */ /*+ MAX_EXECUTION_TIME(2) */ 1",
                "/*+ MAX_EXECUTION_TIME(1) */ SELECT 1",
                "SELECT 1 /*+ MAX_EXECUTION_TIME(1) */",
                "SELECT (SELECT /*+ MAX_EXECUTION_TIME(1) */ 1)",
                "UPDATE /*+ MAX_EXECUTION_TIME(1) */ business SET v=1",
                "SELECT 1; /*+ MAX_EXECUTION_TIME(1) */",
                "SELECT /*+ MAX_EXECUTION_TIME(1) */ 1; DELETE FROM business",
                "SELECT /*+ MAX_EXECUTION_TIME(1) */ * FROM writer_registry FOR UPDATE",
                "SELECT /*+ MAX_EXECUTION_TIME(1) */ GET_LOCK('x',1)",
                "SELECT /*+ MAX_EXECUTION_TIME(1) */ 1 INTO OUTFILE 'x'")){
            var f=fixture();
            assertEquals("42000",assertThrows(SQLException.class,()->f.managed.prepareStatement(sql),sql).getSQLState());
            verify(f.raw,never()).prepareStatement(anyString());
        }
        assertThrows(SQLException.class,()->AuthoritySql.deadlineCaps(
                "SET SESSION MAX_EXECUTION_TIME=2147483648,TIDB_DML_MAX_EXECUTION_TIME=1"));
    }

    @Test void fixedFirstMatchHintsRemainPerSelectHeadAndKeepDriverSql()throws Exception{
        // RecommendStore两个现producer无exclude时的原SQL；保留五个nested FIRSTMATCH、连接顺序、窗口与参数。
        String mutual="""
                SELECT f2.friend_player_id, COUNT(*) AS mutual
                FROM friend f1 FORCE INDEX (PRIMARY)
                STRAIGHT_JOIN friend f2 FORCE INDEX (PRIMARY) ON f2.player_id = f1.friend_player_id
                WHERE f1.player_id = ?
                  AND f2.friend_player_id <> ?
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend f FORCE INDEX (PRIMARY)
                        WHERE f.player_id = ? AND f.friend_player_id = f2.friend_player_id)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_out FORCE INDEX (PRIMARY)
                        WHERE b_out.player_id = ? AND b_out.blocked_player_id = f2.friend_player_id)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_in FORCE INDEX (PRIMARY)
                        WHERE b_in.player_id = f2.friend_player_id AND b_in.blocked_player_id = ?)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_out FORCE INDEX (PRIMARY)
                        WHERE r_out.from_player_id = ? AND r_out.to_player_id = f2.friend_player_id AND r_out.status = 1)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_in FORCE INDEX (PRIMARY)
                        WHERE r_in.from_player_id = f2.friend_player_id AND r_in.to_player_id = ? AND r_in.status = 1)
                GROUP BY f2.friend_player_id
                ORDER BY mutual DESC, RAND()
                LIMIT ?""";
        String anchor="""
                SELECT c.player_id, 0 AS mutual
                FROM (SELECT DISTINCT player_id FROM friend FORCE INDEX (PRIMARY)
                      WHERE player_id >= ?
                      ORDER BY player_id
                      LIMIT ?) AS c
                WHERE c.player_id <> ?
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend f FORCE INDEX (PRIMARY)
                        WHERE f.player_id = ? AND f.friend_player_id = c.player_id)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_out FORCE INDEX (PRIMARY)
                        WHERE b_out.player_id = ? AND b_out.blocked_player_id = c.player_id)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_block b_in FORCE INDEX (PRIMARY)
                        WHERE b_in.player_id = c.player_id AND b_in.blocked_player_id = ?)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_out FORCE INDEX (PRIMARY)
                        WHERE r_out.from_player_id = ? AND r_out.to_player_id = c.player_id AND r_out.status = 1)
                  AND NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM friend_request r_in FORCE INDEX (PRIMARY)
                        WHERE r_in.from_player_id = c.player_id AND r_in.to_player_id = ? AND r_in.status = 1)
                ORDER BY c.player_id
                LIMIT ?""";
        for(String sql:java.util.List.of(mutual,anchor,
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1",
                "select\t/*+\r\nsemijoin(firstmatch)\t*/ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1) SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) MAX_EXECUTION_TIME(2147483647) */ 1",
                "SELECT (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1), (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 2)",
                "SELECT /*+ MAX_EXECUTION_TIME(7) */ 1 WHERE NOT EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 FROM business WHERE v=?)")){
            var f=fixture();PreparedStatement driver=mock(PreparedStatement.class);
            when(f.raw.prepareStatement(sql)).thenReturn(driver);
            try(var managed=f.managed.prepareStatement(sql)){
                assertNotSame(driver,managed);
                assertEquals(AuthoritySql.Kind.READ,AuthoritySql.validate(sql));
                verify(f.raw).prepareStatement(sql);
                verify(driver,never()).executeQuery();
            }
        }
    }

    @Test void firstMatchHintGrammarDoesNotAdmitOtherPlansOrDuplicateHeads()throws Exception{
        for(String sql:java.util.List.of(
                "SELECT /*+ SET_VAR(sql_mode='ANSI_QUOTES') SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) SET_VAR(max_execution_time=1) */ 1",
                "SELECT /*+ SEMIJOIN(LOOSESCAN) */ 1",
                "SELECT /*+ SEMIJOIN(MATERIALIZATION) */ 1",
                "SELECT /*+ NO_SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH,LOOSESCAN) */ 1",
                "SELECT /*+ SEMIJOIN(@q FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN (FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN( FIRSTMATCH ) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1) MAX_EXECUTION_TIME(2) SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) MAX_EXECUTION_TIME(1) MAX_EXECUTION_TIME(2) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ /*+ SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ MAX_EXECUTION_TIME(1) */ /*+ SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ /*+ MAX_EXECUTION_TIME(1) */ 1",
                "SELECT /* ordinary */ /*+ SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT -- ordinary\n/*+ SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT 1 /*+ SEMIJOIN(FIRSTMATCH) */",
                "/*+ SEMIJOIN(FIRSTMATCH) */ SELECT 1",
                "SELECT /* + SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+\u00a0SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT (SELECT /*+ MAX_EXECUTION_TIME(1) SEMIJOIN(FIRSTMATCH) */ 1)",
                "SELECT /*+ MAX_EXECUTION_TIME(0) SEMIJOIN(FIRSTMATCH) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) MAX_EXECUTION_TIME(2147483648) */ 1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH), MAX_EXECUTION_TIME(1) */ 1",
                "WITH x AS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1) SELECT 1",
                "INSERT INTO business(v) SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1",
                "DELETE FROM business WHERE EXISTS (SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1)",
                "UPDATE /*+ SEMIJOIN(FIRSTMATCH) */ business SET v=1",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1; DELETE FROM business",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ * FROM writer_registry FOR UPDATE",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ GET_LOCK('x',1)",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ 1 INTO OUTFILE 'x'",
                "SELECT /*+ SEMIJOIN(FIRSTMATCH) */ /*!50000 SQL_NO_CACHE */ 1",
                "SELECT /*T! SET_VAR(tidb_snapshot='old') */ /*+ SEMIJOIN(FIRSTMATCH) */ 1")){
            var f=fixture();
            assertEquals("42000",assertThrows(SQLException.class,()->f.managed.prepareStatement(sql),sql).getSQLState());
            verify(f.raw,never()).prepareStatement(anyString());
        }
    }

}
