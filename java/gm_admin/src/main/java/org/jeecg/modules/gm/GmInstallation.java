package org.jeecg.modules.gm;

import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import java.security.SecureRandom;
import java.sql.Connection;
import java.sql.SQLException;
import java.util.HexFormat;
import java.util.List;
import java.util.Set;
import org.apache.shiro.SecurityUtils;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.ApplicationArguments;
import org.springframework.boot.ApplicationRunner;
import org.springframework.context.annotation.Configuration;
import org.springframework.context.annotation.Lazy;
import org.springframework.core.io.ClassPathResource;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.ConnectionCallback;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.init.ResourceDatabasePopulator;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;
import org.springframework.web.servlet.HandlerInterceptor;
import org.springframework.web.servlet.config.annotation.InterceptorRegistry;
import org.springframework.web.servlet.config.annotation.WebMvcConfigurer;

/**
 * 仅初始化独立 GM 库；关闭演示账号和无关公开入口，后续启动与多副本并发启动均不重置管理员。
 *
 * <p>访问保护按身份模式分两套（docs/design/admin-identity-and-dual-control.md §5.2）：
 * OIDC 模式下原生口令登录与全部原生 /sys 管理页一律拒绝，只留会话读取与登出；原生口令模式只给本机开发用，监听地址不是回环就拒绝启动。
 */
@Configuration
@Lazy(false)
public class GmInstallation implements ApplicationRunner, WebMvcConfigurer {
    // 上游 logout 为 Shiro anon，控制器自行验证签名并撤销令牌，不能要求此时已有认证 Subject。
    private static final Set<String> NATIVE_OPEN = Set.of("/sys/login", "/sys/checkCaptcha", "/sys/getEncryptedString", "/sys/logout", "/gm/sso/config");
    // 企业登录的三个入口本身就是登录过程，此时还没有认证主体；会话由 GmOidcLogin 在身份提供方校验通过后签发。
    private static final Set<String> OIDC_OPEN = Set.of("/sys/logout", "/gm/sso/config", "/gm/sso/authorize", "/gm/sso/token");
    private static final Set<String> SESSION_READ = Set.of("/sys/user/getUserInfo", "/sys/permission/getUserPermissionByToken", "/sys/user/queryUserComponent",
            "/sys/permission/getPermCode", "/sys/dict/queryAllDictItems", "/sys/user/verifyIzDefaultPwd", "/sys/annountCement/getUnreadMessageCount");
    private final JdbcTemplate db;
    private final TransactionTemplate tx;
    private final String password;
    private final boolean oidc;
    private volatile boolean ready;

    public GmInstallation(JdbcTemplate db, PlatformTransactionManager transactions,
            @Value("${gm.bootstrap-password:}") String password, @Value("${gm.identity.mode:native}") String identityMode,
            @Value("${server.address:}") String listenAddress) {
        this.db = db; this.tx = new TransactionTemplate(transactions); this.password = password;
        this.oidc = GmOidcLogin.isOidc(identityMode);
        // 在构造期判，早于 Web 服务器开始监听：口令存在后台自己库里的模式不能出现在本机之外。
        if (!oidc && !isLoopback(listenAddress))
            throw new IllegalStateException("GM native password mode is only allowed when server.address is a loopback address; set gm.identity.mode=oidc");
    }

    private static boolean isLoopback(String address) {
        try {
            return address != null && !address.isBlank() && java.net.InetAddress.getByName(address.strip()).isLoopbackAddress();
        } catch (java.net.UnknownHostException unresolvable) {
            return false;
        }
    }

