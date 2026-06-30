package agent

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/ekilie/ekilied/internals/dtos"
	"github.com/ekilie/ekilied/internals/models"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
)

var startTime = time.Now()

func collectMetrics() dtos.HeartbeatMetrics {
	cpuP, _ := cpu.Percent(0, false)
	memV, _ := mem.VirtualMemory()
	diskV, _ := disk.Usage("/")
	loadV, _ := load.Avg()
	hostInfo, _ := host.Info()
	hostname, _ := os.Hostname()

	m := dtos.HeartbeatMetrics{
		CPUPercent:    0.0,
		MemoryPercent: 0.0,
		DiskPercent:   0.0,
		UptimeSeconds: int64(time.Since(startTime).Seconds()),
		AgentVersion:  "1.0.0",
		Hostname:      hostname,
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
