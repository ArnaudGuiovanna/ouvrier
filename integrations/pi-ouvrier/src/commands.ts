import type { ExtensionAPI, ExtensionCommandContext, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { InboxPanel, MessageDetailPanel, TracePanel } from "./tui";
import {
	fetchTrace,
	findWorker,
	inbox,
	plansFor,
	refreshWorkers,
	updateStatus,
	workerLine,
	workers,
} from "./state";
import { parseJSONOrText, splitFirst, triggerResultLine, triggerWorker } from "./trigger";
import type { AdminTraceResponse, DiscoveredWorker, InboxMessage, InboxPanelAction } from "./types";

export async function handleOvrCommand(pi: ExtensionAPI, args: string, ctx: ExtensionCommandContext): Promise<void> {
	const [subcommand, rest] = splitFirst(args.trim());
	switch (subcommand || "help") {
		case "workers":
			await showWorkers(ctx);
			return;
		case "inbox":
			await openInbox(pi, ctx);
			return;
		case "trigger":
			await triggerFromCommand(rest, ctx);
			return;
		case "trace":
			await traceFromCommand(rest, ctx);
			return;
		case "health":
			await showHealth(ctx);
			return;
		case "compose":
			composeWorker(pi, rest, ctx);
			return;
		case "read-all":
			inbox.markAllRead();
			updateStatus(ctx);
			notifyOrLog(ctx, "Inbox Ouvrier marquée comme lue", "info");
			return;
		default:
			await showHelp(ctx);
	}
}

export async function openInbox(pi: ExtensionAPI, ctx: ExtensionContext): Promise<void> {
	if (!ctx.hasUI) {
		writeConsole(formatInboxMessages(inbox.list()));
		return;
	}
	let keepOpen = true;
	while (keepOpen) {
		const action = await ctx.ui.custom<InboxPanelAction>(
			(_tui, theme, _keybindings, done) => new InboxPanel(inbox.list(), theme, done),
			{
				overlay: true,
				overlayOptions: { anchor: "right-center", width: "46%", minWidth: 44, maxHeight: "90%", margin: 1 },
			},
		);
		if (!action || action.kind === "close") return;
		keepOpen = await handleInboxAction(pi, ctx, action);
	}
}

async function showWorkers(ctx: ExtensionCommandContext): Promise<void> {
	await refreshWorkers(ctx, false);
	if (workers.length === 0) {
		notifyOrLog(ctx, "Aucun manifest ouvrier.worker.json trouvé dans ce workspace", "error");
		return;
	}
	await showText(ctx, "Workers Ouvrier", workers.map(formatWorkerDetails).join("\n\n"));
}

async function showHealth(ctx: ExtensionCommandContext): Promise<void> {
	await refreshWorkers(ctx, ctx.hasUI);
	if (workers.length === 0) {
		notifyOrLog(ctx, "Aucun worker Ouvrier découvert", "error");
		return;
	}
	await showText(ctx, "Santé Ouvrier", workers.map(workerLine).join("\n"));
}

async function handleInboxAction(pi: ExtensionAPI, ctx: ExtensionContext, action: InboxPanelAction): Promise<boolean> {
	const message = "messageID" in action ? inbox.get(action.messageID) : undefined;
	if (!message) return action.kind !== "close";

	switch (action.kind) {
		case "dismiss":
			inbox.dismiss(message.id);
			return true;
		case "read":
			inbox.markRead(message.id);
			return true;
		case "trace":
			await openTraceForMessage(ctx, message);
			return true;
		case "fix":
			inbox.markRead(message.id);
			queueFixWithPi(pi, ctx, message);
			return false;
		case "detail": {
			inbox.markRead(message.id);
			const next = await ctx.ui.custom<InboxPanelAction>(
				(_tui, theme, _keybindings, done) => new MessageDetailPanel(message, theme, done),
				{
					overlay: true,
					overlayOptions: { anchor: "right-center", width: "50%", minWidth: 54, maxHeight: "92%", margin: 1 },
				},
			);
			if (!next || next.kind === "close") return true;
			return handleInboxAction(pi, ctx, next);
		}
		default:
			return false;
	}
}

async function triggerFromCommand(args: string, ctx: ExtensionCommandContext): Promise<void> {
	await refreshWorkers(ctx, false);
	const [workerArg, rest] = splitFirst(args.trim());
	let worker = workerArg ? findWorker(workerArg) : undefined;
	let remaining = rest;
	if (!worker) {
		if (!ctx.hasUI) {
			notifyOrLog(ctx, "Usage : /ovr trigger <worker> [event] [json]", "error");
			return;
		}
		worker = await selectWorker(ctx);
		remaining = args.trim();
	}
	if (!worker) return;
	const [eventName, bodyText] = splitEventAndBody(remaining);
	try {
		const result = await triggerWorker(worker, eventName || "manual", parseJSONOrText(bodyText), ctx.signal);
		if (ctx.hasUI) {
			ctx.ui.notify(triggerResultLine(worker, result), result.status === "ok" || result.status === "accepted" ? "info" : "error");
		} else {
			writeConsole(JSON.stringify(result, null, 2));
		}
	} catch (error) {
		notifyOrLog(ctx, `Déclenchement échoué : ${errorMessage(error)}`, "error");
	}
}

async function traceFromCommand(args: string, ctx: ExtensionCommandContext): Promise<void> {
	await refreshWorkers(ctx, false);
	const [first, second] = splitFirst(args.trim());
	if (!first) {
		notifyOrLog(ctx, "Usage : /ovr trace <exec_id> ou /ovr trace <worker> <exec_id>", "error");
		return;
	}
	const worker = second ? findWorker(first) : undefined;
	const execID = second || first;
	const trace = await fetchTrace(worker, execID, ctx.signal);
	if (!trace) {
		notifyOrLog(ctx, `Trace introuvable : ${execID}`, "error");
		return;
	}
	await showTrace(ctx, trace);
}

function composeWorker(pi: ExtensionAPI, rest: string, ctx: ExtensionCommandContext): void {
	const goal = rest.trim() || "Demande-moi quel worker Ouvrier asynchrone je veux, puis scaffold-le comme du Go normal.";
	const prompt = [
		"Conçois et implémente un nouveau worker Ouvrier asynchrone pour ce workspace.",
		"Garde Pi comme agent de développement synchrone au premier plan et Ouvrier comme middleware d'arrière-plan.",
		"Le worker doit être un projet Go Ouvrier normal, éditable, avec ouvrier.worker.json.",
		`Objectif : ${goal}`,
	].join("\n");
	sendUserMessage(pi, ctx, prompt);
}

async function selectWorker(ctx: ExtensionCommandContext): Promise<DiscoveredWorker | undefined> {
	if (workers.length === 0) {
		notifyOrLog(ctx, "Aucun worker Ouvrier découvert", "error");
		return undefined;
	}
	const items = workers.map((worker) => `${worker.id} · ${worker.name}`);
	const selected = await ctx.ui.select("Sélectionner un worker Ouvrier", items);
	if (!selected) return undefined;
	return workers[items.indexOf(selected)];
}

async function showHelp(ctx: ExtensionCommandContext): Promise<void> {
	await showText(
		ctx,
		"Commandes Ouvrier",
		[
			"/ovr workers  - lister les workers découverts",
			"/ovr inbox    - ouvrir/lire la messagerie des workers",
			"/ovr trigger <worker> [event] [json] - déclencher un worker",
			"/ovr trace <exec_id> - ouvrir/afficher une trace",
			"/ovr health   - rafraîchir santé/capacités",
			"/ovr compose [objectif] - créer un worker avec Pi",
			"/ovr read-all - marquer l'Inbox Ouvrier comme lue",
		].join("\n"),
	);
}

async function openTraceForMessage(ctx: ExtensionContext, message: InboxMessage): Promise<void> {
	if (!message.execID) {
		notifyOrLog(ctx, "Aucun exec_id attaché à ce message", "error");
		return;
	}
	const worker = workers.find((item) => item.id === message.workerID);
	const trace = await fetchTrace(worker, message.execID, ctx.signal);
	if (!trace) {
		notifyOrLog(ctx, `Trace introuvable : ${message.execID}`, "error");
		return;
	}
	await showTrace(ctx, trace);
}

async function showTrace(ctx: ExtensionContext, trace: AdminTraceResponse): Promise<void> {
	if (!ctx.hasUI) {
		writeConsole(JSON.stringify(trace, null, 2));
		return;
	}
	await ctx.ui.custom<void>((_tui, theme, _keybindings, done) => new TracePanel(trace, theme, done), {
		overlay: true,
		overlayOptions: { anchor: "right-center", width: "56%", minWidth: 58, maxHeight: "92%", margin: 1 },
	});
}

function queueFixWithPi(pi: ExtensionAPI, ctx: ExtensionContext, message: InboxMessage): void {
	const prompt = [
		`Un worker Ouvrier asynchrone (${message.worker}) a signalé :`,
		`Sévérité : ${message.severity}`,
		`Titre : ${message.title}`,
		message.execID ? `Exécution : ${message.execID}` : "",
		"",
		message.body,
		"",
		"Analyse ce feedback. Propose d'abord un correctif ; si des changements de code sont nécessaires, garde-les ciblés et explique le plan.",
	]
		.filter(Boolean)
		.join("\n");
	sendUserMessage(pi, ctx, prompt);
}

function sendUserMessage(pi: ExtensionAPI, ctx: ExtensionContext, prompt: string): void {
	if (ctx.isIdle()) pi.sendUserMessage(prompt);
	else pi.sendUserMessage(prompt, { deliverAs: "followUp" });
}

async function showText(ctx: ExtensionContext, title: string, text: string): Promise<void> {
	if (ctx.hasUI) await ctx.ui.confirm(title, text);
	else writeConsole(text);
}

function notifyOrLog(ctx: ExtensionContext, text: string, severity: "info" | "warning" | "error"): void {
	if (ctx.hasUI) ctx.ui.notify(text, severity);
	else writeConsole(text);
}

function formatWorkerDetails(worker: DiscoveredWorker): string {
	const plans = plansFor(worker);
	return [
		`${worker.name} (${worker.id})`,
		worker.description,
		`admin : ${worker.adminUrl}`,
		`santé : ${worker.health?.status ?? "inconnue"}`,
		`événements : ${worker.events.join(", ") || "-"}`,
		`résultats : ${worker.outcomes.join(", ") || "-"}`,
		`plans : ${plans.length}`,
	]
		.filter(Boolean)
		.join("\n");
}

function formatInboxMessages(messages: InboxMessage[]): string {
	if (messages.length === 0) return "Inbox Ouvrier vide";
	return messages
		.map((message) =>
			[
				`[${message.severity}] ${message.worker} · ${message.title}`,
				message.at,
				message.body,
				message.execID ? `exec_id : ${message.execID}` : "",
			].filter(Boolean).join("\n"),
		)
		.join("\n\n");
}

function splitEventAndBody(value: string): [string, string] {
	const trimmed = value.trim();
	if (!trimmed) return ["", ""];

	const quote = trimmed[0];
	if (quote === '"' || quote === "'") {
		const end = trimmed.indexOf(quote, 1);
		if (end > 0) return [trimmed.slice(1, end), trimmed.slice(end + 1).trim()];
	}

	const jsonStart = firstJSONStart(trimmed);
	if (jsonStart === 0) return ["", trimmed];
	if (jsonStart > 0) return [trimmed.slice(0, jsonStart).trim(), trimmed.slice(jsonStart).trim()];
	return splitFirst(trimmed);
}

function firstJSONStart(value: string): number {
	for (let i = 0; i < value.length; i++) {
		const char = value[i];
		if ((char === "{" || char === "[") && (i === 0 || /\s/.test(value[i - 1] ?? ""))) return i;
	}
	return -1;
}

function writeConsole(text: string): void {
	console.log(text);
}

function errorMessage(error: unknown): string {
	return error instanceof Error ? error.message : String(error);
}
