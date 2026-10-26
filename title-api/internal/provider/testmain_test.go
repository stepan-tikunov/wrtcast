package provider

import (
	"os"
	"testing"
	"time"
)

// Tests never reach the ad network or Google's ad host, and the player
// page runs its scripts at once.
func TestMain(m *testing.M) {
	adListURL, adBlockProbeURL = "", ""
	defaultPageTiming.scriptRun = [2]time.Duration{}
	defaultPageTiming.edgeHash = 50 * time.Millisecond
	os.Exit(m.Run())
}
