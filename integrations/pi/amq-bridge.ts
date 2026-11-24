// AMQ pi bridge: makes a live pi session reachable by amq-remote target kind
// `pi`. The wire contract is docs/pi-bridge-protocol.md; this file is the
// extension side of it. The adapter writes requests/; this extension writes
// receipts/, events/, and bridge.liveness.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { randomBytes, randomUUID } from "node:crypto";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

export const PROTOCOL = "amq:pi-bridge:v1";
// BRIDGE_REVISION names the implementation rules the adapter relies on
// (docs/pi-bridge-protocol.md, bridge revision). The adapter sends no new
// requests to a bridge that publishes a lower revision or none. Revision 4
// is revision 3 plus pi's session id in liveness; revision 5 adds the exact
// cancel of one request (cancels/<ref>.json); revision 6 adds the granted
// request and the guard that asks the person at the terminal before its
// tool calls run. Revision 7 adds the wake doorbell exchange
// (docs/pi-bridge-protocol.md, doorbell): the wake asks an idle serving pi
// to show the fixed doorbell as a visible custom message that starts one
// turn. The bridge claims one exclusive decision file per attempt, shows
// the line, and publishes the matching message_start as the acceptance
// result. The wake sends no mail content on this path.
export const BRIDGE_REVISION = 7;
// GRANT_REVISION is the lowest request bridge_revision that may carry
// granted: true.
const GRANT_REVISION = 6;
// DOORBELL_LINE is the fixed visible line the bridge shows; it matches the
// harness doorbell constant (internal/harness), so a reader of the
// transcript sees the same text a typed doorbell leaves.
export const DOORBELL_LINE = ": AMQ doorbell run amq drain --include-body then act on it";
// DOORBELL_ATTEMPT bounds the attempt id the wake mints: one durable id per
// pending cohort, lowercase alphanumerics and dashes.
const DOORBELL_ATTEMPT = /^[a-z0-9][a-z0-9-]{0,63}$/;
// DOORBELL_DONE_MAX bounds the attempts this runtime remembers as decided;
// past it the set resets, because every request is also gated by its
// generation and deadline on a re-read.
const DOORBELL_DONE_MAX = 128;
// A request carries the bridge_revision its adapter requires. A request
// without one comes from an adapter that predates the revision fence and is
// refused, so an old adapter cannot send work through this bridge.
const MIN_ADAPTER_REVISION = 3;

const HEARTBEAT_MS = 2000;
// OWNER_STALE_MS is how long a live liveness record keeps the mailbox for
// its generation. One bridge serves a mailbox: another bridge starts only on
// a record that is older, not live, or absent, and a request is closed
// uncertain as an orphan only when its last event is older. It is far longer
// than the adapter's 5-second staleness, so a stalled owner keeps its
// mailbox and its running request.
const OWNER_STALE_MS = 60_000;
const POLL_MS = 200; // the adapter waits 2 s for a receipt after it publishes a request
// A poll lists requests/ only when the directory changed, and in any case
// every RECONCILE_MS. A listing within COARSE_MTIME_MS of the directory's
// mtime is repeated, because a coarse mtime cannot show a change made later
// in the same tick.
const RECONCILE_MS = 5 * 60 * 1000;
const COARSE_MTIME_MS = 2000;
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

// AmqDialog is a prompt an extension shows in its own terminal UI and also
// offers in Buzz. Option i is shown as number i+1; deny marks the option ❌
// picks. signal aborts when the terminal answered first.
export type AmqDialog = { title: string; body?: string; options: { label: string; deny?: boolean }[]; signal?: AbortSignal };

// AmqDialogAnswer is the owner's verified choice from Buzz: an index into
// the dialog's options.
export type AmqDialogAnswer = { by: "buzz"; index: number };

// AmqBridge is the handle registerAmqBridge returns. openDialog resolves the
// owner's Buzz answer, or undefined when the terminal keeps the decision: no
// Buzz binding serves this pi, the signal aborted, the dialog expired, or
// Buzz failed. It never rejects.
export type AmqBridge = { openDialog(d: AmqDialog): Promise<AmqDialogAnswer | undefined> };

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
	// pending holds each listed ref not handled yet, with its request's
	// mtime; it is the work a poll examines.
	pending: Map<string, number>;
	// listedMtime and listedAt are requests/'s mtime and the time of the
	// last listing.
	listedMtime: number;
	listedAt: number;
	// cancelsOpen holds the refs of cancel files not applied yet; cancelsDone
	// the refs of cancel files that need no more work. cancelListedMtime and
	// cancelListedAt are cancels/'s mtime and the time of the last listing.
	cancelsOpen: Set<string>;
	cancelsDone: Set<string>;
	cancelListedMtime: number;
	cancelListedAt: number;
	// refusals holds a refusal decided for a ref whose append failed. The
	// ref is never examined again; only this same refusal is retried.
	refusals: Map<string, EventFields>;
	inflight: Inflight | null;
	// orphans holds the receipts of other generations that may still need
	// closing; null until receipts/ is listed.
	orphans: Set<string> | null;
	// orphanRetry holds orphan refs whose closing append failed. Their
	// bytes may be on disk without being durable, so they are appended
	// again until an append succeeds.
	orphanRetry: Set<string>;
	// revisionNoticeShown limits the old-adapter notice to once per runtime.
	revisionNoticeShown: boolean;
	// doorbell is the state of the wake doorbell exchange: claimed holds each
	// claimed attempt awaiting its matching message_start, done the attempts
	// decided or seen expired, so a poll never re-examines them.
	doorbell: { claimed: Map<string, number>; done: Set<string> };
	ownerNotice: { shown: boolean };
	displaced: (b: Bridge) => void;
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
	// requestUser identifies the in-flight request whose own user
	// message started and has not ended yet: the first user message, after
	// the hand-off, whose text is the request's text.
	requestUser: { ref: string; text: string } | null;
};

