// AMQ pi bridge: makes a live pi session reachable by amq-remote target kind
// `pi`. The wire contract is docs/pi-bridge-protocol.md; this file is the
// extension side of it. The adapter writes requests/; this extension writes
// receipts/, events/, and bridge.liveness.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";

export const PROTOCOL = "amq:pi-bridge:v1";

const HEARTBEAT_MS = 2000; // the adapter treats a liveness file older than 5 s as stale
const POLL_MS = 200; // the adapter waits 2 s for a receipt after it publishes a request
const MAX_EVENT_TEXT = 256 * 1024;
const REQUEST_FILE = /^(amqr1_[a-z2-7]{16,472})\.json$/;
const HANDLE = /^[a-z0-9_][a-z0-9_-]*$/;
const TERMINAL = new Set(["completed", "failed", "cancelled", "refused"]);

type EventFields = { event: string; text?: string; error?: string; reason?: string };

type Bridge = {
	dir: string;
	generation: string;
	surface: string;
	timers: ReturnType<typeof setInterval>[];
	handled: Set<string>;
	inflight: Inflight | null;
};

type Inflight = {
	ref: string;
	text: string;
	started: boolean;
	lastText: string;
	lastError: string;
	outcome: string;
};

let inactiveNoticeShown = false;

export default function amqPiBridge(pi: ExtensionAPI): void {
	let bridge: Bridge | null = null;

	pi.on("session_start", (_event, ctx) => {
		stop(bridge, "restart");
		bridge = start(pi, ctx);
		if (bridge) scan(pi, bridge);
	});

	pi.on("session_shutdown", (event) => {
		stop(bridge, event.reason);
		bridge = null;
	});

	pi.on("message_start", (event) => {
		const f = bridge?.inflight;
		if (!bridge || !f || f.started) return;
		const msg = event.message as { role?: string; content?: unknown };
		if (msg.role === "user" && textOf(msg.content) === f.text) {
			f.started = true;
			appendEvent(bridge.dir, f.ref, { event: "started" });
		}
	});

	pi.on("message_end", (event) => {
		const f = bridge?.inflight;
		if (f) noteAssistant(f, event.message);
	});

	pi.on("agent_end", (event) => {
		const f = bridge?.inflight;
		if (!f) return;
		const last = [...event.messages].reverse().find((m) => (m as { role?: string }).role === "assistant");
		if (last) noteAssistant(f, last);
	});

	pi.on("agent_before_settle", (event) => {
		const f = bridge?.inflight;
		if (f) f.outcome = event.outcome;
	});

	pi.on("agent_settled", (_event, ctx) => {
		const b = bridge;
		const f = b?.inflight;
		if (!b || !f) return;
		// A settle before the follow-up was seen belongs to earlier work unless
		// pi no longer holds the follow-up in its queue.
		if (!f.started && ctx.hasPendingMessages()) return;
		if (f.outcome === "aborted") {
			appendEvent(b.dir, f.ref, { event: "cancelled" });
		} else if (f.outcome === "error") {
			appendEvent(b.dir, f.ref, { event: "failed", error: f.lastError || "pi run ended with an error" });
		} else {
			appendEvent(b.dir, f.ref, { event: "completed", text: f.lastText });
		}
		b.inflight = null;
		scan(pi, b);
	});
}

function start(pi: ExtensionAPI, ctx: ExtensionContext): Bridge | null {
	const root = process.env.AM_ROOT ?? "";
	const handle = process.env.AM_ME ?? "";
	if (!root || !HANDLE.test(handle)) {
		if (!inactiveNoticeShown) {
			inactiveNoticeShown = true;
			notify(ctx, "amq-bridge inactive: AM_ROOT and AM_ME are not set to a valid AMQ identity. Run pi under `amq coop exec` to make it reachable by amq-remote.");
		}
		return null;
	}
	const dir = path.join(root, "agents", handle, "extensions", "pi-bridge");
	try {
		for (const sub of ["requests", "receipts", "events"]) {
			fs.mkdirSync(path.join(dir, sub), { recursive: true, mode: 0o700 });
		}
	} catch (err) {
		notify(ctx, `amq-bridge inactive: cannot create ${dir}: ${String(err)}`);
		return null;
	}
	const b: Bridge = {
		dir,
		generation: randomUUID(),
		surface: ctx.mode,
		timers: [],
		handled: new Set(),
		inflight: null,
	};
	writeLiveness(b, true);
	failOrphanedReceipts(b);
	const beat = setInterval(() => writeLiveness(b, true), HEARTBEAT_MS);
	const poll = setInterval(() => {
		try {
			scan(pi, b);
		} catch {
			// A transient filesystem error is retried on the next poll.
		}
	}, POLL_MS);
	beat.unref();
	poll.unref();
	b.timers.push(beat, poll);
	return b;
}

