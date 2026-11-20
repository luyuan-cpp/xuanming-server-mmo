package com.game.gateway.dto;

/**
 * {@code POST /api/account/delete/cancel} 的请求体(docs/design/account-deletion.md §4.4、§5.2):冷静期内 /api/login
 * 签发的 access token,login 用它解出账号。{@code zone_id} 只用来选路,同 {@link AccountDeletionRequest}。
 */
public class CancelAccountDeletionRequest {

    private long zoneId;
    private String accessToken;

    public long getZoneId() { return zoneId; }
    public void setZoneId(long zoneId) { this.zoneId = zoneId; }
    public String getAccessToken() { return accessToken; }
    public void setAccessToken(String accessToken) { this.accessToken = accessToken; }
}
