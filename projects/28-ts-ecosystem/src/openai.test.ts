import { MaxTurnsExceededError, ModelBehaviorError } from '@openai/agents';
import { describe, expect, it } from 'vitest';
import { ScriptedOpenAIModel, fnCall, reply, restore, run, supportAgent } from './openai.js';
import { Shop } from './shop.js';

describe('OpenAI Agents SDK (JS) · run', () => {
  it('跑完工具循环：instructions 单独放在 systemInstructions，input 是 Responses 风格的输出项', async () => {
    const shop = new Shop();
    const model = new ScriptedOpenAIModel([fnCall('c1', 'lookup_order', { orderId: 'A1' }), reply('订单 A1 金额 120 元')]);
    const r = await run(supportAgent(model, shop), '查一下 A1');

    expect(r.finalOutput).toBe('订单 A1 金额 120 元');
    expect(model.requests).toHaveLength(2);
    expect(model.requests[1]!.systemInstructions).toBe('你是客服 agent，需要时调用工具。');
    expect((model.requests[1]!.input as { type: string }[]).map((i) => i.type)).toEqual([
      'message',
      'function_call',
      'function_call_result',
    ]);
    expect(model.lastInputAt(1)).toMatchObject({ output: { type: 'text', text: '{"orderId":"A1","amount":120}' } });
  });

  it('maxTurns 默认 10；设成 2 时模型调用 2 次、两个工具都执行，然后抛 MaxTurnsExceededError', async () => {
    const shop = new Shop();
    const model = new ScriptedOpenAIModel([
      fnCall('c1', 'lookup_order', { orderId: 'A1' }),
      fnCall('c2', 'lookup_order', { orderId: 'A2' }),
      fnCall('c3', 'lookup_order', { orderId: 'A3' }),
      reply('不会用到'),
    ]);
    await expect(run(supportAgent(model, shop), '查三个', { maxTurns: 2 })).rejects.toBeInstanceOf(MaxTurnsExceededError);
    expect(model.requests).toHaveLength(2);
    expect(shop.lookups).toEqual(['A1', 'A2']);
  });

  it('工具抛错和参数不合 schema：模型只看到一句通用错误，看不到原因', async () => {
    const shop = new Shop();
    const model = new ScriptedOpenAIModel([
      fnCall('c1', 'lookup_order', { orderId: 'Z9' }),
      fnCall('c2', 'lookup_order', { orderId: 9 }),
      reply('转人工'),
    ]);
    const r = await run(supportAgent(model, shop), '处理');
    expect(r.finalOutput).toBe('转人工');
    const generic = { output: { type: 'text', text: 'An error occurred while running the tool.' } };
    expect(model.lastInputAt(1)).toMatchObject(generic);
    expect(model.lastInputAt(2)).toMatchObject(generic);
  });

  it('工具名不存在默认抛 ModelBehaviorError；toolNotFoundBehavior 可改成交给模型', async () => {
    const shop = new Shop();
    const strict = new ScriptedOpenAIModel([fnCall('c1', 'cancel_order', {}), reply('x')]);
    await expect(run(supportAgent(strict, shop), '取消')).rejects.toBeInstanceOf(ModelBehaviorError);

    const lenient = new ScriptedOpenAIModel([fnCall('c1', 'cancel_order', {}), reply('没有这个功能')]);
    const r = await run(supportAgent(lenient, shop), '取消', { toolNotFoundBehavior: 'return_error_to_model' });
    expect(r.finalOutput).toBe('没有这个功能');
    expect(lenient.lastInputAt(1)).toMatchObject({ output: { type: 'text', text: "Tool 'cancel_order' not found." } });
  });
});

describe('OpenAI Agents SDK (JS) · needsApproval 与 RunState', () => {
  it('大额退款中断；状态存成字符串，新的 agent 实例读回来批准后继续', async () => {
    const shop = new Shop();
    const r1 = await run(supportAgent(new ScriptedOpenAIModel([fnCall('c1', 'refund', { orderId: 'A2', amount: 800 })]), shop), '给 A2 退 800');

    expect(r1.interruptions).toHaveLength(1);
    expect(r1.interruptions[0]).toMatchObject({ type: 'tool_approval_item', name: 'refund', arguments: '{"orderId":"A2","amount":800}' });
    expect(shop.refunds).toEqual([]);
    const saved = r1.state.toString(); // 存进数据库

    const model2 = new ScriptedOpenAIModel([reply('已退款 800 元')]);
    const agent2 = supportAgent(model2, shop);
    const state = await restore(agent2, saved);
    state.approve(state.getInterruptions()[0]!);
    const r2 = await run(agent2, state);

    expect(r2.finalOutput).toBe('已退款 800 元');
    expect(shop.refunds).toEqual(['A2:800']);
    expect(model2.requests).toHaveLength(1);
  });

  it('拒绝时工具不执行，模型看到固定的 “not approved”；小额退款不中断', async () => {
    const shop = new Shop();
    const model = new ScriptedOpenAIModel([fnCall('c1', 'refund', { orderId: 'A2', amount: 800 }), reply('没批下来')]);
    const agent = supportAgent(model, shop);
    const r1 = await run(agent, '退 800');
    r1.state.reject(r1.interruptions[0]!);
    const r2 = await run(agent, r1.state);

    expect(r2.finalOutput).toBe('没批下来');
    expect(shop.refunds).toEqual([]);
    expect(model.lastInputAt(1)).toMatchObject({ output: { type: 'text', text: 'Tool execution was not approved.' } });

    const small = await run(supportAgent(new ScriptedOpenAIModel([fnCall('c1', 'refund', { orderId: 'A3', amount: 60 }), reply('已退')]), shop), '退 60');
    expect(small.interruptions).toHaveLength(0);
    expect(shop.refunds).toEqual(['A3:60']);
  });
});
