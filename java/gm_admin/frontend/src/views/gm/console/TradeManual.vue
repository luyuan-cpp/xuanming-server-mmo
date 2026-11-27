<template>
  <a-card title="交易人工处置" class="trade-card">
    <template #extra>
      <a-button :loading="contractsLoading" :disabled="busy" @click="reloadAll">刷新</a-button>
    </template>
    <a-alert v-if="statusError" type="warning" show-icon :message="statusError" />
    <a-alert v-else-if="status && !status.enabled" type="warning" show-icon message="交易管理通道未配置，当前只能查看本地记录。" />
    <a-alert
      v-else-if="status?.dualControl"
      type="info"
      show-icon
      message="交易人工处置需要两人：一人发起，另一人核对后批准执行。批准前会按当前合同重算一遍计划。"
    />

    <h3 class="trade-space">转人工的合同</h3>
    <a-table
      row-key="commerceId"
      size="small"
      :columns="contractColumns"
      :data-source="contracts"
      :loading="contractsLoading"
      :pagination="false"
    >
      <template #bodyCell="{ column, record }">
        <a-button v-if="column.key === 'open'" size="small" :disabled="busy" @click="openContract(String(record.commerceId))">查看</a-button>
      </template>
    </a-table>
    <a-button v-if="nextCursor" class="trade-space" :disabled="busy || contractsLoading" @click="loadContracts(true)">下一页</a-button>

    <h3 class="trade-space">处置申请</h3>
    <a-table row-key="id" size="small" :columns="commandColumns" :data-source="commands" :loading="commandsLoading" :pagination="false">
      <template #bodyCell="{ column, record }">
        <a-tag v-if="column.key === 'status'" :color="statusColor(record.status)">{{ statusLabel(record.status) }}</a-tag>
        <a-button v-else-if="column.key === 'open'" size="small" :disabled="busy" @click="openCommand(record)">核对</a-button>
      </template>
    </a-table>

    <a-modal v-model:open="contractOpen" title="合同详情" width="880px" :footer="null" :mask-closable="false">
      <a-spin :spinning="contractLoading">
        <a-alert v-if="contractError" type="warning" show-icon :message="contractError" />
        <template v-else-if="contract">
          <a-descriptions :column="2" bordered size="small">
            <a-descriptions-item label="合同号">{{ contract.record?.commerceId }}</a-descriptions-item>
            <a-descriptions-item label="版本">{{ contract.record?.version }}</a-descriptions-item>
            <a-descriptions-item label="环境">{{ contract.target }}</a-descriptions-item>
            <a-descriptions-item label="状态">{{ contract.record?.status }}</a-descriptions-item>
          </a-descriptions>
          <h4 class="trade-space">各动作试算</h4>
          <a-table row-key="action" size="small" :columns="verdictColumns" :data-source="contract.actions || []" :pagination="false" />
          <a-collapse class="trade-space">
            <a-collapse-panel key="legs" header="腿与资产指令"><pre class="trade-json">{{ pretty(contract.legs) }}</pre></a-collapse-panel>
            <a-collapse-panel key="parties" header="两方角色归属"><pre class="trade-json">{{ pretty(contract.parties) }}</pre></a-collapse-panel>
          </a-collapse>

          <template v-if="hasPermission('gm:trade:prepare')">
            <h4 class="trade-space">发起处置</h4>
            <a-form layout="vertical">
              <a-form-item label="动作" required>
                <a-select v-model:value="form.action" :options="allowedActions" placeholder="只列出试算允许的动作" />
              </a-form-item>
              <a-form-item label="环境名（照工单填写，不要从上面照抄）" required>
                <a-input v-model:value="form.expectTarget" :maxlength="64" />
              </a-form-item>
              <a-form-item v-if="form.action === 'partial'" label="缺口（每行一条：腿名=缺口）" required>
                <a-textarea v-model:value="form.parts" :rows="3" />
              </a-form-item>
              <template v-if="form.action === 'redirect'">
                <a-form-item label="改绑哪一方" required>
                  <a-select v-model:value="form.party" :options="partyOptions" />
                </a-form-item>
                <a-form-item label="目标角色 ID" required><a-input v-model:value="form.toPlayerId" :maxlength="20" /></a-form-item>
              </template>
              <template v-if="form.action === 'void-auction'">
                <a-form-item label="出价合同号" required><a-input v-model:value="form.linkedCommerceId" :maxlength="20" /></a-form-item>
                <a-form-item label="出价版本" required><a-input v-model:value="form.linkedVersion" :maxlength="20" /></a-form-item>
              </template>
              <a-form-item label="原因（工单号与核对结论）" required>
                <a-input v-model:value="form.reason" :maxlength="191" />
              </a-form-item>
            </a-form>
            <a-alert v-if="proposeError" type="warning" show-icon :message="proposeError" />
            <a-button type="primary" :loading="proposing" :disabled="busy || !canPropose" @click="propose">提交申请</a-button>
          </template>
        </template>
      </a-spin>
    </a-modal>

    <a-modal v-model:open="reviewOpen" title="核对处置申请" width="880px" :mask-closable="false" :closable="!busy">
      <template v-if="reviewed">
        <a-descriptions :column="1" bordered size="small">
          <a-descriptions-item label="申请号">{{ reviewed.id }}</a-descriptions-item>
          <a-descriptions-item label="状态">
            <a-tag :color="statusColor(reviewed.status)">{{ statusLabel(reviewed.status) }}</a-tag>
          </a-descriptions-item>
          <a-descriptions-item label="原因">{{ reviewed.reason }}</a-descriptions-item>
        </a-descriptions>
        <ApprovalPanel
          :command="reviewed"
          kind="trade-manual"
          :can-release="hasPermission('gm:trade:approve')"
          :can-propose="hasPermission('gm:trade:prepare')"
          :busy="busy"
          @declined="onDeclined"
        />
        <a-collapse class="trade-space">
          <a-collapse-panel key="request" header="冻结的处置参数"><pre class="trade-json">{{ pretty(reviewed.request) }}</pre></a-collapse-panel>
          <a-collapse-panel v-if="reviewed.result" key="result" header="执行结果"><pre class="trade-json">{{ pretty(reviewed.result) }}</pre></a-collapse-panel>
          <a-collapse-panel v-if="hasPermission('gm:audit:read')" key="audit" header="状态变更轨迹">
            <pre class="trade-json">{{ pretty(auditTrail) }}</pre>
          </a-collapse-panel>
        </a-collapse>
        <template v-if="plan">
          <h4 class="trade-space">按当前合同重算的计划</h4>
          <a-alert v-if="plan.verdict !== 'MANUAL_CONTRACT_VERDICT_OK'" type="error" show-icon :message="`不可执行：${plan.verdict} ${plan.refusal || ''}`" />
          <a-alert v-else-if="plan.alreadyExecuted" type="success" show-icon message="这条处置已经执行过。" />
          <pre class="trade-json">{{ pretty(plan.plan) }}</pre>
        </template>
        <a-alert v-if="reviewError" type="warning" show-icon :message="reviewError" class="trade-space" />
        <a-checkbox v-if="canRelease" v-model:checked="reviewConfirmed" :disabled="busy" class="trade-space">
          我已核对合同、动作、参数与重算的计划，确认{{ reviewed.status === 'UNKNOWN' ? '沿用原申请重试' : '执行这次处置' }}。
        </a-checkbox>
      </template>
      <template #footer>
        <a-button :disabled="busy" @click="reviewOpen = false">关闭</a-button>
        <a-button :loading="planning" :disabled="busy || !reviewed" @click="replan">重算计划</a-button>
        <a-button
          v-if="canRelease"
          type="primary"
          :loading="releasing"
          :disabled="busy || !reviewConfirmed || !planIsExecutable"
          @click="release"
        >
          {{ reviewed?.status === 'UNKNOWN' ? '沿用原申请重试' : status?.dualControl ? '批准并执行' : '确认执行' }}
        </a-button>
      </template>
    </a-modal>
  </a-card>
