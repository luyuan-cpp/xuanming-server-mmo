package org.jeecg.modules.gm;

import java.util.List;
import org.apache.shiro.subject.Subject;
import org.apache.shiro.util.ThreadContext;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.boot.DefaultApplicationArguments;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.SingleConnectionDataSource;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.servlet.HandlerInterceptor;
import org.springframework.web.servlet.config.annotation.InterceptorRegistry;
import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

class GmInstallationTest {
    private SingleConnectionDataSource data;
    private JdbcTemplate db;
    private GmInstallation installation;
    private MockMvc mvc;
    private final Subject subject = mock(Subject.class);

    @BeforeEach void setup() {
        data = new SingleConnectionDataSource("jdbc:h2:mem:gm_admin;MODE=MySQL;DATABASE_TO_LOWER=TRUE", "sa", "", true);
        db = new JdbcTemplate(data);
        db.execute("CREATE TABLE gm_install(version INTEGER PRIMARY KEY, installed_at TIMESTAMP NOT NULL)");
        db.update("INSERT INTO gm_install VALUES(1,CURRENT_TIMESTAMP)");
        db.execute("CREATE TABLE sys_role_index(id VARCHAR(32) PRIMARY KEY,role_code VARCHAR(50),url VARCHAR(100),component VARCHAR(255),is_route INTEGER,priority INTEGER,status VARCHAR(2),relation_type VARCHAR(20))");
        db.update("INSERT INTO sys_role_index VALUES('demo_index','DEF_INDEX_ALL','/dashboard/analysis','dashboard/Analysis',1,0,'1','DEFAULT')");
        db.execute("CREATE TABLE sys_user(id VARCHAR(32), password VARCHAR(255))");
        db.update("INSERT INTO sys_user VALUES('admin','operator-changed-password')");
        db.execute("CREATE TABLE sys_role(id VARCHAR(32),role_name VARCHAR(200),role_code VARCHAR(50),description VARCHAR(255),create_by VARCHAR(32),create_time TIMESTAMP,tenant_id INT)");
        db.update("INSERT INTO sys_role(id,role_code) VALUES('role_admin','admin')");
        db.execute("CREATE TABLE sys_role_permission(id VARCHAR(32) PRIMARY KEY,role_id VARCHAR(32),permission_id VARCHAR(32))");
        db.execute("CREATE TABLE sys_permission(id VARCHAR(32) PRIMARY KEY,parent_id VARCHAR(32),name VARCHAR(100),url VARCHAR(100),component VARCHAR(100),is_route INT,menu_type INT,perms VARCHAR(100),perms_type VARCHAR(2),sort_no INT,is_leaf INT,hidden INT,del_flag INT,status VARCHAR(2),internal_or_external INT)");
        mount(new GmInstallation(db, new DataSourceTransactionManager(data), "", "native", "127.0.0.1"));
        ThreadContext.bind(subject);
    }

    private void mount(GmInstallation candidate) {
        installation = candidate;
        var registry = new TestRegistry();
        installation.addInterceptors(registry);
        mvc = MockMvcBuilders.standaloneSetup(new NativeEndpoint()).addInterceptors(registry.interceptor()).build();
    }

    @AfterEach void cleanup() {
        ThreadContext.unbindSubject();
        data.destroy();
    }

