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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideSeatDoorbell(tc.live, tc.channel, tc.attended); got != tc.want {
				t.Fatalf("doorbell = %v, want %v", got, tc.want)
			}
		})
	}
}
