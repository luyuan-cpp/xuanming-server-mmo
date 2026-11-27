package org.jeecg.modules.gm;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.lang.reflect.Method;
import java.security.SecureRandom;
import java.time.Clock;
import java.util.Base64;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.ApplicationContext;
import org.springframework.context.annotation.Lazy;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.server.ResponseStatusException;

/**
 * OIDC 登录的后台一半(docs/design/admin-identity-and-dual-control.md §5.2):发起授权、换码之后落账号与角色、签发后台会话。
 *
 * <p>三条规则都在这个类里:
 * <ul>
 *   <li>操作人标识就是身份提供方的主体,原样当后台用户名,不另建绑定表。绑定表能被本地管理员改绑,改绑后同一个人就能以两个账号出现,
 *       双人复核就挡不住自己批自己。</li>
 *   <li>角色每次登录按身份提供方的分组整体替换,本地不管角色。离职、调岗在身份提供方那边改,下次登录生效。</li>
 *   <li>会话到期不续。上游只在令牌自身过期(84 小时)时才续 Redis 里的会话键,所以把键的过期设成会话上限,它就是硬上限。</li>
 * </ul>
 */
@RestController
@RequestMapping("/gm/sso")
@Lazy(false) // 全局是懒加载;这里要在启动时就核对 OIDC 配置与上游会话接口,不等到第一个人登录才发现配错。
public class GmOidcLogin {
    private static final Logger LOG = LoggerFactory.getLogger(GmOidcLogin.class);
    /** 一次登录从发起授权到带着授权码回来,最多这么久。 */
    static final long ATTEMPT_TTL_MS = 300_000L;
    /** 发起授权不需要登录,每次落一行;超过这个数就先拒绝,到期的行在下一次发起时清掉。 */
    private static final int MAX_PENDING_ATTEMPTS = 10_000;
    private static final int MAX_CODE_CHARS = 2_048;

    /** 上游(JeecgBoot)会话的签发与权限缓存清除。生产实现按固定提交的签名用反射取上游对象,单测注入替身。 */
    interface UpstreamSessions {
        /** 以 secret 为签名密钥给 username 签发会话令牌,ttlSeconds 之后失效且不续期。 */
        String issue(String username, String secret, long ttlSeconds);

        /** 清掉这个账号缓存的角色与权限,让刚换好的角色立刻生效。 */
        void evictAuthorization(String userId);
    }

    private record Attempt(String nonce, String codeVerifier, long createdMs) {}
    private record Account(String id, String username, String secret, Set<String> roleCodes) {}

    private final JdbcTemplate db;
    private final TransactionTemplate tx;
    private final ObjectMapper json;
    private final GmOidcProvider provider;
    private final UpstreamSessions sessions;
    private final boolean oidc;
    private final Map<String, String> roleByGroup;
    private final long sessionSeconds;
    private final Clock clock;
    private final SecureRandom random = new SecureRandom();

    @Autowired
    public GmOidcLogin(JdbcTemplate db, PlatformTransactionManager transactions, ObjectMapper json, GmOidcProvider provider,
            ApplicationContext context, @Value("${gm.identity.mode:native}") String mode,
            @Value("${gm.oidc.role-mapping:}") String roleMapping, @Value("${gm.oidc.session-hours:12}") long sessionHours) {
        this(db, transactions, json, provider, isOidc(mode) ? reflectUpstream(context) : null, mode, roleMapping, sessionHours, Clock.systemUTC());
    }

    GmOidcLogin(JdbcTemplate db, PlatformTransactionManager transactions, ObjectMapper json, GmOidcProvider provider,
            UpstreamSessions sessions, String mode, String roleMapping, long sessionHours, Clock clock) {
        this.db = db; this.tx = new TransactionTemplate(transactions); this.json = json; this.provider = provider;
        this.sessions = sessions; this.oidc = isOidc(mode); this.clock = clock;
        this.roleByGroup = oidc ? parseRoleMapping(json, roleMapping) : Map.of();
        if (sessionHours < 1 || sessionHours > 24) throw new IllegalStateException("gm.oidc.session-hours must be between 1 and 24");
        this.sessionSeconds = sessionHours * 3_600L;
        if (oidc) provider.requireConfigured();
    }

