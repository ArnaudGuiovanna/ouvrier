import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { registerBridgeEvents } from "./bridge";
import { handleOvrCommand, openInbox } from "./commands";
import {
	bindInboxStatus,
	hydrateInbox,
	refreshWorkers,
	setupOuvrierState,
	shutdownOuvrierState,
} from "./state";
import { registerOuvrierTools } from "./tools";
import { ovrCompletions } from "./trigger";

export default function ouvriPiExtension(pi: ExtensionAPI) {
	setupOuvrierState(pi);

	pi.on("session_start", async (_event, ctx) => {
		hydrateInbox(ctx);
		bindInboxStatus(ctx);
		await refreshWorkers(ctx, false);
	});

	pi.on("session_shutdown", async () => {
		shutdownOuvrierState();
	});

	registerBridgeEvents(pi);

	pi.registerCommand("ovr", {
		description: "Flotte asynchrone Ouvrier : workers, inbox, trigger, trace, santé, composition",
		getArgumentCompletions: (prefix) => ovrCompletions(prefix),
		handler: async (args, ctx) => handleOvrCommand(pi, args, ctx),
	});

	pi.registerShortcut("ctrl+shift+o", {
		description: "Ouvrir l'Inbox Ouvrier",
		handler: async (ctx) => {
			await openInbox(pi, ctx);
		},
	});

	registerOuvrierTools(pi);
}
