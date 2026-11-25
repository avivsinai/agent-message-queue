import assert from "node:assert/strict";
import * as fs from "node:fs";
import nodeFs from "node:fs";
import { registerHooks, syncBuiltinESMExports } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import amqPiBridge, { type AmqBridgeOptions, type AmqOutcome, type AmqRequest, DOORBELL_LINE, registerAmqBridge } from "./amq-bridge.ts";

type Handler = (event: unknown, ctx: unknown) => unknown;

// The goldens under testdata/pi-bridge are the cross-language vector: the Go
// adapter test reads the same bytes with its own readers.
const GOLDEN = path.join(import.meta.dirname, "testdata", "pi-bridge");
const REF = "amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgayq";
const ORPHAN = "amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgaza";
const OTHER = "amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgazq";

// normalize pins the per-run values (pid, clock, generation) so a produced
// record compares byte for byte with its golden.
function normalize(text: string): string {
	return text
		.trim()
		.split("\n")
		.map((line) => {
			const rec = JSON.parse(line);
			if ("pid" in rec) rec.pid = 4242;
			if ("at" in rec) rec.at = "2000-01-01T00:00:00.000Z";
			if ("delivered_at" in rec) rec.delivered_at = "2000-01-01T00:00:00.000Z";
			if ("session_generation" in rec) rec.session_generation = "golden-generation";
			return `${JSON.stringify(rec)}\n`;
		})
		.join("");
}

// aged makes ref's receipt and events older than the bridge's owner
// staleness bound, as an earlier generation's are.
function aged(dir: string, ref: string): void {
	const past = new Date(Date.now() - 120_000);
	for (const rel of [`receipts/${ref}.json`, `events/${ref}.jsonl`]) {
		try {
			fs.utimesSync(path.join(dir, rel), past, past);
		} catch {
			// No such file yet.
		}
	}
}

type BridgeModule = { default: (pi: never) => void; registerAmqBridge: typeof registerAmqBridge };

// freshBridge imports a new instance of the extension module, as pi does on
// every load and reload, so its guard state starts empty. pid, when given,
// is the process id that instance sees at load.
let instances = 0;
async function freshBridge(pid?: number): Promise<BridgeModule> {
	const real = Object.getOwnPropertyDescriptor(process, "pid") as PropertyDescriptor;
	if (pid !== undefined) Object.defineProperty(process, "pid", { ...real, value: pid });
	try {
		return (await import(`./amq-bridge.ts?instance=${++instances}`)) as BridgeModule;
	} finally {
		Object.defineProperty(process, "pid", real);
	}
}

type Tool = { name: string; sourceInfo?: { source: string; path: string } };
const builtinTool = (name: string): Tool => ({ name, sourceInfo: { source: "builtin", path: `builtin:${name}` } });

// harness runs the extension against a fake ExtensionAPI and a real bridge
// directory under a temporary AM_ROOT. host, when given, embeds the bridge
// as a host application does, with no AM_ROOT or AM_ME set. shared, when
// given, is another harness's root, which that harness removes. mod, when
// given, is the module instance to load instead of the shared one.
function harness(host?: (root: string) => AmqBridgeOptions, shared?: string, mod?: BridgeModule) {
	const root = shared ?? fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-bridge-"));
	process.env.AM_ROOT = root;
	process.env.AM_ME = "pi-seat";
	if (host) {
		delete process.env.AM_ROOT;
		delete process.env.AM_ME;
	}
	const handlers = new Map<string, Handler[]>();
	const sent: { content: unknown; options: unknown }[] = [];
	// customSent records pi.sendMessage calls: the wake doorbell's visible
	// custom messages, which never touch sendUserMessage.
	const customSent: { content: unknown; options: unknown }[] = [];
	const state = { idle: true, pending: false, idleChecks: 0, aborts: 0, lookups: 0, throwSend: false };
	const notices: string[] = [];
	// armedAtSend records, for each sendUserMessage, whether a guard file of
	// this process existed at that moment.
	const armedAtSend: boolean[] = [];
	const tools: Tool[] = ["read", "bash", "edit", "write", "grep", "find", "ls"].map(builtinTool);
	const dialogs: { title: string; placeholder: string | undefined }[] = [];
	const guards = () => {
		try {
			return fs.readdirSync(dir).filter((n) => n.startsWith("granted-guard."));
		} catch {
			return [];
		}
	};
	const pi = {
		on(name: string, h: Handler) {
			handlers.set(name, [...(handlers.get(name) ?? []), h]);
			return () => {};
		},
		sendUserMessage(content: unknown, options: unknown) {
			armedAtSend.push(guards().length > 0);
			if (state.throwSend) throw new Error("not delivered");
			sent.push({ content, options });
		},
		sendMessage(content: unknown, options: unknown) {
			if (state.throwSend) throw new Error("not delivered");
			customSent.push({ content, options });
		},
		getAllTools() {
			state.lookups++;
			return tools;
		},
	};
	// answer is what the person types in a dialog, undefined for Escape;
	// input resolves undefined for an aborted signal, as pi's TUI does. The
	// fake has no select, so a guard that opens one fails.
	let answer: (title: string) => string | undefined | Promise<string | undefined> = () => undefined;
	const ctx = {
		mode: "tui",
		hasUI: true,
		signal: undefined as AbortSignal | undefined,
		ui: {
			notify(message: string) {
				notices.push(message);
			},
			async input(title: string, placeholder?: string, opts?: { signal?: AbortSignal }) {
				dialogs.push({ title, placeholder });
				if (opts?.signal?.aborted) return undefined;
				return answer(title);
			},
		},
		isIdle: () => {
			state.idleChecks++;
			return state.idle;
		},
		hasPendingMessages: () => state.pending,
		abort: () => {
			state.aborts++;
		},
		sessionManager: { getSessionId: () => "pi-session-1" },
	};
	const dir = path.join(root, "agents", "pi-seat", "extensions", "pi-bridge");
	const register = mod?.registerAmqBridge ?? registerAmqBridge;
	const bridge = host ? register(pi as never, host(root)) : undefined;
	if (!host) (mod?.default ?? amqPiBridge)(pi as never);
	const emit = async (name: string, event: object = {}) => {
		for (const h of handlers.get(name) ?? []) await h({ type: name, ...event }, ctx);
	};
	return {
		root,
		dir,
		bridge,
		sent,
		customSent,
		state,
		notices,
		emit,
		ctx,
		tools,
		dialogs,
		armedAtSend,
		guards,
		set answer(fn: (title: string) => string | undefined | Promise<string | undefined>) {
			answer = fn;
		},
		// tool raises pi's tool_call for one call and returns the first
		// block, as pi does, or undefined when every handler let it run.
		async tool(toolName: string, input: unknown, extra: object = {}) {
			for (const h of handlers.get("tool_call") ?? []) {
				const r = (await h({ type: "tool_call", toolCallId: "call-1", toolName, input, ...extra }, ctx)) as { block?: boolean } | undefined;
				if (r?.block) return r;
			}
			return undefined;
		},
		start: () => emit("session_start", { reason: "startup" }),
		generation: () => JSON.parse(fs.readFileSync(path.join(dir, "bridge.liveness"), "utf8")).session_generation as string,
		publish(ref: string, text: string, epochHint: string, fields: Record<string, unknown> = { bridge_revision: 3 }) {
			const request = {
				ref,
				text,
				deliver_as: "followUp",
				not_after: new Date(Date.now() + 60_000).toISOString(),
				epoch_hint: epochHint,
				created_at: new Date().toISOString(),
				...fields,
			};
			this.write(ref, request);
		},
		write(ref: string, request: object) {
			const tmp = path.join(dir, "requests", ".publish-test");
			fs.writeFileSync(tmp, JSON.stringify(request));
			fs.renameSync(tmp, path.join(dir, "requests", `${ref}.json`));
		},
		cancel(ref: string, fields: Record<string, unknown> = {}) {
			const cancels = path.join(dir, "cancels");
			fs.mkdirSync(cancels, { recursive: true });
			const tmp = path.join(cancels, ".publish-test");
			fs.writeFileSync(tmp, JSON.stringify({ protocol: "amq:pi-bridge:v1", ref, at: new Date().toISOString(), ...fields }));
			fs.renameSync(tmp, path.join(cancels, `${ref}.json`));
		},
		events(ref: string): Record<string, string>[] {
			let data: string;
			try {
				data = fs.readFileSync(path.join(dir, "events", `${ref}.jsonl`), "utf8");
			} catch {
				return [];
			}
			return data
				.split("\n")
				.filter((l) => l)
				.map((l) => JSON.parse(l));
		},
		hasReceipt: (ref: string) => fs.existsSync(path.join(dir, "receipts", `${ref}.json`)),
		async waitFor(cond: () => boolean) {
			for (let i = 0; i < 100 && !cond(); i++) await new Promise((r) => setTimeout(r, 20));
		},
		async done() {
			await emit("session_shutdown", { reason: "quit" });
			if (!shared) fs.rmSync(root, { recursive: true, force: true });
		},
	};
}

const userMessage = (text: string) => ({ message: { role: "user", content: [{ type: "text", text }] } });
const assistantMessage = (text: string) => ({ message: { role: "assistant", content: [{ type: "text", text }], stopReason: "stop" } });
const toolUseMessage = (text: string) => ({
	message: {
		role: "assistant",
		content: [
			{ type: "text", text },
			{ type: "toolCall", id: "call-1", name: "read", arguments: { path: "." } },
		],
		stopReason: "toolUse",
	},
});

test("a cancel for a request that has not run refuses it cancelled and it never runs", async () => {
	const h = harness();
	fs.mkdirSync(path.join(h.dir, "requests"), { recursive: true });
	h.write(REF, { ref: REF, text: "remote task", deliver_as: "followUp", not_after: new Date(Date.now() + 60_000).toISOString(), epoch_hint: "x", bridge_revision: 5 });
	h.cancel(REF);
	await h.start();
	try {
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(h.sent, []);
		assert.equal(h.hasReceipt(REF), false);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.reason]),
			[["refused", "cancelled"]],
		);
	} finally {
		await h.done();
	}
});

test("a cancel for the request in flight aborts it and its terminal event is cancelled", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);
		await h.emit("message_start", userMessage("remote task"));
		h.cancel(REF);
		await h.waitFor(() => h.state.aborts > 0);
		assert.equal(h.state.aborts, 1);
		await h.emit("agent_before_settle", { outcome: "aborted" });
		await h.emit("agent_settled", { aborted: true });
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["started", "cancelled"],
		);
	} finally {
		await h.done();
	}
});

// agent-message-queue-kj9z: Pi 1.5.2 skipped agent_before_settle on Escape.
test("a remote request interrupted at the terminal ends cancelled", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);
		await h.emit("message_start", userMessage("remote task"));
		await h.emit("agent_end", { messages: [] });
		await h.emit("agent_settled", { aborted: true });
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["started", "cancelled"],
		);
	} finally {
		await h.done();
	}
});

