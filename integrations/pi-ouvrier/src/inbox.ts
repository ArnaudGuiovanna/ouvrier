import type { AdminEvent, DiscoveredWorker, InboxAction, InboxMessage, InboxSeverity } from "./types";

const MAX_MESSAGES = 200;

const HIGH_EVENTS = new Set([
	"pipeline_failed",
	"pipe_failed",
	"llm_call_failed",
	"tool_call_failed",
	"schema_repair_failed",
	"budget_exceeded",
	"stream_dead_lettered",
	"task_failed",
]);

const MEDIUM_EVENTS = new Set([
	"approval_requested",
	"execution_suspended",
	"hook_failed",
	"schema_validation_failed",
]);

const INFO_EVENTS = new Set(["sink_logged"]);

export class OuvrierInbox {
	private messages: InboxMessage[] = [];
	private readonly listeners = new Set<() => void>();

	list(): InboxMessage[] {
		return [...this.messages].sort((a, b) => b.at.localeCompare(a.at));
	}

	unreadCount(): number {
		return this.messages.filter((message) => !message.read).length;
	}

	add(message: InboxMessage): boolean {
		if (this.messages.some((existing) => existing.id === message.id)) return false;
		this.messages.unshift(message);
		this.messages = this.messages.slice(0, MAX_MESSAGES);
		this.notify();
		return true;
	}

	hydrate(messages: InboxMessage[]): void {
		for (const message of messages) {
			if (!message.id || !message.worker || !message.title) continue;
			if (this.messages.some((existing) => existing.id === message.id)) continue;
			this.messages.push(message);
		}
		this.messages = this.list().slice(0, MAX_MESSAGES);
		this.notify();
	}

	get(id: string): InboxMessage | undefined {
		return this.messages.find((message) => message.id === id);
	}

	markRead(id: string): void {
		const message = this.get(id);
		if (message) message.read = true;
		this.notify();
	}

	markAllRead(): void {
		for (const message of this.messages) message.read = true;
		this.notify();
	}

	dismiss(id: string): void {
		this.messages = this.messages.filter((message) => message.id !== id);
		this.notify();
	}

	onChange(listener: () => void): () => void {
		this.listeners.add(listener);
		return () => this.listeners.delete(listener);
	}

	private notify(): void {
		for (const listener of this.listeners) listener();
	}
}

export function eventToInboxMessage(worker: DiscoveredWorker, event: AdminEvent): InboxMessage | undefined {
	if (!shouldSurfaceEvent(event)) return undefined;

	const feedback = feedbackPayload(event.payload);
	const severity = normalizeSeverity(feedback.severity) ?? severityForEvent(event);
	const body = feedback.body || genericEventBody(event);
	const title = feedback.title || titleForEvent(event);
	const actions = actionsForEvent(event, severity, feedback.actions);

	return {
		id: `${worker.id}:${event.id || event.exec_id || event.trace_id || event.kind}`,
		workerID: worker.id,
		worker: worker.name,
		severity,
		title,
		body,
		at: event.at || new Date().toISOString(),
		read: false,
		execID: event.exec_id,
		traceID: event.trace_id,
		sessionID: event.session_id,
		eventID: event.id,
		eventKind: event.kind,
		actions,
	};
}

export function workerWantsPiEvent(worker: DiscoveredWorker, eventName: string): boolean {
	return worker.events.some((event) => event === eventName || event === "pi.*" || event === "*");
}

function shouldSurfaceEvent(event: AdminEvent): boolean {
	if (INFO_EVENTS.has(event.kind)) return true;
	if (HIGH_EVENTS.has(event.kind) || MEDIUM_EVENTS.has(event.kind)) return true;
	if (event.kind === "permission_decision") return permissionWasDenied(event.payload);
	return false;
}

function severityForEvent(event: AdminEvent): InboxSeverity {
	if (HIGH_EVENTS.has(event.kind)) return "high";
	if (MEDIUM_EVENTS.has(event.kind)) return "medium";
	return "info";
}

