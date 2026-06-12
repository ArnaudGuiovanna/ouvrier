import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { discoverWorkers } from "./manifest";
import { eventToInboxMessage, OuvrierInbox } from "./inbox";
import { OuvrierClient, sleep, withTimeout } from "./ouvrier-client";
import type { AdminEvent, AdminTraceResponse, DiscoveredWorker, InboxMessage } from "./types";

const INBOX_ENTRY_TYPE = "ouvrier.inbox.message";
const STATUS_KEY = "ouvrier";
const STREAM_RECONNECT_MS = 1500;

export const inbox = new OuvrierInbox();
export let workers: DiscoveredWorker[] = [];

const clients = new Map<string, OuvrierClient>();
const streamControllers = new Map<string, AbortController>();
let removeInboxStatusListener: (() => void) | undefined;
let appendInboxEntry: ((message: InboxMessage) => void) | undefined;

export function setupOuvrierState(pi: ExtensionAPI): void {
	appendInboxEntry = (message) => pi.appendEntry(INBOX_ENTRY_TYPE, message);
}

export function hydrateInbox(ctx: ExtensionContext): void {
	const entries = ctx.sessionManager.getEntries() as Array<{ type?: string; customType?: string; data?: unknown }>;
	const messages: InboxMessage[] = [];
	for (const entry of entries) {
		if (entry.type === "custom" && entry.customType === INBOX_ENTRY_TYPE && isInboxMessage(entry.data)) messages.push(entry.data);
	}
	inbox.hydrate(messages);
}

export function bindInboxStatus(ctx: ExtensionContext): void {
	removeInboxStatusListener?.();
	removeInboxStatusListener = inbox.onChange(() => updateStatus(ctx));
	updateStatus(ctx);
}

export function shutdownOuvrierState(): void {
	stopEventStreams();
	removeInboxStatusListener?.();
	removeInboxStatusListener = undefined;
}

export async function refreshWorkers(ctx: ExtensionContext, notify: boolean): Promise<void> {
	workers = mergeLastEventIDs(await discoverWorkers(ctx.cwd));
	clients.clear();

	await Promise.all(workers.map((worker) => enrichWorker(worker, ctx.signal)));
	startEventStreams(ctx);
	updateStatus(ctx);

	if (notify) {
		const online = workers.filter(isOnline).length;
		ctx.ui.notify(`Workers Ouvrier : ${online}/${workers.length} en ligne`, workers.length === 0 ? "error" : "info");
	}
}

export function updateStatus(ctx: ExtensionContext): void {
	if (!ctx.hasUI) return;
	const unread = inbox.unreadCount();
	const online = workers.filter(isOnline).length;
	const total = workers.length;
	const theme = ctx.ui.theme;
	const dot = unread > 0 ? theme.fg("accent", "●") : theme.fg("dim", "○");
	ctx.ui.setStatus(STATUS_KEY, `${dot} ${theme.fg("dim", `ouvrier ${online}/${total} · ${unread} non lus`)}`);
}

export function clientFor(worker: DiscoveredWorker): OuvrierClient {
	const key = streamKey(worker);
	let client = clients.get(key);
	if (!client) {
		client = new OuvrierClient(worker);
		clients.set(key, client);
	}
	return client;
}

export function streamKey(worker: DiscoveredWorker): string {
	return `${worker.id}:${worker.adminUrl}`;
}

export function isOnline(worker: DiscoveredWorker): boolean {
	return worker.health?.status === "ok";
}

export function workerLine(worker: DiscoveredWorker): string {
	const online = isOnline(worker) ? "ok" : "hors ligne";
	return `[${online}] ${worker.id} · ${worker.name} · ${worker.adminUrl}`;
}

export function workerSummary(worker: DiscoveredWorker): Record<string, unknown> {
	return {
		id: worker.id,
		name: worker.name,
		description: worker.description,
		admin_url: worker.adminUrl,
		health: worker.health?.status ?? "inconnue",
		events: worker.events,
		outcomes: worker.outcomes,
		plans: plansFor(worker).length,
	};
}

export function plansFor(worker: DiscoveredWorker) {
	return worker.capabilities?.capabilities ?? worker.capabilities?.plans ?? [];
}

