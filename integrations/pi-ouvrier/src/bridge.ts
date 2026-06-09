import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { workerWantsPiEvent } from "./inbox";
import { updateStatus, workers } from "./state";
import { triggerWorker } from "./trigger";
import type { PiBridgeEventName } from "./types";

export function registerBridgeEvents(pi: ExtensionAPI): void {
	pi.on("agent_end", async (event, ctx) => {
		void (async () => {
			await emitPiBridgeEvent(ctx, "pi.agent_end", {
				summary: summarizeMessages((event as { messages?: unknown[] }).messages),
				git: await gitSnapshot(pi),
			});
		})();
	});

	pi.on("turn_end", async (event, ctx) => {
		void emitPiBridgeEvent(ctx, "pi.turn_end", {
			turn_index: (event as { turnIndex?: number }).turnIndex,
			tool_results: summarizeToolResults((event as { toolResults?: unknown[] }).toolResults),
		});
	});

	pi.on("tool_execution_end", async (event, ctx) => {
		void emitPiBridgeEvent(ctx, "pi.tool_execution_end", {
			tool_call_id: (event as { toolCallId?: string }).toolCallId,
			tool_name: (event as { toolName?: string }).toolName,
			is_error: (event as { isError?: boolean }).isError,
		});
	});
}

async function emitPiBridgeEvent(ctx: ExtensionContext, eventName: PiBridgeEventName, payload: Record<string, unknown>): Promise<void> {
	const interested = workers.filter((worker) => workerWantsPiEvent(worker, eventName));
	if (interested.length === 0) return;
	const envelope = {
		event: eventName,
		source: "pi",
		workspace: ctx.cwd,
		at: new Date().toISOString(),
		payload,
	};
	await Promise.allSettled(interested.map((worker) => triggerWorker(worker, eventName, envelope, ctx.signal)));
	updateStatus(ctx);
}

async function gitSnapshot(pi: ExtensionAPI): Promise<Record<string, string>> {
	const [branch, status, diffStat] = await Promise.all([
		execText(pi, "git", ["branch", "--show-current"]),
		execText(pi, "git", ["status", "--short"]),
		execText(pi, "git", ["diff", "--stat"]),
	]);
	return { branch, status: truncate(status, 4000), diff_stat: truncate(diffStat, 4000) };
}

async function execText(pi: ExtensionAPI, command: string, args: string[]): Promise<string> {
	try {
		const result = await pi.exec(command, args, { timeout: 2500 });
		return String(result.stdout ?? "").trim();
	} catch {
		return "";
	}
}

function summarizeMessages(messages: unknown[] | undefined): Array<Record<string, string>> {
	if (!Array.isArray(messages)) return [];
	return messages.slice(-6).map((message) => {
		const record = typeof message === "object" && message ? (message as Record<string, unknown>) : {};
		return { role: String(record.role ?? "unknown"), text: truncate(extractText(record.content), 2000) };
	});
}

function summarizeToolResults(results: unknown[] | undefined): Array<Record<string, unknown>> {
	if (!Array.isArray(results)) return [];
	return results.slice(-10).map((result) => {
		const record = typeof result === "object" && result ? (result as Record<string, unknown>) : {};
		return {
			tool: record.toolName ?? record.tool_name ?? record.name,
			is_error: record.isError ?? record.is_error,
			text: truncate(extractText(record.content), 1000),
		};
	});
}

function extractText(content: unknown): string {
	if (typeof content === "string") return content;
	if (Array.isArray(content)) {
		return content
			.map((item) => {
				if (typeof item === "string") return item;
				if (typeof item === "object" && item && "text" in item) return String((item as { text?: unknown }).text ?? "");
				return "";
			})
			.join("\n");
	}
	return "";
}

function truncate(value: string, max: number): string {
	return value.length <= max ? value : `${value.slice(0, max)}…`;
}
