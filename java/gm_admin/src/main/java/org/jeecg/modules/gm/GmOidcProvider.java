package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.jwk.source.JWKSource;
import com.nimbusds.jose.jwk.source.JWKSourceBuilder;
import com.nimbusds.jose.proc.JWSVerificationKeySelector;
import com.nimbusds.jose.proc.SecurityContext;
import com.nimbusds.jose.util.DefaultResourceRetriever;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.proc.DefaultJWTClaimsVerifier;
import com.nimbusds.jwt.proc.DefaultJWTProcessor;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;
import java.util.stream.Collectors;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

/**
 * 标准 OIDC 授权码流程的协议端(docs/design/admin-identity-and-dual-control.md §5.2):发现文档、授权地址、换码、ID Token 校验。
 * 只说协议,不碰后台账号与会话;那一半在 {@link GmOidcLogin}。
 *
 * <p>身份提供方返回的令牌只到这里为止,不进浏览器。验签与 JWKS 拉取交给 nimbus-jose-jwt(自带缓存、换钥重取与限速),
 * 其余是普通的 HTTP 调用,用 JDK 自带客户端。
 */
@Component
public class GmOidcProvider {
    private static final Logger LOG = LoggerFactory.getLogger(GmOidcProvider.class);
    /** 只认非对称签名。对称算法(HS*)会让持有客户端密钥的任何一方都能伪造 ID Token,none 则等于不验。 */
    private static final Set<JWSAlgorithm> SIGNATURE_ALGORITHMS = Set.of(
            JWSAlgorithm.RS256, JWSAlgorithm.RS384, JWSAlgorithm.RS512,
            JWSAlgorithm.PS256, JWSAlgorithm.PS384, JWSAlgorithm.PS512,
            JWSAlgorithm.ES256, JWSAlgorithm.ES384, JWSAlgorithm.ES512);
    /** 操作人标识会原样当后台用户名,并下传给游戏服务写进留痕,所以只收这一小撮字符。 */
    static final Pattern SUBJECT = Pattern.compile("[A-Za-z0-9._@-]{1,64}");
    private static final int MAX_RESPONSE_BYTES = 262_144;
    private static final int MAX_DISPLAY_NAME_CHARS = 100;

    /** 校验通过的一次登录:稳定的主体标识、仅供显示的姓名、身份提供方给的分组。 */
    public record Identity(String subject, String displayName, Set<String> groups) {}

    /** 登录没走通。code 是给调用方分类用的短码,message 只给日志看,不含任何令牌或密钥。 */
    public static final class Failure extends RuntimeException {
        private final String code;
        Failure(String code, String message) { super(message); this.code = code; }
        Failure(String code, String message, Throwable cause) { super(message, cause); this.code = code; }
        public String getCode() { return code; }
    }

    private record Endpoints(URI authorization, URI token, DefaultJWTProcessor<SecurityContext> idTokens, boolean secretInBody) {}

    private final String issuer;
    private final String clientId;
    private final String clientSecret;
    private final String redirectUri;
    private final String scope;
    private final String subjectClaim;
    private final String groupsClaim;
    private final String nameClaim;
    private final HttpClient http;
    private final ObjectMapper json;
    private volatile Endpoints endpoints;

