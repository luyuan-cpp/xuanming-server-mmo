package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.net.URI;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.time.Clock;
import java.time.Instant;
import java.time.ZoneId;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicLong;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.core.io.ClassPathResource;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.SingleConnectionDataSource;
import org.springframework.jdbc.datasource.init.ResourceDatabasePopulator;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;

class GmOidcLoginTest {
    private static final String MAPPING = "{\"gm-admins\":\"admin\",\"gm-approvers\":\"gm_approver\"}";
    private final ObjectMapper json = new ObjectMapper();
    private final AtomicLong nowMs = new AtomicLong(1_800_000_000_000L);
    private final Clock clock = new Clock() {
        @Override public ZoneId getZone() { return ZoneId.of("UTC"); }
        @Override public Clock withZone(ZoneId zone) { return this; }
        @Override public Instant instant() { return Instant.ofEpochMilli(nowMs.get()); }
    };
    private final List<String> issued = new ArrayList<>();
    private final List<String> evicted = new ArrayList<>();
    private final GmOidcLogin.UpstreamSessions sessions = new GmOidcLogin.UpstreamSessions() {
        @Override public String issue(String username, String secret, long ttlSeconds) {
            issued.add(username + "|" + secret + "|" + ttlSeconds);
            return "session-" + issued.size();
        }
        @Override public void evictAuthorization(String userId) { evicted.add(userId); }
    };
    private SingleConnectionDataSource data;
    private JdbcTemplate db;
    private FakeIdentityProvider idp;
    private GmOidcLogin login;

    @BeforeEach void setup() throws Exception {
        data = new SingleConnectionDataSource("jdbc:h2:mem:gm_oidc_login;MODE=MySQL;DATABASE_TO_LOWER=TRUE", "sa", "", true);
        db = new JdbcTemplate(data);
        new ResourceDatabasePopulator(new ClassPathResource("gm-schema.sql")).execute(data);
        // 上游账号表的用户名列不分大小写;测试库用同样的比较规则,才能复现大小写碰撞。
        db.execute("CREATE TABLE sys_user(id VARCHAR(32) PRIMARY KEY,username VARCHAR_IGNORECASE(100) UNIQUE,realname VARCHAR(100),"
                + "password VARCHAR(255),salt VARCHAR(45),status TINYINT,del_flag TINYINT,create_by VARCHAR(32),create_time TIMESTAMP)");
        db.execute("CREATE TABLE sys_role(id VARCHAR(32) PRIMARY KEY,role_code VARCHAR(50))");
        db.execute("CREATE TABLE sys_user_role(id VARCHAR(32) PRIMARY KEY,user_id VARCHAR(32),role_id VARCHAR(32),tenant_id INT)");
        db.update("INSERT INTO sys_role VALUES('role_admin','admin')");
        db.update("INSERT INTO sys_role VALUES('role_approver','gm_approver')");
        idp = new FakeIdentityProvider();
        login = login("oidc", MAPPING);
    }

    @AfterEach void cleanup() {
        idp.close();
        db.execute("DROP ALL OBJECTS");
        data.destroy();
    }

    private GmOidcLogin login(String mode, String mapping) {
        return new GmOidcLogin(db, new DataSourceTransactionManager(data), json, idp.provider(), sessions, mode, mapping, 12, clock);
    }

