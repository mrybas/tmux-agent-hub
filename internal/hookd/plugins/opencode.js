// tmux-agent-hub plugin for opencode.
//
// opencode has no hooks file: plugins are JavaScript modules loaded from
// ~/.config/opencode/plugins. This one does the two jobs the other agents'
// hooks do for us — it reports lifecycle events, and it writes a
// transcript we can read.
//
// The transcript is written in Claude Code's JSONL shape on purpose. It is
// the format our readers already parse, so an opencode session gets the
// delta feed, the activity timeline and the stuck detectors for free. The
// file is ours, under $XDG_STATE_HOME/tmux-agent-hub/opencode/.
//
// This targets the opencode 2.x plugin API: the module's default export is
// { id, setup }, and setup gets a context whose event.subscribe yields the
// server's events as { type, location?, data }. The 1.x shape (a named
// export returning hook functions) is rejected by 2.x at load time.
//
// BIN is replaced with the plugin's absolute path when it is installed.

import { appendFileSync, mkdirSync, statSync, writeFileSync } from "node:fs"
import { spawn } from "node:child_process"
import { homedir } from "node:os"
import { join } from "node:path"

const BIN = "__TMUX_AGENT_HUB_BIN__"

const stateDir = join(
  process.env.XDG_STATE_HOME || join(homedir(), ".local", "state"),
  "tmux-agent-hub",
  "opencode",
)

const transcriptFor = (sessionID) => join(stateDir, `${sessionID}.jsonl`)

function append(sessionID, entry) {
  try {
    mkdirSync(stateDir, { recursive: true })
    appendFileSync(transcriptFor(sessionID), JSON.stringify(entry) + "\n")
  } catch {
    // a transcript we cannot write is not worth breaking a session over
  }
}

const userLine = (text) => ({ type: "user", message: { role: "user", content: text } })

const agentLine = (text, model) => ({
  type: "assistant",
  message: { role: "assistant", model, content: [{ type: "text", text }] },
})

const toolLine = (callID, tool, input) => ({
  type: "assistant",
  message: {
    role: "assistant",
    content: [{ type: "tool_use", id: callID, name: tool, input: input || {} }],
  },
})

const resultLine = (callID, output, isError) => ({
  type: "user",
  message: {
    role: "user",
    content: [{ type: "tool_result", tool_use_id: callID, content: output || "", is_error: !!isError }],
  },
})

// textOf flattens a tool's content parts ([{ type: "text", text }, ...])
// into the plain string a tool_result carries.
const textOf = (content) =>
  (Array.isArray(content) ? content : [])
    .filter((p) => p && p.type === "text" && p.text)
    .map((p) => p.text)
    .join("")

const errorText = (error) =>
  typeof error === "string" ? error : error?.message || (error ? JSON.stringify(error) : "")

// notify runs the hook binary the way an agent's own hooks would: fire and
// forget, never blocking the session, never surfacing an error into it.
function notify(payload) {
  try {
    const child = spawn(BIN, ["hook", "opencode"], {
      stdio: ["pipe", "ignore", "ignore"],
      detached: false,
    })
    child.on("error", () => {})
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(payload))
  } catch {
    // same rule as above: the plugin is an observer, not a participant
  }
}

// loaded leaves a line saying this plugin is in place. opencode loads
// plugins at startup, so "did my running session pick it up?" is
// otherwise unanswerable until the session does something.
function loaded(directory) {
  try {
    mkdirSync(stateDir, { recursive: true })
    const path = join(stateDir, "plugin.log")
    try {
      if (statSync(path).size > 32 * 1024) writeFileSync(path, "")
    } catch {}
    appendFileSync(path, `${new Date().toISOString()} loaded in ${directory}\n`)
  } catch {}
}

const TYPES = [
  "session.created",
  "session.deleted",
  "session.inbox.enqueued",
  "session.step.started",
  "session.text.ended",
  "session.tool.input.started",
  "session.tool.called",
  "session.tool.success",
  "session.tool.failed",
  "session.execution.succeeded",
  "session.execution.failed",
  "session.execution.interrupted",
  "permission.asked",
]

async function setup(ctx) {
  const home = ctx.location?.directory
  loaded(home || "?")

  // opencode 2.x runs one server for every directory and may set a plugin
  // up once per directory. A session belongs to the instance whose
  // directory its events carry; events without a location (the
  // execution.* ones) are handled only for sessions already claimed.
  const sessions = new Map() // sessionID -> { cwd, model, tools: Map(callID -> { name, input }) }

  const claim = (event) => {
    const id = event.data?.sessionID
    if (!id) return undefined
    const dir = event.location?.directory
    if (!sessions.has(id)) {
      if (!dir || (home && dir !== home)) return undefined
      sessions.set(id, { cwd: dir, tools: new Map() })
      // a session we meet mid-flight (the server outlived a plugin
      // reload) is announced the way a fresh one is
      if (event.type !== "session.created") report(id, "SessionStart")
    }
    return sessions.get(id)
  }

  const report = (sessionID, event, extra = {}) => {
    const s = sessions.get(sessionID) || {}
    notify({
      hook_event_name: event,
      session_id: sessionID,
      cwd: s.cwd || home,
      transcript_path: transcriptFor(sessionID),
      model: s.model,
      ...extra,
    })
  }

  const handle = (event) => {
    const data = event.data || {}
    const s = claim(event)
    if (!s) return
    const id = data.sessionID

    switch (event.type) {
      case "session.created":
        if (data.model?.id) s.model = data.model.id
        report(id, "SessionStart")
        break

      case "session.deleted":
        report(id, "SessionEnd")
        sessions.delete(id)
        break

      case "session.inbox.enqueued": {
        const item = data.item || {}
        if (item.type !== "user") break
        const text = item.payload?.text || ""
        if (text) append(id, userLine(text))
        report(id, "UserPromptSubmit", { prompt: text })
        break
      }

      case "session.step.started":
        if (data.model?.id) s.model = data.model.id
        break

      case "session.text.ended":
        if (data.text) append(id, agentLine(data.text, s.model))
        break

      case "session.tool.input.started":
        s.tools.set(data.id, { name: data.name })
        break

      case "session.tool.called": {
        const tool = s.tools.get(data.id) || { name: "" }
        tool.input = data.input
        s.tools.set(data.id, tool)
        report(id, "PreToolUse", { tool_name: tool.name })
        break
      }

      case "session.tool.success":
      case "session.tool.failed": {
        const tool = s.tools.get(data.id) || { name: "" }
        s.tools.delete(data.id)
        const failed = event.type === "session.tool.failed"
        const output = failed ? errorText(data.error) || textOf(data.content) : textOf(data.content)
        append(id, toolLine(data.id, tool.name, tool.input))
        append(id, resultLine(data.id, output, failed))
        report(id, "PostToolUse", { tool_name: tool.name })
        break
      }

      case "session.execution.succeeded":
      case "session.execution.failed":
      case "session.execution.interrupted":
        report(id, "Stop")
        break

      case "permission.asked":
        report(id, "PermissionRequest", {
          tool_name: data.action || "",
          message: data.message || "permission",
        })
        break
    }
  }

  const abort = new AbortController()
  ;(async () => {
    for await (const event of ctx.event.subscribe({ types: TYPES, signal: abort.signal })) {
      try {
        handle(event)
      } catch {
        // one odd event must not end the subscription
      }
    }
  })().catch(() => {})

  return () => abort.abort()
}

export default { id: "tmux-agent-hub", setup }
