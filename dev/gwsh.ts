// Run a shell command on the Xiaomi Gateway 3 over telnet and print its output.
// Usage: bun gwsh.ts '<command>' [timeoutSec]
const HOST = process.env.GW ?? (() => { throw new Error("set GW to the gateway IP"); })();
const cmd = process.argv[2] ?? "uname -a";
const timeout = Number(process.argv[3] ?? 20) * 1000;
const MARK = "__GWSH_DONE__";

let buf = "";
let stage: "login" | "pass" | "shell" | "run" = "login";
let out = "";
let done = false;

function strip(data: Uint8Array): string {
  // Answer telnet IAC negotiation with "won't/don't" and drop it from the text.
  const bytes: number[] = [];
  for (let i = 0; i < data.length; i++) {
    if (data[i] === 255 && i + 2 < data.length && data[i + 1] >= 251) {
      i += 2;
      continue;
    }
    bytes.push(data[i]);
  }
  return Buffer.from(bytes).toString("latin1");
}

const sock = await Bun.connect({
  hostname: HOST,
  port: 23,
  socket: {
    data(s, data) {
      const text = strip(data);
      buf += text;
      if (stage === "login" && /login:\s*$/.test(buf)) {
        s.write("admin\r\n");
        stage = "pass";
        buf = "";
      } else if ((stage === "login" || stage === "pass") && /[#$]\s*$/.test(buf)) {
        stage = "run";
        buf = "";
        s.write(`${cmd}; echo __GWSH""_DONE__\r\n`);
      } else if (stage === "pass" && /assword:\s*$/.test(buf)) {
        s.write("\r\n");
        buf = "";
      } else if (stage === "run") {
        out += text;
        const end = out.lastIndexOf(MARK);
        // the echoed command line spells the marker with quotes, so only the output matches
        if (end >= 0) {
          const lines = out.slice(0, end).split(/\r?\n/);
          console.log(lines.slice(1).join("\n").trimEnd());
          done = true;
          s.end();
          process.exit(0);
        }
      }
    },
    close() {
      if (out && !done) console.log(out);
      process.exit(stage === "run" ? 0 : 1);
    },
  },
});

setTimeout(() => {
  console.error(`timeout (stage=${stage})\n${buf}${out}`);
  sock.end();
  process.exit(2);
}, timeout);
