import type {
	AdminCapabilitiesResponse,
	AdminEvent,
	AdminHealthResponse,
	AdminTraceResponse,
	AdminTriggerRequest,
	AdminTriggerResponse,
	DiscoveredWorker,
} from "./types";

export class OuvrierHTTPError extends Error {
	readonly status: number;
	readonly body: string;

	constructor(status: number, body: string) {
		super(`Ouvrier HTTP ${status}: ${body || "<body vide>"}`);
		this.name = "OuvrierHTTPError";
		this.status = status;
		this.body = body;
	}
}

export class OuvrierClient {
	readonly worker: DiscoveredWorker;
	private readonly token?: string;

	constructor(worker: DiscoveredWorker) {
		this.worker = worker;
		this.token = resolveAdminToken(worker);
	}

	health(signal?: AbortSignal): Promise<AdminHealthResponse> {
		return this.getJSON<AdminHealthResponse>("/admin/health", signal);
	}

	capabilities(signal?: AbortSignal): Promise<AdminCapabilitiesResponse> {
		return this.getJSON<AdminCapabilitiesResponse>("/admin/capabilities", signal);
	}

	trigger(request: AdminTriggerRequest, signal?: AbortSignal): Promise<AdminTriggerResponse> {
		return this.postJSON<AdminTriggerResponse>("/admin/trigger", request, signal);
	}

	trace(execID: string, signal?: AbortSignal): Promise<AdminTraceResponse> {
		return this.getJSON<AdminTraceResponse>(`/admin/traces/${encodeURIComponent(execID)}`, signal);
	}

	async streamEvents(afterID: number, onEvent: (event: AdminEvent) => void, signal?: AbortSignal): Promise<void> {
		const url = this.url("/admin/events");
		url.searchParams.set("format", "sse");
		if (afterID > 0) url.searchParams.set("after_id", String(afterID));

		const response = await fetch(url, {
			method: "GET",
			headers: this.headers({ Accept: "text/event-stream, application/x-ndjson" }),
			signal,
		});
		if (!response.ok) throw new OuvrierHTTPError(response.status, await safeBody(response));
		if (!response.body) return;

		const contentType = response.headers.get("content-type") ?? "";
		if (contentType.includes("text/event-stream")) {
			await parseSSE(response.body, onEvent, signal);
			return;
		}
		await parseJSONL(response.body, onEvent, signal);
	}

	private async getJSON<T>(route: string, signal?: AbortSignal): Promise<T> {
		const response = await fetch(this.url(route), {
			method: "GET",
			headers: this.headers({ Accept: "application/json" }),
			signal,
		});
		return decodeJSONResponse<T>(response);
	}

	private async postJSON<T>(route: string, body: unknown, signal?: AbortSignal): Promise<T> {
		const response = await fetch(this.url(route), {
			method: "POST",
			headers: this.headers({ Accept: "application/json", "Content-Type": "application/json" }),
			body: JSON.stringify(body),
			signal,
		});
		return decodeJSONResponse<T>(response);
	}

	private url(route: string): URL {
		return new URL(route, `${this.worker.adminUrl}/`);
	}

	private headers(extra: Record<string, string>): Record<string, string> {
		if (!this.token) return extra;
		return { ...extra, Authorization: `Bearer ${this.token}` };
	}
}

export function resolveAdminToken(worker: DiscoveredWorker): string | undefined {
	const envName = worker.adminTokenEnv?.trim();
	if (envName && process.env[envName]) return process.env[envName];
	return process.env.OUVRIER_ADMIN_TOKEN || process.env.PIP_ADMIN_TOKEN || undefined;
}

export function withTimeout(parent: AbortSignal | undefined, timeoutMS: number): { signal: AbortSignal; cleanup: () => void } {
	const controller = new AbortController();
	const abort = () => controller.abort();
	const timeout = setTimeout(abort, timeoutMS);
	if (parent) {
		if (parent.aborted) controller.abort();
		else parent.addEventListener("abort", abort, { once: true });
	}
	return {
		signal: controller.signal,
		cleanup: () => {
			clearTimeout(timeout);
			if (parent) parent.removeEventListener("abort", abort);
		},
	};
}

export async function sleep(ms: number, signal?: AbortSignal): Promise<void> {
	if (signal?.aborted) return;
	await new Promise<void>((resolve) => {
		let abort: (() => void) | undefined;
		const done = () => {
			if (abort && signal) signal.removeEventListener("abort", abort);
			resolve();
		};
		const timeout = setTimeout(done, ms);
		if (signal) {
			abort = () => {
				clearTimeout(timeout);
				done();
			};
			signal.addEventListener("abort", abort, { once: true });
		}
	});
}

async function decodeJSONResponse<T>(response: Response): Promise<T> {
	const body = await safeBody(response);
	if (!response.ok) throw new OuvrierHTTPError(response.status, body);
	if (!body.trim()) return {} as T;
	return JSON.parse(body) as T;
}

async function safeBody(response: Response): Promise<string> {
	try {
		return await response.text();
	} catch {
		return "";
	}
}

async function parseSSE(stream: ReadableStream<Uint8Array>, onEvent: (event: AdminEvent) => void, signal?: AbortSignal): Promise<void> {
	const reader = stream.getReader();
	const decoder = new TextDecoder();
	let buffer = "";
	while (!signal?.aborted) {
		const { done, value } = await reader.read();
		if (done) break;
		buffer += decoder.decode(value, { stream: true });
		const parts = buffer.split(/\r?\n\r?\n/);
		buffer = parts.pop() ?? "";
		for (const part of parts) emitSSEBlock(part, onEvent);
	}
	if (buffer.trim()) emitSSEBlock(buffer, onEvent);
}

function emitSSEBlock(block: string, onEvent: (event: AdminEvent) => void): void {
	const dataLines: string[] = [];
	let id: number | undefined;
	for (const line of block.split(/\r?\n/)) {
		if (line.startsWith("data:")) dataLines.push(line.slice(5).trimStart());
		else if (line.startsWith("id:")) {
			const parsed = Number.parseInt(line.slice(3).trim(), 10);
			if (Number.isFinite(parsed)) id = parsed;
		}
	}
	const data = dataLines.join("\n").trim();
	if (!data) return;
	try {
		const event = JSON.parse(data) as AdminEvent;
		if (id !== undefined && !event.id) event.id = id;
		onEvent(event);
	} catch {
		// Ignore les frames invalides ; le runtime continue de streamer les événements suivants.
	}
}

async function parseJSONL(stream: ReadableStream<Uint8Array>, onEvent: (event: AdminEvent) => void, signal?: AbortSignal): Promise<void> {
	const reader = stream.getReader();
	const decoder = new TextDecoder();
	let buffer = "";
	while (!signal?.aborted) {
		const { done, value } = await reader.read();
		if (done) break;
		buffer += decoder.decode(value, { stream: true });
		const lines = buffer.split(/\r?\n/);
		buffer = lines.pop() ?? "";
		for (const line of lines) emitJSONLLine(line, onEvent);
	}
	if (buffer.trim()) emitJSONLLine(buffer, onEvent);
}

function emitJSONLLine(line: string, onEvent: (event: AdminEvent) => void): void {
	const trimmed = line.trim();
	if (!trimmed) return;
	try {
		onEvent(JSON.parse(trimmed) as AdminEvent);
	} catch {
		// Ignore les lignes invalides ; le runtime continue de streamer les événements suivants.
	}
}
