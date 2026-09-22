package fingerprint

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Profile v1 is frozen: changing tables, labels, or selection order would
// change existing seeded devices. It contains no host, clock, or random input.
// All seeded profiles are Linux clients, matching the inference environment.
type hardwareProfile struct {
	arch, model string
	cpus        []int
}

var hardwareProfilesV1 = []hardwareProfile{
	{"x64", "Intel(R) Xeon(R) Gold 6230 CPU @ 2.10GHz", []int{2, 4, 8, 16, 32}},
	{"x64", "Intel(R) Xeon(R) Platinum 8375C CPU @ 2.90GHz", []int{2, 4, 8, 16, 32, 64}},
	{"x64", "AMD EPYC 7R32", []int{2, 4, 8, 16, 32, 64}},
	{"x64", "AMD EPYC 7763 64-Core Processor", []int{2, 4, 8, 16, 32, 64}},
	{"arm64", "Neoverse-N1", []int{2, 4, 8, 16, 32, 64}},
	{"arm64", "Neoverse-V1", []int{2, 4, 8, 16, 32, 64}},
}

func seedBytes(seed, label string) []byte {
	h := hmac.New(sha256.New, []byte(seed))
	h.Write([]byte(label))
	return h.Sum(nil)
}

func profilePick(seed, label string, size int) int {
	return int(binary.BigEndian.Uint64(seedBytes(seed, "profile:v1:"+label)[:8]) % uint64(size))
}

func seedHex(seed, label string, length int) string {
	return hex.EncodeToString(seedBytes(seed, label))[:length]
}

func seededHardware(seed, arch string) hardwareProfile {
	var candidates []hardwareProfile
	for _, profile := range hardwareProfilesV1 {
		if arch == "" || profile.arch == arch {
			candidates = append(candidates, profile)
		}
	}
	if len(candidates) == 0 {
		// Only live collection can reach other architectures. Keep its native
		// architecture instead of advertising an incompatible x86/ARM model.
		return hardwareProfile{arch, arch + " processor", []int{2, 4, 8}}
	}
	return candidates[profilePick(seed, "hardware", len(candidates))]
}

func hardwareValues(seed string, profile hardwareProfile) (int, int) {
	cpus := profile.cpus[profilePick(seed, "cpus", len(profile.cpus))]
	// Plausible VM configurations: choose RAM together with vCPU count.
	memoryPerCPU := []int{2, 4, 8}[profilePick(seed, "memory", 3)]
	return cpus, cpus * memoryPerCPU
}

func deriveFromSeed(seed string) *Fingerprint {
	profile := seededHardware(seed, "")
	cpus, memory := hardwareValues(seed, profile)
	macs := make([]string, 1+profilePick(seed, "mac-count", 3))
	for i := range macs {
		b := seedHex(seed, fmt.Sprintf("mac:%d", i), 12)
		macs[i] = "02:" + b[0:2] + ":" + b[2:4] + ":" + b[4:6] + ":" + b[6:8] + ":" + b[8:10]
	}
	kernels := []string{"5.15.0-116-generic", "6.8.0-45-generic", "6.1.0-25-amd64"}
	if profile.arch == "arm64" {
		kernels[2] = "6.1.0-25-arm64"
	}
	timezones := []string{"UTC", "Asia/Shanghai", "Asia/Tokyo", "Europe/London", "Europe/Berlin", "America/New_York", "America/Los_Angeles"}
	return build(rawSignals{
		machineID: seedHex(seed, "machineId", 32),
		macs:      macs,
		osUser:    "u" + seedHex(seed, "osUser", 8),
		hostname:  "host-" + seedHex(seed, "hostname", 8),
		gitEmail:  seedHex(seed, "gitEmail", 8) + "@users.noreply.local",
		cpuModel:  profile.model, cpuCount: cpus, memGiB: memory,
		platform: "linux", arch: profile.arch,
		osRelease: kernels[profilePick(seed, "kernel", len(kernels))],
		container: profilePick(seed, "container", 2) == 1,
		timezone:  timezones[profilePick(seed, "timezone", len(timezones))],
	})
}

// completeHardware keeps real values whenever available. A single random
// seed per Resolve fills gaps; the resulting fingerprint is immutable for
// that process and, when configured, persisted to the existing state file.
func completeHardware(s rawSignals, seed string) rawSignals {
	profile := seededHardware(seed, s.arch)
	cpus, _ := hardwareValues(seed, profile)
	if s.cpuModel == "" {
		s.cpuModel = profile.model
	}
	if s.cpuCount <= 0 {
		s.cpuCount = cpus
	}
	if s.memGiB <= 0 {
		s.memGiB = s.cpuCount * []int{2, 4, 8}[profilePick(seed, "memory", 3)]
	}
	return s
}