test("a cancel for a request that already ended does nothing", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);
		await h.emit("message_start", userMessage("remote task"));
		await h.emit("message_end", assistantMessage("done"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled");
		h.cancel(REF);
		await new Promise((r) => setTimeout(r, 600));
		assert.equal(h.state.aborts, 0);
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["started", "completed"],
		);
	} finally {
		await h.done();
	}
});

// Bead agent-message-queue-vd4.6: the wake holds its doorbell while a pi
// extension prompt is open, so the marker exists exactly while pi waits.
test("an open extension prompt holds the wake doorbell until it ends", async () => {
	const h = harness();
	await h.start();
	const marker = path.join(h.dir, "..", "..", ".wake.dialog");
	await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
	const opened = JSON.parse(fs.readFileSync(marker, "utf8"));
	assert.equal(opened.event, "ui_prompt");
	assert.equal(opened.harness, "pi");
	assert.equal(opened.kind, "confirm");
	assert.equal(fs.statSync(marker).mode & 0o777, 0o600);
	await h.emit("ui_prompt_end", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
	assert.equal(fs.existsSync(marker), false);
	await h.done();
});

test("an open extension prompt is shown in Buzz as an option-less dialog until it ends", async () => {
	const h = harness();
	const store = fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-remote-"));
	process.env.AMQ_REMOTE_BINDING = path.join(store, "binding.json");
	fs.mkdirSync(path.join(store, "bindings"), { recursive: true });
	fs.writeFileSync(
		path.join(store, "bindings", "pi-seat.json"),
		JSON.stringify({ carrier: "mailbox", root: process.env.AM_ROOT, handle: "pi-seat", bound_at: "2026-11-02T00:00:00Z" }),
	);
	try {
		await h.start();
		await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
		const dir = path.join(store, "dialogs", "pi-seat");
		const [file] = fs.readdirSync(path.join(dir, "requests")).filter((f) => f.endsWith(".json"));
		const request = JSON.parse(fs.readFileSync(path.join(dir, "requests", file), "utf8"));
		assert.equal(request.harness, "pi");
		assert.deepEqual(request.options, []);
		assert.equal(request.text, "Run it? (confirm)");
		assert.equal(fs.existsSync(path.join(dir, "resolved", file)), false);
		await h.emit("ui_prompt_end", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
		const resolved = JSON.parse(fs.readFileSync(path.join(dir, "resolved", file), "utf8"));
		assert.equal(resolved.id, request.id);
		assert.equal(resolved.by, "terminal");
	} finally {
		delete process.env.AMQ_REMOTE_BINDING;
	}
	await h.done();
});

// remoteStore points the dialog store at a temporary directory with a
// mailbox binding for root's pi-seat; done removes it.
function remoteStore(root: string): { dialogs: string; done: () => void } {
	const store = fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-remote-"));
	process.env.AMQ_REMOTE_BINDING = path.join(store, "binding.json");
	fs.mkdirSync(path.join(store, "bindings"), { recursive: true });
	fs.writeFileSync(path.join(store, "bindings", "pi-seat.json"), JSON.stringify({ carrier: "mailbox", root, handle: "pi-seat", bound_at: "2026-11-02T00:00:00Z" }));
	return {
		dialogs: path.join(store, "dialogs", "pi-seat"),
		done: () => {
			delete process.env.AMQ_REMOTE_BINDING;
			fs.rmSync(store, { recursive: true, force: true });
		},
	};
}

const hostIdentity = (root: string): AmqBridgeOptions => ({ identity: () => ({ root, handle: "pi-seat" }) });

test("an extension's own dialog takes the owner's Buzz answer", async () => {
	const h = harness(hostIdentity);
	const store = remoteStore(h.root);
	try {
		await h.start();
		const answer = h.bridge!.openDialog({ title: "Deploy?", body: "to prod", options: [{ label: "Yes" }, { label: "No", deny: true }] });
		const [file] = fs.readdirSync(path.join(store.dialogs, "requests")).filter((f) => f.endsWith(".json"));
		const request = JSON.parse(fs.readFileSync(path.join(store.dialogs, "requests", file), "utf8"));
		assert.equal(request.harness, "pi");
		assert.equal(request.text, "Deploy?\nto prod");
		assert.deepEqual(request.options, [
			{ n: 1, label: "Yes", value: "1" },
			{ n: 2, label: "No", value: "2", deny: true },
		]);
		// The extension's terminal prompt posts no option-less copy beside it.
		await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "select", title: "Deploy?" });
		assert.deepEqual(fs.readdirSync(path.join(store.dialogs, "requests")).filter((f) => f.endsWith(".json")), [file]);
		fs.writeFileSync(path.join(store.dialogs, "answers", file), JSON.stringify({ id: request.id, option: 2, by: "buzz", at: new Date().toISOString() }), { flag: "wx" });
		assert.deepEqual(await answer, { by: "buzz", index: 1 });
		const resolved = JSON.parse(fs.readFileSync(path.join(store.dialogs, "resolved", file), "utf8"));
		assert.equal(resolved.by, "buzz");
		assert.equal(resolved.outcome, "denied");
		assert.equal(resolved.label, "No");
	} finally {
		store.done();
	}
	await h.done();
});

test("an extension's own dialog answered in the terminal closes as terminal", async () => {
	const h = harness(hostIdentity);
	const store = remoteStore(h.root);
	try {
		await h.start();
		const terminal = new AbortController();
		const answer = h.bridge!.openDialog({ title: "Deploy?", options: [{ label: "Yes" }], signal: terminal.signal });
		const [file] = fs.readdirSync(path.join(store.dialogs, "requests")).filter((f) => f.endsWith(".json"));
		terminal.abort();
		assert.equal(await answer, undefined);
		const resolved = JSON.parse(fs.readFileSync(path.join(store.dialogs, "resolved", file), "utf8"));
		assert.equal(resolved.by, "terminal");
		assert.equal(resolved.outcome, "terminal");
	} finally {
		store.done();
	}
	await h.done();
});

// #1199 review: pi awaits each extension's handler separately, so the end
// of prompt A can reach this extension after prompt B started; it must not
// remove B's marker.
test("a late end of one prompt keeps the next prompt's hold", async () => {
	const h = harness();
	await h.start();
	const marker = path.join(h.dir, "..", "..", ".wake.dialog");
	await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm" });
	await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "select" });
	await h.emit("ui_prompt_end", { reason: "ui_prompt", kind: "confirm" });
	assert.equal(fs.existsSync(marker), true);
	await h.emit("ui_prompt_end", { reason: "ui_prompt", kind: "select" });
	assert.equal(fs.existsSync(marker), false);
	await h.done();
});

// #1199 review: pi 1.3.0 can deliver a prompt's end to this extension
// before its start; the late start must not leave a hold nothing clears.
test("an end delivered before its start leaves no hold", async () => {
	const h = harness();
	await h.start();
	const marker = path.join(h.dir, "..", "..", ".wake.dialog");
	await h.emit("ui_prompt_end", { reason: "ui_prompt", kind: "confirm" });
	await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm" });
	assert.equal(fs.existsSync(marker), false);
	await h.done();
});

// Bead agent-message-queue-vd4.6: pi's own "Trust project folder?" prompt
// opens before the session starts; the marker holds the doorbell until the
// answer lets the session start.
test("the startup trust prompt holds the wake doorbell until the session starts", async () => {
	const h = harness();
	const agent = path.join(h.dir, "..", "..");
	fs.mkdirSync(agent, { recursive: true, mode: 0o700 });
	const marker = path.join(agent, ".wake.dialog");
	await h.emit("project_trust", { cwd: "/work" });
	assert.equal(JSON.parse(fs.readFileSync(marker, "utf8")).event, "project_trust");
	await h.start();
	assert.equal(fs.existsSync(marker), false);
	await h.done();
});

// Bead agent-message-queue-611.60: a relay share pins pi's session id, so the
// reference bridge publishes it in liveness.
test("the liveness record carries pi's session id", async () => {
	const h = harness();
	await h.start();
	const live = JSON.parse(fs.readFileSync(path.join(h.dir, "bridge.liveness"), "utf8"));
	assert.equal(live.session_id, "pi-session-1");
	await h.emit("session_shutdown", { reason: "quit" });
});

test("a request file becomes a pi follow-up with a receipt and a terminal event", async () => {
	const h = harness();
	const produced = (rel: string) => normalize(fs.readFileSync(path.join(h.dir, rel), "utf8"));
	const golden = (rel: string) => fs.readFileSync(path.join(GOLDEN, rel), "utf8");

	// A receipt an earlier generation left without a terminal event.
	fs.mkdirSync(path.join(h.dir, "receipts"), { recursive: true });
	fs.copyFileSync(path.join(GOLDEN, "receipts", `${ORPHAN}.json`), path.join(h.dir, "receipts", `${ORPHAN}.json`));
	aged(h.dir, ORPHAN);

	await h.start();
	try {
		// The adapter's request bytes (asserted by the Go golden test),
		// addressed to this run's generation.
		const request = JSON.parse(golden(`requests/${REF}.json`));
		request.epoch_hint = h.generation();
		h.write(REF, request);
		await h.waitFor(() => h.hasReceipt(REF));
		assert.deepEqual(h.sent, [{ content: "summarize the diff", options: { deliverAs: "followUp" } }]);

		const receipt = JSON.parse(fs.readFileSync(path.join(h.dir, "receipts", `${REF}.json`), "utf8"));
		assert.equal(receipt.pid, process.pid);
		assert.ok(receipt.session_generation);
		assert.equal(receipt.session_generation, h.generation());

		await h.emit("message_start", userMessage("summarize the diff"));
		await h.emit("message_end", assistantMessage("two files changed"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled");

		for (const rel of [`receipts/${REF}.json`, "bridge.liveness", `events/${REF}.jsonl`, `events/${ORPHAN}.jsonl`]) {
			assert.equal(produced(rel), golden(rel), rel);
		}
	} finally {
		await h.done();
	}
});

// Review of pi bridge ownership: a second pi on one mailbox masked the first request's real result.
test("a second bridge on a live, even stalled, owner's mailbox stays inactive and the request keeps its real outcome", async () => {
	const h1 = harness();
	await h1.start();
	const h2 = harness(undefined, h1.root);
	try {
		const gen = h1.generation();
		h1.publish(REF, "remote task", gen);
		await h1.waitFor(() => h1.hasReceipt(REF));
		await h1.emit("message_start", userMessage("remote task"));
		// A stalled owner (no heartbeat for 10 s) is still the owner.
		const past = new Date(Date.now() - 10_000);
		fs.utimesSync(path.join(h1.dir, "bridge.liveness"), past, past);
		await h2.start();
		assert.equal(h1.generation(), gen);
		assert.equal(h2.notices.length, 1);
		await h1.emit("message_end", assistantMessage("done"));
		await h1.emit("agent_before_settle", { outcome: "completed" });
		await h1.emit("agent_settled");
		assert.deepEqual(h1.events(REF).map((e) => e.event), ["started", "completed"]);
		await h2.done();
		const live = JSON.parse(fs.readFileSync(path.join(h1.dir, "bridge.liveness"), "utf8"));
		assert.deepEqual([live.live, live.session_generation], [true, gen]);
	} finally {
		await h1.done();
	}
});

// Review of pi bridge ownership: a bridge started over another generation's live liveness.
test("a bridge stays inactive while another generation's liveness is live, and takes over a beat after it sees it aged", async () => {
	const h = harness();
	const foreign = JSON.stringify({ protocol: "amq:pi-bridge:v1", live: true, at: new Date().toISOString(), pid: 4242, surface: "tui", session_generation: "other-generation", bridge_revision: 4 });
	fs.mkdirSync(h.dir, { recursive: true });
	fs.writeFileSync(path.join(h.dir, "bridge.liveness"), foreign);
	await h.start();
	try {
		assert.equal(fs.readFileSync(path.join(h.dir, "bridge.liveness"), "utf8"), foreign);
		assert.equal(h.notices.length, 1);
		assert.match(h.notices[0], /pid 4242/);
		// Review of the liveness ruling: a pi restarted within 60 s of a crash stayed inactive all session.
		const past = new Date(Date.now() - 120_000);
		fs.utimesSync(path.join(h.dir, "bridge.liveness"), past, past);
		// Review of sleep safety: a stale live record is not taken on the first beat that sees it.
		await new Promise((r) => setTimeout(r, 2300));
		assert.equal(fs.readFileSync(path.join(h.dir, "bridge.liveness"), "utf8"), foreign);
		for (let i = 0; i < 125 && h.notices.length < 2; i++) await new Promise((r) => setTimeout(r, 20));
		assert.equal(h.notices[1], "amq-bridge now serves Buzz requests.");
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.hasReceipt(REF));
		assert.equal(h.sent.length, 1);
	} finally {
		await h.done();
	}
});

// Review of the liveness ownership: a displaced bridge stayed inactive after the bridge that displaced it left.
test("a displaced bridge waits and serves again once the other bridge leaves", async () => {
	const h = harness();
	await h.start();
	try {
		const live = path.join(h.dir, "bridge.liveness");
		const other = { protocol: "amq:pi-bridge:v1", live: true, at: new Date().toISOString(), pid: 4242, surface: "tui", session_generation: "other-generation", bridge_revision: 4 };
		fs.writeFileSync(live, JSON.stringify(other)); // a bridge that started at the same moment wrote last
		await h.waitFor(() => h.notices.length === 1);
		assert.match(h.notices[0], /no longer serves Buzz requests/);
		fs.writeFileSync(live, JSON.stringify({ ...other, live: false })); // it exits
		for (let i = 0; i < 150 && h.notices.length < 2; i++) await new Promise((r) => setTimeout(r, 20));
		assert.equal(h.notices[1], "amq-bridge now serves Buzz requests.");
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.hasReceipt(REF));
		assert.equal(h.sent.length, 1);
	} finally {
		await h.done();
	}
});

