package cli

import "testing"

func TestSeatDoorbellSelectsOneRinger(t *testing.T) {
	for _, tc := range []struct {
		name     string
		live     bool
		channel  wakeChannel
		attended bool
		want     seatDoorbell
	}{
		{"attended", true, wakeChannelHook, true, doorbellNone},
		{"without a wake", false, "", false, doorbellHook},
		{"hook wake", true, wakeChannelHook, false, doorbellHook},
		{"notify-only wake", true, wakeChannelNone, false, doorbellHook},
		{"typing wake", true, wakeChannelTyped, false, doorbellTyped},
		{"unpublished wake", true, "", false, doorbellTyped},
		// Review finding H1: a notify-only wake with no recorded channel
		// typed nothing and silenced the waiter, so the seat had no ringer.
		{"notify-only wake with no record", true, publishedWakeChannel("", wakeInjectModeNone), false, doorbellHook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideSeatDoorbell(tc.live, tc.channel, tc.attended); got != tc.want {
				t.Fatalf("doorbell = %v, want %v", got, tc.want)
			}
		})
	}
}
