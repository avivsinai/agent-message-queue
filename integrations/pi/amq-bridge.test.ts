import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { test } from "node:test";
import amqPiBridge, { PROTOCOL } from "./amq-bridge.ts";

type Handler = (event: unknown, ctx: unknown) => unknown;

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

	amqPiBridge(pi as never);
	await emit("session_start", { reason: "startup" });
	try {
		const dir = path.join(root, "agents", "pi-seat", "extensions", "pi-bridge");
		const ref = "amqr1_naahiabrgizwknbvgy3s2zjyhfrc2mjsmqzs2yjugu3c2nbsgy3dcnbrg42dambq";
		const request = {
			ref,
			text: "summarize the diff",
			deliver_as: "followUp",
			not_after: new Date(Date.now() + 60_000).toISOString(),
			created_at: new Date().toISOString(),
		};
		const tmp = path.join(dir, "requests", ".publish-test");
		fs.writeFileSync(tmp, JSON.stringify(request));
		fs.renameSync(tmp, path.join(dir, "requests", `${ref}.json`));

		const receiptPath = path.join(dir, "receipts", `${ref}.json`);
		for (let i = 0; i < 100 && !fs.existsSync(receiptPath); i++) await new Promise((r) => setTimeout(r, 20));

		assert.deepEqual(sent, [{ content: "summarize the diff", options: { deliverAs: "followUp" } }]);
		const receipt = JSON.parse(fs.readFileSync(receiptPath, "utf8"));
		const liveness = JSON.parse(fs.readFileSync(path.join(dir, "bridge.liveness"), "utf8"));
		assert.equal(receipt.protocol, PROTOCOL);
		assert.equal(liveness.protocol, "amq:pi-bridge:v1");
		assert.equal(liveness.live, true);
		assert.equal(receipt.ref, ref);
		assert.equal(receipt.pid, process.pid);
		assert.ok(receipt.session_generation);
		assert.equal(receipt.session_generation, liveness.session_generation);

		await emit("message_start", { message: { role: "user", content: [{ type: "text", text: "summarize the diff" }] } });
		await emit("message_end", { message: { role: "assistant", content: [{ type: "text", text: "two files changed" }] } });
		await emit("agent_before_settle", { outcome: "completed" });
		await emit("agent_settled", {});

		const events = fs
			.readFileSync(path.join(dir, "events", `${ref}.jsonl`), "utf8")
			.trim()
			.split("\n")
			.map((l) => JSON.parse(l));
		assert.deepEqual(
			events.map((e) => [e.protocol, e.ref, e.event, e.text]),
			[
				[PROTOCOL, ref, "started", undefined],
				[PROTOCOL, ref, "completed", "two files changed"],
			],
		);
	} finally {
		await emit("session_shutdown", { reason: "quit" });
		fs.rmSync(root, { recursive: true, force: true });
	}
});
