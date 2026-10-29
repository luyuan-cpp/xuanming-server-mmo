package com.game.authority;

import java.io.*;
import java.lang.reflect.*;
import java.math.*;
import java.net.URL;
import java.sql.*;
import java.util.*;
import java.util.concurrent.Executor;
import java.util.concurrent.atomic.AtomicBoolean;
import javax.sql.rowset.*;

/**
 * physical driver内侧的完整标准JDBC代理。业务看见的DruidPooledConnection保持原类型；
 * 此对象不暴露vendor连接，所有Statement/ResultSet/metadata回链仍回到本受管handle。
 */
public final class AuthorityConnection implements InvocationHandler {
    /** 仅请求同handle的正常首锁，不带维护、注册或raw控制能力。 */
    public interface Fence { void requireNormalTransaction() throws SQLException; }
    /** 物化资源边界可由启动配置收紧/放宽；绝不更改updateCount，超限在COMMIT前完整回滚。 */
    public record KeyLimits(int rows,long bytes,int columns,long millis){
        public KeyLimits{if(rows<1||bytes<1||columns<1||millis<1)throw new IllegalArgumentException("keys资源上限必须为正数");}
        static KeyLimits environment()throws SQLException{
            try{return new KeyLimits(Integer.parseInt(System.getenv().getOrDefault("MMORPG_SQL_KEYS_MAX_ROWS","4096")),
                    Long.parseLong(System.getenv().getOrDefault("MMORPG_SQL_KEYS_MAX_BYTES","16777216")),
                    Integer.parseInt(System.getenv().getOrDefault("MMORPG_SQL_KEYS_MAX_COLUMNS","256")),
                    Long.parseLong(System.getenv().getOrDefault("MMORPG_SQL_KEYS_MAX_MILLIS","30000")));}
            catch(IllegalArgumentException invalid){throw new SQLException("generated keys资源配置非法",invalid);}
        }
    }
    private final KeyLimits keyLimits;
    private long sessionDeadlineUntil;
    private final Connection physical;
    private final WriterAuthority authority;
    private final Connection view;
    private final Set<StatementOwner> statements=Collections.newSetFromMap(new IdentityHashMap<>());
    private final Set<ValueStream> valueStreams=Collections.newSetFromMap(new IdentityHashMap<>());
    private final AtomicBoolean unknown=new AtomicBoolean();
    private boolean auto=true,readOnly,closed,ending,serverAborted;
    private volatile boolean begun;
    private volatile StatementOwner activeAuto;
    private AuthorityConnection(Connection physical,WriterAuthority authority)throws SQLException {
        this.physical=Objects.requireNonNull(physical);this.authority=Objects.requireNonNull(authority);this.keyLimits=KeyLimits.environment();
        if(!physical.getAutoCommit())throw new SQLException("受管安装不能继承已有事务");
        view=(Connection)Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),
                new Class<?>[]{Connection.class,Fence.class},this);
    }
    public static Connection wrap(Connection physical,WriterAuthority authority)throws SQLException {
        authority.verifyPhysical(physical);return new AuthorityConnection(physical,authority).view;
    }
    public static void require(Connection connection)throws SQLException {
        connection.unwrap(Fence.class).requireNormalTransaction();
    }
    @Override public Object invoke(Object proxy,Method method,Object[] args)throws Throwable {
        if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"AuthorityConnection");
        synchronized(this) {
            String name=method.getName();
            if(name.equals("isClosed"))return closed||physical.isClosed();
            if(name.equals("close")){close();return null;}
            if(name.equals("abort")){poison();physical.abort((Executor)args[0]);closed=true;return null;}
            if(name.equals("isWrapperFor"))return ((Class<?>)args[0]).isInstance(view);
            if(name.equals("unwrap"))return unwrap(view,(Class<?>)args[0]);
            healthy();
            if(name.equals("requireNormalTransaction")){
                if(auto)throw new SQLException("领域提交必须使用显式事务","25000");begin();return null;
            }
            if(name.equals("getAutoCommit"))return auto;
            if(name.equals("setAutoCommit")){
                boolean next=(Boolean)args[0];
                if(auto&&!next&&begun)throw new SQLException("先完成活动auto operation再开始显式事务","HY010");
                if(next&&!auto&&begun)finish(!serverAborted);auto=next;return null;
            }
            if(name.equals("commit")){if(auto)throw new SQLException("autoCommit不能显式commit","25000");finish(true);return null;}
            if(name.equals("rollback")){
                if(args==null||args.length==0){if(auto)throw new SQLException("autoCommit不能显式rollback","25000");finish(false);return null;}
                if(auto)throw new SQLException("savepoint必须为显式事务","25000");
                Point point=point(args[0]);delegate(() -> {physical.rollback(point.raw);return null;});return null;
            }
            if(name.equals("setSavepoint")){
                if(auto)throw new SQLException("savepoint必须为显式事务","25000");begin();
                Savepoint raw=delegate(() -> args==null||args.length==0?physical.setSavepoint():physical.setSavepoint((String)args[0]));
                return new Point(raw);
            }
            if(name.equals("releaseSavepoint")){
                Point point=point(args[0]);delegate(() -> {physical.releaseSavepoint(point.raw);return null;});point.released=true;return null;
            }
            if(name.equals("prepareCall"))throw new SQLFeatureNotSupportedException("normal不执行CALL","0A000");
            if(name.equals("createStatement")||name.equals("prepareStatement")){
                String sql=name.equals("prepareStatement")?(String)args[0]:null;
                AuthoritySql.Kind kind=sql==null?null:AuthoritySql.validate(sql);
                Statement raw=(Statement)call(physical,method,args);
                StatementOwner owner=new StatementOwner(raw,sql,kind,name.equals("prepareStatement")&&requestedKeys(args));
                statements.add(owner);return owner.view;
            }
            if(name.equals("getMetaData"))return metadata(physical.getMetaData(),null);
            if(name.equals("setReadOnly")){if(begun)throw new SQLException("事务内不能切换只读属性","25001");readOnly=(Boolean)args[0];return null;}
            if(name.equals("isReadOnly"))return readOnly;
            if(name.equals("setCatalog")||name.equals("setSchema")){
                if(begun)throw new SQLException("事务内不能切换schema","25001");
                Object current=name.equals("setCatalog")?physical.getCatalog():physical.getSchema();
                if(!Objects.equals(current,args[0]))throw new SQLException("受管handle的schema固定于启动grant","25001");return null;
            }
            if(name.equals("setTransactionIsolation")){
                if(begun||(Integer)args[0]==Connection.TRANSACTION_NONE)throw new SQLException("事务隔离切换时机或值非法","25001");
            }
            if(name.startsWith("setShardingKey"))throw new SQLFeatureNotSupportedException("不得重路由已固定physical authority","0A000");
            if(name.equals("nativeSQL")){String sql=(String)args[0];AuthoritySql.validate(sql);return sql;}
            if(name.equals("beginRequest"))return null;
            if(name.equals("endRequest")){if(begun)finish(false);return null;}
            return safeValue(call(physical,method,args),null,false);
        }
    }
    private void healthy()throws SQLException {
        if(closed||physical.isClosed())throw new SQLNonTransientConnectionException("连接已关闭","08003");
        if(unknown.get())throw new SQLNonTransientConnectionException("原事务结局未知，禁止继续或重放","08007");
    }
    private void begin()throws SQLException {
        healthy();if(serverAborted)throw new SQLException("服务器已中止原事务，仅允许完整rollback","25000");
        if(!begun){try{authority.begin(physical);begun=true;}catch(SQLException failure){if(uncertain(failure)){poison();throw unknown(failure);}throw failure;}}
    }
    private void finish(boolean commit)throws SQLException {
        healthy();if(!begun)return;
        if(commit&&serverAborted)throw new SQLException("服务器已中止原事务，不能commit","25000");
        if(ending)throw new SQLException("事务结束过程重入","HY010");
        if(commit&&!valueStreams.isEmpty())throw new SQLException("可写LOB流尚未关闭，不能提交","HY010");ending=true;
        try {
            if(!commit)for(ValueStream stream:List.copyOf(valueStreams))stream.discardForRollback();
            if(commit)physical.commit();else physical.rollback();
            physical.setAutoCommit(true);begun=false;serverAborted=false;activeAuto=null;transactionId++;
        }catch(SQLException|RuntimeException|Error failure){poison();throw unknown(failure);}
        finally{ending=false;}
    }
    private void close()throws SQLException {
        if(closed)return;
        SQLException failure=null;
        try {
            if(!unknown.get()){
                for(StatementOwner owner:List.copyOf(statements))owner.close();
                if(begun)finish(false);
            }
        }catch(SQLException problem){failure=problem;poison();}
        finally{closed=true;try{physical.close();}catch(SQLException close){if(failure==null)failure=close;else failure.addSuppressed(close);}}
        if(failure!=null)throw failure;
    }
    private void poison(){
        unknown.set(true);
        try{physical.abort(Runnable::run);}catch(SQLException|RuntimeException ignored){try{physical.close();}catch(SQLException ignoredClose){}}
    }
    private static SQLNonTransientConnectionException unknown(Throwable cause){
        return new SQLNonTransientConnectionException("事务或取消结局未知，物理连接已隔离，保留原业务key","08007");
    }
    private static boolean uncertain(SQLException error){
        String state=error.getSQLState();
        return error instanceof SQLRecoverableException||error instanceof SQLNonTransientConnectionException
                ||error instanceof SQLTransientConnectionException||state!=null&&(state.startsWith("08")||state.equals("70100"))
                ||error.getErrorCode()==1317;
    }
    private static boolean aborted(SQLException error){return error.getErrorCode()==1213||"40001".equals(error.getSQLState());}
    @FunctionalInterface private interface Work<T>{T run()throws SQLException;}
    private <T>T delegate(Work<T> work)throws SQLException {
        healthy();
        try{return work.run();}
        catch(SQLException failure){if(uncertain(failure)){poison();throw unknown(failure);}else if(aborted(failure))serverAborted=true;throw failure;}
        catch(RuntimeException|Error fatal){poison();throw fatal;}
    }
    private Point point(Object supplied)throws SQLException {
        healthy();if(serverAborted)throw new SQLException("原事务已中止","25000");
        if(!(supplied instanceof Point point)||point.owner!=this||point.released||!begun||point.transaction!=transactionId)
            throw new SQLException("savepoint不属于当前事务","3B001");
        return point;
    }
    private long transactionId;
    private final class Point implements Savepoint {
        private final AuthorityConnection owner=AuthorityConnection.this;
        private final Savepoint raw;private final long transaction=transactionId;private boolean released;
        Point(Savepoint raw){this.raw=raw;}
        public int getSavepointId()throws SQLException{point(this);return raw.getSavepointId();}
        public String getSavepointName()throws SQLException{point(this);return raw.getSavepointName();}
    }

    private final class StatementOwner implements InvocationHandler {
        final Statement raw;final Statement view;final String preparedSql;final AuthoritySql.Kind preparedKind;final boolean preparedKeys;
        final Map<ResultSet,Rows> rows=new IdentityHashMap<>();
        final AtomicBoolean executing=new AtomicBoolean();
        boolean closed,generic,sequenceComplete,cleanup,batchWrites,captureKeys;
        CopyBudget operationBudget;
        ResultSet keys;
        StatementOwner(Statement raw,String sql,AuthoritySql.Kind kind,boolean keysRequested){
            this.raw=raw;preparedSql=sql;preparedKind=kind;preparedKeys=keysRequested;
            view=(Statement)Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),
                    new Class<?>[]{raw instanceof PreparedStatement?PreparedStatement.class:Statement.class},this);
        }
        public Object invoke(Object proxy,Method method,Object[] args)throws Throwable {
            String name=method.getName();
            if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"AuthorityStatement");
            if(name.equals("cancel")){
                if(executing.get()||begun&&activeAuto==this){unknown.set(true);try{raw.cancel();}finally{poison();}}
                else raw.cancel();
                return null;
            }
            synchronized(AuthorityConnection.this){
                if(name.equals("close")){close();return null;}
                if(name.equals("isClosed"))return closed||raw.isClosed();
                if(name.equals("getConnection"))return AuthorityConnection.this.view;
                if(name.equals("unwrap"))return unwrap(view,(Class<?>)args[0]);
                if(name.equals("isWrapperFor"))return ((Class<?>)args[0]).isInstance(view);
                healthy();if(closed)throw new SQLException("Statement已关闭","HY010");
                if(name.equals("getGeneratedKeys")){
                    if(keys!=null)return keys;
                    if(auto&&!begun){
                        try(ResultSet original=raw.getGeneratedKeys()){keys=Snapshot.capture(original,new CopyBudget(raw.getQueryTimeout(),physical.getNetworkTimeout(),keyLimits,0),this);return keys;}
                    }
                    ResultSet original=raw.getGeneratedKeys();return wrapRows(original,this,false).view;
                }
                if(name.equals("getResultSet")){
                    ResultSet original=raw.getResultSet();return original==null?null:wrapRows(original,this,true).view;
                }
                if(name.equals("getMoreResults")){
                    int policy=args==null||args.length==0?Statement.CLOSE_CURRENT_RESULT:(Integer)args[0];
                    if(policy!=Statement.KEEP_CURRENT_RESULT){
                        ResultSet current=raw.getResultSet();Rows prior=current==null?null:rows.get(current);
                        if(prior!=null)prior.completed=true;
                        if(policy==Statement.CLOSE_ALL_RESULTS)for(Rows row:rows.values())row.completed=true;
                    }
                    boolean next=(Boolean)call(raw,method,args);
                    sequenceComplete=!next&&raw.getUpdateCount()==-1;
                    if(next)wrapRows(raw.getResultSet(),this,true);
                    completeIfReady();return next;
                }
                if(name.equals("addBatch")){
                    AuthoritySql.Kind kind=preparedKind;
                    if(args!=null&&args.length>0&&args[0] instanceof String sql)kind=AuthoritySql.validate(sql);
                    if(kind==AuthoritySql.Kind.DEADLINE)throw new SQLSyntaxErrorException("期限SET不能加入batch","42000");
                    if(kind==AuthoritySql.Kind.WRITE)batchWrites=true;
                    return call(raw,method,args);
                }
                if(name.equals("clearBatch")){Object value=call(raw,method,args);batchWrites=false;return value;}
                if(name.equals("executeBatch")||name.equals("executeLargeBatch")){
                    if(auto)throw new SQLFeatureNotSupportedException("autoCommit native batch不能改变逐元素提交语义","0A000");
                    if(readOnly&&(batchWrites||preparedKind==AuthoritySql.Kind.WRITE))throw new SQLException("只读Connection不能批写业务数据","25006");
                    begin();return business(method,args);
                }
                if(name.equals("execute")||name.equals("executeQuery")||name.equals("executeUpdate")||name.equals("executeLargeUpdate")){
                    if(auto&&!valueStreams.isEmpty())throw new SQLException("可写SQL流尚未结束，不能开始另一auto operation","HY010");
                    AuthoritySql.Kind kind=preparedKind;
                    if(args!=null&&args.length>0&&args[0] instanceof String sql)kind=AuthoritySql.validate(sql);
                    if(kind==null)throw new SQLSyntaxErrorException("缺少数据SQL","42000");
                    if(kind==AuthoritySql.Kind.DEADLINE){
                        closePrevious();if(auto&&activeAuto!=null&&activeAuto!=this)throw new SQLException("已有未完成auto operation，不能夹入期限SET","HY010");
                        String control=preparedSql!=null?preparedSql:(String)args[0];long[] caps=AuthoritySql.deadlineCaps(control);
                        try{
                            Object result=call(raw,method,args);
                            try(var check=physical.createStatement();var actual=check.executeQuery("SELECT @@SESSION.max_execution_time,@@SESSION.tidb_dml_max_execution_time")){
                                if(!actual.next()||actual.getLong(1)!=caps[0]||actual.getLong(2)!=caps[1]||actual.next())throw new SQLException("期限SET没有精确读回");
                            }
                            long cap=caps[0]==0?caps[1]:caps[1]==0?caps[0]:Math.min(caps[0],caps[1]);
                            sessionDeadlineUntil=cap==0?0:System.nanoTime()+java.util.concurrent.TimeUnit.MILLISECONDS.toNanos(cap);return result;
                        }catch(Throwable failure){poison();throw unknown(failure);}
                    }
                    if(readOnly&&kind==AuthoritySql.Kind.WRITE)throw new SQLException("只读Connection不能写业务数据","25006");
                    closePrevious();
                    if(auto&&activeAuto!=null&&activeAuto!=this)throw new SQLException("同physical Connection已有未完成auto operation","HY010");
                    begin();if(auto)activeAuto=this;
                    CopyBudget budget=new CopyBudget(raw.getQueryTimeout(),physical.getNetworkTimeout(),keyLimits,sessionDeadlineUntil);
                    operationBudget=budget;captureKeys=kind==AuthoritySql.Kind.WRITE&&(preparedSql!=null?preparedKeys:requestedKeys(args));
                    try{
                        Object outcome=business(method,args);
                        if(name.equals("executeUpdate")||name.equals("executeLargeUpdate")){
                            if(auto){
                                if(captureKeys)try(ResultSet original=raw.getGeneratedKeys()){keys=Snapshot.capture(original,budget,this);}
                                budget.restore(raw,physical);captureKeys=false;finish(true);
                            }
                            return outcome;
                        }
                        if(name.equals("executeQuery")){
                            generic=false;sequenceComplete=true;return wrapRows((ResultSet)outcome,this,true).view;
                        }
                        generic=true;sequenceComplete=!(Boolean)outcome&&raw.getUpdateCount()==-1;
                        if((Boolean)outcome)wrapRows(raw.getResultSet(),this,true);
                        completeIfReady();return outcome;
                    }catch(SQLException failure){
                        if(auto&&begun&&!unknown.get()){
                            try{budget.restore(raw,physical);finish(false);}catch(SQLException rollback){failure.addSuppressed(rollback);poison();throw unknown(failure);}
                        }
                        throw failure;
                    }catch(RuntimeException|Error fatal){poison();throw fatal;}
                }
                return safeValue(call(raw,method,args),this,false);
            }
        }
        private Object business(Method method,Object[] args)throws Throwable {
            executing.set(true);
            try {
                Object result=call(raw,method,args);healthy();return result;
            }catch(SQLException failure){
                if(uncertain(failure)){poison();throw unknown(failure);}else if(aborted(failure))serverAborted=true;
                throw failure;
            }finally{executing.set(false);}
        }
        private void closePrevious()throws SQLException {
            cleanup=true;
            try{
                for(Rows row:List.copyOf(rows.values()))row.closeRaw();
                rows.clear();if(keys!=null){keys.close();keys=null;}
            }finally{cleanup=false;}
            sequenceComplete=true;completeIfReady();generic=false;
        }
        void completeIfReady()throws SQLException {
            if(!cleanup&&auto&&begun&&activeAuto==this&&sequenceComplete
                    &&rows.values().stream().noneMatch(row->row.ownsCompletion&&!row.completed)){
                if(captureKeys){
                    try(ResultSet original=raw.getGeneratedKeys()){keys=Snapshot.capture(original,operationBudget,this);}
                    operationBudget.restore(raw,physical);captureKeys=false;
                }
                finish(true);
            }
        }
        void close()throws SQLException {
            if(closed)return;
            closePrevious();
            try{raw.close();closed=true;statements.remove(this);}
            catch(SQLException failure){poison();throw failure;}
        }
    }
    private Rows wrapRows(ResultSet raw,StatementOwner owner,boolean completion)throws SQLException {
        if(raw==null)throw new SQLException("driver缺少结果集");
        if(owner!=null){Rows prior=owner.rows.get(raw);if(prior!=null)return prior;}
        Rows result=new Rows(raw,owner,completion);
        if(owner!=null)owner.rows.put(raw,result);return result;
    }
    private final class Rows implements InvocationHandler {
        final ResultSet raw,view;final StatementOwner owner;final boolean ownsCompletion;
        boolean completed,closed;
        Rows(ResultSet raw,StatementOwner owner,boolean completion){
            this.raw=raw;this.owner=owner;ownsCompletion=completion;
            view=(ResultSet)Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),new Class<?>[]{ResultSet.class},this);
        }
        public Object invoke(Object proxy,Method method,Object[] args)throws Throwable {
            synchronized(AuthorityConnection.this){
                String name=method.getName();
                if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"AuthorityResultSet");
                if(name.equals("close")){closeRaw();if(owner!=null)owner.completeIfReady();return null;}
                if(name.equals("isClosed"))return closed||raw.isClosed();
                if(name.equals("getStatement"))return owner==null?null:owner.view;
                if(name.equals("unwrap"))return unwrap(view,(Class<?>)args[0]);
                if(name.equals("isWrapperFor"))return ((Class<?>)args[0]).isInstance(view);
                healthy();if(closed)throw new SQLException("ResultSet已关闭","HY010");
                if(serverAborted)throw new SQLException("原事务已中止，结果不能继续访问","25000");
                boolean mutating=Set.of("updateRow","insertRow","deleteRow").contains(name);
                boolean moving=Set.of("next","previous","absolute","relative","first","last","beforeFirst","afterLast","refreshRow","moveToInsertRow","moveToCurrentRow","cancelRowUpdates").contains(name);
                boolean setting=name.startsWith("update");
                if((mutating||setting)&&readOnly)throw new SQLException("只读Connection不能通过updatable ResultSet写业务","25006");
                if((mutating||setting)&&owner==null)throw new SQLFeatureNotSupportedException("无业务Statement的结果集不可更新","0A000");
                if(owner!=null&&(mutating||setting||moving)&&auto&&(!valueStreams.isEmpty()||activeAuto!=null&&activeAuto!=owner))throw new SQLException("已有另一个未完成auto operation","HY010");
                if(owner!=null&&(mutating||setting||moving)){
                    // HOLD_CURSORS_OVER_COMMIT可跨事务存活；每个新事务仍须先取同handle围栏。
                    begin();if(auto)activeAuto=owner;completed=false;
                }
                Object value;
                try{value=call(raw,method,args);}
                catch(SQLException failure){if(uncertain(failure)){poison();throw unknown(failure);}if(aborted(failure))serverAborted=true;throw failure;}
                if(name.equals("next")&&Boolean.FALSE.equals(value)){
                    completed=true;if(owner!=null)owner.completeIfReady();
                }
                return safeValue(value,owner,false);
            }
        }
        void closeRaw()throws SQLException {
            if(closed)return;try{raw.close();closed=true;completed=true;}
            catch(SQLException failure){poison();throw failure;}
        }
    }
    private Object metadata(Object raw,StatementOwner statement){
        Class<?> api=raw instanceof DatabaseMetaData?DatabaseMetaData.class:raw instanceof ResultSetMetaData?ResultSetMetaData.class:ParameterMetaData.class;
        return Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),new Class<?>[]{api},(proxy,method,args)->{
            synchronized(AuthorityConnection.this){
                if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"AuthorityMetadata");
                if(method.getName().equals("getConnection"))return view;
                if(method.getName().equals("unwrap"))return unwrap(proxy,(Class<?>)args[0]);
                if(method.getName().equals("isWrapperFor"))return ((Class<?>)args[0]).isInstance(proxy);
                healthy();return safeValue(call(raw,method,args),statement,false);
            }
        });
    }
    private Object safeValue(Object value,StatementOwner statement,boolean detached)throws SQLException {
        if(value instanceof Connection)return view;
        if(value instanceof Object[] values){
            Object[] copied=values.clone();
            for(int i=0;i<values.length;i++){Object element=safeValue(values[i],statement,detached);try{copied[i]=element;}catch(ArrayStoreException vendor){throw new SQLFeatureNotSupportedException("vendor数组不能暴露未受管JDBC值","0A000");}}
            return copied;
        }
        if(value instanceof ResultSet result)return wrapRows(result,statement,false).view;
        if(value instanceof DatabaseMetaData||value instanceof ResultSetMetaData||value instanceof ParameterMetaData)return metadata(value,statement);
        Class<?> api=value instanceof NClob?NClob.class:value instanceof Clob?Clob.class:value instanceof Blob?Blob.class:
                value instanceof java.sql.Array?java.sql.Array.class:value instanceof Ref?Ref.class:value instanceof SQLXML?SQLXML.class:value instanceof Struct?Struct.class:null;
        if(api!=null)return Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),new Class<?>[]{api},(proxy,method,args)->{
            synchronized(AuthorityConnection.this){
                String name=method.getName();
                if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"AuthoritySqlValue");
                healthy();
                boolean write=name.startsWith("set")||name.equals("truncate");
                if(detached&&write)throw new SQLFeatureNotSupportedException("缓存keys是只读值","0A000");
                // 任意Result实现可外泄DOM/SAX/StAX写通道；只开放能完整包裹的JDBC字符串及流入口。
                if(value instanceof SQLXML&&name.equals("setResult"))throw new SQLFeatureNotSupportedException("SQLXML可写Result不能脱离受管流","0A000");
                if(write){
                    if(readOnly)throw new SQLException("只读Connection不能修改SQL值","25006");
                    if(auto&&(activeAuto!=null&&activeAuto!=statement||!valueStreams.isEmpty()))throw new SQLException("已有未完成auto operation","HY010");
                    begin();if(auto&&statement!=null)activeAuto=statement;
                }
                Object result;
                try{result=call(value,method,args);}
                catch(SQLException failure){
                    if(uncertain(failure)){poison();throw unknown(failure);}if(aborted(failure))serverAborted=true;
                    if(write&&auto&&begun&&!unknown.get())try{finish(false);}catch(SQLException rollback){poison();throw unknown(rollback);}
                    throw failure;
                }
                catch(RuntimeException|Error fatal){poison();throw fatal;}
                if(write&&result instanceof OutputStream stream)return managedOutput(stream,statement);
                if(write&&result instanceof Writer writer)return managedWriter(writer,statement);
                if(write&&auto)completeValue(statement);
                return safeValue(result,statement,detached);
            }
        });
        return value;
    }
    private void completeValue(StatementOwner statement)throws SQLException {
        if(!valueStreams.isEmpty())return;
        if(statement==null){if(auto&&begun&&activeAuto==null)finish(true);}
        else statement.completeIfReady();
    }
    @FunctionalInterface private interface StreamWork { void run()throws IOException; }
    private final class ValueStream {
        final Closeable raw;final StatementOwner statement;final long transaction=transactionId;boolean closed;
        ValueStream(Closeable raw,StatementOwner statement){this.raw=raw;this.statement=statement;valueStreams.add(this);}
        void perform(StreamWork work)throws IOException {
            synchronized(AuthorityConnection.this){
                try{healthy();if(closed||!begun||transaction!=transactionId)throw new SQLException("可写SQL流已不属于当前事务","HY010");if(readOnly)throw new SQLException("只读Connection不能写SQL流","25006");}
                catch(SQLException invalid){throw new IOException("受管SQL流拒绝访问",invalid);}
                try{work.run();}catch(IOException|RuntimeException failure){poison();throw new IOException("SQL流写入结局未知，physical已隔离");}catch(Error fatal){poison();throw fatal;}
            }
        }
        void close()throws IOException {
            synchronized(AuthorityConnection.this){
                if(closed)return;
                perform(raw::close);closed=true;valueStreams.remove(this);
                try{if(auto)completeValue(statement);}catch(SQLException failure){throw new IOException("SQL流完成失败",failure);}
            }
        }
        void discardForRollback()throws SQLException {
            try{raw.close();closed=true;valueStreams.remove(this);}catch(IOException|RuntimeException failure){throw new SQLException("SQL流关闭结局未知","08007");}
        }
    }
    private OutputStream managedOutput(OutputStream raw,StatementOwner statement){
        ValueStream value=new ValueStream(raw,statement);
        return new OutputStream(){
            @Override public void write(int b)throws IOException{value.perform(()->raw.write(b));}
            @Override public void write(byte[] b,int offset,int length)throws IOException{value.perform(()->raw.write(b,offset,length));}
            @Override public void flush()throws IOException{value.perform(raw::flush);}
            @Override public void close()throws IOException{value.close();}
        };
    }
    private Writer managedWriter(Writer raw,StatementOwner statement){
        ValueStream value=new ValueStream(raw,statement);
        return new Writer(){
            @Override public void write(char[] text,int offset,int length)throws IOException{value.perform(()->raw.write(text,offset,length));}
            @Override public void flush()throws IOException{value.perform(raw::flush);}
            @Override public void close()throws IOException{value.close();}
        };
    }
    private static boolean requestedKeys(Object[] args){
        if(args==null||args.length!=2)return false;
        return args[1] instanceof Integer flag&&flag==Statement.RETURN_GENERATED_KEYS
                ||args[1] instanceof int[]||args[1] instanceof String[];
    }
    private static Object unwrap(Object proxy,Class<?> requested)throws SQLException {
        if(requested!=null&&requested.isInstance(proxy))return proxy;
        throw new SQLException("禁止unwrap原vendor对象或内部控制能力","S1009");
    }
    private static Object object(Object proxy,Method method,Object[] args,String label){
        return switch(method.getName()){
            case "equals"->proxy==args[0];case "hashCode"->System.identityHashCode(proxy);case "toString"->label;
            default->throw new IllegalStateException(method.getName());
        };
    }
    private static Object call(Object target,Method method,Object[] args)throws Throwable {
        try{return method.invoke(target,args);}
        catch(InvocationTargetException original){throw original.getCause();}
    }

    private static final class CopyBudget {
        final long until;final int network,query;
        final KeyLimits limits;
        long bytes;int rows;
        CopyBudget(int query,int network,KeyLimits limits,long sessionDeadline){
            this.network=network;this.query=query;this.limits=limits;
            long millis=limits.millis();
            if(query>0)millis=Math.min(millis,query*1000L);
            if(network>0)millis=Math.min(millis,network);
            long own=System.nanoTime()+java.util.concurrent.TimeUnit.MILLISECONDS.toNanos(millis);
            until=sessionDeadline==0?own:Math.min(own,sessionDeadline);
        }
        void check(long extra)throws SQLException{
            if(Thread.currentThread().isInterrupted()||until!=Long.MAX_VALUE&&System.nanoTime()>=until)
                throw new SQLTimeoutException("generated keys复制用尽原期限","HYT00");
            if(extra<0||bytes>limits.bytes()-extra)throw new SQLDataException("generated keys完整缓存超过配置字节上限，原事务必须回滚","22003");
            bytes+=extra;
        }
        void row()throws SQLException {if(++rows>limits.rows())throw new SQLDataException("generated keys超过配置行上限，原事务必须回滚","HY001");}
        void restore(Statement statement,Connection connection)throws SQLException {
            check(0);
            statement.setQueryTimeout(query);
            if(statement.getQueryTimeout()!=query)throw new SQLException("原queryTimeout未精确恢复");
            connection.setNetworkTimeout(Runnable::run,network);
            if(connection.getNetworkTimeout()!=network)throw new SQLException("原networkTimeout未精确恢复");
        }
    }

    /** 仅generated-keys使用：完整脱离physical，标准只读ResultSet全getter由本地rowset实现。 */
    private static final class Snapshot implements InvocationHandler {
        private static final List<String> OBSERVED=List.of("getBoolean","getByte","getShort","getInt","getLong",
                "getFloat","getDouble","getBigDecimal","getString","getNString","getBytes","getDate","getTime","getTimestamp","getURL");
        private record Failed(String message,String state,int code,boolean unsupported){
            SQLException error(){return unsupported?new SQLFeatureNotSupportedException(message,state,code):new SQLDataException(message,state,code);}
        }
        private final CachedRowSet data;
        private final List<Object[]> originals;
        private final List<Map<String,Object>[]> observations;
        private final ResultSetMetaData metadata;
        private final StatementOwner statement;
        private final int type,holdability;
        private ResultSet view;private boolean closed,lastNull;
        private Snapshot(CachedRowSet data,List<Object[]> originals,List<Map<String,Object>[]> observations,
                         ResultSetMetaData metadata,StatementOwner statement,int type,int holdability){
            this.data=data;this.originals=List.copyOf(originals);this.observations=List.copyOf(observations);
            this.metadata=metadata;this.statement=statement;this.type=type;this.holdability=holdability;
        }
        static ResultSet capture(ResultSet raw,CopyBudget budget,StatementOwner owner)throws SQLException {
            ResultSetMetaData original=raw.getMetaData();int columns=original.getColumnCount();
            if(columns<1||columns>budget.limits.columns())throw new SQLDataException("generated keys列数超过配置完整缓存契约","HY001");
            RowSetMetaDataImpl fields=new RowSetMetaDataImpl();fields.setColumnCount(columns);
            Map<String,Object[]> metadata=new HashMap<>();
            for(Method getter:ResultSetMetaData.class.getMethods()){
                if(getter.getParameterCount()!=1||getter.getParameterTypes()[0]!=int.class)continue;
                Object[] values=new Object[columns];
                for(int c=1;c<=columns;c++){
                    budget.check(64);
                    try{values[c-1]=getter.invoke(original,c);}
                    catch(ReflectiveOperationException failure){Throwable cause=failure instanceof InvocationTargetException target?target.getCause():failure;if(cause instanceof SQLException sql)throw sql;throw new SQLException("复制metadata失败",cause);}
                }
                metadata.put(getter.getName(),values);
            }
            for(int c=1;c<=columns;c++){
                fields.setColumnType(c,original.getColumnType(c));fields.setColumnTypeName(c,original.getColumnTypeName(c));
                fields.setColumnName(c,original.getColumnName(c));fields.setColumnLabel(c,original.getColumnLabel(c));
                fields.setNullable(c,original.isNullable(c));fields.setPrecision(c,original.getPrecision(c));fields.setScale(c,original.getScale(c));
                fields.setAutoIncrement(c,original.isAutoIncrement(c));fields.setCaseSensitive(c,original.isCaseSensitive(c));
                fields.setCurrency(c,original.isCurrency(c));fields.setSigned(c,original.isSigned(c));fields.setSearchable(c,original.isSearchable(c));
                fields.setColumnDisplaySize(c,original.getColumnDisplaySize(c));fields.setSchemaName(c,original.getSchemaName(c));
                fields.setTableName(c,original.getTableName(c));fields.setCatalogName(c,original.getCatalogName(c));
            }
            CachedRowSet data=RowSetProvider.newFactory().createCachedRowSet();data.setMetaData(fields);
            List<Object[]> originals=new ArrayList<>();List<Map<String,Object>[]> observed=new ArrayList<>();
            try{
                while(true){
                    budget.check(32);if(!raw.next())break;budget.row();
                    Object[] values=new Object[columns];
                    @SuppressWarnings("unchecked") Map<String,Object>[] reads=(Map<String,Object>[])new Map<?,?>[columns];
                    data.moveToInsertRow();
                    for(int c=1;c<=columns;c++){
                        budget.check(128);values[c-1]=detach(raw.getObject(c),budget);data.updateObject(c,values[c-1]);
                        Map<String,Object> conversions=new HashMap<>();
                        for(String name:OBSERVED){
                            budget.check(64);
                            try{Object value=ResultSet.class.getMethod(name,int.class).invoke(raw,c);conversions.put(name,detach(value,budget));}
                            catch(InvocationTargetException target){
                                if(!(target.getCause() instanceof SQLException error))throw new SQLException("生成键getter异常",target.getCause());
                                if(uncertain(error)||error instanceof SQLTimeoutException)throw error;
                                conversions.put(name,new Failed(error.getMessage(),error.getSQLState(),error.getErrorCode(),error instanceof SQLFeatureNotSupportedException));
                            }catch(ReflectiveOperationException invalid){throw new SQLException("缺少标准getter",invalid);}
                        }
                        reads[c-1]=Collections.unmodifiableMap(conversions);
                    }
                    data.insertRow();data.moveToCurrentRow();data.last();
                    originals.add(values);observed.add(reads);
                }
                data.beforeFirst();data.setReadOnly(true);
                ResultSetMetaData detachedMetadata=(ResultSetMetaData)Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),new Class<?>[]{ResultSetMetaData.class},(proxy,method,args)->{
                    String name=method.getName();
                    if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"GeneratedKeysMetadata");
                    if(name.equals("unwrap"))return unwrap(proxy,(Class<?>)args[0]);
                    if(name.equals("isWrapperFor"))return ((Class<?>)args[0]).isInstance(proxy);
                    if(name.equals("getColumnCount"))return columns;
                    int index=(Integer)args[0]-1;if(index<0||index>=columns)throw new SQLException("列下标越界","S1002");
                    Object[] values=metadata.get(name);if(values==null)throw new SQLFeatureNotSupportedException(name,"0A000");return values[index];
                });
                Snapshot snapshot=new Snapshot(data,originals,observed,detachedMetadata,owner,raw.getType(),raw.getHoldability());
                snapshot.view=(ResultSet)Proxy.newProxyInstance(AuthorityConnection.class.getClassLoader(),new Class<?>[]{ResultSet.class},snapshot);
                return snapshot.view;
            }catch(SQLException|RuntimeException|Error failure){try{data.close();}catch(SQLException close){failure.addSuppressed(close);}throw failure;}
        }
        public Object invoke(Object proxy,Method method,Object[] args)throws Throwable {
            String name=method.getName();
            if(method.getDeclaringClass()==Object.class)return object(proxy,method,args,"GeneratedKeysReadOnly");
            if(name.equals("close")){if(!closed){data.close();closed=true;}return null;}
            if(name.equals("isClosed"))return closed;
            if(name.equals("getStatement"))return statement.view;
            if(name.equals("unwrap"))return unwrap(view,(Class<?>)args[0]);
            if(name.equals("isWrapperFor"))return ((Class<?>)args[0]).isInstance(view);
            if(closed)throw new SQLException("缓存keys已关闭","HY010");
            if(name.equals("getMetaData"))return metadata;
            if(name.equals("getType"))return type;
            if(name.equals("getConcurrency"))return ResultSet.CONCUR_READ_ONLY;
            if(name.equals("getHoldability"))return holdability;
            if(name.equals("wasNull"))return lastNull;
            if(name.startsWith("update")||Set.of("insertRow","deleteRow","moveToInsertRow","moveToCurrentRow","refreshRow","cancelRowUpdates").contains(name))
                throw new SQLFeatureNotSupportedException("generated keys缓存只读","0A000");
            if(type==ResultSet.TYPE_FORWARD_ONLY&&Set.of("previous","absolute","relative","first","last","beforeFirst","afterLast").contains(name))
                throw new SQLFeatureNotSupportedException("原keys是forward-only","0A000");
            if(name.equals("getObject")||OBSERVED.contains(name)&&args!=null&&args.length==1){
                int column=args[0] instanceof String label?data.findColumn(label):(Integer)args[0];
                int row=data.getRow()-1;if(row<0||row>=originals.size())throw new SQLException("keys游标未定位","24000");
                if(column<1||column>metadata.getColumnCount())throw new SQLException("keys列越界","S1002");
                Object original=originals.get(row)[column-1];lastNull=original==null;
                if(name.equals("getObject")){
                    Object value=copy(original);
                    if(args.length==2&&args[1] instanceof Class<?> expected){
                        if(value==null)return null;
                        if(expected.isInstance(value))return value;
                        String getter=expected==Long.class||expected==long.class?"getLong":expected==Integer.class||expected==int.class?"getInt":
                                expected==Short.class||expected==short.class?"getShort":expected==Byte.class||expected==byte.class?"getByte":
                                expected==Boolean.class||expected==boolean.class?"getBoolean":expected==Double.class||expected==double.class?"getDouble":
                                expected==Float.class||expected==float.class?"getFloat":expected==String.class?"getString":
                                expected==BigDecimal.class?"getBigDecimal":expected==java.sql.Date.class?"getDate":
                                expected==Time.class?"getTime":expected==Timestamp.class?"getTimestamp":
                                expected==URL.class?"getURL":expected==byte[].class?"getBytes":null;
                        if(getter==null)throw new SQLFeatureNotSupportedException("typed getObject转换不受支持","0A000");
                        Object converted=observations.get(row)[column-1].get(getter);if(converted instanceof Failed failed)throw failed.error();return copy(converted);
                    }
                    return value;
                }
                Object converted=observations.get(row)[column-1].get(name);
                if(converted instanceof Failed failed)throw failed.error();return copy(converted);
            }
            Object result=call(data,method,args);
            if(name.startsWith("get"))lastNull=data.wasNull();
            return result;
        }
        private static Object detach(Object value,CopyBudget budget)throws SQLException {
            if(value instanceof Blob blob){long n=blob.length();if(n>Integer.MAX_VALUE)throw new SQLDataException("LOB无法用单JDBC数组完整缓存","22003");budget.check(n);return new javax.sql.rowset.serial.SerialBlob(blob.getBytes(1,(int)n));}
            if(value instanceof Clob clob){long n=clob.length();if(n>Integer.MAX_VALUE)throw new SQLDataException("CLOB无法完整缓存","22003");budget.check(n*2);return new javax.sql.rowset.serial.SerialClob(clob.getSubString(1,(int)n).toCharArray());}
            if(value instanceof byte[] b)budget.check(b.length);
            else if(value instanceof String s)budget.check(s.length()*2L);
            else if(value!=null)budget.check(64);
            return copy(value);
        }
        private static Object copy(Object value)throws SQLException{
            if(value==null||value instanceof String||value instanceof Boolean||value instanceof Byte||value instanceof Short
                    ||value instanceof Integer||value instanceof Long||value instanceof Float||value instanceof Double
                    ||value instanceof BigInteger||value instanceof BigDecimal||value instanceof URL)return value;
            if(value instanceof byte[] bytes)return bytes.clone();
            if(value instanceof Timestamp at){Timestamp copy=new Timestamp(at.getTime());copy.setNanos(at.getNanos());return copy;}
            if(value instanceof Time at)return new Time(at.getTime());
            if(value instanceof java.sql.Date at)return new java.sql.Date(at.getTime());
            if(value instanceof Calendar calendar)return calendar.clone();
            if(value instanceof javax.sql.rowset.serial.SerialBlob blob)return new javax.sql.rowset.serial.SerialBlob(blob);
            if(value instanceof javax.sql.rowset.serial.SerialClob clob)return new javax.sql.rowset.serial.SerialClob(clob);
            throw new SQLFeatureNotSupportedException("未定义生成键对象的无连接复制","0A000");
        }
    }
}

