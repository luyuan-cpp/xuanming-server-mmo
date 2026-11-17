<template>
  <a-card title="玩家 GM" class="player-gm">
    <template #extra
      ><a-button
        :loading="catalogLoading"
        :disabled="blocked"
        @click="loadCatalog"
        >刷新可用操作</a-button
      ></template
    >
    <a-alert
      type="info"
      show-icon
      message="仅用于开发和测试环境。玩家须在线，命令会交给玩家当前所在的场景服执行。"
    />
    <a-alert
      v-if="catalogError"
      class="space"
      type="error"
      show-icon
      :message="catalogError"
    />
    <a-alert
      v-else-if="!catalog?.enabled"
      class="space"
      type="warning"
      show-icon
      message="玩家 GM 服务未启用，当前不能生成或执行命令。"
    />
    <a-row :gutter="[24, 16]" class="space">
      <a-col :xs="24" :lg="10">
        <label for="gm-player-operation">选择操作</label>
        <a-select
          id="gm-player-operation"
          v-model:value="operation"
          class="operation-select"
          :options="operationOptions"
          :disabled="blocked"
          @change="invalidateDraft"
        />
        <p class="hint space">
          {{
            selectedCatalog?.description ||
            "选择操作后填写玩家 ID、参数和原因。货币操作按所选币种执行。"
          }}
        </p>
        <a-button
          v-if="playerId"
          class="space"
          :disabled="blocked || !operationEnabled"
          @click="useQueriedPlayer"
          >使用已查询玩家 ID</a-button
        >
      </a-col>
      <a-col :xs="24" :lg="14">
        <BasicForm name="gm-player-command" @register="registerForm" />
        <a-alert
          v-if="prepareError"
          class="space"
          type="warning"
          show-icon
          :message="prepareError"
        />
        <div class="actions">
          <a-button
            v-if="hasPermission(selectedDefinition.permission)"
            :key="selectedDefinition.permission"
            v-auth="selectedDefinition.permission"
            type="primary"
            :loading="preparing"
            :disabled="blocked || !operationEnabled"
            @click="prepare"
          >
            {{
              preparedCommand
                ? "查看已有草稿"
                : prepareError
                  ? "沿用请求号获取草稿"
                  : "生成草稿并核对"
            }}
          </a-button>
          <a-button :disabled="blocked" @click="reset"
            >清空并开始新操作</a-button
          >
        </div>
        <p v-if="clientRequestId" class="hint space">
          本次请求号：<span class="identifier">{{ clientRequestId }}</span>
        </p>
        <p class="hint space">
          草稿不会修改玩家数据，5
          分钟内有效；过期请新建。只有核对并确认后才会执行，结果未知时禁止重发。
        </p>
      </a-col>
    </a-row>

    <div v-if="hasPermission('gm:audit:read')" class="audit">
      <div class="audit-heading">
        <h3>玩家 GM 操作记录</h3>
        <a-button
          v-auth="'gm:audit:read'"
          :loading="auditLoading"
          :disabled="blocked"
          @click="loadAudit"
          >刷新审计</a-button
        >
      </div>
      <a-alert
        v-if="auditError"
        class="space"
        type="warning"
        show-icon
        :message="auditError"
      />
      <BasicTable
        @register="registerTable"
        :data-source="records"
        :loading="auditLoading"
      >
        <template #status="{ record }"
          ><a-tag :color="statusColor(record.status)">{{
            statusLabel(record.status)
          }}</a-tag></template
        >
        <template #operation="{ record }">{{
          operationLabel(record.operation)
        }}</template>
        <template #action="{ record }"
          ><a-button type="link" :disabled="blocked" @click="review(record)"
            >核对 / 详情</a-button
          ></template
        >
      </BasicTable>
      <p class="hint space">
        最近 50 条，共
        {{
          total
        }}
        条。结果未知时先检查玩家状态和服务日志，不要重复创建同一操作。
      </p>
    </div>

    <BasicModal
      @register="registerModal"
      title="核对玩家 GM 操作"
      :width="800"
      :mask-closable="false"
      :closable="!blocked"
      :keyboard="!blocked"
      :can-fullscreen="false"
    >
      <template v-if="reviewed">
        <a-alert
          type="warning"
          show-icon
          message="此操作直接修改在线玩家数据。请核对玩家 ID、操作类型、数量和原因。"
        />
        <a-descriptions class="space" :column="1" bordered size="small">
          <a-descriptions-item label="命令号"
            ><span class="identifier">{{
              reviewed.id
            }}</span></a-descriptions-item
          >
          <a-descriptions-item label="状态"
            ><a-tag :color="statusColor(reviewed.status)">{{
              statusLabel(reviewed.status)
            }}</a-tag></a-descriptions-item
          >
          <a-descriptions-item label="操作">{{
            operationLabel(reviewed.operation)
          }}</a-descriptions-item>
          <a-descriptions-item label="玩家 ID"
            ><span class="identifier">{{
              reviewed.playerId
            }}</span></a-descriptions-item
          >
          <a-descriptions-item label="操作人">{{
            reviewed.operator
          }}</a-descriptions-item>
          <a-descriptions-item label="操作原因">{{
            reviewed.reason
          }}</a-descriptions-item>
          <a-descriptions-item
            v-for="(value, key) in reviewedArguments"
            :key="key"
            :label="argumentLabel(String(key))"
            ><span class="identifier">{{
              argumentValue(String(key), value)
            }}</span></a-descriptions-item
          >
          <a-descriptions-item label="创建时间">{{
            reviewed.createdAt
          }}</a-descriptions-item>
          <a-descriptions-item label="确认截止时间">{{
            reviewExpiry
              ? new Date(reviewExpiry).toLocaleString("zh-CN", {
                  hour12: false,
                })
              : "草稿时间未提供，禁止执行"
          }}</a-descriptions-item>
        </a-descriptions>
        <a-alert
          v-if="reviewed.status === 'DRAFT' && draftExpired"
          class="space"
          type="warning"
          show-icon
          message="草稿已超过 5 分钟有效期，不能刷新原命令时间或继续执行。请关闭后清空并新建草稿。"
        />
        <a-alert
          v-if="
            reviewed.status === 'UNKNOWN' ||
            (attemptedIds.has(reviewed.id) && reviewed.status === 'DRAFT')
          "
          class="space"
          type="warning"
          show-icon
          message="执行结果未确认。此命令不会自动重发，也不能手动重发；请先核查审计和玩家实际状态。"
        />
        <a-alert
          v-else-if="reviewed.status === 'EXECUTED'"
          class="space"
          type="success"
          show-icon
          message="场景服已确认执行完成，此命令不能再次执行。"
        />
        <a-alert
          v-else-if="reviewed.status === 'SENDING'"
          class="space"
          type="info"
          show-icon
          message="命令已提交，正在等待执行结果。请刷新原命令状态，勿重复发起。"
        />
        <a-alert
          v-if="sendError"
          class="space"
          type="warning"
          show-icon
          :message="sendError"
        />
        <a-collapse class="space"
          ><a-collapse-panel key="details" header="冻结的请求与执行结果">
            <pre class="json">{{
              pretty({ request: reviewed.request, result: reviewed.result })
            }}</pre>
          </a-collapse-panel></a-collapse
        >
        <a-checkbox
          v-if="canSend"
          v-model:checked="confirmed"
          :disabled="blocked"
          class="space"
          >我已核对玩家 ID、操作参数与原因，确认执行此操作。</a-checkbox
        >
      </template>
      <template #footer>
        <a-button :disabled="blocked" @click="closeModal">关闭</a-button>
        <a-button
          v-auth="'gm:audit:read'"
          :loading="auditLoading"
          :disabled="blocked"
          @click="loadAudit"
          >刷新原命令状态</a-button
        >
        <a-button
          v-if="canSend"
          :key="reviewPermission"
          v-auth="reviewPermission"
          type="primary"
          :loading="sending"
          :disabled="blocked || !confirmed"
          @click="send"
          >确认执行</a-button
        >
      </template>
    </BasicModal>
  </a-card>
