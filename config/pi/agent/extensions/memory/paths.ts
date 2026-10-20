import * as os from "node:os";
import * as path from "node:path";
import { getAgentDir } from "@earendil-works/pi-coding-agent";

/** Session-style slug of a directory: /home/me/app -> --home-me-app-- */
export function projectSlug(cwd: string): string {
	return `--${cwd.replace(/^\/+/, "").replace(/[\\/:]+/g, "-")}--`;
}

export function memoryDir(cwd: string): string {
	return path.join(getAgentDir(), "memories", projectSlug(cwd));
}

export function globalMemoryFile(): string {
	return path.join(getAgentDir(), "memories", "GLOBAL.md");
}

export function projectMemoryFile(cwd: string): string {
	return path.join(memoryDir(cwd), "MEMORY.md");
}

/** omp keeps the same layout under ~/.omp/agent/memories; read-only here. */
export function ompSummaryFile(cwd: string): string {
	return path.join(os.homedir(), ".omp", "agent", "memories", projectSlug(cwd), "memory_summary.md");
}