    @Autowired
    public GmOidcProvider(@Value("${gm.oidc.issuer:}") String issuer, @Value("${gm.oidc.client-id:}") String clientId,
            @Value("${gm.oidc.client-secret:}") String clientSecret, @Value("${gm.oidc.redirect-uri:}") String redirectUri,
            @Value("${gm.oidc.scope:openid profile}") String scope, @Value("${gm.oidc.subject-claim:sub}") String subjectClaim,
            @Value("${gm.oidc.groups-claim:groups}") String groupsClaim, @Value("${gm.oidc.name-claim:name}") String nameClaim,
            ObjectMapper json) {
        this(issuer, clientId, clientSecret, redirectUri, scope, subjectClaim, groupsClaim, nameClaim, json,
                HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(5)).followRedirects(HttpClient.Redirect.NEVER).build());
    }

    GmOidcProvider(String issuer, String clientId, String clientSecret, String redirectUri, String scope, String subjectClaim,
            String groupsClaim, String nameClaim, ObjectMapper json, HttpClient http) {
        this.issuer = issuer.strip(); this.clientId = clientId.strip(); this.clientSecret = clientSecret.strip();
        this.redirectUri = redirectUri.strip(); this.scope = scope.strip(); this.subjectClaim = subjectClaim.strip();
        this.groupsClaim = groupsClaim.strip(); this.nameClaim = nameClaim.strip(); this.json = json; this.http = http;
    }

    /** 四项必填配置都在,且发行方与回调地址都是 HTTPS(或本机回环)。OIDC 模式启动时调用,配置不全就不启动。 */
    public void requireConfigured() {
        if (issuer.isEmpty() || clientId.isEmpty() || clientSecret.isEmpty() || redirectUri.isEmpty())
            throw new IllegalStateException("GM OIDC mode requires gm.oidc.issuer, client-id, client-secret and redirect-uri");
        if (!Set.of(scope.split("\\s+")).contains("openid"))
            throw new IllegalStateException("GM OIDC scope must contain openid");
        if (subjectClaim.isEmpty() || groupsClaim.isEmpty() || nameClaim.isEmpty())
            throw new IllegalStateException("GM OIDC claim names must not be blank");
        requireSecure(URI.create(issuer), "issuer");
        requireSecure(URI.create(redirectUri), "redirect-uri");
    }

    /** 浏览器要去的授权地址。codeChallenge 是 PKCE 校验串的 S256 摘要,由 {@link #codeChallenge} 算出。 */
    public String authorizationUrl(String state, String nonce, String codeChallenge) {
        URI authorization = endpoints().authorization();
        String separator = authorization.getRawQuery() == null ? "?" : "&";
        return authorization + separator + form(Map.of("response_type", "code", "client_id", clientId, "redirect_uri", redirectUri,
                "scope", scope, "state", state, "nonce", nonce, "code_challenge", codeChallenge, "code_challenge_method", "S256"));
    }

    /** 用授权码换 ID Token 并校验;expectedNonce 是发起授权时存下的那一个。 */
    public Identity redeem(String code, String codeVerifier, String expectedNonce) {
        Endpoints current = endpoints();
        Map<String, String> fields = new LinkedHashMap<>(Map.of("grant_type", "authorization_code", "code", code,
                "redirect_uri", redirectUri, "code_verifier", codeVerifier));
        HttpRequest.Builder request = HttpRequest.newBuilder(current.token()).timeout(Duration.ofSeconds(10))
                .header("Content-Type", "application/x-www-form-urlencoded").header("Accept", "application/json");
        if (current.secretInBody()) {
            fields.put("client_id", clientId); fields.put("client_secret", clientSecret);
        } else {
            String basic = encode(clientId) + ":" + encode(clientSecret);
            request.header("Authorization", "Basic " + Base64.getEncoder().encodeToString(basic.getBytes(StandardCharsets.UTF_8)));
        }
        JsonNode reply = fetch(request.POST(HttpRequest.BodyPublishers.ofString(form(fields))).build(), "token endpoint");
        String idToken = reply.path("id_token").asText("");
        if (idToken.isEmpty()) throw new Failure("ID_TOKEN_REJECTED", "token endpoint returned no id_token");
        return verify(idToken, expectedNonce);
    }

    /** 包内可见是为了单测能直接喂一枚 ID Token;生产只经 {@link #redeem} 进来。 */
    Identity verify(String idToken, String expectedNonce) {
        JWTClaimsSet claims;
        try {
            // 签名、算法白名单、iss 逐字相等、aud 含本客户端、exp(容差 60 秒)都在这一步;none 与加密 JWT 由处理器缺省拒绝。
            claims = endpoints().idTokens().process(idToken, null);
        } catch (Exception rejected) {
            throw new Failure("ID_TOKEN_REJECTED", "id_token rejected: " + rejected.getMessage(), rejected);
        }
        try {
            String nonce = claims.getStringClaim("nonce");
            if (nonce == null || !MessageDigest.isEqual(nonce.getBytes(StandardCharsets.UTF_8), expectedNonce.getBytes(StandardCharsets.UTF_8)))
                throw new Failure("ID_TOKEN_REJECTED", "id_token nonce does not match this login attempt");
            // 一枚发给多个受众的令牌必须点名本客户端是被授权方,否则可能是别的应用的令牌被拿来冒用。
            String authorizedParty = claims.getStringClaim("azp");
            if ((claims.getAudience().size() > 1 && authorizedParty == null) || (authorizedParty != null && !authorizedParty.equals(clientId)))
                throw new Failure("ID_TOKEN_REJECTED", "id_token authorized party is not this client");
            String subject = claims.getStringClaim(subjectClaim);
            if (subject == null || !SUBJECT.matcher(subject).matches())
                throw new Failure("SUBJECT_REJECTED", "claim " + subjectClaim + " is missing or outside [A-Za-z0-9._@-]{1,64}; choose another stable claim via gm.oidc.subject-claim");
            return new Identity(subject, displayName(claims.getStringClaim(nameClaim), subject), groups(claims.getClaim(groupsClaim)));
        } catch (java.text.ParseException wrongType) {
            throw new Failure("ID_TOKEN_REJECTED", "id_token claim has an unexpected type: " + wrongType.getMessage());
        }
    }

    /** PKCE 的 S256 变换:BASE64URL(SHA-256(校验串))。 */
    public static String codeChallenge(String codeVerifier) {
        try {
            return Base64.getUrlEncoder().withoutPadding().encodeToString(
                    MessageDigest.getInstance("SHA-256").digest(codeVerifier.getBytes(StandardCharsets.US_ASCII)));
        } catch (NoSuchAlgorithmException impossible) {
            throw new IllegalStateException("SHA-256 is required by the JDK", impossible);
        }
    }

    private static String displayName(String raw, String fallback) {
        if (raw == null) return fallback;
        String cleaned = raw.codePoints().filter(point -> !Character.isISOControl(point)).limit(MAX_DISPLAY_NAME_CHARS)
                .collect(StringBuilder::new, StringBuilder::appendCodePoint, StringBuilder::append).toString().strip();
        return cleaned.isEmpty() ? fallback : cleaned;
    }

    private static Set<String> groups(Object claim) {
        if (claim instanceof String single) return Set.of(single);
        if (!(claim instanceof List<?> many)) return Set.of();
        return many.stream().filter(String.class::isInstance).map(String.class::cast).collect(Collectors.toCollection(LinkedHashSet::new));
    }

    /** 发现文档只在第一次用到时取,取成功后在进程内不变;身份提供方换端点要重启后台。取失败不缓存,下次再取。 */
    private Endpoints endpoints() {
        Endpoints loaded = endpoints;
        if (loaded != null) return loaded;
        synchronized (this) {
            if (endpoints == null) endpoints = discover();
            return endpoints;
        }
    }

    private Endpoints discover() {
        requireConfigured();
        String base = issuer.endsWith("/") ? issuer.substring(0, issuer.length() - 1) : issuer;
        JsonNode document = fetch(HttpRequest.newBuilder(URI.create(base + "/.well-known/openid-configuration"))
                .timeout(Duration.ofSeconds(10)).header("Accept", "application/json").GET().build(), "discovery document");
        // 发现文档自报的发行方必须与配置逐字相等,防止被指到另一家身份提供方。
        if (!issuer.equals(document.path("issuer").asText("")))
            throw new Failure("IDP_MISCONFIGURED", "discovery document issuer differs from gm.oidc.issuer");
        URI authorization;
        URI token;
        URI keys;
        try {
            authorization = requireSecure(URI.create(document.path("authorization_endpoint").asText("")), "authorization_endpoint");
            token = requireSecure(URI.create(document.path("token_endpoint").asText("")), "token_endpoint");
            keys = requireSecure(URI.create(document.path("jwks_uri").asText("")), "jwks_uri");
        } catch (IllegalArgumentException | IllegalStateException unusable) {
            throw new Failure("IDP_MISCONFIGURED", "discovery document endpoint rejected: " + unusable.getMessage(), unusable);
        }
        // 没声明就是 client_secret_basic(OIDC Discovery 的缺省);只有明说不支持 basic 时才把密钥放进表单。
        JsonNode methods = document.path("token_endpoint_auth_methods_supported");
        boolean secretInBody = methods.isArray() && !contains(methods, "client_secret_basic") && contains(methods, "client_secret_post");
        try {
            JWKSource<SecurityContext> source = JWKSourceBuilder
                    .create(keys.toURL(), new DefaultResourceRetriever(5_000, 5_000, MAX_RESPONSE_BYTES)).retrying(true).build();
            DefaultJWTProcessor<SecurityContext> processor = new DefaultJWTProcessor<>();
            processor.setJWSKeySelector(new JWSVerificationKeySelector<>(SIGNATURE_ALGORITHMS, source));
            processor.setJWTClaimsSetVerifier(new DefaultJWTClaimsVerifier<>(clientId,
                    new JWTClaimsSet.Builder().issuer(issuer).build(), Set.of("iat", "exp", "nonce")));
            LOG.info("GM OIDC discovery loaded issuer={} client_secret_in_body={}", issuer, secretInBody);
            return new Endpoints(authorization, token, processor, secretInBody);
        } catch (java.net.MalformedURLException malformed) {
            throw new Failure("IDP_MISCONFIGURED", "jwks_uri is not a valid URL", malformed);
        }
    }

    private static boolean contains(JsonNode array, String value) {
        for (JsonNode item : array) if (value.equals(item.asText())) return true;
        return false;
    }

    private JsonNode fetch(HttpRequest request, String what) {
        try {
            HttpResponse<InputStream> response = http.send(request, HttpResponse.BodyHandlers.ofInputStream());
            byte[] body;
            try (InputStream stream = response.body()) { body = stream.readNBytes(MAX_RESPONSE_BYTES + 1); }
            if (body.length > MAX_RESPONSE_BYTES) throw new Failure("IDP_UNAVAILABLE", what + " response is too large");
            // 换码被拒(授权码过期、重放、PKCE 不符)回 400;正文里的 error 只是短码,不含秘密,记下来方便排查。
            if (response.statusCode() != 200) {
                throw new Failure(response.statusCode() / 100 == 4 ? "IDP_REFUSED" : "IDP_UNAVAILABLE",
                        what + " answered HTTP " + response.statusCode() + errorCode(body));
            }
            JsonNode parsed = json.readTree(body);
            if (parsed == null || !parsed.isObject()) throw new Failure("IDP_UNAVAILABLE", what + " did not return a JSON object");
            return parsed;
        } catch (IOException unreachable) {
            throw new Failure("IDP_UNAVAILABLE", what + " is unreachable: " + unreachable.getClass().getSimpleName(), unreachable);
        } catch (InterruptedException cancelled) {
            Thread.currentThread().interrupt();
            throw new Failure("IDP_UNAVAILABLE", what + " request was interrupted", cancelled);
        }
    }

    private String errorCode(byte[] body) {
        try {
            String error = json.readTree(body).path("error").asText("");
            // 只取规范里的短码字符,身份提供方回什么都不会把别的内容带进日志。
            return error.matches("[A-Za-z0-9_]{1,64}") ? " error=" + error : "";
        } catch (IOException notJson) {
            return "";
        }
    }

    /** 只许 HTTPS;明文 HTTP 只放行本机回环,给本机的测试身份提供方用。 */
    private static URI requireSecure(URI uri, String what) {
        String host = uri.getHost();
        boolean loopback = host != null && Set.of("127.0.0.1", "localhost", "[::1]").contains(host);
        if (host == null || uri.getFragment() != null || !("https".equals(uri.getScheme()) || ("http".equals(uri.getScheme()) && loopback)))
            throw new IllegalStateException("GM OIDC " + what + " must be an https URL (plain http only on loopback) without a fragment");
        return uri;
    }

    private static String form(Map<String, String> fields) {
        return fields.entrySet().stream().map(field -> encode(field.getKey()) + "=" + encode(field.getValue())).collect(Collectors.joining("&"));
    }

    private static String encode(String value) { return URLEncoder.encode(value, StandardCharsets.UTF_8); }
}
