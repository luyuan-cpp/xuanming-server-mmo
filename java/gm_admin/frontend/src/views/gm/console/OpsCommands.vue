<template>
  <a-card title="运营处置" class="ops-card">
    <template #extra>
      <a-button :disabled="busy" @click="reloadAll">刷新</a-button>
    </template>
    <a-alert v-if="statusError" type="warning" show-icon :message="statusError" />
    <a-alert
      v-else-if="status?.dualControl"
      type="info"
      show-icon
      message="运营处置需要两人：一人发起，另一人核对冻结的参数后批准执行。下游按申请号判重，结果未知时可沿用原申请重试。"
    />
    <a-alert v-for="target in disabledTargets" :key="target" type="warning" show-icon class="ops-space" :message="`${targetLabel(target)}管理通道未配置，只能查看本地记录。`" />

    <template v-if="hasPermission('gm:mall:read')">
      <h3 class="ops-space">商城现状</h3>
      <a-space wrap>
        <a-button :loading="mallLoading" :disabled="busy" @click="loadMallManual">人工待办</a-button>
        <a-button :loading="mallLoading" :disabled="busy" @click="loadMallSkus">SKU 目录</a-button>
        <a-input v-model:value="orderLookup" placeholder="订单号" :maxlength="64" style="width: 220px" />
        <a-button :loading="mallLoading" :disabled="busy || !orderLookup.trim()" @click="loadMallOrder">查订单</a-button>
      </a-space>
      <a-alert v-if="mallError" type="warning" show-icon :message="mallError" class="ops-space" />
      <pre v-if="mallView" class="ops-json ops-space">{{ pretty(mallView) }}</pre>
    </template>

    <template v-if="proposable.length > 0">
      <h3 class="ops-space">发起处置</h3>
      <a-form layout="vertical">
        <a-form-item label="命令" required>
          <a-select v-model:value="form.key" :options="proposable" placeholder="选择要发起的命令" @change="resetFields" />
        </a-form-item>
        <template v-if="selected">
          <a-form-item v-for="field in selected.fields" :key="field.name" :label="field.label" :required="field.required">
            <a-select v-if="field.options" v-model:value="form.values[field.name]" :options="field.options" />
            <a-textarea v-else-if="field.kind === 'json'" v-model:value="form.values[field.name]" :rows="4" />
            <a-input v-else v-model:value="form.values[field.name]" :maxlength="128" />
          </a-form-item>
        </template>
        <a-form-item label="原因（工单号与核对结论）" required>
          <a-input v-model:value="form.reason" :maxlength="191" />
        </a-form-item>
      </a-form>
      <a-alert v-if="proposeError" type="warning" show-icon :message="proposeError" />
      <a-button type="primary" :loading="proposing" :disabled="busy || !selected" @click="propose">提交申请</a-button>
    </template>

    <template v-if="hasPermission('gm:audit:read')">
      <h3 class="ops-space">处置申请</h3>
      <a-table row-key="id" size="small" :columns="commandColumns" :data-source="commands" :loading="commandsLoading" :pagination="false">
        <template #bodyCell="{ column, record }">
          <a-tag v-if="column.key === 'status'" :color="statusColor(record.status)">{{ statusLabel(record.status) }}</a-tag>
          <span v-else-if="column.key === 'command'">{{ record.request?.target }} / {{ record.request?.command }}</span>
          <a-button v-else-if="column.key === 'open'" size="small" :disabled="busy" @click="openCommand(record)">核对</a-button>
        </template>
      </a-table>
    </template>

    <a-modal v-model:open="reviewOpen" title="核对运营处置申请" width="880px" :mask-closable="false" :closable="!busy">
      <template v-if="reviewed">
        <a-descriptions :column="1" bordered size="small">
          <a-descriptions-item label="申请号">{{ reviewed.id }}</a-descriptions-item>
          <a-descriptions-item label="命令">{{ reviewed.request?.target }} / {{ reviewed.request?.command }}</a-descriptions-item>
          <a-descriptions-item label="状态">
            <a-tag :color="statusColor(reviewed.status)">{{ statusLabel(reviewed.status) }}</a-tag>
          </a-descriptions-item>
          <a-descriptions-item label="原因">{{ reviewed.reason }}</a-descriptions-item>
        </a-descriptions>
        <ApprovalPanel
          :command="reviewed"
          kind="ops"
          :can-release="hasPermission(permissionOf(reviewed, 'approve'))"
          :can-propose="hasPermission(permissionOf(reviewed, 'prepare'))"
          :busy="busy"
          @declined="onDeclined"
        />
        <a-collapse class="ops-space">
          <a-collapse-panel key="request" header="冻结的处置参数"><pre class="ops-json">{{ pretty(reviewed.request) }}</pre></a-collapse-panel>
          <a-collapse-panel v-if="reviewed.result" key="result" header="执行结果"><pre class="ops-json">{{ pretty(reviewed.result) }}</pre></a-collapse-panel>
          <a-collapse-panel v-if="hasPermission('gm:audit:read')" key="audit" header="状态变更轨迹"><pre class="ops-json">{{ pretty(auditTrail) }}</pre></a-collapse-panel>
        </a-collapse>
        <a-alert v-if="reviewError" type="warning" show-icon :message="reviewError" class="ops-space" />
        <a-checkbox v-if="canRelease" v-model:checked="reviewConfirmed" :disabled="busy" class="ops-space">
          我已核对命令、冻结的参数与下游现状，确认{{ reviewed.status === 'UNKNOWN' ? '沿用原申请重试' : '执行这次处置' }}。
        </a-checkbox>
      </template>
      <template #footer>
        <a-button :disabled="busy" @click="reviewOpen = false">关闭</a-button>
        <a-button v-if="canRelease" type="primary" :loading="releasing" :disabled="busy || !reviewConfirmed" @click="release">
          {{ reviewed?.status === 'UNKNOWN' ? '沿用原申请重试' : status?.dualControl ? '批准并执行' : '确认执行' }}
        </a-button>
      </template>
    </a-modal>
  </a-card>
