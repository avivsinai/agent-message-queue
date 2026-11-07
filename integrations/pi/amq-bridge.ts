// AMQ pi bridge: makes a live pi session reachable by amq-remote target kind
// `pi`. The wire contract is docs/pi-bridge-protocol.md; this file is the
// extension side of it. The adapter writes requests/; this extension writes
// receipts/, events/, and bridge.liveness.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

export const PROTOCOL = "amq:pi-bridge:v1";
// BRIDGE_REVISION names the implementation rules the adapter relies on
// (docs/pi-bridge-protocol.md, bridge revision). The adapter sends no new
// requests to a bridge that publishes a lower revision or none. Revision 4
// is revision 3 plus pi's session id in liveness.
export const BRIDGE_REVISION = 4;
// A request carries the bridge_revision its adapter requires. A request
// without one comes from an adapter that predates the revision fence and is
// refused, so an old adapter cannot send work through this bridge.
const MIN_ADAPTER_REVISION = 3;

const HEARTBEAT_MS = 2000; // the adapter treats a liveness file older than 5 s as stale
const POLL_MS = 200; // the adapter waits 2 s for a receipt after it publishes a request
const START_GRACE_MS = 5000; // an idle pi that has not started a follow-up by then dropped it
const MAX_EVENT_TEXT = 256 * 1024;
// Activity records are a bounded, best-effort view of the whole session for
// the relay's session panel and the session mirror
// (docs/pi-bridge-protocol.md, activity). Text is cut to the relay frame's
// plaintext cap. Past 4 MiB, a starting run replaces the file with the
// records of the run before it, so a reader that has not read that run's end
// still finds it; past 16 MiB the file is truncated at any record.
const MAX_ACTIVITY_TEXT = 48000;
const MAX_ACTIVITY_FILE = 4 * 1024 * 1024;
const MAX_ACTIVITY_FILE_HARD = 16 * 1024 * 1024;
// ACTIVITY_VERSION is the activity record version liveness publishes: 2
// records runs (run, run_start, settled) and the request ref on its user
// record.
const ACTIVITY_VERSION = 2;
const SESSION_ID = /^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,126}[A-Za-z0-9])?$/;
const REQUEST_FILE = /^(amqr1_[a-z2-7]{16,472})\.json$/;
const HANDLE = /^[a-z0-9_][a-z0-9_-]*$/;
const TERMINAL = new Set(["completed", "failed", "cancelled", "refused", "uncertain"]);

type EventFields = { event: string; text?: string; error?: string; reason?: string };

// AmqIdentity is the AMQ participation identity: the queue root and the
// handle pi runs as.
export type AmqIdentity = { root: string; handle: string };

// AmqRequest is a request the bridge admitted and hands to a host's
// delivery. origin is present only when the request carries a valid one
// (docs/pi-bridge-protocol.md, requests); it is display text, never
// authority.
export type AmqRequest = { ref: string; text: string; notAfter: string; origin?: { carrier: string; name: string } };

// AmqOutcome is what a host's delivery reports for the request it was
// handed: started at its real admission, then exactly one terminal outcome.
export type AmqOutcome =
	| { event: "started" }
	| { event: "completed"; text: string }
	| { event: "failed" | "uncertain"; error: string }
	| { event: "cancelled" }
	| { event: "refused"; reason: "expired"; error?: string };

// AmqDelivery replaces the bridge's own hand-off to pi. busy reports that a
// request would have to wait behind other work, so the bridge refuses it
// busy. send runs right after the receipt is claimed; the host reports the
// request's start and outcome through report, and the bridge then does no
// correlation of its own for that request.
export type AmqDelivery = {
	busy(ctx: ExtensionContext): boolean;
	send(ctx: ExtensionContext, request: AmqRequest, report: (outcome: AmqOutcome) => void): void;
};

// AmqBridgeOptions lets a host application embed the bridge. Every option
// is optional, and the defaults are the behaviour of the plain extension.
export type AmqBridgeOptions = {
	// identity resolves root and handle; ctx is undefined before the session
	// starts. Default: AM_ROOT and AM_ME.
	identity?: (ctx: ExtensionContext | undefined) => AmqIdentity | undefined;
	// deliver default: sendUserMessage as a follow-up, correlated by the
	// bridge itself.
	deliver?: AmqDelivery;
	// upgrade is the host's own remedy for an owner whose bridge is too
	// old, published in liveness. Default: none.
	upgrade?: string;
};

