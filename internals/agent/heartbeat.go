package agent

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
	"github.com/ekilie/ekilied/internals/models"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
)

var startTime = time.Now()

// hostUptimeFunc returns the host uptime in seconds. It is a variable so
// tests can stub both the value and the error path.
var hostUptimeFunc = host.Uptime

func collectMetrics() dtos.HeartbeatMetrics {
	cpuP, _ := cpu.Percent(0, false)
	memV, _ := mem.VirtualMemory()
	diskV, _ := disk.Usage("/")
	loadV, _ := load.Avg()
	hostInfo, _ := host.Info()
	hostname, _ := os.Hostname()

	agentUptime := int64(time.Since(startTime).Seconds())

	m := dtos.HeartbeatMetrics{
		CPUPercent:         0.0,
		MemoryPercent:      0.0,
		DiskPercent:        0.0,
		AgentUptimeSeconds: agentUptime,
		AgentVersion:       config.Version,
		Hostname:           hostname,
	}

	// UptimeSeconds is host uptime, matching the diagnostics job. If the host
	// value is unavailable, fall back to the agent uptime and say so instead
	// of shipping a silent zero.
	if hostUptime, err := hostUptimeFunc(); err == nil {
		m.UptimeSeconds = int64(hostUptime)
	} else {
		m.UptimeSeconds = agentUptime
		m.UptimeFallback = true
	}

	if len(cpuP) > 0 {
		m.CPUPercent = cpuP[0]
	}
	if memV != nil {
		m.MemoryPercent = memV.UsedPercent
		m.MemoryTotalBytes = memV.Total
		m.MemoryUsedBytes = memV.Used
		m.MemoryAvailBytes = memV.Available
	}
	if diskV != nil {
		m.DiskPercent = diskV.UsedPercent
		m.DiskTotalBytes = diskV.Total
		m.DiskUsedBytes = diskV.Used
	}
	if loadV != nil {
		m.LoadAvg = []float64{loadV.Load1, loadV.Load5, loadV.Load15}
	}
	cpuCount, _ := cpu.Counts(true)
	m.CPUCount = cpuCount

	if hostInfo != nil {
		m.Platform = hostInfo.Platform + " " + hostInfo.PlatformVersion
		m.KernelArch = hostInfo.KernelArch
	}

	return m
}

func (e *Ekilied) sendHeartbeat(ctx context.Context) error {
	metrics := collectMetrics()

	loadVal := 0.0
	if len(metrics.LoadAvg) > 0 {
		loadVal = metrics.LoadAvg[0]
	}
	log.Printf("heartbeat: cpu=%.1f%% mem=%.1f%% disk=%.1f%% load=%.2f host=%s",
		metrics.CPUPercent, metrics.MemoryPercent, metrics.DiskPercent,
		loadVal, metrics.Hostname)

	e.db.Model(&models.Identity{}).Where("1 = 1").Update("last_heartbeat", time.Now().Unix())

	return e.ws.SendHeartbeat(ctx, e.cfg.AgentID, e.cfg.SessionToken, metrics)
}