// Review of dialog ownership: a pi displaced while its prompt was open left its marker, holding the serving pi's doorbell.
test("a prompt opened while serving closes its marker after the pi is displaced", async () => {
	const h = harness();
	await h.start();
	try {
		const marker = path.join(h.dir, "..", "..", ".wake.dialog");
		await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
		assert.ok(fs.existsSync(marker));
		const other = { protocol: "amq:pi-bridge:v1", live: true, at: new Date().toISOString(), pid: 4242, surface: "tui", session_generation: "other-generation", bridge_revision: 4 };
		fs.writeFileSync(path.join(h.dir, "bridge.liveness"), JSON.stringify(other));
		await h.waitFor(() => h.notices.length === 1);
		assert.match(h.notices[0], /no longer serves Buzz requests/);
		await h.emit("ui_prompt_end", { reason: "ui_prompt" });
		assert.equal(fs.existsSync(marker), false);
	} finally {
		await h.done();
	}
});

// Review of dialog ownership: a displaced pi's prompt end removed the serving pi's marker that had replaced its own.
test("a displaced pi's prompt end leaves the serving pi's marker that replaced its own", async () => {
	const h = harness();
	await h.start();
	try {
		const marker = path.join(h.dir, "..", "..", ".wake.dialog");
		await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
		const other = { protocol: "amq:pi-bridge:v1", live: true, at: new Date().toISOString(), pid: 4242, surface: "tui", session_generation: "other-generation", bridge_revision: 4 };
		fs.writeFileSync(path.join(h.dir, "bridge.liveness"), JSON.stringify(other));
		await h.waitFor(() => h.notices.length === 1);
		const owners = `${JSON.stringify({ schema: 1, event: "ui_prompt", harness: "pi", kind: "select", opened_at: "2000-01-01T00:00:00Z" })}\n`;
		fs.writeFileSync(marker, owners);
		await h.emit("ui_prompt_end", { reason: "ui_prompt" });
		assert.equal(fs.readFileSync(marker, "utf8"), owners);
	} finally {
		await h.done();
	}
});

// Review of dialog ownership: a pi that took over during its own open prompt served with no marker, so the doorbell could type into the prompt.
test("a pi that takes over during its own open prompt holds the doorbell for it", async () => {
	const h = harness();
	const live = path.join(h.dir, "bridge.liveness");
	const other = { protocol: "amq:pi-bridge:v1", live: true, at: new Date().toISOString(), pid: 4242, surface: "tui", session_generation: "other-generation", bridge_revision: 4 };
	fs.mkdirSync(h.dir, { recursive: true });
	fs.writeFileSync(live, JSON.stringify(other));
	await h.start();
	try {
		const marker = path.join(h.dir, "..", "..", ".wake.dialog");
		await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
		assert.equal(fs.existsSync(marker), false); // held off: not this pi's doorbell yet
		fs.writeFileSync(live, JSON.stringify({ ...other, live: false })); // the holder exits
		for (let i = 0; i < 150 && h.notices.length < 2; i++) await new Promise((r) => setTimeout(r, 20));
		assert.equal(h.notices[1], "amq-bridge now serves Buzz requests.");
		assert.equal(JSON.parse(fs.readFileSync(marker, "utf8")).kind, "confirm");
	} finally {
		await h.done();
	}
});

// Review of pi admission evidence: a short receipt write was published and handed to pi.
test("a receipt cut short by a short write is never published; the retry publishes it whole", async () => {
	const h = harness();
	const realWrite = nodeFs.writeSync;
	let cut = false;
	nodeFs.writeSync = ((fd: number, data: string | Uint8Array, ...rest: unknown[]) => {
		const bytes = typeof data === "string" ? Buffer.from(data) : Buffer.from(data);
		if (!cut && bytes.includes("delivered_at")) {
			cut = true;
			return realWrite(fd, bytes.subarray(0, 8));
		}
		return (realWrite as (...a: unknown[]) => number)(fd, data, ...rest);
	}) as typeof nodeFs.writeSync;
	syncBuiltinESMExports();
	try {
		await h.start();
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.hasReceipt(REF));
		assert.ok(cut);
		assert.equal(JSON.parse(fs.readFileSync(path.join(h.dir, "receipts", `${REF}.json`), "utf8")).ref, REF);
		assert.equal(h.sent.length, 1);
	} finally {
		nodeFs.writeSync = realWrite;
		syncBuiltinESMExports();
		await h.done();
	}
});

// Review of pi admission evidence: a new events file's directory entry was never synced.
test("a line written into an empty events file syncs the events directory", async () => {
	const h = harness();
	const synced = new Set<number>();
	const realFsync = nodeFs.fsyncSync;
	nodeFs.fsyncSync = (fd: number) => {
		const st = fs.fstatSync(fd);
		if (st.isDirectory()) synced.add(st.ino);
		realFsync(fd);
	};
	syncBuiltinESMExports();
	try {
		fs.mkdirSync(path.join(h.dir, "events"), { recursive: true });
		fs.writeFileSync(path.join(h.dir, "events", `${REF}.jsonl`), ""); // its entry may not be durable yet
		await h.start();
		h.state.idle = false;
		h.publish(REF, "remote task", h.generation()); // refused busy: the first line in the empty file
		await h.waitFor(() => h.events(REF).length === 1);
		assert.equal(h.events(REF)[0].reason, "busy");
		assert.ok(synced.has(fs.statSync(path.join(h.dir, "events")).ino));
	} finally {
		nodeFs.fsyncSync = realFsync;
		syncBuiltinESMExports();
		await h.done();
	}
});

// Pro review 2026-09-29 #1: output collected before the request's own
// message_start, and a settle while it had not started, completed the
// request with a local turn's answer.
test("a settle before the request starts does not complete it with local output", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);

		await h.emit("message_start", userMessage("local task"));
		await h.emit("message_end", assistantMessage("LOCAL TASK RESULT"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled");
		assert.deepEqual(h.events(REF), []);

		await h.emit("message_start", userMessage("remote task"));
		await h.emit("message_end", assistantMessage("REMOTE RESULT"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled");
		const events = h.events(REF);
		assert.deepEqual(
			events.map((e) => e.event),
			["started", "completed"],
		);
		assert.equal(events[1].text, "REMOTE RESULT");
	} finally {
		await h.done();
	}
});

// Pro review 2026-09-29 #2: a request with an empty epoch_hint was a
// wildcard, so a request retained from generation A ran in generation B
// after session_start.
test("a request addressed to an earlier generation, or to none, is refused", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "retained task", h.generation());
		h.publish(OTHER, "retained task", "");
		await h.emit("session_start", { reason: "new" }); // generation B scans at once
		assert.deepEqual(h.sent, []);
		for (const ref of [REF, OTHER]) {
			assert.equal(h.hasReceipt(ref), false, ref);
			assert.deepEqual(
				h.events(ref).map((e) => [e.event, e.reason]),
				[["refused", "generation"]],
				ref,
			);
		}
	} finally {
		await h.done();
	}
});

// Pro review 2026-09-29 #3: a failed terminal append was dropped and the
// request cleared, so the stream stayed at started forever; orphan recovery
// appended onto a partial trailing line and recorded failed for an unknown
// outcome.
test("a terminal append is retried until durable, and recovery repairs a partial tail", async () => {
	const h = harness();
	const fragment = `{"protocol":"amq:pi-bridge:v1","ref":"${ORPHAN}","event":"star`;
	fs.mkdirSync(path.join(h.dir, "receipts"), { recursive: true });
	fs.mkdirSync(path.join(h.dir, "events"), { recursive: true });
	fs.copyFileSync(path.join(GOLDEN, "receipts", `${ORPHAN}.json`), path.join(h.dir, "receipts", `${ORPHAN}.json`));
	aged(h.dir, ORPHAN);
	fs.writeFileSync(path.join(h.dir, "events", `${ORPHAN}.jsonl`), fragment);
	aged(h.dir, ORPHAN);

	await h.start();
	try {
		const lines = fs.readFileSync(path.join(h.dir, "events", `${ORPHAN}.jsonl`), "utf8").split("\n");
		assert.equal(lines[0], fragment);
		assert.equal(JSON.parse(lines[1]).event, "uncertain");

		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);
		const stream = path.join(h.dir, "events", `${REF}.jsonl`);
		fs.mkdirSync(stream); // every append now fails
		await h.emit("message_start", userMessage("remote task"));
		await h.emit("message_end", assistantMessage("DONE"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled");

		fs.rmdirSync(stream); // the filesystem recovers
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.text]),
			[["completed", "DONE"]],
		);
	} finally {
		await h.done();
	}
});

