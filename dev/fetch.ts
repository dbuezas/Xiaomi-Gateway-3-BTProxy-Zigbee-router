// Receive a file from a "nc -l -p 6001 < file" listener on the gateway. Usage: bun fetch.ts out
const chunks: Uint8Array[] = [];
await Bun.connect({ hostname: process.env.GW ?? (() => { throw new Error("set GW to the gateway IP"); })(), port: 6001, socket: {
  data(_s, d) { chunks.push(new Uint8Array(d)); },
  async close() { await Bun.write(process.argv[2], Buffer.concat(chunks)); process.exit(0); },
}});
setTimeout(() => process.exit(1), 60000);
