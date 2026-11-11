package com.game.authority;

import javax.sql.DataSource;
import java.io.PrintWriter;
import java.sql.*;
import java.util.*;
import java.util.logging.Logger;

/**
 * 池内侧的physical DataSource adapter。每次新连接先初始化/核验，再交受管Connection；
 * getConnection(user,password)不提供切换principal的旁门。
 */
public final class AuthorityDataSource implements DataSource {
    public static final String STRONG_SESSION_SQL="SET SESSION tidb_snapshot='',tidb_read_staleness=0,"
            +"tidb_low_resolution_tso=OFF,tidb_read_consistency='strict',tidb_replica_read='leader',"
            +"tidb_txn_mode='pessimistic',transaction_isolation='READ-COMMITTED',"
            +"sql_mode=CONCAT(@@SESSION.sql_mode,',STRICT_TRANS_TABLES')";
    private static final String J_MALL_INIT="SET SESSION tidb_snapshot='',tidb_txn_mode='pessimistic',tidb_replica_read='leader',tidb_read_consistency='strict',"
            +"tidb_read_staleness=0,tidb_low_resolution_tso=OFF,"
            +"sql_mode=CONCAT(@@SESSION.sql_mode,',STRICT_TRANS_TABLES'),innodb_lock_wait_timeout=5";
    private static final String J_MALL_MYSQL="SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode,',STRICT_TRANS_TABLES'),innodb_lock_wait_timeout=5";
    private static final String R_MALL_INIT="SET SESSION tidb_snapshot='',tidb_txn_mode='pessimistic',tidb_replica_read='leader',tidb_read_consistency='strict',tidb_read_staleness=0,tidb_low_resolution_tso=OFF,sql_mode='STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION',innodb_lock_wait_timeout=5";
    private static final String R_MALL_MYSQL="SET SESSION sql_mode='STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION',innodb_lock_wait_timeout=5";
    /** mysql 档逐物理连接的核对,与 Go 的 authoritysql(mysqlServerSQL)同一组判据。 */
    static final String MYSQL_SERVER_SQL="SELECT VERSION(),@@version_comment,@@read_only,@@super_read_only";
    /** mysql 档的会话固定:与 TiDB 档同样是 RC 加严格模式,不发任何 tidb_* 语句。 */
    static final String MYSQL_SESSION_SQL="SET SESSION transaction_isolation='READ-COMMITTED',sql_mode=CONCAT(@@SESSION.sql_mode,',STRICT_TRANS_TABLES')";

    /**
     * 物理连接按哪一档核对(主仓 docs/design/no-single-node-horizontal-scaling-20261001.md §6;Go 侧同名档位见 shared/authoritysql/backend.go):
     * TIDB 只认真实 TiDB 并固定强会话;MYSQL 只认可写的 MySQL 主库(不是 TiDB、不是只读实例)并固定 RC;
     * ANY 不核对端,只给 dev / test 的本机库与 H2 夹具。
     */
    public enum Backend {
        TIDB, MYSQL, ANY;

        /**
         * 配置 → 档位,规则只在这里。backend 取 mmorpg.sql.backend(tidb | mysql);requireTiDb 取 mmorpg.sql.require-tidb,只对 tidb 档有意义;
         * runMode 取 mmorpg.run-mode。tidb 档关掉强核验只许 dev / test;mysql 档在任何运行模式下都按主库严格核对。
         */
        public static Backend resolve(String backend,boolean requireTiDb,String runMode){
            String name=backend==null?"tidb":backend.trim().toLowerCase(Locale.ROOT);
            switch(name){
                case "tidb":
                    if(requireTiDb)return TIDB;
                    if(Set.of("dev","test").contains(runMode))return ANY;
                    throw new IllegalStateException("mmorpg.sql.require-tidb=false turns off the TiDB session check and is only allowed with mmorpg.run-mode dev or test, got "+runMode);
                case "mysql":
                    return MYSQL;
                default:
                    throw new IllegalStateException("mmorpg.sql.backend must be tidb or mysql, got "+backend);
            }
        }
    }