// Pro review 2026-09-29 #4: with local work running, the bridge claimed a
// receipt and queued the request as a native follow-up instead of refusing
// busy.
test("a request that arrives while pi is busy is refused busy without a receipt", async () => {
	const h = harness();
	await h.start();
	try {
		h.state.idle = false;
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(h.sent, []);
		assert.equal(h.hasReceipt(REF), false);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.reason]),
			[["refused", "busy"]],
		);
	} finally {
		await h.done();
	}
});

// Pro review of #920, 2026-09-29, #3: after the remote request started, a
// local follow-up that started before the session settled supplied the
// completed answer.
test("a later user message ends the request's ownership of output", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);

		await h.emit("message_start", userMessage("remote task"));
		await h.emit("message_end", assistantMessage("REMOTE ANSWER"));
		await h.emit("message_start", userMessage("local follow-up"));
		await h.emit("message_end", assistantMessage("LOCAL ANSWER"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled");
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.text]),
			[
				["started", undefined],
				["completed", "REMOTE ANSWER"],
			],
		);
	} finally {
		await h.done();
	}
});

// Pro review of #920, 2026-09-29, #4: a busy refusal whose append failed
// went back to admission, so once pi was idle the next poll claimed a
// receipt and sent the request.
test("a busy refusal whose append failed is retried, never sent", async () => {
	const h = harness();
	await h.start();
	try {
		const stream = path.join(h.dir, "events", `${REF}.jsonl`);
		fs.mkdirSync(stream); // the refusal append fails
		h.state.idle = false;
		const checks = h.state.idleChecks;
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.state.idleChecks > checks); // the busy refusal was decided
		h.state.idle = true;
		fs.rmdirSync(stream); // the filesystem recovers
		await h.waitFor(() => h.events(REF).length > 0 || h.sent.length > 0);
		assert.deepEqual(h.sent, []);
		assert.equal(h.hasReceipt(REF), false);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.reason]),
			[["refused", "busy"]],
		);
	} finally {
		await h.done();
	}
});

// Pro review of #920, 2026-09-29, #6: one failed receipt read during
// orphan recovery counted as done, so the orphan was never closed in that
// runtime.
test("an orphan receipt whose read failed is recovered on a later poll", async () => {
	const h = harness();
	const receipt = path.join(h.dir, "receipts", `${ORPHAN}.json`);
	fs.mkdirSync(receipt, { recursive: true }); // the first read fails
	await h.start();
	try {
		fs.rmdirSync(receipt);
		fs.copyFileSync(path.join(GOLDEN, "receipts", `${ORPHAN}.json`), receipt); // the filesystem recovers
		aged(h.dir, ORPHAN);
		await h.waitFor(() => h.events(ORPHAN).length > 0);
		assert.deepEqual(
			h.events(ORPHAN).map((e) => e.event),
			["uncertain"],
		);
	} finally {
		await h.done();
	}
});

// Pro review of #923, 2026-09-29, #1: a tool-use preamble followed by local
// steering completed the request with the preamble, and a later error could
// not correct it.
test("a user message during a tool run leaves the request uncertain", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.sent.length === 1);

		await h.emit("message_start", userMessage("remote task"));
		await h.emit("message_end", toolUseMessage("I will inspect the files first"));
		await h.emit("message_start", userMessage("local steering"));
		await h.emit("message_end", { message: { role: "assistant", content: [], stopReason: "error", errorMessage: "boom" } });
		await h.emit("agent_before_settle", { outcome: "error" });
		await h.emit("agent_settled");
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["started", "uncertain"],
		);
	} finally {
		await h.done();
	}
});

// Pro review of #923, 2026-09-29, #4: a complete line whose fsync failed
// read as a terminal, so neither the kept refusal nor the orphan closure
// was appended again until durable.
test("a refusal and an orphan closure whose fsync failed are appended again", async () => {
	const h = harness();
	fs.mkdirSync(path.join(h.dir, "receipts"), { recursive: true });
	fs.mkdirSync(path.join(h.dir, "events"), { recursive: true });
	fs.copyFileSync(path.join(GOLDEN, "receipts", `${ORPHAN}.json`), path.join(h.dir, "receipts", `${ORPHAN}.json`));
	aged(h.dir, ORPHAN);
	const failOnce = new Set([`${ORPHAN}.jsonl`, `${REF}.jsonl`].map((n) => path.join(h.dir, "events", n)));
	const realFsync = nodeFs.fsyncSync;
	nodeFs.fsyncSync = (fd: number) => {
		for (const p of failOnce) {
			let same = false;
			try {
				same = fs.statSync(p).ino === fs.fstatSync(fd).ino;
			} catch {
				// Not created yet.
			}
			if (same) {
				failOnce.delete(p);
				throw new Error("injected fsync failure");
			}
		}
		realFsync(fd);
	};
	syncBuiltinESMExports();
	try {
		await h.start(); // the orphan's closing line is written, its fsync fails
		h.state.idle = false;
		h.publish(REF, "remote task", h.generation()); // the busy refusal's fsync fails
		await h.waitFor(() => h.events(REF).length === 2 && h.events(ORPHAN).length === 2);
		assert.deepEqual(h.events(ORPHAN).map((e) => e.event), ["uncertain", "uncertain"]);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.reason]),
			[
				["refused", "busy"],
				["refused", "busy"],
			],
		);
		assert.deepEqual(h.sent, []);
		assert.equal(h.hasReceipt(REF), false);
	} finally {
		nodeFs.fsyncSync = realFsync;
		syncBuiltinESMExports();
		await h.done();
	}
});

// Pro review of #923, 2026-09-29, #2: the extension accepted a request from
// an adapter that predates the revision fence, so an old amq-remote could
// send work into a replacement session. Such a request is refused without a
// receipt or a delivery.
// Pro review of #923 r3, 2026-09-29, #2: an old amq-remote reports the
// revision refusal as uncertain and then native_error, with no upgrade text,
// so the extension itself tells the owner, once per runtime.
test("an old adapter's requests are refused with one upgrade notice in pi", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "first", h.generation(), {});
		h.publish(OTHER, "second", h.generation(), {});
		await h.waitFor(() => h.events(REF).length > 0 && h.events(OTHER).length > 0);
		for (const ref of [REF, OTHER]) {
			assert.deepEqual(
				h.events(ref).map((e) => [e.event, e.reason]),
				[["refused", "revision"]],
			);
		}
		assert.deepEqual(h.sent, []);
		assert.equal(h.hasReceipt(REF), false);
		assert.equal(h.hasReceipt(OTHER), false);
		assert.equal(h.notices.length, 1);
		assert.match(h.notices[0], /amq-remote on this machine is older than this pi extension.*restart it/);
	} finally {
		await h.done();
	}
});

// Pre-merge review: truncating the activity file at a run start dropped the
// run before it, whose end the session mirror had not read yet.
test("a run start past the size cap keeps the run before it", async () => {
	const h = harness();
	await h.start();
	await h.emit("agent_start");
	const big = "x".repeat(48000);
	for (let i = 0; i < 90; i++) {
		await h.emit("tool_execution_end", { toolCallId: `c${i}`, toolName: "read", result: { content: [{ type: "text", text: big }] }, isError: false });
	}
	await h.emit("message_end", assistantMessage("first answer"));
	await h.emit("agent_before_settle", { outcome: "completed" });
	await h.emit("agent_settled", { aborted: false });
	await h.emit("agent_start");
	const lines = fs
		.readFileSync(path.join(h.dir, "activity", "pi-session-1.jsonl"), "utf8")
		.trim()
		.split("\n")
		.map((l) => JSON.parse(l));
	const kinds = lines.map((l) => l.kind);
	assert.equal(kinds[0], "run_start");
	assert.deepEqual(kinds.slice(-3), ["assistant", "settled", "run_start"]);
	assert.notEqual(lines[0].run, lines[lines.length - 1].run);
	await h.done();
});

test("session activity is recorded to activity/<session_id>.jsonl", async () => {
	const h = harness();
	await h.start();
	await h.emit("agent_start");
	await h.emit("turn_start", { turnIndex: 0 });
	await h.emit("message_end", userMessage("hello"));
	await h.emit("tool_execution_start", { toolCallId: "c1", toolName: "read", args: {} });
	await h.emit("tool_execution_end", { toolCallId: "c1", toolName: "read", result: { content: [{ type: "text", text: "file body" }] }, isError: false });
	await h.emit("message_end", assistantMessage("done"));
	await h.emit("turn_end", { turnIndex: 0 });
	await h.emit("agent_before_settle", { outcome: "completed" });
	await h.emit("agent_settled", { aborted: false });
	const lines = fs
		.readFileSync(path.join(h.dir, "activity", "pi-session-1.jsonl"), "utf8")
		.trim()
		.split("\n")
		.map((l) => JSON.parse(l));
	assert.deepEqual(
		lines.map((l) => [l.kind, l.text, l.tool, l.status]),
		[
			["run_start", undefined, undefined, undefined],
			["turn_start", undefined, undefined, undefined],
			["user", "hello", undefined, undefined],
			["tool_start", undefined, "read", undefined],
			["tool_end", "file body", "read", "completed"],
			["assistant", "done", undefined, undefined],
			["turn_end", undefined, undefined, undefined],
			["settled", undefined, undefined, "completed"],
		],
	);
	assert.equal(lines[2].turn, lines[1].turn);
	assert.match(lines[0].run, /^[0-9a-f]{8}-r1$/);
	assert.deepEqual(new Set(lines.map((l) => l.run)), new Set([lines[0].run]));
	await h.done();
});

test("a host application's identity and delivery run a request without AM_ROOT or AM_ME", async () => {
	const handed: { request: AmqRequest; report: (outcome: AmqOutcome) => void; receipt: boolean }[] = [];
	let busy = false;
	const h = harness((root) => ({
		identity: () => ({ root, handle: "pi-seat" }),
		deliver: {
			busy: () => busy,
			send(_ctx, request, report) {
				const receipt = fs.existsSync(path.join(root, "agents", "pi-seat", "extensions", "pi-bridge", "receipts", `${request.ref}.json`));
				handed.push({ request, report, receipt });
			},
		},
		upgrade: "update the host application",
	}));
	await h.start();
	try {
		const live = JSON.parse(fs.readFileSync(path.join(h.dir, "bridge.liveness"), "utf8"));
		assert.equal(live.upgrade, "update the host application");
		assert.equal(live.bridge_revision, 7);

		h.publish(REF, "remote task", h.generation(), { bridge_revision: 3, origin: { carrier: "link", name: "example" } });
		await h.waitFor(() => handed.length === 1);
		assert.equal(handed.length, 1);
		const { request, report, receipt } = handed[0];
		assert.equal(receipt, true);
		assert.equal(request.ref, REF);
		assert.equal(request.text, "remote task");
		assert.deepEqual(request.origin, { carrier: "link", name: "example" });
		assert.deepEqual(h.sent, []);

		// The host reports its own admission and outcome; pi's events do not
		// decide the request.
		report({ event: "started" });
		await h.emit("message_start", userMessage("remote task"));
		await h.emit("message_end", userMessage("remote task"));
		await h.emit("message_end", assistantMessage("pi's own text"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled", { aborted: false });
		report({ event: "completed", text: "HOST RESULT" });
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.text]),
			[
				["started", undefined],
				["completed", "HOST RESULT"],
			],
		);
		const activity = fs
			.readFileSync(path.join(h.dir, "activity", "pi-session-1.jsonl"), "utf8")
			.trim()
			.split("\n")
			.map((l) => JSON.parse(l));
		assert.equal(activity.find((l) => l.kind === "user").id, REF);

		busy = true;
		h.publish(OTHER, "second task", h.generation());
		await h.waitFor(() => h.events(OTHER).length > 0);
		assert.deepEqual(
			h.events(OTHER).map((e) => [e.event, e.reason]),
			[["refused", "busy"]],
		);
		assert.equal(h.hasReceipt(OTHER), false);
		assert.equal(handed.length, 1);
	} finally {
		await h.done();
	}
});

