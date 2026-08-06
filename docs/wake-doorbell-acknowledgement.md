# Wake doorbell acknowledgement policy

Status: binding contract for owner-bound wake input retries.

The normative words MUST, MUST NOT, SHOULD, and MAY describe the behavior
implemented by `amq wake`.

## Problem

An external `--inject-via` process exposes two different delivery facts:

1. exit zero proves that the injector accepted the complete doorbell; and
2. removal of a message from `inbox/new` proves durable consumer progress.

Those facts are not interchangeable. Exit zero does not prove that an agent
executed the doorbell, while an unchanged inbox does not prove that a second
terminal prompt is useful. Integrations that already own receipt-based
recovery need the first fact to suppress duplicate terminal turns; standalone
AMQ keeps the second fact as its safer default.

## CLI contract

`amq wake` accepts:

```text
--retry-until drained|injected
```

- `drained` is the default and preserves the existing retry ladder.
- `injected` requires `--inject-via`. A zero exit status acknowledges the
  current physical inbox cohort and suppresses further input retries for that
  unchanged cohort.
- A nonzero exit, timeout, authority refusal, partial write, or output-only
  fallback MUST NOT acknowledge injection. Existing bounded retry and recovery
  semantics continue to apply.

The requested value is part of the retained wake target identity. The optional
`retry_until` target/state field omits the default `drained` value for byte and
digest compatibility with existing targets. A missing field MUST be read as
`drained`. Repair, reuse, and retirement MUST compare the normalized policy;
an `injected` wake cannot be silently reused or retired as a `drained` wake.

## State machine

Owner-bound doorbell state adds an `announced` phase:

```text
idle -> retrying -> announced
          |             |
          | failure     | new physical message
          v             v
       retrying      retrying
```

- `retrying` retains the current 5s exponential input ladder and slower
  attention ladder.
- Successful input delivery under `--retry-until injected` transitions to
  `announced`, retains the physical cohort snapshot, and owns no retry
  deadline.
- Re-scanning the same cohort, including after maintenance ticks, MUST NOT
  produce another input attempt.
- Removing already announced members rebases the snapshot without reannouncing
  the remaining members.
- Adding a new physical inbox member starts one fresh attempt covering the
  expanded cohort. A successful attempt announces the expanded cohort.
- An empty inbox resets the state to `idle`.

The announced state is process-local. A wake process replacement MAY produce
one fresh doorbell for an unread cohort; cross-process exactly-once injection
would require a separate durable injected-receipt protocol and is outside this
contract.

## Integration rule

Consumers selecting `injected` MUST retain their own durable-consumption
policy. Injector exit zero MUST NOT be reported as a drained message receipt.
Orch continues to use the original message's `drained` receipt for task
delivery status and explicit recovery.

## Acceptance criteria

1. Default `drained` mode preserves the existing 5s/10s retry behavior.
2. `injected` mode invokes a successful external injector once for an
   unchanged unread cohort, even after its former retry deadlines.
3. A new inbox member after acknowledgement produces one new injection.
4. Injector failure or output-only fallback remains retryable.
5. CLI validation rejects unknown values and `injected` without
   `--inject-via`.
6. Retained target/state round trips preserve `injected`; omitted values read
   as `drained`.
