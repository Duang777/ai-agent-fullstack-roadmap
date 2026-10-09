// 本地体验：npm run dev，然后另开终端 curl -N -X POST http://127.0.0.1:<端口>/chat
import { streamChat } from "./client.ts";
import { startServer } from "./server.ts";

const srv = await startServer({
  tokens: ["你", "好", "，", "我", "是", " Agent", "。"],
  tokenIntervalMs: 150,
  heartbeatMs: 1000,
});
console.log(`listening on ${srv.url}/chat`);

let out = "";
for await (const ev of streamChat(`${srv.url}/chat`, { messages: [] })) {
  if (ev.type === "token") {
    out += ev.text;
    process.stdout.write(ev.text);
  }
}
console.log(`\n完整输出：${out}`);
await srv.close();
