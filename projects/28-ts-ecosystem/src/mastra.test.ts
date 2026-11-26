import { InMemoryStore } from '@mastra/core/storage';
import { beforeEach, describe, expect, it } from 'vitest';
import { buildMastra, stepRuns } from './mastra.js';
import { ScriptedModel, call, say } from './script.js';
import { Shop } from './shop.js';

const toolIds = (n: number) => Array.from({ length: n }, (_, i) => call(`c${i}`, 'lookup_order', { orderId: 'A1' }));

describe('Mastra · Agent', () => {
  it('同一个假模型直接能用：工具循环、system 消息、工具结果和 AI SDK 一样', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([call('c1', 'lookup_order', { orderId: 'A1' }), say('订单 A1 金额 120 元')]);
    const agent = buildMastra(model, shop, new InMemoryStore()).getAgent('support');
    const r = await agent.generate('查一下 A1');

    expect(r.text).toBe('订单 A1 金额 120 元');
    expect(model.callCount).toBe(2);
    expect(model.promptAt(1).map((m) => m.role)).toEqual(['system', 'user', 'assistant', 'tool']);
    expect(model.lastMessageAt(1)).toMatchObject({
      content: [{ type: 'tool-result', output: { type: 'json', value: { orderId: 'A1', amount: 120 } } }],
    });
  });

  it('默认 maxSteps 是 5：模型调用 5 次后停下，不抛错，text 为空', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([...toolIds(8), say('不会用到')]);
    const r = await buildMastra(model, shop, new InMemoryStore()).getAgent('support').generate('一直查');

    expect(model.callCount).toBe(5);
    expect(shop.lookups).toHaveLength(5);
    expect(r.finishReason).toBe('tool-calls');
    expect(r.text).toBe('');
  });

  it('工具抛错、工具名不存在、参数不合 schema：都交给模型，参数错误会带上字段名', async () => {
    const shop = new Shop();
    const model = new ScriptedModel([
      call('c1', 'lookup_order', { orderId: 'Z9' }),
      call('c2', 'cancel_order', { orderId: 'A1' }),
      call('c3', 'lookup_order', { orderId: 9 }),
      say('转人工'),
    ]);
    const r = await buildMastra(model, shop, new InMemoryStore()).getAgent('support').generate('处理');

    expect(r.text).toBe('转人工');
    expect(model.lastMessageAt(1)).toMatchObject({ content: [{ output: { type: 'error-text', value: '订单 Z9 不存在' } }] });
    expect(model.lastMessageAt(2)).toMatchObject({
      content: [{ output: { type: 'error-text', value: expect.stringContaining('Available tools: lookup_order, refund, refund_checked') } }],
    });
    expect(model.lastMessageAt(3)).toMatchObject({
      content: [{ output: { type: 'json', value: { error: true, message: expect.stringContaining('orderId: Invalid input') } } }],
    });
  });
});

describe('Mastra · 人工审批', () => {
  it('requireApproval：generate 以 suspended 结束；另一个 Mastra 实例共享 storage 就能批准', async () => {
    const shop = new Shop();
    const storage = new InMemoryStore();
    const first = buildMastra(new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 800 })]), shop, storage);
    const r1 = await first.getAgent('support').generate('给 A2 退 800');

    expect(r1.finishReason).toBe('suspended');
    expect(r1.suspendPayload).toMatchObject({ toolCallId: 'c1', toolName: 'refund', args: { orderId: 'A2', amount: 800 } });
    expect(shop.refunds).toEqual([]);

    const second = new ScriptedModel([say('已退款 800 元')]);
    const r2 = await buildMastra(second, shop, storage)
      .getAgent('support')
      .approveToolCallGenerate({ runId: r1.runId!, toolCallId: 'c1' });

    expect(shop.refunds).toEqual(['A2:800']);
    expect(r2.text).toBe('已退款 800 元');
    expect(second.callCount).toBe(1);
  });

  it('拒绝时带上原因，模型看到的是这段原因而不是工具结果', async () => {
    const shop = new Shop();
    const storage = new InMemoryStore();
    const agent = buildMastra(new ScriptedModel([call('c1', 'refund', { orderId: 'A2', amount: 800 })]), shop, storage).getAgent('support');
    const r1 = await agent.generate('退 800');

    const model = new ScriptedModel([say('没批下来')]);
    await buildMastra(model, shop, storage)
      .getAgent('support')
      .declineToolCallGenerate({ runId: r1.runId!, toolCallId: 'c1', reason: '超过单笔额度' });

    expect(shop.refunds).toEqual([]);
    expect(model.lastMessageAt(0)).toMatchObject({
      content: [{ type: 'tool-result', toolName: 'refund', output: { type: 'text', value: expect.stringContaining('超过单笔额度') } }],
    });
  });

  it('工具里调 suspend()：停下时带着自己的问题，resumeGenerate 把答复交回同一个工具', async () => {
    const shop = new Shop();
    const storage = new InMemoryStore();
    const model = new ScriptedModel([call('c1', 'refund_checked', { orderId: 'A2', amount: 800 }), say('已退款')]);
    const agent = buildMastra(model, shop, storage).getAgent('support');
    const r1 = await agent.generate('退 800');

    expect(r1.finishReason).toBe('suspended');
    expect((r1.suspendPayload as { suspendPayload: unknown }).suspendPayload).toEqual({ question: '批准给 A2 退 800 吗？' });

    const r2 = await agent.resumeGenerate({ approved: true }, { runId: r1.runId! });
    expect(r2.text).toBe('已退款');
    expect(shop.refunds).toEqual(['A2:800']);
  });
});

describe('Mastra · workflow', () => {
  beforeEach(() => {
    stepRuns.approve = 0;
  });

  it('branch 只走匹配的分支，输出以步骤 id 为键', async () => {
    const shop = new Shop();
    const mastra = buildMastra(new ScriptedModel([]), shop, new InMemoryStore());
    const run = await mastra.getWorkflow('triage').createRun();
    const r = await run.start({ inputData: { id: 'T1', text: '登录不了', orderId: 'A1' } });

    expect(r.status).toBe('success');
    expect(r.status === 'success' && r.result).toEqual({ human: { result: '已转人工：T1' } });
    expect(stepRuns.approve).toBe(0);
  });

  it('步骤 suspend 后由另一个实例按 runId 恢复；恢复时步骤从头再跑一遍', async () => {
    const shop = new Shop();
    const storage = new InMemoryStore();
    const run = await buildMastra(new ScriptedModel([]), shop, storage).getWorkflow('triage').createRun();
    const r1 = await run.start({ inputData: { id: 'T2', text: '我要退款', orderId: 'A2' } });

    expect(r1.status).toBe('suspended');
    expect(r1.status === 'suspended' && r1.suspended).toEqual([['approve']]);
    expect(r1.status === 'suspended' && r1.suspendPayload).toEqual({ approve: { question: '批准给 A2 退 800 吗？' } });
    expect(shop.lookups).toEqual(['A2']);

    const again = await buildMastra(new ScriptedModel([]), shop, storage).getWorkflow('triage').createRun({ runId: run.runId });
    const r2 = await again.resume({ step: 'approve', resumeData: { approved: true } });

    expect(r2.status).toBe('success');
    expect(r2.status === 'success' && r2.result).toEqual({ approve: { result: '已向 A2 退款 800' } });
    expect(shop.refunds).toEqual(['A2:800']);
    expect(stepRuns.approve).toBe(2); // 第一次 suspend，恢复时再跑一次
    expect(shop.lookups).toEqual(['A2']); // 已完成的 classify 不重跑
  });
});
