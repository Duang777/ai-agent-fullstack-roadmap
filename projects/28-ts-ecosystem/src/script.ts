// 按脚本回复的假模型，实现 AI SDK 的 LanguageModelV4 规范。
// Vercel AI SDK 和 Mastra 都直接接受这个对象：Mastra 的 Agent 底层用的就是 AI SDK 的模型接口。
import { simulateReadableStream } from 'ai';
import { MockLanguageModelV4 } from 'ai/test';

export type ToolCallSpec = { id: string; name: string; input: unknown };
export type Turn = { kind: 'text'; text: string } | { kind: 'tools'; calls: ToolCallSpec[] };

export const say = (text: string): Turn => ({ kind: 'text', text });
export const call = (id: string, name: string, input: unknown): Turn => ({
  kind: 'tools',
  calls: [{ id, name, input }],
});
export const calls = (...specs: ToolCallSpec[]): Turn => ({ kind: 'tools', calls: specs });

const usage = {
  inputTokens: { total: 10, noCache: 10, cacheRead: undefined, cacheWrite: undefined },
  outputTokens: { total: 5, text: 5, reasoning: undefined },
};

function contentOf(t: Turn) {
  if (t.kind === 'text') return [{ type: 'text' as const, text: t.text }];
  return t.calls.map((c) => ({
    type: 'tool-call' as const,
    toolCallId: c.id,
    toolName: c.name,
    input: JSON.stringify(c.input),
  }));
}

function finishOf(t: Turn) {
  return { unified: t.kind === 'text' ? ('stop' as const) : ('tool-calls' as const), raw: undefined };
}

export class ScriptedModel extends MockLanguageModelV4 {
  constructor(turns: Turn[]) {
    let i = 0;
    const next = (): Turn => {
      const t = turns[i++];
      if (!t) throw new Error(`script exhausted at model call ${i}`);
      return t;
    };
    // 回调交给父类构造函数，父类会把每次调用的参数记进 doGenerateCalls / doStreamCalls。
    super({
      provider: 'script',
      modelId: 'scripted',
      doGenerate: async () => {
        const t = next();
        return { content: contentOf(t), finishReason: finishOf(t), usage, warnings: [] };
      },
      doStream: async () => {
        const t = next();
        const chunks: any[] = [];
        if (t.kind === 'text') {
          chunks.push({ type: 'text-start', id: 't' });
          for (const ch of Array.from(t.text)) chunks.push({ type: 'text-delta', id: 't', delta: ch });
          chunks.push({ type: 'text-end', id: 't' });
        } else {
          chunks.push(...contentOf(t));
        }
        chunks.push({ type: 'finish', finishReason: finishOf(t), usage });
        return { stream: simulateReadableStream({ chunks, initialDelayInMs: 0, chunkDelayInMs: 0 }) };
      },
    });
  }

  /** 模型被调用的总次数（generate 和 stream 都算）。 */
  get callCount(): number {
    return this.doGenerateCalls.length + this.doStreamCalls.length;
  }

  /** 第 n 次调用（从 0 开始）时模型收到的 prompt。 */
  promptAt(n: number) {
    const all = [...this.doGenerateCalls, ...this.doStreamCalls];
    const p = all[n]?.prompt;
    if (!p) throw new Error(`no model call #${n}`);
    return p;
  }

  /** 第 n 次调用时 prompt 的最后一条消息，通常是上一轮的工具结果。 */
  lastMessageAt(n: number) {
    return this.promptAt(n).at(-1);
  }
}