</template>

<script lang="ts" setup name="gm-player-commands">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { BasicForm, useForm, type FormSchema } from "/@/components/Form";
import { BasicTable, useTable, type BasicColumn } from "/@/components/Table";
import { BasicModal, useModal } from "/@/components/Modal";
import { usePermission } from "/@/hooks/web/usePermission";
import { useMessage } from "/@/hooks/web/useMessage";
import { newClientRequestId } from "/@/api/gm/console";
import {
  PLAYER_OPERATIONS,
  buildPlayerCommandDraft,
  getPlayerCommandCatalog,
  getPlayerCommands,
  preparePlayerCommand,
  playerCommandExpiresAt,
  sendPlayerCommand,
  type PlayerOperation,
  type PlayerCommand,
  type PlayerCommandCatalog,
} from "/@/api/gm/playerCommands";

const props = defineProps<{ busy: boolean; playerId?: string }>();
const emit = defineEmits<{ (event: "busy-change", busy: boolean): void }>();
const { hasPermission } = usePermission();
const { createMessage } = useMessage();
const operation = ref<PlayerOperation>(
  PLAYER_OPERATIONS.find((entry) => hasPermission(entry.permission))?.code ||
    "currency.add",
);
const catalog = ref<PlayerCommandCatalog>();
const catalogLoading = ref(false);
const catalogError = ref("");
const preparing = ref(false);
const sending = ref(false);
const blocked = computed(() => props.busy || preparing.value || sending.value);
const selectedDefinition = computed(() =>
  PLAYER_OPERATIONS.find((entry) => entry.code === operation.value)!,
);
const selectedCatalog = computed(() =>
  catalog.value?.operations.find((entry) => entry.code === operation.value),
);
const operationEnabled = computed(
  () =>
    catalog.value?.enabled === true &&
    selectedCatalog.value?.enabled === true &&
    hasPermission(selectedDefinition.value.permission),
);
const operationOptions = computed(() =>
  PLAYER_OPERATIONS.map((entry) => ({
    value: entry.code,
    label: entry.label,
    disabled: !hasPermission(entry.permission),
  })),
);
const clientRequestId = ref("");
const preparedCommand = ref<PlayerCommand>();
const prepareError = ref("");
const reviewed = ref<PlayerCommand>();
const confirmed = ref(false);
const sendError = ref("");
// 网络异常后保留本地已提交标记，即使审计暂时返回旧草稿也不再次执行。
const attemptedIds = ref(new Set<string>());
const currentTime = ref(Date.now());
const reviewExpiry = computed(() =>
  reviewed.value ? playerCommandExpiresAt(reviewed.value) : undefined,
);
const draftExpired = computed(
  () =>
    reviewExpiry.value !== undefined && currentTime.value >= reviewExpiry.value,
);
const reviewPermission = computed(
  () =>
    PLAYER_OPERATIONS.find((entry) => entry.code === reviewed.value?.operation)
      ?.permission || "",
);
const canSend = computed(() => {
  const command = reviewed.value;
  return (
    !!command &&
    command.status === "DRAFT" &&
    reviewExpiry.value !== undefined &&
    !draftExpired.value &&
    !attemptedIds.value.has(command.id) &&
    catalog.value?.enabled === true &&
    catalog.value.operations.some(
      (entry) => entry.code === command.operation && entry.enabled,
    ) &&
    hasPermission(reviewPermission.value)
  );
});
const reviewedArguments = computed<Record<string, unknown>>(() => {
  const args = reviewed.value?.request.arguments;
  return args && typeof args === "object" && !Array.isArray(args)
    ? (args as Record<string, unknown>)
    : {};
});
const [registerModal, { openModal, closeModal }] = useModal();
const records = ref<PlayerCommand[]>([]);
const total = ref(0);
const auditLoading = ref(false);
const auditError = ref("");