    private final String url;
    private final Properties properties;
    private final WriterAuthority authority;
    private final Backend backend;
    private final List<String> initialization;
    /** 旧调用方的写法:requireTiDb=true 是 TIDB 档,false 是 ANY 档(只给 dev / test)。 */
    public AuthorityDataSource(String url,Properties credentials,WriterAuthority authority,boolean requireTiDb,List<String> initialization)throws SQLException {
        this(url,credentials,authority,requireTiDb?Backend.TIDB:Backend.ANY,initialization);
    }
    public AuthorityDataSource(String url,Properties credentials,WriterAuthority authority,Backend backend,List<String> initialization)throws SQLException {
        this.url=Objects.requireNonNull(url);this.properties=new Properties();this.properties.putAll(credentials);
        this.authority=Objects.requireNonNull(authority);this.backend=Objects.requireNonNull(backend);
        validateUrl(url);
        this.initialization=checkedInitialization(initialization);
        validateProperties(credentials);
        validateProperties(this.properties);
    }
    /** CLI也使用同一NORMAL启动原件与独立主体，URL不得藏另一套账号。 */
    public static AuthorityDataSource fromEnvironment(String url,Map<String,String> env)throws SQLException{
        if(url==null||url.matches("(?is).*[?&](user|password|token)=.*"))throw new SQLException("NORMAL CLI连接串不得携带主体或秘密");
        String user=env.get("MMORPG_SQL_USERNAME"),password=env.get("MMORPG_SQL_PASSWORD");
        if(user==null||user.isBlank()||password==null)throw new SQLException("缺少独立NORMAL主体环境");
        Properties credentials=new Properties();credentials.setProperty("user",user);credentials.setProperty("password",password);
        try{return new AuthorityDataSource(url,credentials,WriterAuthority.consume(env),true,List.of());}
        catch(java.io.IOException invalid){throw new SQLException("NORMAL CLI启动资格不可消费",invalid);}
    }
    public static List<String> checkedInitialization(Collection<String> sqls)throws SQLException {
        List<String> result=new ArrayList<>();
        for(String sql:sqls){
            if(!Set.of(STRONG_SESSION_SQL,J_MALL_INIT,J_MALL_MYSQL,R_MALL_INIT,R_MALL_MYSQL).contains(sql))throw new SQLException("未批准的池初始化SQL，不能在受管前执行");
            if(!result.contains(sql))result.add(sql);
        }
        return List.copyOf(result);
    }
    private static final Set<String> FORBIDDEN_BOOLEAN_PROPERTIES=Set.of("allowmultiqueries","autoreconnect","autoreconnectforpools","reconnectattxend","createdatabaseifnotexist");
    private static void validateProperty(String key,String value)throws SQLException{
        String name=key.toLowerCase(Locale.ROOT);
        if(name.equals("useconfigs")||name.equals("propertiestransform"))throw new SQLException("normal连接不得加载隐藏属性变换");
        // Connector/J还接受YES等真值。这里只接受明确false，未知值也不能放行给driver解释。
        if(FORBIDDEN_BOOLEAN_PROPERTIES.contains(name)&&!"false".equalsIgnoreCase(value))throw new SQLException("normal连接不得启用重放、多语句或建库属性");
    }
    public static void validateProperties(Properties properties)throws SQLException{
        if(properties==null)return;
        for(Object key:properties.keySet()){
            if(!(key instanceof String name)||!(properties.get(key) instanceof String value))throw new SQLException("JDBC属性必须为字符串");
            validateProperty(name,value);
        }
        for(String name:properties.stringPropertyNames())validateProperty(name,properties.getProperty(name));
    }
    public static void validateUrl(String url)throws SQLException{
        if(url==null)throw new SQLException("JDBC URL缺失");
        int query=url.indexOf('?');
        String target=query<0?url:url.substring(0,query);
        if(target.indexOf('(')>=0||target.indexOf(')')>=0)throw new SQLException("受管physical URL不接受内嵌连接属性");
        if(query<0)return;
        for(String entry:url.substring(query+1).split("[&;]",-1)){
            if(entry.isEmpty())continue;
            int equals=entry.indexOf('=');
            try{
                String key=java.net.URLDecoder.decode(equals<0?entry:entry.substring(0,equals),java.nio.charset.StandardCharsets.UTF_8);
                String value=equals<0?"":java.net.URLDecoder.decode(entry.substring(equals+1),java.nio.charset.StandardCharsets.UTF_8);
                validateProperty(key,value);
            }catch(IllegalArgumentException malformed){throw new SQLException("JDBC URL属性编码无效");}
        }
    }
    @Override public Connection getConnection()throws SQLException{
        Connection raw=DriverManager.getConnection(url,properties);
        try{initialize(raw,backend,initialization);return AuthorityConnection.wrap(raw,authority);}
        catch(SQLException|RuntimeException|Error failure){try{raw.close();}catch(SQLException close){failure.addSuppressed(close);}throw failure;}
    }
    /** 仅physical adapter安装阶段使用；输入必须是上面已审固定SQL等集。requireTiDb=true 是 TIDB 档,false 是 ANY 档。 */
    public static void initialize(Connection raw,boolean requireTiDb,List<String> sqls)throws SQLException{
        initialize(raw,requireTiDb?Backend.TIDB:Backend.ANY,sqls);
    }
    public static void initialize(Connection raw,Backend backend,List<String> sqls)throws SQLException{
        if(!raw.getAutoCommit())throw new SQLException("新physical连接已有事务");
        List<String> accepted=checkedInitialization(sqls);
        String product=raw.getMetaData().getDatabaseProductName();
        boolean h2="H2".equals(product);
        if(backend==Backend.MYSQL){
            initializeMySqlPrimary(raw,h2,accepted);
            return;
        }
        boolean requireTiDb=backend==Backend.TIDB;
        boolean tidb=false;
        if(!h2)try(var s=raw.createStatement();var r=s.executeQuery("SELECT VERSION()")){tidb=r.next()&&r.getString(1).toLowerCase(Locale.ROOT).contains("tidb");}
        if(requireTiDb&&!tidb)throw new SQLException("normal生产连接必须是真实TiDB");
        for(String sql:accepted){
            if(sql.equals(STRONG_SESSION_SQL))continue;
            if(h2)throw new SQLException("H2 fixture不能伪执行TiDB初始化");
            if(!tidb&&sql.contains("tidb_"))throw new SQLException("非TiDB不能接受TiDB会话初始化");
            try(var statement=raw.createStatement()){statement.execute(sql);}
        }
        if(tidb){
            try(var s=raw.createStatement()){s.execute(STRONG_SESSION_SQL);}
            try(var s=raw.createStatement();var r=s.executeQuery("SELECT @@SESSION.tidb_snapshot,@@SESSION.tidb_read_staleness,@@SESSION.tidb_low_resolution_tso,@@SESSION.tidb_read_consistency,@@SESSION.tidb_replica_read,@@SESSION.tidb_txn_mode,@@SESSION.transaction_isolation")){
                if(!r.next()||!r.getString(1).isEmpty()||r.getLong(2)!=0||r.getBoolean(3)
                        ||!"strict".equalsIgnoreCase(r.getString(4))||!"leader".equalsIgnoreCase(r.getString(5))
                        ||!"pessimistic".equalsIgnoreCase(r.getString(6))||!"READ-COMMITTED".equalsIgnoreCase(r.getString(7)))
                    throw new SQLException("physical强会话初始化没有精确读回");
            }
        }
    }
    /**
     * mysql 档:先核对端再动会话。每条新的物理连接都核:主从切换之后同一地址可能已经是从库,只在启动时核一次拦不住。
     * TiDB 的强会话语句(STRONG_SESSION_SQL)跳过,别的带 tidb_* 的初始化语句拒绝。
     */
    private static void initializeMySqlPrimary(Connection raw,boolean h2,List<String> accepted)throws SQLException{
        if(h2)throw new SQLException("mmorpg.sql.backend=mysql needs a MySQL primary, got H2");
        try(var s=raw.createStatement();var r=s.executeQuery(MYSQL_SERVER_SQL)){
            if(!r.next())throw new SQLException("mmorpg.sql.backend=mysql could not read the server version and read-only flags");
            checkMySqlServer(r.getString(1),r.getString(2),r.getLong(3),r.getLong(4));
        }
        for(String sql:accepted){
            if(sql.equals(STRONG_SESSION_SQL))continue;
            if(sql.contains("tidb_"))throw new SQLException("mmorpg.sql.backend=mysql does not accept TiDB session initialization");
            try(var statement=raw.createStatement()){statement.execute(sql);}
        }
        try(var s=raw.createStatement()){s.execute(MYSQL_SESSION_SQL);}
        try(var s=raw.createStatement();var r=s.executeQuery("SELECT @@SESSION.transaction_isolation")){
            if(!r.next()||!"READ-COMMITTED".equalsIgnoreCase(r.getString(1)))throw new SQLException("mmorpg.sql.backend=mysql session did not read back READ-COMMITTED");
        }
    }
    /**
     * mysql 档的纯判定,与 Go 的 authoritysql.checkMySQLServer 同一判据:版本串与 version_comment 都看(TiDB 配了 server-version 时
     * VERSION() 里没有 TiDB 字样);只读实例一律拒绝 —— 持 SUPER 的账号写得进从库,写进去就是静默分叉,权威读也要读到最新提交。
     */
    static void checkMySqlServer(String version,String comment,long readOnly,long superReadOnly)throws SQLException{
        String text=((version==null?"":version)+" "+(comment==null?"":comment)).toLowerCase(Locale.ROOT);
        if(text.contains("tidb"))throw new SQLException("mmorpg.sql.backend=mysql is connected to TiDB");
        if(readOnly!=0||superReadOnly!=0)throw new SQLException("mmorpg.sql.backend=mysql must connect to the primary (read_only="+readOnly+" super_read_only="+superReadOnly+")");
    }
    @Override public Connection getConnection(String user,String password)throws SQLException{
        if(!Objects.equals(user,properties.getProperty("user"))||!Objects.equals(password,properties.getProperty("password")))
            throw new SQLException("受管DataSource不能切换启动principal");
        return getConnection();
    }
    @Override public PrintWriter getLogWriter(){return DriverManager.getLogWriter();}
    @Override public void setLogWriter(PrintWriter out){DriverManager.setLogWriter(out);}
    @Override public void setLoginTimeout(int seconds){DriverManager.setLoginTimeout(seconds);}
    @Override public int getLoginTimeout(){return DriverManager.getLoginTimeout();}
    @Override public Logger getParentLogger(){return Logger.getLogger("com.game.common.sql");}
    @Override public boolean isWrapperFor(Class<?> type){return type!=null&&type.isInstance(this);}
    @Override public <T>T unwrap(Class<T> type)throws SQLException{
        if(isWrapperFor(type))return type.cast(this);throw new SQLException("不能unwrap原physical DataSource");
    }
}