type Inflight = {
	ref: string;
	text: string;
	deliveredAt: number;
	started: boolean;
	// tagged is true once the request's own user message started.
	tagged: boolean;
	// arm is the guard arm of a granted request, "" for any other request.
	arm: string;
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

// The guard of granted tasks (docs/pi-bridge-protocol.md, bridge revision
// 6). While any arm is open, every tool call except pi's own read-only
// built-ins waits for the typed yes of the person at the terminal. The arms
// are state of this module instance plus one file per pi process,
// granted-guard.<pid>.json in the bridge directory; no mailbox object holds
// them, so a displaced bridge, a new session or a reload keeps them. An arm
// ends only when its hand-off failed synchronously, or when its granted
// message started in a session, that session then settled with no pending
// message, and after that the person started a new run in that session;
// otherwise it stays for the life of the pi process.
type Arm = {
	ref: string;
	// tagged: the granted message started. owner is the registration whose
	// session ran it, the only one whose events settle or end the arm.
	tagged: boolean;
	owner: ExtensionAPI | null;
	// settled: the owner's session settled after the tag with no pending
	// message.
	settled: boolean;
	dir: string;
	armedAt: string;
};
const arms = new Map<string, Arm>();
// guardRestored holds the bridge directories whose guard file this module
// instance has read.
const guardRestored = new Set<string>();
// GUARD_PID is the pi process that loaded this module. It names the only
// guard file this module reads, writes or removes.
const GUARD_PID = process.pid;
// The on-disk unit is Unix seconds. Keep millisecond precision across reloads.
const GUARD_STARTED = Math.round(performance.timeOrigin) / 1000;
const ARM_ID = /^[0-9a-f]{32}$/;
const READ_ONLY_BUILTINS = new Set(["read", "grep", "find", "ls"]);
const gateTextLimit = 2000;
const gateRowLimit = 20;
// Pi 1.5.2 ExtensionInputComponent (extension-input.js:30-49) adds two
// borders, four single-row spacers, the input and its key hints to the title.
const inputDialogRows = 8;
// The fullscreen dock reserves a transcript row, a widget spacer and two footer rows.
const dockRows = 4;
const gateCannotMeasure = "This pi cannot measure the approval dialog; a task that needs this call must be sent with a signature.";
const gateDenied = "The person at the terminal did not approve this call. It is a call of a task that a linked server sent under a session grant.";
const gateNoTerminal = "This call needs the person's yes at the terminal, and this pi session has no terminal UI.";
const gateTooLong = "This call is too long to show whole at the terminal for approval. A task that needs it must be sent with a signature.";
const gateExtensionLine = "This is a tool of an extension. AMQ cannot see what it does or starts; one yes covers the whole call.";

function guardFile(dir: string): string {
	return path.join(dir, `granted-guard.${GUARD_PID}.json`);
}

// Only the native hand-off and its ownership match use the tagged text.
function handoffText(f: Inflight): string {
	return f.arm ? `${f.text}\n\n[AMQ granted task ${f.arm}]` : f.text;
}

// writeGuard makes this process's guard file in dir hold the open arms of
// dir: it replaces the whole file, mode 0600, or removes it when dir has
// none. It reports whether the file now holds them.
function writeGuard(dir: string): boolean {
	const open = [...arms].filter(([, a]) => a.dir === dir).map(([arm, a]) => ({ arm, ref: a.ref, armed_at: a.armedAt }));
	try {
		if (open.length === 0) {
			fs.rmSync(guardFile(dir), { force: true });
			return true;
		}
		const tmp = writeTemp(dir, JSON.stringify({ pid: GUARD_PID, started: GUARD_STARTED, arms: open }));
		try {
			fs.renameSync(tmp, guardFile(dir));
		} catch (err) {
			fs.rmSync(tmp, { force: true });
			throw err;
		}
		return true;
	} catch {
		return false;
	}
}

// restoreGuard reads this process's guard file in dir once per module
// instance, so the arms an earlier load of the extension in the same pi
// process opened stay open. A file whose start is not this process's start
// was left by an earlier process with the same pid: it is removed and
// restores nothing. A restored arm is not tagged: it ends only with the
// process.
function restoreGuard(dir: string): void {
	if (guardRestored.has(dir)) return;
	guardRestored.add(dir);
	let rec: { pid?: unknown; started?: unknown; arms?: unknown };
	try {
		rec = JSON.parse(fs.readFileSync(guardFile(dir), "utf8"));
	} catch {
		return;
	}
	if (!rec || rec.pid !== GUARD_PID || !Array.isArray(rec.arms)) return;
	// An unknown origin leaves the arms open. Only a known different
	// process start proves that the file is stale.
	if (typeof rec.started === "number" && Number.isFinite(rec.started) && rec.started > 0 && Math.abs(rec.started - GUARD_STARTED) > 1) {
		try {
			fs.rmSync(guardFile(dir), { force: true });
		} catch {
			// It stays; only a process with this pid reads it.
		}
		return;
	}
	for (const a of rec.arms as { arm?: unknown; ref?: unknown; armed_at?: unknown }[]) {
		if (typeof a?.arm !== "string" || !ARM_ID.test(a.arm) || arms.has(a.arm)) continue;
		arms.set(a.arm, { ref: typeof a.ref === "string" ? a.ref : "", tagged: false, owner: null, settled: false, dir, armedAt: typeof a.armed_at === "string" ? a.armed_at : "" });
	}
}

// openArm opens arm for the granted request ref and writes the guard file
// of dir. When the file cannot be written the arm is not opened and it
// returns false, so the request is not sent.
function openArm(dir: string, arm: string, ref: string): boolean {
	arms.set(arm, { ref, tagged: false, owner: null, settled: false, dir, armedAt: new Date().toISOString() });
	if (writeGuard(dir)) return true;
	arms.delete(arm);
	return false;
}

function endArm(arm: string): void {
	const a = arms.get(arm);
	if (!a) return;
	arms.delete(arm);
	writeGuard(a.dir);
}

// settleArms marks settled every tagged arm of owner when pi holds no
// pending message: the granted message started in owner's session and the
// run it joined has settled. The arm stays open until the person types.
function settleArms(owner: ExtensionAPI, ctx: ExtensionContext): void {
	if (arms.size === 0) return;
	let pending = true;
	try {
		pending = ctx.hasPendingMessages();
	} catch {
		// Unknown counts as pending: nothing settles.
	}
	if (pending) return;
	for (const a of arms.values()) {
		if (a.tagged && a.owner === owner) a.settled = true;
	}
}

// endTypedArms ends every settled arm of owner: the person typed a prompt in
// owner's session after the granted run settled, so the run it starts is
// the person's own.
function endTypedArms(owner: ExtensionAPI): void {
	const dirs = new Set<string>();
	for (const [arm, a] of arms) {
		if (!a.settled || a.owner !== owner) continue;
		arms.delete(arm);
		dirs.add(a.dir);
	}
	for (const dir of dirs) writeGuard(dir);
}

// C0, DEL, C1, format controls and Unicode line/paragraph separators.
const HIDDEN = /[\u0000-\u001f\u007f-\u009f\u2028\u2029]|\p{Cf}/gu;

// visible writes every HIDDEN character of text as its \uXXXX escape, one
// per UTF-16 code unit.
function visible(text: string): string {
	return text.replace(HIDDEN, (c) => {
		let out = "";
		for (let i = 0; i < c.length; i++) out += `\\u${c.charCodeAt(i).toString(16).padStart(4, "0")}`;
		return out;
	});
}

// callText is the text the dialog shows for a call's input: every argument,
// escaped by JSON and with no character that can hide text, the same for
// every tool. undefined when the input has no JSON form.
function callText(input: unknown): string | undefined {
	let text: string | undefined;
	try {
		text = JSON.stringify(input, null, 1);
	} catch {
		return undefined;
	}
	// JSON has already escaped controls in its strings. Keep its layout newlines.
	return text === undefined ? undefined : text.split("\n").map(visible).join("\n");
}

type ToolCall = { toolName?: unknown; input?: unknown };
type GateResult = { block: true; reason: string } | undefined;

// gate decides one tool call while an arm is open. A call runs only when it
// is a read-only built-in of pi itself, or when the person at the terminal
// types yes; every other answer and every failure blocks it.
async function gate(pi: ExtensionAPI, event: ToolCall, ctx: ExtensionContext): Promise<GateResult> {
	const deny = (reason: string): GateResult => ({ block: true, reason });
	const name = typeof event.toolName === "string" ? event.toolName : "";
	let source: { source?: unknown; path?: unknown } | undefined;
	try {
		source = pi.getAllTools().find((t) => t.name === name)?.sourceInfo as { source?: unknown; path?: unknown } | undefined;
	} catch {
		source = undefined;
	}
	// pi's own tool, not an extension's: a built-in extension's tools also
	// carry source builtin, under the path of that extension.
	const builtin = source?.source === "builtin" && source.path === `builtin:${name}`;
	if (builtin && READ_ONLY_BUILTINS.has(name)) return undefined;
	try {
		if (ctx.mode !== "tui") return deny(gateNoTerminal);
		const text = callText(event.input);
		if (text === undefined || text.length > gateTextLimit) return deny(gateTooLong);
		const shown = visible(name);
		const head = [...arms.values()].some((a) => a.tagged)
			? `A task that a linked server sent wants to run \`${shown}\`:`
			: `A task that a linked server sent may still run in this session. \`${shown}\` wants to run:`;
		const title = [head, text, ...(builtin ? [] : [gateExtensionLine])].join("\n") + `\n\nType yes to run ${shown}. Any other answer does not run it.`;
		// The dialog title has one cell of padding on either side. Pi's
		// wrapper counts glyph cells and uses the same word breaks as its Text.
		const tui = await import("@earendil-works/pi-tui").catch(() => undefined);
		if (typeof tui?.wrapTextWithAnsi !== "function") return deny(gateCannotMeasure);
		const terminalColumns = process.stdout.columns || 80;
		if (terminalColumns < 40) return deny(gateTooLong);
		const columns = terminalColumns - 2;
		const rows = process.stdout.rows || Infinity;
		const availableTitleRows = rows - inputDialogRows - dockRows;
		if (tui.wrapTextWithAnsi(text, columns).length > gateRowLimit || tui.wrapTextWithAnsi(title, columns).length > availableTitleRows) {
			return deny(gateTooLong);
		}
		const answer = await ctx.ui.input(title, "", { signal: ctx.signal });
		return typeof answer === "string" && answer.trim().toLowerCase() === "yes" ? undefined : deny(gateDenied);
	} catch {
		return deny(gateDenied);
	}
}

export default function amqPiBridge(pi: ExtensionAPI): void {
	registerAmqBridge(pi);
}

// registerAmqBridge registers the bridge on pi. A host application passes
// its own identity, delivery, or upgrade remedy; without options this is
// the plain extension.
export function registerAmqBridge(pi: ExtensionAPI, options: AmqBridgeOptions = {}): AmqBridge {
	// The guard does not use the mailbox: these handlers read only the arms,
	// so a pi that lost or never took its mailbox stays guarded. pi emits
	// session_start, which restores the guard file, before any run.
	pi.on("tool_call", async (event, ctx) => {
		if (arms.size === 0) return undefined;
		return gate(pi, event, ctx);
	});
	pi.on("agent_settled", (_event, ctx) => settleArms(pi, ctx));
	// pi emits input before the run that the input starts.
	pi.on("input", (event) => {
		if (arms.size === 0) return undefined;
		if (event.source === "interactive" && event.streamingBehavior === undefined) endTypedArms(pi);
		return undefined;
	});

	let bridge: Bridge | null = null;
	// ownerNotice limits the notices about another bridge to once.
	const ownerNotice = { shown: false };
	// waiting is the beat of a pi that does not serve its mailbox: another
	// live bridge held it off at session_start, or displaced it. A record
	// that is absent or not live is taken at the next beat. A live record is
	// taken only after two of these beats at least a beat apart saw it stale
	// and unchanged, so an owner that resumes from sleep refreshes it first.
	let waiting: ReturnType<typeof setInterval> | null = null;
	const stopWaiting = () => {
		if (waiting) clearInterval(waiting);
		waiting = null;
	};
	const begin = (ctx: ExtensionContext, identity: AmqIdentity) => {
		bridge = start(pi, ctx, options, identity, ownerNotice, displaced);
		if (!bridge) return;
		// A pi that takes over while its own prompt is open holds the
		// doorbell for that prompt from now on.
		if (openPrompts > 0 && !written && outerPrompt) {
			openMarker(WAKE_DIALOG_PROMPT, outerPrompt.kind);
			if (ownDialogs === 0) openRemoteDialog(who(), outerPrompt.kind, outerPrompt.title, bridge.sessionId);
		}
		scanSafely(pi, bridge);
	};
	// serve starts the bridge for identity, or waits while another live
	// bridge holds the mailbox.
	const serve = (ctx: ExtensionContext, identity: AmqIdentity): void => {
		stopWaiting();
		const dir = bridgeDir(identity);
		const held = holderOf(dir);
		if (!held) return begin(ctx, identity);
		if (!held.stale && !ownerNotice.shown) {
			ownerNotice.shown = true;
			notify(ctx, `amq-bridge inactive: another live pi bridge${typeof held.pid === "number" ? ` (pid ${held.pid})` : ""} serves this AMQ mailbox, so amq-remote reaches that pi, not this one.`);
		}
		let seen: { key: string; at: number } | null = null;
		waiting = setInterval(() => {
			const now = Date.now();
			const h = holderOf(dir);
			if (h && !h.stale) {
				seen = null;
				return;
			}
			if (h) {
				// Less a poll for timer jitter: the two beats are a beat apart.
				if (!seen || seen.key !== h.key) seen = { key: h.key, at: now };
				if (now - seen.at < HEARTBEAT_MS - POLL_MS) return;
			}
			stopWaiting();
			const told = ownerNotice.shown;
			ownerNotice.shown = false;
			begin(ctx, identity);
			if (bridge && told) notify(ctx, "amq-bridge now serves Buzz requests.");
		}, HEARTBEAT_MS);
		waiting.unref();
	};
	// displaced is called by a bridge that found another generation in
	// liveness; this pi then waits as a held-off pi does.
	const displaced = (b: Bridge) => {
		if (bridge !== b) return;
		bridge = null;
		serve(b.ctx, b.identity);
	};
	// session holds the identity resolved for the open session, none
	// included; it is null outside a session.
	let session: { identity: AmqIdentity | undefined } | null = null;
	// who is the identity the wake marker and the Buzz dialog use: the
	// session's once it started, else what the identity source gives
	// without a session.
	const who = (): AmqIdentity | undefined => (session ? session.identity : resolveIdentity(options, undefined));
	// serving reports whether this pi may hold the wake doorbell and show Buzz
	// dialogs for its mailbox: only the owning bridge may, never a pi that
	// another live bridge holds off. Before the session starts, a pi serves
	// unless a fresh live record names another bridge.
	const serving = (): boolean => {
		if (bridge) return true;
		if (waiting) return false;
		const id = who();
		const h = id ? holderOf(bridgeDir(id)) : null;
		return !h || h.stale;
	};

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
	// outerPrompt is the kind and title of the outermost open prompt.
	let outerPrompt: { kind: string; title: string } | null = null;
	// Opening a marker is gated on serving(); closing one is not. written
	// holds the bytes of the marker this process last wrote, "" when none
	// may stand. That marker is closed at prompt end and at every session
	// boundary even after this pi stopped serving, so it never holds the
	// serving pi's doorbell; it is removed only while the file still holds
	// those bytes, so another pi's marker that replaced it stays. A marker
	// this process did not write is closed only by the serving pi.
	let written = "";
	const openMarker = (event: string, kind: string) => {
		written = openWakeDialog(who(), event, kind);
	};
	const closeMarker = () => {
		if (written) closeWakeDialog(who(), written);
		else if (serving()) closeWakeDialog(who());
		written = "";
	};
	// The same prompt is shown in Buzz as a dialog with no options: pi
	// exposes no way to answer a prompt from an extension (the prompt events
	// carry only its kind and title), so the owner answers in the terminal and
	// the dialog closes when the prompt ends. While an extension's own
	// openDialog is open, that dialog stands for the prompt in Buzz, so no
	// option-less copy is posted beside it: one open dialog takes the owner's
	// typed reply.
	let ownDialogs = 0;
	pi.on("project_trust", () => {
		if (serving()) {
			openMarker(WAKE_DIALOG_TRUST, "select");
			openRemoteDialog(who(), "select", "Trust project folder?", "");
		}
		return { trusted: "undecided" };
	});
	pi.on("ui_prompt_start", (event) => {
		if (++openPrompts !== 1) return;
		outerPrompt = { kind: event.kind, title: (event as { title?: string }).title ?? "" };
		if (serving()) {
			openMarker(WAKE_DIALOG_PROMPT, outerPrompt.kind);
			if (ownDialogs === 0) openRemoteDialog(who(), outerPrompt.kind, outerPrompt.title, bridge?.sessionId ?? "");
		}
	});
	pi.on("ui_prompt_end", () => {
		if (--openPrompts <= 0) {
			outerPrompt = null;
			closeMarker();
			closeRemoteDialog();
		}
	});

	pi.on("session_start", (_event, ctx) => {
		openPrompts = 0;
		outerPrompt = null;
		closeRemoteDialog();
		stopWaiting();
		stop(bridge, "restart");
		bridge = null;
		ownerNotice.shown = false;
		session = { identity: resolveIdentity(options, ctx) };
		if (session.identity) {
			restoreGuard(bridgeDir(session.identity));
			serve(ctx, session.identity);
		}
		closeMarker();
		if (!session.identity && !options.identity && !inactiveNoticeShown) {
			// A host that gives no identity for this session keeps the bridge
			// off on purpose; only the plain extension tells the owner how to
			// fix it.
			inactiveNoticeShown = true;
			const root = process.env.AM_ROOT ?? "";
			const cause = root && !path.isAbsolute(root) ? `AM_ROOT (${root}) is not an absolute path` : "AM_ROOT and AM_ME are not set to a valid AMQ identity";
			notify(ctx, `amq-bridge inactive: ${cause}. Run pi under \`amq coop exec\` to make it reachable by amq-remote.`);
		}
	});

	pi.on("session_shutdown", (event) => {
		openPrompts = 0;
		outerPrompt = null;
		closeMarker();
		closeRemoteDialog();
		stopWaiting();
		stop(bridge, event.reason);
		// A claimed doorbell never crosses a session boundary: a claim whose
		// acceptance never arrived waits for the wake's deadline, and a new
		// generation cannot service its predecessor's request.
		if (bridge) bridge.doorbell.claimed.clear();
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
	// The doorbell acceptance comes before the user-role handler: the
	// custom entry's role is not a user's, and a matching start proves the
	// bridge's claim reached pi. Require the pending claim, the exact
	// attempt and generation, the fixed content; anything else never reads
	// as acceptance (docs/pi-bridge-protocol.md, doorbell).
	pi.on("message_start", (event) => {
		const b = bridge;
		if (b && b.doorbell.claimed.size > 0) {
			const dm = (event.message ?? {}) as {
				role?: unknown;
				customType?: unknown;
				content?: unknown;
				details?: { attempt?: unknown; generation?: unknown };
			};
			// Real pi builds a custom message with customType, content, display
			// and details at the top level of the message, with the custom role
			// (pi core agent-session.ts, sendCustomMessage); only that shape is
			// acceptance.
			if (
				dm.customType === "amq-doorbell" &&
				dm.role === "custom"
			) {
				for (const [attempt] of b.doorbell.claimed) {
					if (
						dm.details?.attempt === attempt &&
						dm.details?.generation === b.generation &&
						dm.content === DOORBELL_LINE
					) {
						b.doorbell.claimed.delete(attempt);
						b.doorbell.done.add(attempt);
						publishDoorbellResult(b.dir, attempt, { accepted: true });
						return;
					}
				}
				// A doorbell-shaped message is never a request's user message.
				return;
			}
		}
		const f = b?.inflight;
		if (!b || !f || f.terminal) return;
		const msg = event.message as { role?: string; content?: unknown };
		if (msg.role !== "user") return;
		const own = !f.tagged && textOf(msg.content) === handoffText(f);
		if (own) {
			f.tagged = true;
			b.requestUser = { ref: f.ref, text: f.text };
			// A granted request's text ends with its arm line, so this
			// match is the start of the message that carries the arm.
			const a = f.arm ? arms.get(f.arm) : undefined;
			if (a) {
				a.tagged = true;
				a.owner = pi;
			}
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

	pi.on("agent_settled", (event) => {
		const b = bridge;
		const f = active();
		// A settle before the request started belongs to other work. It is
		// never the request's outcome; the start watchdog in scan closes a
		// request that never starts.
		if (!b || !f || !f.started) return;
		if (event.aborted || f.outcome === "aborted") {
			settle(b, { event: "cancelled" });
		} else if (f.outcome === "error") {
			settle(b, { event: "failed", error: f.lastError || "pi run ended with an error" });
		} else {
			settle(b, { event: "completed", text: f.lastText });
		}
		if (!b.inflight) scanSafely(pi, b);
	});

	// openDialog is a dialog seam for a prompt the extension owns, with the
	// rules of the Claude permission hook: it writes the request, then waits
	// for the first of the owner's answer (which the Buzz agent verified and
	// wrote), the signal, or a resolution written elsewhere. The first create
	// of resolved/<id>.json decides; an answer is returned only after this
	// seam won that create.
	const openDialog = (d: AmqDialog): Promise<AmqDialogAnswer | undefined> => {
		let open: { dir: string; id: string; options: DialogOption[] } | null = null;
		try {
			if (d.signal?.aborted || d.options.length === 0 || !serving()) return Promise.resolve(undefined);
			open = openOwnDialog(who(), d, bridge?.sessionId ?? "");
		} catch (err) {
			report(`amq-bridge: the Buzz dialog was not opened: ${String(err)}`);
		}
		if (!open) return Promise.resolve(undefined);
		const { dir, id, options: opts } = open;
		ownDialogs++;
		closeRemoteDialog(); // an option-less copy opened before this dialog
		return new Promise((resolve) => {
			let settled = false;
			let timer: ReturnType<typeof setTimeout> | null = null;
			let wait = POLL_MS;
			const finish = (answer: AmqDialogAnswer | undefined) => {
				if (settled) return;
				settled = true;
				if (timer) clearTimeout(timer);
				d.signal?.removeEventListener("abort", onAbort);
				ownDialogs--;
				resolve(answer);
			};
			// claim writes the resolution; a lost claim follows the winner.
			const claim = (rec: Record<string, string>, won: AmqDialogAnswer | undefined) => {
				try {
					finish(claimResolved(dir, id, { id, ...rec, at: new Date().toISOString() }) ? won : winnerOf(dir, id, opts));
				} catch (err) {
					report(`amq-bridge: the Buzz dialog was not closed: ${String(err)}`);
					finish(undefined);
				}
			};
			const onAbort = () => claim({ outcome: "terminal", by: "terminal" }, undefined);
			const poll = () => {
				timer = null;
				if (settled) return;
				const res = readDialogFile(dir, "resolved", id);
				if (res) return finish(winnerOf(dir, id, opts));
				const ans = readDialogFile(dir, "answers", id) as { option?: unknown } | null;
				if (ans) {
					const n = ans.option;
					const opt = typeof n === "number" ? opts.find((o) => o.n === n) : undefined;
					if (opt) return claim({ outcome: opt.deny ? "denied" : "allowed", label: opt.label, by: "buzz" }, { by: "buzz", index: opt.n - 1 });
					// The answer fits no option: reject it, so the owner can
					// answer again, and keep waiting.
					try {
						fs.unlinkSync(path.join(dir, "answers", `${id}.json`));
					} catch {
						// Already gone.
					}
				}
				wait = Math.min(2 * wait, MAX_DIALOG_POLL_MS);
				timer = setTimeout(poll, wait);
				timer.unref();
			};
			d.signal?.addEventListener("abort", onAbort, { once: true });
			timer = setTimeout(poll, wait);
			timer.unref();
		});
	};
	const report = (message: string) => (bridge ? notify(bridge.ctx, message) : console.error(message));
	return { openDialog };
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

function bridgeDir(identity: AmqIdentity): string {
	return path.join(identity.root, "agents", identity.handle, "extensions", "pi-bridge");
}

// start starts the bridge and takes the mailbox by writing liveness, or
// returns null when the bridge cannot run. displaced is called when another
// generation's liveness later replaces this bridge's.
function start(pi: ExtensionAPI, ctx: ExtensionContext, options: AmqBridgeOptions, identity: AmqIdentity, ownerNotice: { shown: boolean }, displaced: (b: Bridge) => void): Bridge | null {
	const dir = bridgeDir(identity);
	try {
		for (const sub of ["requests", "receipts", "events", "doorbell"]) {
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
	const generation = randomUUID();
	const b: Bridge = {
		dir,
		identity,
		deliver: options.deliver ?? null,
		upgrade: options.upgrade ?? "",
		ctx,
		generation,
		surface: ctx.mode,
		timers: [],
		handled: new Set(),
		pending: new Map(),
		listedMtime: -1,
		listedAt: 0,
		cancelsOpen: new Set(),
		cancelsDone: new Set(),
		cancelListedMtime: -1,
		cancelListedAt: 0,
		refusals: new Map(),
		inflight: null,
		orphans: null,
		ownerNotice,
		displaced,
		orphanRetry: new Set(),
		revisionNoticeShown: false,
		doorbell: { claimed: new Map(), done: new Set() },
		sessionId: sessionIdOf(ctx),
		turn: "",
		turns: 0,
		seq: 0,
		run: "",
		runs: 0,
		runOutcome: "",
		runStartAt: -1,
		requestUser: null,
	};
	writeLiveness(b, true);
	closeOrphanedReceipts(b);
	const beat = setInterval(() => owns(b) && writeLiveness(b, true), HEARTBEAT_MS);
	const poll = setInterval(() => scanSafely(pi, b), POLL_MS);
	beat.unref();
	poll.unref();
	b.timers.push(beat, poll);
	return b;
}

function retire(b: Bridge): void {
	for (const t of b.timers.splice(0)) clearInterval(t);
}

// holderOf reads the liveness record of the bridge that holds the mailbox:
// its pid, whether it is older than OWNER_STALE_MS, and a key that changes
// on every write. It is null when the record is absent, unreadable, or not
// live; such a mailbox is free.
function holderOf(dir: string): { pid: unknown; stale: boolean; key: string } | null {
	const file = path.join(dir, "bridge.liveness");
	let mtime: number;
	let text: string;
	try {
		mtime = fs.statSync(file).mtimeMs;
		text = fs.readFileSync(file, "utf8");
	} catch (err) {
		// Only a missing record frees the mailbox; a record that cannot be
		// read now may be a live holder's.
		return (err as NodeJS.ErrnoException).code === "ENOENT" ? null : { pid: undefined, stale: false, key: "" };
	}
	// A clock step can put the mtime ahead of now; one more than
	// OWNER_STALE_MS ahead is stale too.
	const age = Date.now() - mtime;
	const stale = age >= OWNER_STALE_MS || -age > OWNER_STALE_MS;
	let rec: { protocol?: unknown; live?: unknown; pid?: unknown };
	try {
		rec = JSON.parse(text);
	} catch {
		return { pid: undefined, stale, key: `${mtime}:${text}` };
	}
	if (rec?.protocol !== PROTOCOL || rec.live !== true) return null;
	return { pid: rec.pid, stale, key: `${mtime}:${text}` };
}

// livenessOf is the generation and pid the liveness record names, or null
// when it cannot be read.
function livenessOf(dir: string): { generation: unknown; pid: unknown } | null {
	try {
		const rec = JSON.parse(fs.readFileSync(path.join(dir, "bridge.liveness"), "utf8"));
		return { generation: rec?.session_generation, pid: rec?.pid };
	} catch {
		return null;
	}
}

// owns reports whether the liveness record still names this bridge's
// generation. When two bridges start at once the last liveness write wins;
// a bridge that reads another generation retires at once, tells its pi
// once, and hands its pi back to waiting, so it never writes over the
// owner's liveness or answers its requests. A record that cannot be read
// leaves this bridge the owner.
function owns(b: Bridge): boolean {
	const rec = livenessOf(b.dir);
	if (!rec || rec.generation === b.generation) return true;
	retire(b);
	if (!b.ownerNotice.shown) {
		b.ownerNotice.shown = true;
		notify(b.ctx, `amq-bridge: this pi no longer serves Buzz requests: another pi bridge (pid ${String(rec.pid)}) took over its AMQ mailbox.`);
	}
	b.displaced(b);
	return false;
}

function stop(b: Bridge | null, reason: string): void {
	if (!b) return;
	retire(b);
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
	// Only the owner marks the mailbox offline.
	const rec = livenessOf(b.dir);
	if (!rec || rec.generation === b.generation) writeLiveness(b, false);
}

// scanSafely is scan whose failure waits for the next poll: a transient
// filesystem error is retried then.
function scanSafely(pi: ExtensionAPI, b: Bridge): void {
	try {
		if (owns(b)) {
			scan(pi, b);
			scanDoorbell(pi, b);
		}
	} catch {
		// Retried on the next poll.
	}
}

// scanDoorbell serves the wake doorbell exchange (docs/pi-bridge-protocol.md,
// doorbell): one request file per attempt under doorbell/, one exclusive
// decision file the wake and this bridge race to create. Only the winner
// acts: the wake's withdrawn removes the doorbell from this poll; a claim
// shows the fixed line as a visible custom message that starts one turn;
// a busy refusal is final for the attempt and the wake asks again. The
// claim is flushed before pi is called, the deadline and generation are
// rechecked immediately before it, and a claim never crosses a session
// boundary: shutdown drops the pending claims and a claimed attempt without
// acceptance waits for the wake's deadline, like any claim the bridge lost.
function scanDoorbell(pi: ExtensionAPI, b: Bridge): void {
	const dir = path.join(b.dir, "doorbell");
	let names: string[];
	try {
		names = fs.readdirSync(dir);
	} catch {
		return;
	}
	if (b.doorbell.done.size > DOORBELL_DONE_MAX) b.doorbell.done.clear();
	for (const name of names.sort()) {
		if (!name.endsWith(".json") || name.endsWith(".decision.json") || name.endsWith(".result.json")) continue;
		const attempt = name.slice(0, -5);
		if (!DOORBELL_ATTEMPT.test(attempt) || b.doorbell.claimed.has(attempt) || b.doorbell.done.has(attempt)) continue;
		let rec: { protocol?: unknown; op?: unknown; attempt?: unknown; generation?: unknown; deadline?: unknown };
		try {
			rec = JSON.parse(fs.readFileSync(path.join(dir, name), "utf8"));
		} catch {
			continue; // a partial or foreign file is never evidence
		}
		if (rec.protocol !== PROTOCOL || rec.op !== "doorbell" || rec.attempt !== attempt) continue;
		// The request names the pi process it is for; a request whose target
		// pid is not this process is never served.
		if (rec.pid !== GUARD_PID) {
			b.doorbell.done.add(attempt);
			continue;
		}
		const deadline = typeof rec.deadline === "string" ? Date.parse(rec.deadline) : NaN;
		if (!Number.isFinite(deadline) || Date.now() > deadline || rec.generation !== b.generation) {
			b.doorbell.done.add(attempt); // expired or not addressed to this generation; the wake owns the file
			continue;
		}
		const decision = readDoorbellDecision(dir, attempt);
		if (decision) {
			if (decision.decision === "claimed" && decision.generation === b.generation) {
				b.doorbell.claimed.set(attempt, Date.now()); // claimed by an earlier poll of this runtime
			} else {
				b.doorbell.done.add(attempt); // withdrawn, refused, or another generation's
			}
			continue;
		}
		// Busy refusal first: a busy pi gets nothing, and the refusal is
		// final for the attempt.
		const busy = busyReason(b);
		if (busy) {
			writeDoorbellDecision(b, attempt, { decision: "refused", reason: busy });
			b.doorbell.done.add(attempt);
			continue;
		}
		if (!writeDoorbellDecision(b, attempt, { decision: "claimed" })) continue; // the wake withdrew first
		b.doorbell.claimed.set(attempt, Date.now());
		try {
			pi.sendMessage(
				{ customType: "amq-doorbell", content: DOORBELL_LINE, display: true, details: { attempt, generation: b.generation } },
				{ triggerTurn: true, deliverAs: "followUp" },
			);
		} catch (err) {
			// A synchronous throw is proven non-delivery, but a decision is
			// never deleted or rewritten: the claim stays, no message_start can
			// match it, and the bridge publishes a result that says so. The
			// wake keeps its presented view; the reminder cadence covers a
			// doorbell that did not start a turn.
			b.doorbell.claimed.delete(attempt);
			b.doorbell.done.add(attempt);
			publishDoorbellResult(b.dir, attempt, { accepted: false, reason: `sendMessage: ${String(err)}` });
		}
	}
}

// readDoorbellDecision returns the decision file's body, or null when it is
// absent or unreadable; an unreadable file is never a win for either side.
function readDoorbellDecision(dir: string, attempt: string): { decision?: unknown; generation?: unknown } | null {
	try {
		return JSON.parse(fs.readFileSync(path.join(dir, `${attempt}.decision.json`), "utf8"));
	} catch {
		return null;
	}
}

// writeDoorbellDecision races the wake for the attempt's single decision
// file with create-new link semantics and reports whether this call won.
function writeDoorbellDecision(b: Bridge, attempt: string, fields: { decision: string; reason?: string }): boolean {
	const dir = path.join(b.dir, "doorbell");
	const body = JSON.stringify({
		protocol: PROTOCOL,
		attempt,
		generation: b.generation,
		pid: process.pid,
		at: new Date().toISOString(),
		...fields,
	});
	try {
		linkNew(dir, path.join(dir, `${attempt}.decision.json`), body);
		syncDir(dir);
		return true;
	} catch (err) {
		// EEXIST is the wake's win or an earlier decision; anything else is
		// a failed write, recorded best-effort as a failed decision.
		if ((err as NodeJS.ErrnoException).code !== "EEXIST") {
			try {
				linkNew(dir, path.join(dir, `${attempt}.decision.json`), JSON.stringify({ protocol: PROTOCOL, attempt, generation: b.generation, pid: process.pid, at: new Date().toISOString(), decision: "failed", reason: String(err) }));
			} catch {
				// Unwritable; the wake's deadline governs.
			}
		}
		return false;
	}
}

// publishDoorbellResult records the evidence for one attempt: accepted is
// the matching message_start of the addressed generation (the flat custom
// message real pi emits); accepted false with a reason records a send that
// threw after the claim. The wake reads it for its attempt ledger; control
// flow never waits for it.
function publishDoorbellResult(dir: string, attempt: string, fields: { accepted: boolean; reason?: string }): void {
	const out = path.join(dir, "doorbell");
	const body = JSON.stringify({
		protocol: PROTOCOL,
		accepted: fields.accepted,
		attempt,
		reason: fields.reason,
		at: new Date().toISOString(),
	});
	try {
		linkNew(out, path.join(out, `${attempt}.result.json`), body);
		syncDir(out);
	} catch {
		// Best effort: the wake's deadline governs when evidence is unreadable.
	}
}

// discover adds the requests listed in requests/ that are not handled yet
// to b.pending. requests/ retains every request, so it is listed only when
// it changed (see RECONCILE_MS): an idle poll's cost does not grow with the
// history.
function discover(b: Bridge): void {
	const reqDir = path.join(b.dir, "requests");
	const now = Date.now();
	let names: string[];
	let mtime: number;
	try {
		mtime = fs.statSync(reqDir).mtimeMs;
		if (mtime === b.listedMtime && b.listedAt - mtime > COARSE_MTIME_MS && now - b.listedAt < RECONCILE_MS) return;
		names = fs.readdirSync(reqDir);
	} catch {
		return;
	}
	// The first listing finds only receipts that orphan recovery listed at
	// start; a later one can find a receipt another generation wrote since.
	const later = b.listedMtime !== -1;
	b.listedMtime = mtime;
	b.listedAt = now;
	const listed = new Set<string>();
	for (const name of names) {
		const m = REQUEST_FILE.exec(name);
		if (!m) continue;
		const ref = m[1];
		listed.add(ref);
		if (b.handled.has(ref) || b.pending.has(ref)) continue;
		const receipt = exists(path.join(b.dir, "receipts", `${ref}.json`));
		if (receipt || hasTerminal(b.dir, ref)) {
			if (receipt && later) b.orphans?.add(ref);
			markHandled(b, ref);
			continue;
		}
		try {
			b.pending.set(ref, fs.statSync(path.join(reqDir, name)).mtimeMs);
		} catch {
			// Vanished between readdir and stat: nothing to answer.
		}
	}
	// handled holds only refs in the latest listing.
	for (const ref of b.handled) if (!listed.has(ref)) b.handled.delete(ref);
}

// applyCancels applies the cancel files in cancels/ (docs/pi-bridge-protocol.md,
// bridge revision 5). A cancel names one request: one without a receipt or a
// terminal event is refused cancelled and never runs; the request in flight
// is aborted only once it has started, so the abort never reaches a local
// turn; any other request is left alone. A cancel that is not applied yet
// stays open and is tried again on the next poll.
function applyCancels(b: Bridge): void {
	const dir = path.join(b.dir, "cancels");
	const now = Date.now();
	let names: string[];
	let mtime: number;
	try {
		mtime = fs.statSync(dir).mtimeMs;
		const unchanged = mtime === b.cancelListedMtime && b.cancelListedAt - mtime > COARSE_MTIME_MS && now - b.cancelListedAt < RECONCILE_MS;
		if (unchanged && b.cancelsOpen.size === 0) return;
		names = unchanged ? [] : fs.readdirSync(dir);
	} catch {
		return;
	}
	if (names.length > 0 || b.cancelListedMtime !== mtime) {
		b.cancelListedMtime = mtime;
		b.cancelListedAt = now;
	}
	const listed = new Set<string>();
	for (const name of names) {
		const m = REQUEST_FILE.exec(name);
		if (!m) continue;
		listed.add(m[1]);
		if (!b.cancelsDone.has(m[1])) b.cancelsOpen.add(m[1]);
	}
	if (names.length > 0) for (const ref of b.cancelsDone) if (!listed.has(ref)) b.cancelsDone.delete(ref);
	for (const ref of [...b.cancelsOpen]) {
		if (cancelApplied(b, ref)) {
			b.cancelsOpen.delete(ref);
			b.cancelsDone.add(ref);
		}
	}
}

// cancelApplied reports whether the cancel for ref needs no more work.
function cancelApplied(b: Bridge, ref: string): boolean {
	let rec: Record<string, unknown>;
	try {
		rec = JSON.parse(fs.readFileSync(path.join(b.dir, "cancels", `${ref}.json`), "utf8"));
	} catch {
		return true;
	}
	if (rec === null || typeof rec !== "object" || rec.protocol !== PROTOCOL || rec.ref !== ref) return true;
	const f = b.inflight;
	if (f?.ref === ref) {
		if (f.terminal) return true;
		if (!f.started) return false;
		b.ctx.abort();
		return true;
	}
	if (b.refusals.has(ref) || exists(path.join(b.dir, "receipts", `${ref}.json`)) || hasTerminal(b.dir, ref)) return true;
	if (!b.pending.has(ref)) return !exists(path.join(b.dir, "requests", `${ref}.json`)) || b.handled.has(ref);
	const fields: EventFields = { event: "refused", reason: "cancelled", error: "the request was cancelled before it was delivered" };
	if (appendEvent(b.dir, ref, fields)) markHandled(b, ref);
	else b.refusals.set(ref, fields);
	return true;
}

// markHandled records that ref needs no more work.
function markHandled(b: Bridge, ref: string): void {
	b.pending.delete(ref);
	b.handled.add(ref);
}

// scan examines every pending request in arrival order. Refusals are
// written at once; at most one request is delivered and in flight, and only
// into an idle pi. A request that arrives while pi or the bridge is busy is
// refused busy, never queued. A pending request stays pending until it is
// handled, so a failed claim or refusal append is retried on the next poll;
// a known refusal whose append failed stays pending even when its line is
// visible, because the line may not be durable.
function scan(pi: ExtensionAPI, b: Bridge): void {
	discover(b);
	applyCancels(b);
	const pending = [...b.pending].sort(([ra, ma], [rb, mb]) => ma - mb || (ra < rb ? -1 : ra > rb ? 1 : 0));

	if (!b.orphans || b.orphans.size > 0) closeOrphanedReceipts(b);

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

	for (const [ref] of pending) {
		const decided = b.refusals.get(ref);
		if (decided) {
			if (appendEvent(b.dir, ref, decided)) {
				b.refusals.delete(ref);
				markHandled(b, ref);
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
			if (appendEvent(b.dir, ref, fields)) markHandled(b, ref);
			else b.refusals.set(ref, fields);
			continue;
		}
		const claim = claimReceipt(b, ref);
		if (claim === "error") continue; // retried on the next poll
		markHandled(b, ref);
		if (claim === "taken") continue; // another bridge process already claimed it
		// A granted request's text ends with the line of its own arm, so its
		// own user message is the only one that matches it.
		const arm = verdict.granted ? randomBytes(16).toString("hex") : "";
		const f: Inflight = {
			ref,
			text: verdict.text,
			deliveredAt: Date.now(),
			started: false,
			tagged: false,
			arm,
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
		// The guard is armed before the hand-off, so the first tool call of
		// the granted run already finds it.
		if (arm && !openArm(b.dir, arm, ref)) {
			settle(b, { event: "failed", error: "the guard of a granted task could not be armed; nothing was sent to pi" });
			continue;
		}
		try {
			pi.sendUserMessage(handoffText(f), { deliverAs: "followUp" });
		} catch (err) {
			if (arm) endArm(arm); // proven non-delivery
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

type Verdict = { refuse: string; error: string; text: string; notAfter: string; granted: boolean; origin?: { carrier: string; name: string } };

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
	const refuse = (reason: string, error: string): Verdict => ({ refuse: reason, error, text: "", notAfter: "", granted: false });
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
	const { granted = false } = req;
	if (typeof granted !== "boolean") return refuse("invalid", "granted is not a boolean");
	if (granted) {
		// A granted request runs only where the guard asks the person at
		// this pi's terminal: the TUI, with this extension's own hand-off. A
		// bridge that cannot ask publishes no gate, so this is a second fence.
		if (rev < GRANT_REVISION) {
			return refuse("revision", `a granted request needs bridge_revision ${GRANT_REVISION} or later, and this one needs ${rev}: upgrade amq-remote to the release that matches this extension and restart it`);
		}
		if (modeOf(b.ctx) !== "tui" || b.deliver) return refuse("unsupported", "a granted task needs the pi terminal UI");
	}
	return { refuse: "", error: "", text: req.text, notAfter: req.not_after as string, granted, origin: originOf(req.origin) };
}

// modeOf is pi's run mode now, "" when the context cannot tell.
function modeOf(ctx: ExtensionContext): string {
	try {
		return ctx.mode;
	} catch {
		return "";
	}
}

// gates reports whether this bridge can guard a granted task: pi runs in
// its TUI and the bridge hands requests to pi with its own sendUserMessage.
function gates(b: Bridge): boolean {
	return b.surface === "tui" && !b.deliver;
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
	try {
		linkNew(dir, path.join(dir, `${ref}.json`), body);
	} catch (err) {
		return (err as NodeJS.ErrnoException).code === "EEXIST" ? "taken" : "error";
	}
	// The link published the receipt; a failed directory sync cannot take it
	// back, so the request is dispatched with its durability uncertain, as
	// the adapter's own publication does.
	syncDir(dir);
	return "claimed";
}

// linkNew publishes body at file create-new: a complete, fsynced temporary
// file in dir is hard-linked into place, so a reader sees the whole body or
// no file. It throws EEXIST when file exists.
function linkNew(dir: string, file: string, body: string): void {
	const tmp = writeTemp(dir, body);
	try {
		fs.linkSync(tmp, file);
	} finally {
		fs.rmSync(tmp, { force: true });
	}
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
		// The wake doorbell exchange exists in this revision; a reader that
		// wants it still checks surface and the owner process itself.
		doorbell: true,
		gate: gates(b) || undefined,
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
// event is `uncertain`. A request whose last event (or receipt) is younger
// than OWNER_STALE_MS may still run in a stalled owner and is left open
// until it is older. receipts/ is listed once; later polls look only at the
// receipts still open.
function closeOrphanedReceipts(b: Bridge): void {
	if (!b.orphans) {
		try {
			b.orphans = new Set(fs.readdirSync(path.join(b.dir, "receipts")).flatMap((name) => REQUEST_FILE.exec(name)?.[1] ?? []));
		} catch {
			return;
		}
	}
	for (const ref of [...b.orphans]) {
		try {
			const rc = JSON.parse(fs.readFileSync(path.join(b.dir, "receipts", `${ref}.json`), "utf8"));
			const retry = b.orphanRetry.has(ref);
			if (rc.session_generation === b.generation || (!retry && hasTerminal(b.dir, ref))) {
				b.orphans.delete(ref);
				continue;
			}
			if (!retry && Date.now() - lastEventAt(b.dir, ref) < OWNER_STALE_MS) continue;
			const ok = appendEvent(b.dir, ref, {
				event: "uncertain",
				error: "the pi bridge restarted before the request settled; the outcome is unknown",
			});
			if (ok) {
				b.orphanRetry.delete(ref);
				b.orphans.delete(ref);
			} else {
				b.orphanRetry.add(ref);
			}
		} catch {
			// An unreadable receipt is not recovered yet; scan reads it again.
		}
	}
}

// lastEventAt is when ref's last event was written, or its receipt when it
// has no event.
function lastEventAt(dir: string, ref: string): number {
	try {
		return fs.statSync(path.join(dir, "events", `${ref}.jsonl`)).mtimeMs;
	} catch {
		return fs.statSync(path.join(dir, "receipts", `${ref}.json`)).mtimeMs;
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
// not evidence. A line written into an empty file is durable only once the
// directory entry is synced too, because the file may be new.
function appendLine(file: string, line: string, maxSize: number): boolean {
	let fd: number | undefined;
	try {
		fd = fs.openSync(file, "a+", 0o600);
		let size = fs.fstatSync(fd).size;
		const empty = size === 0;
		if (maxSize > 0 && size > maxSize) {
			fs.ftruncateSync(fd, 0);
			size = 0;
		}
		const last = Buffer.alloc(1);
		if (size > 0 && fs.readSync(fd, last, 0, 1, size - 1) === 1 && last[0] !== 0x0a) writeAll(fd, "\n");
		writeAll(fd, line);
		fs.fsyncSync(fd);
		return !empty || syncDir(path.dirname(file));
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
		b.requestUser = null;
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
		const request = msg.role === "user" ? b.requestUser : null;
		if (msg.role === "user") b.requestUser = null;
		const text = truncateUtf8(request?.text ?? textOf(msg.content), MAX_ACTIVITY_TEXT);
		if (text) appendActivity(b, { kind: msg.role, id: request?.ref, text });
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

// writeTemp writes body whole to a new fsynced temporary file in dir and
// returns its path; on failure it leaves no file.
function writeTemp(dir: string, body: string): string {
	const tmp = path.join(dir, `.tmp-${process.pid}-${randomUUID()}`);
	const fd = fs.openSync(tmp, "wx", 0o600);
	let whole = false;
	try {
		writeAll(fd, body);
		fs.fsyncSync(fd);
		whole = true;
	} finally {
		fs.closeSync(fd);
		if (!whole) fs.rmSync(tmp, { force: true });
	}
	return tmp;
}

// syncDir fsyncs a directory and reports whether its entries are durable. A
// platform that does not support directory fsync (EINVAL, ENOTSUP) counts
// as durable; any other failure does not.
function syncDir(dir: string): boolean {
	try {
		const fd = fs.openSync(dir, "r");
		try {
			fs.fsyncSync(fd);
		} finally {
			fs.closeSync(fd);
		}
		return true;
	} catch (err) {
		const code = (err as NodeJS.ErrnoException).code;
		return code === "EINVAL" || code === "ENOTSUP";
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
// without fsync: the marker is advisory, and the prompt is rendering. It
// returns the bytes it installed, or "" when it installed none.
function openWakeDialog(id: AmqIdentity | undefined, event: string, kind: string): string {
	const dir = wakeAgentDir(id);
	if (!dir) return "";
	const marker = {
		schema: 1,
		event,
		harness: "pi",
		kind,
		opened_at: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
	};
	const temp = path.join(dir, `.wake.dialog.tmp.${process.pid}.${randomUUID()}`);
	const text = `${JSON.stringify(marker)}\n`;
	try {
		fs.writeFileSync(temp, text, { flag: "wx", mode: 0o600 });
		fs.renameSync(temp, path.join(dir, WAKE_DIALOG));
		return text;
	} catch {
		try {
			fs.unlinkSync(temp);
		} catch {
			// The temp file was never created or is already gone.
		}
		return "";
	}
}

// closeWakeDialog removes the marker only when this extension wrote it, so
// a marker another writer installed keeps holding. With only, it removes the
// marker only while the file holds exactly those bytes.
function closeWakeDialog(id: AmqIdentity | undefined, only = ""): void {
	const dir = wakeAgentDir(id);
	if (!dir) return;
	const file = path.join(dir, WAKE_DIALOG);
	try {
		const text = fs.readFileSync(file, "utf8");
		if (only) {
			if (text === only) fs.unlinkSync(file);
			return;
		}
		const marker = JSON.parse(text) as { event?: string; harness?: string };
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
	try {
		claimResolved(open.dir, open.id, { id: open.id, outcome: "terminal", by: "terminal", at: new Date().toISOString() });
	} catch {
		// The store is gone.
	}
}

// claimResolved writes resolved/<id>.json whole by a hard link, so the first
// writer wins. It returns false when the dialog was resolved first and
// throws on any other failure.
function claimResolved(dir: string, id: string, rec: Record<string, string>): boolean {
	const sub = path.join(dir, "resolved");
	const temp = writeTemp(sub, JSON.stringify(rec));
	try {
		fs.linkSync(temp, path.join(sub, `${id}.json`));
		syncDir(sub);
		return true;
	} catch (err) {
		if ((err as NodeJS.ErrnoException).code === "EEXIST") return false;
		throw err;
	} finally {
		fs.rmSync(temp, { force: true });
	}
}

// An extension's own dialog. Its option values are their numbers; the text
// is bounded so the request stays readable by the Go reader's size bound.
type DialogOption = { n: number; label: string; value: string; deny?: boolean };
const MAX_DIALOG_POLL_MS = 2000;
const MAX_DIALOG_TEXT = 64 * 1024;

// openOwnDialog writes a request with d's options for the mailbox binding
// that serves this pi. It returns null when no binding does.
function openOwnDialog(identity: AmqIdentity | undefined, d: AmqDialog, session: string): { dir: string; id: string; options: DialogOption[] } | null {
	const store = remoteStore();
	const name = remoteBinding(store, identity);
	if (!name) return null;
	const dir = path.join(store, "dialogs", name);
	const id = `d-${randomUUID().replace(/-/g, "")}`;
	const options: DialogOption[] = d.options.map((o, i) => ({ n: i + 1, label: o.label, value: String(i + 1), ...(o.deny ? { deny: true } : {}) }));
	const text = truncateUtf8(d.body ? `${d.title}\n${d.body}` : d.title, MAX_DIALOG_TEXT);
	const request = { id, harness: "pi", session, tool: "", text, options, opened_at: new Date().toISOString(), pid: process.pid };
	for (const sub of ["requests", "answers", "resolved"]) fs.mkdirSync(path.join(dir, sub), { recursive: true, mode: 0o700 });
	const temp = writeTemp(path.join(dir, "requests"), JSON.stringify(request));
	try {
		fs.renameSync(temp, path.join(dir, "requests", `${id}.json`));
	} catch (err) {
		fs.rmSync(temp, { force: true });
		throw err;
	}
	syncDir(path.join(dir, "requests"));
	return { dir, id, options };
}

// readDialogFile reads <dir>/<sub>/<id>.json; null when it is absent or not
// readable yet, so the next poll reads it again.
function readDialogFile(dir: string, sub: string, id: string): Record<string, unknown> | null {
	try {
		const v = JSON.parse(fs.readFileSync(path.join(dir, sub, `${id}.json`), "utf8")) as unknown;
		return v && typeof v === "object" ? (v as Record<string, unknown>) : null;
	} catch {
		return null;
	}
}

// winnerOf is the answer of a dialog that is resolved: the owner's option
// when Buzz resolved it, else undefined (the terminal, or expiry).
function winnerOf(dir: string, id: string, options: DialogOption[]): AmqDialogAnswer | undefined {
	if (readDialogFile(dir, "resolved", id)?.by !== "buzz") return undefined;
	const n = readDialogFile(dir, "answers", id)?.option;
	const opt = options.find((o) => o.n === n);
	return opt ? { by: "buzz", index: opt.n - 1 } : undefined;
}

function notify(ctx: ExtensionContext, message: string): void {
	try {
		ctx.ui.notify(message, "info");
	} catch {
		console.error(message);
	}
}
