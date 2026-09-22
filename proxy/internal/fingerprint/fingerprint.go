// Package fingerprint generates the machine fingerprint the real CLI
// reports to /alpha/fingerprint/record — and, crucially, makes it
// replayable across pod reschedules.
//
// Resolution priority (design doc §7):
//  1. FINGERPRINT_SEED   — deterministic HMAC derivation (k8s-friendly)
//  2. state file         — collect-once-replay-forever (PVC-friendly)
//  3. live collect       — gather from the actual environment, persist
//
// The thumbmark algorithm matches command-code@1.32.2 exactly and was
// revalidated against command-code@1.51.3:
//
//	thumbmark = sha256(namespace + "\0machine\0" + parts.join("|"))
//	parts     = [machineId, sortedUniqueMACs.join(","),
//	             hostname (only if no machineId), cpuModel (only if no machineId)]
//	namespace = "command-code:device-fingerprint:v1"
package fingerprint

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// namespace is the public constant baked into the real CLI.
const namespace = "command-code:device-fingerprint:v1"

// collectorVersion matches the real CLI's reported version.
const collectorVersion = 1

// Components mirrors the CLI's reported component set.
type Components struct {
	MachineIDHash    string   `json:"machineIdHash"`
	MACHashes        []string `json:"macHashes"`
	OSUserHash       string   `json:"osUserHash"`
	HostnameHash     string   `json:"hostnameHash"`
	GitEmailHash     string   `json:"gitEmailHash"`
	Platform         string   `json:"platform"`
	Arch             string   `json:"arch"`
	OSRelease        string   `json:"osRelease"`
	CPUModel         string   `json:"cpuModel"`
	CPUCount         int      `json:"cpuCount"`
	MemGiB           int      `json:"memGiB"`
	IsContainer      bool     `json:"isContainer"`
	Timezone         string   `json:"timezone,omitempty"`
	Runtime          string   `json:"runtime"`
	CollectorVersion int      `json:"collectorVersion"`
}

// Fingerprint is the full report payload.
type Fingerprint struct {
	Thumbmark  string     `json:"thumbmark"`
	Components Components `json:"components"`
}

// rawSignals are the pre-hash inputs (never reported directly).
type rawSignals struct {
	machineID string
	macs      []string
	osUser    string
	hostname  string
	gitEmail  string
	cpuModel  string
	cpuCount  int
	memGiB    int
	platform  string
	arch      string
	osRelease string
	container bool
	timezone  string
}

// build applies the real CLI algorithm to raw signals.
func build(s rawSignals) *Fingerprint {
	macs := sortedUnique(s.macs)

	parts := []string{strings.TrimSpace(s.machineID), strings.Join(macs, ",")}
	if strings.TrimSpace(s.machineID) == "" {
		// hostname and CPU model only participate when machineId is missing.
		if h := strings.TrimSpace(s.hostname); h != "" {
			parts = append(parts, h)
		}
		if c := strings.TrimSpace(s.cpuModel); c != "" {
			parts = append(parts, c)
		}
	}
	joined := strings.Join(filterEmpty(parts), "|")
	if joined == "" {
		joined = "unknown"
	}

	thumb := sha256.New()
	thumb.Write([]byte(namespace))
	thumb.Write([]byte("\x00machine\x00"))
	thumb.Write([]byte(joined))

	return &Fingerprint{
		Thumbmark: hex.EncodeToString(thumb.Sum(nil)),
		Components: Components{
			MachineIDHash:    hashSignal(s.machineID),
			MACHashes:        hashAll(macs),
			OSUserHash:       hashSignal(s.osUser),
			HostnameHash:     hashSignal(s.hostname),
			GitEmailHash:     hashSignal(s.gitEmail),
			Platform:         s.platform,
			Arch:             s.arch,
			OSRelease:        s.osRelease,
			CPUModel:         s.cpuModel,
			CPUCount:         s.cpuCount,
			MemGiB:           s.memGiB,
			IsContainer:      s.container,
			Timezone:         s.timezone,
			Runtime:          "cli",
			CollectorVersion: collectorVersion,
		},
	}
}

// hashSignal hashes one string component the way the CLI does:
// sha256(namespace + "\0" + value.toLowerCase()). Empty input → empty output.
func hashSignal(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(namespace))
	h.Write([]byte("\x00"))
	h.Write([]byte(strings.ToLower(v)))
	return hex.EncodeToString(h.Sum(nil))
}

func hashAll(vs []string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if h := hashSignal(v); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func sortedUnique(macs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range macs {
		m = strings.ToLower(strings.TrimSpace(m))
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func filterEmpty(vs []string) []string {
	out := vs[:0]
	for _, v := range vs {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ── resolution ──────────────────────────────────────────────────────

// Resolve returns the fingerprint per the priority chain
// seed > state file > live collect (persisting a live collect).
func Resolve(seed, stateFile string) (*Fingerprint, error) {
	if seed != "" {
		return deriveFromSeed(seed), nil
	}
	if stateFile != "" {
		if fp, err := loadState(stateFile); err == nil {
			return fp, nil
		}
	}
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, fmt.Errorf("fingerprint: fallback entropy: %w", err)
	}
	fp := build(completeHardware(collectLive(), hex.EncodeToString(entropy[:])))
	if stateFile != "" {
		if err := saveState(stateFile, fp); err != nil {
			// Non-fatal: fingerprint still works, just won't survive restarts.
			return fp, fmt.Errorf("fingerprint: persist state: %w", err)
		}
	}
	return fp, nil
}

// The CLI uses Node's os.platform()/os.arch() spellings, not Go's.
func cliPlatform(platform string) string {
	if platform == "windows" {
		return "win32"
	}
	return platform
}

func cliArch(arch string) string {
	switch arch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return arch
	}
}

// state is the persisted shape (identical to the report payload).
func loadState(path string) (*Fingerprint, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fp Fingerprint
	if err := json.Unmarshal(raw, &fp); err != nil {
		return nil, err
	}
	if fp.Thumbmark == "" {
		return nil, fmt.Errorf("fingerprint: state file %s has no thumbmark", path)
	}
	// Normalize old Go spellings without changing the persisted device ID.
	fp.Components.Platform = cliPlatform(fp.Components.Platform)
	fp.Components.Arch = cliArch(fp.Components.Arch)
	return &fp, nil
}

func saveState(path string, fp *Fingerprint) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(fp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
