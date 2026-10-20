/**
 * Vision fallback.
 *
 * When the active model cannot read images (pi would replace them with an
 * "image omitted" placeholder), images are described by the `vision` subagent
 * instead: images attached to a prompt and images returned by tools (the read
 * tool, browser screenshots, MCP tools) become text before the model sees them.
 * Without a `vision` agent, or on a vision-capable model, nothing changes.
 *
 * The vision agent usually runs another gateway model, so each description
 * reloads the backend (~1-2.5 min) and the parent's model again afterwards.
 * Descriptions run one at a time.
 */

import { spawn } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { discoverAgents } from "../subagent/agents.ts";

interface Image {
	type: "image";
	data: string;
	mimeType: string;
}

const QUESTION =
	"Describe each image for a developer who cannot see it: visible text verbatim, layout and structure, UI elements and their states " +
	"(checked, disabled, open), colours, and anything that looks wrong. Say 'unclear' instead of guessing.";

const EXT: Record<string, string> = { "image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif" };

function piInvocation(args: string[]): { command: string; args: string[] } {
	const script = process.argv[1];
	if (script && !script.startsWith("/$bunfs/root/") && fs.existsSync(script)) {
		return { command: process.execPath, args: [script, ...args] };
	}
	const exec = path.basename(process.execPath).toLowerCase();
	return /^(node|bun)(\.exe)?$/.test(exec) ? { command: "pi", args } : { command: process.execPath, args };
}

function finalText(stdout: string): string {
	let text = "";
	for (const line of stdout.split("\n")) {
		try {
			const event = JSON.parse(line);
			if (event.type === "message_end" && event.message?.role === "assistant") {
				const parts = (event.message.content ?? []).filter((c: any) => c.type === "text").map((c: any) => c.text);
				if (parts.length > 0) text = parts.join("\n").trim();
			}
		} catch {
			/* not a JSON event line */
		}
	}
	return text;
}

export default function (pi: ExtensionAPI) {
	if (process.env.PI_SUBAGENT) return;
	let queue: Promise<unknown> = Promise.resolve();

	const lacksVision = (ctx: { model?: { input?: string[] } }) => ctx.model !== undefined && !ctx.model.input?.includes("image");

	async function runVision(cwd: string, files: string[], context: string, signal?: AbortSignal): Promise<string> {
		const agent = discoverAgents(cwd, "user").agents.find((a) => a.name === "vision");
		if (!agent) throw new Error("no 'vision' agent installed");
		const promptDir = await fs.promises.mkdtemp(path.join(os.tmpdir(), "pi-vision-prompt-"));
		const promptFile = path.join(promptDir, "vision.md");
		await fs.promises.writeFile(promptFile, agent.systemPrompt, "utf-8");
		const args = ["--mode", "json", "-p", "--no-session", "--tools", "read", "--append-system-prompt", promptFile];
		if (agent.model) args.push("--model", agent.model);
		if (agent.thinking) args.push("--thinking", agent.thinking);
		args.push(`Task: ${QUESTION}${context ? `\nContext: ${context}` : ""}\nImage files:\n${files.map((f) => `- ${f}`).join("\n")}`);
		try {
			return await new Promise<string>((resolve, reject) => {
				const inv = piInvocation(args);
				const proc = spawn(inv.command, inv.args, {
					cwd,
					stdio: ["ignore", "pipe", "pipe"],
					env: { ...process.env, PI_SUBAGENT: "1" },
				});
				let out = "";
				let err = "";
				proc.stdout.on("data", (d) => (out += d));
				proc.stderr.on("data", (d) => (err += d));
				proc.on("error", reject);
				proc.on("close", (code) => {
					const text = finalText(out);
					if (text) resolve(text);
					else reject(new Error(err.trim().split("\n").pop() || `vision agent exited with ${code}`));
				});
				signal?.addEventListener("abort", () => proc.kill("SIGTERM"), { once: true });
			});
		} finally {
			await fs.promises.rm(promptDir, { recursive: true, force: true });
		}
	}

	/** Image blocks -> one text block; runs one description at a time. */
	function describe(cwd: string, images: Image[], paths: string[], context: string, signal?: AbortSignal): Promise<string> {
		const job = queue.then(async () => {
			const dir = await fs.promises.mkdtemp(path.join(os.tmpdir(), "pi-vision-img-"));
			try {
				const files = [...paths];
				for (const [i, img] of images.entries()) {
					if (i < paths.length && paths[i]) continue;
					const file = path.join(dir, `image-${i + 1}.${EXT[img.mimeType] ?? "png"}`);
					await fs.promises.writeFile(file, Buffer.from(img.data, "base64"));
					files[i] = file;
				}
				const text = await runVision(cwd, files.filter(Boolean), context, signal);
				return `[Image description by the vision agent]\n${text}`;
			} catch (e) {
				return `[Image could not be described: ${(e as Error).message}]`;
			} finally {
				await fs.promises.rm(dir, { recursive: true, force: true });
			}
		});
		queue = job.catch(() => undefined);
		return job;
	}

	pi.on("input", async (event, ctx) => {
		if (!lacksVision(ctx) || !event.images || event.images.length === 0) return { action: "continue" };
		const description = await describe(ctx.cwd, event.images as Image[], [], event.text.slice(0, 500), ctx.signal);
		return { action: "transform", text: `${event.text}\n\n${description}`, images: [] };
	});

	pi.on("tool_result", async (event, ctx) => {
		if (!lacksVision(ctx)) return;
		const images = event.content.filter((c) => c.type === "image") as Image[];
		if (images.length === 0) return;
		const given = typeof event.input.path === "string" && event.toolName === "read" ? [path.resolve(ctx.cwd, event.input.path)] : [];
		const description = await describe(ctx.cwd, images, given, `result of the ${event.toolName} tool`, ctx.signal);
		// Drop the read tool's own "model does not support images" notice: the description replaces it.
		const kept = event.content.filter((c) => c.type !== "image" && !(c.type === "text" && c.text.includes("does not support images")));
		return { content: [...kept, { type: "text", text: description }] };
	});
}
