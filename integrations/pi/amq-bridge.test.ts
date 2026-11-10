import assert from "node:assert/strict";
import * as fs from "node:fs";
import nodeFs from "node:fs";
import { syncBuiltinESMExports } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import amqPiBridge, { type AmqBridgeOptions, type AmqOutcome, type AmqRequest, registerAmqBridge } from "./amq-bridge.ts";

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

// harness runs the extension against a fake ExtensionAPI and a real bridge
// directory under a temporary AM_ROOT. host, when given, embeds the bridge
// as a host application does, with no AM_ROOT or AM_ME set. shared, when
// given, is another harness's root, which that harness removes.
function harness(host?: (root: string) => AmqBridgeOptions, shared?: string) {
	const root = shared ?? fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-bridge-"));
	process.env.AM_ROOT = root;
	process.env.AM_ME = "pi-seat";
	if (host) {
		delete process.env.AM_ROOT;
		delete process.env.AM_ME;
	}
	const handlers = new Map<string, Handler[]>();
	const sent: { content: unknown; options: unknown }[] = [];
	const state = { idle: true, pending: false, idleChecks: 0, aborts: 0 };
	const notices: string[] = [];
	const pi = {
		on(name: string, h: Handler) {
			handlers.set(name, [...(handlers.get(name) ?? []), h]);
			return () => {};
		},
		sendUserMessage(content: unknown, options: unknown) {
			sent.push({ content, options });
		},
	};
	const ctx = {
		mode: "tui",
		hasUI: true,
		ui: {
			notify(message: string) {
				notices.push(message);
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
	const bridge = host ? registerAmqBridge(pi as never, host(root)) : undefined;
	if (!host) amqPiBridge(pi as never);
	const emit = async (name: string, event: object = {}) => {
		for (const h of handlers.get(name) ?? []) await h({ type: name, ...event }, ctx);
	};
	return {
		root,
		dir,
		bridge,
		sent,
		state,
		notices,
		emit,
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
		assert.equal(live.bridge_revision, 5);

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