function stop(b: Bridge | null, reason: string): void {
	if (!b) return;
	for (const t of b.timers.splice(0)) clearInterval(t);
	if (b.inflight) {
		appendEvent(b.dir, b.inflight.ref, {
			event: "failed",
			error: `pi session_shutdown (${reason}) before the request settled`,
		});
		b.inflight = null;
	}
	writeLiveness(b, false);
}

// scan examines every unhandled request in arrival order. Refusals are
// written at once; at most one request is delivered and in flight.
function scan(pi: ExtensionAPI, b: Bridge): void {
	const reqDir = path.join(b.dir, "requests");
	let names: string[];
	try {
		names = fs.readdirSync(reqDir);
	} catch {
		return;
	}
	const pending: { ref: string; mtime: number }[] = [];
	for (const name of names) {
		const m = REQUEST_FILE.exec(name);
		if (!m || b.handled.has(m[1])) continue;
		const ref = m[1];
		if (exists(path.join(b.dir, "receipts", `${ref}.json`)) || exists(path.join(b.dir, "events", `${ref}.jsonl`))) {
			b.handled.add(ref);
			continue;
		}
		try {
			pending.push({ ref, mtime: fs.statSync(path.join(reqDir, name)).mtimeMs });
		} catch {
			// Vanished between readdir and stat: nothing to answer.
		}
	}
	pending.sort((x, y) => x.mtime - y.mtime || (x.ref < y.ref ? -1 : x.ref > y.ref ? 1 : 0));

	for (const { ref } of pending) {
		const verdict = examine(b, ref);
		if (verdict.refuse) {
			appendEvent(b.dir, ref, { event: "refused", reason: verdict.refuse, error: verdict.error });
			b.handled.add(ref);
			continue;
		}
		if (b.inflight) continue; // stays queued until the in-flight request settles
		const claim = claimReceipt(b, ref);
		if (claim === "error") continue; // retried on the next poll
		b.handled.add(ref);
		if (claim === "taken") continue; // another bridge process already claimed it
		b.inflight = { ref, text: verdict.text, started: false, lastText: "", lastError: "", outcome: "" };
		try {
			pi.sendUserMessage(verdict.text, { deliverAs: "followUp" });
		} catch (err) {
			appendEvent(b.dir, ref, { event: "failed", error: `sendUserMessage: ${String(err)}` });
			b.inflight = null;
		}
	}
}

type Verdict = { refuse: string; error: string; text: string };

function examine(b: Bridge, ref: string): Verdict {
	const refuse = (reason: string, error: string): Verdict => ({ refuse: reason, error, text: "" });
	let req: Record<string, unknown>;
	try {
		req = JSON.parse(fs.readFileSync(path.join(b.dir, "requests", `${ref}.json`), "utf8"));
	} catch (err) {
		return refuse("invalid", `unreadable request: ${String(err)}`);
	}
	if (req === null || typeof req !== "object") return refuse("invalid", "request is not a JSON object");
	if (req.ref !== ref) return refuse("invalid", "request ref does not match its file name");
	if (typeof req.text !== "string" || req.text.trim() === "") return refuse("invalid", "request text is empty");
	if (req.deliver_as !== "followUp") return refuse("unsupported", `deliver_as ${JSON.stringify(req.deliver_as)} is not followUp`);
	const notAfter = typeof req.not_after === "string" ? Date.parse(req.not_after) : Number.NaN;
	if (Number.isNaN(notAfter)) return refuse("invalid", "not_after is not an RFC 3339 time");
	if (Date.now() > notAfter) return refuse("expired", "not_after passed before delivery");
	const hint = req.epoch_hint ?? "";
	if (typeof hint !== "string") return refuse("invalid", "epoch_hint is not a string");
	if (hint !== "" && hint !== b.generation) return refuse("generation", "epoch_hint does not match the live session generation");
	return { refuse: "", error: "", text: req.text };
}

// claimReceipt publishes receipts/<ref>.json create-new. The receipt is the
// admission claim, written before the hand-off to pi, so a ref is never
// delivered twice.
function claimReceipt(b: Bridge, ref: string): "claimed" | "taken" | "error" {
	const body = JSON.stringify({
		protocol: PROTOCOL,
		ref,
		session_generation: b.generation,
		delivered_at: new Date().toISOString(),
		pid: process.pid,
	});
	const dir = path.join(b.dir, "receipts");
	let tmp = "";
	try {
		tmp = writeTemp(dir, body);
		fs.linkSync(tmp, path.join(dir, `${ref}.json`));
	} catch (err) {
		return (err as NodeJS.ErrnoException).code === "EEXIST" ? "taken" : "error";
	} finally {
		if (tmp) fs.rmSync(tmp, { force: true });
	}
	syncDir(dir);
	return "claimed";
}

