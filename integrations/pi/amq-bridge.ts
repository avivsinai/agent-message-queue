// AMQ pi bridge: makes a live pi session reachable by amq-remote target kind
// `pi`. The wire contract is docs/pi-bridge-protocol.md; this file is the
// extension side of it. The adapter writes requests/; this extension writes
// receipts/, events/, and bridge.liveness.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as path from "node:path";

export const PROTOCOL = "amq:pi-bridge:v1";
// BRIDGE_REVISION names the implementation rules the adapter relies on
// (docs/pi-bridge-protocol.md, bridge revision). The adapter sends no new
// requests to a bridge that publishes a lower revision or none.
export const BRIDGE_REVISION = 2;

const HEARTBEAT_MS = 2000; // the adapter treats a liveness file older than 5 s as stale
const POLL_MS = 200; // the adapter waits 2 s for a receipt after it publishes a request
const START_GRACE_MS = 5000; // an idle pi that has not started a follow-up by then dropped it
const MAX_EVENT_TEXT = 256 * 1024;
const REQUEST_FILE = /^(amqr1_[a-z2-7]{16,472})\.json$/;
const HANDLE = /^[a-z0-9_][a-z0-9_-]*$/;
const TERMINAL = new Set(["completed", "failed", "cancelled", "refused", "uncertain"]);

type EventFields = { event: string; text?: string; error?: string; reason?: string };

type Bridge = {
	dir: string;
	ctx: ExtensionContext;
	generation: string;
	surface: string;
	timers: ReturnType<typeof setInterval>[];
	handled: Set<string>;
	// refusals holds a refusal decided for a ref whose append failed. The
	// ref is never examined again; only this same refusal is retried.
	refusals: Map<string, EventFields>;
	inflight: Inflight | null;
	orphansPending: boolean;
};

type Inflight = {
	ref: string;
	text: string;
	deliveredAt: number;
	started: boolean;
	lastText: string;
	lastError: string;
	outcome: string;
	// terminal is the decided outcome. The request stays in flight until
	// that event is durably appended, so a failed write is retried.
	terminal: EventFields | null;
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

	// active is the in-flight request whose outcome is still open.
	const active = (): Inflight | null => {
		const f = bridge?.inflight;
		return f && !f.terminal ? f : null;
	};

	// Output and outcome belong to the request only after its own user
	// message starts. The buffers reset at that boundary, so a local turn
	// that ran before it never becomes the remote result. The start of any
	// later user message ends that ownership: the request completes with
	// the assistant text it already has, or is uncertain without it.
	pi.on("message_start", (event) => {
		const b = bridge;
		const f = active();
		if (!b || !f) return;
		const msg = event.message as { role?: string; content?: unknown };
		if (msg.role !== "user") return;
		if (f.started) {
			if (f.lastText && !f.lastError) {
				settle(b, { event: "completed", text: f.lastText });
			} else {
				settle(b, { event: "uncertain", error: "another user message started before the request produced its answer; the outcome is unknown" });
			}
			return;
		}
		if (textOf(msg.content) === f.text) {
			f.started = true;
			f.lastText = "";
			f.lastError = "";
			f.outcome = "";
			appendEvent(b.dir, f.ref, { event: "started" });
		}
	});

	pi.on("message_end", (event) => {
		const f = active();
		if (f?.started) noteAssistant(f, event.message);
	});

	pi.on("agent_end", (event) => {
		const f = active();
		if (!f?.started) return;
		const last = [...event.messages].reverse().find((m) => (m as { role?: string }).role === "assistant");
		if (last) noteAssistant(f, last);
	});

	pi.on("agent_before_settle", (event) => {
		const f = active();
		if (f?.started) f.outcome = event.outcome;
	});

	pi.on("agent_settled", () => {
		const b = bridge;
		const f = active();
		// A settle before the request started belongs to other work. It is
		// never the request's outcome; the start watchdog in scan closes a
		// request that never starts.
		if (!b || !f || !f.started) return;
		if (f.outcome === "aborted") {
			settle(b, { event: "cancelled" });
		} else if (f.outcome === "error") {
			settle(b, { event: "failed", error: f.lastError || "pi run ended with an error" });
		} else {
			settle(b, { event: "completed", text: f.lastText });
		}
		if (!b.inflight) scan(pi, b);
	});
}

// settle records the in-flight request's terminal outcome and appends it.
// Only a durable append ends the request; otherwise scan retries it.
function settle(b: Bridge, fields: EventFields): void {
	const f = b.inflight;
	if (!f) return;
	f.terminal = fields;
	if (appendEvent(b.dir, f.ref, fields)) b.inflight = null;
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
		ctx,
		generation: randomUUID(),
		surface: ctx.mode,
		timers: [],
		handled: new Set(),
		refusals: new Map(),
		inflight: null,
		orphansPending: false,
	};
	writeLiveness(b, true);
	b.orphansPending = !closeOrphanedReceipts(b);
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
	const f = b.inflight;
	if (f) {
		// A decided outcome is written as decided. Otherwise a started run
		// ends with the session; a request that never started has no
		// native outcome. A failed append leaves the receipt for the next
		// runtime's orphan recovery.
		const fields: EventFields =
			f.terminal ??
			(f.started
				? { event: "failed", error: `pi session_shutdown (${reason}) before the request settled` }
				: { event: "uncertain", error: `pi session_shutdown (${reason}) before the request started; the outcome is unknown` });
		appendEvent(b.dir, f.ref, fields);
		b.inflight = null;
	}
	writeLiveness(b, false);
}