    @Test void homepageMigrationUsesNativeDefaultAndDoesNotResetAccountsOnRestart() throws Exception {
        start();
        assertEquals("/gm/console", db.queryForObject("SELECT url FROM sys_role_index WHERE role_code='DEF_INDEX_ALL' AND status='1'", String.class));
        assertEquals("gm/console/index", db.queryForObject("SELECT component FROM sys_role_index WHERE id='gm_default_index'", String.class));
        assertEquals("DEFAULT", db.queryForObject("SELECT relation_type FROM sys_role_index WHERE id='gm_default_index'", String.class));
        assertEquals(1, db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=2", Integer.class));
        assertEquals(15, db.queryForObject("SELECT COUNT(*) FROM sys_role_permission WHERE role_id='role_admin'", Integer.class));
        assertEquals(1, db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=6", Integer.class));
        assertEquals(2, db.queryForObject("SELECT COUNT(*) FROM sys_permission WHERE perms IN ('gm:system-mail:prepare','gm:system-mail:send')", Integer.class));
        db.update("UPDATE sys_role_index SET priority=5 WHERE id='gm_default_index'");
        start();
        assertEquals(5, db.queryForObject("SELECT priority FROM sys_role_index WHERE id='gm_default_index'", Integer.class));
        assertEquals("operator-changed-password", db.queryForObject("SELECT password FROM sys_user", String.class));
    }

    @Test void homepageFailureRollsBackAndKeepsRequestsClosed() throws Exception {
        db.update("INSERT INTO sys_role_index(id,status) VALUES('gm_default_index','1')");
        assertThrows(Exception.class, this::start);
        assertEquals("1", db.queryForObject("SELECT status FROM sys_role_index WHERE id='demo_index'", String.class));
        assertEquals(0, db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=2", Integer.class));
        request("/sys/login", 503);
    }

    @Test void independentConsoleHidesDemoMenusButPreservesAccountControlsAndPermissions() throws Exception {
        db.update("INSERT INTO sys_permission(id,url,menu_type,hidden,is_leaf) VALUES('gm_console','/gm/console',0,0,1)");
        for (String route : List.of("/isystem", "/system/user", "/system/role", "/system/menu", "/dashboard/analysis", "/ai")) {
            db.update("INSERT INTO sys_permission(id,url,menu_type,hidden) VALUES(?,?,1,0)", route, route);
        }
        db.update("INSERT INTO sys_permission(id,menu_type,hidden,perms) VALUES('native_button',2,0,'system:user:add')");
        start();
        assertEquals(0, db.queryForObject("SELECT is_leaf FROM sys_permission WHERE id='gm_console'", Integer.class));
        for (String route : List.of("/isystem", "/system/user", "/system/role", "/system/menu")) {
            assertEquals(0, db.queryForObject("SELECT hidden FROM sys_permission WHERE id=?", Integer.class, route));
        }
        for (String route : List.of("/dashboard/analysis", "/ai")) {
            assertEquals(1, db.queryForObject("SELECT hidden FROM sys_permission WHERE id=?", Integer.class, route));
        }
        assertEquals("system:user:add", db.queryForObject("SELECT perms FROM sys_permission WHERE id='native_button'", String.class));
        assertEquals(15, db.queryForObject("SELECT COUNT(*) FROM sys_role_permission WHERE role_id='role_admin'", Integer.class));
        db.update("UPDATE sys_permission SET hidden=0 WHERE id='/ai'");
        start();
        assertEquals(0, db.queryForObject("SELECT hidden FROM sys_permission WHERE id='/ai'", Integer.class));
        assertEquals(1, db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=4", Integer.class));
    }

    @Test void nativeLogoutReachesItsOwnTokenValidatorWithoutAnAuthenticatedSubject() throws Exception {
        start();
        request("/sys/logout", 200);
        request("/sys/login", 200);
        request("/sys/randomImage/123", 200);
        request("/gm/overview", 401);
        request("/sys/user/getUserInfo", 401);
    }

    @Test void operatorCanLoadNativeShellButNotUnrelatedSystemAdministration() throws Exception {
        start();
        when(subject.isAuthenticated()).thenReturn(true);
        for (String path : List.of("/sys/user/getUserInfo", "/sys/permission/getUserPermissionByToken", "/sys/permission/getPermCode",
                "/sys/dict/queryAllDictItems", "/sys/user/verifyIzDefaultPwd", "/sys/annountCement/getUnreadMessageCount", "/gm/overview")) {
            request(path, 200);
        }
        request("/sys/role/list", 403);
        request("/sys/user/register", 403);
        request("/jmreport/list", 403);
    }

    @Test void administratorStillCannotUseAlternativePublicLoginOrRegistration() throws Exception {
        start();
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.hasRole("admin")).thenReturn(true);
        request("/sys/role/list", 200);
        for (String path : List.of("/sys/user/register", "/sys/phoneLogin", "/sys/thirdLogin/github", "/sys/getLoginQrcode", "/sys/user/passwordChange")) {
            request(path, 403);
        }
    }

    @Test void standardRolesSeparateProposingFromReleasingAndAuditing() throws Exception {
        start();
        assertEquals(List.of("gm_approver", "gm_auditor", "gm_operator"),
                db.queryForList("SELECT role_code FROM sys_role WHERE id LIKE 'gm_role_%' ORDER BY role_code", String.class));
        // 发起人有两个发起权限、没有放行权限；批准人相反；审计只有只读四项。七项玩家 GM 的权限串发起与放行共用，两边都有。
        assertEquals(List.of("gm_console", "gm_ops_mall_permission_0", "gm_ops_mall_permission_1",
                "gm_permission_0", "gm_permission_1", "gm_permission_2", "gm_permission_4",
                "gm_player_permission_0", "gm_player_permission_1", "gm_player_permission_2", "gm_player_permission_3",
                "gm_player_permission_4", "gm_player_permission_5", "gm_player_permission_6", "gm_sys_mail_permission_0",
                "gm_trade_permission_0", "gm_trade_permission_1"), granted("gm_role_operator"));
        assertEquals(List.of("gm_console", "gm_ops_mall_permission_0", "gm_ops_mall_permission_2",
                "gm_permission_0", "gm_permission_1", "gm_permission_3", "gm_permission_4",
                "gm_player_permission_0", "gm_player_permission_1", "gm_player_permission_2", "gm_player_permission_3",
                "gm_player_permission_4", "gm_player_permission_5", "gm_player_permission_6", "gm_sys_mail_permission_1",
                "gm_trade_permission_0", "gm_trade_permission_2"), granted("gm_role_approver"));
        assertEquals(List.of("gm_console", "gm_ops_mall_permission_0", "gm_permission_0", "gm_permission_1", "gm_permission_4",
                "gm_trade_permission_0"), granted("gm_role_auditor"));
        // 运营处置按下游分权限(迁移 9 起):gm:<target>:read / prepare / approve。
        assertEquals(List.of("gm:mall:approve", "gm:mall:prepare", "gm:mall:read"),
                db.queryForList("SELECT perms FROM sys_permission WHERE id LIKE 'gm_ops_mall_permission_%' ORDER BY perms", String.class));
        assertEquals(List.of("gm:trade:approve", "gm:trade:prepare", "gm:trade:read"),
                db.queryForList("SELECT perms FROM sys_permission WHERE id LIKE 'gm_trade_permission_%' ORDER BY perms", String.class));
        // 重启不重复建角色，也不动管理员已有的授权。
        start();
        assertEquals(3, db.queryForObject("SELECT COUNT(*) FROM sys_role WHERE id LIKE 'gm_role_%'", Integer.class));
        assertEquals(15, db.queryForObject("SELECT COUNT(*) FROM sys_role_permission WHERE role_id='role_admin'", Integer.class));
    }

    private List<String> granted(String roleId) {
        return db.queryForList("SELECT permission_id FROM sys_role_permission WHERE role_id=? ORDER BY permission_id", String.class, roleId);
    }

    @Test void nativePasswordModeOnlyStartsOnALoopbackListenAddress() {
        var transactions = new DataSourceTransactionManager(data);
        for (String address : List.of("0.0.0.0", "", "10.0.0.8")) {
            assertThrows(IllegalStateException.class, () -> new GmInstallation(db, transactions, "", "native", address), address);
        }
        assertDoesNotThrow(() -> new GmInstallation(db, transactions, "", "native", "::1"));
        assertDoesNotThrow(() -> new GmInstallation(db, transactions, "", "oidc", "0.0.0.0"));
        assertThrows(IllegalStateException.class, () -> new GmInstallation(db, transactions, "", "password", "127.0.0.1"));
    }

    @Test void oidcModeClosesNativePasswordLoginAndEveryNativeAdministrationPage() throws Exception {
        mount(new GmInstallation(db, new DataSourceTransactionManager(data), "", "oidc", "0.0.0.0"));
        start();
        for (String path : List.of("/sys/logout", "/gm/sso/config", "/gm/sso/authorize", "/gm/sso/token")) {
            request(path, 200);
        }
        // 原生口令登录及其验证码在 OIDC 模式下不是公开入口；没有会话的请求到不了上游登录控制器。
        for (String path : List.of("/sys/login", "/sys/checkCaptcha", "/sys/getEncryptedString", "/sys/randomImage/123", "/gm/overview")) {
            request(path, 401);
        }
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.hasRole("admin")).thenReturn(true);
        for (String path : List.of("/sys/user/getUserInfo", "/sys/permission/getUserPermissionByToken", "/gm/overview")) {
            request(path, 200);
        }
        // 管理员角色也进不了原生管理页：角色来自身份提供方，本地没有要管的东西。
        for (String path : List.of("/sys/login", "/sys/role/list", "/sys/user/list", "/sys/user/add", "/sys/permission/list", "/jmreport/list")) {
            request(path, 403);
        }
    }

    @Test void nativeModeLetsTheLoginPageAskWhichModeIsActive() throws Exception {
        start();
        request("/gm/sso/config", 200);
        request("/gm/sso/authorize", 401);
        request("/gm/sso/token", 401);
    }

    @Test void indexMigrationCreatesAuditAndQueryIndexesIdempotently() throws Exception {
        request("/actuator/health/liveness", 200);
        request("/actuator/health/readiness", 503);
        start();
        request("/actuator/health/readiness", 200);
        request("/actuator/health", 401);
        assertEquals(1, db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=5", Integer.class));
        for (String index : List.of("idx_gm_command_type_created", "idx_gm_command_type_player_created", "idx_gm_command_type_status_created")) {
            assertTrue(hasIndex("gm_command", index), "Missing index: " + index);
        }
        assertTrue(hasIndex("gm_command_audit", "idx_gm_command_audit_command"));
        // 删除版本 5 标记后重跑仍幂等，不会因索引已存在而报错。
        db.update("DELETE FROM gm_install WHERE version=5");
        start();
        assertEquals(1, db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=5", Integer.class));
    }

    private boolean hasIndex(String table, String indexName) {
        return Boolean.TRUE.equals(db.execute((org.springframework.jdbc.core.ConnectionCallback<Boolean>) connection -> {
            try (var indexes = connection.getMetaData().getIndexInfo(connection.getCatalog(), null, table, false, false)) {
                while (indexes.next()) {
                    String name = indexes.getString("INDEX_NAME");
                    if (name != null && name.equalsIgnoreCase(indexName)) return true;
                }
                return false;
            }
        }));
    }

    private void start() throws Exception { installation.run(new DefaultApplicationArguments(new String[0])); }
    private void request(String path, int expectedStatus) throws Exception {
        mvc.perform(get(path).servletPath(path)).andExpect(status().is(expectedStatus));
    }
    private static class TestRegistry extends InterceptorRegistry {
        HandlerInterceptor interceptor() { return (HandlerInterceptor) getInterceptors().get(0); }
    }
    @RestController public static class NativeEndpoint {
        @RequestMapping("/**") public String endpoint() { return "native-handler-reached"; }
    }
}
