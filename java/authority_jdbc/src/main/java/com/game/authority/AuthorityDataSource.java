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
    private final String url;
    private final Properties properties;
    private final WriterAuthority authority;
    private final boolean requireTiDb;
    private final List<String> initialization;
    public AuthorityDataSource(String url,Properties credentials,WriterAuthority authority,boolean requireTiDb,List<String> initialization)throws SQLException {
        this.url=Objects.requireNonNull(url);this.properties=new Properties();this.properties.putAll(credentials);
        this.authority=Objects.requireNonNull(authority);this.requireTiDb=requireTiDb;
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
        try{initialize(raw,requireTiDb,initialization);return AuthorityConnection.wrap(raw,authority);}
        catch(SQLException|RuntimeException|Error failure){try{raw.close();}catch(SQLException close){failure.addSuppressed(close);}throw failure;}
    }
    /** 仅physical adapter安装阶段使用；输入必须是上面已审固定SQL等集。 */
    public static void initialize(Connection raw,boolean requireTiDb,List<String> sqls)throws SQLException{
        if(!raw.getAutoCommit())throw new SQLException("新physical连接已有事务");
        List<String> accepted=checkedInitialization(sqls);
        String product=raw.getMetaData().getDatabaseProductName();
        boolean h2="H2".equals(product);
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

