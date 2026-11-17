package org.jeecg.modules.gm;

import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.security.SecureRandom;
import java.util.HexFormat;
import java.util.List;
import java.util.Set;
import org.apache.shiro.SecurityUtils;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.ApplicationArguments;
import org.springframework.boot.ApplicationRunner;
import org.springframework.context.annotation.Configuration;
import org.springframework.core.io.ClassPathResource;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.init.ResourceDatabasePopulator;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;
import org.springframework.web.servlet.HandlerInterceptor;
import org.springframework.web.servlet.config.annotation.InterceptorRegistry;
import org.springframework.web.servlet.config.annotation.WebMvcConfigurer;

/** 仅初始化独立 GM 库；关闭演示账号和无关公开入口，后续启动不重置管理员。 */
@Configuration
public class GmInstallation implements ApplicationRunner, WebMvcConfigurer {
    // 上游 logout 为 Shiro anon，控制器自行验证签名并撤销令牌，不能要求此时已有认证 Subject。
    private static final Set<String> OPEN = Set.of("/sys/login", "/sys/checkCaptcha", "/sys/getEncryptedString", "/sys/logout");
    private static final Set<String> SESSION_READ = Set.of("/sys/user/getUserInfo", "/sys/permission/getUserPermissionByToken", "/sys/user/queryUserComponent",
            "/sys/permission/getPermCode", "/sys/dict/queryAllDictItems", "/sys/user/verifyIzDefaultPwd", "/sys/annountCement/getUnreadMessageCount");
    private final JdbcTemplate db;
    private final TransactionTemplate tx;
    private final String password;
    private volatile boolean ready;
    public GmInstallation(JdbcTemplate db, PlatformTransactionManager transactions,
            @Value("${gm.bootstrap-password:}") String password) {
        this.db = db; this.tx = new TransactionTemplate(transactions); this.password = password;
    }
    @Override public void run(ApplicationArguments args) throws Exception {
        if (!"gm_admin".equals(db.queryForObject("SELECT DATABASE()", String.class)))
            throw new IllegalStateException("GM installation requires its dedicated gm_admin database");
        new ResourceDatabasePopulator(new ClassPathResource("gm-schema.sql")).execute(db.getDataSource());
        db.execute("CREATE TABLE IF NOT EXISTS gm_install (version INTEGER PRIMARY KEY, installed_at TIMESTAMP NOT NULL)");
        if (db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=1", Integer.class) == 0) {
            if (password.length() < 20) throw new IllegalStateException("GM bootstrap password must be injected and at least 20 characters");
            byte[] saltBytes = new byte[4]; new SecureRandom().nextBytes(saltBytes);
            String salt = HexFormat.of().formatHex(saltBytes);
            // 复用上游登录算法，不维护第二套密码实现。
            String encoded = (String) Class.forName("org.jeecg.common.util.PasswordUtil")
                    .getMethod("encrypt", String.class, String.class, String.class).invoke(null, "admin", password, salt);
            if (encoded == null || encoded.isBlank()) throw new IllegalStateException("GM bootstrap password encoding failed");
            tx.executeWithoutResult(status -> {
                db.update("INSERT INTO gm_install(version,installed_at) VALUES(1,CURRENT_TIMESTAMP)");
                db.update("UPDATE sys_user SET status=2");
                int changed = db.update("UPDATE sys_user SET password=?,salt=?,status=1,del_flag=0,realname=?,phone=NULL,email=NULL,third_id=NULL,third_type=NULL,last_pwd_update_time=CURRENT_TIMESTAMP WHERE username='admin'", encoded, salt, "GM管理员");
                if (changed != 1) throw new IllegalStateException("Expected exactly one upstream bootstrap administrator");
                String userId = db.queryForObject("SELECT id FROM sys_user WHERE username='admin'", String.class);
                String roleId = db.queryForObject("SELECT id FROM sys_role WHERE role_code='admin'", String.class);
                addMenu("gm_console", null, "游戏管理", "/gm/console", "gm/console/index", 0, null);
                String[] permissions = {"gm:zone:read", "gm:player:read", "gm:mail:prepare", "gm:mail:send", "gm:audit:read"};
                String[] labels = {"查看区服", "查询玩家", "准备发放", "确认发放", "查看审计"};
                for (int i = 0; i < permissions.length; i++) {
                    String id = "gm_permission_" + i;
                    addMenu(id, "gm_console", labels[i], null, null, 2, permissions[i]);
                    db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", "gm_admin_perm_" + i, roleId, id);
                }
                db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES('gm_admin_console',?,'gm_console')", roleId);
                if (db.queryForObject("SELECT COUNT(*) FROM sys_user_role WHERE user_id=? AND role_id=?", Integer.class, userId, roleId) == 0)
                    db.update("INSERT INTO sys_user_role(id,user_id,role_id,tenant_id) VALUES('gm_admin_role',?,?,0)", userId, roleId);
            });
        }
        if (db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=2", Integer.class) == 0) {
            tx.executeWithoutResult(status -> {
                // 首次安装统一使用原生首页配置；独立版本号让已完成账号初始化的库也能补齐首页。
                db.update("UPDATE sys_role_index SET status='0'");
                db.update("INSERT INTO sys_role_index(id,role_code,url,component,is_route,priority,status,relation_type) VALUES('gm_default_index','DEF_INDEX_ALL','/gm/console','gm/console/index',1,-100,'1','DEFAULT')");
                db.update("INSERT INTO gm_install(version,installed_at) VALUES(2,CURRENT_TIMESTAMP)");
            });
        }
        ready = true;
    }
    private void addMenu(String id, String parent, String name, String url, String component, int type, String permission) {
        db.update("INSERT INTO sys_permission(id,parent_id,name,url,component,is_route,menu_type,perms,perms_type,sort_no,is_leaf,hidden,del_flag,status,internal_or_external) VALUES(?,?,?,?,?,1,?,?, '1',-100,1,0,0,'1',0)",
                id, parent, name, url, component, type, permission);
    }
    @Override public void addInterceptors(InterceptorRegistry registry) {
        registry.addInterceptor(new HandlerInterceptor() {
            @Override public boolean preHandle(HttpServletRequest request, HttpServletResponse response, Object handler) throws Exception {
                String path = request.getServletPath();
                if (!ready) return reject(response, 503);
                if (OPEN.contains(path) || path.startsWith("/sys/randomImage/")) return true;
                if (!SecurityUtils.getSubject().isAuthenticated()) return reject(response, 401);
                if (path.startsWith("/gm/") || SESSION_READ.contains(path)) return true;
                // 只开放原生账号/角色管理给管理员，游戏运营角色不能访问平台的演示、报表或任意数据源。
                if (path.startsWith("/sys/") && SecurityUtils.getSubject().hasRole("admin") && !blocked(path)) return true;
                return reject(response, 403);
            }
        });
    }
    private static boolean blocked(String path) {
        return List.of("/sys/user/register", "/sys/phoneLogin", "/sys/thirdLogin", "/sys/cas/", "/sys/mLogin", "/sys/sms", "/sys/user/phoneVerification", "/sys/user/passwordChange", "/sys/getLoginQrcode", "/sys/getQrcodeToken", "/sys/checkAuth").stream().anyMatch(path::startsWith);
    }
    private static boolean reject(HttpServletResponse response, int status) throws java.io.IOException {
        response.setStatus(status); response.setContentType("application/json;charset=UTF-8");
        response.getWriter().write("{\"success\":false,\"code\":" + status + ",\"message\":\"GM访问未获授权或服务尚未就绪\"}");
        return false;
    }
}
