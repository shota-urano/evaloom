package main

import (
	"testing"

	"github.com/shota-urano/evaloom/backend/internal/notification"
)

func TestPhase1RegistersOnlyMacOSNotifier(t *testing.T) {
	channels := phase1NotificationChannels("darwin", "/Users/owner", 49123)
	if len(channels) != 1 || channels[0].Name != "macos" {
		t.Fatalf("Phase 1 channels = %+v; want only macos", channels)
	}
	if _, ok := channels[0].Notifier.(*notification.MacOS); !ok {
		t.Fatalf("registered Notifier = %T; want *notification.MacOS", channels[0].Notifier)
	}
	for _, goos := range []string{"linux", "windows"} {
		if got := phase1NotificationChannels(goos, "/unused", 8080); len(got) != 0 {
			t.Fatalf("%s registered macOS helper: %+v", goos, got)
		}
	}
}
