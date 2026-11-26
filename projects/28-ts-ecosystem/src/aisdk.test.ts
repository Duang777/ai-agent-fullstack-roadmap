import { generateText, type ModelMessage } from 'ai';
import { describe, expect, it } from 'vitest';
import { approvalMessage, capped, pendingApprovals, shopTools, supportAgent } from './aisdk.js';
import { ScriptedModel, call, calls, say } from './script.js';
import { Shop } from './shop.js';

describe('Vercel AI SDK · ToolLoopAgent', () => {
  it('跑完工具循环：instructions 变成 system 消息，工具结果作为 tool 消息交回模型', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([call('c1', 'lookup_order', { orderId: 'A1' }), say('订单 A1 金额 120 元')]);
    const r = await supportAgent(model, shop).generate({ prompt: '查一下 A1' });

    expect(r.text).toBe('订单 A1 金额 120 元');
    expect(r.stoppedBy).toBe('model');
    expect(model.callCount).toBe(2);
    expect(shop.lookups).toEqual(['A1']);
    const prompt = model.promptAt(1);
    expect(prompt.map((m) => m.role)).toEqual(['system', 'user', 'assistant', 'tool']);
    expect(prompt[0]).toEqual({ role: 'system', content: '你是客服 agent，需要时调用工具。' });
    expect(model.lastMessageAt(1)).toMatchObject({
      content: [{ type: 'tool-result', output: { type: 'json', value: { orderId: 'A1', amount: 120 } } }],
    });
  });

  it('generateText 默认只走一步：工具执行了，但不会再调模型，text 为空', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([call('c1', 'lookup_order', { orderId: 'A1' }), say('不会用到')]);
    const r = await generateText({ model, prompt: '查 A1', tools: shopTools(shop) });

    expect(model.callCount).toBe(1);
    expect(shop.lookups).toEqual(['A1']);
    expect(r.text).toBe('');
    expect(r.stoppedBy).toBe('stop-condition');
  });

  it('isStepCount(2)：模型调用 2 次，第 2 步的工具也执行了，然后停下，不抛错', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([
      call('c1', 'lookup_order', { orderId: 'A1' }),
      call('c2', 'lookup_order', { orderId: 'A2' }),
      call('c3', 'lookup_order', { orderId: 'A3' }),
      say('不会用到'),
    ]);
    const r = await supportAgent(model, shop, { stopWhen: capped(2) }).generate({ prompt: '查三个订单' });

    expect(model.callCount).toBe(2);
    expect(shop.lookups).toEqual(['A1', 'A2']);
    expect(r.stoppedBy).toBe('stop-condition');
    expect(r.finishReason).toBe('tool-calls');
    expect(r.steps).toHaveLength(2);
  });

  it('工具抛错、工具名不存在、参数不合 schema：三种都变成 error-text 交给模型，循环继续', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([
      call('c1', 'lookup_order', { orderId: 'Z9' }),
      call('c2', 'cancel_order', { orderId: 'A1' }),
      call('c3', 'refund', { orderId: 'A1', amount: '120' }),
      say('处理不了，转人工'),
    ]);
    const r = await supportAgent(model, shop).generate({ prompt: '帮我处理' });

    expect(r.text).toBe('处理不了，转人工');
    expect(model.lastMessageAt(1)).toMatchObject({
      content: [{ output: { type: 'error-text', value: expect.stringContaining('订单 Z9 不存在') } }],
    });
    expect(model.lastMessageAt(2)).toMatchObject({
      content: [{ output: { type: 'error-text', value: expect.stringContaining('Available tools: lookup_order, refund') } }],
    });
    expect(model.lastMessageAt(3)).toMatchObject({
      content: [{ output: { type: 'error-text', value: expect.stringContaining('AI_InvalidToolInputError') } }],
    });
    expect(shop.refunds).toEqual([]);
  });

  it('同一步的多个工具调用默认并发执行', async () => {
    const shop = new Shop(undefined, 20);
    const model = new ScriptedModel([
      calls(
        { id: 'c1', name: 'lookup_order', input: { orderId: 'A1' } },
        { id: 'c2', name: 'lookup_order', input: { orderId: 'A3' } },
      ),
      say('两个都查到了'),
    ]);
    const r = await supportAgent(model, shop).generate({ prompt: '查 A1 和 A3' });
    expect(r.text).toBe('两个都查到了');
    expect(shop.lookups.sort()).toEqual(['A1', 'A3']);
    expect(shop.maxInFlight).toBe(2);
    expect(model.lastMessageAt(1)).toMatchObject({ role: 'tool' });
    expect((model.lastMessageAt(1)!.content as unknown[]).length).toBe(2);
  });

  it('stream：事件按步排列，textStream 只给文字', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([call('c1', 'lookup_order', { orderId: 'A1' }), say('金额120')]);
    const r = await supportAgent(model, shop).stream({ prompt: '查 A1' });
    const types: string[] = [];
    for await (const part of r.stream) types.push(part.type);

    expect(types.filter((t) => !t.startsWith('text-delta'))).toEqual([
      'start',
      'start-step',
      'tool-call',
      'tool-result',
      'finish-step',
      'start-step',
      'text-start',
      'text-end',
      'finish-step',
      'finish',
    ]);
    expect(types.filter((t) => t === 'text-delta')).toHaveLength(5);
    expect(await r.text).toBe('金额120');
    expect(model.doStreamCalls).toHaveLength(2);
  });
});

