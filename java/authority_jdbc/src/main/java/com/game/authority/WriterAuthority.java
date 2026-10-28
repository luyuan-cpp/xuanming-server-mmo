package com.game.authority;

import com.fasterxml.jackson.databind.*;
import java.io.*;
import java.nio.*;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.security.*;
import java.sql.*;
import java.util.*;

/** 已消费的NORMAL启动资格。只读registry与meta；无注册、补片、退休、维护或DDL入口。 */
public final class WriterAuthority {
    private static final ObjectMapper JSON=new ObjectMapper().enable(DeserializationFeature.FAIL_ON_TRAILING_TOKENS).enable(com.fasterxml.jackson.core.JsonParser.Feature.STRICT_DUPLICATE_DETECTION);
    private static final Set<String> FIELDS=Set.of("format","writerKey","instanceId","generation","authoritySchema",
            "protectedSchemas","sqlPrincipal","registrationDigest","deploymentId","grantId","grantedMs",
            "permission","physicalAuthoritySha256","maintenanceRunId","lockPrimitive","lockQualificationSha256");
    private final java.util.concurrent.atomic.AtomicBoolean sealed=new java.util.concurrent.atomic.AtomicBoolean();
    /** 仅P3真实join后或进程销毁时调用；终态拒未来SQL，不充当排空证明，也不修改SQL registry。 */
    public void seal(){sealed.set(true);}
    public boolean isSealed(){return sealed.get();}
    private void requireOpen()throws SQLException{if(sealed.get())throw new SQLNonTransientConnectionException("本boot writer已永久seal，不能复用handle或重开池","08003");}
    private final byte[] writer, instance, principal, registration, grant, physical, maintenanceRun;
    private final String generation, schema, deployment;
    private final boolean lockTouch;
    private final byte[] qualification;
    private final List<String> protectedSchemas;
    private final java.util.concurrent.atomic.AtomicInteger nextSlot=new java.util.concurrent.atomic.AtomicInteger(
            java.util.concurrent.ThreadLocalRandom.current().nextInt(64));
    private WriterAuthority(JsonNode root)throws IOException {
        if(!root.isObject()||root.size()<FIELDS.size()-1||root.size()>FIELDS.size())throw new IOException("启动grant字段集合不符");
        Iterator<String> names=root.fieldNames();while(names.hasNext())if(!FIELDS.contains(names.next()))throw new IOException("未知grant字段");
        if(!"SQL_WRITER_BOOT_V1".equals(text(root,"format"))||!"NORMAL".equals(text(root,"permission")))throw new IOException("启动资格不是NORMAL");
        writer=bytes(root,"writerKey",16);instance=bytes(root,"instanceId",16);grant=bytes(root,"grantId",16);
        registration=bytes(root,"registrationDigest",32);physical=bytes(root,"physicalAuthoritySha256",32);
        for(byte[] identity:List.of(writer,instance,grant,registration,physical))if(Arrays.equals(identity,new byte[identity.length]))throw new IOException("启动身份或摘要不能为零");
        maintenanceRun=root.has("maintenanceRunId")?bytes(root,"maintenanceRunId",-1):new byte[0];
        if(maintenanceRun.length!=0&&maintenanceRun.length!=16)throw new IOException("maintenance run原件长度不符");
        String primitive=text(root,"lockPrimitive");
        if(!primitive.equals("SELECT_ONLY")&&!primitive.equals("LOCK_TOUCH"))throw new IOException("锁原语未取得真实资格");
        qualification=bytes(root,"lockQualificationSha256",32);
        if(Arrays.equals(qualification,new byte[32]))throw new IOException("缺少真实I3资格摘要");
        lockTouch=primitive.equals("LOCK_TOUCH");
        principal=bytes(root,"sqlPrincipal",-1);
        if(principal.length==0||principal.length>288)
            throw new IOException("SQL主体原件非法");
        try{StandardCharsets.UTF_8.newDecoder().onMalformedInput(java.nio.charset.CodingErrorAction.REPORT)
                .onUnmappableCharacter(java.nio.charset.CodingErrorAction.REPORT).decode(ByteBuffer.wrap(principal));}
        catch(java.nio.charset.CharacterCodingException invalid){throw new IOException("SQL主体必须为UTF8",invalid);}
        generation=text(root,"generation");
        try{if(!generation.matches("[0-9a-f]{1,32}")||new java.math.BigInteger(generation,16).signum()==0)throw new IllegalArgumentException();}
        catch(IllegalArgumentException invalid){throw new IOException("authority generation不是规范非零hex",invalid);}
        schema=text(root,"authoritySchema");if(!schema.matches("[A-Za-z0-9_]{1,64}"))throw new IOException("authority schema非法");
        deployment=text(root,"deploymentId");if(deployment.isBlank()||deployment.length()>256||deployment.codePoints().anyMatch(Character::isISOControl))throw new IOException("deployment绑定缺失");
        JsonNode list=root.get("protectedSchemas");if(!list.isArray()||list.isEmpty()||list.size()>1024)throw new IOException("缺少完整protected schema集合");
        List<String> schemas=new ArrayList<>();String previous="";
        for(JsonNode value:list){String next=value.asText();if(!value.isTextual()||!next.matches("[A-Za-z0-9_]{1,64}")||previous.compareTo(next)>=0)throw new IOException("protected schema必须完整有序且无重复");schemas.add(next);previous=next;}
        if(!schemas.contains(schema))throw new IOException("authority schema必须在保护集合");
        protectedSchemas=List.copyOf(schemas);
        try{var stamp=new java.math.BigInteger(text(root,"grantedMs"));if(stamp.signum()<=0||stamp.bitLength()>64)throw new IOException("grant时刻非法");}
        catch(NumberFormatException bad){throw new IOException("grant时刻非法",bad);}
    }
    public static WriterAuthority consume(Map<String,String> env)throws IOException {
        String file=required(env,"MMORPG_SQL_WRITER_GRANT_FILE"), expected=required(env,"MMORPG_SQL_WRITER_GRANT_SHA256");
        if(!expected.matches("[0-9a-fA-F]{64}"))throw new IOException("缺少启动grant SHA256");
        Path path=Path.of(file).toAbsolutePath().normalize();
        byte[] encoded=readPrivateControlFile(path,1024*1024,env);
        if(!MessageDigest.isEqual(hex(expected),sha(encoded)))throw new IOException("grant原bytes摘要不匹配");
        WriterAuthority identity=new WriterAuthority(JSON.readTree(encoded));
        if(!identity.deployment.equals(required(env,"MMORPG_SQL_DEPLOYMENT_ID"))
                ||!MessageDigest.isEqual(identity.physical,hex(required(env,"MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256"))))
            throw new IOException("deployment或physical authority原件不符");
        Path directory=Path.of(required(env,"MMORPG_SQL_WRITER_CONSUME_DIR")).toAbsolutePath().normalize();
        Path marker=directory.resolve("grant-"+HexFormat.of().formatHex(identity.grant)+".consumed");
        byte[] receipt=(expected.toLowerCase(Locale.ROOT)+"\n"+identity.deployment+"\n").getBytes(StandardCharsets.UTF_8);
        createPrivateControlFile(marker,receipt,env);return identity;
    }
    private static boolean windows(){return File.separatorChar=='\\';}
    /** Windows原件必须由同一受信helper句柄完成校验和I/O；不能验证后重新打开路径。 */
    public static byte[] readPrivateControlFile(Path path,int maximum)throws IOException{return readPrivateControlFile(path,maximum,System.getenv());}
    private static byte[] readPrivateControlFile(Path path,int maximum,Map<String,String> environment)throws IOException{
        if(windows())return privateFileHelper("read-file",path,maximum,new byte[0],environment);
        if(maximum<1||maximum>16*1024*1024||!path.isAbsolute()||!Files.isRegularFile(path,LinkOption.NOFOLLOW_LINKS)||Files.isSymbolicLink(path))throw new IOException("私有原件路径或界限非法");
        requirePrivateDirectory(path.getParent());try(var stream=Files.newInputStream(path,LinkOption.NOFOLLOW_LINKS)){byte[] bytes=stream.readNBytes(maximum+1);if(bytes.length>maximum)throw new IOException("私有原件超过界限");return bytes;}
    }
    public static void createPrivateControlFile(Path path,byte[] bytes)throws IOException{createPrivateControlFile(path,bytes,System.getenv());}
    private static void createPrivateControlFile(Path path,byte[] bytes,Map<String,String> environment)throws IOException{
        if(bytes.length<1||bytes.length>16*1024*1024||!path.isAbsolute())throw new IOException("私有原件大小或路径非法");
        if(windows()){privateFileHelper("create-file",path,bytes.length,bytes,environment);return;}
        requirePrivateDirectory(path.getParent());Object key=Files.readAttributes(path.getParent(),java.nio.file.attribute.BasicFileAttributes.class,LinkOption.NOFOLLOW_LINKS).fileKey();
        try(var record=FileChannel.open(path,Set.of(StandardOpenOption.CREATE_NEW,StandardOpenOption.WRITE,StandardOpenOption.SYNC),java.nio.file.attribute.PosixFilePermissions.asFileAttribute(java.nio.file.attribute.PosixFilePermissions.fromString("rw-------")))){var buffer=ByteBuffer.wrap(bytes);while(buffer.hasRemaining())record.write(buffer);record.force(true);}
        requirePrivateDirectory(path.getParent());if(!Objects.equals(key,Files.readAttributes(path.getParent(),java.nio.file.attribute.BasicFileAttributes.class,LinkOption.NOFOLLOW_LINKS).fileKey()))throw new IOException("私有目录身份变化，创建结局未明");
        try(var directory=FileChannel.open(path.getParent(),StandardOpenOption.READ)){directory.force(true);}
    }
    public static void requirePrivateDirectory(Path directory)throws IOException{
        if(windows()){privateFileHelper("validate-directory",directory,1,new byte[0],System.getenv());return;}
        Path absolute=directory.toAbsolutePath().normalize();var attributes=Files.readAttributes(absolute,java.nio.file.attribute.BasicFileAttributes.class,LinkOption.NOFOLLOW_LINKS);
        if(!attributes.isDirectory()||attributes.isOther()||Files.isSymbolicLink(absolute))throw new IOException("私有目录不存在或含重解析入口");
        var view=Files.getFileAttributeView(absolute,java.nio.file.attribute.PosixFileAttributeView.class,LinkOption.NOFOLLOW_LINKS);if(view==null)throw new IOException("未取得私有目录平台资格");
        var value=view.readAttributes();var permissions=value.permissions();var current=absolute.getFileSystem().getUserPrincipalLookupService().lookupPrincipalByName(System.getProperty("user.name"));
        if(!value.owner().equals(current)||permissions.contains(java.nio.file.attribute.PosixFilePermission.GROUP_WRITE)||permissions.contains(java.nio.file.attribute.PosixFilePermission.OTHERS_WRITE))throw new IOException("私有目录所有者或写权限不符");
    }
    /** 文件写入与祖先替换是不同权限；目录新建子项不能覆盖已存在的私有路径。 */
    static void requireHelperPathAcl(boolean executableFile,java.nio.file.attribute.UserPrincipal owner,
            List<java.nio.file.attribute.AclEntry> entries,Set<java.nio.file.attribute.UserPrincipal> trusted)throws IOException{
        if(!trusted.contains(owner))throw new IOException("helper owner未取得资格");
        Set<java.nio.file.attribute.AclEntryPermission> mutation=EnumSet.of(java.nio.file.attribute.AclEntryPermission.WRITE_NAMED_ATTRS,java.nio.file.attribute.AclEntryPermission.DELETE_CHILD,java.nio.file.attribute.AclEntryPermission.WRITE_ATTRIBUTES,java.nio.file.attribute.AclEntryPermission.DELETE,java.nio.file.attribute.AclEntryPermission.WRITE_ACL,java.nio.file.attribute.AclEntryPermission.WRITE_OWNER);
        // 在文件上两位表示写入/追加；在祖先目录上只表示创建兄弟文件/目录，替换仍受上列权限约束。
        if(executableFile){mutation.add(java.nio.file.attribute.AclEntryPermission.WRITE_DATA);mutation.add(java.nio.file.attribute.AclEntryPermission.APPEND_DATA);}
        for(var entry:entries)if(entry.type()==java.nio.file.attribute.AclEntryType.ALLOW&&!entry.flags().contains(java.nio.file.attribute.AclEntryFlag.INHERIT_ONLY)&&!trusted.contains(entry.principal())&&!Collections.disjoint(entry.permissions(),mutation))throw new IOException("helper可被非受信主体替换");
    }

