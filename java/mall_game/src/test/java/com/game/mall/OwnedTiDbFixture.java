package com.game.mall;

import java.net.InetAddress;
import java.net.URI;
import java.sql.SQLException;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.game.authority.WriterAuthority;
import com.zaxxer.hikari.HikariDataSource;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.LinkOption;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import javax.sql.DataSource;
import org.springframework.boot.autoconfigure.jdbc.DataSourceProperties;
import org.springframework.jdbc.datasource.DelegatingDataSource;

/** 仅测试的自有随机库NORMAL夹具；控制面复用正规producer，当前物理/I3资格由执行者提供。 */
final class OwnedTiDbFixture {
    private static final ObjectMapper JSON = new ObjectMapper();
    private static volatile boolean unresolved;
    private OwnedTiDbFixture() {}

    /** 每次open创建一个新owner。任何未确认尝试都保留原件，不换ID、重发或兜底为Schema连接。 */
    static Owner open() throws IOException {
        if (unresolved) { throw new IOException("本轮真库已有UNKNOWN原件；禁止换owner或ID继续"); }
        String configured = endpoint(System.getProperty("commerce.it.tidb", ""));
        if (!configured.equals("jdbc:mysql://127.0.0.1:24000/")) {
            throw new IOException("当前真库provider仅取得127.0.0.1:24000资格");
        }
        Path parent = Path.of(required("COMMERCE_IT_PRIVATE_ROOT")).toAbsolutePath().normalize();
        WriterAuthority.requirePrivateDirectory(parent);
        String nonce = UUID.randomUUID().toString().replace("-", "");
        Path root = Files.createDirectory(parent.resolve("java-fixture-" + nonce));
        WriterAuthority.requirePrivateDirectory(root);
        Owner owner = new Owner(root, nonce);
        try {
            owner.first = owner.invoke("create", Map.of("endpoint", configured, "schema", "mall_it_" + nonce));
            owner.ownerFile = privatePath(root, text(owner.first, "ownerFile"));
            owner.ownerSha = text(owner.first, "ownerSha256");
            return owner;
        } catch (IOException | RuntimeException failed) { owner.hold = true; unresolved = true; throw failed; }
    }

    static DataSource openHttpDataSource(DataSourceProperties source, CommerceProperties props,
            String contextFile, String contextSha) throws IOException, SQLException {
        Path path = Path.of(contextFile).toAbsolutePath().normalize();
        // 仅接收本进程新建owner下、由prepareHttp产生的上下文；不能把接口变成任意历史私密文件读取器。
        Owner owner = HttpOwner.take(path, contextSha);
        try {
            byte[] bytes = WriterAuthority.readPrivateControlFile(path, 1024 * 1024);
            if (!sha(bytes).equalsIgnoreCase(contextSha)) { throw new IOException("HTTP上下文原件漂移"); }
            return owner.openPool(source, props, owner.first, true);
        } catch (IOException | SQLException | RuntimeException failed) { owner.hold = true; unresolved = true; throw failed; }
    }

    private static final class HttpOwner {
        private static Owner prepared;
        static synchronized Owner prepare() throws IOException {
            if (prepared == null) { prepared = open(); }
            return prepared;
        }
        static synchronized Owner take(Path path, String sha) throws IOException {
            if (prepared == null || prepared.httpTaken || !prepared.firstPath.equals(path)
                    || !prepared.firstSha.equalsIgnoreCase(sha)) { throw new IOException("HTTP真库上下文不是本例未消费原件"); }
            prepared.httpTaken = true;
            return prepared;
        }
    }
    static Owner prepareHttp() throws IOException { return HttpOwner.prepare(); }