describe('Vercel AI SDK · toolApproval', () => {
  it('大额退款停在审批，批准后由另一个 agent 实例接着跑：状态就是消息历史', async () => {
    const shop = new Shop();
    const messages: ModelMessage[] = [{ role: 'user', content: '给 A2 退 800' }];
    const first = new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 800 })]);
    const r1 = await supportAgent(first, shop).generate({ messages });

    expect(r1.stoppedBy).toBe('approval');
    expect(shop.refunds).toEqual([]);
    const pending = pendingApprovals(r1.content);
    expect(pending).toMatchObject([{ toolName: 'refund', input: { orderId: 'A2', amount: 800 } }]);

    // 只存 messages 就够了；下次请求可能落在另一台机器上。
    messages.push(...r1.responseMessages, approvalMessage([{ approvalId: pending[0]!.approvalId, approved: true }]));
    const second = new ScriptedModel([say('已经给 A2 退款 800 元')]);
    const r2 = await supportAgent(second, shop).generate({ messages: JSON.parse(JSON.stringify(messages)) });

    expect(shop.refunds).toEqual(['A2:800']);
    expect(r2.text).toBe('已经给 A2 退款 800 元');
    expect(second.callCount).toBe(1); // 工具先执行，再调一次模型
  });

  it('拒绝时工具不执行，模型看到 execution-denied 和原因；小额退款不走审批', async () => {
    const shop = new Shop();
    const r1 = await supportAgent(new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 800 })]), shop).generate({
      prompt: '退 800',
    });
    const [p] = pendingApprovals(r1.content);
    const messages: ModelMessage[] = [
      { role: 'user', content: '退 800' },
      ...r1.responseMessages,
      approvalMessage([{ approvalId: p!.approvalId, approved: false, reason: '超过单笔额度' }]),
    ];
    const model = new ScriptedModel([say('这笔退款没有通过审批')]);
    await supportAgent(model, shop).generate({ messages });
    expect(shop.refunds).toEqual([]);
    expect(model.lastMessageAt(0)).toMatchObject({
      content: [{ type: 'tool-result', output: { type: 'execution-denied', reason: '超过单笔额度' } }],
    });

    const small = await supportAgent(new ScriptedModel([call('c1', 'refund', { orderId: 'A3', amount: 60 }), say('已退')]), shop).generate({
      prompt: '退 60',
    });
    expect(small.stoppedBy).toBe('model');
    expect(shop.refunds).toEqual(['A3:60']);
  });

  it('历史由客户端保存时可能被篡改：不签名会照改过的参数执行，签名后恢复时报错', async () => {
    const run = async (secret?: string) => {
      const shop = new Shop();
      const opts = secret ? { approvalSecret: secret } : {};
      const r1 = await supportAgent(new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 800 })]), shop, opts).generate({
        prompt: '退 800',
      });
      const [p] = pendingApprovals(r1.content);
      // 审批人看到的是 A2，客户端把历史里的订单号改成了 B9
      const tampered = JSON.parse(JSON.stringify(r1.responseMessages).replaceAll('"A2"', '"B9"'));
      const messages: ModelMessage[] = [
        { role: 'user', content: '退 800' },
        ...tampered,
        approvalMessage([{ approvalId: p!.approvalId, approved: true }]),
      ];
      const r2 = supportAgent(new ScriptedModel([say('好了')]), shop, opts).generate({ messages });
      return { shop, r2 };
    };

    const unsigned = await run();
    await unsigned.r2;
    expect(unsigned.shop.refunds).toEqual(['B9:800']);

    const signed = await run('x'.repeat(32));
    await expect(signed.r2).rejects.toThrow(/signature verification failed/);
    expect(signed.shop.refunds).toEqual([]);
  });
});
