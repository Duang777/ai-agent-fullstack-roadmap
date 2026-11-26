// Mastra 1.x：Agent + createTool、工具审批与 suspend()、workflow 的 branch 与 suspend / resume。
import { Mastra } from '@mastra/core';
import { Agent } from '@mastra/core/agent';
import type { MastraCompositeStore } from '@mastra/core/storage';
import { createTool } from '@mastra/core/tools';
import { createStep, createWorkflow } from '@mastra/core/workflows';
import { z } from 'zod';
import { APPROVAL_THRESHOLD, type Shop } from './shop.js';

export function mastraTools(shop: Shop) {
  return {
    lookup_order: createTool({
      id: 'lookup_order',
      description: '按订单号查询订单金额',
      inputSchema: z.object({ orderId: z.string() }),
      execute: async ({ orderId }) => shop.lookup(orderId),
    }),
    // requireApproval: true 表示每次调用都先停下等批准，在 execute 之前。
    refund: createTool({
      id: 'refund',
      description: '给订单退款（每次都要人工批准）',
      inputSchema: z.object({ orderId: z.string(), amount: z.number() }),
      requireApproval: true,
      execute: async ({ orderId, amount }) => shop.refund(orderId, amount),
    }),
    // 另一种做法：工具自己判断要不要停，停的时候带上要问的话，恢复时拿到 resumeData。
    refund_checked: createTool({
      id: 'refund_checked',
      description: '给订单退款，大额时先问人',
      inputSchema: z.object({ orderId: z.string(), amount: z.number() }),
      suspendSchema: z.object({ question: z.string() }),
      resumeSchema: z.object({ approved: z.boolean() }),
      execute: async ({ orderId, amount }, ctx) => {
        const decision = ctx?.agent?.resumeData as { approved: boolean } | undefined;
        if (amount > APPROVAL_THRESHOLD && decision === undefined) {
          // suspend 不会抛出，必须 return，否则后面的代码会接着执行
          return (await ctx!.agent!.suspend({ question: `批准给 ${orderId} 退 ${amount} 吗？` })) as never;
        }
        if (decision && !decision.approved) return '退款被拒绝';
        return shop.refund(orderId, amount);
      },
    }),
  };
}

/** Mastra 接受的模型配置：字符串 'openai/xxx'，或者任何 AI SDK 规范的模型对象。 */
export type MastraModel = ConstructorParameters<typeof Agent>[0]['model'];

export type MastraAgentOptions = { maxSteps?: number };

/**
 * 审批要靠 storage 里的快照恢复，所以 agent 必须注册到带 storage 的 Mastra 实例上，
 * 再用 mastra.getAgent 取回来用。传同一个 storage 就能在另一个实例上恢复。
 */
export function buildMastra(model: MastraModel, shop: Shop, storage: MastraCompositeStore, opts: MastraAgentOptions = {}) {
  const support = new Agent({
    id: 'support',
    name: 'support',
    instructions: '你是客服 agent，需要时调用工具。',
    model,
    tools: mastraTools(shop),
    ...(opts.maxSteps ? { defaultOptions: { maxSteps: opts.maxSteps } } : {}),
  });
  return new Mastra({ agents: { support }, workflows: { triage: triageWorkflow(shop) }, storage, logger: false });
}

// ---------------- workflow ----------------

export const Ticket = z.object({ id: z.string(), text: z.string(), orderId: z.string() });
const Classified = Ticket.extend({ kind: z.enum(['refund', 'other']), amount: z.number() });
export const TriageOutput = z.object({ result: z.string() });

/** 记录 approve 步骤的 execute 被调用了几次，用来观察恢复时步骤会重跑。 */
export const stepRuns = { approve: 0 };

export function triageWorkflow(shop: Shop) {
  const classify = createStep({
    id: 'classify',
    inputSchema: Ticket,
    outputSchema: Classified,
    execute: async ({ inputData }) => {
      const kind = inputData.text.includes('退款') ? ('refund' as const) : ('other' as const);
      const amount = kind === 'refund' ? (await shop.lookup(inputData.orderId)).amount : 0;
      return { ...inputData, kind, amount };
    },
  });

  const approve = createStep({
    id: 'approve',
    inputSchema: Classified,
    outputSchema: TriageOutput,
    suspendSchema: z.object({ question: z.string() }),
    resumeSchema: z.object({ approved: z.boolean() }),
    execute: async ({ inputData, resumeData, suspend }) => {
      stepRuns.approve++;
      if (inputData.amount > APPROVAL_THRESHOLD && resumeData === undefined) {
        return await suspend({ question: `批准给 ${inputData.orderId} 退 ${inputData.amount} 吗？` });
      }
      if (resumeData && !resumeData.approved) return { result: 'rejected' };
      return { result: shop.refund(inputData.orderId, inputData.amount) };
    },
  });

  const human = createStep({
    id: 'human',
    inputSchema: Classified,
    outputSchema: TriageOutput,
    execute: async ({ inputData }) => ({ result: `已转人工：${inputData.id}` }),
  });

  return createWorkflow({ id: 'triage', inputSchema: Ticket, outputSchema: TriageOutput })
    .then(classify)
    .branch([
      [async ({ inputData }) => inputData.kind === 'refund', approve],
      [async ({ inputData }) => inputData.kind !== 'refund', human],
    ])
    .commit();
}
