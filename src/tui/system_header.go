package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type cpuCounters struct {
	user, system, nice, idle, wait, irq, softirq, steal uint64
}

type systemHeader struct {
	Now                                     time.Time
	Uptime                                  time.Duration
	Users                                   int
	Load1, Load5, Load15                    float64
	CPU                                     [8]float64
	MemTotal, MemFree, MemUsed, MemCache    float64
	SwapTotal, SwapFree, SwapUsed, MemAvail float64
}

func readSystemHeader(previous cpuCounters, havePrevious bool) (systemHeader, cpuCounters) {
	h := systemHeader{Now: time.Now()}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			if seconds, err := strconv.ParseFloat(fields[0], 64); err == nil {
				h.Uptime = time.Duration(seconds * float64(time.Second))
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 3 {
			h.Load1, _ = strconv.ParseFloat(fields[0], 64)
			h.Load5, _ = strconv.ParseFloat(fields[1], 64)
			h.Load15, _ = strconv.ParseFloat(fields[2], 64)
		}
	}
	current := readCPUCounters()
	if havePrevious {
		var total uint64
		var delta [8]uint64
		old := [8]uint64{previous.user, previous.system, previous.nice, previous.idle, previous.wait, previous.irq, previous.softirq, previous.steal}
		now := [8]uint64{current.user, current.system, current.nice, current.idle, current.wait, current.irq, current.softirq, current.steal}
		for i := range now {
			delta[i] = now[i] - old[i]
			total += delta[i]
		}
		if total > 0 {
			for i := range delta {
				h.CPU[i] = float64(delta[i]) * 100 / float64(total)
			}
		}
	}
	h.Users = loggedInUsers()
	mem := readMemInfo()
	h.MemTotal = mem["MemTotal"]
	h.MemFree = mem["MemFree"]
	h.MemCache = mem["Buffers"] + mem["Cached"] + mem["SReclaimable"] - mem["Shmem"]
	h.MemUsed = h.MemTotal - h.MemFree - h.MemCache
	if h.MemUsed < 0 {
		h.MemUsed = 0
	}
	h.SwapTotal = mem["SwapTotal"]
	h.SwapFree = mem["SwapFree"]
	h.SwapUsed = h.SwapTotal - h.SwapFree
	h.MemAvail = mem["MemAvailable"]
	return h, current
}

func readCPUCounters() cpuCounters {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuCounters{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] != "cpu" {
			continue
		}
		v := make([]uint64, 8)
		for i := range v {
			v[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
		}
		return cpuCounters{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
	}
	return cpuCounters{}
}

func readMemInfo() map[string]float64 {
	out := map[string]float64{}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		// /proc/meminfo reports kB; top's MiB display is binary MiB.
		out[strings.TrimSuffix(fields[0], ":")] = value / 1024
	}
	return out
}

func loggedInUsers() int {
	b, err := exec.Command("who").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func (h systemHeader) lines() []string {
	return []string{
		fmt.Sprintf("top - %s up %s, %d user%s, load average: %.2f, %.2f, %.2f", h.Now.Format("15:04:05"), formatUptime(h.Uptime), h.Users, plural(h.Users), h.Load1, h.Load5, h.Load15),
		fmt.Sprintf("%%Cpu(s): %4.1f us, %4.1f sy, %4.1f ni, %4.1f id, %4.1f wa, %4.1f hi, %4.1f si, %4.1f st", h.CPU[0], h.CPU[1], h.CPU[2], h.CPU[3], h.CPU[4], h.CPU[5], h.CPU[6], h.CPU[7]),
		fmt.Sprintf("MiB Mem : %8.1f total, %8.1f free, %8.1f used, %8.1f buff/cache", h.MemTotal, h.MemFree, h.MemUsed, h.MemCache),
		fmt.Sprintf("MiB Swap: %8.1f total, %8.1f free, %8.1f used. %8.1f avail Mem", h.SwapTotal, h.SwapFree, h.SwapUsed, h.MemAvail),
	}
}

func formatUptime(d time.Duration) string {
	minutes := int(d / time.Minute)
	if minutes < 1 {
		return fmt.Sprintf("%d sec", int(d/time.Second))
	}
	days, hours := minutes/1440, (minutes%1440)/60
	minutes %= 60
	if days > 0 {
		return fmt.Sprintf("%d day%s, %d:%02d", days, plural(days), hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%d:%02d", hours, minutes)
	}
	return fmt.Sprintf("%d min", minutes)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
