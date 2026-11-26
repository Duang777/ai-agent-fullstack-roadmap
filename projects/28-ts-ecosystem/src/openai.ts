// OpenAI Agents SDK (JS) 0.22：Agent、run、needsApproval 与 RunState 序列化。
import {
  Agent,
  RunState,
  Usage,
  run,
  setTracingDisabled,
  tool,
  type Model,
  type ModelRequest,
  type ModelResponse,
} from '@openai/agents';
import { z } from 'zod';
import { APPROVAL_THRESHOLD, type Shop } from './shop.js';

// 测试里不上传 trace。生产环境默认会把 trace 发到 OpenAI 后台。
setTracingDisabled(true);

export type OpenAITurn = { text: string } | { calls: { id: string; name: string; args: unknown }[] };
export const reply = (text: string): OpenAITurn => ({ text });
export const fnCall = (id: string, name: string, args: unknown): OpenAITurn => ({ calls: [{ id, name, args }] });

/** 实现 Agents SDK 的 Model 接口：按脚本返回 Responses 风格的输出项。 */
export class ScriptedOpenAIModel implements Model {
  readonly requests: ModelRequest[] = [];
  private i = 0;

  constructor(private readonly turns: OpenAITurn[]) {}

  async getResponse(request: ModelRequest): Promise<ModelResponse> {
    this.requests.push(request);
    const t = this.turns[this.i++];
    if (!t) throw new Error(`script exhausted at model call ${this.i}`);
    const output =
      'text' in t
        ? [
            {
              type: 'message' as const,
              role: 'assistant' as const,
              status: 'completed' as const,
              content: [{ type: 'output_text' as const, text: t.text }],
            },
          ]
        : t.calls.map((c) => ({
            type: 'function_call' as const,
            callId: c.id,
            name: c.name,
            arguments: JSON.stringify(c.args),
            status: 'completed' as const,
          }));
    return { usage: new Usage(), output };
  }

  // eslint-disable-next-line require-yield
  async *getStreamedResponse(): AsyncIterable<never> {
    throw new Error('ScriptedOpenAIModel does not stream');
  }

  /** 第 n 次调用时 input 的最后一项。 */
  lastInputAt(n: number): unknown {
    const input = this.requests[n]?.input;
    if (!Array.isArray(input)) throw new Error(`no array input for call #${n}`);
    return input.at(-1);
  }
}

export function openaiTools(shop: Shop) {
  return [
    tool({
      name: 'lookup_order',
      description: '按订单号查询订单金额',
      parameters: z.object({ orderId: z.string() }),
      execute: async ({ orderId }) => shop.lookup(orderId),
    }),
    tool({
      name: 'refund',
      description: '给订单退款',
      parameters: z.object({ orderId: z.string(), amount: z.number() }),
      // 审批条件写在工具上，拿得到解析后的参数
      needsApproval: async (_ctx, { amount }) => amount > APPROVAL_THRESHOLD,
      execute: async ({ orderId, amount }) => shop.refund(orderId, amount),
    }),
  ];
}

export function supportAgent(model: Model, shop: Shop) {
  return new Agent({
    name: 'support',
    instructions: '你是客服 agent，需要时调用工具。',
    model,
    tools: openaiTools(shop),
  });
}

/** 中断后把运行状态存成字符串；恢复时用“同样定义的”agent 读回来。 */
export async function restore(agent: Agent<any, any>, saved: string) {
  return RunState.fromString(agent, saved);
}

export { run };
