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
        db.execute("CREATE TABLE sys_role(id VARCHAR(32),role_code VARCHAR(50))");
        db.update("INSERT INTO sys_role VALUES('role_admin','admin')");
        db.execute("CREATE TABLE sys_role_permission(id VARCHAR(32) PRIMARY KEY,role_id VARCHAR(32),permission_id VARCHAR(32))");
        db.execute("CREATE TABLE sys_permission(id VARCHAR(32) PRIMARY KEY,parent_id VARCHAR(32),name VARCHAR(100),url VARCHAR(100),component VARCHAR(100),is_route INT,menu_type INT,perms VARCHAR(100),perms_type VARCHAR(2),sort_no INT,is_leaf INT,hidden INT,del_flag INT,status VARCHAR(2),internal_or_external INT)");
        installation = new GmInstallation(db, new DataSourceTransactionManager(data), "");
        var registry = new TestRegistry();
        installation.addInterceptors(registry);
        mvc = MockMvcBuilders.standaloneSetup(new NativeEndpoint()).addInterceptors(registry.interceptor()).build();
        ThreadContext.bind(subject);
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
        assertEquals(7, db.queryForObject("SELECT COUNT(*) FROM sys_role_permission WHERE role_id='role_admin'", Integer.class));
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
        assertEquals(7, db.queryForObject("SELECT COUNT(*) FROM sys_role_permission WHERE role_id='role_admin'", Integer.class));
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

    @Test void indexMigrationCreatesAuditAndQueryIndexesIdempotently() throws Exception {
        start();
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