function invalidateDraft() {
  clientRequestId.value = "";
  preparedCommand.value = undefined;
  prepareError.value = "";
}
const schemas: FormSchema[] = [
  {
    field: "playerId",
    label: "玩家 ID",
    component: "Input",
    required: true,
    componentProps: {
      inputmode: "numeric",
      placeholder: "完整玩家 ID，精确执行",
      onChange: invalidateDraft,
    },
  },
  {
    field: "currencyType",
    label: "币种",
    component: "Select",
    required: true,
    ifShow: () => operation.value.startsWith("currency."),
    componentProps: () => ({
      options:
        catalog.value?.currencies.map((currency) => ({
          value: currency.id,
          label: currencyLabel(currency.label),
        })) || [],
      onChange: invalidateDraft,
    }),
  },
  {
    field: "amount",
    label: "货币数量",
    component: "Input",
    required: true,
    ifShow: () => ["currency.add", "currency.deduct"].includes(operation.value),
    componentProps: { inputmode: "numeric", onChange: invalidateDraft },
  },
  {
    field: "level",
    label: "目标等级",
    component: "Input",
    required: true,
    ifShow: () => operation.value === "player.level",
    componentProps: {
      inputmode: "numeric",
      placeholder: "1 至 85",
      onChange: invalidateDraft,
    },
  },
  {
    field: "petTableId",
    label: "宠物配置 ID",
    component: "Input",
    required: true,
    ifShow: () => operation.value === "pet.grant",
    componentProps: { inputmode: "numeric", onChange: invalidateDraft },
  },
  {
    field: "configId",
    label: "物品配置 ID",
    component: "Input",
    required: true,
    ifShow: () => operation.value === "item.grant",
    componentProps: { inputmode: "numeric", onChange: invalidateDraft },
  },
  {
    field: "count",
    label: "物品数量",
    component: "Input",
    required: true,
    ifShow: () => operation.value === "item.grant",
    componentProps: {
      inputmode: "numeric",
      placeholder: "1 至 99",
      onChange: invalidateDraft,
    },
  },
  {
    field: "reason",
    label: "操作原因",
    component: "Input",
    required: true,
    componentProps: {
      placeholder: "填写单行具体原因，最多 191 个字符",
      onChange: invalidateDraft,
    },
  },
];
// 全局 disabled 在原生 BasicForm 中会切换为无边框详情样式；逐字段禁用以保留清晰的输入控件。
schemas.forEach((schema) => {
  schema.dynamicDisabled = () => blocked.value || !operationEnabled.value;
});
const [registerForm, { getFieldsValue, setFieldsValue, resetFields }] = useForm(
  {
    schemas,
    layout: "vertical",
    baseColProps: { span: 24 },
    showActionButtonGroup: false,
    autoSubmitOnEnter: false,
  },
);
const columns: BasicColumn[] = [
  { title: "命令号", dataIndex: "id", width: 230 },
  {
    title: "状态",
    dataIndex: "status",
    width: 120,
    slots: { customRender: "status" },
  },
  {
    title: "操作",
    dataIndex: "operation",
    width: 150,
    slots: { customRender: "operation" },
  },
  { title: "玩家 ID", dataIndex: "playerId", width: 190 },
  { title: "操作人", dataIndex: "operator", width: 120 },
  { title: "原因", dataIndex: "reason", width: 200 },
  { title: "创建时间", dataIndex: "createdAt", width: 190 },
];
const [registerTable] = useTable({
  columns,
  rowKey: "id",
  pagination: false,
  showIndexColumn: false,
  showTableSetting: false,
  useSearchForm: false,
  canResize: false,
  bordered: true,
  actionColumn: {
    title: "操作",
    dataIndex: "action",
    width: 120,
    slots: { customRender: "action" },
  },
  scroll: { x: 1420 },
});

