import * as fs from "node:fs";
import * as path from "node:path";
import { getAgentDir } from "@earendil-works/pi-coding-agent";

export interface AutolearnConfig {
	enabled: boolean;
	autoContinue: boolean;
	minToolCalls: number;
}

/** ~/.pi/agent/autolearn.json, with PI_AUTOLEARN=0|1 overriding `enabled`. */
export function loadAutolearnConfig(): AutolearnConfig {
	let file: Partial<AutolearnConfig> = {};
	try {
		file = JSON.parse(fs.readFileSync(path.join(getAgentDir(), "autolearn.json"), "utf-8"));
	} catch {
		/* defaults */
	}
	const env = process.env.PI_AUTOLEARN;
	return {
		enabled: env === "1" ? true : env === "0" ? false : file.enabled === true,
		autoContinue: file.autoContinue === true,
		minToolCalls: typeof file.minToolCalls === "number" ? file.minToolCalls : 5,
	};
}

export const AUTOLEARN_GUIDANCE = `
## Auto-learn

When you finish a task and learned something durable that a future session should know — a project convention, a
non-obvious command, a pitfall and its fix, a decision with its reason — record it with \`memory_add\`.
When a multi-step workflow proved reusable, capture it with \`skill_save\`. Record only verified, reusable facts;
skip anything already in memory or obvious from the code.`;

export const AUTOLEARN_NUDGE = `The task is complete. Before stopping, capture what is worth keeping:
call \`memory_add\` for each durable, verified lesson (convention, command, pitfall, decision) and \`skill_save\` for
a reusable workflow. If nothing qualifies, reply "Nothing to capture." and do not call any tool.`;
