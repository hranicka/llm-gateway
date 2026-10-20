/**
 * Restore - pick an interrupted session back up.
 *
 * Resuming a session (pi -c / --resume, or a client reopening a thread) that
 * ended mid-turn (killed process, aborted run, error, unanswered tool call)
 * adds a one-time note to the next prompt: verify the state before repeating
 * side effects, then finish the unfinished work. `/restore` sends that
 * continuation as a prompt of its own. Subagents run one at a time inside a
 * tool call, so an interrupted one is simply re-run by the continued turn.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

const NOTE =
	"The previous run in this session was interrupted before it finished. Before acting, check the actual state " +
	"(git status/diff, files touched, anything started but not confirmed) and do not repeat side effects that " +
	"already happened. Then continue the unfinished work from the last request below.";

function wasInterrupted(entries: readonly any[]): boolean {
	for (let i = entries.length - 1; i >= 0; i--) {
		const entry = entries[i];
		if (entry.type !== "message") continue;
		const msg = entry.message;
		if (msg.role === "user" || msg.role === "toolResult") return true;
		if (msg.role === "assistant") return msg.stopReason !== "stop" && msg.stopReason !== "length";
	}
	return false;
}

export default function (pi: ExtensionAPI) {
	if (process.env.PI_SUBAGENT) return;
	let pending = false;

	pi.on("session_start", async (event, ctx) => {
		pending = event.reason !== "new" && wasInterrupted(ctx.sessionManager.getBranch());
		if (pending) ctx.ui.notify("Previous run was interrupted — /restore continues it.", "info");
	});

	pi.on("before_agent_start", async (event) => {
		if (!pending || event.messageType !== "user") return;
		pending = false;
		return { message: { customType: "restore", content: NOTE, display: false } };
	});

	pi.registerCommand("restore", {
		description: "Continue the interrupted work of this session",
		handler: async (_args, ctx) => {
			pending = false;
			pi.sendUserMessage(`${NOTE} Continue now.`, ctx.isIdle() ? undefined : { deliverAs: "followUp" });
		},
	});
}
