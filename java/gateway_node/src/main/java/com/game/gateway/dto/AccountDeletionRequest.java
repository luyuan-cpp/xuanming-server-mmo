package com.game.gateway.dto;

/**
 * {@code POST /api/account/delete} 的请求体(docs/design/account-deletion.md §4.2、§5.2)。
 *
 * <p>凭据字段与 {@link LoginRequest} 同义:注销要求重新认证,不收 access token。口令账号填 {@code account} + {@code password};
 * 第三方账号重新拉起渠道 SDK,填 {@code auth_type} + {@code auth_token};Apple 另带重新授权时拿到的 {@code authorization_code}。
 * {@code zone_id} 只用来选路(按区找 login,与 /api/login 同一个选路器):各区 login 共用同一地域的账号目录,任一区都能办。
 * JSON 名按网关全局的 SNAKE_CASE。
 */
public class AccountDeletionRequest {

    private long zoneId;
    private String account;
    private String password;
    private String authType;
    private String authToken;
    private String authorizationCode;

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
    public String getAuthorizationCode() { return authorizationCode; }
    public void setAuthorizationCode(String authorizationCode) { this.authorizationCode = authorizationCode; }
}
