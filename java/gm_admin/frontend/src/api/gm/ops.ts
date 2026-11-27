import { defHttp } from '/@/utils/http/axios';
import type { GmAuditEntry } from '/@/api/gm/console';

const requestOptions = { errorMessageMode: 'none', successMessageMode: 'none' } as const;

/** 运营处置的下游(后端 GmOpsController.TARGETS);权限串是 gm:<target>:read / prepare / approve。 */
export const OPS_TARGETS = [{ value: 'mall', label: '商城' }] as const;
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