export function findWorker(value: string): DiscoveredWorker | undefined {
	const query = value.trim().toLowerCase();
	return workers.find((worker) => worker.id.toLowerCase() === query || worker.name.toLowerCase() === query);
}

export async function fetchTrace(
	worker: DiscoveredWorker | undefined,
	execID: string,
	parentSignal?: AbortSignal,
): Promise<AdminTraceResponse | undefined> {
	const candidates = worker ? [worker] : workers;
	for (const candidate of candidates) {
		const timeout = withTimeout(parentSignal, 4000);
		try {
			return await clientFor(candidate).trace(execID, timeout.signal);
		} catch {
			// Try the next worker.
		} finally {
			timeout.cleanup();
		}
	}
	return undefined;
}

async function enrichWorker(worker: DiscoveredWorker, parentSignal?: AbortSignal): Promise<void> {
	const client = clientFor(worker);
	const healthTimeout = withTimeout(parentSignal, 2000);
	try {
		worker.health = await client.health(healthTimeout.signal);
	} catch (error) {
		worker.health = { status: "hors ligne", error: errorMessage(error) };
	} finally {
		healthTimeout.cleanup();
	}

	const capsTimeout = withTimeout(parentSignal, 2500);
	try {
		worker.capabilities = await client.capabilities(capsTimeout.signal);
	} catch (error) {
		worker.capabilities = { status: "hors ligne", error: errorMessage(error) };
	} finally {
		capsTimeout.cleanup();
	}
}

function mergeLastEventIDs(discovered: DiscoveredWorker[]): DiscoveredWorker[] {
	return discovered.map((worker) => {
		const previous = workers.find((item) => item.id === worker.id && item.adminUrl === worker.adminUrl);
		const inboxMax = Math.max(
			0,
			...inbox
				.list()
				.filter((message) => message.workerID === worker.id && typeof message.eventID === "number")
				.map((message) => message.eventID ?? 0),
		);
		return { ...worker, lastEventID: Math.max(previous?.lastEventID ?? 0, inboxMax) };
	});
}

function startEventStreams(ctx: ExtensionContext): void {
	const activeKeys = new Set(workers.map(streamKey));
	for (const [key, controller] of streamControllers) {
		if (!activeKeys.has(key)) {
			controller.abort();
			streamControllers.delete(key);
		}
	}

	for (const worker of workers) {
		const key = streamKey(worker);
		if (streamControllers.has(key) || !isOnline(worker)) continue;
		const controller = new AbortController();
		streamControllers.set(key, controller);
		void streamWorkerEvents(worker, controller, ctx);
	}
}

function stopEventStreams(): void {
	for (const controller of streamControllers.values()) controller.abort();
	streamControllers.clear();
}

async function streamWorkerEvents(worker: DiscoveredWorker, controller: AbortController, ctx: ExtensionContext): Promise<void> {
	const client = clientFor(worker);
	while (!controller.signal.aborted) {
		try {
			await client.streamEvents(worker.lastEventID, (event) => handleWorkerEvent(worker, event, ctx), controller.signal);
		} catch {
			// Reconnexion silencieuse : l'inbox et la ligne de statut restent non intrusives.
		}
		await sleep(STREAM_RECONNECT_MS, controller.signal);
	}
}

function handleWorkerEvent(worker: DiscoveredWorker, event: AdminEvent, ctx: ExtensionContext): void {
	if (event.id > worker.lastEventID) worker.lastEventID = event.id;
	const message = eventToInboxMessage(worker, event);
	if (!message) return;
	if (!inbox.add(message)) return;
	appendInboxEntry?.(message);
	updateStatus(ctx);
	if (message.severity === "high" && ctx.hasUI) {
		ctx.ui.notify(`[${message.worker}] ${message.title}`, "error");
	}
}

function isInboxMessage(value: unknown): value is InboxMessage {
	if (!value || typeof value !== "object") return false;
	const record = value as Record<string, unknown>;
	return typeof record.id === "string" && typeof record.worker === "string" && typeof record.title === "string";
}

function errorMessage(error: unknown): string {
	return error instanceof Error ? error.message : String(error);
}
