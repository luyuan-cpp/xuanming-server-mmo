import { defHttp } from '/@/utils/http/axios';
import type { GmAuditEntry } from '/@/api/gm/console';

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

/** 运营处置的下游(后端 GmOpsController.TARGETS);权限串是 gm:<target>:read / prepare / approve。 */
export const OPS_TARGETS = [
  { value: 'mall', label: '商城' },
  { value: 'account', label: '账号' },
  { value: 'recall', label: '回收' },
  { value: 'inbox', label: '收件箱' },
  { value: 'rank', label: '榜单' },
  { value: 'match', label: '滞留阵容' },
] as const;
export type OpsTarget = (typeof OPS_TARGETS)[number]['value'];

/** text 原样提交;integer 提交为数字;json 解析后提交(如 SKU 条款)。 */
export interface OpsField {
  name: string;
  label: string;
  kind: 'text' | 'integer' | 'json';
  required?: boolean;
  options?: { value: string; label: string }[];
}

export interface OpsCommandSpec {
  target: OpsTarget;
  command: string;
  label: string;
  fields: OpsField[];
  /** 有不进流水的预览(演练 / 试算):发起前拿计划摘要,批准人核对现状。 */
  preview?: boolean;
}

const actions = (...values: string[]) => values.map((value) => ({ value, label: value }));

/**
 * 可发起的命令与表单字段,与后端目录(GmOpsController.COMMANDS)一一对应;原因由服务端填进需要它的命令,这里不重复填。
 * 取值规则由下游判定,页面只做形状提示。
 */
