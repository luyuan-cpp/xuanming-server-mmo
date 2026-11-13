package com.game.gateway.dto;

/**
 * Inbound payload for {@code POST /api/login}.
 *
 * <p>Mirrors {@code login.LoginRequest} (proto/login/login.proto). Field naming
 * matches the existing snake_case Jackson convention configured globally on
 * the gateway.
 *
 * <p>For password auth: set {@code account} + {@code password}.
 * For third-party auth: set {@code authType} + {@code authToken}; {@code account}
 * is ignored.
 */
public class LoginRequest {

    /** Player-visible zone the user wants to log into. 0 = unspecified. */
    private long zoneId;

    /** Account name (used directly when authType is "" or "password"). */
    private String account;

    /** Password (used only when authType is "" or "password"). */
    private String password;

    /**
     * Auth provider:
     *   "" / "password"      — built-in password auth
     *   "wechat" / "qq"      — third-party OAuth (authToken is the SDK code/access_token)
     *   "wechat_minigame" / "douyin_minigame"
     *                        — mini game login (authToken is the wx.login / tt.login code)
     *   "satoken"            — Sa-Token bridge
     *   "access_token"       — silent re-login with a previously issued opaque token
     */
    private String authType;

    /** Third-party code/token; meaning depends on {@link #authType}. */
    private String authToken;

    /** Stable per-device id, used for risk-control & access-token binding. */
    private String deviceId;

    public long getZoneId() { return zoneId; }
    public void setZoneId(long zoneId) { this.zoneId = zoneId; }
    public String getAccount() { return account; }
    public void setAccount(String account) { this.account = account; }
    public String getPassword() { return password; }
    public void setPassword(String password) { this.password = password; }
    public String getAuthType() { return authType; }
    public void setAuthType(String authType) { this.authType = authType; }
    public String getAuthToken() { return authToken; }
    public void setAuthToken(String authToken) { this.authToken = authToken; }
    public String getDeviceId() { return deviceId; }
    public void setDeviceId(String deviceId) { this.deviceId = deviceId; }
}