function errorText(error: unknown) {
  return error instanceof Error && error.message
    ? error.message
    : "请求失败，请刷新服务状态与审计。";
}
function pretty(value: unknown) {
  return JSON.stringify(value ?? null, null, 2);
}
function operationLabel(code: string) {
  return PLAYER_OPERATIONS.find((entry) => entry.code === code)?.label || code;
}
function statusLabel(value: string) {
  return (
    (
      {
        DRAFT: "待确认草稿",
        SENDING: "执行中",
        EXECUTED: "已执行",
        UNKNOWN: "结果未知",
        REJECTED: "已拒绝",
      } as Record<string, string>
    )[value] || value
  );
}
function statusColor(value: string) {
  return (
    (
      {
        DRAFT: "blue",
        SENDING: "processing",
        EXECUTED: "success",
        UNKNOWN: "warning",
        REJECTED: "error",
      } as Record<string, string>
    )[value] || "default"
  );
}
function argumentLabel(key: string) {
  return (
    (
      {
        currencyType: "币种",
        amount: "货币数量",
        level: "目标等级",
        petTableId: "宠物配置 ID",
        configId: "物品配置 ID",
        count: "物品数量",
      } as Record<string, string>
    )[key] || key
  );
}
function argumentValue(key: string, value: unknown) {
  return key === "currencyType"
    ? currencyLabel(
        catalog.value?.currencies.find((currency) => currency.id === value)
          ?.label || String(value),
      )
    : typeof value === "object"
      ? pretty(value)
      : String(value ?? "未提供");
}
function currencyLabel(label: string) {
  return (
    (
      { GOLD: "银两", DIAMOND: "灵石", BIND_DIAMOND: "绑定灵石" } as Record<
        string,
        string
      >
    )[label.replace(/^CURRENCY_TYPE_/, "")] || label
  );
}

