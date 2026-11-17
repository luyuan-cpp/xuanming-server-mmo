<template>
  <div class="gm-console">
    <div class="gm-heading">
      <div>
        <h1>GM 控制台</h1>
        <p>查询玩家与区服，通过邮件发放银两和物品，保留每次操作记录。</p>
      </div>
      <a-button :loading="overviewLoading" :disabled="mutationBusy" @click="loadOverview">刷新服务状态</a-button>
    </div>

    <a-alert v-if="!mailEnabled" type="warning" show-icon :message="serviceMessage" />
    <a-alert v-else type="info" show-icon message="邮件已投递不代表资产已到账；玩家领取附件后才会到账。" />

    <a-row :gutter="[16, 16]">
      <a-col :xs="24" :xl="10">
        <a-card v-if="hasPermission('gm:player:read')" title="玩家查询" class="gm-card">
          <label for="gm-player-id">玩家 ID</label>
          <div class="gm-search">
            <a-input
              id="gm-player-id"
              v-model:value="playerIdInput"
              inputmode="numeric"
              placeholder="完整玩家 ID，精确查询"
              :disabled="mutationBusy"
              @change="invalidatePlayer"
              @press-enter="searchPlayer"
            />
            <a-button v-auth="'gm:player:read'" type="primary" :loading="playerLoading" :disabled="mutationBusy" @click="searchPlayer">
              查询
            </a-button>
          </div>
          <a-alert v-if="playerError" type="error" show-icon :message="playerError" class="gm-space" />
          <a-empty v-else-if="!player && !playerLoading" description="输入玩家 ID 后查询" />
          <template v-if="player">
            <a-descriptions class="gm-space" :column="1" bordered size="small">
              <a-descriptions-item label="玩家 ID"
                ><span class="gm-id">{{ player.playerId }}</span></a-descriptions-item
              >
              <a-descriptions-item label="角色记录">{{ player.found ? '已找到' : '未找到' }}</a-descriptions-item>
              <a-descriptions-item label="归属区服">{{ player.homeZoneId ?? '未提供' }}</a-descriptions-item>
              <a-descriptions-item label="会话记录">{{ player.sessionFound ? '已找到会话记录' : '未找到会话记录' }}</a-descriptions-item>
            </a-descriptions>
            <p class="gm-hint">会话记录用于排查连接状态，不等同于实时在线确认。</p>
            <a-collapse v-if="player.found || player.sessionFound" class="gm-space">
              <a-collapse-panel key="player" header="玩家与会话详情">
                <pre class="gm-json">{{ pretty({ display: player.display, session: player.session }) }}</pre>
              </a-collapse-panel>
            </a-collapse>
            <a-button
              v-if="player.found"
              v-auth="'gm:mail:prepare'"
              class="gm-space"
              :disabled="mutationBusy || !mailEnabled"
              @click="usePlayerForMail"
            >
              将此玩家填入邮件收件人
            </a-button>
          </template>
        </a-card>

        <a-card v-if="hasPermission('gm:zone:read')" title="区服状态" class="gm-card">
          <a-spin :spinning="overviewLoading">
            <a-alert v-if="overviewError" type="error" show-icon :message="overviewError" />
            <a-alert v-else-if="overview?.zonesAvailable === false" type="warning" show-icon message="区服服务暂不可用，本次未获取到区服状态。" />
            <a-empty v-else-if="!overview?.zones.length" description="暂无区服数据" />
            <a-collapse v-else>
              <a-collapse-panel v-for="(zone, index) in overview.zones" :key="index" :header="zoneLabel(zone, index)">
                <a-descriptions :column="1" bordered size="small">
                  <a-descriptions-item v-for="(value, key) in zone" :key="key" :label="zoneFieldLabel(String(key))">
                    <span class="gm-value">{{ displayValue(value) }}</span>
                  </a-descriptions-item>
                </a-descriptions>
              </a-collapse-panel>
            </a-collapse>
          </a-spin>
        </a-card>
      </a-col>

      <a-col :xs="24" :xl="14">
        <a-card v-if="hasPermission('gm:mail:prepare')" title="邮件发放" class="gm-card">
          <p class="gm-hint">附件可同时包含银两和一种物品；留空则不附加该项。所有操作都需要填写原因。</p>
          <BasicForm @register="registerMailForm" :disabled="mutationBusy || !mailEnabled" />
          <a-alert v-if="prepareError" type="warning" show-icon :message="prepareError" class="gm-space" />
          <div class="gm-actions">
            <a-button v-auth="'gm:mail:prepare'" type="primary" :loading="preparing" :disabled="mutationBusy || !mailEnabled" @click="prepareMail">
              {{ preparedCommand ? '查看已有草稿' : prepareError ? '沿用请求号获取草稿' : '生成草稿并核对' }}
            </a-button>
            <a-button :disabled="mutationBusy" @click="resetMail">清空并开始新操作</a-button>
          </div>
          <p v-if="clientRequestId" class="gm-hint gm-space"
            >本次请求号：<span class="gm-id">{{ clientRequestId }}</span></p
          >
          <p class="gm-hint">生成草稿不会发送邮件；核对弹窗中确认后才会投递。修改输入会生成新的草稿请求。</p>
        </a-card>
        <a-card v-else title="邮件发放" class="gm-card">
          <a-empty description="当前账号没有生成邮件草稿的权限" />
        </a-card>
      </a-col>
    </a-row>

    <a-card v-if="hasPermission('gm:audit:read')" title="最近操作记录" class="gm-card">
      <template #extra>
        <a-button v-auth="'gm:audit:read'" :loading="auditLoading" :disabled="mutationBusy" @click="loadAudit">刷新审计</a-button>
      </template>
      <a-alert v-if="auditError" type="warning" show-icon :message="auditError" class="gm-space" />
      <p class="gm-hint">展示最近 50 条命令。已投递表示邮件服务确认收件，不表示玩家已领取。点击“核对 / 详情”查看冻结的请求与结果。</p>
      <BasicTable @register="registerAuditTable" :data-source="auditRecords" :loading="auditLoading">
        <template #status="{ record }"
          ><a-tag :color="statusColor(record.status)">{{ statusLabel(record.status) }}</a-tag></template
        >
        <template #action="{ record }"><a-button type="link" :disabled="mutationBusy" @click="reviewCommand(record)">核对 / 详情</a-button></template>
      </BasicTable>
      <p class="gm-hint gm-space">共 {{ auditTotal }} 条记录，本页显示 {{ auditRecords.length }} 条。</p>
    </a-card>

    <BasicModal
      @register="registerReviewModal"
      title="核对邮件操作"
      :width="800"
      :mask-closable="false"
      :closable="!mutationBusy"
      :keyboard="!mutationBusy"
      :can-fullscreen="false"
    >
      <template v-if="reviewedCommand">
        <a-alert type="info" show-icon message="请核对收件人和附件。确认发送后，玩家需要领取邮件附件才会到账。" />
        <a-descriptions class="gm-space" :column="1" bordered size="small">
          <a-descriptions-item label="命令号"
            ><span class="gm-id">{{ reviewedCommand.id }}</span></a-descriptions-item
          >
          <a-descriptions-item label="状态"
            ><a-tag :color="statusColor(reviewedCommand.status)">{{ statusLabel(reviewedCommand.status) }}</a-tag></a-descriptions-item
          >
          <a-descriptions-item label="操作人">{{ reviewedCommand.operator }}</a-descriptions-item>
          <a-descriptions-item label="收件玩家"
            ><span class="gm-id">{{ reviewedCommand.playerId }}</span></a-descriptions-item
          >
          <a-descriptions-item label="操作原因">{{ reviewedCommand.reason }}</a-descriptions-item>
          <a-descriptions-item label="邮件标题">{{ displayValue(reviewedCommand.request.title) }}</a-descriptions-item>
          <a-descriptions-item label="邮件正文"
            ><span class="gm-value">{{ displayValue(reviewedCommand.request.body) }}</span></a-descriptions-item
          >
          <a-descriptions-item label="货币附件">
            <div v-for="(currency, index) in reviewCurrencies" :key="index" class="gm-id">
              {{ currency.currencyType === 0 ? '银两' : `货币 ${currency.currencyType}` }}：{{ currency.amount }}
            </div>
            <span v-if="!reviewCurrencies.length">无</span>
          </a-descriptions-item>
          <a-descriptions-item label="物品附件">
            <div v-for="(item, index) in reviewItems" :key="index">配置 ID {{ item.configId }} × {{ item.count }}</div>
            <span v-if="!reviewItems.length">无</span>
          </a-descriptions-item>
          <a-descriptions-item label="邮件到期时间">{{ expireTime(reviewedCommand.request.expireMs) }}</a-descriptions-item>
          <a-descriptions-item label="创建时间">{{ reviewedCommand.createdAt }}</a-descriptions-item>
          <a-descriptions-item label="更新时间">{{ reviewedCommand.updatedAt }}</a-descriptions-item>
        </a-descriptions>
        <a-collapse class="gm-space">
          <a-collapse-panel key="request" header="完整请求记录">
            <pre class="gm-json">{{ pretty(reviewedCommand.request) }}</pre>
          </a-collapse-panel>
        </a-collapse>
        <template v-if="reviewedCommand.result">
          <h3>服务端执行结果</h3>
          <pre class="gm-json">{{ pretty(reviewedCommand.result) }}</pre>
        </template>
        <a-alert v-if="sendError" type="warning" show-icon :message="sendError" class="gm-space" />
        <a-alert
          v-else-if="reviewedCommand.status === 'UNKNOWN'"
          type="warning"
          show-icon
          message="上次结果未知。先刷新审计并核对原命令；如需重试，将沿用同一命令号，不创建新发放。"
          class="gm-space"
        />
        <a-alert
          v-else-if="reviewedCommand.status === 'SENDING'"
          type="info"
          show-icon
          message="此命令正在发送，请刷新审计查看结果。"
          class="gm-space"
        />
        <a-alert
          v-else-if="reviewedCommand.status === 'DELIVERED'"
          type="success"
          show-icon
          message="邮件已投递。附件是否到账取决于玩家领取结果。"
          class="gm-space"
        />
        <a-checkbox v-if="canSendReviewed" v-model:checked="reviewConfirmed" :disabled="mutationBusy || !mailEnabled" class="gm-space">
          我已核对玩家 ID、附件数量及操作原因，确认{{ retryingCommand ? '沿用原命令重试' : '发送这封邮件' }}。
        </a-checkbox>
      </template>
      <template #footer>
        <a-button :disabled="mutationBusy" @click="closeReviewModal">关闭</a-button>
        <a-button v-auth="'gm:audit:read'" :loading="auditLoading" :disabled="mutationBusy" @click="loadAudit">刷新原命令状态</a-button>
        <a-button
          v-if="canSendReviewed"
          v-auth="'gm:mail:send'"
          type="primary"
          :loading="sending"
          :disabled="mutationBusy || !mailEnabled || !reviewConfirmed"
          @click="sendReviewedMail"
        >
          {{ retryingCommand ? '沿用原命令重试' : '确认发送' }}
        </a-button>
      </template>
    </BasicModal>
  </div>