export const OPS_COMMANDS: OpsCommandSpec[] = [
  {
    target: 'mall',
    command: 'manual/orders/resolve',
    label: '处置人工订单',
    fields: [
      { name: 'orderId', label: '订单号', kind: 'text', required: true },
      { name: 'action', label: '动作', kind: 'text', required: true, options: actions('RETRY_REVERSAL', 'MARK_REVERSED', 'MARK_DELIVERED', 'CREATE_REFUND') },
    ],
  },
  {
    target: 'mall',
    command: 'manual/refunds/resolve',
    label: '处置人工退款',
    fields: [
      { name: 'orderId', label: '订单号', kind: 'text', required: true },
      { name: 'channel', label: '支付渠道', kind: 'text', required: true },
      { name: 'paymentTransactionId', label: '付款流水号', kind: 'text', required: true },
      { name: 'action', label: '动作', kind: 'text', required: true, options: actions('RETRY_REFUND', 'MARK_REFUNDED', 'MARK_REVOKED') },
      { name: 'externalRefundId', label: '渠道退款流水号(仅 MARK_REFUNDED)', kind: 'text' },
    ],
  },
  {
    target: 'mall',
    command: 'manual/consignments/resolve',
    label: '处置人工寄售',
    fields: [
      { name: 'listingId', label: '寄售单号', kind: 'text', required: true },
      { name: 'action', label: '动作', kind: 'text', required: true, options: actions('RETRY_DELIVERY', 'RETURN_TO_SELLER', 'UNFREEZE_INCOME', 'FORFEIT_INCOME') },
    ],
  },
  {
    target: 'mall',
    command: 'manual/incomes/dispute',
    label: '卖家收入争议冻结 / 释放',
    fields: [
      { name: 'orderId', label: '订单号', kind: 'text', required: true },
      { name: 'action', label: '动作', kind: 'text', required: true, options: actions('FREEZE_DISPUTE', 'RELEASE_DISPUTE') },
      { name: 'amountFen', label: '金额(分)', kind: 'integer', required: true },
      { name: 'caseId', label: '争议单号', kind: 'text', required: true },
    ],
  },
  {
    target: 'mall',
    command: 'skus/create',
    label: '新建 SKU',
    fields: [
      { name: 'skuId', label: 'SKU ID', kind: 'integer', required: true },
      { name: 'terms', label: '条款(JSON)', kind: 'json', required: true },
      { name: 'stock', label: '初始库存', kind: 'integer', required: true },
    ],
  },
  {
    target: 'mall',
    command: 'skus/update',
    label: '修改 SKU 条款(含改价)',
    fields: [
      { name: 'skuId', label: 'SKU ID', kind: 'integer', required: true },
      { name: 'expectedRevision', label: '当前修订号', kind: 'integer', required: true },
      { name: 'terms', label: '条款(JSON)', kind: 'json', required: true },
    ],
  },
  {
    target: 'mall',
    command: 'skus/stock',
    label: '调整 SKU 库存',
    fields: [
      { name: 'skuId', label: 'SKU ID', kind: 'integer', required: true },
      { name: 'expectedRevision', label: '当前修订号', kind: 'integer', required: true },
      { name: 'delta', label: '增减数量', kind: 'integer', required: true },
    ],
  },
  // 下面几类的 64 位 ID 一律按十进制文本提交(protobuf JSON 收字符串),不经 JavaScript 数字。
  {
    target: 'account',
    command: 'ScheduleAccountDeletion',
    label: '安排注销账号(按角色找账号)',
    fields: [{ name: 'playerId', label: '角色 ID', kind: 'text', required: true }],
  },
  {
    target: 'account',
    command: 'RemovePlayersFromAccounts',
    label: '从账号上摘掉孤儿角色',
    preview: true,
    fields: [{ name: 'playerIds', label: '角色 ID 列表(JSON 数组,元素是十进制文本)', kind: 'json', required: true }],
  },
  {
    target: 'account',
    command: 'ResetAccountRealName',
    label: '重置账号实名',
    fields: [
      { name: 'playerId', label: '角色 ID', kind: 'text', required: true },
      { name: 'account', label: '账号', kind: 'text', required: true },
    ],
  },
  {
    target: 'recall',
    command: 'BatchRecallItems',
    label: '批量回收(先演练拿计划摘要)',
    preview: true,
    fields: [
      { name: 'playerIds', label: '角色 ID 列表(JSON 数组)', kind: 'json', required: true },
      { name: 'itemConfigId', label: '物品配置 ID', kind: 'integer' },
      { name: 'currencyType', label: '货币种类', kind: 'integer' },
      { name: 'timeStart', label: '起始时间(毫秒)', kind: 'text' },
      { name: 'timeEnd', label: '结束时间(毫秒)', kind: 'text' },
      { name: 'txTypes', label: '流水类型(JSON 数组)', kind: 'json' },
      { name: 'recallKind', label: '回收种类', kind: 'text' },
      { name: 'expectedPlanDigest', label: '演练给出的计划摘要(执行时必填)', kind: 'text' },
    ],
  },
  {
    target: 'recall',
    command: 'CancelRecallBatch',
    label: '撤回回收批次',
    fields: [{ name: 'requestId', label: '批次号', kind: 'text', required: true }],
  },
  {
    target: 'recall',
    command: 'ResolveRecallOp',
    label: '人工终结回收行',
    fields: [
      { name: 'opId', label: '回收行 ID', kind: 'text', required: true },
      { name: 'finalStatus', label: '终局状态', kind: 'integer', required: true },
    ],
  },
  {
    target: 'recall',
    command: 'RegisterRecallReviewHandled',
    label: '登记待核对指令已人工处置(以后的回收跳过这些流水,撤不回)',
    fields: [
      { name: 'recallKind', label: '回收种类', kind: 'text', required: true },
      { name: 'itemConfigId', label: '物品配置 ID', kind: 'integer' },
      { name: 'currencyType', label: '货币种类', kind: 'integer' },
      { name: 'playerId', label: '角色 ID', kind: 'text', required: true },
      { name: 'correlationId', label: '指令号', kind: 'text', required: true },
      { name: 'txIds', label: '流水号列表(JSON 数组,元素是十进制文本)', kind: 'json', required: true },
      { name: 'reviewReason', label: '待核对原因', kind: 'text', required: true },
    ],
  },
  {
    target: 'inbox',
    command: 'GmSanctionPlayer',
    label: '玩家处罚(封获取 / 解封 / 挂债,填其中一项)',
    fields: [
      { name: 'playerId', label: '角色 ID', kind: 'text', required: true },
      { name: 'blockGain', label: '封获取(JSON)', kind: 'json' },
      { name: 'unblockGain', label: '解封获取(JSON)', kind: 'json' },
      { name: 'attachDebt', label: '挂债(JSON)', kind: 'json' },
      { name: 'blockItemGain', label: '封物品获取(JSON)', kind: 'json' },
      { name: 'unblockItemGain', label: '解封物品获取(JSON)', kind: 'json' },
    ],
  },
  {
    target: 'inbox',
    command: 'VoidItem',
    label: '作废收件箱条目(结果未知时不能重试,要去收件箱核对)',
    fields: [
      { name: 'playerId', label: '角色 ID', kind: 'text', required: true },
      {
        name: 'domain',
        label: '域',
        kind: 'text',
        required: true,
        options: actions('PLAYER_INBOX_DOMAIN_SCENE_GM_SANCTION', 'PLAYER_INBOX_DOMAIN_SCENE_BATTLE_SETTLEMENT'),
      },
      { name: 'seq', label: '序号', kind: 'text', required: true },
      { name: 'reason', label: '作废原因', kind: 'text', required: true, options: actions('PLAYER_INBOX_VOID_CANCELLED', 'PLAYER_INBOX_VOID_QUARANTINED') },
      { name: 'mode', label: '模式', kind: 'text', options: actions('PLAYER_INBOX_VOID_MODE_NORMAL', 'PLAYER_INBOX_VOID_MODE_FORCE_MISMATCH') },
    ],
  },
  {
    target: 'rank',
    command: 'RemoveRankEntry',
    label: '删除榜单条目',
    fields: [
      { name: 'boardId', label: '榜 ID', kind: 'text', required: true },
      { name: 'generation', label: '代', kind: 'text', required: true },
      { name: 'entityId', label: '条目 ID', kind: 'text', required: true },
    ],
  },
  {
    target: 'match',
    command: 'ResolveStrandedFormation',
    label: '终结滞留阵容(先演练拿成局摘要;结果未知时不能重试)',
    preview: true,
    fields: [
      { name: 'battleId', label: '战斗 ID', kind: 'text', required: true },
      { name: 'expectDigest', label: '演练给出的成局摘要', kind: 'text', required: true },
      { name: 'expectTarget', label: '环境名(照工单填写)', kind: 'text', required: true },
    ],
  },
];