</template>

<script lang="ts" setup name="gm-trade-manual">
  import { computed, onMounted, reactive, ref, watch } from 'vue';
  import { usePermission } from '/@/hooks/web/usePermission';
  import ApprovalPanel from './ApprovalPanel.vue';
  import { APPROVAL_STATUS_COLORS, APPROVAL_STATUS_LABELS, awaitsAnotherApprover, dualControl, type ApprovalView } from '/@/api/gm/approval';
  import { newClientRequestId, type GmAuditEntry } from '/@/api/gm/console';
  import {
    TRADE_MANUAL_ACTIONS,
    dryRunTradeManual,
    getTradeManualAudit,
    getTradeManualCommand,
    getTradeStatus,
    listManualContracts,
    listTradeManualCommands,
    prepareTradeManual,
    sendTradeManual,
    showManualContract,
    type ManualContractRecord,
    type ManualContractReply,
    type TradeManualCommand,
    type TradeStatus,
  } from '/@/api/gm/tradeManual';

  const emit = defineEmits<{ (event: 'busy-change', busy: boolean): void }>();
  const { hasPermission } = usePermission();

  const status = ref<TradeStatus>();
  const statusError = ref('');
  const contracts = ref<ManualContractRecord[]>([]);
  const contractsLoading = ref(false);
  const nextCursor = ref<{ updated: string; id: string } | null>(null);
  const commands = ref<TradeManualCommand[]>([]);
  const commandsLoading = ref(false);

  const contractOpen = ref(false);
  const contractLoading = ref(false);
  const contractError = ref('');
  const contract = ref<ManualContractReply>();
  const form = reactive({ action: undefined as string | undefined, expectTarget: '', reason: '', parts: '', party: undefined as string | undefined,
    toPlayerId: '', linkedCommerceId: '', linkedVersion: '', clientRequestId: '' });
  const proposing = ref(false);
  const proposeError = ref('');

  const reviewOpen = ref(false);
  const reviewed = ref<TradeManualCommand>();
  const plan = ref<ManualContractReply>();
  const planning = ref(false);
  const releasing = ref(false);
  const reviewConfirmed = ref(false);
  const reviewError = ref('');
  const auditTrail = ref<GmAuditEntry[]>([]);

  const busy = computed(() => proposing.value || releasing.value || planning.value);
  watch(busy, (value) => emit('busy-change', value));

  const partyOptions = [{ value: 'seller', label: '卖方' }, { value: 'buyer', label: '买方' }];
  const contractColumns = [
    { title: '合同号', dataIndex: 'commerceId' },
    { title: '类型', dataIndex: 'kind' },
    { title: '状态', dataIndex: 'status' },
    { title: '版本', dataIndex: 'version' },
    { title: '', key: 'open', width: 80 },
  ];
  const commandColumns = [
    { title: '申请号', dataIndex: 'id' },
    { title: '状态', key: 'status' },
    { title: '发起人', dataIndex: 'operator' },
    { title: '批准人', dataIndex: 'approver' },
    { title: '创建时间', dataIndex: 'createdAt' },
    { title: '', key: 'open', width: 80 },
  ];
  const verdictColumns = [
    { title: '动作', dataIndex: 'action' },
    { title: '允许', dataIndex: 'allowed', customRender: ({ value }: { value: unknown }) => (value ? '是' : '否') },
    { title: '说明', dataIndex: 'detail' },
  ];

  const allowedActions = computed(() =>
    TRADE_MANUAL_ACTIONS.filter((option) => contract.value?.actions?.some((entry) => entry.action === option.value && entry.allowed))
  );
  const canPropose = computed(() => {
    if (!form.action || !form.expectTarget.trim() || !form.reason.trim()) return false;
    if (form.action === 'partial') return parsedParts().length > 0;
    if (form.action === 'redirect') return !!form.party && /^[1-9][0-9]{0,19}$/.test(form.toPlayerId);
    if (form.action === 'void-auction') return /^[1-9][0-9]{0,19}$/.test(form.linkedCommerceId) && /^[1-9][0-9]{0,19}$/.test(form.linkedVersion);
    return true;
  });
  // 自己发起、还没人批准的申请，开着双人复核时自己不能放行（后台同样会拒绝）。
  const canRelease = computed(
    () =>
      !!reviewed.value &&
      hasPermission('gm:trade:approve') &&
      ['DRAFT', 'UNKNOWN'].includes(reviewed.value.status) &&
      !awaitsAnotherApprover(reviewed.value) &&
      status.value?.enabled === true
  );
  const planIsExecutable = computed(() => plan.value?.verdict === 'MANUAL_CONTRACT_VERDICT_OK' && !plan.value?.alreadyExecuted);

  function errorText(error: unknown): string {
    const message = (error as { response?: { data?: { message?: unknown } } })?.response?.data?.message ?? (error as Error)?.message;
    return typeof message === 'string' && message.length > 0 ? message : '请求未完成，请稍后重试';
  }
  function pretty(value: unknown): string {
    return JSON.stringify(value ?? null, null, 2);
  }
  function statusLabel(value: string): string {
    return (
      ({ DRAFT: '待批准', SENDING: '执行中', EXECUTED: '已执行', UNKNOWN: '结果未知', REJECTED: '已拒绝', ...APPROVAL_STATUS_LABELS } as Record<string, string>)[
        value
      ] || value
    );
  }
  function statusColor(value: string): string {
    return (
      ({ DRAFT: 'blue', SENDING: 'processing', EXECUTED: 'success', UNKNOWN: 'warning', REJECTED: 'error', ...APPROVAL_STATUS_COLORS } as Record<string, string>)[
        value
      ] || 'default'
    );
  }
  function parsedParts(): string[] {
    return form.parts
      .split('\n')
      .map((line) => line.trim())
      .filter((line) => line.length > 0);
  }

  async function loadStatus() {
    try {
      status.value = await getTradeStatus();
      dualControl.value = status.value.dualControl === true;
      statusError.value = '';
    } catch (error) {
      status.value = undefined;
      statusError.value = `无法确认交易管理通道状态。${errorText(error)}`;
    }
  }

  async function loadContracts(next = false) {
    if (contractsLoading.value) return;
    contractsLoading.value = true;
    try {
      const cursor = next && nextCursor.value ? nextCursor.value : { updated: '0', id: '0' };
      const page = await listManualContracts(cursor.updated, cursor.id, 50);
      contracts.value = next ? [...contracts.value, ...(page.contracts || [])] : page.contracts || [];
      const more = page.nextAfterUpdatedMs && page.nextAfterUpdatedMs !== '0';
      nextCursor.value = more ? { updated: String(page.nextAfterUpdatedMs), id: String(page.nextAfterCommerceId || '0') } : null;
    } catch (error) {
      statusError.value = `转人工合同列表加载失败。${errorText(error)}`;
    } finally {
      contractsLoading.value = false;
    }
  }

  async function loadCommands() {
    if (!hasPermission('gm:audit:read')) return;
    commandsLoading.value = true;
    try {
      commands.value = (await listTradeManualCommands(1, 50)).records || [];
    } catch (error) {
      statusError.value = `处置申请列表加载失败。${errorText(error)}`;
    } finally {
      commandsLoading.value = false;
    }
  }

  async function reloadAll() {
    await loadStatus();
    await Promise.all([loadContracts(false), loadCommands()]);
  }

  async function openContract(commerceId: string) {
    contractOpen.value = true;
    contractLoading.value = true;
    contractError.value = '';
    contract.value = undefined;
    Object.assign(form, { action: undefined, expectTarget: '', reason: '', parts: '', party: undefined, toPlayerId: '', linkedCommerceId: '',
      linkedVersion: '', clientRequestId: newClientRequestId() });
    proposeError.value = '';
    try {
      contract.value = await showManualContract(commerceId);
      if (contract.value.verdict !== 'MANUAL_CONTRACT_VERDICT_OK') contractError.value = `读不到合同：${contract.value.verdict} ${contract.value.refusal || ''}`;
    } catch (error) {
      contractError.value = errorText(error);
    } finally {
      contractLoading.value = false;
    }
  }

  async function propose() {
    const record = contract.value?.record;
    if (!record?.commerceId || !record.version || !form.action || !canPropose.value || proposing.value) return;
    proposing.value = true;
    proposeError.value = '';
    try {
      const { command } = await prepareTradeManual({
        clientRequestId: form.clientRequestId,
        commerceId: String(record.commerceId),
        action: form.action as never,
        expectVersion: String(record.version),
        expectTarget: form.expectTarget.trim(),
        reason: form.reason.trim(),
        ...(form.action === 'partial' ? { parts: parsedParts() } : {}),
        ...(form.action === 'redirect' ? { party: form.party as 'seller' | 'buyer', toPlayerId: form.toPlayerId } : {}),
        ...(form.action === 'void-auction' ? { linkedCommerceId: form.linkedCommerceId, linkedVersion: form.linkedVersion } : {}),
      });
      contractOpen.value = false;
      await loadCommands();
      await openCommand(command);
    } catch (error) {
      proposeError.value = errorText(error);
    } finally {
      proposing.value = false;
    }
  }

  async function openCommand(command: TradeManualCommand) {
    reviewed.value = command;
    plan.value = undefined;
    reviewConfirmed.value = false;
    reviewError.value = '';
    auditTrail.value = [];
    reviewOpen.value = true;
    await Promise.all([replan(), loadAudit(command.id)]);
  }

  async function loadAudit(commandId: string) {
    if (!hasPermission('gm:audit:read')) return;
    try {
      auditTrail.value = await getTradeManualAudit(commandId);
    } catch (error) {
      reviewError.value = `状态轨迹加载失败。${errorText(error)}`;
    }
  }

  async function replan() {
    const command = reviewed.value;
    if (!command || planning.value || !['DRAFT', 'UNKNOWN'].includes(command.status)) return;
    planning.value = true;
    try {
      plan.value = await dryRunTradeManual(command.id);
    } catch (error) {
      reviewError.value = `重算计划失败。${errorText(error)}`;
    } finally {
      planning.value = false;
    }
  }

  async function release() {
    const command = reviewed.value;
    if (!command || !canRelease.value || !reviewConfirmed.value || releasing.value) return;
    releasing.value = true;
    reviewError.value = '';
    try {
      reviewed.value = await sendTradeManual(command.id);
    } catch (error) {
      reviewError.value = `执行结果未确认，请刷新后核对这条申请。${errorText(error)}`;
      try {
        reviewed.value = await getTradeManualCommand(command.id);
      } catch {
        // 刷新失败时保留原状态，提示已经给出。
      }
    } finally {
      releasing.value = false;
      reviewConfirmed.value = false;
    }
    await loadAudit(command.id);
    await reloadAll();
  }

  async function onDeclined(command: ApprovalView) {
    if (reviewed.value?.id !== command.id) return;
    reviewed.value = command as TradeManualCommand;
    reviewConfirmed.value = false;
    await loadAudit(command.id);
    await loadCommands();
  }

  onMounted(reloadAll);
</script>

<style lang="less" scoped>
  .trade-card {
    margin-top: 16px;
  }

  .trade-space {
    margin-top: 12px;
  }

  .trade-json {
    max-height: 320px;
    overflow: auto;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