type Bridge = {
	dir: string;
	identity: AmqIdentity;
	deliver: AmqDelivery | null;
	upgrade: string;
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
	// orphanRetry holds orphan refs whose closing append failed. Their
	// bytes may be on disk without being durable, so they are appended
	// again until an append succeeds.
	orphanRetry: Set<string>;
	// revisionNoticeShown limits the old-adapter notice to once per runtime.
	revisionNoticeShown: boolean;
	// sessionId is pi's own session id; "" disables activity records.
	sessionId: string;
	// turn is the id of the open turn, "" outside one. turns counts turns.
	turn: string;
	turns: number;
	// seq numbers activity records within this runtime.
	seq: number;
	// run is the id of the open run, from the first agent_start after a
	// settle to the next agent_settled; "" outside one. runs counts runs.
	run: string;
	runs: number;
	// runOutcome is the open run's last agent_before_settle outcome.
	runOutcome: string;
	// runStartAt is the byte offset in the activity file where the last
	// run_start record was written, -1 before the first.
	runStartAt: number;
	// requestUser is the ref of the in-flight request whose own user
	// message started and has not ended yet: the first user message, after
	// the hand-off, whose text is the request's text.
	requestUser: string;
};

type Inflight = {
	ref: string;
	text: string;
	deliveredAt: number;
	started: boolean;
	// tagged is true once the request's own user message started.
	tagged: boolean;
	lastText: string;
	lastError: string;
	// finalText is the text of the request's last assistant message when
	// that message is a final answer: stop reason "stop" and no tool call.
	// "" while the run is mid-tool-use or its last message is not final.
	finalText: string;
	outcome: string;
	// terminal is the decided outcome. The request stays in flight until
	// that event is durably appended, so a failed write is retried.
	terminal: EventFields | null;
};

let inactiveNoticeShown = false;

export default function amqPiBridge(pi: ExtensionAPI): void {
	registerAmqBridge(pi);
}