    @Test void firstLoginCreatesTheAccountUnderTheSubjectAndLaterLoginsReplaceItsRoles() throws Exception {
        assertEquals("session-1", signIn("alice.w", List.of("gm-admins", "gm-approvers", "unrelated")));
        String userId = db.queryForObject("SELECT id FROM sys_user WHERE username='alice.w'", String.class);
        assertEquals("测试员", db.queryForObject("SELECT realname FROM sys_user WHERE id=?", String.class, userId));
        assertEquals(List.of("role_admin", "role_approver"), roles(userId));
        // 会话用账号行里那段随机数签名,有效期是配置的上限,到期不续。
        String secret = db.queryForObject("SELECT password FROM sys_user WHERE id=?", String.class, userId);
        assertEquals(64, secret.length());
        assertEquals(List.of("alice.w|" + secret + "|43200"), issued);
        assertEquals(List.of(userId), evicted);
        assertEquals(0, db.queryForObject("SELECT COUNT(*) FROM gm_oidc_attempt", Integer.class));

        // 调岗:身份提供方里少了一个分组,下次登录后台的角色跟着少。账号与签名密钥不变。
        assertEquals("session-2", signIn("alice.w", List.of("gm-approvers")));
        assertEquals(1, db.queryForObject("SELECT COUNT(*) FROM sys_user", Integer.class));
        assertEquals(List.of("role_approver"), roles(userId));
        assertEquals("alice.w|" + secret + "|43200", issued.get(1));
    }

    @Test void aLoginAttemptCanBeRedeemedOnlyOnceAndOnlyWithinFiveMinutes() throws Exception {
        String[] attempt = begin();
        idp.nextIdToken = idp.sign(idp.claims("alice", attempt[1]).build());
        login.token(body("code", attempt[0]));
        assertStatus(401, () -> login.token(body("code", attempt[0])));
        assertStatus(401, () -> login.token(body("code", "state-nobody-issued")));

        String[] slow = begin();
        nowMs.addAndGet(GmOidcLogin.ATTEMPT_TTL_MS + 1);
        idp.nextIdToken = idp.sign(idp.claims("alice", slow[1]).build());
        int before = idp.tokenRequests;
        assertStatus(401, () -> login.token(body("code", slow[0])));
        // 过期的尝试不去换码,也不留在表里。
        assertEquals(before, idp.tokenRequests);
        assertEquals(0, db.queryForObject("SELECT COUNT(*) FROM gm_oidc_attempt", Integer.class));
        assertEquals(1, issued.size());
    }

    @Test void aNonceFromAnotherAttemptDoesNotSignIn() throws Exception {
        String[] mine = begin();
        String[] other = begin();
        idp.nextIdToken = idp.sign(idp.claims("alice", other[1]).build());
        assertThrows(GmOidcProvider.Failure.class, () -> login.token(body("code", mine[0])));
        assertTrue(issued.isEmpty());
        assertEquals(0, db.queryForObject("SELECT COUNT(*) FROM sys_user", Integer.class));
    }

    @Test void aSubjectWithoutAMappedGroupGetsNoAccountAndNoSession() throws Exception {
        assertStatus(403, () -> signIn("mallory", List.of("engineering")));
        assertEquals(0, db.queryForObject("SELECT COUNT(*) FROM sys_user", Integer.class));
        assertTrue(issued.isEmpty());
    }

    @Test void aMappingToARoleThatDoesNotExistIsRefusedInsteadOfSilentlyGrantingLess() throws Exception {
        login = login("oidc", "{\"gm-admins\":\"admin\",\"gm-auditors\":\"gm_auditor_missing\"}");
        assertStatus(403, () -> signIn("alice", List.of("gm-admins", "gm-auditors")));
        // 整个登录事务回滚:不留下只有一半角色的账号。
        assertEquals(0, db.queryForObject("SELECT COUNT(*) FROM sys_user", Integer.class));
        assertTrue(issued.isEmpty());
    }

    @Test void aSubjectThatOnlyDiffersByCaseFromAnExistingAccountIsNotTheSamePerson() throws Exception {
        signIn("Alice", List.of("gm-admins"));
        assertStatus(403, () -> signIn("alice", List.of("gm-admins")));
        assertEquals("Alice", db.queryForObject("SELECT username FROM sys_user", String.class));
        assertEquals(1, issued.size());
    }

