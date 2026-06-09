import { withTimeout } from "./ouvrier-client";
import { clientFor, plansFor, workers } from "./state";
import type {
	AdminPlan,
	AdminPlanTrigger,
	AdminTriggerRequest,
	AdminTriggerResponse,
	DiscoveredWorker,
} from "./types";

export async function triggerWorker(
	worker: DiscoveredWorker,
	eventName: string,
	body: unknown,
	parentSignal?: AbortSignal,
): Promise<AdminTriggerResponse> {
	const request = triggerRequestForWorker(worker, eventName, body);
	const timeout = withTimeout(parentSignal, 6000);
	try {
		return await clientFor(worker).trigger(request, timeout.signal);
	} finally {
		timeout.cleanup();
	}
}

export function triggerResultLine(worker: DiscoveredWorker, result: AdminTriggerResponse): string {
	const ids = [result.exec_id && `exec=${result.exec_id}`, result.trace_id && `trace=${result.trace_id}`].filter(Boolean).join(" ");
	return `${worker.name}: ${result.status ?? "inconnu"}${ids ? ` (${ids})` : ""}`;
}

export function parseJSONOrText(value: string): unknown {
	const trimmed = value.trim();
	if (!trimmed) return {};
	try {
		return JSON.parse(trimmed) as unknown;
	} catch {
		return { text: trimmed };
	}
}

export function splitFirst(value: string): [string, string] {
	const trimmed = value.trim();
	if (!trimmed) return ["", ""];
	const index = trimmed.search(/\s/);
	if (index === -1) return [trimmed, ""];
	return [trimmed.slice(0, index), trimmed.slice(index + 1).trim()];
}

export function triggerRequestForWorker(worker: DiscoveredWorker, eventName: string, body: unknown): AdminTriggerRequest {
	const plan = choosePlan(worker, eventName);
	const trigger = plan?.trigger ?? triggerFromEvent(eventName) ?? triggerFromWorkerManifest(worker) ?? {};
	const kind = String(trigger.kind ?? "http").toLowerCase();
	const envelope = {
		event: eventName || "manual",
		source: "pi",
		worker: worker.name,
		at: new Date().toISOString(),
		body,
	};

	if (kind === "cron") {
		return { trigger: "cron", expr: String(trigger.expr ?? trigger.value ?? ""), scheduled_at: new Date().toISOString(), body: envelope };
	}
	if (kind === "stream") {
		return {
			trigger: "stream",
			uri: String(trigger.uri ?? ""),
			id: `${eventName || "manual"}:${Date.now()}`,
			metadata: { source: "pi", event: eventName || "manual" },
			body: envelope,
		};
	}
	return {
		trigger: kind === "webhook" ? "webhook" : "http",
		method: String(trigger.method ?? "POST").toUpperCase(),
		path: concretePath(String(trigger.path ?? "/")),
		body: envelope,
	};
}

function choosePlan(worker: DiscoveredWorker, eventName: string): AdminPlan | undefined {
	const plans = plansFor(worker);
	const target = triggerFromEvent(eventName);
	if (target) {
		const match = plans.find((plan) => triggerMatches(plan.trigger, target));
		if (match) return match;
	}
	return plans[0];
}

function triggerFromWorkerManifest(worker: DiscoveredWorker): AdminPlanTrigger | undefined {
	for (const event of worker.events) {
		const trigger = triggerFromEvent(event);
		if (trigger) return trigger;
	}
	return undefined;
}

function triggerFromEvent(eventName: string): AdminPlanTrigger | undefined {
	const raw = eventName.trim();
	let match = raw.match(/^http\.(GET|POST)\s+(.+)$/i) ?? raw.match(/^(GET|POST)\s+(.+)$/i);
	if (match) return { kind: "http", method: match[1]!.toUpperCase(), path: match[2]!.trim() };
	match = raw.match(/^webhook\s+([a-z0-9_-]+)$/i);
	if (match) return { kind: "webhook", method: "POST", path: `/webhooks/${match[1]}` };
	match = raw.match(/^cron\s+(.+)$/i);
	if (match) return { kind: "cron", expr: match[1]!.trim() };
	match = raw.match(/^stream\s+(.+)$/i);
	if (match) return { kind: "stream", uri: match[1]!.trim() };
	return undefined;
}

function triggerMatches(planTrigger: AdminPlanTrigger | undefined, target: AdminPlanTrigger): boolean {
	if (!planTrigger) return false;
	const kind = String(planTrigger.kind ?? "").toLowerCase();
	const targetKind = String(target.kind ?? "").toLowerCase();
	if (kind !== targetKind) return false;
	if (targetKind === "http" || targetKind === "webhook") {
		return String(planTrigger.method ?? "").toUpperCase() === String(target.method ?? "").toUpperCase() && planTrigger.path === target.path;
	}
	if (targetKind === "cron") return planTrigger.expr === target.expr || planTrigger.value === target.expr;
	if (targetKind === "stream") return planTrigger.uri === target.uri;
	return false;
}

function concretePath(path: string): string {
	return path.replace(/\{[^/]+\}/g, "pi");
}

export function ovrCompletions(prefix: string): Array<{ value: string; label: string }> | null {
	const commands = ["workers", "inbox", "trigger", "trace", "health", "compose", "read-all"];
	const parts = prefix.trimStart().split(/\s+/);
	if (parts.length <= 1) {
		const needle = parts[0] ?? "";
		const items = commands.filter((command) => command.startsWith(needle)).map((command) => ({ value: command, label: command }));
		return items.length > 0 ? items : null;
	}
	if (parts[0] === "trigger") {
		const needle = parts[1] ?? "";
		const items = workers
			.filter((worker) => worker.id.startsWith(needle) || worker.name.toLowerCase().startsWith(needle.toLowerCase()))
			.map((worker) => ({ value: `trigger ${worker.id}`, label: worker.name }));
		return items.length > 0 ? items : null;
	}
	return null;
}