// hostHarness embeds the bridge with a host identity at root and the given
// delivery.
const hostHarness = (deliver: AmqBridgeOptions["deliver"]) => harness((root) => ({ identity: () => ({ root, handle: "pi-seat" }), deliver }));

// Review of the pi and images code, finding 2: the request's ref tags its own
// user message, matched by its text, not whichever user message ends after the
// report.
test("a host request's ref tags only its own user message", async () => {
	let report: ((outcome: AmqOutcome) => void) | undefined;
	const h = hostHarness({
		busy: () => false,
		send(_ctx, _request, r) {
			report = r;
		},
	});
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => report !== undefined);
		report?.({ event: "started" });
		for (const text of ["queued local note", "remote task"]) {
			await h.emit("message_start", userMessage(text));
			await h.emit("message_end", userMessage(text));
		}
		const users = fs
			.readFileSync(path.join(h.dir, "activity", "pi-session-1.jsonl"), "utf8")
			.trim()
			.split("\n")
			.map((l) => JSON.parse(l))
			.filter((l) => l.kind === "user")
			.map((l) => [l.text, l.id ?? ""]);
		assert.deepEqual(users, [
			["queued local note", ""],
			["remote task", REF],
		]);
	} finally {
		await h.done();
	}
});

// Review of the pi and images code, finding 3: a report outside the AmqOutcome
// shapes, such as a busy refusal after the receipt, settles the request
// uncertain.
test("a host report outside the outcome shapes settles the request uncertain", async () => {
	const h = hostHarness({
		busy: () => false,
		send(_ctx, _request, report) {
			report({ event: "started" });
			report({ event: "refused", reason: "busy" } as never);
		},
	});
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.events(REF).length > 1);
		const events = h.events(REF);
		assert.deepEqual(
			events.map((e) => e.event),
			["started", "uncertain"],
		);
		assert.match(events[1].error, /"reason":"busy"/);
	} finally {
		await h.done();
	}
});

// Review of the pi and images code, finding 4: a host send that throws after it
// reported the start leaves the outcome unknown, never failed.
test("a host send that throws after the start settles the request uncertain", async () => {
	const h = hostHarness({
		busy: () => false,
		send(_ctx, _request, report) {
			report({ event: "started" });
			throw new Error("host broke");
		},
	});
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.events(REF).length > 1);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.error]),
			[
				["started", undefined],
				["uncertain", "deliver: Error: host broke"],
			],
		);
	} finally {
		await h.done();
	}
});

// Review of the pi and images code, finding 5: a host busy check that throws
// refuses the request busy instead of stopping every scan.
test("a host busy check that throws refuses the request busy", async () => {
	const h = hostHarness({
		busy: () => {
			throw new Error("busy check broke");
		},
		send() {
			assert.fail("a request was sent past a broken busy check");
		},
	});
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation());
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.reason, e.error]),
			[["refused", "busy", "deliver.busy: Error: busy check broke"]],
		);
		assert.equal(h.hasReceipt(REF), false);
	} finally {
		await h.done();
	}
});

// Review of the pi and images code, finding 6: a session the host keeps
// inactive shows no wake marker for the identity the host gives without a session.
test("a session the host keeps inactive holds no wake doorbell", async () => {
	let agentDir = "";
	const h = harness((root) => {
		agentDir = path.join(root, "agents", "pi-seat");
		fs.mkdirSync(agentDir, { recursive: true });
		return { identity: (ctx) => (ctx ? undefined : { root, handle: "pi-seat" }) };
	});
	await h.start();
	try {
		await h.emit("ui_prompt_start", { reason: "ui_prompt", kind: "confirm", title: "Run it?" });
		assert.equal(fs.existsSync(path.join(agentDir, ".wake.dialog")), false);
	} finally {
		await h.done();
	}
});

// Review of the pi and images code, finding 7: a relative root is no identity,
// so the bridge never writes under the working directory.
test("a host identity with a relative root keeps the bridge inactive", async () => {
	const rel = `amq-pi-relative-${process.pid}`;
	const h = harness(() => ({ identity: () => ({ root: rel, handle: "pi-seat" }) }));
	await h.start();
	try {
		assert.equal(fs.existsSync(path.resolve(rel)), false);
	} finally {
		fs.rmSync(path.resolve(rel), { recursive: true, force: true });
		await h.done();
	}
});

// Review round 1 of the pi and images code, finding 5: the plain
// extension's inactive notice names a relative AM_ROOT as the cause.
test("a relative AM_ROOT is named in the inactive notice", async () => {
	const h = harness();
	const rel = `amq-pi-relative-env-${process.pid}`;
	process.env.AM_ROOT = rel;
	await h.start();
	try {
		assert.equal(h.notices.length, 1);
		assert.match(h.notices[0], /AM_ROOT .* is not an absolute path/);
		assert.equal(fs.existsSync(path.resolve(rel)), false);
	} finally {
		fs.rmSync(path.resolve(rel), { recursive: true, force: true });
		await h.done();
	}
});

// --- The guard of granted tasks (bridge revision 6) ---------------------------

const GRANTED = { bridge_revision: 6, granted: true };
const DENIED = { block: true, reason: "The person at the terminal did not approve this call. It is a call of a task that a linked server sent under a session grant." };
const NO_TERMINAL = { block: true, reason: "This call needs the person's yes at the terminal, and this pi session has no terminal UI." };
const TOO_LONG = { block: true, reason: "This call is too long to show whole at the terminal for approval. A task that needs it must be sent with a signature." };
const EXTENSION_LINE = "This is a tool of an extension. AMQ cannot see what it does or starts; one yes covers the whole call.";
const typeYes = (tool: string) => `\n\nType yes to run ${tool}. Any other answer does not run it.`;
const askTitle = (tool: string, input: unknown, extension = false) =>
	`A task that a linked server sent wants to run \`${tool}\`:\n${JSON.stringify(input, null, 1)}${extension ? `\n${EXTENSION_LINE}` : ""}${typeYes(tool)}`;
const mayStillTitle = (tool: string, input: unknown) => `A task that a linked server sent may still run in this session. \`${tool}\` wants to run:\n${JSON.stringify(input, null, 1)}${typeYes(tool)}`;

type Harness = ReturnType<typeof harness>;

// typed is the person's next prompt typed at pi's terminal: pi emits input
// with source interactive before the run that it starts.
const typed = (h: Harness, fields: object = {}) => h.emit("input", { text: "my own prompt", source: "interactive", ...fields });

// guardOf reads the guard file of process pid in h's bridge directory.
function guardOf(h: Harness, pid = process.pid): { pid: number; arms: { arm: string; ref: string; armed_at: string }[] } | null {
	try {
		return JSON.parse(fs.readFileSync(path.join(h.dir, `granted-guard.${pid}.json`), "utf8"));
	} catch {
		return null;
	}
}

// granted starts a fresh module instance and hands it one granted request.
// tag starts the request's own user message, as pi does when it runs it.
async function granted(tag = true): Promise<Harness> {
	const h = harness(undefined, undefined, await freshBridge());
	await h.start();
	h.publish(REF, "remote task", h.generation(), GRANTED);
	await h.waitFor(() => h.sent.length === 1);
	assert.equal(h.sent.length, 1);
	if (tag) await h.emit("message_start", userMessage(h.sent[0].content as string));
	return h;
}

test("a granted task's tool calls ask the person at the terminal until the person types after its run settles", async () => {
	const h = harness(undefined, undefined, await freshBridge());
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation(), GRANTED);
		await h.waitFor(() => h.sent.length === 1);
		assert.deepEqual(h.armedAtSend, [true]);
		const file = path.join(h.dir, `granted-guard.${process.pid}.json`);
		assert.equal(fs.statSync(file).mode & 0o777, 0o600);
		const guard = guardOf(h);
		assert.equal(guard?.pid, process.pid);
		assert.equal(guard?.arms.length, 1);
		const { arm, ref } = guard!.arms[0];
		assert.match(arm, /^[0-9a-f]{32}$/);
		assert.equal(ref, REF);
		assert.deepEqual(h.sent[0], { content: `remote task\n\n[AMQ granted task ${arm}]`, options: { deliverAs: "followUp" } });

		await h.emit("message_start", userMessage(h.sent[0].content as string));
		await h.emit("message_end", userMessage(h.sent[0].content as string));
		const activity = fs
			.readFileSync(path.join(h.dir, "activity", "pi-session-1.jsonl"), "utf8")
			.trim()
			.split("\n")
			.map((line) => JSON.parse(line));
		assert.deepEqual(activity.filter((row) => row.kind === "user").map((row) => [row.id, row.text]), [[REF, "remote task"]]);
		h.answer = () => "yes";
		const input = { command: "ls -la" };
		assert.equal(await h.tool("bash", input), undefined);
		assert.deepEqual(h.dialogs, [{ title: askTitle("bash", input), placeholder: "" }]);

		await h.emit("message_end", assistantMessage("listed"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.equal(fs.existsSync(file), false);
		const lookups = h.state.lookups;
		assert.equal(await h.tool("bash", input), undefined);
		assert.equal(h.dialogs.length, 1);
		assert.equal(h.state.lookups, lookups);
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["started", "completed"],
		);
	} finally {
		await h.done();
	}
});

test("liveness publishes gate only for the TUI with the bridge's own hand-off", async () => {
	const gateOf = (h: Harness) => JSON.parse(fs.readFileSync(path.join(h.dir, "bridge.liveness"), "utf8")).gate;
	const tui = harness();
	await tui.start();
	assert.equal(gateOf(tui), true);
	await tui.done();
	for (const mode of ["rpc", "print"]) {
		const h = harness();
		h.ctx.mode = mode;
		await h.start();
		assert.equal(gateOf(h), undefined, mode);
		await h.done();
	}
	const host = hostHarness({ busy: () => false, send() {} });
	await host.start();
	assert.equal(gateOf(host), undefined, "host deliver seam");
	await host.done();
});

