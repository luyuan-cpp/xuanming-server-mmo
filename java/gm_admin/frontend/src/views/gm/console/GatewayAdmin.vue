<template>
  <a-card title="网关管理（区服、白名单、公告、RPC 热关停）" class="gateway-card">
    <a-alert
      type="info"
      show-icon
      message="单人可做，网关与后台都按当前登录的管理员留痕。只能执行下面列出的操作；做之前先在区服列表里核对现状。"
    />
    <a-form layout="vertical" class="gateway-space">
      <a-form-item label="操作" required>
        <a-select v-model:value="form.template" :options="templates" placeholder="选择操作" @change="applyTemplate" />
      </a-form-item>
      <a-form-item label="方法与路径（把路径里的 {zoneId} 等换成实际值）" required>
        <a-input-group compact>
          <a-select v-model:value="form.method" :options="methods" style="width: 110px" />
          <a-input v-model:value="form.path" :maxlength="400" style="width: calc(100% - 110px)" />
        </a-input-group>
      </a-form-item>
      <a-form-item label="请求体（JSON，可空）">
        <a-textarea v-model:value="form.body" :rows="4" />
      </a-form-item>
    </a-form>
    <a-alert v-if="error" type="warning" show-icon :message="error" />
    <a-checkbox v-model:checked="confirmed" :disabled="sending" class="gateway-space">我已核对方法、路径与请求体，确认执行。</a-checkbox>
    <div class="gateway-space">
      <a-button type="primary" :loading="sending" :disabled="!confirmed || !form.path" @click="submit">执行</a-button>
    </div>
    <template v-if="result">
      <h4 class="gateway-space">网关回包（状态 {{ result.status }}）</h4>
      <pre class="gateway-json">{{ JSON.stringify(result.body ?? null, null, 2) }}</pre>
    </template>
  </a-card>
</template>

<script lang="ts" setup name="gm-gateway-admin">
  import { reactive, ref } from 'vue';
  import { defHttp } from '/@/utils/http/axios';

  const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;
  /** 与后端 GmController.GATEWAY_WRITES 白名单一一对应;页面只帮着填,判定在后端。 */
  const TEMPLATES = [
    { value: 'zone-maintenance', label: '区服进入维护', method: 'POST', path: '/admin/zones/{zoneId}/maintenance', body: '{"maintenanceMsg": ""}' },
    { value: 'zone-open', label: '区服开放', method: 'POST', path: '/admin/zones/{zoneId}/open', body: '' },
    { value: 'role-close', label: '关闭创角', method: 'POST', path: '/admin/zones/{zoneId}/create-role/close', body: '' },
    { value: 'role-open', label: '开放创角', method: 'POST', path: '/admin/zones/{zoneId}/create-role/open', body: '' },
    { value: 'role-capacity', label: '设置角色容量', method: 'PUT', path: '/admin/zones/{zoneId}/role-capacity', body: '{"roleCapacity": 0}' },
    { value: 'zone-create', label: '新建区服', method: 'POST', path: '/admin/zones', body: '{}' },
    { value: 'zone-update', label: '修改区服', method: 'PUT', path: '/admin/zones/{zoneId}', body: '{}' },
    { value: 'zone-delete', label: '删除区服', method: 'DELETE', path: '/admin/zones/{zoneId}', body: '' },
    { value: 'whitelist-add', label: '加白名单', method: 'POST', path: '/admin/whitelist', body: '{"zoneId": 0, "accountId": 0}' },
    { value: 'whitelist-remove', label: '移出白名单', method: 'DELETE', path: '/admin/whitelist/{zoneId}/{accountId}', body: '' },
    { value: 'announcement-add', label: '发布公告', method: 'POST', path: '/admin/announcements', body: '{}' },
    { value: 'announcement-remove', label: '删除公告', method: 'DELETE', path: '/admin/announcements/{id}', body: '' },
    { value: 'killswitch-put', label: '设置 RPC 热关停规则', method: 'PUT', path: '/admin/killswitch', body: '{"target": "", "deny": true, "reason": "", "code": 0}' },
    { value: 'killswitch-delete', label: '删除 RPC 热关停规则', method: 'DELETE', path: '/admin/killswitch?key={规则键，需 URL 编码}', body: '' },
  ];
  const templates = TEMPLATES.map((entry) => ({ value: entry.value, label: entry.label }));
  const methods = ['POST', 'PUT', 'DELETE'].map((value) => ({ value, label: value }));

  const form = reactive({ template: undefined as string | undefined, method: 'POST', path: '', body: '' });
  const confirmed = ref(false);
  const sending = ref(false);
  const error = ref('');
  const result = ref<{ status: number; body: unknown }>();

  function applyTemplate(value: string) {
    const entry = TEMPLATES.find((candidate) => candidate.value === value);
    if (!entry) return;
    Object.assign(form, { method: entry.method, path: entry.path, body: entry.body });
    confirmed.value = false;
    result.value = undefined;
  }

  async function submit() {
    error.value = '';
    let body: unknown;
    try {
      body = form.body.trim() ? JSON.parse(form.body) : undefined;
    } catch {
      error.value = '请求体不是合法的 JSON';
      return;
    }
    sending.value = true;
    try {
      result.value = await defHttp.post<{ status: number; body: unknown }>(
        { url: '/gm/gateway/admin', data: { method: form.method, path: form.path.trim(), ...(body === undefined ? {} : { body }) } },
        requestOptions
      );
    } catch (failure) {
      const message = (failure as { response?: { data?: { message?: unknown } } })?.response?.data?.message;
      error.value = typeof message === 'string' && message ? message : '请求未完成，请刷新核对结果';
    } finally {
      sending.value = false;
      confirmed.value = false;
    }
  }
</script>

<style lang="less" scoped>
  .gateway-card {
    margin-top: 16px;
  }

  .gateway-space {
    margin-top: 12px;
  }

  .gateway-json {
    max-height: 320px;
    overflow: auto;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
