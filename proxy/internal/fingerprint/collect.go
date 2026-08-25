package fingerprint

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"
)

// collectLive gathers raw signals from the actual environment.
// Every probe is best-effort; failures degrade to empty strings.
func collectLive() rawSignals {
	return rawSignals{
		machineID: readMachineID(),
		macs:      readMACs(),
		osUser:    readOSUser(),
		hostname:  readHostname(),
		gitEmail:  readGitEmail(),
		cpuModel:  readCPUModel(),
		cpuCount:  runtime.NumCPU(),
		memGiB:    readMemGiB(),
	}
}

// readMachineID reads the OS machine ID (Linux /etc/machine-id family).
func readMachineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if raw, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(raw)); s != "" {
				return s
			}
		}
	}
	return ""
}

// readMACs collects MAC addresses of up, non-loopback interfaces.
func readMACs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		if ifc.HardwareAddr != nil {
			out = append(out, ifc.HardwareAddr.String())
		}
	}
	return out
}

func readOSUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func readHostname() string {
	h, _ := os.Hostname()
	return h
}

// readGitEmail shells out to git config; empty when git is absent.
func readGitEmail() string {
	out, err := exec.Command("git", "config", "--global", "user.email").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readCPUModel extracts the first model name from /proc/cpuinfo.
func readCPUModel() string {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(line, "model name"); ok {
			if i := strings.Index(rest, ":"); i >= 0 {
				return strings.TrimSpace(rest[i+1:])
			}
		}
	}
	return ""
}

// readMemGiB reads total memory from /proc/meminfo.
func readMemGiB() int {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			var kb int
			if _, err := fmt.Sscanf(strings.TrimSpace(rest), "%d kB", &kb); err == nil {
				return kb / 1024 / 1024
			}
		}
	}
	return 0
}

// osRelease approximates the CLI's os.release() on Linux.
func osRelease() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isContainer detects common container markers.
func isContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if raw, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(raw)
		if strings.Contains(s, "docker") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd") {
			return true
		}
	}
	return false
}

// timezone returns the local IANA timezone name.
func timezone() string {
	name, _ := time.Now().Zone()
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	return name
}