// scan examines every unhandled request in arrival order. Refusals are
// written at once; at most one request is delivered and in flight, and only
// into an idle pi. A request that arrives while pi or the bridge is busy is
// refused busy, never queued.
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
		if (exists(path.join(b.dir, "receipts", `${ref}.json`)) || hasTerminal(b.dir, ref)) {
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

	if (b.orphansPending) b.orphansPending = !closeOrphanedReceipts(b);

	const f = b.inflight;
	if (f?.terminal) {
		settle(b, f.terminal); // retry a terminal append that failed
	} else if (f && !f.started && Date.now() - f.deliveredAt > START_GRACE_MS && b.ctx.isIdle() && !b.ctx.hasPendingMessages()) {
		// pi reports a failed sendUserMessage out of band. An idle session
		// with nothing queued that never started the follow-up most likely
		// dropped it, but nothing proves that it did not run.
		settle(b, { event: "uncertain", error: "pi is idle and never started the follow-up; the outcome is unknown" });
	}

	for (const { ref } of pending) {
		const decided = b.refusals.get(ref);
		if (decided) {
			if (appendEvent(b.dir, ref, decided)) {
				b.refusals.delete(ref);
				b.handled.add(ref);
			}
			continue;
		}
		const verdict = examine(b, ref);
		let { refuse, error } = verdict;
		// v1 refuses a busy target (the remote-control ADR). The check sits
		// at the admission boundary, before the receipt and the hand-off.
		if (!refuse && (b.inflight || !b.ctx.isIdle() || b.ctx.hasPendingMessages())) {
			refuse = "busy";
			error = b.inflight ? "another remote request is in flight" : "pi is busy with other work";
		}
		if (refuse) {
			// The refusal is final for the ref. A failed append keeps it,
			// and the next poll appends the same refusal again.
			const fields: EventFields = { event: "refused", reason: refuse, error };
			if (appendEvent(b.dir, ref, fields)) b.handled.add(ref);
			else b.refusals.set(ref, fields);
			continue;
		}
		const claim = claimReceipt(b, ref);
		if (claim === "error") continue; // retried on the next poll
		b.handled.add(ref);
		if (claim === "taken") continue; // another bridge process already claimed it
		b.inflight = {
			ref,
			text: verdict.text,
			deliveredAt: Date.now(),
			started: false,
			lastText: "",
			lastError: "",
			outcome: "",
			terminal: null,
		};
		try {
			pi.sendUserMessage(verdict.text, { deliverAs: "followUp" });
		} catch (err) {
			settle(b, { event: "failed", error: `sendUserMessage: ${String(err)}` });
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
	// The hint names the session generation the request addresses. An
	// absent or empty hint addresses none, so a request retained across
	// session_start never runs in a session it was not sent to.
	if (hint !== b.generation) return refuse("generation", "epoch_hint does not match the live session generation");
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
		bridge_revision: BRIDGE_REVISION,
	});
	try {
		const tmp = writeTemp(b.dir, body);
		fs.renameSync(tmp, path.join(b.dir, "bridge.liveness"));
	} catch {
		// The next heartbeat retries; a missed beat reads as stale, never live.
	}
}

// closeOrphanedReceipts closes receipts that an earlier bridge process or
// generation left without a terminal event, so the adapter does not report
// them running forever. The outcome of such a request is unknown, so the
// event is `uncertain`. It returns false when an append failed; scan then
// runs it again (it is idempotent).
function closeOrphanedReceipts(b: Bridge): boolean {
	let names: string[];
	try {
		names = fs.readdirSync(path.join(b.dir, "receipts"));
	} catch {
		return false;
	}
	let done = true;
	for (const name of names) {
		const m = REQUEST_FILE.exec(name);
		if (!m) continue;
		const ref = m[1];
		try {
			const rc = JSON.parse(fs.readFileSync(path.join(b.dir, "receipts", name), "utf8"));
			if (rc.session_generation === b.generation || hasTerminal(b.dir, ref)) continue;
			const ok = appendEvent(b.dir, ref, {
				event: "uncertain",
				error: "the pi bridge restarted before the request settled; the outcome is unknown",
			});
			if (!ok) done = false;
		} catch {
			// An unreadable receipt is not recovered yet; scan reads it again.
			done = false;
		}
	}
	return done;
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

// appendEvent appends one fsynced JSON line to events/<ref>.jsonl and reports
// whether the line is durable. The extension is the single writer of the
// stream. A trailing fragment without a newline (a crash or a failed write
// mid-append) is closed with a newline before the new line, so the new line
// always parses; the fragment stays a line that readers skip. Bytes are only
// ever appended, never truncated.
function appendEvent(dir: string, ref: string, fields: EventFields): boolean {
	const ev: Record<string, string> = { protocol: PROTOCOL, ref, event: fields.event };
	if (fields.text) ev.text = truncateUtf8(fields.text, MAX_EVENT_TEXT);
	if (fields.error) ev.error = truncateUtf8(fields.error, MAX_EVENT_TEXT);
	if (fields.reason) ev.reason = fields.reason;
	ev.at = new Date().toISOString();
	const file = path.join(dir, "events", `${ref}.jsonl`);
	let fd: number | undefined;
	try {
		fd = fs.openSync(file, "a+", 0o600);
		const size = fs.fstatSync(fd).size;
		const last = Buffer.alloc(1);
		if (size > 0 && fs.readSync(fd, last, 0, 1, size - 1) === 1 && last[0] !== 0x0a) writeAll(fd, "\n");
		writeAll(fd, `${JSON.stringify(ev)}\n`);
		fs.fsyncSync(fd);
		return true;
	} catch {
		return false;
	} finally {
		if (fd !== undefined) {
			try {
				fs.closeSync(fd);
			} catch {
				// The line was fsynced before close.
			}
		}
	}
}

function writeAll(fd: number, text: string): void {
	const buf = Buffer.from(text, "utf8");
	if (fs.writeSync(fd, buf) !== buf.length) throw new Error("short write");
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
