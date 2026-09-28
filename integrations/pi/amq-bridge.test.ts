import assert from "node:assert/strict";
import * as fs from "node:fs";
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

test("a request file becomes a pi follow-up with a receipt and a terminal event", async () => {
	const root = fs.mkdtempSync(path.join(os.tmpdir(), "amq-pi-bridge-"));
	process.env.AM_ROOT = root;
	process.env.AM_ME = "pi-seat";
	const handlers = new Map<string, Handler[]>();
	const sent: { content: unknown; options: unknown }[] = [];
	const pi = {
		on(name: string, h: Handler) {
			handlers.set(name, [...(handlers.get(name) ?? []), h]);
			return () => {};
		},
		sendUserMessage(content: unknown, options: unknown) {
			sent.push({ content, options });
		},
	};
	const ctx = { mode: "tui", hasUI: true, ui: { notify() {} }, isIdle: () => true, hasPendingMessages: () => false };
	const emit = async (name: string, event: object) => {
		for (const h of handlers.get(name) ?? []) await h({ type: name, ...event }, ctx);
	};
	const dir = path.join(root, "agents", "pi-seat", "extensions", "pi-bridge");
	const produced = (rel: string) => normalize(fs.readFileSync(path.join(dir, rel), "utf8"));
	const golden = (rel: string) => fs.readFileSync(path.join(GOLDEN, rel), "utf8");

	// A receipt an earlier generation left without a terminal event.
	fs.mkdirSync(path.join(dir, "receipts"), { recursive: true });
	fs.copyFileSync(path.join(GOLDEN, "receipts", `${ORPHAN}.json`), path.join(dir, "receipts", `${ORPHAN}.json`));

	amqPiBridge(pi as never);
	await emit("session_start", { reason: "startup" });
	try {
		const request = {
			ref: REF,
			text: "summarize the diff",
			deliver_as: "followUp",
			not_after: new Date(Date.now() + 60_000).toISOString(),
			created_at: new Date().toISOString(),
		};
		const tmp = path.join(dir, "requests", ".publish-test");
		fs.writeFileSync(tmp, JSON.stringify(request));
		fs.renameSync(tmp, path.join(dir, "requests", `${REF}.json`));

		const receiptPath = path.join(dir, "receipts", `${REF}.json`);
		for (let i = 0; i < 100 && !fs.existsSync(receiptPath); i++) await new Promise((r) => setTimeout(r, 20));
		assert.deepEqual(sent, [{ content: "summarize the diff", options: { deliverAs: "followUp" } }]);

		const receipt = JSON.parse(fs.readFileSync(receiptPath, "utf8"));
		const liveness = JSON.parse(fs.readFileSync(path.join(dir, "bridge.liveness"), "utf8"));
		assert.equal(receipt.pid, process.pid);
		assert.ok(receipt.session_generation);
		assert.equal(receipt.session_generation, liveness.session_generation);

		await emit("message_start", { message: { role: "user", content: [{ type: "text", text: "summarize the diff" }] } });
		await emit("message_end", { message: { role: "assistant", content: [{ type: "text", text: "two files changed" }] } });
		await emit("agent_before_settle", { outcome: "completed" });
		await emit("agent_settled", {});

		for (const rel of [`receipts/${REF}.json`, "bridge.liveness", `events/${REF}.jsonl`, `events/${ORPHAN}.jsonl`]) {
			assert.equal(produced(rel), golden(rel), rel);
		}
	} finally {
		await emit("session_shutdown", { reason: "quit" });
		fs.rmSync(root, { recursive: true, force: true });
	}
});