// registerAmqBridge registers the bridge on pi. A host application passes
// its own identity, delivery, or upgrade remedy; without options this is
// the plain extension.
export function registerAmqBridge(pi: ExtensionAPI, options: AmqBridgeOptions = {}): void {
	let bridge: Bridge | null = null;
	// session holds the identity resolved for the open session, none
	// included; it is null outside a session.
	let session: { identity: AmqIdentity | undefined } | null = null;
	// who is the identity the wake marker and the Buzz dialog use: the
	// session's once it started, else what the identity source gives
	// without a session.
	const who = (): AmqIdentity | undefined => (session ? session.identity : resolveIdentity(options, undefined));

	// The doorbell must never answer a pi prompt. pi counts nested
	// extension prompts itself: ui_prompt_start fires when it starts waiting
	// and ui_prompt_end when it no longer waits. project_trust fires before
	// pi may ask "Trust project folder?"; the session starts only after that
	// question is answered, so session_start closes it. A session boundary
	// also clears the marker, so one left by a crash does not hold the next
	// session.
	// pi queues and awaits each extension's handlers, so this extension can
	// see prompt events in any order, even an end before its start. The
	// marker follows one count of open prompts, which does not depend on
	// order: it exists exactly while the count is above zero.
	let openPrompts = 0;
	// The same prompt is shown in Buzz as a dialog with no options: pi
	// exposes no way to answer a prompt from an extension (the prompt events
	// carry only its kind and title), so the owner answers in the terminal and
	// the dialog closes when the prompt ends.
	pi.on("project_trust", () => {
		openWakeDialog(who(), WAKE_DIALOG_TRUST, "select");
		openRemoteDialog(who(), "select", "Trust project folder?", "");
		return { trusted: "undecided" };
	});
	pi.on("ui_prompt_start", (event) => {
		if (++openPrompts === 1) {
			openWakeDialog(who(), WAKE_DIALOG_PROMPT, event.kind);
			openRemoteDialog(who(), event.kind, (event as { title?: string }).title ?? "", bridge?.sessionId ?? "");
		}
	});
	pi.on("ui_prompt_end", () => {
		if (--openPrompts <= 0) {
			closeWakeDialog(who());
			closeRemoteDialog();
		}
	});

	pi.on("session_start", (_event, ctx) => {
		openPrompts = 0;
		closeWakeDialog(who());
		closeRemoteDialog();
		stop(bridge, "restart");
		session = { identity: resolveIdentity(options, ctx) };
		bridge = session.identity ? start(pi, ctx, options, session.identity) : null;
		if (bridge) scanSafely(pi, bridge);
		else if (!session.identity && !options.identity && !inactiveNoticeShown) {
			// A host that gives no identity for this session keeps the bridge
			// off on purpose; only the plain extension tells the owner how to
			// fix it.
			inactiveNoticeShown = true;
			notify(ctx, "amq-bridge inactive: AM_ROOT and AM_ME are not set to a valid AMQ identity. Run pi under `amq coop exec` to make it reachable by amq-remote.");
		}
	});

	pi.on("session_shutdown", (event) => {
		openPrompts = 0;
		closeWakeDialog(who());
		closeRemoteDialog();
		stop(bridge, event.reason);
		bridge = null;
		session = null;
	});

	// active is the in-flight request whose outcome is still open and that
	// the bridge correlates itself. A host's delivery reports its own
	// request, so the handlers below never decide it.
	const active = (): Inflight | null => {
		const f = bridge?.inflight;
		return f && !f.terminal && !bridge?.deliver ? f : null;
	};

	// The request's own user message is the first user message whose text
	// is the request's text; its activity record carries the request's ref,
	// whichever delivery sent it. Without a host's delivery, output and
	// outcome belong to the request only after that message starts. The
	// buffers reset at that boundary, so a local turn that ran before it
	// never becomes the remote result. The start of any later user message
	// ends that ownership: the request completes only when its last
	// assistant message is a final answer, otherwise it is uncertain (a
	// tool-use preamble is never an answer).
	pi.on("message_start", (event) => {
		const b = bridge;
		const f = b?.inflight;
		if (!b || !f || f.terminal) return;
		const msg = event.message as { role?: string; content?: unknown };
		if (msg.role !== "user") return;
		const own = !f.tagged && textOf(msg.content) === f.text;
		if (own) {
			f.tagged = true;
			b.requestUser = f.ref;
		}
		if (b.deliver) return;
		if (f.started) {
			if (f.finalText && !f.lastError) {
				settle(b, { event: "completed", text: f.finalText });
			} else {
				settle(b, { event: "uncertain", error: "another user message started before the request produced its answer; the outcome is unknown" });
			}
			return;
		}
		if (own) {
			f.started = true;
			f.lastText = "";
			f.lastError = "";
			f.finalText = "";
			f.outcome = "";
			appendEvent(b.dir, f.ref, { event: "started" });
		}
	});

	pi.on("message_end", (event) => {
		const f = active();
		if (f?.started) noteAssistant(f, event.message);
	});

	activityHooks(pi, () => bridge);

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
		if (!b.inflight) scanSafely(pi, b);
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

// resolveIdentity is the identity from the configured source, or undefined
// when it gives none that is valid: an absolute root and a valid handle. The
// default source is the AMQ participation contract, AM_ROOT and AM_ME, set
// by `amq coop exec`.
function resolveIdentity(options: AmqBridgeOptions, ctx: ExtensionContext | undefined): AmqIdentity | undefined {
	let id: AmqIdentity | undefined;
	try {
		id = options.identity ? options.identity(ctx) : { root: process.env.AM_ROOT ?? "", handle: process.env.AM_ME ?? "" };
	} catch {
		return undefined;
	}
	return id && typeof id.root === "string" && path.isAbsolute(id.root) && typeof id.handle === "string" && HANDLE.test(id.handle) ? { root: id.root, handle: id.handle } : undefined;
}

function start(pi: ExtensionAPI, ctx: ExtensionContext, options: AmqBridgeOptions, identity: AmqIdentity): Bridge | null {
	const dir = path.join(identity.root, "agents", identity.handle, "extensions", "pi-bridge");
	try {
		for (const sub of ["requests", "receipts", "events"]) {
			fs.mkdirSync(path.join(dir, sub), { recursive: true, mode: 0o700 });
		}
	} catch (err) {
		notify(ctx, `amq-bridge inactive: cannot create ${dir}: ${String(err)}`);
		return null;
	}
	try {
		fs.mkdirSync(path.join(dir, "activity"), { recursive: true, mode: 0o700 });
	} catch {
		// Activity is optional: without its directory no activity is
		// recorded, and requests run as before.
	}
	const b: Bridge = {
		dir,
		identity,
		deliver: options.deliver ?? null,
		upgrade: options.upgrade ?? "",
		ctx,
		generation: randomUUID(),
		surface: ctx.mode,
		timers: [],
		handled: new Set(),
		refusals: new Map(),
		inflight: null,
		orphansPending: false,
		orphanRetry: new Set(),
		revisionNoticeShown: false,
		sessionId: sessionIdOf(ctx),
		turn: "",
		turns: 0,
		seq: 0,
		run: "",
		runs: 0,
		runOutcome: "",
		runStartAt: -1,
		requestUser: "",
	};
	writeLiveness(b, true);
	b.orphansPending = !closeOrphanedReceipts(b);
	const beat = setInterval(() => writeLiveness(b, true), HEARTBEAT_MS);
	const poll = setInterval(() => scanSafely(pi, b), POLL_MS);
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

// scanSafely is scan whose failure waits for the next poll: a transient
// filesystem error is retried then.
function scanSafely(pi: ExtensionAPI, b: Bridge): void {
	try {
		scan(pi, b);
	} catch {
		// Retried on the next poll.
	}
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
		// A known refusal whose append failed stays pending even when its
		// line is visible: the line may not be durable.
		if (!b.refusals.has(ref) && (exists(path.join(b.dir, "receipts", `${ref}.json`)) || hasTerminal(b.dir, ref))) {
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
	} else if (f && !b.deliver && !f.started && Date.now() - f.deliveredAt > START_GRACE_MS && b.ctx.isIdle() && !b.ctx.hasPendingMessages()) {
		// pi reports a failed sendUserMessage out of band. An idle session
		// with nothing queued that never started the follow-up most likely
		// dropped it, but nothing proves that it did not run. A host's
		// delivery reports its own outcome instead.
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
		const busy = refuse ? "" : busyReason(b);
		if (busy) {
			refuse = "busy";
			error = busy;
		}
		if (refuse) {
			// The refusal is final for the ref. A failed append keeps it,
			// and the next poll appends the same refusal again.
			if (refuse === "revision" && !b.revisionNoticeShown) {
				// An old amq-remote cannot read the refusal's text, so the
				// owner's cue is this notice in pi.
				b.revisionNoticeShown = true;
				notify(b.ctx, "amq-bridge refused a remote request: the amq-remote on this machine is older than this pi extension. Upgrade amq-remote to the matching release and restart it.");
			}
			const fields: EventFields = { event: "refused", reason: refuse, error };
			if (appendEvent(b.dir, ref, fields)) b.handled.add(ref);
			else b.refusals.set(ref, fields);
			continue;
		}
		const claim = claimReceipt(b, ref);
		if (claim === "error") continue; // retried on the next poll
		b.handled.add(ref);
		if (claim === "taken") continue; // another bridge process already claimed it
		const f: Inflight = {
			ref,
			text: verdict.text,
			deliveredAt: Date.now(),
			started: false,
			tagged: false,
			lastText: "",
			lastError: "",
			finalText: "",
			outcome: "",
			terminal: null,
		};
		b.inflight = f;
		if (b.deliver) {
			const request: AmqRequest = { ref, text: verdict.text, notAfter: verdict.notAfter };
			if (verdict.origin) request.origin = verdict.origin;
			try {
				b.deliver.send(b.ctx, request, reporter(b, ref));
			} catch (err) {
				// A reported outcome stands. A request the host reported
				// started may have run, so its outcome is unknown.
				if (!f.terminal && b.inflight === f) settle(b, { event: f.started ? "uncertain" : "failed", error: `deliver: ${String(err)}` });
			}
			continue;
		}
		try {
			pi.sendUserMessage(verdict.text, { deliverAs: "followUp" });
		} catch (err) {
			settle(b, { event: "failed", error: `sendUserMessage: ${String(err)}` });
		}
	}
}

// reporter is the report callback for the request ref that a host's
// delivery was handed. A report for a request that is no longer this
// bridge's open request (decided, closed at shutdown, or from an earlier
// session) is ignored.
function reporter(b: Bridge, ref: string): (outcome: AmqOutcome) => void {
	return (outcome) => {
		const f = b.inflight;
		if (!f || f.ref !== ref || f.terminal) return;
		if ((outcome as { event?: unknown } | null)?.event !== "started") {
			settle(b, reportedFields(outcome));
			return;
		}
		if (f.started) return;
		f.started = true;
		appendEvent(b.dir, ref, { event: "started" });
	};
}

// reportedFields is the terminal event a host's report records: the report
// when it has a terminal AmqOutcome shape, else uncertain, with an error that
// names what the host sent.
function reportedFields(outcome: unknown): EventFields {
	const o = (outcome ?? {}) as { event?: unknown; text?: unknown; error?: unknown; reason?: unknown };
	switch (o.event) {
		case "completed":
			if (typeof o.text === "string") return { event: "completed", text: o.text };
			break;
		case "failed":
		case "uncertain":
			if (typeof o.error === "string") return { event: o.event, error: o.error };
			break;
		case "cancelled":
			return { event: "cancelled" };
		case "refused":
			if (o.reason === "expired" && (o.error === undefined || typeof o.error === "string")) return { event: "refused", reason: "expired", error: o.error };
			break;
	}
	let sent = "an unprintable value";
	try {
		sent = JSON.stringify(outcome) ?? String(outcome);
	} catch {
		// sent stays the placeholder.
	}
	return { event: "uncertain", error: `the host reported an outcome that is not an AmqOutcome: ${truncateUtf8(sent, 1024)}` };
}

// busyReason is why a request would wait behind other work, or "" when it
// would not. A host busy check that throws counts as busy.
function busyReason(b: Bridge): string {
	if (b.inflight) return "another remote request is in flight";
	if (!b.deliver) return !b.ctx.isIdle() || b.ctx.hasPendingMessages() ? "pi is busy with other work" : "";
	try {
		return b.deliver.busy(b.ctx) ? "pi is busy with other work" : "";
	} catch (err) {
		return `deliver.busy: ${String(err)}`;
	}
}

type Verdict = { refuse: string; error: string; text: string; notAfter: string; origin?: { carrier: string; name: string } };

// MAX_ORIGIN_NAME is the protocol's bound on a request origin's name.
const MAX_ORIGIN_NAME = 64;

// originOf is the request's origin when it is valid (both fields non-empty
// strings, name at most 64 bytes), else undefined.
function originOf(v: unknown): { carrier: string; name: string } | undefined {
	if (v === null || typeof v !== "object") return undefined;
	const { carrier, name } = v as { carrier?: unknown; name?: unknown };
	if (typeof carrier !== "string" || !carrier || typeof name !== "string" || !name) return undefined;
	return Buffer.byteLength(name, "utf8") <= MAX_ORIGIN_NAME ? { carrier, name } : undefined;
}

function examine(b: Bridge, ref: string): Verdict {
	const refuse = (reason: string, error: string): Verdict => ({ refuse: reason, error, text: "", notAfter: "" });
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
	const rev = req.bridge_revision;
	if (typeof rev !== "number" || rev < MIN_ADAPTER_REVISION || rev > BRIDGE_REVISION) {
		return refuse(
			"revision",
			`the request needs bridge_revision ${rev === undefined ? "(none)" : JSON.stringify(rev)} and this pi bridge accepts ${MIN_ADAPTER_REVISION} to ${BRIDGE_REVISION}: upgrade amq-remote to the release that matches this extension and restart it`,
		);
	}
	const notAfter = typeof req.not_after === "string" ? Date.parse(req.not_after) : Number.NaN;
	if (Number.isNaN(notAfter)) return refuse("invalid", "not_after is not an RFC 3339 time");
	if (Date.now() > notAfter) return refuse("expired", "not_after passed before delivery");
	const hint = req.epoch_hint ?? "";
	if (typeof hint !== "string") return refuse("invalid", "epoch_hint is not a string");
	// The hint names the session generation the request addresses. An
	// absent or empty hint addresses none, so a request retained across
	// session_start never runs in a session it was not sent to.
	if (hint !== b.generation) return refuse("generation", "epoch_hint does not match the live session generation");
	return { refuse: "", error: "", text: req.text, notAfter: req.not_after as string, origin: originOf(req.origin) };
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
		session_id: sessionIdOf(b.ctx) || undefined,
		activity: ACTIVITY_VERSION,
		upgrade: b.upgrade || undefined,
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
			const retry = b.orphanRetry.has(ref);
			if (rc.session_generation === b.generation || (!retry && hasTerminal(b.dir, ref))) continue;
			const ok = appendEvent(b.dir, ref, {
				event: "uncertain",
				error: "the pi bridge restarted before the request settled; the outcome is unknown",
			});
			if (ok) {
				b.orphanRetry.delete(ref);
			} else {
				b.orphanRetry.add(ref);
				done = false;
			}
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
	return appendLine(path.join(dir, "events", `${ref}.jsonl`), `${JSON.stringify(ev)}\n`, 0);
}

// appendLine appends one fsynced line and reports whether it is durable. A
// file already past maxSize (when set) is truncated first: the history is
// not evidence.
function appendLine(file: string, line: string, maxSize: number): boolean {
	let fd: number | undefined;
	try {
		fd = fs.openSync(file, "a+", 0o600);
		let size = fs.fstatSync(fd).size;
		if (maxSize > 0 && size > maxSize) {
			fs.ftruncateSync(fd, 0);
			size = 0;
		}
		const last = Buffer.alloc(1);
		if (size > 0 && fs.readSync(fd, last, 0, 1, size - 1) === 1 && last[0] !== 0x0a) writeAll(fd, "\n");
		writeAll(fd, line);
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

// sessionIdOf is pi's own session id, which a relay share pins and which
// names the activity file; "" when pi gives none that is safe as a file name.
function sessionIdOf(ctx: ExtensionContext): string {
	try {
		// ReadonlySessionManager.getSessionId: pi session-manager.d.ts:178,246.
		const id = ctx.sessionManager.getSessionId();
		return SESSION_ID.test(id) ? id : "";
	} catch {
		return "";
	}
}

type ActivityFields = { kind: string; id?: string; text?: string; tool?: string; status?: string };

// appendActivity appends one record to activity/<session_id>.jsonl. The
// stream is best effort and never affects a request: a failed append drops
// the record.
function appendActivity(b: Bridge, f: ActivityFields): void {
	if (!b.sessionId) return;
	const rec: Record<string, string | number> = { protocol: PROTOCOL, seq: ++b.seq, at: new Date().toISOString(), session: b.sessionId, turn: b.turn };
	if (b.run) rec.run = b.run;
	rec.kind = f.kind;
	if (f.id) rec.id = f.id;
	if (f.text) rec.text = f.text;
	if (f.tool) rec.tool = f.tool;
	if (f.status) rec.status = f.status;
	const file = path.join(b.dir, "activity", `${b.sessionId}.jsonl`);
	if (f.kind === "run_start") b.runStartAt = keepLastRun(file, b.runStartAt);
	appendLine(file, `${JSON.stringify(rec)}\n`, MAX_ACTIVITY_FILE_HARD);
}

// keepLastRun replaces an activity file past MAX_ACTIVITY_FILE with its bytes
// from offset from on (the last run, which has settled), through a renamed
// temporary file, so a reader sees either the old file or the new one. It
// returns the file's size, where the next record starts.
function keepLastRun(file: string, from: number): number {
	let size: number;
	try {
		size = fs.statSync(file).size;
	} catch {
		return 0;
	}
	if (size <= MAX_ACTIVITY_FILE) return size;
	const tail = Buffer.alloc(from >= 0 && from < size ? size - from : 0);
	const tmp = `${file}.tmp`;
	try {
		const fd = fs.openSync(file, "r");
		try {
			if (tail.length > 0 && fs.readSync(fd, tail, 0, tail.length, from) !== tail.length) throw new Error("short read");
		} finally {
			fs.closeSync(fd);
		}
		const out = fs.openSync(tmp, "w", 0o600);
		try {
			if (tail.length > 0) writeAll(out, tail.toString("utf8"));
			fs.fsyncSync(out);
		} finally {
			fs.closeSync(out);
		}
		fs.renameSync(tmp, file);
		return tail.length;
	} catch {
		try {
			fs.unlinkSync(tmp);
		} catch {
			// Nothing was left behind.
		}
		return size;
	}
}

// activityHooks records the whole session, not only remote requests: runs,
// turn boundaries, whole user and assistant messages, and tool calls with
// their results. Event names and payloads are pi-coding-agent
// dist/core/extensions/types.d.ts: turn_start/turn_end :790-:802,
// message_end :816, tool_execution_start :821, tool_execution_end :840;
// agent_start, agent_before_settle and agent_settled are src/core/extensions/
// types.ts AgentStartEvent, AgentBeforeSettleEvent and AgentSettledEvent.
// Streaming deltas are not recorded.
//
// A run is one prompt: agent_start repeats inside it when pi continues or
// retries, and agent_settled comes once, when nothing more will run.
function activityHooks(pi: ExtensionAPI, get: () => Bridge | null): void {
	pi.on("agent_start", () => {
		const b = get();
		if (!b || b.run) return;
		b.run = `${b.generation.slice(0, 8)}-r${++b.runs}`;
		b.runOutcome = "";
		appendActivity(b, { kind: "run_start" });
	});
	pi.on("agent_before_settle", (event) => {
		const b = get();
		if (b) b.runOutcome = event.outcome;
	});
	pi.on("agent_settled", (event) => {
		const b = get();
		if (!b) return;
		appendActivity(b, { kind: "settled", status: event.aborted ? "aborted" : b.runOutcome || "error" });
		b.run = "";
		b.runOutcome = "";
		b.requestUser = "";
	});
	pi.on("turn_start", () => {
		const b = get();
		if (!b) return;
		b.turn = `${b.generation.slice(0, 8)}-${++b.turns}`;
		appendActivity(b, { kind: "turn_start" });
	});
	pi.on("turn_end", () => {
		const b = get();
		if (!b) return;
		appendActivity(b, { kind: "turn_end" });
		b.turn = "";
	});
	pi.on("message_end", (event) => {
		const b = get();
		if (!b) return;
		const msg = event.message as { role?: string; content?: unknown };
		if (msg.role !== "user" && msg.role !== "assistant") return;
		// The request's own user message carries its ref.
		const id = msg.role === "user" ? b.requestUser : "";
		if (msg.role === "user") b.requestUser = "";
		const text = truncateUtf8(textOf(msg.content), MAX_ACTIVITY_TEXT);
		if (text) appendActivity(b, { kind: msg.role, id, text });
	});
	pi.on("tool_execution_start", (event) => {
		const b = get();
		if (b) appendActivity(b, { kind: "tool_start", id: event.toolCallId, tool: event.toolName });
	});
	pi.on("tool_execution_end", (event) => {
		const b = get();
		if (!b) return;
		// result is an AgentToolResult: pi-agent-core dist/types.d.ts:370,
		// content is text or image parts.
		const text = textOf((event.result as { content?: unknown } | undefined)?.content);
		const cap = MAX_ACTIVITY_TEXT - 12;
		const bounded = Buffer.byteLength(text, "utf8") > cap ? `${truncateUtf8(text, cap)}\n[truncated]` : text;
		appendActivity(b, { kind: "tool_end", id: event.toolCallId, tool: event.toolName, text: bounded, status: event.isError ? "failed" : "completed" });
	});
}

function noteAssistant(f: Inflight, message: unknown): void {
	const msg = message as { role?: string; content?: unknown; errorMessage?: unknown; stopReason?: unknown };
	if (msg.role !== "assistant") return;
	const text = textOf(msg.content);
	if (text) f.lastText = text;
	if (typeof msg.errorMessage === "string" && msg.errorMessage) f.lastError = msg.errorMessage;
	const toolCall = Array.isArray(msg.content) && msg.content.some((p) => p && p.type === "toolCall");
	f.finalText = msg.stopReason === "stop" && !toolCall ? text : "";
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

// WAKE_DIALOG is the marker the AMQ wake reads (docs/wake-operations.md):
// while it exists the wake holds its doorbell instead of typing into pi.
// This extension is the only writer of a pi agent's marker, and it removes
// only a marker it wrote; no AMQ Go path writes or conditionally removes a
// pi agent's marker. That is why it needs no .wake.dialog.lock, which Node
// cannot take (it has no flock).
const WAKE_DIALOG = ".wake.dialog";
const WAKE_DIALOG_PROMPT = "ui_prompt";
const WAKE_DIALOG_TRUST = "project_trust";

// wakeAgentDir is this pi's AMQ agent directory, or "" without an identity
// or an existing mailbox.
function wakeAgentDir(id: AmqIdentity | undefined): string {
	if (!id) return "";
	const dir = path.join(id.root, "agents", id.handle);
	try {
		return fs.lstatSync(dir).isDirectory() ? dir : "";
	} catch {
		return "";
	}
}

// openWakeDialog installs the marker by an exclusive temp file and a rename,
// without fsync: the marker is advisory, and the prompt is rendering.
function openWakeDialog(id: AmqIdentity | undefined, event: string, kind: string): void {
	const dir = wakeAgentDir(id);
	if (!dir) return;
	const marker = {
		schema: 1,
		event,
		harness: "pi",
		kind,
		opened_at: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
	};
	const temp = path.join(dir, `.wake.dialog.tmp.${process.pid}.${randomUUID()}`);
	try {
		fs.writeFileSync(temp, `${JSON.stringify(marker)}\n`, { flag: "wx", mode: 0o600 });
		fs.renameSync(temp, path.join(dir, WAKE_DIALOG));
	} catch {
		try {
			fs.unlinkSync(temp);
		} catch {
			// The temp file was never created or is already gone.
		}
	}
}

// closeWakeDialog removes the marker only when this extension wrote it, so
// a marker another writer installed keeps holding.
function closeWakeDialog(id: AmqIdentity | undefined): void {
	const dir = wakeAgentDir(id);
	if (!dir) return;
	const file = path.join(dir, WAKE_DIALOG);
	try {
		const marker = JSON.parse(fs.readFileSync(file, "utf8")) as { event?: string; harness?: string };
		if ((marker.event === WAKE_DIALOG_PROMPT || marker.event === WAKE_DIALOG_TRUST) && marker.harness === "pi") fs.unlinkSync(file);
	} catch {
		// No marker, or one this extension did not write.
	}
}

// The remote dialog store is <store>/dialogs/<binding>/{requests,resolved}
// (internal/remote/dialog). A seam shows a dialog in Buzz only when a mailbox
// binding names this pi's root and handle; without one it does nothing. The Go
// liveness rule also compares the holder's pid and process start token, which
// Node cannot read portably, so only a binding whose root is gone counts as
// ended here.
let remoteDialog: { dir: string; id: string } | null = null;

function remoteStore(): string {
	const file = process.env.AMQ_REMOTE_BINDING?.trim();
	return file ? path.dirname(file) : path.join(os.homedir(), ".amq", "remote");
}

function realPath(p: string): string {
	try {
		return fs.realpathSync(p);
	} catch {
		return path.resolve(p);
	}
}

// remoteBinding is the name of the mailbox binding that serves this pi.
function remoteBinding(store: string, id: AmqIdentity | undefined): string {
	if (!id) return "";
	const { root, handle } = id;
	const want = realPath(root);
	let names: string[] = [];
	try {
		names = fs.readdirSync(path.join(store, "bindings"));
	} catch {
		return "";
	}
	for (const file of names.sort()) {
		const name = file.replace(/\.json$/, "");
		if (name === file || file.endsWith(".ended.json")) continue;
		try {
			const b = JSON.parse(fs.readFileSync(path.join(store, "bindings", file), "utf8")) as { carrier?: string; root?: string; handle?: string };
			if (b.carrier === "mailbox" && b.handle === handle && b.root && fs.existsSync(b.root) && realPath(b.root) === want) return name;
		} catch {
			// An unreadable binding serves nothing.
		}
	}
	return "";
}

// openRemoteDialog writes a request with no options, so Buzz shows the prompt
// and says to answer in the terminal.
function openRemoteDialog(identity: AmqIdentity | undefined, kind: string, title: string, session: string): void {
	closeRemoteDialog();
	const store = remoteStore();
	const name = remoteBinding(store, identity);
	if (!name) return;
	const dir = path.join(store, "dialogs", name);
	const id = `d-${randomUUID().replace(/-/g, "")}`;
	const text = `${title || "a prompt"} (${kind})`;
	const request = { id, harness: "pi", session, tool: "", text, options: [], opened_at: new Date().toISOString(), pid: process.pid };
	try {
		for (const sub of ["requests", "answers", "resolved"]) fs.mkdirSync(path.join(dir, sub), { recursive: true, mode: 0o700 });
		const temp = path.join(dir, "requests", `.${id}.tmp-${randomUUID()}`);
		fs.writeFileSync(temp, JSON.stringify(request), { flag: "wx", mode: 0o600 });
		fs.renameSync(temp, path.join(dir, "requests", `${id}.json`));
		remoteDialog = { dir, id };
	} catch {
		// A dialog that cannot be shown leaves the prompt to the terminal.
	}
}

// closeRemoteDialog records that the terminal answered. The record is linked
// into place whole and never replaces an earlier resolution.
function closeRemoteDialog(): void {
	const open = remoteDialog;
	remoteDialog = null;
	if (!open) return;
	const resolved = { id: open.id, outcome: "terminal", by: "terminal", at: new Date().toISOString() };
	const sub = path.join(open.dir, "resolved");
	const temp = path.join(sub, `.${open.id}.tmp-${randomUUID()}`);
	try {
		fs.writeFileSync(temp, JSON.stringify(resolved), { flag: "wx", mode: 0o600 });
		fs.linkSync(temp, path.join(sub, `${open.id}.json`));
	} catch {
		// Already resolved, or the store is gone.
	} finally {
		try {
			fs.unlinkSync(temp);
		} catch {
			// No temp file left.
		}
	}
}

function notify(ctx: ExtensionContext, message: string): void {
	try {
		ctx.ui.notify(message, "info");
	} catch {
		console.error(message);
	}
}
