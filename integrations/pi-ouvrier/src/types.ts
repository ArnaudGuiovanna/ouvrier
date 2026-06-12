export type WorkerManifest = {
	name?: string;
	description?: string;
	events?: string[];
	outcomes?: string[];
	admin_url?: string;
	adminUrl?: string;
	admin_token_env?: string;
	adminTokenEnv?: string;
};

export type DiscoveredWorker = {
	id: string;
	name: string;
	description: string;
	events: string[];
	outcomes: string[];
	adminUrl: string;
	adminTokenEnv?: string;
	manifestPath: string;
	rootDir: string;
	health?: AdminHealthResponse;
	capabilities?: AdminCapabilitiesResponse;
	lastEventID: number;
};

export type AdminHealthResponse = {
	status?: string;
	version?: string;
	[key: string]: unknown;
};

export type AdminCapabilitiesResponse = {
	status?: string;
	capabilities?: AdminPlan[];
	plans?: AdminPlan[];
	[key: string]: unknown;
};

export type AdminPlansResponse = {
	status?: string;
	plans?: AdminPlan[];
	[key: string]: unknown;
};

export type AdminPlan = {
	id?: string;
	trigger?: AdminPlanTrigger;
	steps?: AdminPlanStep[];
	terminal?: AdminPlanTerminal;
	[key: string]: unknown;
};

export type AdminPlanTrigger = {
	kind?: string;
	method?: string;
	path?: string;
	expr?: string;
	value?: string;
	uri?: string;
	[key: string]: unknown;
};

export type AdminPlanStep = {
	index?: number;
	kind?: string;
	goal?: string;
	model?: string;
	tools?: Array<{ name?: string; description?: string; effect?: string; requires_approval?: boolean }>;
	bash?: Array<{ name?: string }>;
	skills?: string[];
	mcp_servers?: string[];
	[key: string]: unknown;
};

export type AdminPlanTerminal = {
	kind?: string;
	async?: boolean;
	sse?: boolean;
	[key: string]: unknown;
};

export type AdminTriggerRequest = {
	trigger?: string;
	method?: string;
	path?: string;
	expr?: string;
	uri?: string;
	id?: string;
	scheduled_at?: string;
	metadata?: Record<string, string>;
	body?: unknown;
};

export type AdminTriggerResponse = {
	status?: string;
	output?: string;
	exec_id?: string;
	trace_id?: string;
	session_id?: string;
	[key: string]: unknown;
};

export type AdminEvent = {
	id: number;
	at?: string;
	kind: string;
	exec_id?: string;
	session_id?: string;
	trace_id?: string;
	payload?: Record<string, unknown>;
};

export type AdminTraceResponse = {
	status?: string;
	execution?: Record<string, unknown>;
	events?: AdminEvent[];
	sessions?: number;
	last_event_id?: number;
	[key: string]: unknown;
};

export type InboxSeverity = "info" | "low" | "medium" | "high";

export type InboxAction = "fix_with_pi" | "open_trace" | "dismiss" | "mark_read";

export type InboxMessage = {
	id: string;
	workerID: string;
	worker: string;
	severity: InboxSeverity;
	title: string;
	body: string;
	at: string;
	read: boolean;
	execID?: string;
	traceID?: string;
	sessionID?: string;
	eventID?: number;
	eventKind?: string;
	actions: InboxAction[];
};

export type PiBridgeEventName = "pi.agent_end" | "pi.turn_end" | "pi.tool_execution_end";

export type InboxPanelAction =
	| { kind: "close" }
	| { kind: "dismiss"; messageID: string }
	| { kind: "fix"; messageID: string }
	| { kind: "trace"; messageID: string }
	| { kind: "read"; messageID: string }
	| { kind: "detail"; messageID: string };
