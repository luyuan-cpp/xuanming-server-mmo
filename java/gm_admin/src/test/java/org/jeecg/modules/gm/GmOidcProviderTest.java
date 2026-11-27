package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.MACSigner;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.PlainJWT;
import com.nimbusds.jwt.SignedJWT;
import java.net.URI;
import java.net.URLDecoder;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Base64;
import java.util.Date;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.stream.Collectors;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import static org.junit.jupiter.api.Assertions.*;

class GmOidcProviderTest {
    private static final String NONCE = "nonce-for-this-login-attempt";
    private FakeIdentityProvider idp;
    private GmOidcProvider provider;

    @BeforeEach void setup() throws Exception {
        idp = new FakeIdentityProvider();
        provider = idp.provider();
    }

    @AfterEach void cleanup() { idp.close(); }

    @Test void redeemsTheCodeWithPkceAndClientAuthenticationAndReturnsTheVerifiedIdentity() {
        idp.nextIdToken = idp.sign(idp.claims("alice.w", NONCE).build());
        GmOidcProvider.Identity identity = provider.redeem("the-code", "the-verifier", NONCE);
        assertEquals("alice.w", identity.subject());
        assertEquals("测试员", identity.displayName());
        assertEquals(Set.of("gm-admins"), identity.groups());
        Map<String, String> form = form(idp.lastTokenRequestBody);
        assertEquals("authorization_code", form.get("grant_type"));
        assertEquals("the-code", form.get("code"));
        assertEquals("the-verifier", form.get("code_verifier"));
        assertEquals(FakeIdentityProvider.REDIRECT_URI, form.get("redirect_uri"));
        // 缺省把客户端密钥放在 Basic 头里,不进表单。
        assertFalse(form.containsKey("client_secret"));
        String basic = new String(Base64.getDecoder().decode(idp.lastTokenRequestAuthorization.substring("Basic ".length())), StandardCharsets.UTF_8);
        assertEquals(FakeIdentityProvider.CLIENT_ID + ":" + FakeIdentityProvider.CLIENT_SECRET, basic);
    }

    @Test void authorizationUrlCarriesStateNonceAndTheS256Challenge() {
        String url = provider.authorizationUrl("state-1", NONCE, GmOidcProvider.codeChallenge("the-verifier"));
        assertTrue(url.startsWith(idp.issuer() + "/authorize?"));
        Map<String, String> query = form(URI.create(url).getRawQuery());
        assertEquals("code", query.get("response_type"));
        assertEquals(FakeIdentityProvider.CLIENT_ID, query.get("client_id"));
        assertEquals(FakeIdentityProvider.REDIRECT_URI, query.get("redirect_uri"));
        assertEquals("openid profile", query.get("scope"));
        assertEquals("state-1", query.get("state"));
        assertEquals(NONCE, query.get("nonce"));
        assertEquals("S256", query.get("code_challenge_method"));
        assertEquals(GmOidcProvider.codeChallenge("the-verifier"), query.get("code_challenge"));
    }

