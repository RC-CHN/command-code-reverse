package fingerprint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSeedProfileIndependentOfHostAndState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Resolve("golden-device-seed", path+"/state.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "Pacific/Auckland")
	t.Setenv("KUBERNETES_SERVICE_HOST", "different-node")
	b, err := Resolve("golden-device-seed", path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("host environment or state changed a seeded profile")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "untouched" {
		t.Fatal("seed mode wrote a state file")
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	const want = "b7e0093aa846f8ab8b71fc26f5049afbc8b1303398e434ac28a93e63fce2c6cc"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != want {
		t.Fatalf("profile v1 drifted: got %s, want %s; do not silently change existing seeded devices", got, want)
	}
}

func TestSeedProfileDiversityAndHardwareConsistency(t *testing.T) {
	models, sizes, kernels, zones, macCounts := map[string]bool{}, map[int]bool{}, map[string]bool{}, map[string]bool{}, map[int]bool{}
	for i := range 64 {
		fp := deriveFromSeed(string(rune('a' + i)))
		c := fp.Components
		models[c.CPUModel] = true
		sizes[c.CPUCount] = true
		kernels[c.OSRelease] = true
		zones[c.Timezone] = true
		macCounts[len(c.MACHashes)] = true
		if c.Platform != "linux" || c.CPUCount <= 0 || c.MemGiB < 2*c.CPUCount || c.Runtime != "cli" {
			t.Fatalf("inconsistent hardware: %+v", c)
		}
		found := false
		for _, p := range hardwareProfilesV1 {
			if p.model == c.CPUModel && p.arch == c.Arch {
				found = true
			}
		}
		if !found {
			t.Fatal("CPU model and architecture disagree")
		}
	}
	if len(models) < 2 || len(sizes) < 2 || len(kernels) < 2 || len(zones) < 2 || len(macCounts) < 2 {
		t.Fatal("profiles collapsed to a shared template")
	}
}

func TestLiveHardwarePreservedAndGapsFilled(t *testing.T) {
	real := rawSignals{machineID: "real", cpuModel: "real CPU", cpuCount: 24, memGiB: 96, platform: "linux", arch: "x64", osRelease: "real kernel", timezone: "real zone", container: true}
	if got := completeHardware(real, "seed"); !reflect.DeepEqual(got, real) {
		t.Fatal("real hardware was overwritten")
	}
	missing := real
	missing.cpuModel = ""
	missing.memGiB = 0
	a, b := completeHardware(missing, "seed"), completeHardware(missing, "seed")
	if !reflect.DeepEqual(a, b) || a.cpuCount != real.cpuCount || a.cpuModel == "" || a.memGiB <= 0 || a.machineID != "real" || a.arch != "x64" {
		t.Fatal("hardware fallback is invalid or unstable")
	}
}
