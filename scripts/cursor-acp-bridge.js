#!/usr/bin/env node
/**
 * cursor-acp-bridge.js — host-side bridge for the Cursor ACP integration.
 *
 * LiteRouter runs in a container, but the Cursor CLI ships a macOS binary
 * (bundled node is Mach-O) that cannot run inside a Linux distroless image.
 * This bridge runs on the host where `agent` works, spawns `agent acp` per
 * connection, and relays newline-delimited JSON-RPC over a TCP socket.
 *
 * Usage:
 *   node cursor-acp-bridge.js [port]        # default 1459, listens on 127.0.0.1
 *   LITEROUTER_CURSOR_ACP_HOST=127.0.0.1:1459 docker compose up -d
 *
 * Each inbound connection spawns a fresh `agent acp` and pipes stdin/stdout
 * bidirectionally. The agent authenticates through the token LiteRouter stores
 * in its DB (Phase 1), so no keychain access is needed here.
 */

const net = require("node:net");
const { spawn } = require("node:child_process");

const PORT = parseInt(process.argv[2] || process.env.CURSOR_ACP_BRIDGE_PORT || "1459", 10);
const HOST = process.env.CURSOR_ACP_BRIDGE_HOST || "127.0.0.1";

// Find the agent binary: prefer the CLI's own symlink on PATH, then the
// newest version dir.
function findAgent() {
  const fs = require("node:fs");
  const path = require("node:path");
  const home = process.env.HOME || require("node:os").homedir();
  const versions = path.join(home, ".local", "share", "cursor-agent", "versions");
  let best = null;
  try {
    for (const entry of fs.readdirSync(versions)) {
      const candidate = path.join(versions, entry, "cursor-agent");
      if (fs.existsSync(candidate) && (!best || entry > best)) best = candidate;
    }
  } catch {}
  if (best) return best;
  return "agent";
}

const agentBinary = findAgent();
console.error(`[cursor-acp-bridge] agent = ${agentBinary}, listening on ${HOST}:${PORT}`);

const server = net.createServer((socket) => {
  console.error(`[cursor-acp-bridge] connection from ${socket.remoteAddress}:${socket.remotePort}`);
  const child = spawn(agentBinary, ["acp"], {
    stdio: ["pipe", "pipe", "pipe"],
    env: { ...process.env, CURSOR_INVOKED_AS: "agent" },
  });
  child.stdout.pipe(socket);
  socket.pipe(child.stdin);
  child.stderr.on("data", (d) => process.stderr.write(`[agent] ${d}`));
  socket.on("error", () => {});
  socket.on("close", () => child.kill());
  child.on("exit", () => socket.end());
});

server.listen(PORT, HOST, () => {
  console.error(`[cursor-acp-bridge] ready`);
});

process.on("SIGINT", () => { server.close(); process.exit(0); });
process.on("SIGTERM", () => { server.close(); process.exit(0); });
