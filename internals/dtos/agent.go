package dtos

type RegisterRequest struct {
	ServerID     uint   `json:"server_id"`
	Token        string `json:"token"`
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname,omitempty"`
	OS           string `json:"os,omitempty"`
	PublicIP     string `json:"public_ip,omitempty"`
	Arch         string `json:"arch,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
}

type RegisterResponse struct {
	AgentID      string `json:"agent_id"`
	SessionToken string `json:"session_token"`
	WsURL        string `json:"ws_url"`
	PollInterval int    `json:"poll_interval"`
}

type Capability struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Available bool   `json:"available"`
}

type HeartbeatRequest struct {
	AgentID  string         `json:"agent_id"`
	ServerID uint           `json:"server_id"`
	TS       string         `json:"ts,omitempty"`
	Metrics  HeartbeatMetrics `json:"metrics"`
}

type HeartbeatMetrics struct {
	CPUPercent    float64   `json:"cpu_percent"`
	MemoryPercent float64   `json:"memory_percent"`
	DiskPercent   float64   `json:"disk_percent"`
	LoadAvg       []float64 `json:"load_avg,omitempty"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	AgentVersion  string    `json:"agent_version"`
}

type HeartbeatResponse struct {
	Status           string `json:"status"`
	ServerStatus     string `json:"server_status"`
	PendingJobsCount int    `json:"pending_jobs_count"`
}

type JobItem struct {
	ID         uint                   `json:"id"`
	Type       string                 `json:"type"`
	Action     string                 `json:"action"`
	Params     map[string]interface{} `json:"params"`
	MaxRetries int                    `json:"max_retries"`
	Timeout    int                    `json:"timeout"`
}

type PollJobsResponse struct {
	Data []JobItem `json:"data"`
}

type StreamLogsRequest struct {
	Lines []LogLine `json:"lines"`
}

type LogLine struct {
	Stream   string `json:"stream"`
	Line     string `json:"line"`
	Step     string `json:"step,omitempty"`
	TS       string `json:"ts"`
	Sequence int    `json:"sequence"`
}

type CompleteJobRequest struct {
	Status string      `json:"status"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
	Step   string      `json:"step,omitempty"`
}