// Guard table, row "read-only built-ins": pi's own read, grep, find and ls
// run with no dialog.
test("guard: pi's own read-only built-ins pass with no dialog", async () => {
	const h = await granted();
	try {
		for (const name of ["read", "grep", "find", "ls"]) assert.equal(await h.tool(name, { path: "." }), undefined, name);
		assert.equal(h.dialogs.length, 0);
	} finally {
		await h.done();
	}
});

// Guard table, row "read replaced": an extension can register a tool under a
// built-in's name; provenance, not the name, exempts a call.
test("guard: read replaced by an extension asks, as an extension's tool", async () => {
	const h = await granted();
	try {
		h.tools[0] = { name: "read", sourceInfo: { source: "local", path: "/ext/read.ts" } };
		h.answer = () => "no";
		assert.deepEqual(await h.tool("read", { path: "." }), DENIED);
		assert.equal(h.dialogs[0].title, askTitle("read", { path: "." }, true));
	} finally {
		await h.done();
	}
});

// Guard table, the typed answer: only yes, after trimming spaces and
// lowering case, runs a call. A short or foreign yes, text typed ahead into
// the dialog, Enter alone, Escape, a throwing dialog and an aborted run
// block it.
test("guard: only a typed yes runs the call", async () => {
	const h = await granted();
	try {
		for (const yes of ["yes", " Yes "]) {
			h.answer = () => yes;
			assert.equal(await h.tool("bash", { command: "rm -rf build" }), undefined, JSON.stringify(yes));
		}
		const answers: [string, () => string | undefined][] = [
			["y", () => "y"],
			["j", () => "j"],
			["Enter alone", () => ""],
			["typed ahead", () => "please adjust the build"],
			["Escape", () => undefined],
			[
				"throw",
				() => {
					throw new Error("dialog broke");
				},
			],
		];
		for (const [name, fn] of answers) {
			h.answer = fn;
			assert.deepEqual(await h.tool("bash", { command: "rm -rf build" }), DENIED, name);
		}
		h.answer = () => "yes";
		h.ctx.signal = AbortSignal.abort();
		assert.deepEqual(await h.tool("bash", { command: "rm -rf build" }), DENIED, "aborted");
		assert.equal(h.dialogs.length, 9);
	} finally {
		await h.done();
	}
});

// A pi host without the optional title measurer can still serve ordinary requests.
test("guard: an unavailable TUI package refuses only the call that needs measurement", async (t) => {
	const hooks = registerHooks({
		resolve(specifier, context, nextResolve) {
			if (specifier === "@earendil-works/pi-tui") throw new Error("pi-tui unavailable in this host");
			return nextResolve(specifier, context);
		},
	});
	t.after(() => hooks.deregister());
	const h = harness(undefined, undefined, await freshBridge());
	try {
		await h.start();
		h.publish(REF, "ordinary request", h.generation());
		await h.waitFor(() => h.sent.length === 1);
		assert.equal(h.sent[0].content, "ordinary request");
		await h.emit("message_start", userMessage("ordinary request"));
		await h.emit("message_end", assistantMessage("done"));
		await h.emit("agent_before_settle", { outcome: "completed" });
		await h.emit("agent_settled", { aborted: false });
		assert.equal(h.events(REF).at(-1)?.event, "completed");
		h.publish(OTHER, "guarded request", h.generation(), GRANTED);
		await h.waitFor(() => h.sent.length === 2);
		assert.deepEqual(await h.tool("bash", { command: "pwd" }), {
			block: true, reason: "This pi cannot measure the approval dialog; a task that needs this call must be sent with a signature.",
		});
		assert.equal(h.dialogs.length, 0);
	} finally {
		await h.done();
	}
});

// Guard table, rows "rpc" and "print": with no terminal there is nobody to
// ask, so the call is blocked and no dialog opens.
test("guard: a session that is not tui blocks the call with no dialog", async () => {
	const h = await granted();
	try {
		for (const mode of ["rpc", "print"]) {
			h.ctx.mode = mode;
			assert.deepEqual(await h.tool("bash", { command: "make" }), NO_TERMINAL, mode);
		}
		assert.equal(h.dialogs.length, 0);
	} finally {
		await h.done();
	}
});

// Guard table, rows "nested call", "an extension's tool", "the dialog text":
// every call is shown as the JSON of its whole input, and a tool that is not
// pi's own carries the extension line, a built-in extension's tool too.
test("guard: nested calls and extension tools ask, showing the whole input", async () => {
	const h = await granted();
	try {
		h.tools.push({ name: "deploy", sourceInfo: { source: "local", path: "/ext/deploy.ts" } }, { name: "mcp_query", sourceInfo: { source: "builtin", path: "builtin:mcp" } });
		h.answer = () => "yes";
		const calls: [string, unknown, boolean, object][] = [
			["bash", { command: 'echo "a"\nrm x', timeout: 5 }, false, {}],
			["edit", { path: "a.go", edits: [{ oldText: "x", newText: "y" }] }, false, {}],
			["write", { path: "b.go", content: "package b\n" }, false, { parentToolCallId: "call-0" }],
			["deploy", { target: "prod", args: ["--force"] }, true, {}],
			["mcp_query", { sql: "drop table t" }, true, {}],
		];
		for (const [name, input, extension, extra] of calls) {
			assert.equal(await h.tool(name, input, extra), undefined, name);
			assert.equal(h.dialogs.at(-1)?.title, askTitle(name, input, extension), name);
		}
		assert.equal(h.dialogs.length, calls.length);
	} finally {
		await h.done();
	}
});

// Guard table, row "over the limit": a call the person cannot see whole is
// never offered for approval.
test("guard: a call text over 2000 characters blocks with no dialog", async () => {
	const h = await granted();
	try {
		h.answer = () => "yes";
		assert.deepEqual(await h.tool("write", { path: "big.txt", content: "x".repeat(2000) }), TOO_LONG);
		assert.equal(h.dialogs.length, 0);
	} finally {
		await h.done();
	}
});

// Guard table, row "over one screen": a call under 2000 characters that
// takes more than 20 rows at 80 columns is never offered for approval; a
// long one-line command that fits 20 rows is.
test("guard: a call text over 20 rows at 80 columns blocks with no dialog", async () => {
	const h = await granted();
	try {
		h.answer = () => "yes";
		const edits = { path: "a.go", edits: Array.from({ length: 12 }, (_, i) => ({ oldText: `a${i}`, newText: `b${i}` })) };
		assert.ok(JSON.stringify(edits, null, 1).length < 2000);
		assert.deepEqual(await h.tool("edit", edits), TOO_LONG);
		assert.equal(h.dialogs.length, 0);
		// A long ASCII argument still fits the padded title.
		const command = { command: "x".repeat(1250) };
		assert.equal(await h.tool("bash", command), undefined);
		assert.equal(h.dialogs.length, 1);
	} finally {
		await h.done();
	}
});

// Round-two review: Pi wraps wide glyphs and uses the terminal's current size.
test("guard: the call must fit the displayed cells and available terminal rows", async (t) => {
	const h = await granted();
	try {
		h.answer = () => "no";
		for (const [name, columns, rows, value, fits] of [
			["wide glyphs", 80, 40, "界".repeat(80 * 9), false],
			["wide terminal", 80, 40, "a".repeat(800), true],
			["narrow terminal", 40, 40, "a".repeat(800), false],
			["short terminal", 80, 12, "a".repeat(800), false],
			// The short bash title occupies six rows at this width.
			["exactly fits the dock", 80, 18, "pwd", true],
			["one row beyond the dock", 80, 17, "pwd", false],
			["wrapped key hints", 39, 40, "pwd", false],
		] as const) {
			await t.test(name, async () => {
				const previousColumns = process.stdout.columns;
				const previousRows = process.stdout.rows;
				const dialogs = h.dialogs.length;
				try {
					process.stdout.columns = columns;
					process.stdout.rows = rows;
					assert.deepEqual(await h.tool("bash", { command: value }), fits ? DENIED : TOO_LONG);
					assert.equal(h.dialogs.length - dialogs, fits ? 1 : 0);
				} finally {
					process.stdout.columns = previousColumns;
					process.stdout.rows = previousRows;
				}
			});
		}
	} finally {
		await h.done();
	}
});

// Guard table, row "hidden characters": a bidi override, a zero-width
// space, a C1 control and a line separator in the input or the tool name
// are shown as their escapes, never as they are.
test("guard: characters that hide or reorder text are shown escaped", async () => {
	const h = await granted();
	try {
		h.tools.push({ name: "de\u202eploy\n\u001b\t", sourceInfo: { source: "local", path: "/ext/deploy.ts" } });
		h.answer = () => "no";
		await h.tool("bash", { command: "echo a\u202eb\u200bc\u009bd\u2028e" });
		await h.tool("de\u202eploy\n\u001b\t", {});
		const titles = h.dialogs.map((d) => d.title);
		assert.ok(titles[0].includes(String.raw`echo a\u202eb\u200bc\u009bd\u2028e`), titles[0]);
		assert.ok(titles[1].includes("`de\\u202eploy\\u000a\\u001b\\u0009`"), titles[1]);
		for (const title of titles) assert.doesNotMatch(title, /[\u007f-\u009f\u2028\u2029]|\p{Cf}/u);
	} finally {
		await h.done();
	}
});

// Guard table, row "text changed by an input handler": the arm does not
// depend on a text match, so a granted message whose arm line an input
// extension removed keeps the guard for the life of the process.
test("guard: a granted message whose text an input handler changed stays guarded after it settles", async () => {
	const h = await granted(false);
	try {
		await h.emit("message_start", userMessage("remote task, rewritten by an input extension"));
		h.answer = () => "no";
		assert.deepEqual(await h.tool("bash", { command: "make" }), DENIED);
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.deepEqual(await h.tool("bash", { command: "make" }), DENIED);
		assert.equal(h.dialogs[1].title, mayStillTitle("bash", { command: "make" }));
		assert.equal(guardOf(h)?.arms.length, 1);
	} finally {
		await h.done();
	}
});

// Guard table, row "the person's prompt with the request's text": text
// equality alone cannot tell it from the granted message; the arm line can.
test("guard: the person's prompt with the request's text and no arm line tags and ends nothing", async () => {
	const h = await granted(false);
	try {
		await h.emit("message_start", userMessage("remote task"));
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.equal(guardOf(h)?.arms.length, 1);
		await h.emit("message_start", userMessage(h.sent[0].content as string));
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.equal(guardOf(h), null);
	} finally {
		await h.done();
	}
});

// Guard table, row "a second user message": a message the person types into
// the granted run does not lift the guard.
test("guard: a second user message in the run does not end the guard", async () => {
	const h = await granted();
	try {
		await h.emit("message_start", userMessage("also fix the tests"));
		h.answer = () => "no";
		assert.deepEqual(await h.tool("bash", { command: "make test" }), DENIED);
		assert.equal(guardOf(h)?.arms.length, 1);
	} finally {
		await h.done();
	}
});

