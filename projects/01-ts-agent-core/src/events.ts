import { z } from "zod";

export const StreamEventSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("text"), delta: z.string() }),
  z.object({ type: z.literal("tool_call"), id: z.string(), name: z.string(), args: z.string() }),
  z.object({ type: z.literal("done"), usage: z.object({ input: z.number(), output: z.number() }) }),
  z.object({ type: z.literal("error"), message: z.string() }),
]);

export type StreamEvent = z.infer<typeof StreamEventSchema>;

export type ParseEventResult =
  | { ok: true; event: StreamEvent }
  | { ok: false; error: string };

export function parseEvent(data: string): ParseEventResult {
  let json: unknown;
  try {
    json = JSON.parse(data);
  } catch {
    return { ok: false, error: "invalid json" };
  }
  const r = StreamEventSchema.safeParse(json);
  if (r.success) return { ok: true, event: r.data };
  return { ok: false, error: z.prettifyError(r.error) };
}
