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

export const TmuxAgentHub = async ({ directory, worktree }) => {
  const cwd = worktree || directory
  loaded(cwd)
  // what we have learned about each session, and which parts are already
  // in its transcript — parts stream, so they arrive many times over
  const sessions = new Map() // sessionID -> { model, cwd }
  const written = new Set() // part id
  const roles = new Map() // messageID -> "user" | "assistant"

  const session = (id) => {
    if (!sessions.has(id)) sessions.set(id, {})
    return sessions.get(id)
  }

  const report = (sessionID, event, extra = {}) =>
    notify({
      hook_event_name: event,
      session_id: sessionID,
      // the session's own directory, not the plugin's: a plugin loaded
      // globally reports "/" for it, and a pane in the wrong directory is
      // a pane the hub cannot match to this agent
      cwd: session(sessionID).cwd || cwd,
      transcript_path: transcriptFor(sessionID),
      model: session(sessionID).model,
      ...extra,
    })

  return {
    "chat.message": async (_input, output) => {
      const sessionID = output.message.sessionID
      roles.set(output.message.id, "user")
      const text = (output.parts || [])
        .filter((p) => p.type === "text" && p.text)
        .map((p) => p.text)
        .join("\n")
      if (text) {
        append(sessionID, userLine(text))
        // the parts of a user message need no second look
        for (const p of output.parts || []) written.add(p.id)
      }
      report(sessionID, "UserPromptSubmit", { prompt: text })
    },

    event: async ({ event }) => {
      const props = event.properties || {}
      switch (event.type) {
        case "session.created":
          if (props.info?.directory) session(props.info.id).cwd = props.info.directory
          report(props.info?.id, "SessionStart")
          break

        case "session.updated":
          if (props.info?.id && props.info.directory) {
            session(props.info.id).cwd = props.info.directory
          }
          break

        case "session.deleted":
          report(props.info?.id, "SessionEnd")
          break

        case "session.idle":
          report(props.sessionID, "Stop")
          break

        case "permission.updated":
          report(props.sessionID, "PermissionRequest", {
            tool_name: props.title || props.type || "",
            message: "permission",
          })
          break

        case "message.updated": {
          const info = props.info
          if (!info) break
          roles.set(info.id, info.role)
          if (info.role === "assistant") {
            if (info.modelID) session(info.sessionID).model = info.modelID
            if (info.path?.cwd) session(info.sessionID).cwd = info.path.cwd
          }
          break
        }

        case "message.part.updated": {
          const part = props.part
          if (!part || written.has(part.id)) break

          if (part.type === "text") {
            // a text part is final once it has an end time; before that it
            // is still being typed and would land in the transcript twice
            if (!part.time?.end || !part.text) break
            written.add(part.id)
            if (roles.get(part.messageID) === "user") {
              append(part.sessionID, userLine(part.text))
            } else {
              append(part.sessionID, agentLine(part.text, session(part.sessionID).model))
            }
            break
          }

          if (part.type === "tool") {
            const state = part.state || {}
            if (state.status === "running" && !written.has(`pre:${part.callID}`)) {
              written.add(`pre:${part.callID}`)
              report(part.sessionID, "PreToolUse", { tool_name: part.tool })
              break
            }
            if (state.status !== "completed" && state.status !== "error") break
            written.add(part.id)
            append(part.sessionID, toolLine(part.callID, part.tool, state.input))
            append(
              part.sessionID,
              resultLine(part.callID, state.status === "error" ? state.error : state.output, state.status === "error"),
            )
            report(part.sessionID, "PostToolUse", { tool_name: part.tool })
            break
          }
          break
        }
      }
    },
  }
}
