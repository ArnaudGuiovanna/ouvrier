import type { Theme } from "@earendil-works/pi-coding-agent";
import { matchesKey, visibleWidth } from "@earendil-works/pi-tui";
import type { AdminTraceResponse, InboxMessage, InboxPanelAction } from "./types";

export class InboxPanel {
	private selected = 0;
	private readonly messages: InboxMessage[];
	private readonly theme: Theme;
	private readonly done: (action: InboxPanelAction) => void;

	constructor(messages: InboxMessage[], theme: Theme, done: (action: InboxPanelAction) => void) {
		this.messages = messages;
		this.theme = theme;
		this.done = done;
	}

	handleInput(data: string): void {
		if (matchesKey(data, "escape") || data === "q") {
			this.done({ kind: "close" });
			return;
		}
		if (this.messages.length === 0) return;
		const current = this.messages[this.selected]!;
		if (matchesKey(data, "up") || data === "k") {
			this.selected = Math.max(0, this.selected - 1);
		} else if (matchesKey(data, "down") || data === "j") {
			this.selected = Math.min(this.messages.length - 1, this.selected + 1);
		} else if (matchesKey(data, "return")) {
			this.done({ kind: "detail", messageID: current.id });
		} else if (data === "d" || matchesKey(data, "backspace")) {
			this.done({ kind: "dismiss", messageID: current.id });
		} else if (data === "r") {
			this.done({ kind: "read", messageID: current.id });
		} else if (data === "f") {
			this.done({ kind: "fix", messageID: current.id });
		} else if (data === "t") {
			this.done({ kind: "trace", messageID: current.id });
		}
	}

	render(width: number): string[] {
		const w = Math.max(44, Math.min(78, width));
		const innerW = w - 2;
		const th = this.theme;
		const unread = this.messages.filter((message) => !message.read).length;
		const lines: string[] = [];
		const row = (content = "", tone: "text" | "accent" | "dim" | "error" | "warning" | "success" = "text") =>
			borderRow(th, innerW, content, tone);

		lines.push(th.fg("border", `╭${"─".repeat(innerW)}╮`));
		lines.push(row(`Inbox Ouvrier  ${unread} non lus`, "accent"));
		lines.push(row("Les workers publient ici leurs retours asynchrones", "dim"));
		lines.push(row("↑↓/jk naviguer · Entrée détail · f corriger · t trace · d ignorer · q fermer", "dim"));
		lines.push(th.fg("border", `├${"─".repeat(innerW)}┤`));

		if (this.messages.length === 0) {
			lines.push(row("Aucun message de worker pour l'instant.", "dim"));
			lines.push(row("Démarre un worker Ouvrier ou lance /ovr trigger.", "dim"));
		} else {
			for (let i = 0; i < Math.min(this.messages.length, 12); i++) {
				const message = this.messages[i]!;
				const selected = i === this.selected;
				const unreadMark = message.read ? " " : "●";
				const severity = message.severity.toUpperCase().padEnd(6, " ");
				const title = compact(`${unreadMark} ${message.worker} · ${severity} · ${message.title}`);
				lines.push(row(`${selected ? "▶" : " "} ${title}`, selected ? severityTone(message.severity) : "text"));
				lines.push(row(`  ${snippet(message.body, innerW - 4)}`, selected ? "text" : "dim"));
			}
			if (this.messages.length > 12) lines.push(row(`… ${this.messages.length - 12} de plus`, "dim"));
		}

		lines.push(th.fg("border", `╰${"─".repeat(innerW)}╯`));
		return lines;
	}

	invalidate(): void {}
	dispose(): void {}
}

export class MessageDetailPanel {
	private readonly message: InboxMessage;
	private readonly theme: Theme;
	private readonly done: (action: InboxPanelAction) => void;
	private scroll = 0;

	constructor(message: InboxMessage, theme: Theme, done: (action: InboxPanelAction) => void) {
		this.message = message;
		this.theme = theme;
		this.done = done;
	}

	handleInput(data: string): void {
		if (matchesKey(data, "escape") || data === "q") this.done({ kind: "close" });
		else if (matchesKey(data, "up") || data === "k") this.scroll = Math.max(0, this.scroll - 1);
		else if (matchesKey(data, "down") || data === "j") this.scroll++;
		else if (data === "f") this.done({ kind: "fix", messageID: this.message.id });
		else if (data === "t") this.done({ kind: "trace", messageID: this.message.id });
		else if (data === "d") this.done({ kind: "dismiss", messageID: this.message.id });
	}