async function loadCatalog() {
  if (catalogLoading.value) return;
  catalogLoading.value = true;
  try {
    catalog.value = await getPlayerCommandCatalog();
    catalogError.value = "";
    if (
      getFieldsValue().currencyType === undefined &&
      catalog.value.currencies.length
    )
      await setFieldsValue({ currencyType: catalog.value.currencies[0].id });
    if (
      !selectedCatalog.value?.enabled ||
      !hasPermission(selectedDefinition.value.permission)
    ) {
      const next = PLAYER_OPERATIONS.find(
        (entry) =>
          hasPermission(entry.permission) &&
          catalog.value?.operations.some(
            (item) => item.code === entry.code && item.enabled,
          ),
      );
      if (next && next.code !== operation.value) {
        operation.value = next.code;
        invalidateDraft();
      }
    }
  } catch (error) {
    catalog.value = undefined;
    catalogError.value = `无法确认玩家 GM 服务状态，执行已禁用。${errorText(error)}`;
  } finally {
    catalogLoading.value = false;
  }
}
async function useQueriedPlayer() {
  if (blocked.value || !operationEnabled.value || !props.playerId) return;
  invalidateDraft();
  await setFieldsValue({ playerId: props.playerId });
}
async function reset() {
  if (blocked.value) return;
  await resetFields();
  invalidateDraft();
}
function showReview(command: PlayerCommand) {
  currentTime.value = Date.now();
  reviewed.value = command;
  confirmed.value = false;
  sendError.value = "";
  openModal(true);
}
function review(command: PlayerCommand) {
  if (!blocked.value) showReview(command);
}
async function prepare() {
  if (blocked.value || !operationEnabled.value) return;
  if (preparedCommand.value) {
    showReview(preparedCommand.value);
    return;
  }
  preparing.value = true;
  emit("busy-change", true);
  let command: PlayerCommand | undefined;
  try {
    const draft = buildPlayerCommandDraft(
      getFieldsValue(),
      operation.value,
      clientRequestId.value,
      catalog.value,
    );
    if (!clientRequestId.value) clientRequestId.value = newClientRequestId();
    draft.clientRequestId = clientRequestId.value;
    command = await preparePlayerCommand(draft);
    preparedCommand.value = command;
    prepareError.value = "";
  } catch (error) {
    prepareError.value = clientRequestId.value
      ? `未确认草稿结果，请求号已保留。再次点击只会获取同一草稿，不会执行命令。${errorText(error)}`
      : errorText(error);
  } finally {
    preparing.value = false;
    emit("busy-change", false);
  }
  if (command) {
    showReview(command);
    await loadAudit();
  }
}
async function send() {
  currentTime.value = Date.now();
  const command = reviewed.value;
  if (!command || blocked.value || !canSend.value || !confirmed.value) return;
  sending.value = true;
  emit("busy-change", true);
  confirmed.value = false;
  sendError.value = "";
  attemptedIds.value.add(command.id);
  try {
    const result = await sendPlayerCommand(command.id);
    reviewed.value = result;
    if (preparedCommand.value?.id === result.id) preparedCommand.value = result;
    if (result.status === "EXECUTED")
      createMessage.success("场景服已确认执行完成。");
  } catch (error) {
    sendError.value = `服务器未确认执行结果。此命令禁止再次发送，请刷新审计并核对玩家状态。${errorText(error)}`;
  } finally {
    sending.value = false;
    emit("busy-change", false);
  }
  await loadAudit();
}
async function loadAudit() {
  if (auditLoading.value || !hasPermission("gm:audit:read")) return;
  auditLoading.value = true;
  try {
    const page = await getPlayerCommands();
    records.value = page.records;
    total.value = page.total;
    auditError.value = "";
    const updated = page.records.find(
      (record) => record.id === reviewed.value?.id,
    );
    if (updated) {
      if (reviewed.value?.status !== updated.status) confirmed.value = false;
      reviewed.value = updated;
      if (preparedCommand.value?.id === updated.id)
        preparedCommand.value = updated;
      if (updated.status === "EXECUTED" || updated.status === "REJECTED")
        sendError.value = "";
    }
  } catch (error) {
    auditError.value = `审计刷新失败，当前可能是旧记录。${errorText(error)}`;
  } finally {
    auditLoading.value = false;
  }
}
let clockTimer: ReturnType<typeof setInterval> | undefined;
onMounted(() => {
  clockTimer = setInterval(() => {
    currentTime.value = Date.now();
  }, 1000);
  void loadCatalog();
  void loadAudit();
});
onUnmounted(() => {
  if (clockTimer !== undefined) clearInterval(clockTimer);
});
</script>

<style lang="less" scoped>
.player-gm {
  margin-bottom: 16px;
}
.operation-select {
  display: block;
  width: 100%;
  margin-top: 8px;
}
.space {
  margin-top: 12px;
}
.hint {
  color: #64748b;
}
.identifier {
  font-family: ui-monospace, monospace;
  overflow-wrap: anywhere;
}
.actions,
.audit-heading {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
}
.actions {
  margin-top: 12px;
}
.audit {
  margin-top: 24px;
}
.audit-heading {
  justify-content: space-between;
  margin-bottom: 12px;
}
.audit-heading h3 {
  margin: 0;
}
.json {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  max-height: 340px;
  overflow: auto;
}
</style>
