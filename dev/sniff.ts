// Man-in-the-middle recorder between Xiaomi's silabs_ncp_bt (on the gateway pty, bridged to TCP 6002)
// and the BT chip (/dev/ttyS1, bridged to TCP 6000). Relays bytes both ways and logs decoded BGAPI frames.
// Usage: bun sniff.ts [seconds]
import { appendFileSync, writeFileSync } from "node:fs";

const HOST = process.env.GW ?? (() => { throw new Error("set GW to the gateway IP"); })();
const SECS = Number(process.argv[2] ?? 90);
const LOG = `${import.meta.dir}/sniff-${new Date().toISOString().replace(/[:.]/g, "-")}.log`;
writeFileSync(LOG, "");
const t0 = Date.now();
const ts = () => ((Date.now() - t0) / 1000).toFixed(3).padStart(8);
const log = (s: string) => {
  appendFileSync(LOG, `${ts()} ${s}\n`);
};
const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");

// one framing parser per direction: SLIP (C0..C0) or raw length-prefixed BGAPI
function parser(dir: string, allowSlip: boolean) {
  let rx = Buffer.alloc(0);
  let advCount = 0;
  const emit = (b: Buffer, framing: string) => {
    const evt = (b[0] & 0x80) !== 0;
    const cls = b[2], id = b[3];
    if (evt && cls === 3 && (id === 0 || id === 4)) {
      advCount++;
      return;
    }
    log(`${dir} ${framing} ${evt ? "evt" : (b[0] & 0x78) === 0x20 ? "cmd/rsp" : "????"} ${cls}.${id} ${hex(b)}`);
  };
  return {
    advs: () => advCount,
    feed(chunk: Buffer) {
      rx = Buffer.concat([rx, chunk]);
      for (;;) {
        if (!rx.length) return;
        if (allowSlip && rx[0] === 0xc0) {
          const end = rx.indexOf(0xc0, 1);
          if (end < 0) return;
          const raw = rx.subarray(1, end);
          rx = rx.subarray(end);
          if (!raw.length) continue;
          const out: number[] = [];
          for (let i = 0; i < raw.length; i++) out.push(raw[i] === 0xdb && i + 1 < raw.length ? (raw[++i] === 0xdc ? 0xc0 : 0xdb) : raw[i]);
          emit(Buffer.from(out), "slip");
          continue;
        }
        if ((rx[0] & 0x78) === 0x20) {
          if (rx.length < 4) return;
          const len = (((rx[0] & 7) << 8) | rx[1]) + 4;
          if (rx.length < len) return;
          emit(Buffer.from(rx.subarray(0, len)), "raw ");
          rx = rx.subarray(len);
          continue;
        }
        log(`${dir} junk ${rx[0].toString(16)}`);
        rx = rx.subarray(1);
      }
    },
  };
}

// chip->host is SLIP-framed; host->chip is raw BGAPI (SLIP there fails with 0x0195)
const fromChip = parser("CHIP->HOST", true);
const fromHost = parser("HOST->CHIP", false);
let chip: any, host: any;
const pendingToHost: Buffer[] = [];

chip = await Bun.connect({
  hostname: HOST,
  port: 6000,
  socket: {
    data(_s, d) {
      const b = Buffer.from(d);
      fromChip.feed(b);
      if (host) host.write(b);
      else pendingToHost.push(b);
    },
    close: () => log("chip bridge closed"),
  },
});
host = await Bun.connect({
  hostname: HOST,
  port: 6002,
  socket: {
    data(_s, d) {
      const b = Buffer.from(d);
      log(`HOST bytes ${hex(b)}`);
      fromHost.feed(b);
      chip.write(b);
    },
    close: () => log("host bridge closed"),
  },
});
for (const b of pendingToHost) host.write(b);
console.log(`relaying for ${SECS}s, log ${LOG}`);
log("relay up");

const tick = setInterval(() => console.log(`${ts()} advs relayed: ${fromChip.advs()}`), 15000);
await Bun.sleep(SECS * 1000);
clearInterval(tick);
log(`end: ${fromChip.advs()} advertisements relayed`);
chip.end();
host.end();
console.log("done");
process.exit(0);
