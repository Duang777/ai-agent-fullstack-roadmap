// Vercel AI SDK 7：ToolLoopAgent、工具、toolApproval。
import {
  ToolLoopAgent,
  isStepCount,
  tool,
  type LanguageModel,
  type ModelMessage,
  type StopCondition,
  type ToolApprovalResponse,
} from 'ai';
import { z } from 'zod';
import { APPROVAL_THRESHOLD, type Shop } from './shop.js';

export function shopTools(shop: Shop) {
  return {
    lookup_order: tool({
      description: '按订单号查询订单金额',
      inputSchema: z.object({ orderId: z.string().describe('订单号，例如 A1') }),
      execute: async ({ orderId }) => shop.lookup(orderId),
    }),
    refund: tool({
      description: '给订单退款',
      inputSchema: z.object({ orderId: z.string(), amount: z.number() }),
      execute: async ({ orderId, amount }) => shop.refund(orderId, amount),
    }),
  };
}

export type ShopTools = ReturnType<typeof shopTools>;

export type AgentOptions = {
  /** 不传就用 ToolLoopAgent 的默认值 isStepCount(20)。 */
  stopWhen?: StopCondition<ShopTools> | StopCondition<ShopTools>[];
  /** 传了就对 HMAC 签名审批请求，恢复时校验签名。 */
  approvalSecret?: string;
};

export function supportAgent(model: LanguageModel, shop: Shop, opts: AgentOptions = {}) {
  return new ToolLoopAgent({
    model,
    instructions: '你是客服 agent，需要时调用工具。',
    tools: shopTools(shop),
    // 审批策略放在 agent 上，不放在工具上（v7 起 tool 上的 needsApproval 已废弃）。
    // 返回 undefined 等于 'not-applicable'：照常执行。
    toolApproval: {
      refund: async ({ amount }) => (amount > APPROVAL_THRESHOLD ? 'user-approval' : undefined),
    },
    ...(opts.stopWhen ? { stopWhen: opts.stopWhen } : {}),
    ...(opts.approvalSecret ? { experimental_toolApprovalSecret: opts.approvalSecret } : {}),
  });
}

export const capped = (n: number) => isStepCount(n);

export type PendingApproval = { approvalId: string; toolName: string; input: unknown };

/** 从一次 generate 的结果里取出等人审批的工具调用（自动批准 / 拒绝的不算）。 */
export function pendingApprovals(content: readonly { type: string }[]): PendingApproval[] {
  const out: PendingApproval[] = [];
  for (const part of content) {
    if (part.type !== 'tool-approval-request') continue;
    const p = part as unknown as {
      approvalId: string;
      isAutomatic?: boolean;
      toolCall: { toolName: string; input: unknown };
    };
    if (p.isAutomatic) continue;
    out.push({ approvalId: p.approvalId, toolName: p.toolCall.toolName, input: p.toolCall.input });
  }
  return out;
}

/** 把人的决定写成一条 tool 消息，追加到对话历史里，然后再调一次 agent。 */
export function approvalMessage(
  decisions: { approvalId: string; approved: boolean; reason?: string }[],
): ModelMessage {
  const content: ToolApprovalResponse[] = decisions.map((d) => ({
    type: 'tool-approval-response',
    approvalId: d.approvalId,
    approved: d.approved,
    ...(d.reason === undefined ? {} : { reason: d.reason }),
  }));
  return { role: 'tool', content };
}