</template>

<script lang="ts" name="gm-console" setup>
  import { computed, onMounted, ref } from 'vue';
  import { BasicForm, useForm, type FormSchema } from '/@/components/Form';
  import { BasicTable, useTable, type BasicColumn } from '/@/components/Table';
  import { BasicModal, useModal } from '/@/components/Modal';
  import { usePermission } from '/@/hooks/web/usePermission';
  import { useMessage } from '/@/hooks/web/useMessage';
  import {
    buildMailDraft,
    getGmCommands,
    getGmOverview,
    getGmPlayer,
    newClientRequestId,
    normalizePlayerId,
    prepareGmMail,
    sendGmMail,
    type GmCommand,
    type GmOverview,
    type GmPlayer,
  } from '/@/api/gm/console';

  const { hasPermission } = usePermission();
  const { createMessage } = useMessage();
  const overview = ref<GmOverview>();
  const overviewLoading = ref(false);
  const overviewError = ref('');
  const mailEnabled = computed(() => overview.value?.mailEnabled === true && !overviewError.value);
  const serviceMessage = computed(() =>
    overviewLoading.value ? '正在检查邮件服务状态…' : overviewError.value || '邮件服务尚未接入或未启用，暂不能生成或发送邮件。'
  );
  const preparing = ref(false);
  const sending = ref(false);
  const mutationBusy = computed(() => preparing.value || sending.value);
  const playerIdInput = ref('');
  const player = ref<GmPlayer>();
  const playerLoading = ref(false);
  const playerError = ref('');
  let playerQueryVersion = 0;
  const clientRequestId = ref('');
  const preparedCommand = ref<GmCommand>();
  const prepareError = ref('');
  const reviewedCommand = ref<GmCommand>();
  const reviewCurrencies = computed(() => attachmentEntries('currencies'));
  const reviewItems = computed(() => attachmentEntries('items'));
  const reviewConfirmed = ref(false);
  const sendError = ref('');
  const unconfirmedCommandId = ref('');
  const retryingCommand = computed(() => reviewedCommand.value?.status === 'UNKNOWN' || reviewedCommand.value?.id === unconfirmedCommandId.value);
  const canSendReviewed = computed(() => {
    const command = reviewedCommand.value;
    return !!command && hasPermission('gm:mail:send') && ['DRAFT', 'UNKNOWN'].includes(command.status);
  });
  const auditRecords = ref<GmCommand[]>([]);
  const auditTotal = ref(0);
  const auditLoading = ref(false);
  const auditError = ref('');
  const [registerReviewModal, { openModal: openReviewModal, closeModal: closeReviewModal }] = useModal();

  function invalidateDraft() {
    clientRequestId.value = '';
    preparedCommand.value = undefined;
    prepareError.value = '';
  }

  const formSchemas: FormSchema[] = [
    {
      field: 'playerId',
      label: '收件玩家 ID',
      component: 'Input',
      required: true,
      componentProps: { inputmode: 'numeric', onChange: invalidateDraft },
    },
    {
      field: 'title',
      label: '邮件标题',
      component: 'Input',
      required: true,
      componentProps: { placeholder: '最多 128 个字符', onChange: invalidateDraft },
    },
    {
      field: 'body',
      label: '邮件正文',
      component: 'InputTextArea',
      componentProps: { rows: 3, placeholder: '可留空，最多 4096 个字符', onChange: invalidateDraft },
    },
    {
      field: 'currencyAmount',
      label: '银两数量',
      component: 'Input',
      componentProps: { inputmode: 'numeric', placeholder: '留空则不发银两', onChange: invalidateDraft },
    },
    {
      field: 'itemConfigId',
      label: '物品配置 ID',
      component: 'Input',
      componentProps: { inputmode: 'numeric', placeholder: '留空则不发物品', onChange: invalidateDraft },
    },
    {
      field: 'itemCount',
      label: '物品数量',
      component: 'Input',
      componentProps: { inputmode: 'numeric', placeholder: '1 至 4096，与配置 ID 一起填写', onChange: invalidateDraft },
    },
    {
      field: 'validDays',
      label: '有效天数',
      component: 'InputNumber',
      defaultValue: 7,
      componentProps: { min: 1, max: 30, precision: 0, onChange: invalidateDraft },
    },
    {
      field: 'reason',
      label: '操作原因',
      component: 'InputTextArea',
      required: true,
      componentProps: { rows: 2, placeholder: '填写补偿、测试等具体原因，最多 191 个字符', onChange: invalidateDraft },
    },
  ];
  const [registerMailForm, { getFieldsValue, setFieldsValue, resetFields }] = useForm({
    schemas: formSchemas,
    layout: 'vertical',
    baseColProps: { span: 24 },
    showActionButtonGroup: false,
    autoSubmitOnEnter: false,
  });
  const auditColumns: BasicColumn[] = [
    { title: '命令号', dataIndex: 'id', width: 270 },
    { title: '状态', dataIndex: 'status', width: 120, slots: { customRender: 'status' } },
    { title: '操作人', dataIndex: 'operator', width: 140 },
    { title: '收件玩家 ID', dataIndex: 'playerId', width: 190 },
    { title: '原因', dataIndex: 'reason', width: 220 },
    { title: '创建时间', dataIndex: 'createdAt', width: 200 },
    { title: '更新时间', dataIndex: 'updatedAt', width: 200 },
  ];
  const [registerAuditTable] = useTable({
    columns: auditColumns,
    rowKey: 'id',
    pagination: false,
    showIndexColumn: false,
    showTableSetting: false,
    useSearchForm: false,
    canResize: false,
    bordered: true,
    actionColumn: { title: '操作', dataIndex: 'action', width: 130, slots: { customRender: 'action' } },
    scroll: { x: 1480 },
  });

  function errorText(error: unknown): string {
    return error instanceof Error && error.message ? error.message : '请求失败，请稍后重试。';
  }

  function pretty(value: unknown): string {
    return JSON.stringify(value ?? null, null, 2);
  }

  function displayValue(value: unknown): string {
    if (value === null || value === undefined) return '未提供';
    if (typeof value === 'boolean') return value ? '是' : '否';
    return typeof value === 'object' ? pretty(value) : String(value);
  }

  function attachmentEntries(kind: string): Record<string, unknown>[] {
    const attachments = reviewedCommand.value?.request.attachments;
    if (!attachments || typeof attachments !== 'object') return [];
    const entries = (attachments as Record<string, unknown>)[kind];
    return Array.isArray(entries) ? entries.filter((entry) => entry && typeof entry === 'object') : [];
  }

  function expireTime(value: unknown): string {
    if (typeof value !== 'string' || !/^\d{1,15}$/.test(value)) return '未提供';
    const time = new Date(Number(value));
    return Number.isNaN(time.getTime()) ? '未提供' : time.toLocaleString('zh-CN', { hour12: false });
  }

  function zoneLabel(zone: Record<string, unknown>, index: number): string {
    const id = zone.zoneId ?? zone.zone_id ?? zone.id;
    const name = zone.zoneName ?? zone.zone_name ?? zone.name;
    return `${name ? String(name) : '区服'} ${id === undefined ? index + 1 : String(id)}`;
  }

  function zoneFieldLabel(key: string): string {
    const names: Record<string, string> = {
      zoneId: '区服 ID',
      zone_id: '区服 ID',
      name: '名称',
      zoneName: '名称',
      zone_name: '名称',
      status: '状态',
      manualStatus: '手动状态',
      recommended: '推荐区服',
      createRoleClosed: '关闭建角',
      roleCapacity: '角色容量',
      version: '版本',
    };
    return names[key] || key;
  }

  function statusLabel(status: string): string {
    return (
      ({ DRAFT: '待确认草稿', SENDING: '发送中', DELIVERED: '已投递', REJECTED: '已拒绝', UNKNOWN: '结果未知' } as Record<string, string>)[status] ||
      status
    );
  }

  function statusColor(status: string): string {
    return (
      ({ DRAFT: 'blue', SENDING: 'processing', DELIVERED: 'success', REJECTED: 'error', UNKNOWN: 'warning' } as Record<string, string>)[status] ||
      'default'
    );
  }

  async function loadOverview() {
    if (overviewLoading.value) return;
    overviewLoading.value = true;
    try {
      overview.value = await getGmOverview();
      overviewError.value = '';
    } catch (error) {
      overview.value = undefined;
      overviewError.value = `无法确认邮件服务状态，发放已禁用。${errorText(error)}`;
    } finally {
      overviewLoading.value = false;
    }
  }

  function invalidatePlayer() {
    playerQueryVersion += 1;
    player.value = undefined;
    playerError.value = '';
  }

  async function searchPlayer() {
    if (playerLoading.value || mutationBusy.value || !hasPermission('gm:player:read')) return;
    const version = ++playerQueryVersion;
    player.value = undefined;
    playerError.value = '';
    playerLoading.value = true;
    try {
      const result = await getGmPlayer(normalizePlayerId(playerIdInput.value));
      if (version === playerQueryVersion) player.value = result;
    } catch (error) {
      if (version === playerQueryVersion) playerError.value = errorText(error);
    } finally {
      playerLoading.value = false;
    }
  }

  async function usePlayerForMail() {
    if (!player.value?.found || mutationBusy.value || !hasPermission('gm:mail:prepare')) return;
    invalidateDraft();
    await setFieldsValue({ playerId: player.value.playerId });
    createMessage.info('已填入收件玩家，请填写邮件内容并生成草稿。');
  }

  async function resetMail() {
    if (mutationBusy.value) return;
    await resetFields();
    invalidateDraft();
  }

  function reviewCommand(command: GmCommand) {
    if (mutationBusy.value) return;
    reviewedCommand.value = command;
    reviewConfirmed.value = false;
    sendError.value = command.id === unconfirmedCommandId.value ? '上次发送未获确认，不会自动重发。请先核对审计记录；重试将沿用此命令号。' : '';
    openReviewModal(true);
  }

  async function prepareMail() {
    if (mutationBusy.value || !mailEnabled.value || !hasPermission('gm:mail:prepare')) return;
    if (preparedCommand.value) {
      reviewCommand(preparedCommand.value);
      return;
    }
    preparing.value = true;
    let command: GmCommand | undefined;
    try {
      // 先完成纯校验，再产生请求号。请求失败保留原号；只有输入变更或明确开始新操作才清除。
      const draft = buildMailDraft(getFieldsValue(), clientRequestId.value);
      if (!clientRequestId.value) clientRequestId.value = newClientRequestId();
      draft.clientRequestId = clientRequestId.value;
      command = await prepareGmMail(draft);
      preparedCommand.value = command;
      prepareError.value = '';
    } catch (error) {
      prepareError.value = clientRequestId.value
        ? `未确认草稿结果，本次请求号已保留。再次点击将获取同一草稿，不会发送邮件。${errorText(error)}`
        : errorText(error);
    } finally {
      preparing.value = false;
    }
    if (command) {
      reviewCommand(command);
      await loadAudit();
    }
  }

  async function sendReviewedMail() {
    const command = reviewedCommand.value;
    if (!command || mutationBusy.value || !mailEnabled.value || !reviewConfirmed.value || !canSendReviewed.value) return;
    sending.value = true;
    reviewConfirmed.value = false;
    sendError.value = '';
    try {
      const result = await sendGmMail(command.id);
      reviewedCommand.value = result;
      if (preparedCommand.value?.id === result.id) preparedCommand.value = result;
      unconfirmedCommandId.value = result.status === 'UNKNOWN' ? result.id : '';
      if (result.status === 'DELIVERED') createMessage.success('邮件已投递，玩家领取附件后到账。');
    } catch (error) {
      // 通信异常不伪造服务端状态，也不重新生成命令；保留原命令给管理员明确核对和重试。
      unconfirmedCommandId.value = command.id;
      sendError.value = `服务器未确认发送结果，不会自动重发。请刷新审计核对原命令。${errorText(error)}`;
    } finally {
      sending.value = false;
    }
    await loadAudit();
  }

  async function loadAudit() {
    if (auditLoading.value || !hasPermission('gm:audit:read')) return;
    auditLoading.value = true;
    try {
      const page = await getGmCommands();
      auditRecords.value = page.records;
      auditTotal.value = page.total;
      auditError.value = '';
      const current = reviewedCommand.value;
      if (current) {
        const updated = page.records.find((record) => record.id === current.id);
        if (updated) {
          if (updated.status !== current.status) reviewConfirmed.value = false;
          reviewedCommand.value = updated;
          if (preparedCommand.value?.id === updated.id) preparedCommand.value = updated;
          if (updated.status !== 'UNKNOWN') {
            if (unconfirmedCommandId.value === updated.id) unconfirmedCommandId.value = '';
            sendError.value = '';
          }
        }
      }
    } catch (error) {
      auditError.value = `审计刷新失败，当前显示的可能是旧记录。${errorText(error)}`;
    } finally {
      auditLoading.value = false;
    }
  }

  onMounted(() => {
    void loadOverview();
    void loadAudit();
  });
</script>

<style lang="less" scoped>
  .gm-console {
    padding: 20px;
  }
  .gm-heading {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 16px;
    margin-bottom: 16px;
  }
  .gm-heading h1 {
    margin: 0 0 4px;
    font-size: 24px;
  }
  .gm-heading p,
  .gm-hint {
    color: #64748b;
    margin: 0;
  }
  .gm-console > .ant-alert {
    margin-bottom: 16px;
  }
  .gm-card {
    margin-bottom: 16px;
  }
  .gm-search,
  .gm-actions {
    display: flex;
    gap: 8px;
    margin-top: 8px;
  }
  .gm-actions {
    flex-wrap: wrap;
  }
  .gm-space {
    margin-top: 16px;
  }
  .gm-hint {
    font-size: 12px;
    line-height: 1.7;
  }
  .gm-id {
    font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
    overflow-wrap: anywhere;
  }
  .gm-value {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .gm-json {
    max-height: 320px;
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    padding: 12px;
    background: #f1f5f9;
    color: #1e293b;
    border-radius: 6px;
  }
  @media (max-width: 640px) {
    .gm-console {
      padding: 12px;
    }
    .gm-heading {
      align-items: flex-start;
      flex-direction: column;
    }
  }
</style>