    /** 生产模式(OIDC)为 true;本机开发的原生口令模式为 false。未知取值直接拒绝启动。 */
    static boolean isOidc(String mode) {
        if ("oidc".equals(mode)) return true;
        if ("native".equals(mode)) return false;
        throw new IllegalStateException("gm.identity.mode must be oidc or native");
    }

    public boolean oidc() { return oidc; }

    /** 登录页据此决定显示企业登录还是原生口令表单。 */
    @GetMapping("/config")
    public String config() { return ok(json.createObjectNode().put("mode", oidc ? "oidc" : "native")); }

    @GetMapping("/authorize")
    public String authorize() {
        requireOidc();
        long now = clock.millis();
        db.update("DELETE FROM gm_oidc_attempt WHERE created_ms<?", now - ATTEMPT_TTL_MS);
        Integer pending = db.queryForObject("SELECT COUNT(*) FROM gm_oidc_attempt", Integer.class);
        if (pending != null && pending >= MAX_PENDING_ATTEMPTS)
            throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE, "登录请求过多，请稍后再试");
        String state = token();
        String nonce = token();
        String codeVerifier = token();
        db.update("INSERT INTO gm_oidc_attempt(state,nonce,code_verifier,created_ms) VALUES(?,?,?,?)", state, nonce, codeVerifier, now);
        return ok(json.createObjectNode().put("url", provider.authorizationUrl(state, nonce, GmOidcProvider.codeChallenge(codeVerifier))));
    }

    @PostMapping("/token")
    public String token(@RequestBody String body) {
        requireOidc();
        JsonNode input;
        try { input = json.readTree(body); } catch (java.io.IOException invalid) { input = null; }
        String code = input == null ? "" : input.path("code").asText("");
        String state = input == null ? "" : input.path("state").asText("");
        if (code.isEmpty() || code.length() > MAX_CODE_CHARS || state.isEmpty() || state.length() > 64)
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "登录回调参数无效");
        Attempt attempt = consume(state);
        GmOidcProvider.Identity identity = provider.redeem(code, attempt.codeVerifier(), attempt.nonce());
        Account account = tx.execute(status -> admit(identity));
        // 提交之后再清缓存:先清的话,并发请求可能在提交前把旧角色又缓存回去。
        sessions.evictAuthorization(account.id());
        String session = sessions.issue(account.username(), account.secret(), sessionSeconds);
        LOG.info("GM OIDC login accepted subject={} roles={} session_seconds={}", account.username(), account.roleCodes(), sessionSeconds);
        return ok(json.createObjectNode().put("token", session));
    }

    /** 取出即删:同一个 state 只能换一次。两个并发请求里只有删到行的那一个继续。 */
    private Attempt consume(String state) {
        List<Attempt> found = db.query("SELECT nonce,code_verifier,created_ms FROM gm_oidc_attempt WHERE state=?",
                (row, index) -> new Attempt(row.getString("nonce"), row.getString("code_verifier"), row.getLong("created_ms")), state);
        if (found.isEmpty() || db.update("DELETE FROM gm_oidc_attempt WHERE state=?", state) != 1
                || clock.millis() - found.get(0).createdMs() > ATTEMPT_TTL_MS)
            throw new ResponseStatusException(HttpStatus.UNAUTHORIZED, "登录已过期或已使用，请重新登录");
        return found.get(0);
    }

    /** 在一个事务里:按主体找到或新建账号,核对它可用,再把角色换成身份提供方分组映射出来的那一组。 */
    private Account admit(GmOidcProvider.Identity identity) {
        Set<String> roleCodes = new TreeSet<>();
        for (String group : identity.groups()) {
            String role = roleByGroup.get(group);
            if (role != null) roleCodes.add(role);
        }
        if (roleCodes.isEmpty()) throw denied(identity.subject(), "no identity provider group maps to a GM role");
        String subject = identity.subject();
        // 先不加锁地看一眼:对不存在的行做加锁读会留下间隙锁,两个副本同时为同一个新主体建号时会互相死锁。
        if (db.queryForObject("SELECT COUNT(*) FROM sys_user WHERE username=?", Integer.class, subject) == 0) {
            try {
                // 新账号的口令列放一段随机数:它只当会话令牌的签名密钥用,没有人知道它,原生口令登录也登不进来。
                db.update("INSERT INTO sys_user(id,username,realname,password,salt,status,del_flag,create_by,create_time) "
                        + "VALUES(?,?,?,?,'',1,0,'gm-oidc',CURRENT_TIMESTAMP)", hex(16), subject, identity.displayName(), hex(32));
            } catch (DuplicateKeyException concurrent) {
                // 另一个副本刚为同一主体建好账号,下面按已存在处理。
            }
        }
        // 锁住账号行,让同一个人在两个副本上同时登录时角色替换串行进行。可用性在 SQL 里比:驱动可能把 tinyint(1) 读成布尔值。
        List<Map<String, Object>> rows = db.queryForList("SELECT id,username,password,"
                + "CASE WHEN status=1 AND del_flag=0 THEN 1 ELSE 0 END AS usable FROM sys_user WHERE username=? FOR UPDATE", subject);
        if (rows.size() != 1) throw denied(subject, "expected exactly one account for this subject but found " + rows.size());
        Map<String, Object> user = rows.get(0);
        // 账号表的用户名列不分大小写;库里按大小写折叠命中的另一个主体不是同一个人。
        if (!subject.equals(user.get("username"))) throw denied(subject, "subject differs only by case from an existing account");
        if (!(user.get("usable") instanceof Number usable) || usable.intValue() != 1)
            throw denied(subject, "account is frozen or deleted in the GM console");
        String userId = (String) user.get("id");
        String secret = (String) user.get("password");
        if (secret == null || secret.length() < 32) {
            secret = hex(32);
            db.update("UPDATE sys_user SET password=?,salt='' WHERE id=?", secret, userId);
        }
        db.update("UPDATE sys_user SET realname=? WHERE id=?", identity.displayName(), userId);
        String placeholders = String.join(",", roleCodes.stream().map(code -> "?").toList());
        List<String> roleIds = db.queryForList("SELECT id FROM sys_role WHERE role_code IN (" + placeholders + ")", String.class, roleCodes.toArray());
        if (roleIds.size() != roleCodes.size()) throw denied(subject, "gm.oidc.role-mapping names a role that does not exist: " + roleCodes);
        db.update("DELETE FROM sys_user_role WHERE user_id=?", userId);
        for (String roleId : roleIds)
            db.update("INSERT INTO sys_user_role(id,user_id,role_id,tenant_id) VALUES(?,?,?,0)", hex(16), userId, roleId);
        return new Account(userId, subject, secret, roleCodes);
    }

    private static ResponseStatusException denied(String subject, String reason) {
        LOG.warn("GM OIDC login denied subject={} reason=\"{}\"", subject, reason);
        return new ResponseStatusException(HttpStatus.FORBIDDEN, "该账号没有可用的 GM 权限，请联系管理员");
    }

    private void requireOidc() {
        if (!oidc) throw new ResponseStatusException(HttpStatus.NOT_FOUND, "当前为本机口令模式，未启用企业登录");
    }

    /** 32 字节随机数的 URL 安全编码,43 个字符:state、nonce、PKCE 校验串都用它。 */
    private String token() {
        byte[] bytes = new byte[32];
        random.nextBytes(bytes);
        return Base64.getUrlEncoder().withoutPadding().encodeToString(bytes);
    }

    private String hex(int byteCount) {
        byte[] bytes = new byte[byteCount];
        random.nextBytes(bytes);
        return HexFormat.of().formatHex(bytes);
    }

    /** 映射是一个 JSON 对象:身份提供方的分组名 → 后台角色编码。OIDC 模式下不能为空,否则没有人登得进来。 */
    private static Map<String, String> parseRoleMapping(ObjectMapper json, String raw) {
        Map<String, String> mapping;
        try {
            mapping = json.readValue(raw == null || raw.isBlank() ? "{}" : raw, new TypeReference<Map<String, String>>() {});
        } catch (java.io.IOException invalid) {
            throw new IllegalStateException("gm.oidc.role-mapping must be a JSON object of identity provider group to GM role code");
        }
        if (mapping.isEmpty()) throw new IllegalStateException("gm.oidc.role-mapping must map at least one identity provider group to a GM role code");
        for (Map.Entry<String, String> entry : mapping.entrySet()) {
            if (entry.getKey().isBlank() || entry.getValue() == null || !entry.getValue().matches("[A-Za-z0-9_]{1,64}"))
                throw new IllegalStateException("gm.oidc.role-mapping has a blank group or an invalid role code");
        }
        return Map.copyOf(mapping);
    }

    /** 上游版本是钉死的(prepare.py 核对提交号);签名对不上就在启动时失败,不等到第一个人登录。 */
    private static UpstreamSessions reflectUpstream(ApplicationContext context) {
        try {
            Class<?> constants = Class.forName("org.jeecg.common.constant.CommonConstant");
            String sessionKeyPrefix = (String) constants.getField("PREFIX_USER_TOKEN").get(null);
            String authorizationKeyPrefix = (String) constants.getField("PREFIX_USER_SHIRO_CACHE").get(null);
            String clientType = (String) constants.getField("CLIENT_TYPE_PC").get(null);
            Method sign = Class.forName("org.jeecg.common.system.util.JwtUtil").getMethod("sign", String.class, String.class, String.class);
            Object redis = context.getBean("redisUtil");
            Method setWithExpiry = redis.getClass().getMethod("set", String.class, Object.class, long.class);
            Method delete = redis.getClass().getMethod("del", String[].class);
            return new UpstreamSessions() {
                @Override public String issue(String username, String secret, long ttlSeconds) {
                    try {
                        String session = (String) sign.invoke(null, username, secret, clientType);
                        // 上游的写入失败时回 false 而不是抛异常;没写进去的令牌发出去也用不了。
                        if (!Boolean.TRUE.equals(setWithExpiry.invoke(redis, sessionKeyPrefix + session, session, ttlSeconds)))
                            throw new IllegalStateException("GM session store rejected the new session");
                        return session;
                    } catch (ReflectiveOperationException failed) {
                        throw new IllegalStateException("GM session issue failed", failed);
                    }
                }

                @Override public void evictAuthorization(String userId) {
                    try {
                        delete.invoke(redis, (Object) new String[] {authorizationKeyPrefix + userId});
                    } catch (ReflectiveOperationException failed) {
                        throw new IllegalStateException("GM authorization cache eviction failed", failed);
                    }
                }
            };
        } catch (ReflectiveOperationException | RuntimeException changed) {
            throw new IllegalStateException("GM OIDC mode cannot reach the pinned JeecgBoot session API", changed);
        }
    }

    private String ok(JsonNode result) {
        return json.createObjectNode().put("success", true).put("code", 200).put("message", "").set("result", result).toString();
    }

    @ExceptionHandler(GmOidcProvider.Failure.class)
    public ResponseEntity<String> identityProvider(GmOidcProvider.Failure failure) {
        LOG.warn("GM OIDC login failed code={} detail=\"{}\"", failure.getCode(), failure.getMessage());
        boolean unavailable = failure.getCode().equals("IDP_UNAVAILABLE") || failure.getCode().equals("IDP_MISCONFIGURED");
        return unavailable ? error(502, "身份提供方暂时不可用，请稍后再试") : error(401, "登录未通过身份校验，请重新登录");
    }

    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<String> rejected(ResponseStatusException rejected) {
        return error(rejected.getStatusCode().value(), rejected.getReason());
    }

    private ResponseEntity<String> error(int code, String message) {
        return ResponseEntity.status(code).body(json.createObjectNode().put("success", false).put("code", code).put("message", message).toString());
    }
}
