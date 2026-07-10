package jobengine

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
)

func (e *JobEngine) runDiagnostics(ctx context.Context, jobID uint, action string, lb *LogBatcher, logf func(string, ...any)) {
	logf("[diag] gathering server metrics...")
	diagStarted := time.Now()

	hostname, _ := os.Hostname()
	logf("[diag] hostname: %s", hostname)
	logf("[diag] agent version: %s (commit: %s)", config.Version, config.Commit)
	lb.flushNow()

	// ── CPU ──
	logf("[diag] collecting CPU info...")
	cpuInfo, _ := cpu.Info()
	if len(cpuInfo) > 0 {
		logf("[diag]   model: %s", cpuInfo[0].ModelName)
	}
	cores, _ := cpu.Counts(true)
	logical, _ := cpu.Counts(false)
	logf("[diag]   cores: %d physical / %d logical", cores, logical)
	cpuPct, _ := cpu.Percent(200*time.Millisecond, false)
	if len(cpuPct) > 0 {
		logf("[diag]   usage: %.1f%%", cpuPct[0])
	}
	lb.flushNow()

	// ── Memory ──
	logf("[diag] collecting memory info...")
	memInfo, _ := mem.VirtualMemory()
	if memInfo != nil {
		logf("[diag]   total: %s", fmtBytes(memInfo.Total))
		logf("[diag]   used:  %s (%.1f%%)", fmtBytes(memInfo.Used), memInfo.UsedPercent)
		logf("[diag]   free:  %s", fmtBytes(memInfo.Free))
	}
	lb.flushNow()

	// ── Disk ──
	logf("[diag] collecting disk info...")
	diskInfo, _ := disk.Usage("/")
	if diskInfo != nil {
		logf("[diag]   total: %s", fmtBytes(diskInfo.Total))
		logf("[diag]   used:  %s (%.1f%%)", fmtBytes(diskInfo.Used), diskInfo.UsedPercent)
		logf("[diag]   free:  %s", fmtBytes(diskInfo.Free))
	}
	lb.flushNow()

	// ── Load ──
	logf("[diag] collecting load averages...")
	loadAvg, _ := load.Avg()
	if loadAvg != nil {
		logf("[diag]   1 min:  %.2f", loadAvg.Load1)
		logf("[diag]   5 min:  %.2f", loadAvg.Load5)
		logf("[diag]   15 min: %.2f", loadAvg.Load15)
	}
	lb.flushNow()

	// ── Uptime ──
	logf("[diag] collecting uptime...")
	upSeconds, _ := host.Uptime()
	days := upSeconds / 86400
	hours := (upSeconds % 86400) / 3600
	mins := (upSeconds % 3600) / 60
	logf("[diag]   uptime: %dd %dh %dm (%d seconds)", days, hours, mins, upSeconds)
	lb.flushNow()

	// ── OS ──
	logf("[diag] collecting OS info...")
	hostInfo, _ := host.Info()
	if hostInfo != nil {
		logf("[diag]   os:       %s", hostInfo.OS)
		logf("[diag]   platform: %s %s", hostInfo.Platform, hostInfo.PlatformVersion)
		logf("[diag]   kernel:   %s", hostInfo.KernelVersion)
	}
	lb.flushNow()

	// ── Network ──
	logf("[diag] collecting network I/O...")
	netIO, _ := net.IOCounters(false)
	if len(netIO) > 0 {
		logf("[diag]   bytes sent:    %s", fmtBytes(netIO[0].BytesSent))
		logf("[diag]   bytes received: %s", fmtBytes(netIO[0].BytesRecv))
		logf("[diag]   packets sent:  %d", netIO[0].PacketsSent)
		logf("[diag]   packets recv:  %d", netIO[0].PacketsRecv)
	}
	lb.flushNow()

	duration := time.Since(diagStarted)
	logf("[diag] diagnostics complete in %v", duration)
	lb.flushNow()

	// Guarded values: gopsutil calls can return nil/empty on some hosts,
	// so build the result payload from safe fallbacks to avoid panics.
	cpuUsage := "unknown"
	if len(cpuPct) > 0 {
		cpuUsage = fmt.Sprintf("%.1f%%", cpuPct[0])
	}
	memUsage := "unknown"
	if memInfo != nil {
		memUsage = fmt.Sprintf("%.1f%%", memInfo.UsedPercent)
	}
	diskUsage := "unknown"
	if diskInfo != nil {
		diskUsage = fmt.Sprintf("%.1f%%", diskInfo.UsedPercent)
	}
	var load1, load5, load15 float64
	if loadAvg != nil {
		load1, load5, load15 = loadAvg.Load1, loadAvg.Load5, loadAvg.Load15
	}
	osName, platform, kernel := "unknown", "unknown", "unknown"
	if hostInfo != nil {
		osName = hostInfo.OS
		platform = hostInfo.Platform + " " + hostInfo.PlatformVersion
		kernel = hostInfo.KernelVersion
	}

	result := map[string]any{
		"total_ms":  duration.Milliseconds(),
		"ok":        true,
		"version":   config.Version,
		"hostname":  hostname,
		"cpu_usage": cpuUsage,
		"memory":    memUsage,
		"disk":      diskUsage,
		"load_1":    load1,
		"load_5":    load5,
		"load_15":   load15,
		"uptime_s":  upSeconds,
		"os":        osName,
		"platform":  platform,
		"kernel":    kernel,
	}
	if err := e.client.CompleteJob(ctx, jobID, "success", "", action, result); err != nil {
		log.Printf("complete job %d failed: %v", jobID, err)
	}
}