    @Test void codeChallengeMatchesTheRfc7636Example() {
        assertEquals("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
                GmOidcProvider.codeChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"));
    }

    @Test void rejectsATokenSignedByAnotherKey() {
        String forged = FakeIdentityProvider.sign(idp.claims("alice", NONCE).build(), FakeIdentityProvider.newKey());
        assertRejected("ID_TOKEN_REJECTED", forged);
    }

    @Test void rejectsUnsignedAndSymmetricallySignedTokens() throws Exception {
        assertRejected("ID_TOKEN_REJECTED", new PlainJWT(idp.claims("alice", NONCE).build()).serialize());
        // 对称签名:知道客户端密钥的一方就能伪造,所以即使密钥对得上也不认。
        SignedJWT symmetric = new SignedJWT(new JWSHeader(JWSAlgorithm.HS256), idp.claims("alice", NONCE).build());
        symmetric.sign(new MACSigner(FakeIdentityProvider.CLIENT_SECRET.getBytes(StandardCharsets.UTF_8)));
        assertRejected("ID_TOKEN_REJECTED", symmetric.serialize());
    }

    @Test void rejectsWrongIssuerAudienceExpiryAndNonce() {
        long now = System.currentTimeMillis();
        assertRejected("ID_TOKEN_REJECTED", idp.sign(idp.claims("alice", NONCE).issuer("https://evil.example.test").build()));
        assertRejected("ID_TOKEN_REJECTED", idp.sign(idp.claims("alice", NONCE).audience("another-client").build()));
        assertRejected("ID_TOKEN_REJECTED", idp.sign(idp.claims("alice", NONCE).expirationTime(new Date(now - 600_000)).build()));
        assertRejected("ID_TOKEN_REJECTED", idp.sign(idp.claims("alice", "nonce-of-another-attempt").build()));
        assertRejected("ID_TOKEN_REJECTED", idp.sign(idp.claims("alice", NONCE).claim("nonce", null).build()));
    }

    @Test void aTokenForSeveralAudiencesMustNameThisClientAsTheAuthorizedParty() {
        JWTClaimsSet.Builder shared = idp.claims("alice", NONCE).audience(List.of(FakeIdentityProvider.CLIENT_ID, "another-client"));
        assertRejected("ID_TOKEN_REJECTED", idp.sign(shared.build()));
        assertRejected("ID_TOKEN_REJECTED", idp.sign(shared.claim("azp", "another-client").build()));
        assertEquals("alice", provider.verify(idp.sign(shared.claim("azp", FakeIdentityProvider.CLIENT_ID).build()), NONCE).subject());
    }

    @Test void theSubjectMustBeUsableAsAnOperatorIdentifier() {
        for (String subject : List.of("auth0|5f7c", "a b", "名字", "x".repeat(65), "")) {
            assertRejected("SUBJECT_REJECTED", idp.sign(idp.claims(subject, NONCE).build()));
        }
        // 主体不合用时可以换一个稳定的声明。
        GmOidcProvider byUsername = idp.provider("preferred_username");
        String token = idp.sign(idp.claims("auth0|5f7c", NONCE).claim("preferred_username", "alice@corp.example").build());
        assertEquals("alice@corp.example", byUsername.verify(token, NONCE).subject());
    }

    @Test void groupsMayBeAListASingleStringOrAbsentAndTheNameFallsBackToTheSubject() {
        assertEquals(Set.of("only"), provider.verify(idp.sign(idp.claims("alice", NONCE).claim("groups", "only").build()), NONCE).groups());
        GmOidcProvider.Identity bare = provider.verify(idp.sign(idp.claims("alice", NONCE).claim("groups", null).claim("name", null).build()), NONCE);
        assertEquals(Set.of(), bare.groups());
        assertEquals("alice", bare.displayName());
        // 姓名只用于显示:去掉控制字符并截到 100 个字符。
        String noisy = "张\u0000三\n" + "x".repeat(200);
        String shown = provider.verify(idp.sign(idp.claims("alice", NONCE).claim("name", noisy).build()), NONCE).displayName();
        assertEquals(100, shown.codePointCount(0, shown.length()));
        assertTrue(shown.startsWith("张三x"));
    }

    @Test void aRefusedCodeAndAMismatchedDiscoveryDocumentAreReportedDistinctly() {
        idp.nextIdToken = null;
        GmOidcProvider.Failure refused = assertThrows(GmOidcProvider.Failure.class, () -> provider.redeem("used-code", "v", NONCE));
        assertEquals("IDP_REFUSED", refused.getCode());
        assertTrue(refused.getMessage().contains("error=invalid_grant"));
        idp.advertisedIssuer = "https://evil.example.test";
        GmOidcProvider fresh = idp.provider();
        GmOidcProvider.Failure misdirected = assertThrows(GmOidcProvider.Failure.class, () -> fresh.redeem("code", "v", NONCE));
        assertEquals("IDP_MISCONFIGURED", misdirected.getCode());
    }

    @Test void configurationMustBeCompleteAndUseHttpsOutsideLoopback() {
        assertDoesNotThrow(provider::requireConfigured);
        assertThrows(IllegalStateException.class, () -> configured("", FakeIdentityProvider.REDIRECT_URI, "openid").requireConfigured());
        assertThrows(IllegalStateException.class, () -> configured("http://idp.example.test", FakeIdentityProvider.REDIRECT_URI, "openid").requireConfigured());
        assertThrows(IllegalStateException.class, () -> configured("https://idp.example.test", "http://gm.example.test/login", "openid").requireConfigured());
        assertThrows(IllegalStateException.class, () -> configured("https://idp.example.test", "https://gm.example.test/#/login", "openid").requireConfigured());
        assertThrows(IllegalStateException.class, () -> configured("https://idp.example.test", FakeIdentityProvider.REDIRECT_URI, "profile").requireConfigured());
        assertDoesNotThrow(() -> configured("https://idp.example.test", FakeIdentityProvider.REDIRECT_URI, "openid profile").requireConfigured());
    }

    private static GmOidcProvider configured(String issuer, String redirectUri, String scope) {
        return new GmOidcProvider(issuer, FakeIdentityProvider.CLIENT_ID, FakeIdentityProvider.CLIENT_SECRET, redirectUri, scope,
                "sub", "groups", "name", new ObjectMapper(), HttpClient.newHttpClient());
    }

    private void assertRejected(String code, String idToken) {
        GmOidcProvider.Failure failure = assertThrows(GmOidcProvider.Failure.class, () -> provider.verify(idToken, NONCE));
        assertEquals(code, failure.getCode());
    }

    private static Map<String, String> form(String encoded) {
        return Arrays.stream(encoded.split("&")).map(pair -> pair.split("=", 2))
                .collect(Collectors.toMap(pair -> decode(pair[0]), pair -> pair.length > 1 ? decode(pair[1]) : ""));
    }

    private static String decode(String value) { return URLDecoder.decode(value, StandardCharsets.UTF_8); }
}