export type OpsStatusValue = 'DRAFT' | 'SENDING' | 'EXECUTED' | 'UNKNOWN' | 'REJECTED' | 'DECLINED';

export interface OpsStatus {
  dualControl: boolean;
  targets: Record<string, boolean>;
}

export interface OpsCommand {
  id: string;
  commandType: 'OPS';
  status: OpsStatusValue;
  operator: string;
  approver?: string | null;
  approvedAt?: string | null;
  reason: string;
  request: { target?: string; command?: string; requestId?: string; body?: Record<string, unknown> };
  result: Record<string, unknown> | null;
  createdAt: string;
  updatedAt: string;
}

export interface OpsDraft {
  clientRequestId: string;
  target: OpsTarget;
  command: string;
  reason: string;
  body: Record<string, unknown>;
}

export function getOpsStatus(): Promise<OpsStatus> {
  return defHttp.get<OpsStatus>({ url: '/gm/ops/status' }, requestOptions);
}

export function listMallManual(limit = 50): Promise<Record<string, unknown>> {
  return defHttp.get<Record<string, unknown>>({ url: '/gm/ops/mall/manual', params: { limit } }, requestOptions);
}

export function getMallOrder(orderId: string): Promise<Record<string, unknown>> {
  return defHttp.get<Record<string, unknown>>({ url: `/gm/ops/mall/orders/${encodeURIComponent(orderId)}` }, requestOptions);
}

export function listMallSkus(afterSkuId = 0, limit = 50): Promise<{ skus?: Record<string, unknown>[] }> {
  return defHttp.get<{ skus?: Record<string, unknown>[] }>({ url: '/gm/ops/mall/skus', params: { afterSkuId, limit } }, requestOptions);
}

/** 预览:直接调下游的演练 / 试算,不进流水;返回下游回包原样。 */
export function previewOps(target: OpsTarget, command: string, body: Record<string, unknown>): Promise<Record<string, unknown>> {
  return defHttp.post<Record<string, unknown>>({ url: '/gm/ops/preview', data: { target, command, body } }, requestOptions);
}

export function prepareOps(draft: OpsDraft): Promise<OpsCommand> {
  return defHttp.post<OpsCommand>({ url: '/gm/ops/drafts', data: draft }, requestOptions);
}

export function sendOps(commandId: string): Promise<OpsCommand> {
  return defHttp.post<OpsCommand>({ url: `/gm/ops/commands/${encodeURIComponent(commandId)}/send` }, requestOptions);
}

export function listOpsCommands(page: number, pageSize: number, status?: string): Promise<{ records: OpsCommand[]; total: number }> {
  return defHttp.get<{ records: OpsCommand[]; total: number }>({ url: '/gm/ops/commands', params: { page, pageSize, status } }, requestOptions);
}

export function getOpsCommand(commandId: string): Promise<OpsCommand> {
  return defHttp.get<OpsCommand>({ url: `/gm/ops/commands/${encodeURIComponent(commandId)}` }, requestOptions);
}

export async function getOpsAudit(commandId: string): Promise<GmAuditEntry[]> {
  const result = await defHttp.get<{ records: GmAuditEntry[] }>({ url: `/gm/ops/commands/${encodeURIComponent(commandId)}/audit` }, requestOptions);
  return Array.isArray(result?.records) ? result.records : [];
}
