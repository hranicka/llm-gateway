/**
 * Rules - Markdown rule files in the omp / Claude Code / Cursor layout.
 *
 * Scans <project>/{.pi,.omp,.claude,.cursor}/rules and
 * ~/.{pi/agent,omp/agent,claude}/rules. A rule with `alwaysApply: true` is
 * inlined into the system prompt; every other rule is listed with its
 * description and globs so the model reads it when the task matches.
 * The block is built once per session so the system prompt (and the
 * backend's KV prefix) stays stable.
 */

import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { type ExtensionAPI, parseFrontmatter } from "@earendil-works/pi-coding-agent";

const MAX_ALWAYS_CHARS = 8000;
const PROJECT_DIRS = [".pi/rules", ".omp/rules", ".claude/rules", ".cursor/rules"];
const USER_DIRS = [".pi/agent/rules", ".omp/agent/rules", ".claude/rules"];

type Frontmatter = { description?: unknown; globs?: unknown; alwaysApply?: unknown };

interface Rule {
	file: string;
	display: string;
	description: string;
	globs: string[];
	alwaysApply: boolean;
	body: string;
}

function findRuleFiles(dir: string): string[] {
	const out: string[] = [];
	let entries: fs.Dirent[];
	try {
		entries = fs.readdirSync(dir, { withFileTypes: true });
	} catch {
		return out;
	}
	for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
		const full = path.join(dir, entry.name);
		if (entry.isDirectory()) out.push(...findRuleFiles(full));
		else if (entry.name.endsWith(".md") || entry.name.endsWith(".mdc")) out.push(full);
	}
	return out;
}

function toList(value: unknown): string[] {
	const raw = Array.isArray(value) ? value : typeof value === "string" ? value.split(",") : [];
	return raw.filter((v): v is string => typeof v === "string").map((v) => v.trim()).filter(Boolean);
}

function loadRules(cwd: string): Rule[] {
	const roots: { dir: string; label: string }[] = [
		...USER_DIRS.map((d) => ({ dir: path.join(os.homedir(), d), label: `~/${d}` })),
	];
	// Project rule dirs of the cwd and its ancestors, nearest last so it wins on duplicates.
	const ancestors: string[] = [];
	for (let cur = cwd; ; cur = path.dirname(cur)) {
		ancestors.unshift(cur);
		if (fs.existsSync(path.join(cur, ".git")) || path.dirname(cur) === cur) break;
	}
	for (const base of ancestors) {
		for (const d of PROJECT_DIRS) roots.push({ dir: path.join(base, d), label: path.relative(cwd, path.join(base, d)) || d });
	}

	const byName = new Map<string, Rule>();
	for (const root of roots) {
		for (const file of findRuleFiles(root.dir)) {
			let content: string;
			try {
				content = fs.readFileSync(file, "utf-8");
			} catch {
				continue;
			}
			const { frontmatter, body } = parseFrontmatter<Frontmatter>(content);
			const rel = path.relative(root.dir, file);
			byName.set(rel.replace(/\.(md|mdc)$/, ""), {
				file,
				display: path.join(root.label, rel),
				description: typeof frontmatter.description === "string" ? frontmatter.description : "",
				globs: toList(frontmatter.globs),
				alwaysApply: frontmatter.alwaysApply === true,
				body: body.trim(),
			});
		}
	}
	return [...byName.values()];
}

function buildBlock(rules: Rule[]): string {
	if (rules.length === 0) return "";
	let always = "";
	const onDemand: string[] = [];
	for (const rule of rules) {
		if (rule.alwaysApply && always.length + rule.body.length <= MAX_ALWAYS_CHARS) {
			always += `\n### ${rule.display}\n\n${rule.body}\n`;
		} else {
			const scope = rule.globs.length > 0 ? ` (applies to: ${rule.globs.join(", ")})` : "";
			onDemand.push(`- ${rule.file}${rule.description ? ` — ${rule.description}` : ""}${scope}`);
		}
	}
	let block = "\n\n## Rules\n";
	if (always) block += `\nThese rules always apply:\n${always}`;
	if (onDemand.length > 0) {
		block += `\nWhen a task matches a rule below, read the file before acting:\n\n${onDemand.join("\n")}\n`;
	}
	return block;
}

export default function (pi: ExtensionAPI) {
	let block = "";

	pi.on("session_start", async (_event, ctx) => {
		block = buildBlock(loadRules(ctx.cwd));
	});

	pi.on("before_agent_start", async (event) => {
		if (!block) return;
		return { systemPrompt: event.systemPrompt + block };
	});
}