</template>

<script lang="ts" setup name="gm-ops-commands">
  import { computed, onMounted, reactive, ref, watch } from 'vue';
  import { usePermission } from '/@/hooks/web/usePermission';
  import ApprovalPanel from './ApprovalPanel.vue';
  import { APPROVAL_STATUS_COLORS, APPROVAL_STATUS_LABELS, awaitsAnotherApprover, dualControl, type ApprovalView } from '/@/api/gm/approval';
  import { newClientRequestId, type GmAuditEntry } from '/@/api/gm/console';
  import {
    OPS_COMMANDS,
    OPS_TARGETS,
    getMallOrder,
    getOpsAudit,
    getOpsCommand,
    getOpsStatus,
    listMallManual,
    listMallSkus,
    listOpsCommands,
    prepareOps,
    sendOps,
    type OpsCommand,
    type OpsStatus,
    type OpsTarget,
  } from '/@/api/gm/ops';

  const emit = defineEmits<{ (event: 'busy-change', busy: boolean): void }>();
  const { hasPermission } = usePermission();

  const status = ref<OpsStatus>();
  const statusError = ref('');
  const mallLoading = ref(false);
  const mallError = ref('');
  const mallView = ref<unknown>();
  const orderLookup = ref('');
  const commands = ref<OpsCommand[]>([]);
  const commandsLoading = ref(false);

  const form = reactive({ key: undefined as string | undefined, values: {} as Record<string, string>, reason: '', clientRequestId: newClientRequestId() });
  const proposing = ref(false);
  const proposeError = ref('');

  const reviewOpen = ref(false);
  const reviewed = ref<OpsCommand>();
  const releasing = ref(false);
  const reviewConfirmed = ref(false);
  const reviewError = ref('');
  const auditTrail = ref<GmAuditEntry[]>([]);

  const busy = computed(() => proposing.value || releasing.value);
  watch(busy, (value) => emit('busy-change', value));

  const commandColumns = [
    { title: '申请号', dataIndex: 'id' },
    { title: '命令', key: 'command' },
    { title: '状态', key: 'status' },
    { title: '发起人', dataIndex: 'operator' },
    { title: '批准人', dataIndex: 'approver' },
    { title: '创建时间', dataIndex: 'createdAt' },
    { title: '', key: 'open', width: 80 },
  ];

  const keyOf = (spec: { target: string; command: string }) => `${spec.target}/${spec.command}`;
  const proposable = computed(() =>
    OPS_COMMANDS.filter((spec) => hasPermission(`gm:${spec.target}:prepare`)).map((spec) => ({
      value: keyOf(spec),
      label: `${targetLabel(spec.target)} · ${spec.label}`,
    }))
  );
  const selected = computed(() => OPS_COMMANDS.find((spec) => keyOf(spec) === form.key));
  const disabledTargets = computed(() => Object.entries(status.value?.targets || {}).filter(([, on]) => !on).map(([target]) => target));
  const canRelease = computed(
    () =>
      !!reviewed.value &&
      hasPermission(permissionOf(reviewed.value, 'approve')) &&
      ['DRAFT', 'UNKNOWN'].includes(reviewed.value.status) &&
      !awaitsAnotherApprover(reviewed.value) &&
      status.value?.targets?.[String(reviewed.value.request?.target)] === true
  );

  function targetLabel(target: string): string {
    return OPS_TARGETS.find((entry) => entry.value === target)?.label || target;
  }
  function permissionOf(command: OpsCommand, action: 'prepare' | 'approve'): string {
    return `gm:${String(command.request?.target || '')}:${action}`;
  }
  function errorText(error: unknown): string {
    const message = (error as { response?: { data?: { message?: unknown } } })?.response?.data?.message ?? (error as Error)?.message;
    return typeof message === 'string' && message.length > 0 ? message : '请求未完成，请稍后重试';
  }
  function pretty(value: unknown): string {
    return JSON.stringify(value ?? null, null, 2);
  }
  function statusLabel(value: string): string {
    return ({ DRAFT: '待批准', SENDING: '执行中', EXECUTED: '已执行', UNKNOWN: '结果未知', REJECTED: '已拒绝', ...APPROVAL_STATUS_LABELS } as Record<string, string>)[value] || value;
  }
  function statusColor(value: string): string {
    return (
      ({ DRAFT: 'blue', SENDING: 'processing', EXECUTED: 'success', UNKNOWN: 'warning', REJECTED: 'error', ...APPROVAL_STATUS_COLORS } as Record<string, string>)[value] ||
      'default'
    );
  }
  function resetFields() {
    form.values = {};
    proposeError.value = '';
  }

  async function loadStatus() {
    try {
      status.value = await getOpsStatus();
      dualControl.value = status.value.dualControl === true;
      statusError.value = '';
    } catch (error) {
      status.value = undefined;
      statusError.value = `无法确认运营处置通道状态。${errorText(error)}`;
    }
  }

  async function mallRead(load: () => Promise<unknown>) {
    mallLoading.value = true;
    mallError.value = '';
    try {
      mallView.value = await load();
    } catch (error) {
      mallError.value = errorText(error);
    } finally {
      mallLoading.value = false;
    }
  }
  const loadMallManual = () => mallRead(() => listMallManual(50));
  const loadMallSkus = () => mallRead(() => listMallSkus(0, 50));
  const loadMallOrder = () => mallRead(() => getMallOrder(orderLookup.value.trim()));

  async function loadCommands() {
    if (!hasPermission('gm:audit:read')) return;
    commandsLoading.value = true;
    try {
      commands.value = (await listOpsCommands(1, 50)).records || [];
    } catch (error) {
      statusError.value = `处置申请列表加载失败。${errorText(error)}`;
    } finally {
      commandsLoading.value = false;
    }
  }

  async function reloadAll() {
    await loadStatus();
    await loadCommands();
  }

  /** 按字段类型把表单值转成下游要的形状;空的可选字段不提交。 */
  function buildBody(): Record<string, unknown> {
    const body: Record<string, unknown> = {};
    for (const field of selected.value?.fields || []) {
      const raw = (form.values[field.name] ?? '').trim();
      if (!raw) {
        if (field.required) throw new Error(`请填写「${field.label}」`);
        continue;
      }
      if (field.kind === 'integer') {
        if (!/^-?[0-9]{1,15}$/.test(raw)) throw new Error(`「${field.label}」必须是整数`);
        body[field.name] = Number(raw);
      } else if (field.kind === 'json') {
        try {
          body[field.name] = JSON.parse(raw);
        } catch {
          throw new Error(`「${field.label}」不是合法的 JSON`);
        }
      } else {
        body[field.name] = raw;
      }
    }
    return body;
  }

  async function propose() {
    const spec = selected.value;
    if (!spec || proposing.value) return;
    proposeError.value = '';
    let body: Record<string, unknown>;
    try {
      body = buildBody();
      if (!form.reason.trim()) throw new Error('请填写原因');
    } catch (error) {
      proposeError.value = (error as Error).message;
      return;
    }
    proposing.value = true;
    try {
      const command = await prepareOps({
        clientRequestId: form.clientRequestId,
        target: spec.target as OpsTarget,
        command: spec.command,
        reason: form.reason.trim(),
        body,
      });
      Object.assign(form, { key: undefined, values: {}, reason: '', clientRequestId: newClientRequestId() });
      await loadCommands();
      await openCommand(command);
    } catch (error) {
      proposeError.value = errorText(error);
    } finally {
      proposing.value = false;
    }
  }

  async function openCommand(command: OpsCommand) {
    reviewed.value = command;
    reviewConfirmed.value = false;
    reviewError.value = '';
    auditTrail.value = [];
    reviewOpen.value = true;
    await loadAudit(command.id);
  }

  async function loadAudit(commandId: string) {
    if (!hasPermission('gm:audit:read')) return;
    try {
      auditTrail.value = await getOpsAudit(commandId);
    } catch (error) {
      reviewError.value = `状态轨迹加载失败。${errorText(error)}`;
    }
  }

  async function release() {
    const command = reviewed.value;
    if (!command || !canRelease.value || !reviewConfirmed.value || releasing.value) return;
    releasing.value = true;
    reviewError.value = '';
    try {
      reviewed.value = await sendOps(command.id);
    } catch (error) {
      reviewError.value = `执行结果未确认，请刷新后核对这条申请。${errorText(error)}`;
      try {
        reviewed.value = await getOpsCommand(command.id);
      } catch {
        // 刷新失败时保留原状态，提示已经给出。
      }
    } finally {
      releasing.value = false;
      reviewConfirmed.value = false;
    }
    await loadAudit(command.id);
    await loadCommands();
  }

  async function onDeclined(command: ApprovalView) {
    if (reviewed.value?.id !== command.id) return;
    reviewed.value = command as OpsCommand;
    reviewConfirmed.value = false;
    await loadAudit(command.id);
    await loadCommands();
  }

  onMounted(reloadAll);
</script>

<style lang="less" scoped>
  .ops-card {
    margin-top: 16px;
  }

  .ops-space {
    margin-top: 12px;
  }

  .ops-json {
    max-height: 320px;
    overflow: auto;
    font-size: 12px;
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