function writeLiveness(b: Bridge, live: boolean): void {
	const body = JSON.stringify({
		protocol: PROTOCOL,
		live,
		at: new Date().toISOString(),
		pid: process.pid,
		surface: b.surface,
		session_generation: b.generation,
	});
	try {
		const tmp = writeTemp(b.dir, body);
		fs.renameSync(tmp, path.join(b.dir, "bridge.liveness"));
	} catch {
		// The next heartbeat retries; a missed beat reads as stale, never live.
	}
}

// failOrphanedReceipts closes receipts that an earlier bridge process or
// generation left without a terminal event, so the adapter does not report
// them running forever.
function failOrphanedReceipts(b: Bridge): void {
	let names: string[];
	try {
		names = fs.readdirSync(path.join(b.dir, "receipts"));
	} catch {
		return;
	}
	for (const name of names) {
		const m = REQUEST_FILE.exec(name);
		if (!m) continue;
		const ref = m[1];
		try {
			const rc = JSON.parse(fs.readFileSync(path.join(b.dir, "receipts", name), "utf8"));
			if (rc.session_generation === b.generation || hasTerminal(b.dir, ref)) continue;
			appendEvent(b.dir, ref, {
				event: "failed",
				error: "the pi bridge restarted before the request settled; the outcome is unknown",
			});
		} catch {
			// An unreadable receipt is left for the adapter to report.
		}
	}
}

function hasTerminal(dir: string, ref: string): boolean {
	let data: string;
	try {
		data = fs.readFileSync(path.join(dir, "events", `${ref}.jsonl`), "utf8");
	} catch {
		return false;
	}
	for (const line of data.split("\n")) {
		try {
			if (TERMINAL.has(JSON.parse(line).event)) return true;
		} catch {
			// A partial line is never terminal evidence.
		}
	}
	return false;
}

// appendEvent appends one fsynced JSON line to events/<ref>.jsonl.
function appendEvent(dir: string, ref: string, fields: EventFields): void {
	const ev: Record<string, string> = { protocol: PROTOCOL, ref, event: fields.event };
	if (fields.text) ev.text = truncateUtf8(fields.text, MAX_EVENT_TEXT);
	if (fields.error) ev.error = truncateUtf8(fields.error, MAX_EVENT_TEXT);
	if (fields.reason) ev.reason = fields.reason;
	ev.at = new Date().toISOString();
	const file = path.join(dir, "events", `${ref}.jsonl`);
	let fd: number | undefined;
	try {
		fd = fs.openSync(file, "a", 0o600);
		fs.writeSync(fd, `${JSON.stringify(ev)}\n`);
		fs.fsyncSync(fd);
	} catch {
		// Without the events directory there is no stream to append to.
	} finally {
		if (fd !== undefined) fs.closeSync(fd);
	}
}

function noteAssistant(f: Inflight, message: unknown): void {
	const msg = message as { role?: string; content?: unknown; errorMessage?: unknown };
	if (msg.role !== "assistant") return;
	const text = textOf(msg.content);
	if (text) f.lastText = text;
	if (typeof msg.errorMessage === "string" && msg.errorMessage) f.lastError = msg.errorMessage;
}

function textOf(content: unknown): string {
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.filter((p) => p && p.type === "text" && typeof p.text === "string")
		.map((p) => p.text as string)
		.join("");
}

function truncateUtf8(s: string, max: number): string {
	const buf = Buffer.from(s, "utf8");
	if (buf.length <= max) return s;
	let end = max;
	while (end > 0 && (buf[end] & 0xc0) === 0x80) end--;
	return buf.subarray(0, end).toString("utf8");
}

function writeTemp(dir: string, body: string): string {
	const tmp = path.join(dir, `.tmp-${process.pid}-${randomUUID()}`);
	const fd = fs.openSync(tmp, "wx", 0o600);
	try {
		fs.writeSync(fd, body);
		fs.fsyncSync(fd);
	} finally {
		fs.closeSync(fd);
	}
	return tmp;
}

function syncDir(dir: string): void {
	try {
		const fd = fs.openSync(dir, "r");
		try {
			fs.fsyncSync(fd);
		} finally {
			fs.closeSync(fd);
		}
	} catch {
		// Directory fsync is best effort on platforms that refuse it.
	}
}

function exists(p: string): boolean {
	try {
		fs.statSync(p);
		return true;
	} catch {
		return false;
	}
}

function notify(ctx: ExtensionContext, message: string): void {
	try {
		ctx.ui.notify(message, "info");
	} catch {
		console.error(message);
	}
}
