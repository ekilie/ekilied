package agent

import (
	"context"
	"log"
	"time"

	"github.com/ekilie/ekilied/internals/models"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

var startTime = time.Now()

type HeartbeatMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	DiskPercent   float64 `json:"disk_percent"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	AgentVersion  string  `json:"agent_version"`
}

func collectMetrics() HeartbeatMetrics {
	cpuP, _ := cpu.Percent(0, false)
	memV, _ := mem.VirtualMemory()
	diskV, _ := disk.Usage("/")

	cpuVal := 0.0
	if len(cpuP) > 0 {
		cpuVal = cpuP[0]
	}
	memVal := 0.0
	if memV != nil {
		memVal = memV.UsedPercent
	}
	diskVal := 0.0
	if diskV != nil {
		diskVal = diskV.UsedPercent
	}

	return HeartbeatMetrics{
		CPUPercent:    cpuVal,
		MemoryPercent: memVal,
		DiskPercent:   diskVal,
		UptimeSeconds: int64(time.Since(startTime).Seconds()),
		AgentVersion:  "1.0.0",
	}
}

func (e *Ekilied) sendHeartbeat(ctx context.Context) error {
	metrics := collectMetrics()
	log.Printf("heartbeat: cpu=%.1f%% mem=%.1f%% disk=%.1f%%",
		metrics.CPUPercent, metrics.MemoryPercent, metrics.DiskPercent)

	e.db.Model(&models.Identity{}).Where("1 = 1").Update("last_heartbeat", time.Now().Unix())

	return e.ws.SendHeartbeat(ctx, e.cfg.AgentID, e.cfg.SessionToken, metrics)
}
