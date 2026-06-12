import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { findWorker, inbox, refreshWorkers, workerSummary, workers } from "./state";
import { parseJSONOrText, triggerWorker } from "./trigger";

export function registerOuvrierTools(pi: ExtensionAPI): void {
	pi.registerTool({
		name: "ouvrier_workers",
		label: "Workers Ouvrier",
		description: "Liste les workers Ouvrier asynchrones découverts dans le workspace courant.",
		parameters: Type.Object({}),
		async execute(_toolCallID, _params, _signal, _onUpdate, ctx) {
			await refreshWorkers(ctx, false);
			return textToolResult(JSON.stringify(workers.map(workerSummary), null, 2), { workers: workers.map(workerSummary) });
		},
	});

	pi.registerTool({
		name: "ouvrier_trigger",
		label: "Déclencher un worker Ouvrier",
		description: "Déclenche un worker Ouvrier découvert via son endpoint admin.",
		parameters: Type.Object({
			worker: Type.String({ description: "Identifiant ou nom du worker retourné par ouvrier_workers." }),
			event: Type.Optional(Type.String({ description: "Nom logique de l'événement, par exemple pi.agent_end." })),
			body: Type.Optional(Type.String({ description: "Body JSON optionnel. Le texte brut est encapsulé dans {text}." })),
		}),
		async execute(_toolCallID, params, signal, _onUpdate, ctx) {
			await refreshWorkers(ctx, false);
			const worker = findWorker(params.worker);
			if (!worker) return textToolResult(`Worker introuvable : ${params.worker}`, {}, true);
			const body = parseJSONOrText(params.body ?? "");
			const result = await triggerWorker(worker, params.event || "manual", body, signal);
			return textToolResult(JSON.stringify(result, null, 2), result);
		},
	});

	pi.registerTool({
		name: "ouvrier_inbox",
		label: "Inbox Ouvrier",
		description: "Lit les messages de feedback asynchrones publiés par les workers Ouvrier.",
		parameters: Type.Object({
			unread_only: Type.Optional(Type.Boolean({ description: "Retourner uniquement les messages non lus." })),
		}),
		async execute(_toolCallID, params) {
			const messages = inbox.list().filter((message) => !params.unread_only || !message.read);
			return textToolResult(JSON.stringify(messages, null, 2), { messages });
		},
	});
}

function textToolResult(text: string, details: unknown, isError = false) {
	return { content: [{ type: "text" as const, text }], details, isError };
}
