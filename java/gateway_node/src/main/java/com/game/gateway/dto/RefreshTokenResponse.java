package com.game.gateway.dto;

import com.fasterxml.jackson.annotation.JsonInclude;

/**
 * Response for {@code POST /api/refresh-token}.
 *
 * <p>Shape mirrors {@link LoginResponse}'s token section; codes reuse the same
 * enum so clients can have a single error-handling path:
 * <ul>
 *   <li>0    — success, new access/refresh token pair issued</li>
 *   <li>401  — refresh token invalid/expired (client must redo OAuth)</li>
 *   <li>429  — IP rate limit exceeded (client backs off and retries later)</li>
 *   <li>500  — backend unavailable, or a login in-band tip that Tip.xlsx marks as fault (retry later)</li>
 * </ul>
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class RefreshTokenResponse {

    public static final int CODE_OK             = 0;
    public static final int CODE_AUTH_REJECTED  = 401;
    public static final int CODE_RATE_LIMITED   = 429;
    public static final int CODE_INTERNAL       = 500;

    private int code;
    private String message;
    private String accessToken;
    private String refreshToken;
    private Long accessTokenExpire;
    private Long refreshTokenExpire;

    public static RefreshTokenResponse ok(String access, String refresh, long accessExpire, long refreshExpire) {
        RefreshTokenResponse r = new RefreshTokenResponse();
        r.code = CODE_OK;
        r.accessToken = access;
        r.refreshToken = refresh;
        r.accessTokenExpire = accessExpire;
        r.refreshTokenExpire = refreshExpire;
        return r;
    }

    public static RefreshTokenResponse error(int code, String message) {
        RefreshTokenResponse r = new RefreshTokenResponse();
        r.code = code;
        r.message = message;
        return r;
    }

    public int getCode() { return code; }
    public void setCode(int code) { this.code = code; }
    public String getMessage() { return message; }
    public void setMessage(String message) { this.message = message; }
    public String getAccessToken() { return accessToken; }
    public void setAccessToken(String accessToken) { this.accessToken = accessToken; }
    public String getRefreshToken() { return refreshToken; }
    public void setRefreshToken(String refreshToken) { this.refreshToken = refreshToken; }
    public Long getAccessTokenExpire() { return accessTokenExpire; }
    public void setAccessTokenExpire(Long accessTokenExpire) { this.accessTokenExpire = accessTokenExpire; }
    public Long getRefreshTokenExpire() { return refreshTokenExpire; }
    public void setRefreshTokenExpire(Long refreshTokenExpire) { this.refreshTokenExpire = refreshTokenExpire; }
}