    @Override public void run(ApplicationArguments args) throws Exception {
        if (!"gm_admin".equals(db.queryForObject("SELECT DATABASE()", String.class)))
            throw new IllegalStateException("GM installation requires its dedicated gm_admin database");
        new ResourceDatabasePopulator(new ClassPathResource("gm-schema.sql")).execute(db.getDataSource());
        ensureColumn("gm_command", "command_type", "ALTER TABLE gm_command ADD COLUMN command_type VARCHAR(16) NOT NULL DEFAULT 'MAIL'");
        // 双人复核：第一次放行时写入批准人与批准时刻，之后不再改（GmCommandJournal）。
        ensureColumn("gm_command", "approver_name", "ALTER TABLE gm_command ADD COLUMN approver_name VARBINARY(512) NULL");
        ensureColumn("gm_command", "approved_ms", "ALTER TABLE gm_command ADD COLUMN approved_ms BIGINT NULL");
        db.execute("CREATE TABLE IF NOT EXISTS gm_install (version INTEGER PRIMARY KEY, installed_at TIMESTAMP NOT NULL)");
        if (!hasVersion(1)) {
            SecureRandom random = new SecureRandom();
            String bootstrap = password;
            if (bootstrap.length() < 20) {
                if (!oidc) throw new IllegalStateException("GM bootstrap password must be injected and at least 20 characters");
                // OIDC 模式下原生口令登录是关着的，初始管理员用不上口令：给它一段没人知道的随机数，不要求部署方再保管一份用不上的凭据。
                byte[] unknown = new byte[32]; random.nextBytes(unknown);
                bootstrap = HexFormat.of().formatHex(unknown);
            }
            byte[] saltBytes = new byte[4]; random.nextBytes(saltBytes);
            String salt = HexFormat.of().formatHex(saltBytes);
            // 复用上游登录算法，不维护第二套密码实现。
            String encoded = (String) Class.forName("org.jeecg.common.util.PasswordUtil")
                    .getMethod("encrypt", String.class, String.class, String.class).invoke(null, "admin", bootstrap, salt);
            if (encoded == null || encoded.isBlank()) throw new IllegalStateException("GM bootstrap password encoding failed");
            runMigration(1, () -> {
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
        runMigration(2, () -> {
            // 首次安装统一使用原生首页配置；独立版本号让已完成账号初始化的库也能补齐首页。
            db.update("UPDATE sys_role_index SET status='0'");
            db.update("INSERT INTO sys_role_index(id,role_code,url,component,is_route,priority,status,relation_type) VALUES('gm_default_index','DEF_INDEX_ALL','/gm/console','gm/console/index',1,-100,'1','DEFAULT')");
        });
        runMigration(3, () -> {
            String roleId = db.queryForObject("SELECT id FROM sys_role WHERE role_code='admin'", String.class);
            String[] permissions = {"gm:currency:add", "gm:currency:deduct", "gm:currency:block", "gm:currency:unblock", "gm:player:level", "gm:pet:grant", "gm:item:grant"};
            String[] labels = {"增加货币", "扣除货币", "封禁货币获取", "解除货币封禁", "设置等级", "发放宝宝", "发放物品"};
            for (int i = 0; i < permissions.length; i++) {
                String id = "gm_player_permission_" + i;
                addMenu(id, "gm_console", labels[i], null, null, 2, permissions[i]);
                db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", "gm_admin_player_" + i, roleId, id);
            }
        });
        runMigration(4, () -> {
            // 独立 GM 仅展示游戏管理和原生账号权限管理，不展示上游演示及开发平台菜单。
            // 只调整菜单展示；保留原生按钮权限和已分配的角色，接口仍由访问保护逐次检查。
            db.update("UPDATE sys_permission SET hidden=1 WHERE menu_type IN (0,1) AND (url IS NULL OR url NOT IN ('/gm/console','/isystem','/system/user','/system/role','/system/menu'))");
            // 原生授权树仅递归非叶子节点，GM 菜单必须展开其十二项按钮权限。
            db.update("UPDATE sys_permission SET is_leaf=0 WHERE id='gm_console'");
        });
        if (!hasVersion(5)) {
            // DDL 在 MySQL 中会隐式提交，先幂等建好查询索引，再以单版本事务落盘版本 5。
            ensureIndex("gm_command", "idx_gm_command_type_created", "command_type, created_ms, id");
            ensureIndex("gm_command", "idx_gm_command_type_player_created", "command_type, player_id, created_ms, id");
            ensureIndex("gm_command", "idx_gm_command_type_status_created", "command_type, status, created_ms, id");
            ensureIndex("gm_command_audit", "idx_gm_command_audit_command", "command_id, id");
            runMigration(5, () -> { });
        }
        runMigration(6, () -> {
            String roleId = db.queryForObject("SELECT id FROM sys_role WHERE role_code='admin'", String.class);
            String[] permissions = {"gm:system-mail:prepare", "gm:system-mail:send"};
            String[] labels = {"准备系统邮件", "确认系统邮件"};
            for (int i = 0; i < permissions.length; i++) {
                String id = "gm_sys_mail_permission_" + i;
                addMenu(id, "gm_console", labels[i], null, null, 2, permissions[i]);
                db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", "gm_admin_sys_mail_" + i, roleId, id);
            }
            db.update("UPDATE sys_permission SET is_leaf=0 WHERE id='gm_console'");
        });
        runMigration(7, () -> {
            // OIDC 模式下原生角色管理页是关着的，角色只能由迁移建好；身份提供方的分组经 gm.oidc.role-mapping 映射到这些角色编码。
            // 发起与批准分成两个角色，同一个人可以两个都有：双人复核只看一条命令上的两个人是否不同。
            // 七项玩家 GM 各只有一个权限串，发起和放行共用，所以发起人与批准人都持有。
            List<String> read = List.of("gm_console", "gm_permission_0", "gm_permission_1", "gm_permission_4");
            List<String> playerCommands = List.of("gm_player_permission_0", "gm_player_permission_1", "gm_player_permission_2",
                    "gm_player_permission_3", "gm_player_permission_4", "gm_player_permission_5", "gm_player_permission_6");
            addRole("gm_role_operator", "gm_operator", "GM 发起人", read, playerCommands, List.of("gm_permission_2", "gm_sys_mail_permission_0"));
            addRole("gm_role_approver", "gm_approver", "GM 批准人", read, playerCommands, List.of("gm_permission_3", "gm_sys_mail_permission_1"));
            addRole("gm_role_auditor", "gm_auditor", "GM 审计", read, List.of(), List.of());
        });
        runMigration(8, () -> {
            // 交易人工处置(GmTradeController):查看、发起、批准三个权限串;发起与批准分给不同的标准角色。
            String roleId = db.queryForObject("SELECT id FROM sys_role WHERE role_code='admin'", String.class);
            String[] permissions = {GmTradeController.READ, GmTradeController.PREPARE, GmTradeController.APPROVE};
            String[] labels = {"查看交易人工处置", "发起交易人工处置", "批准交易人工处置"};
            for (int i = 0; i < permissions.length; i++) {
                String id = "gm_trade_permission_" + i;
                addMenu(id, "gm_console", labels[i], null, null, 2, permissions[i]);
                db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", "gm_admin_trade_" + i, roleId, id);
            }
            grant("gm_role_operator", "trade", List.of("gm_trade_permission_0", "gm_trade_permission_1"));
            grant("gm_role_approver", "trade", List.of("gm_trade_permission_0", "gm_trade_permission_2"));
            grant("gm_role_auditor", "trade", List.of("gm_trade_permission_0"));
            db.update("UPDATE sys_permission SET is_leaf=0 WHERE id='gm_console'");
        });
        runMigration(9, () -> opsPermissions(List.of("mall"), "商城"));
        ready = true;
    }

    /**
     * 运营处置(GmOpsController)的下游各有查看、发起、批准三个权限串 gm:&lt;target&gt;:read / prepare / approve;
     * 写法与迁移 8 相同:建菜单行、授给 admin,再按发起 / 批准 / 只读分给三个标准角色。新接入的下游另开一次迁移调它。
     */
    private void opsPermissions(List<String> targets, String label) {
        String roleId = db.queryForObject("SELECT id FROM sys_role WHERE role_code='admin'", String.class);
        String[] actions = {"read", "prepare", "approve"};
        String[] verbs = {"查看", "发起", "批准"};
        for (String target : targets) {
            for (int i = 0; i < actions.length; i++) {
                String id = "gm_ops_" + target + "_permission_" + i;
                addMenu(id, "gm_console", verbs[i] + label + "运营处置", null, null, 2, GmOpsController.permission(target, actions[i]));
                db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", "gm_admin_ops_" + target + "_" + i, roleId, id);
            }
            String prefix = "gm_ops_" + target + "_permission_";
            grant("gm_role_operator", "ops_" + target, List.of(prefix + 0, prefix + 1));
            grant("gm_role_approver", "ops_" + target, List.of(prefix + 0, prefix + 2));
            grant("gm_role_auditor", "ops_" + target, List.of(prefix + 0));
        }
        db.update("UPDATE sys_permission SET is_leaf=0 WHERE id='gm_console'");
    }

    /** 给迁移 7 建好的标准角色追加权限;tag 让各次迁移写入的关系行 id 互不相撞。 */
    private void grant(String roleId, String tag, List<String> permissions) {
        for (int i = 0; i < permissions.size(); i++) {
            db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", roleId + "_" + tag + "_" + i, roleId, permissions.get(i));
        }
    }

    @SafeVarargs
    private void addRole(String id, String code, String name, List<String>... permissionGroups) {
        db.update("INSERT INTO sys_role(id,role_name,role_code,description,create_by,create_time,tenant_id) VALUES(?,?,?,?,'gm-install',CURRENT_TIMESTAMP,0)",
                id, name, code, name);
        int index = 0;
        for (List<String> group : permissionGroups) {
            for (String permission : group) {
                db.update("INSERT INTO sys_role_permission(id,role_id,permission_id) VALUES(?,?,?)", id + "_" + index++, id, permission);
            }
        }
    }

    private boolean hasVersion(int version) {
        Integer count = db.queryForObject("SELECT COUNT(*) FROM gm_install WHERE version=?", Integer.class, version);
        return count != null && count > 0;
    }

    /** 先在事务首行占位版本号：多副本并发启动时靠主键锁串行化，避免业务表撞键；若本事务内业务失败则整笔回滚。 */
    private void runMigration(int version, Runnable work) {
        if (hasVersion(version)) return;
        try {
            tx.executeWithoutResult(status -> {
                db.update("INSERT INTO gm_install(version,installed_at) VALUES(?,CURRENT_TIMESTAMP)", version);
                work.run();
            });
        } catch (DuplicateKeyException error) {
            if (!hasVersion(version)) throw error;
        }
    }

    private void ensureColumn(String table, String column, String alterSql) {
        db.execute((ConnectionCallback<Void>) connection -> {
            if (columnExists(connection, table, column)) return null;
            try (var statement = connection.createStatement()) {
                statement.execute(alterSql);
            } catch (SQLException error) {
                if (!columnExists(connection, table, column)) throw error;
            }
            return null;
        });
    }

    private static boolean columnExists(Connection connection, String table, String column) throws SQLException {
        try (var columns = connection.getMetaData().getColumns(connection.getCatalog(), null, table, column)) {
            return columns.next();
        }
    }

    private void ensureIndex(String table, String indexName, String columns) {
        db.execute((ConnectionCallback<Void>) connection -> {
            if (indexExists(connection, table, indexName)) return null;
            try (var statement = connection.createStatement()) {
                statement.execute("CREATE INDEX " + indexName + " ON " + table + " (" + columns + ")");
            } catch (SQLException error) {
                if (!indexExists(connection, table, indexName)) throw error;
            }
            return null;
        });
    }

    private static boolean indexExists(Connection connection, String table, String indexName) throws SQLException {
        try (var indexes = connection.getMetaData().getIndexInfo(connection.getCatalog(), null, table, false, false)) {
            while (indexes.next()) {
                String existing = indexes.getString("INDEX_NAME");
                if (existing != null && existing.equalsIgnoreCase(indexName)) return true;
            }
            return false;
        }
    }

    private void addMenu(String id, String parent, String name, String url, String component, int type, String permission) {
        db.update("INSERT INTO sys_permission(id,parent_id,name,url,component,is_route,menu_type,perms,perms_type,sort_no,is_leaf,hidden,del_flag,status,internal_or_external) VALUES(?,?,?,?,?,1,?,?, '1',-100,1,0,0,'1',0)",
                id, parent, name, url, component, type, permission);
    }

    @Override public void addInterceptors(InterceptorRegistry registry) {
        registry.addInterceptor(new HandlerInterceptor() {
            @Override public boolean preHandle(HttpServletRequest request, HttpServletResponse response, Object handler) throws Exception {
                String path = request.getServletPath();
                // K8s 存活探针在启动迁移期间即可放行；就绪探针必须等全部版本迁移落盘后才返回就绪。
                if ("GET".equalsIgnoreCase(request.getMethod()) && "/actuator/health/liveness".equals(path)) return true;
                if (!ready) return reject(response, 503);
                if ("GET".equalsIgnoreCase(request.getMethod()) && "/actuator/health/readiness".equals(path)) return true;
                boolean open = oidc ? OIDC_OPEN.contains(path) : (NATIVE_OPEN.contains(path) || path.startsWith("/sys/randomImage/"));
                if (open) return true;
                if (!SecurityUtils.getSubject().isAuthenticated()) return reject(response, 401);
                if (path.startsWith("/gm/") || SESSION_READ.contains(path)) return true;
                // OIDC 模式下角色来自身份提供方、权限由迁移写死，原生的账号/角色/菜单管理页没有用处，整体关闭。
                if (oidc) return reject(response, 403);
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