    @Test void aFrozenOrDeletedAccountCannotSignInEvenThoughTheIdentityProviderAcceptsIt() throws Exception {
        signIn("alice", List.of("gm-admins"));
        db.update("UPDATE sys_user SET status=2");
        assertStatus(403, () -> signIn("alice", List.of("gm-admins")));
        db.update("UPDATE sys_user SET status=1,del_flag=1");
        assertStatus(403, () -> signIn("alice", List.of("gm-admins")));
        assertEquals(1, issued.size());
    }

    @Test void nativeModeAnswersItsModeAndOffersNoEnterpriseLogin() throws Exception {
        GmOidcLogin local = new GmOidcLogin(db, new DataSourceTransactionManager(data), json, idp.provider(), null, "native", "", 12, clock);
        assertFalse(local.oidc());
        assertEquals("native", json.readTree(local.config()).path("result").path("mode").asText());
        assertStatus(404, local::authorize);
        assertStatus(404, () -> local.token(body("code", "state")));
        assertEquals("oidc", json.readTree(login.config()).path("result").path("mode").asText());
    }

    @Test void oidcModeRefusesToStartWithoutARoleMappingOrWithAnUnboundedSession() {
        assertThrows(IllegalStateException.class, () -> login("oidc", ""));
        assertThrows(IllegalStateException.class, () -> login("oidc", "{}"));
        assertThrows(IllegalStateException.class, () -> login("oidc", "[\"gm-admins\"]"));
        assertThrows(IllegalStateException.class, () -> login("oidc", "{\"gm-admins\":\"bad role\"}"));
        assertThrows(IllegalStateException.class, () -> login("sso", MAPPING));
        var transactions = new DataSourceTransactionManager(data);
        assertThrows(IllegalStateException.class, () -> new GmOidcLogin(db, transactions, json, idp.provider(), sessions, "oidc", MAPPING, 0, clock));
        assertThrows(IllegalStateException.class, () -> new GmOidcLogin(db, transactions, json, idp.provider(), sessions, "oidc", MAPPING, 25, clock));
    }

    @Test void malformedCallbackBodiesAreRejectedBeforeAnyLookup() {
        for (String body : List.of("", "not json", "{}", "{\"code\":\"c\"}", "{\"state\":\"s\"}",
                "{\"code\":\"" + "c".repeat(2_049) + "\",\"state\":\"s\"}", "{\"code\":\"c\",\"state\":\"" + "s".repeat(65) + "\"}")) {
            assertStatus(400, () -> login.token(body));
        }
    }

    /** 发起一次登录,返回 {state, nonce}:都从后台给出的授权地址里取,和浏览器看到的一样。 */
    private String[] begin() throws Exception {
        JsonNode reply = json.readTree(login.authorize());
        String query = URI.create(reply.path("result").path("url").asText()).getRawQuery();
        String state = null;
        String nonce = null;
        for (String pair : query.split("&")) {
            String[] parts = pair.split("=", 2);
            if (parts[0].equals("state")) state = URLDecoder.decode(parts[1], StandardCharsets.UTF_8);
            if (parts[0].equals("nonce")) nonce = URLDecoder.decode(parts[1], StandardCharsets.UTF_8);
        }
        assertNotNull(state);
        assertNotNull(nonce);
        return new String[] {state, nonce};
    }

    private String signIn(String subject, List<String> groups) throws Exception {
        String[] attempt = begin();
        idp.nextIdToken = idp.sign(idp.claims(subject, attempt[1]).claim("groups", groups).build());
        return json.readTree(login.token(body("code-" + subject, attempt[0]))).path("result").path("token").asText();
    }

    private String body(String code, String state) {
        return json.createObjectNode().put("code", code).put("state", state).toString();
    }

    private List<String> roles(String userId) {
        return db.queryForList("SELECT role_id FROM sys_user_role WHERE user_id=? ORDER BY role_id", String.class, userId);
    }

    private interface Call { void run() throws Exception; }

    private static void assertStatus(int expected, Call call) {
        ResponseStatusException rejected = assertThrows(ResponseStatusException.class, call::run);
        assertEquals(expected, rejected.getStatusCode().value());
    }
}
