// 三个框架共用的业务：订单库和退款账本。
export type Order = { orderId: string; amount: number };

export class Shop {
  readonly refunds: string[] = [];
  readonly lookups: string[] = [];

  /** 同时在执行的 lookup 个数的峰值，用来观察框架是否并发执行工具。 */
  maxInFlight = 0;
  private inFlight = 0;

  constructor(
    private readonly orders: Record<string, number> = { A1: 120, A2: 800, A3: 60 },
    private readonly lookupDelayMs = 0,
  ) {}

  async lookup(orderId: string): Promise<Order> {
    this.lookups.push(orderId);
    this.inFlight++;
    this.maxInFlight = Math.max(this.maxInFlight, this.inFlight);
    try {
      if (this.lookupDelayMs > 0) await new Promise((r) => setTimeout(r, this.lookupDelayMs));
    } finally {
      this.inFlight--;
    }
    const amount = this.orders[orderId];
    if (amount === undefined) throw new Error(`订单 ${orderId} 不存在`);
    return { orderId, amount };
  }

  refund(orderId: string, amount: number): string {
    this.refunds.push(`${orderId}:${amount}`);
    return `已向 ${orderId} 退款 ${amount}`;
  }
}

/** 超过这个金额的退款要人工审批。 */
export const APPROVAL_THRESHOLD = 500;