	render(width: number): string[] {
		const w = Math.max(54, Math.min(88, width));
		const innerW = w - 2;
		const th = this.theme;
		const row = (content = "", tone: "text" | "accent" | "dim" | "error" | "warning" | "success" = "text") =>
			borderRow(th, innerW, content, tone);
		const bodyLines = wrapPlain(this.message.body, innerW - 2).slice(this.scroll, this.scroll + 16);
		const lines = [
			th.fg("border", `╭${"─".repeat(innerW)}╮`),
			row(`${this.message.worker} · ${this.message.severity.toUpperCase()}`, severityTone(this.message.severity)),
			row(this.message.title, "accent"),
			row(this.message.at, "dim"),
			th.fg("border", `├${"─".repeat(innerW)}┤`),
		];
		for (const line of bodyLines) lines.push(row(` ${line}`));
		if (bodyLines.length === 0) lines.push(row(" <empty>", "dim"));
		lines.push(th.fg("border", `├${"─".repeat(innerW)}┤`));
		lines.push(row("f Corriger avec Pi · t Ouvrir la trace · d Ignorer · q Retour", "dim"));
		lines.push(th.fg("border", `╰${"─".repeat(innerW)}╯`));
		return lines;
	}

	invalidate(): void {}
	dispose(): void {}
}

export class TracePanel {
	private readonly trace: AdminTraceResponse;
	private readonly theme: Theme;
	private readonly done: () => void;
	private scroll = 0;

	constructor(trace: AdminTraceResponse, theme: Theme, done: () => void) {
		this.trace = trace;
		this.theme = theme;
		this.done = done;
	}

	handleInput(data: string): void {
		if (matchesKey(data, "escape") || data === "q") this.done();
		else if (matchesKey(data, "up") || data === "k") this.scroll = Math.max(0, this.scroll - 1);
		else if (matchesKey(data, "down") || data === "j") this.scroll++;
	}

	render(width: number): string[] {
		const w = Math.max(58, Math.min(100, width));
		const innerW = w - 2;
		const th = this.theme;
		const row = (content = "", tone: "text" | "accent" | "dim" | "error" | "warning" | "success" = "text") =>
			borderRow(th, innerW, content, tone);
		const events = this.trace.events ?? [];
		const eventLines = events.map((event) => `${event.id} ${event.kind} ${event.exec_id ?? ""} ${payloadSummary(event.payload)}`);
		const lines = [
			th.fg("border", `╭${"─".repeat(innerW)}╮`),
			row(`Trace Ouvrier · ${events.length} événements`, "accent"),
			row("↑↓/jk défiler · q fermer", "dim"),
			th.fg("border", `├${"─".repeat(innerW)}┤`),
		];
		for (const line of eventLines.slice(this.scroll, this.scroll + 20)) lines.push(row(line));
		if (eventLines.length === 0) lines.push(row("Aucun événement de trace retourné.", "dim"));
		lines.push(th.fg("border", `╰${"─".repeat(innerW)}╯`));
		return lines;
	}

	invalidate(): void {}
	dispose(): void {}
}

function borderRow(
	theme: Theme,
	innerW: number,
	content: string,
	tone: "text" | "accent" | "dim" | "error" | "warning" | "success",
): string {
	const fitted = pad(fitPlain(content, innerW), innerW);
	return theme.fg("border", "│") + theme.fg(tone, fitted) + theme.fg("border", "│");
}

function severityTone(severity: string): "text" | "accent" | "dim" | "error" | "warning" | "success" {
	if (severity === "high") return "error";
	if (severity === "medium") return "warning";
	if (severity === "low") return "success";
	return "accent";
}

function compact(value: string): string {
	return value.replace(/\s+/g, " ").trim();
}

function snippet(value: string, width: number): string {
	return fitPlain(compact(value), width);
}

function payloadSummary(payload: Record<string, unknown> | undefined): string {
	if (!payload) return "";
	for (const key of ["status", "error", "reason", "output", "input"]) {
		const value = payload[key];
		if (typeof value === "string" && value.trim()) return compact(value);
	}
	return compact(JSON.stringify(payload));
}

function wrapPlain(text: string, width: number): string[] {
	const words = text.replace(/\r/g, "").split(/\s+/);
	const lines: string[] = [];
	let current = "";
	for (const word of words) {
		if (!word) continue;
		const candidate = current ? `${current} ${word}` : word;
		if (visibleWidth(candidate) <= width) current = candidate;
		else {
			if (current) lines.push(current);
			current = fitPlain(word, width);
		}
	}
	if (current) lines.push(current);
	return lines;
}

function fitPlain(value: string, width: number): string {
	if (visibleWidth(value) <= width) return value;
	let out = "";
	for (const char of Array.from(value)) {
		if (visibleWidth(`${out}${char}…`) > width) break;
		out += char;
	}
	return `${out}…`;
}

function pad(value: string, width: number): string {
	return value + " ".repeat(Math.max(0, width - visibleWidth(value)));
}
