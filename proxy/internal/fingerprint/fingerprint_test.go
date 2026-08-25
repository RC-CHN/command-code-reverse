package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildMatchesRealAlgorithm verifies the thumbmark formula against a
// hand-computed vector: sha256(ns + "\0machine\0" + "mid|aa:aa,bb:bb").
func TestBuildMatchesRealAlgorithm(t *testing.T) {
	fp := build(rawSignals{
		machineID: "mid",
		macs:      []string{"BB:BB", "aa:aa", "aa:aa"}, // unsorted, dup, mixed case
		hostname:  "ignored-host",                      // present but machineID set → excluded
		cpuModel:  "Ignored CPU",
		osUser:    "SomeUser",
	})

	h := sha256.New()
	h.Write([]byte("command-code:device-fingerprint:v1"))
	h.Write([]byte("\x00machine\x00"))
	h.Write([]byte("mid|aa:aa,bb:bb"))
	want := hex.EncodeToString(h.Sum(nil))

	if fp.Thumbmark != want {
		t.Errorf("thumbmark = %q, want %q", fp.Thumbmark, want)
	}

	// hashSignal: sha256(ns + "\0" + lower(value)).
	h2 := sha256.New()
	h2.Write([]byte("command-code:device-fingerprint:v1"))
	h2.Write([]byte("\x00"))
	h2.Write([]byte("someuser"))
	if fp.Components.OSUserHash != hex.EncodeToString(h2.Sum(nil)) {
		t.Errorf("osUserHash wrong")
	}
	if fp.Components.HostnameHash == "" {
		t.Error("hostname still hashed into components (just not into thumbmark)")
	}
	if fp.Components.Runtime != "cli" || fp.Components.CollectorVersion != 1 {
		t.Errorf("components = %+v", fp.Components)
	}
}

// TestBuildWithoutMachineID: hostname and cpuModel join the thumbmark only
// when machineId is missing.
func TestBuildWithoutMachineID(t *testing.T) {
	fp := build(rawSignals{
		machineID: "",
		macs:      []string{"aa:aa"},
		hostname:  "myhost",
		cpuModel:  "MyCPU",
	})

	h := sha256.New()
	h.Write([]byte("command-code:device-fingerprint:v1"))
	h.Write([]byte("\x00machine\x00"))
	h.Write([]byte("aa:aa|myhost|MyCPU"))
	if fp.Thumbmark != hex.EncodeToString(h.Sum(nil)) {
		t.Errorf("thumbmark = %q", fp.Thumbmark)
	}
}

// TestSeedDeterministic: same seed → identical fingerprint; different seed → different.
func TestSeedDeterministic(t *testing.T) {
	a1, err := Resolve("seed-alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := Resolve("seed-alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resolve("seed-beta", "")
	if err != nil {
		t.Fatal(err)
	}

	if a1.Thumbmark != a2.Thumbmark {
		t.Error("same seed produced different thumbmarks")
	}
	if a1.Thumbmark == b.Thumbmark {
		t.Error("different seeds produced identical thumbmarks")
	}
	if a1.Components.MachineIDHash == "" || len(a1.Components.MACHashes) != 3 {
		t.Errorf("components = %+v", a1.Components)
	}
}

// TestStateFileReplay: live collect persists; second Resolve replays the file.
func TestStateFileReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fp", "fingerprint.json")

	first, err := Resolve("", path)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not persisted: %v", err)
	}

	second, err := Resolve("", path)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if first.Thumbmark != second.Thumbmark {
		t.Error("state file not replayed faithfully")
	}

	// Seed takes priority over the state file.
	seeded, err := Resolve("seed-wins", path)
	if err != nil {
		t.Fatal(err)
	}
	if seeded.Thumbmark == first.Thumbmark {
		t.Error("seed should take priority over state file")
	}
}

func TestHashSignalEmpty(t *testing.T) {
	if hashSignal("") != "" || hashSignal("  ") != "" {
		t.Error("empty input must yield empty hash")
	}
}
