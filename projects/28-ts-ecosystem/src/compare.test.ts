import { MaxTurnsExceededError } from '@openai/agents';
import { InMemoryStore } from '@mastra/core/storage';
import { describe, expect, it } from 'vitest';
import { capped, supportAgent as aiAgent } from './aisdk.js';
import { buildMastra } from './mastra.js';
import { ScriptedOpenAIModel, fnCall, reply, run, supportAgent as openaiAgent } from './openai.js';
import { ScriptedModel, call, say } from './script.js';
import { Shop } from './shop.js';

// 同一个脚本：模型一直要工具。三个框架都把上限设成 2。
const greedy = () => [...['A1', 'A2', 'A3'].map((id, i) => call(`c${i}`, 'lookup_order', { orderId: id })), say('完')];

describe('同一个上限，三个框架', () => {
  it('上限 2：三家都是模型调用 2 次、工具执行 2 次；AI SDK 和 Mastra 静默结束，OpenAI 抛错', async () => {
    const s1 = new Shop();
    const m1 = new ScriptedModel(greedy());
    const r1 = await aiAgent(m1, s1, { stopWhen: capped(2) }).generate({ prompt: 'q' });

    const s2 = new Shop();
    const m2 = new ScriptedModel(greedy());
    const r2 = await buildMastra(m2, s2, new InMemoryStore(), { maxSteps: 2 }).getAgent('support').generate('q');

    const s3 = new Shop();
    const m3 = new ScriptedOpenAIModel([
      fnCall('c0', 'lookup_order', { orderId: 'A1' }),
      fnCall('c1', 'lookup_order', { orderId: 'A2' }),
      fnCall('c2', 'lookup_order', { orderId: 'A3' }),
      reply('完'),
    ]);
    const r3 = run(openaiAgent(m3, s3), 'q', { maxTurns: 2 });

    expect([m1.callCount, m2.callCount]).toEqual([2, 2]);
    expect([s1.lookups, s2.lookups]).toEqual([
      ['A1', 'A2'],
      ['A1', 'A2'],
    ]);
    expect([r1.text, r2.text]).toEqual(['', '']);
    expect([r1.finishReason, r2.finishReason]).toEqual(['tool-calls', 'tool-calls']);

    await expect(r3).rejects.toBeInstanceOf(MaxTurnsExceededError);
    expect(m3.requests).toHaveLength(2);
    expect(s3.lookups).toEqual(['A1', 'A2']);
  });

  it('默认上限：ToolLoopAgent 20、Mastra 5、OpenAI 10', async () => {
    const forever = (n: number) => Array.from({ length: n }, (_, i) => call(`c${i}`, 'lookup_order', { orderId: 'A1' }));

    const m1 = new ScriptedModel(forever(30));
    await aiAgent(m1, new Shop()).generate({ prompt: 'q' });

    const m2 = new ScriptedModel(forever(30));
    await buildMastra(m2, new Shop(), new InMemoryStore()).getAgent('support').generate('q');

    const m3 = new ScriptedOpenAIModel(Array.from({ length: 30 }, (_, i) => fnCall(`c${i}`, 'lookup_order', { orderId: 'A1' })));
    await expect(run(openaiAgent(m3, new Shop()), 'q')).rejects.toBeInstanceOf(MaxTurnsExceededError);

    expect([m1.callCount, m2.callCount, m3.requests.length]).toEqual([20, 5, 10]);
  });
});
