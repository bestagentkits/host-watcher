/**
 * host-watcher extension for the Pi coding agent.
 *
 * After every bash tool call it runs the Node-free hook
 * (hooks/generic/host_pressure_hook.py) and appends its reminder to the tool
 * result, so the model sees host pressure where it decides the next command.
 * Before each agent turn it injects the same reminder as a hidden message.
 *
 * Install: copy this file into your Pi extensions directory (or load it with
 * Pi's extension flag) and set HOST_WATCHER_DIR to your host-watcher clone.
 * Status: written against the Pi extension events `tool_result` and
 * `before_agent_start`; not exercised by this repository's CI.
 */
import { spawnSync } from "node:child_process";
import { join } from "node:path";

const ROOT = process.env.HOST_WATCHER_DIR || join(__dirname, "..", "..");
const HOOK = join(ROOT, "hooks", "generic", "host_pressure_hook.py");
const PYTHON = process.env.HOST_WATCHER_PYTHON || (process.platform === "win32" ? "python" : "python3");

function reminder(cwd: string | undefined, sessionId: string | undefined): string {
  try {
    const res = spawnSync(PYTHON, [HOOK, "--format", "text"], {
      input: JSON.stringify({ cwd, session_id: sessionId }),
      encoding: "utf8",
      timeout: 5000,
      windowsHide: true,
    });
    return res.status === 0 ? (res.stdout || "").trim() : "";
  } catch {
    return "";
  }
}

function sessionOf(ctx: any): string | undefined {
  try {
    return String(ctx.sessionManager.getSessionId());
  } catch {
    return undefined;
  }
}

export default function hostWatcherExtension(pi: any) {
  pi.on("tool_result", async (event: any, ctx: any) => {
    if (String(event.toolName || "").toLowerCase() !== "bash") return;
    const text = reminder(ctx.cwd, sessionOf(ctx));
    if (text) return { content: [...event.content, { type: "text", text }] };
  });

  pi.on("before_agent_start", (_event: any, ctx: any) => {
    const text = reminder(ctx.cwd, sessionOf(ctx));
    if (text) return { message: { customType: "host-watcher", content: text, display: false } };
  });
}
