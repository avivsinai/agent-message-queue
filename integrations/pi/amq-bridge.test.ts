import assert from "node:assert/strict";
import * as fs from "node:fs";
import nodeFs from "node:fs";
import { syncBuiltinESMExports } from "node:module";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import amqPiBridge from "./amq-bridge.ts";

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

// harness runs the extension against a fake ExtensionAPI and a real bridge
// directory under a temporary AM_ROOT.
function harness() {
	const root = fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-bridge-"));
	process.env.AM_ROOT = root;
	process.env.AM_ME = "pi-seat";
	const handlers = new Map<string, Handler[]>();
	const sent: { content: unknown; options: unknown }[] = [];
	const state = { idle: true, pending: false, idleChecks: 0 };
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
		ui: { notify() {} },
		isIdle: () => {
			state.idleChecks++;
			return state.idle;
		},
		hasPendingMessages: () => state.pending,
	};
	const dir = path.join(root, "agents", "pi-seat", "extensions", "pi-bridge");
	amqPiBridge(pi as never);
	const emit = async (name: string, event: object = {}) => {
		for (const h of handlers.get(name) ?? []) await h({ type: name, ...event }, ctx);
	};
	return {
		dir,
		sent,
		state,
		emit,
		start: () => emit("session_start", { reason: "startup" }),
		generation: () => JSON.parse(fs.readFileSync(path.join(dir, "bridge.liveness"), "utf8")).session_generation as string,
		publish(ref: string, text: string, epochHint: string, fields: Record<string, unknown> = { bridge_revision: 2 }) {
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
			fs.rmSync(root, { recursive: true, force: true });
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

test("a request file becomes a pi follow-up with a receipt and a terminal event", async () => {
	const h = harness();
	const produced = (rel: string) => normalize(fs.readFileSync(path.join(h.dir, rel), "utf8"));
	const golden = (rel: string) => fs.readFileSync(path.join(GOLDEN, rel), "utf8");

	// A receipt an earlier generation left without a terminal event.
	fs.mkdirSync(path.join(h.dir, "receipts"), { recursive: true });
	fs.copyFileSync(path.join(GOLDEN, "receipts", `${ORPHAN}.json`), path.join(h.dir, "receipts", `${ORPHAN}.json`));

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
	fs.writeFileSync(path.join(h.dir, "events", `${ORPHAN}.jsonl`), fragment);

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

// Pro review of #923, 2026-09-29, #2: the extension accepted a request from
// an adapter that predates the revision fence, so an old amq-remote could
// send work into a replacement session.
test("a request without bridge_revision is refused without a receipt", async () => {
	const h = harness();
	await h.start();
	try {
		h.publish(REF, "remote task", h.generation(), {});
		await h.waitFor(() => h.events(REF).length > 0);
		assert.deepEqual(h.sent, []);
		assert.equal(h.hasReceipt(REF), false);
		assert.deepEqual(
			h.events(REF).map((e) => [e.event, e.reason]),
			[["refused", "revision"]],
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