    static final class Owner implements AutoCloseable {
        private final Path root;
        private final String nonce;
        private final List<OwnedPool> pools = new ArrayList<>();
        private JsonNode first;
        private Path firstPath;
        private String firstSha;
        private Path ownerFile;
        private String ownerSha;
        private boolean firstTaken;
        private boolean httpTaken;
        private boolean hold;
        private boolean closed;
        private int sequence;
        private Owner(Path root, String nonce) { this.root = root; this.nonce = nonce; }
        String jdbcUrl() throws IOException { return text(first, "jdbcUrl"); }
        String username() throws IOException { return text(first, "username"); }
        String password() throws IOException { return text(first, "password"); }
        String contextFile() { return firstPath.toString(); }
        String contextSha256() { return firstSha; }
        OwnedPool openPool(CommerceProperties props) throws IOException, SQLException {
            var source = new DataSourceProperties(); source.setUrl(jdbcUrl());
            return openPool(source, props);
        }
        synchronized OwnedPool openPool(DataSourceProperties source, CommerceProperties props) throws IOException, SQLException {
            if (closed || hold) { throw new IOException("真库owner已关闭或HOLD"); }
            try {
                if (!props.isRequireTidb()) { throw new IOException("真库NORMAL池必须保持TiDB门禁"); }
                requireSameSchemaUrl(jdbcUrl(), source.getUrl());
                JsonNode context = firstTaken ? invoke("open-boot", Map.of()) : first;
                firstTaken = true;
                return openPool(source, props, context, false);
            } catch (IOException | SQLException | RuntimeException failed) { hold = true; unresolved = true; throw failed; }
        }
        private synchronized OwnedPool openPool(DataSourceProperties source, CommerceProperties props,
                JsonNode context, boolean closeOwner) throws IOException, SQLException {
            if (closed || hold || !props.isRequireTidb()) { throw new IOException("真库NORMAL池必须保持TiDB门禁"); }
            String expected = text(context, "jdbcUrl");
            requireSameSchemaUrl(expected, source.getUrl());
            source.setUsername(text(context, "username")); source.setPassword(text(context, "password"));
            Map<String,String> environment = new LinkedHashMap<>(System.getenv());
            Map<String,String> normal = normalEnvironment(context);
            privatePath(root, normal.get("MMORPG_SQL_WRITER_GRANT_FILE"));
            privatePath(root, normal.get("MMORPG_SQL_WRITER_CONSUME_DIR"));
            environment.putAll(normal);
            WriterAuthority authority = null;
            try {
                authority = WriterAuthority.consume(environment);
                var pool = (HikariDataSource) new DatabaseConfiguration().dataSource(source, props, authority);
                var owned = new OwnedPool(pool, this, closeOwner); pools.add(owned); return owned;
            } catch (IOException | SQLException | RuntimeException failed) {
                if (authority != null) { authority.seal(); }
                hold = true; unresolved = true; throw failed;
            }
        }
        synchronized void migrate(String resource) throws IOException {
            if (closed || hold || !List.of("db/migration/V3__catalog_admin.sql", "db/migration/V4__mock_payment.sql", "db/migration/V5__rmb_consignment.sql").contains(resource)) {
                throw new IOException("真库fixture仅接受已有商城迁移事实源");
            }
            invoke("migrate", Map.of("resource", resource));
        }
        private synchronized JsonNode invoke(String action, Map<String,String> values) throws IOException {
            try { return invokeOnce(action, values); }
            catch (IOException | RuntimeException failed) { hold = true; unresolved = true; throw failed; }
        }
        private JsonNode invokeOnce(String action, Map<String,String> values) throws IOException {
            if (hold || closed) { throw new IOException("真库fixture处于HOLD或已关闭"); }
            int attempt = ++sequence;
            Path request = root.resolve("action-" + attempt + ".request.json");
            Path output = root.resolve("action-" + attempt + ".context.json");
            Map<String,Object> body = new LinkedHashMap<>();
            body.put("format", "JAVA_OWNED_TIDB_ACTION_V1"); body.put("nonce", nonce);
            body.put("root", root.toString()); body.put("outputFile", output.toString()); body.putAll(values);
            if (ownerFile != null) { body.put("ownerFile", ownerFile.toString()); body.put("ownerSha256", ownerSha); }
            byte[] encoded = JSON.writeValueAsBytes(body);
            WriterAuthority.createPrivateControlFile(request, encoded);
            Path script = qualifiedPublic("COMMERCE_IT_PROVIDER_SCRIPT", "COMMERCE_IT_PROVIDER_SCRIPT_SHA256");
            Path shell = qualifiedPublic("COMMERCE_IT_PWSH", "COMMERCE_IT_PWSH_SHA256");
            Process process = new ProcessBuilder(shell.toString(), "-NoProfile", "-NonInteractive", "-File", script.toString(),
                    "-JavaFixtureAction", action, "-JavaFixtureRequestFile", request.toString(),
                    "-JavaFixtureRequestSha256", sha(encoded)).redirectOutput(ProcessBuilder.Redirect.DISCARD)
                    .redirectError(ProcessBuilder.Redirect.DISCARD).start();
            try {
                if (!process.waitFor(120, TimeUnit.SECONDS)) { hold = true; throw new IOException("真库fixture超时，保留原件HOLD"); }
                if (process.exitValue() != 0) { hold = true; throw new IOException("真库fixture未确认，保留原件HOLD"); }
                byte[] result = WriterAuthority.readPrivateControlFile(output, 1024 * 1024);
                JsonNode context;
                try { context = JSON.readTree(result); }
                catch (com.fasterxml.jackson.core.JsonProcessingException invalid) {
                    // context含NORMAL口令，不能把解析器的原文片段放进测试失败日志。
                    throw new IOException("真库fixture响应JSON未通过合同，保留原件HOLD");
                }
                if (!"JAVA_OWNED_TIDB_CONTEXT_V1".equals(text(context, "format")) || !nonce.equals(text(context, "nonce"))) {
                    hold = true; throw new IOException("真库fixture响应所有权不符");
                }
                if (action.equals("create")) { firstPath = output; firstSha = sha(result); }
                return context;
            } catch (InterruptedException interrupted) {
                Thread.currentThread().interrupt(); hold = true; throw new IOException("真库fixture等待中断，保留原件HOLD");
            } finally {
                if (process.isAlive()) {
                    process.descendants().forEach(ProcessHandle::destroyForcibly); process.destroyForcibly();
                    try { if (!process.waitFor(5, TimeUnit.SECONDS)) { hold = true; } }
                    catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); hold = true; }
                }
            }
        }
        @Override public synchronized void close() {
            if (closed) { return; }
            RuntimeException closeFailure = null;
            for (OwnedPool pool : pools) {
                try { pool.closePool(); }
                catch (RuntimeException failed) { hold = true; unresolved = true; if (closeFailure == null) { closeFailure = failed; } else { closeFailure.addSuppressed(failed); } }
            }
            if (closeFailure != null) { throw closeFailure; }
            if (hold) { throw new IllegalStateException("真库fixture保留HOLD；不自动RETIRE重发或DROP"); }
            try { invoke("close", Map.of()); closed = true; }
            catch (IOException failed) { hold = true; throw new IllegalStateException("真库fixture退休或清理未确认，保留原件HOLD", failed); }
        }
    }

    static final class OwnedPool extends DelegatingDataSource implements AutoCloseable {
        private final HikariDataSource pool;
        private final Owner owner;
        private final boolean closeOwner;
        private boolean closed;
        private OwnedPool(HikariDataSource pool, Owner owner, boolean closeOwner) {
            super(pool); this.pool = pool; this.owner = owner; this.closeOwner = closeOwner;
        }
        synchronized void closePool() {
            if (!closed) {
                try { pool.close(); closed = true; }
                catch (RuntimeException failed) { owner.hold = true; unresolved = true; throw failed; }
            }
        }
        @Override public void close() { closePool(); if (closeOwner) { owner.close(); } }
    }

    static Path privatePath(Path root, String candidate) throws IOException {
        Path path = Path.of(candidate);
        if (!path.isAbsolute()) { throw new IOException("fixture私密路径必须显式绝对路径"); }
        path = path.normalize();
        if (!path.startsWith(root) || path.equals(root)) { throw new IOException("fixture私密路径越过本例owner"); }
        return path;
    }
    static void requireSameSchemaUrl(String expected, String actual) throws IOException {
        if (actual == null || !(actual.equals(expected) || (actual.startsWith(expected + "?sessionVariables=")
                && actual.substring(expected.length()).matches("\\?sessionVariables=[A-Za-z0-9%+._*~-]+")))) {
            throw new IOException("NORMAL池不能切换本例库/端点、凭据或连接权限参数");
        }
    }
    static Map<String,String> normalEnvironment(JsonNode context) throws IOException {
        List<String> names = List.of("MMORPG_SQL_WRITER_GRANT_FILE", "MMORPG_SQL_WRITER_GRANT_SHA256",
                "MMORPG_SQL_WRITER_CONSUME_DIR", "MMORPG_SQL_DEPLOYMENT_ID", "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256");
        JsonNode boot = context.path("environment");
        if (!boot.isObject() || boot.size() != names.size()) { throw new IOException("NORMAL上下文只能携带五项启动资格"); }
        Map<String,String> result = new LinkedHashMap<>();
        for (String name : names) { result.put(name, text(boot, name)); }
        if (!result.get(names.get(1)).matches("[0-9a-fA-F]{64}") || !result.get(names.get(4)).matches("[0-9a-fA-F]{64}")) {
            throw new IOException("NORMAL上下文缺固定grant/physical摘要");
        }
        return result;
    }
    private static Path qualifiedPublic(String name, String digestName) throws IOException {
        Path path = Path.of(required(name)); String expected = required(digestName);
        if (!path.isAbsolute() || !expected.matches("[0-9a-fA-F]{64}") || !Files.isRegularFile(path, LinkOption.NOFOLLOW_LINKS)
                || Files.isSymbolicLink(path) || !sha(Files.readAllBytes(path)).equalsIgnoreCase(expected)) {
            throw new IOException("fixture公开工具原件缺资格或漂移");
        }
        return path;
    }
    private static String required(String name) throws IOException {
        String value = System.getenv(name); if (value == null || value.isBlank()) { throw new IOException("真库fixture缺显式前置：" + name); } return value;
    }
    private static String text(JsonNode node, String field) throws IOException {
        JsonNode value = node.get(field); if (value == null || !value.isTextual() || value.asText().isBlank()) { throw new IOException("fixture上下文字段不符：" + field); } return value.asText();
    }
    private static String sha(byte[] bytes) {
        try { return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(bytes)); }
        catch (java.security.NoSuchAlgorithmException impossible) { throw new IllegalStateException(impossible); }
    }

    static String endpoint(String configured) {
        try {
            if (configured == null || !configured.startsWith("jdbc:mysql://")) { throw new IllegalArgumentException(); }
            URI uri = URI.create(configured.substring("jdbc:".length()));
            String host = uri.getHost();
            if (host == null) { throw new IllegalArgumentException(); }
            if (host.startsWith("[") && host.endsWith("]")) { host = host.substring(1, host.length() - 1); }
            // 先限制为数字IP，禁止InetAddress因localhost或其它名称而查询DNS。
            if (!(host.matches("[0-9]+(?:\\.[0-9]+){3}") || (host.contains(":") && host.matches("[0-9A-Fa-f:]+")))
                || !InetAddress.getByName(host).isLoopbackAddress() || uri.getPort() < 1 || uri.getPort() > 65535
                || uri.getRawUserInfo() != null || uri.getRawQuery() != null || uri.getRawFragment() != null
                || !(uri.getRawPath().isEmpty() || uri.getRawPath().equals("/"))) {
                throw new IllegalArgumentException();
            }
            return configured.endsWith("/") ? configured : configured + "/";
        } catch (IllegalArgumentException | java.net.UnknownHostException invalid) {
            throw new IllegalArgumentException("真实TiDB夹具仅接受显式数值loopback合法非零端口，不得指定已有库、凭据或查询参数");
        }
    }

}