function titleForEvent(event: AdminEvent): string {
	switch (event.kind) {
		case "sink_logged":
			return "Retour du worker";
		case "approval_requested":
			return "Approbation demandée";
		case "execution_suspended":
			return "Exécution suspendue";
		case "schema_validation_failed":
			return "Validation de schéma échouée";
		case "budget_exceeded":
			return "Budget dépassé";
		case "stream_dead_lettered":
			return "Message stream envoyé en DLQ";
		case "pipeline_failed":
			return "Pipeline échoué";
		case "tool_call_failed":
			return "Outil échoué";
		case "llm_call_failed":
			return "Appel modèle échoué";
		default:
			return event.kind.replaceAll("_", " ");
	}
}

function genericEventBody(event: AdminEvent): string {
	const payload = event.payload ?? {};
	const candidates = [payload.output, payload.input, payload.message, payload.error, payload.reason];
	for (const candidate of candidates) {
		if (typeof candidate === "string" && candidate.trim()) return candidate.trim();
	}
	if (Object.keys(payload).length > 0) return JSON.stringify(payload, null, 2);
	return event.exec_id ? `Exécution ${event.exec_id}` : event.kind;
}

function feedbackPayload(payload: Record<string, unknown> | undefined): {
	title?: string;
	body?: string;
	severity?: string;
	actions?: InboxAction[];
} {
	if (!payload) return {};
	const raw = payload.output ?? payload.input ?? payload.feedback ?? payload.message;
	const parsed = parseMaybeJSON(raw);
	if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
		const record = parsed as Record<string, unknown>;
		const title = stringField(record, "title") || stringField(record, "summary") || stringField(record, "subject");
		const body = stringField(record, "body") || stringField(record, "message") || stringField(record, "details") || JSON.stringify(record, null, 2);
		return {
			title,
			body,
			severity: stringField(record, "severity") || stringField(record, "level"),
			actions: actionFields(record.actions),
		};
	}
	if (typeof raw === "string" && raw.trim()) return { body: raw.trim() };
	return {};
}

function parseMaybeJSON(value: unknown): unknown {
	if (typeof value !== "string") return value;
	const trimmed = value.trim();
	if (!trimmed || (!trimmed.startsWith("{") && !trimmed.startsWith("["))) return undefined;
	try {
		return JSON.parse(trimmed) as unknown;
	} catch {
		return undefined;
	}
}

function stringField(record: Record<string, unknown>, key: string): string | undefined {
	const value = record[key];
	return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function actionFields(value: unknown): InboxAction[] | undefined {
	if (!Array.isArray(value)) return undefined;
	const allowed = new Set<InboxAction>(["fix_with_pi", "open_trace", "dismiss", "mark_read"]);
	return value.filter((item): item is InboxAction => typeof item === "string" && allowed.has(item as InboxAction));
}

function normalizeSeverity(value: string | undefined): InboxSeverity | undefined {
	if (!value) return undefined;
	const normalized = value.toLowerCase();
	if (normalized === "info" || normalized === "low" || normalized === "medium" || normalized === "high") return normalized;
	if (normalized === "warning" || normalized === "warn") return "medium";
	if (normalized === "error" || normalized === "critical") return "high";
	return undefined;
}

function actionsForEvent(event: AdminEvent, severity: InboxSeverity, explicit?: InboxAction[]): InboxAction[] {
	const actions = new Set<InboxAction>(explicit ?? []);
	if (event.exec_id) actions.add("open_trace");
	if (severity === "medium" || severity === "high") actions.add("fix_with_pi");
	actions.add("dismiss");
	return [...actions];
}

function permissionWasDenied(payload: Record<string, unknown> | undefined): boolean {
	if (!payload) return false;
	const decision = payload.decision;
	return typeof decision === "string" && ["deny", "denied", "blocked"].includes(decision.toLowerCase());
}