// Guard table, rows "pending message" and "settle before the start".
test("guard: a settle with a pending message, or before the granted message started, does not end it", async () => {
	const early = await granted(false);
	try {
		await early.emit("agent_settled", { aborted: false });
		await typed(early);
		assert.equal(guardOf(early)?.arms.length, 1);
	} finally {
		await early.done();
	}
	const h = await granted();
	try {
		h.state.pending = true;
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.equal(guardOf(h)?.arms.length, 1);
		h.state.pending = false;
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.equal(guardOf(h), null);
	} finally {
		await h.done();
	}
});

// Guard table, rows "after the settle": a settled granted run keeps the
// guard for a run that no person typed, such as one an extension starts or
// steering typed during the run; the person's next typed prompt ends it.
test("guard: the guard ends at the person's next typed prompt after the granted run settled", async () => {
	const h = await granted();
	try {
		h.answer = () => "no";
		await typed(h, { streamingBehavior: "steer" });
		await h.emit("agent_settled", { aborted: false });
		assert.deepEqual(await h.tool("bash", { command: "make" }), DENIED, "after the settle, nothing typed");
		await h.emit("input", { text: "next step", source: "extension" });
		assert.deepEqual(await h.tool("bash", { command: "make" }), DENIED, "an extension's input");
		await h.emit("agent_start");
		for (const streamingBehavior of ["steer", "followUp"]) {
			await typed(h, { streamingBehavior });
			assert.deepEqual(await h.tool("bash", { command: "pwd" }), DENIED, streamingBehavior);
		}
		assert.equal(guardOf(h)?.arms.length, 1);
		await h.emit("agent_settled", { aborted: false });
		await typed(h);
		assert.equal(await h.tool("bash", { command: "make" }), undefined, "the person's typed prompt");
		assert.equal(guardOf(h), null);
	} finally {
		await h.done();
	}
});

// Guard table, row "another session's settle": an arm advances and ends
// only by events of the session that ran its granted message, not by a
// settle and a typed prompt of another registration of the same module.
test("guard: another session of the same module does not end the arm", async () => {
	const mod = await freshBridge();
	const parent = harness(undefined, undefined, mod);
	await parent.start();
	const child = harness(undefined, parent.root, mod);
	try {
		await child.start();
		parent.publish(REF, "remote task", parent.generation(), GRANTED);
		await parent.waitFor(() => parent.sent.length === 1);
		await parent.emit("message_start", userMessage(parent.sent[0].content as string));
		await child.emit("agent_settled", { aborted: false });
		await typed(parent, { streamingBehavior: "steer" });
		parent.answer = () => "no";
		assert.deepEqual(await parent.tool("bash", { command: "pwd" }), DENIED, "child settled, parent steered");
		await parent.emit("agent_settled", { aborted: false });
		await typed(child);
		assert.deepEqual(await parent.tool("bash", { command: "pwd" }), DENIED, "parent settled, child typed");
		assert.equal(guardOf(parent)?.arms.length, 1);
	} finally {
		await child.emit("session_shutdown", { reason: "quit" });
		await parent.done();
	}
});

// Guard table, row "send throws": a hand-off that fails synchronously proves
// that nothing was delivered.
test("guard: a sendUserMessage that throws ends its arm", async () => {
	const h = harness(undefined, undefined, await freshBridge());
	await h.start();
	try {
		h.state.throwSend = true;
		h.publish(REF, "remote task", h.generation(), GRANTED);
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(h.armedAtSend, [true]);
		assert.deepEqual(h.guards(), []);
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["failed"],
		);
		assert.equal(await h.tool("bash", { command: "make" }), undefined);
		assert.equal(h.dialogs.length, 0);
	} finally {
		await h.done();
	}
});

// Guard table, row "guard write fails": a granted request whose guard file
// cannot be written is never handed to pi.
test("guard: a guard file that cannot be written sends nothing and settles the request failed", async () => {
	const h = harness(undefined, undefined, await freshBridge());
	await h.start();
	try {
		fs.mkdirSync(path.join(h.dir, `granted-guard.${process.pid}.json`, "block"), { recursive: true });
		h.publish(REF, "remote task", h.generation(), GRANTED);
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(h.sent, []);
		assert.deepEqual(
			h.events(REF).map((e) => e.event),
			["failed"],
		);
		assert.equal(await h.tool("bash", { command: "make" }), undefined);
	} finally {
		await h.done();
	}
});

// Guard table, row "granted not a boolean": only true grants; any other
// present value that is not a boolean is refused before the receipt.
test("guard: a granted value that is not a boolean is refused invalid with nothing sent", async () => {
	for (const value of [1, "true", null]) {
		const h = harness(undefined, undefined, await freshBridge());
		await h.start();
		try {
			h.publish(REF, "remote task", h.generation(), { bridge_revision: 6, granted: value });
			await h.waitFor(() => h.events(REF).length > 0 || h.sent.length > 0);
			assert.equal(h.hasReceipt(REF), false, JSON.stringify(value));
			assert.deepEqual(
				h.events(REF).map((e) => [e.event, e.reason]),
				[["refused", "invalid"]],
				JSON.stringify(value),
			);
			assert.deepEqual(h.sent, [], JSON.stringify(value));
		} finally {
			await h.done();
		}
	}
});

// Guard table, row "reload": pi loads a new module instance on reload; it
// finds the guard file of its own process and stays guarded, untagged.
test("guard: a reload in the same process restores the guard, which a settled run then keeps", async (t) => {
	const before = await granted();
	await before.emit("session_shutdown", { reason: "reload" });
	// A wall-clock correction between loads leaves this process running.
	const wallNow = Date.now.bind(Date);
	t.mock.method(Date, "now", () => wallNow() + 5000);
	const after = harness(undefined, before.root, await freshBridge());
	try {
		await after.emit("session_start", { reason: "reload" });
		after.answer = () => "no";
		assert.deepEqual(await after.tool("bash", { command: "make" }), DENIED);
		assert.equal(after.dialogs[0].title, mayStillTitle("bash", { command: "make" }));
		await after.emit("agent_settled", { aborted: false });
		await typed(after);
		assert.deepEqual(await after.tool("bash", { command: "make" }), DENIED);
		assert.equal(guardOf(after)?.arms.length, 1);
	} finally {
		await after.done();
		await before.done();
	}
});

// Guard table, row "a reused pid": a guard file named for this pid whose
// process start is not this process's was left by an earlier process with
// the same pid. It is removed and arms nothing; with this process's start
// it restores.
test("guard: a guard file of an earlier process with this pid is removed and arms nothing", async () => {
	const started = Math.round(performance.timeOrigin) / 1000;
	for (const [name, at, armed] of [
		["earlier process", started - 3600, false],
		["this process", started, true],
		["missing origin", undefined, true],
		["unreadable origin", "unknown", true],
	] as const) {
		const root = fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-bridge-"));
		const dir = path.join(root, "agents", "pi-seat", "extensions", "pi-bridge");
		fs.mkdirSync(dir, { recursive: true });
		const file = path.join(dir, `granted-guard.${process.pid}.json`);
		fs.writeFileSync(file, JSON.stringify({ pid: process.pid, started: at, arms: [{ arm: "b".repeat(32), ref: REF, armed_at: "2000-01-01T00:00:00.000Z" }] }), { mode: 0o600 });
		const h = harness(undefined, root, await freshBridge());
		await h.start();
		try {
			h.answer = () => "no";
			assert.deepEqual(await h.tool("bash", { command: "ls" }), armed ? DENIED : undefined, name);
			assert.equal(fs.existsSync(file), armed, name);
		} finally {
			await h.done();
			fs.rmSync(root, { recursive: true, force: true });
		}
	}
});

// Guard table, row "another pid's file": it belongs to another pi process.
test("guard: another process's guard file is not read, changed or obeyed", async () => {
	const other = process.pid + 1;
	const root = fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-bridge-"));
	const dir = path.join(root, "agents", "pi-seat", "extensions", "pi-bridge");
	fs.mkdirSync(dir, { recursive: true });
	const file = path.join(dir, `granted-guard.${other}.json`);
	const body = JSON.stringify({ pid: other, arms: [{ arm: "a".repeat(32), ref: REF, armed_at: "2000-01-01T00:00:00.000Z" }] });
	fs.writeFileSync(file, body, { mode: 0o600 });
	const mtime = fs.statSync(file).mtimeMs;
	const h = harness(undefined, root, await freshBridge());
	await h.start();
	try {
		assert.equal(await h.tool("bash", { command: "make" }), undefined);
		assert.equal(h.dialogs.length, 0);
		await h.emit("session_shutdown", { reason: "quit" });
		assert.equal(fs.readFileSync(file, "utf8"), body);
		assert.equal(fs.statSync(file).mtimeMs, mtime);
	} finally {
		fs.rmSync(root, { recursive: true, force: true });
	}
});

// Guard table, row "two pids on one mailbox": the guard is the executing
// process's, not the mailbox's.
test("guard: two pi processes on one mailbox each keep their own guard", async () => {
	const other = process.pid + 1;
	const served = await granted();
	const held = harness(undefined, served.root, await freshBridge(other));
	try {
		await held.start();
		assert.equal(held.notices.length, 1); // held off: the first pi serves the mailbox
		assert.equal(await held.tool("bash", { command: "make" }), undefined);
		assert.equal(held.dialogs.length, 0);
		served.answer = () => "no";
		assert.deepEqual(await served.tool("bash", { command: "make" }), DENIED);
		await held.emit("session_shutdown", { reason: "quit" });
		assert.deepEqual(served.guards(), [`granted-guard.${process.pid}.json`]);
	} finally {
		await served.done();
	}
});

// Guard table, rows "two arms" and "a second send that throws": each
// granted task ends only its own arm.
test("guard: a second granted task ends only its own arm", async () => {
	for (const throws of [false, true]) {
		const h = await granted(false); // the first task's arm line was removed: no tag
		try {
			const first = guardOf(h)?.arms[0].arm;
			await h.emit("session_start", { reason: "new" });
			h.state.throwSend = throws;
			h.publish(OTHER, "second task", h.generation(), GRANTED);
			await h.waitFor(() => h.sent.length === 2 || h.events(OTHER).length > 0);
			if (!throws) {
				assert.equal(guardOf(h)?.arms.length, 2);
				await h.emit("message_start", userMessage(h.sent[1].content as string));
				await h.emit("agent_settled", { aborted: false });
				await typed(h);
			}
			assert.deepEqual(
				guardOf(h)?.arms.map((a) => a.arm),
				[first],
				throws ? "send throws" : "settled",
			);
			h.answer = () => "no";
			assert.deepEqual(await h.tool("bash", { command: "make" }), DENIED);
		} finally {
			await h.done();
		}
	}
});

// Guard table, row "displacement": a bridge that loses its mailbox keeps the
// guard of the run it handed off.
test("guard: displacement of the mailbox during the run keeps the guard", async () => {
	const h = await granted();
	try {
		const other = { protocol: "amq:pi-bridge:v1", live: true, at: new Date().toISOString(), pid: 4242, surface: "tui", session_generation: "other-generation", bridge_revision: 6 };
		fs.writeFileSync(path.join(h.dir, "bridge.liveness"), JSON.stringify(other));
		await h.waitFor(() => h.notices.length === 1);
		assert.match(h.notices[0], /no longer serves Buzz requests/);
		h.answer = () => "no";
		assert.deepEqual(await h.tool("bash", { command: "make" }), DENIED);
	} finally {
		await h.done();
	}
});

