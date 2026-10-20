/**
 * Per-profile subagent roles.
 *
 * ~/.pi/agent/subagent-roles.json maps the *active model* to a profile. The
 * profile is the merge of every matching key, least specific first:
 *   "*"  ->  "<provider>"  ->  "<provider>/<id>"  ->  "<provider>/<id>:<thinking>"
 * Keys of a profile: `maxParallel` (number) and one entry per agent name:
 *   "inherit"              the parent's model and thinking level
 *   ":<level>"             the parent's model, another thinking level
 *   "<provider>/<id>"      that model, the agent's own thinking level
 *   "<provider>/<id>:<l>"  that model and level
 * An agent without an entry uses its frontmatter, else inherits the parent.
 */

import * as fs from "node:fs";
import * as path from "node:path";
import { getAgentDir } from "@earendil-works/pi-coding-agent";

const LEVELS = new Set(["off", "minimal", "low", "medium", "high", "xhigh", "max"]);
const DEFAULT_MAX_PARALLEL = 10;

export interface ParentModel {
	provider: string;
	id: string;
	thinking?: string;
}

export interface Profile {
	maxParallel: number;
	roles: Record<string, string>;
}

export interface Selection {
	model?: string;
	thinking?: string;
}

export function loadProfile(parent: ParentModel | undefined): Profile {
	let config: Record<string, Record<string, unknown>> = {};
	try {
		config = JSON.parse(fs.readFileSync(path.join(getAgentDir(), "subagent-roles.json"), "utf-8"));
	} catch {
		/* no file: every agent inherits, default concurrency */
	}
	const keys = ["*"];
	if (parent) {
		keys.push(parent.provider, `${parent.provider}/${parent.id}`);
		if (parent.thinking) keys.push(`${parent.provider}/${parent.id}:${parent.thinking}`);
	}
	const profile: Profile = { maxParallel: DEFAULT_MAX_PARALLEL, roles: {} };
	for (const key of keys) {
		const entry = config[key];
		if (!entry || typeof entry !== "object") continue;
		for (const [name, value] of Object.entries(entry)) {
			if (name === "maxParallel") {
				if (typeof value === "number" && value >= 1) profile.maxParallel = Math.floor(value);
			} else if (typeof value === "string") {
				profile.roles[name] = value;
			}
		}
	}
	return profile;
}

export function selectFor(
	agent: { name: string; model?: string; thinking?: string },
	parent: ParentModel | undefined,
	profile: Profile,
): Selection {
	const value = profile.roles[agent.name];
	if (value === undefined) {
		return { model: agent.model, thinking: agent.thinking ?? (agent.model ? undefined : parent?.thinking) };
	}
	if (value === "inherit") return { thinking: parent?.thinking };
	if (value.startsWith(":")) return { thinking: value.slice(1) };
	const cut = value.lastIndexOf(":");
	if (cut > 0 && LEVELS.has(value.slice(cut + 1))) return { model: value.slice(0, cut), thinking: value.slice(cut + 1) };
	return { model: value, thinking: agent.thinking };
}
