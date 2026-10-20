/**
 * Memory.
 *
 * ~/.pi/agent/memories/<project>/MEMORY.md (per project) and GLOBAL.md are
 * injected into the system prompt once per session (a stable prompt keeps the
 * backend's KV prefix warm), so `memory_add` takes effect in the next session.
 * omp's memory_summary.md for the same project is read as well, read-only.
 */

import * as fs from "node:fs";
import * as path from "node:path";
import { type ExtensionAPI, withFileMutationQueue } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { globalMemoryFile, ompSummaryFile, projectMemoryFile } from "./paths.ts";

const MAX_MEMORY_CHARS = 8000;

function readCapped(file: string, max = MAX_MEMORY_CHARS): string {
	try {
		const text = fs.readFileSync(file, "utf-8").trim();
		return text.length > max ? `${text.slice(0, max)}\n[truncated — read ${file} for the rest]` : text;
	} catch {
		return "";
	}
}

function buildMemoryBlock(cwd: string): string {
	const sections: string[] = [];
	const project = readCapped(projectMemoryFile(cwd));
	const global = readCapped(globalMemoryFile());
	const omp = readCapped(ompSummaryFile(cwd), 3000);
	if (global) sections.push(`### Global memory (${globalMemoryFile()})\n\n${global}`);
	if (project) sections.push(`### Project memory (${projectMemoryFile(cwd)})\n\n${project}`);
	if (omp) sections.push(`### Project summary imported from omp\n\n${omp}`);
	if (sections.length === 0) {
		return `\n\n## Memory\n\nNo memory yet. Save durable facts with \`memory_add\` (project: ${projectMemoryFile(cwd)}).\n`;
	}
	return (
		"\n\n## Memory\n\nNotes from earlier sessions. They are advisory: the repository's current state wins over them. " +
		"Add durable facts with `memory_add`; edit the files directly to correct or remove an entry.\n\n" +
		`${sections.join("\n\n")}\n`
	);
}

async function appendNote(file: string, topic: string, text: string): Promise<void> {
	await withFileMutationQueue(file, async () => {
		await fs.promises.mkdir(path.dirname(file), { recursive: true });
		let content = "";
		try {
			content = await fs.promises.readFile(file, "utf-8");
		} catch {
			content = "# Memory\n";
		}
		const bullet = `- ${text.replace(/\s*\n\s*/g, " ").trim()}`;
		const heading = `## ${topic.trim()}`;
		if (content.split("\n").some((l) => l.trim() === bullet)) return;
		const lines = content.replace(/\s+$/, "").split("\n");
		const at = lines.findIndex((l) => l.trim() === heading);
		if (at === -1) {
			lines.push("", heading, bullet);
		} else {
			let end = at + 1;
			while (end < lines.length && !lines[end].startsWith("## ")) end++;
			while (end > at + 1 && lines[end - 1].trim() === "") end--;
			lines.splice(end, 0, bullet);
		}
		await fs.promises.writeFile(file, `${lines.join("\n")}\n`, "utf-8");
	});
}

export default function (pi: ExtensionAPI) {
	if (process.env.PI_SUBAGENT) return;
	let block = "";

	pi.on("session_start", async (_event, ctx) => {
		block = buildMemoryBlock(ctx.cwd);
	});

	pi.on("before_agent_start", async (event) => {
		return block ? { systemPrompt: event.systemPrompt + block } : undefined;
	});

	pi.registerTool({
		name: "memory_add",
		label: "Memory",
		description:
			"Save one durable, verified fact for future sessions (convention, command, pitfall, decision). " +
			"Written under a topic heading; it appears in the system prompt from the next session on.",
		parameters: Type.Object({
			scope: Type.Union([Type.Literal("project"), Type.Literal("global")], {
				description: "project = this working directory; global = every project",
			}),
			topic: Type.String({ description: "Short section heading, e.g. 'Testing' or 'Deployment'" }),
			text: Type.String({ description: "The fact, one or two sentences" }),
		}),
		async execute(_id, params, _signal, _onUpdate, ctx) {
			const file = params.scope === "global" ? globalMemoryFile() : projectMemoryFile(ctx.cwd);
			await appendNote(file, params.topic, params.text);
			return { content: [{ type: "text", text: `Saved to ${file} under "${params.topic}".` }], details: undefined };
		},
	});
}
