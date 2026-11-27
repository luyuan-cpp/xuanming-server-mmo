<template>
  <div class="gm-sso-login" role="status" aria-live="polite">
    <p v-if="phase === 'loading'" class="gm-sso-text">正在准备登录…</p>
    <p v-else-if="phase === 'exchanging'" class="gm-sso-text">正在核对身份…</p>
    <template v-else>
      <p v-if="failure" class="gm-sso-failure" role="alert">{{ failure }}</p>
      <a-button type="primary" size="large" block :loading="redirecting" @click="begin">使用企业账号登录</a-button>
    </template>
  </div>
</template>

<script lang="ts" setup name="gm-sso-login">
  import { onMounted, ref } from 'vue';
  import { useUserStore } from '/@/store/modules/user';
  import { beginEnterpriseLogin, finishEnterpriseLogin, identityMode, loadIdentityMode } from '/@/api/gm/sso';

  // loading：还没问到登录模式；exchanging：带着授权码回来，正在换会话；ready：等待点击。
  const phase = ref<'loading' | 'exchanging' | 'ready'>('loading');
  const failure = ref('');
  const redirecting = ref(false);
  const userStore = useUserStore();

  function describe(error: unknown, fallback: string): string {
    const message = (error as { response?: { data?: { message?: unknown } } })?.response?.data?.message ?? (error as Error)?.message;
    return typeof message === 'string' && message.length > 0 ? message : fallback;
  }

  async function begin() {
    failure.value = '';
    redirecting.value = true;
    try {
      window.location.assign(await beginEnterpriseLogin());
    } catch (error) {
      redirecting.value = false;
      failure.value = describe(error, '无法发起企业登录，请稍后再试');
    }
  }

  onMounted(async () => {
    try {
      if (identityMode.value === 'unknown') await loadIdentityMode();
    } catch (error) {
      // 问不到模式时不显示口令表单：宁可登不了，也不在生产上露出一个用不了的口令入口。
      failure.value = describe(error, '无法连接后台，请稍后刷新重试');
      phase.value = 'ready';
      return;
    }
    if (identityMode.value !== 'oidc') return;
    const query = new URLSearchParams(window.location.search);
    const code = query.get('code');
    const state = query.get('state');
    const refused = query.get('error');
    if (code || state || refused) {
      // 授权码只能用一次；先把它从地址栏拿掉，刷新或后退都不会再提交同一个授权码。
      window.history.replaceState(null, '', window.location.pathname + window.location.hash);
    }
    if (refused) {
      failure.value = '身份提供方没有通过这次登录，请重试';
      phase.value = 'ready';
      return;
    }
    if (!code || !state) {
      phase.value = 'ready';
      return;
    }
    phase.value = 'exchanging';
    try {
      userStore.setToken(await finishEnterpriseLogin(code, state));
      await userStore.afterLoginAction(true, {});
    } catch (error) {
      userStore.setToken(undefined);
      failure.value = describe(error, '登录未完成，请重试');
      phase.value = 'ready';
    }
  });
</script>

<style lang="less" scoped>
  .gm-sso-login {
    padding: 48px 32px;
    text-align: center;
  }

  .gm-sso-text {
    margin: 0;
  }

  .gm-sso-failure {
    margin: 0 0 16px;
    color: #b91c1c;
  }
</style>
