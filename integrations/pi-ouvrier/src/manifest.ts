import { readdir, readFile, stat } from "node:fs/promises";
import path from "node:path";
import type { DiscoveredWorker, WorkerManifest } from "./types";

const MANIFEST_NAME = "ouvrier.worker.json";
const DEFAULT_ADMIN_URL = "http://127.0.0.1:8080";
const MAX_DISCOVERY_DEPTH = 6;
const SKIPPED_DIRS = new Set([
	".git",
	".hg",
	".svn",
	"node_modules",
	"vendor",
	"dist",
	"build",
	".next",
	".cache",
	".ouvrier",
]);

export async function discoverWorkers(cwd: string): Promise<DiscoveredWorker[]> {
	const manifestPaths = await findManifestPaths(cwd, 0);
	const workers = await Promise.all(manifestPaths.map((manifestPath) => readWorkerManifest(manifestPath)));
	return uniquifyWorkerIDs(workers.filter((worker): worker is DiscoveredWorker => worker !== undefined));
}

async function findManifestPaths(dir: string, depth: number): Promise<string[]> {
	if (depth > MAX_DISCOVERY_DEPTH) return [];

	let entries;
	try {
		entries = await readdir(dir, { withFileTypes: true });
	} catch {
		return [];
	}

	const found: string[] = [];
	for (const entry of entries) {
		if (entry.name === MANIFEST_NAME && entry.isFile()) {
			found.push(path.join(dir, entry.name));
			continue;
		}
		if (!entry.isDirectory() || SKIPPED_DIRS.has(entry.name)) continue;
		found.push(...(await findManifestPaths(path.join(dir, entry.name), depth + 1)));
	}
	return found;
}

async function readWorkerManifest(manifestPath: string): Promise<DiscoveredWorker | undefined> {
	try {
		const raw = await readFile(manifestPath, "utf8");
		const parsed = JSON.parse(raw) as WorkerManifest;
		const rootDir = path.dirname(manifestPath);
		const fallbackName = path.basename(rootDir);
		const name = cleanText(parsed.name) || fallbackName;
		const adminUrl = normalizeAdminURL(cleanText(parsed.admin_url) || cleanText(parsed.adminUrl) || process.env.OUVRIER_ADMIN_URL || DEFAULT_ADMIN_URL);
		return {
			id: slugify(name),
			name,
			description: cleanText(parsed.description),
			events: cleanStringArray(parsed.events),
			outcomes: cleanStringArray(parsed.outcomes),
			adminUrl,
			adminTokenEnv: cleanText(parsed.admin_token_env) || cleanText(parsed.adminTokenEnv) || undefined,
			manifestPath,
			rootDir,
			lastEventID: 0,
		};
	} catch {
		return undefined;
	}
}

function uniquifyWorkerIDs(workers: DiscoveredWorker[]): DiscoveredWorker[] {
	const seen = new Map<string, number>();
	return workers.map((worker) => {
		const count = seen.get(worker.id) ?? 0;
		seen.set(worker.id, count + 1);
		if (count === 0) return worker;
		return { ...worker, id: `${worker.id}-${count + 1}` };
	});
}

function cleanStringArray(value: unknown): string[] {
	if (!Array.isArray(value)) return [];
	return value.map(cleanText).filter(Boolean);
}

function cleanText(value: unknown): string {
	return typeof value === "string" ? value.trim() : "";
}

function normalizeAdminURL(value: string): string {
	const trimmed = value.trim() || DEFAULT_ADMIN_URL;
	return trimmed.endsWith("/") ? trimmed.slice(0, -1) : trimmed;
}

function slugify(value: string): string {
	const slug = value
		.toLowerCase()
		.replace(/[^a-z0-9]+/g, "-")
		.replace(/^-|-$/g, "");
	return slug || "ouvrier-worker";
}

export async function isDirectory(value: string): Promise<boolean> {
	try {
		return (await stat(value)).isDirectory();
	} catch {
		return false;
	}
}