    private static void qualifiedHelper(Path executable,byte[] expected)throws IOException{
        if(!executable.isAbsolute()||!Files.isRegularFile(executable,LinkOption.NOFOLLOW_LINKS))throw new IOException("私有文件helper必须为固定绝对原件");
        for(Path part=executable;part!=null;part=part.getParent()){var a=Files.readAttributes(part,java.nio.file.attribute.BasicFileAttributes.class,LinkOption.NOFOLLOW_LINKS);if(a.isSymbolicLink()||a.isOther())throw new IOException("helper路径含重解析入口");}
        var lookup=executable.getFileSystem().getUserPrincipalLookupService();var current=lookup.lookupPrincipalByName(System.getProperty("user.name"));Set<java.nio.file.attribute.UserPrincipal> trusted=new HashSet<>();trusted.add(current);trusted.add(lookup.lookupPrincipalByName("NT AUTHORITY\\SYSTEM"));trusted.add(lookup.lookupPrincipalByName("BUILTIN\\Administrators"));
        // Windows卷根可由系统安装服务持有；仍按固定OS主体解析，不信任环境中的账号名。
        trusted.add(lookup.lookupPrincipalByName("NT SERVICE\\TrustedInstaller"));
        for(Path protectedPath=executable;protectedPath!=null;protectedPath=protectedPath.getParent()){
            var acl=Files.getFileAttributeView(protectedPath,java.nio.file.attribute.AclFileAttributeView.class,LinkOption.NOFOLLOW_LINKS);
            if(acl==null)throw new IOException("helper owner未取得资格");
            requireHelperPathAcl(protectedPath.equals(executable),acl.getOwner(),acl.getAcl(),trusted);
        }
        try(var in=Files.newInputStream(executable,LinkOption.NOFOLLOW_LINKS)){MessageDigest hash;try{hash=MessageDigest.getInstance("SHA-256");}catch(NoSuchAlgorithmException impossible){throw new IOException(impossible);}long total=0;byte[] block=new byte[65536];int n;while((n=in.read(block))!=-1){total+=n;if(total>64*1024*1024)throw new IOException("helper超过资格界限");hash.update(block,0,n);}if(!MessageDigest.isEqual(expected,hash.digest()))throw new IOException("helper SHA256不符");}
    }
    private static byte[] privateFileHelper(String command,Path path,int maximum,byte[] input,Map<String,String> environment)throws IOException{
        if(maximum<1||maximum>16*1024*1024||!path.isAbsolute())throw new IOException("私有文件helper参数超界");
        Path executable=Path.of(required(environment,"MMORPG_SQL_PRIVATE_FILE_HELPER"));String expected=required(environment,"MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256");if(!expected.matches("[0-9a-fA-F]{64}"))throw new IOException("缺少helper固定SHA256");qualifiedHelper(executable,hex(expected));
        var builder=new ProcessBuilder(executable.toString(),"--command",command,"--path",path.normalize().toString(),"--max-bytes",Integer.toString(maximum));builder.redirectError(ProcessBuilder.Redirect.DISCARD);builder.environment().clear();
        Process process=builder.start();long until=System.nanoTime()+10_000_000_000L;
        var workers=java.util.concurrent.Executors.newFixedThreadPool(2,r->{Thread thread=new Thread(r,"private-control-file-io");thread.setDaemon(true);return thread;});
        var output=workers.submit(()->{try(var stream=process.getInputStream()){int bound=command.equals("read-file")?maximum:0;byte[] bytes=stream.readNBytes(bound+1);if(bytes.length>bound)throw new IOException("helper输出超过协议界限");return bytes;}});
        var writer=workers.submit(()->{try(var stream=process.getOutputStream()){stream.write(input);stream.flush();}return null;});
        try{if(!process.waitFor(Math.max(1,until-System.nanoTime()),java.util.concurrent.TimeUnit.NANOSECONDS))throw new IOException("私有文件helper超时，创建结局未明");writer.get(Math.max(1,until-System.nanoTime()),java.util.concurrent.TimeUnit.NANOSECONDS);byte[] bytes=output.get(Math.max(1,until-System.nanoTime()),java.util.concurrent.TimeUnit.NANOSECONDS);if(process.exitValue()!=0)throw new IOException("私有文件helper拒绝或结局未明");return bytes;}
        catch(InterruptedException interrupted){Thread.currentThread().interrupt();throw new IOException("私有文件helper等待被中断，创建结局未明");}
        catch(java.util.concurrent.ExecutionException|java.util.concurrent.TimeoutException failure){throw new IOException("私有文件helper I/O未确认");}
        finally{process.destroyForcibly();output.cancel(true);writer.cancel(true);workers.shutdownNow();}
    }
    public static WriterAuthority consumeEnvironment()throws IOException{return consume(System.getenv());}
    private static String required(Map<String,String> env,String name)throws IOException {
        String value=env.get(name);if(value==null||value.isBlank())throw new IOException("缺少NORMAL启动原件："+name);return value;
    }
    private static String text(JsonNode root,String field)throws IOException {
        JsonNode value=root.get(field);if(value==null||!value.isTextual())throw new IOException("grant字段形状不符："+field);return value.asText();
    }
    private static byte[] bytes(JsonNode root,String name,int size)throws IOException {
        try{String encoded=text(root,name);byte[] value=Base64.getDecoder().decode(encoded);if(!Base64.getEncoder().encodeToString(value).equals(encoded))throw new IOException("grant bytes必须标准base64："+name);if(size>=0&&value.length!=size)throw new IOException("grant bytes长度不符："+name);return value;}
        catch(IllegalArgumentException invalid){throw new IOException("grant bytes编码不符："+name,invalid);}
    }
    private static byte[] hex(String value)throws IOException {
        try{return HexFormat.of().parseHex(value);}catch(IllegalArgumentException invalid){throw new IOException("摘要编码不符",invalid);}
    }
    private static byte[] sha(byte[] value)throws IOException {
        try{return MessageDigest.getInstance("SHA-256").digest(value);}catch(NoSuchAlgorithmException unavailable){throw new IOException(unavailable);}
    }
    private String table(String name){return (char)96+schema+(char)96+"."+name;}
    /** 在真实physical connection交池之前调用，不能把检查放在已有RR事务中。 */
    public void verifyPhysical(Connection raw)throws SQLException {
        requireOpen();
        if(!raw.getAutoCommit())throw new SQLException("physical初始连接不能已有事务");
        String catalog=raw.getCatalog();
        if(catalog==null||!protectedSchemas.contains(catalog))catalog=raw.getSchema();
        if(catalog==null||!protectedSchemas.contains(catalog))throw new SQLException("连接schema不在固定保护集合");
        boolean h2="H2".equals(raw.getMetaData().getDatabaseProductName());
        try(var s=raw.createStatement();var r=s.executeQuery(h2?"SELECT CURRENT_USER":"SELECT CURRENT_USER(),@@SESSION.sql_mode")){
            if(!r.next()||!MessageDigest.isEqual(principal,r.getString(1).getBytes(StandardCharsets.UTF_8)))throw new SQLException("实际SQL认证主体不匹配启动原件");
            String mode=h2?"":r.getString(2).toUpperCase(Locale.ROOT);
            if(mode.contains("NO_BACKSLASH_ESCAPES")||mode.contains("ANSI_QUOTES"))throw new SQLException("SQL词法会话模式未受支持");
        }
        Epoch epoch=current(raw);
        if(maintenanceRun.length!=0&&epoch.run().length!=0&&!Arrays.equals(maintenanceRun,epoch.run()))
            throw new SQLException("prepare-only boot不属于当前release run");
        registry(raw);verifyRegistration(raw);verifyShape(raw);verifySlots(raw,epoch);
    }
    /** 只加载本仓protoc产出的descriptor；不解释.proto文本，不手造平行消息。 */
    private static com.google.protobuf.Descriptors.Descriptor requestDescriptor()throws SQLException {
        try(var input=WriterAuthority.class.getResourceAsStream("/META-INF/sql-authority/route-authority.desc")){
            if(input==null)throw new SQLException("缺少本仓正规authority descriptor资源，拒绝启动");
            var set=com.google.protobuf.DescriptorProtos.FileDescriptorSet.parseFrom(input);
            Map<String,com.google.protobuf.DescriptorProtos.FileDescriptorProto> pending=new HashMap<>();
            for(var file:set.getFileList())pending.put(file.getName(),file);
            Map<String,com.google.protobuf.Descriptors.FileDescriptor> files=new HashMap<>();
            while(!pending.isEmpty()){
                boolean progressed=false;
                for(var entry:new ArrayList<>(pending.entrySet())){
                    var file=entry.getValue();if(!files.keySet().containsAll(file.getDependencyList()))continue;
                    var dependencies=file.getDependencyList().stream().map(files::get).toArray(com.google.protobuf.Descriptors.FileDescriptor[]::new);
                    files.put(entry.getKey(),com.google.protobuf.Descriptors.FileDescriptor.buildFrom(file,dependencies));pending.remove(entry.getKey());progressed=true;
                }
                if(!progressed)throw new SQLException("authority descriptor导入不完整或形成循环");
            }
            for(var file:files.values()){
                var receipt=file.findMessageTypeByName("sql_writer_lifecycle_receipt");
                if(receipt!=null){var request=receipt.findNestedTypeByName("Request");if(request!=null)return request;}
            }
            throw new SQLException("descriptor缺少正式SqlWriterLifecycleReceipt.Request");
        }catch(java.io.IOException|com.google.protobuf.Descriptors.DescriptorValidationException invalid){throw new SQLException("正规authority descriptor不可读",invalid);}
    }
    private static Object field(com.google.protobuf.Message message,String name)throws SQLException{
        var descriptor=message.getDescriptorForType().findFieldByName(name);if(descriptor==null)throw new SQLException("正式descriptor缺字段："+name);return message.getField(descriptor);
    }
    private static byte[] binary(com.google.protobuf.Message message,String name)throws SQLException{return ((com.google.protobuf.ByteString)field(message,name)).toByteArray();}
    private static byte[] canonical(com.google.protobuf.Message message)throws IOException{
        byte[] bytes=new byte[message.getSerializedSize()];var output=com.google.protobuf.CodedOutputStream.newInstance(bytes);output.useDeterministicSerialization();message.writeTo(output);output.checkNoSpaceLeft();return bytes;
    }
    private static void rejectUnknown(com.google.protobuf.Message message)throws SQLException{
        if(!message.getUnknownFields().asMap().isEmpty())throw new SQLException("REGISTER含未知控制字段");
        for(var entry:message.getAllFields().entrySet())if(entry.getKey().getJavaType()==com.google.protobuf.Descriptors.FieldDescriptor.JavaType.MESSAGE){
            if(entry.getKey().isRepeated())for(Object item:(List<?>)entry.getValue())rejectUnknown((com.google.protobuf.Message)item);else rejectUnknown((com.google.protobuf.Message)entry.getValue());
        }
    }
    private void verifyRegistration(Connection raw)throws SQLException {
        try(var s=raw.prepareStatement("SELECT request_proto,request_id FROM "+table("writer_lifecycle_receipt")+" WHERE kind=1 AND request_digest=?")){
            s.setBytes(1,registration);s.setQueryTimeout(3);
            try(var r=s.executeQuery()){
                if(!r.next())throw new SQLException("缺少REGISTER永久原文，不能仅凭grant选择锁原语");
                byte[] original=r.getBytes(1),requestId=r.getBytes(2);if(original==null||original.length>1024*1024||r.next())throw new SQLException("REGISTER原文缺失、过大或不唯一");
                var request=com.google.protobuf.DynamicMessage.parseFrom(requestDescriptor(),original);rejectUnknown(request);
                if(((com.google.protobuf.Descriptors.EnumValueDescriptor)field(request,"kind")).getNumber()!=1
                        ||!MessageDigest.isEqual(binary(request,"request_id"),requestId))throw new SQLException("REGISTER请求kind或永久request_id不符");
                var registered=(com.google.protobuf.Message)field(request,"writer");
                if(((com.google.protobuf.Descriptors.EnumValueDescriptor)field(registered,"state")).getNumber()!=1)throw new SQLException("REGISTER原writer状态不是ACTIVE");
                var primitive=(com.google.protobuf.Descriptors.EnumValueDescriptor)field(request,"lock_primitive");
                if(primitive.getNumber()!=(lockTouch?2:1)||!MessageDigest.isEqual(qualification,binary(request,"lock_qualification_sha256"))
                        ||!MessageDigest.isEqual(registration,binary(registered,"registration_digest"))
                        ||!MessageDigest.isEqual(physical,binary(request,"deployment_digest"))||!deployment.equals(field(request,"deployment_id"))||!schema.equals(field(request,"authority_schema"))
                        ||!protectedSchemas.equals(field(request,"protected_schemas"))
                        ||!generation.equals(field(registered,"generation"))||!MessageDigest.isEqual(writer,binary(registered,"writer_key"))
                        ||!MessageDigest.isEqual(instance,binary(registered,"instance_id"))||!MessageDigest.isEqual(principal,binary(registered,"sql_principal"))
                        ||!MessageDigest.isEqual(maintenanceRun,binary(request,"maintenance_run_id")))throw new SQLException("grant与REGISTER完整原件不符");
                var writerField=request.getDescriptorForType().findFieldByName("writer");
                var digestField=registered.getDescriptorForType().findFieldByName("registration_digest");
                var unsigned=request.toBuilder().setField(writerField,registered.toBuilder().clearField(digestField).build()).build();
                if(!MessageDigest.isEqual(registration,sha(canonical(unsigned))))throw new SQLException("REGISTER确定性原文摘要不符");
            }
        }catch(IOException invalid){throw new SQLException("REGISTER正规消息无法解析或编码",invalid);}
    }
    private void verifyShape(Connection raw)throws SQLException {
        for(String query:List.of("SELECT writer_key,generation,instance_id,sql_principal,registration_digest,state FROM "+table("writer_registry")+" WHERE 1=0",
                "SELECT writer_key,slot,generation,writer_epoch,state FROM "+table("writer_slot")+" WHERE 1=0",
                "SELECT generation,writer_epoch,writable,catalog_revision,freeze_run_id FROM "+table("route_meta")+" WHERE 1=0")){
            try(var s=raw.createStatement();var rows=s.executeQuery(query)){var meta=rows.getMetaData();
                for(int i=1;i<=meta.getColumnCount();i++)if(meta.isNullable(i)!=ResultSetMetaData.columnNoNulls)throw new SQLException("authority正式列不可空");
            }
        }
    }
    private void verifySlots(Connection raw,Epoch epoch)throws SQLException{
        try(var s=raw.prepareStatement("SELECT slot,generation,writer_epoch,state,lock_touch FROM "+table("writer_slot")+" WHERE writer_key=? ORDER BY slot")){
            s.setBytes(1,writer);s.setQueryTimeout(3);try(var r=s.executeQuery()){
                int slot=0;while(r.next()){int touch=r.getInt(5);
                    if(slot>=64||r.getInt(1)!=slot++||!generation.equals(r.getString(2))||u64(r,3)!=epoch.value()||r.getInt(4)!=1||touch<0||touch>1)throw new SQLException("writer原64片缺失、重复、撤销或epoch非法，禁止自动补权");
                }
                if(slot!=64)throw new SQLException("writer必须已有精确64片，禁止normal补片");
            }
        }
    }
    private record Epoch(long value,long catalog,byte[] run) {}
    private Epoch current(Connection raw)throws SQLException {
        try(var s=raw.prepareStatement("SELECT generation,writer_epoch,writable,catalog_revision,freeze_run_id FROM "+table("route_meta")+" WHERE id=1")){
            s.setQueryTimeout(3);try(var r=s.executeQuery()){
                if(!r.next()||!generation.equals(r.getString(1))||u64(r,2)==0||r.getInt(3)!=1||u64(r,4)==0||r.getBytes(5)==null||(r.getBytes(5).length!=0&&r.getBytes(5).length!=16))
                    throw new SQLException("authority已冻结或epoch/meta原件非法");
                Epoch result=new Epoch(u64(r,2),u64(r,4),r.getBytes(5));if(r.next())throw new SQLException("重复meta");return result;
            }
        }
    }
    private void registry(Connection raw)throws SQLException {
        try(var s=raw.prepareStatement("SELECT generation,instance_id,sql_principal,registration_digest,state FROM "+table("writer_registry")+" WHERE writer_key=?")){
            s.setBytes(1,writer);s.setQueryTimeout(3);try(var r=s.executeQuery()){
                if(!r.next()||!generation.equals(r.getString(1))||!Arrays.equals(instance,r.getBytes(2))
                        ||!Arrays.equals(principal,r.getBytes(3))||!Arrays.equals(registration,r.getBytes(4))||r.getInt(5)!=1||r.next())
                    throw new SQLException("writer未ACTIVE或启动原件不符");
            }
        }
    }
    /** normal唯一事务入口：pre-BEGIN强E → own-slot CURRENT ACTIVE锁 → 只读meta/registry。 */
    void begin(Connection raw)throws SQLException {
        requireOpen();
        if(!raw.getAutoCommit())throw new SQLException("禁止在旧事务snapshot内做epoch预读");
        Epoch before=current(raw);
        raw.setAutoCommit(false);
        try {
            int selected=nextSlot.getAndIncrement()&63;
            if(lockTouch)try(var lock=raw.prepareStatement("UPDATE "+table("writer_slot")
                    +" SET lock_touch=1-lock_touch WHERE writer_key=? AND slot=? AND generation=? AND writer_epoch=? AND state=1")){
                lock.setBytes(1,writer);lock.setInt(2,selected);lock.setString(3,generation);
                lock.setObject(4,new java.math.BigInteger(Long.toUnsignedString(before.value())));lock.setQueryTimeout(3);
                if(lock.executeUpdate()!=1)throw new SQLException("CURRENT slot条件锁未命中唯一ACTIVE/E/generation原件","40001");
            }
            try(var s=raw.prepareStatement("SELECT generation,writer_epoch,state FROM "+table("writer_slot")
                    +" WHERE writer_key=? AND slot=?"+(lockTouch?"":" FOR UPDATE"))){
                s.setBytes(1,writer);s.setInt(2,selected);s.setQueryTimeout(3);
                try(var r=s.executeQuery()){
                    if(!r.next()||!generation.equals(r.getString(1))||u64(r,2)!=before.value()||r.getInt(3)!=1||r.next())
                        throw new SQLException("own slot CURRENT状态或epoch已变化，不能复活旧RR资格","40001");
                }
            }
            Epoch after=current(raw);registry(raw);
            if(after.value()!=before.value()||after.catalog()!=before.catalog())throw new SQLException("epoch/catalog在事务进入期间变化","40001");
        } catch(SQLException|RuntimeException|Error failure){
            try{raw.rollback();raw.setAutoCommit(true);}catch(SQLException cleanup){
                failure.addSuppressed(cleanup);throw new SQLNonTransientConnectionException("authority进入阶段的回滚或复位未知","08007",failure);
            }
            throw failure;
        }
    }
    private static long u64(ResultSet row,int column)throws SQLException {
        Object value=row.getObject(column);
        if(value instanceof java.math.BigInteger big){
            if(big.signum()<0||big.bitLength()>64)throw new SQLException("uint64原件越界");return big.longValue();
        }
        if(value instanceof Byte||value instanceof Short||value instanceof Integer||value instanceof Long){
            long number=((Number)value).longValue();if(number<0)throw new SQLException("有符号uint64原件越界");return number;
        }
        if(value instanceof java.math.BigDecimal decimal){try{return unsignedDecimal(decimal);}catch(ArithmeticException invalid){throw new SQLException("uint64原件不是精确整数",invalid);}}
        throw new SQLException("uint64原件类型不符");
    }
    private static long unsignedDecimal(java.math.BigDecimal value)throws SQLException{try{var integer=value.toBigIntegerExact();if(integer.signum()<0||integer.bitLength()>64)throw new ArithmeticException();return integer.longValue();}catch(ArithmeticException invalid){throw new SQLException("uint64原件越界或非整数",invalid);}}
    boolean publicationOpen(Connection raw)throws SQLException{return current(raw).run().length==0;}
    public boolean prepareOnly(){return maintenanceRun.length!=0;}
}