// Guard table, row "no granted": a request without the field changes nothing.
test("guard: a request with an absent or false granted field arms nothing", async () => {
	for (const fields of [{}, { granted: false }]) {
		const h = harness(undefined, undefined, await freshBridge());
		await h.start();
		try {
			h.publish(REF, "remote task", h.generation(), { bridge_revision: 6, ...fields });
			await h.waitFor(() => h.sent.length === 1);
			assert.equal(h.sent[0].content, "remote task");
			assert.deepEqual(h.guards(), []);
			assert.equal(await h.tool("bash", { command: "make" }), undefined);
			assert.equal(h.state.lookups, 0);
		} finally {
			await h.done();
		}
	}
});

// Guard table, rows "rpc", "revision 5", "host deliver seam": a bridge that
// cannot guard the run refuses the granted request before its receipt.
test("guard: a granted request that the bridge cannot guard is refused with nothing sent", async () => {
	const cases: [string, () => Harness, Record<string, unknown>, string][] = [
		[
			"rpc",
			() => {
				const h = harness();
				h.ctx.mode = "rpc";
				return h;
			},
			GRANTED,
			"unsupported",
		],
		["revision 5", () => harness(), { bridge_revision: 5, granted: true }, "revision"],
		["host deliver seam", () => hostHarness({ busy: () => false, send: () => assert.fail("a granted request reached the host") }), GRANTED, "unsupported"],
	];
	for (const [name, make, fields, reason] of cases) {
		const h = make();
		await h.start();
		try {
			h.publish(REF, "remote task", h.generation(), fields);
			await h.waitFor(() => h.events(REF).length > 0);
			assert.deepEqual(
				h.events(REF).map((e) => [e.event, e.reason]),
				[["refused", reason]],
				name,
			);
			assert.deepEqual(h.sent, [], name);
			assert.equal(h.hasReceipt(REF), false, name);
			assert.deepEqual(h.guards(), [], name);
		} finally {
			await h.done();
		}
	}
});

// The wake doorbell exchange (revision 7): the wake writes one request file
// under doorbell/ and races the bridge for the attempt's single decision
// file. A claim shows the fixed line as a visible custom message that
// starts one turn; a matching message_start is the acceptance result.

// doorbell publishes one wake doorbell request for the attempt.
function doorbell(h: ReturnType<typeof harness>, attempt: string, fields: Record<string, unknown> = {}) {
	const file = path.join(h.dir, "doorbell", `${attempt}.json`);
	const request = {
		protocol: "amq:pi-bridge:v1",
		op: "doorbell",
		attempt,
		generation: h.generation(),
	// The pid the wake names is the pi process this request is for; the
// extension serves only its own.
	pid: process.pid,
		deadline: new Date(Date.now() + 30_000).toISOString(),
		...fields,
	};
	fs.mkdirSync(path.dirname(file), { recursive: true });
	const tmp = `${file}.tmp-test`;
	fs.writeFileSync(tmp, JSON.stringify(request));
	fs.renameSync(tmp, file);
}

// doorbellDecision reads the attempt's decision file, null when absent.
function doorbellDecision(h: ReturnType<typeof harness>, attempt: string): Record<string, unknown> | null {
	try {
		return JSON.parse(fs.readFileSync(path.join(h.dir, "doorbell", `${attempt}.decision.json`), "utf8"));
	} catch {
		return null;
	}
}

// doorbellMessageStart is the message_start event pi emits for the custom
// doorbell entry: a machine role carries customType, content, display and
// details at the top level of the message, the shape real pi builds for a
// custom entry (pi core agent-session.ts, sendCustomMessage).
function doorbellMessageStart(attempt: string, generation: string, content = DOORBELL_LINE, role = "custom") {
	return {
		message: { role, customType: "amq-doorbell", content, display: true, details: { attempt, generation } },
	};
}

test("the doorbell is claimed, shown once as the visible custom message, and accepted by its message_start", async () => {
	const h = harness();
	await h.start();
	try {
		doorbell(h, "attempt-1");
		await h.waitFor(() => doorbellDecision(h, "attempt-1") !== null);
		assert.deepEqual(doorbellDecision(h, "attempt-1")?.decision, "claimed");
		assert.deepEqual(h.sent, [], "a claim never types into pi");
		assert.equal(h.customSent.length, 1);
		assert.deepEqual(h.customSent[0].content, {
			customType: "amq-doorbell",
			content: DOORBELL_LINE,
			display: true,
			details: { attempt: "attempt-1", generation: h.generation() },
		});
		assert.deepEqual(h.customSent[0].options, { triggerTurn: true, deliverAs: "followUp" });
		// The acceptance result exists only after the matching message_start.
		assert.equal(fs.existsSync(path.join(h.dir, "doorbell", "attempt-1.result.json")), false);
		await h.emit("message_start", doorbellMessageStart("attempt-1", h.generation()));
		const result = JSON.parse(fs.readFileSync(path.join(h.dir, "doorbell", "attempt-1.result.json"), "utf8"));
		assert.equal(result.accepted, true);
		assert.equal(result.attempt, "attempt-1");
		// One start: a second message_start publishes nothing new.
		await h.emit("message_start", doorbellMessageStart("attempt-1", h.generation()));
	} finally {
		await h.done();
	}
});

test("an accepted doorbell attests its turn running until the turn ends", async () => {
	const h = harness();
	await h.start();
	try {
		doorbell(h, "attempt-turn");
		await h.waitFor(() => doorbellDecision(h, "attempt-turn") !== null);
		assert.equal(doorbellDecision(h, "attempt-turn")?.decision, "claimed");
		await h.emit("message_start", doorbellMessageStart("attempt-turn", h.generation()));
		const turnFile = path.join(h.dir, "doorbell", "attempt-turn.turn.json");
		const running = JSON.parse(fs.readFileSync(turnFile, "utf8"));
		assert.equal(running.phase, "running");
		assert.equal(running.attempt, "attempt-turn");
		assert.equal(running.generation, h.generation());
		assert.ok(running.updatedAt.length > 0);
		await h.emit("turn_end", {});
		// pi emits turn_end per model/tool iteration: a tool call inside the
		// doorbell's work refreshes the attestation and never ends it. The
		// run's settle is the real end.
		await h.emit("turn_end", {});
		const stillRunning = JSON.parse(fs.readFileSync(turnFile, "utf8"));
		assert.equal(stillRunning.phase, "running");
		assert.ok(Date.parse(stillRunning.updatedAt) >= Date.parse(running.updatedAt));
		await h.emit("agent_settled", { aborted: false });
		const ended = JSON.parse(fs.readFileSync(turnFile, "utf8"));
		assert.equal(ended.phase, "ended");
		// A later user turn runs with no attestation of its own.
		await h.emit("turn_end", {});
		assert.equal(JSON.parse(fs.readFileSync(turnFile, "utf8")).phase, "ended");
	} finally {
		await h.done();
	}
});

test("a busy seat refuses the doorbell before dispatch and sends nothing", async () => {
	const h = harness();
	await h.start();
	try {
		h.state.idle = false;
		doorbell(h, "attempt-2");
		await h.waitFor(() => doorbellDecision(h, "attempt-1") !== null);
		assert.equal(doorbellDecision(h, "attempt-2")?.decision, "refused");
		assert.ok(String(doorbellDecision(h, "attempt-2")?.reason).length > 0);
		assert.deepEqual(h.customSent, []);
		assert.deepEqual(h.sent, []);
	} finally {
		await h.done();
	}
});

test("the wake's withdrawn decision ends the attempt; the bridge does nothing", async () => {
	const h = harness();
	await h.start();
	try {
		fs.mkdirSync(path.join(h.dir, "doorbell"), { recursive: true });
		fs.writeFileSync(path.join(h.dir, "doorbell", "attempt-3.decision.json"), JSON.stringify({ protocol: "amq:pi-bridge:v1", attempt: "attempt-3", decision: "withdrawn", generation: "other", pid: 1, at: new Date().toISOString() }));
		doorbell(h, "attempt-3");
		await new Promise((r) => setTimeout(r, 400));
		assert.deepEqual(h.customSent, []);
		assert.equal(fs.existsSync(path.join(h.dir, "doorbell", "attempt-3.result.json")), false);
	} finally {
		await h.done();
	}
});

test("a doorbell addressed to an earlier generation is never served", async () => {
	const h = harness();
	await h.start();
	try {
		doorbell(h, "attempt-4", { generation: "00000000-0000-4000-8000-000000000000" });
		await new Promise((r) => setTimeout(r, 400));
		assert.equal(doorbellDecision(h, "attempt-4"), null);
		assert.deepEqual(h.customSent, []);
	} finally {
		await h.done();
	}
});

test("a doorbell addressed to another pi process is never served", async () => {
	const h = harness();
	await h.start();
	try {
		doorbell(h, "attempt-6", { pid: process.pid + 1 });
		await new Promise((r) => setTimeout(r, 400));
		assert.equal(doorbellDecision(h, "attempt-6"), null);
		assert.deepEqual(h.customSent, []);
	} finally {
		await h.done();
	}
});

test("a send that throws after the claim keeps the claim and publishes a failed result", async () => {
	const h = harness();
	await h.start();
	try {
		h.state.throwSend = true;
		doorbell(h, "attempt-7");
		await h.waitFor(() => fs.existsSync(path.join(h.dir, "doorbell", "attempt-7.result.json")));
		// D4: a decision is never deleted or rewritten; the claim stays.
		assert.equal(doorbellDecision(h, "attempt-7")?.decision, "claimed");
		const result = JSON.parse(fs.readFileSync(path.join(h.dir, "doorbell", "attempt-7.result.json"), "utf8"));
		assert.equal(result.accepted, false);
		assert.ok(String(result.reason).includes("sendMessage"));
		assert.deepEqual(h.customSent, []);
	} finally {
		await h.done();
	}
});

test("a mismatched doorbell message_start is not acceptance", async () => {
	const h = harness();
	await h.start();
	try {
		doorbell(h, "attempt-5");
		await h.waitFor(() => h.customSent.length === 1);
		await h.emit("message_start", doorbellMessageStart("attempt-5", h.generation(), DOORBELL_LINE, "user"));
		await h.emit("message_start", doorbellMessageStart("attempt-5", h.generation(), "some other line"));
		await h.emit("message_start", doorbellMessageStart("attempt-9", h.generation()));
		assert.equal(fs.existsSync(path.join(h.dir, "doorbell", "attempt-5.result.json")), false);
		// The exact match still accepts afterwards.
		await h.emit("message_start", doorbellMessageStart("attempt-5", h.generation()));
		const result = JSON.parse(fs.readFileSync(path.join(h.dir, "doorbell", "attempt-5.result.json"), "utf8"));
		assert.equal(result.accepted, true);
	} finally {
		await h.done();
	}
});
