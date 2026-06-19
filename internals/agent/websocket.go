package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ekilie/ekilied/internals/config"
	"github.com/ekilie/ekilied/internals/dtos"
)

type WSClient struct {
	cfg    *config.Config
	client *http.Client
}

func NewWSClient(cfg *config.Config) *WSClient {
	return &WSClient{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *WSClient) Register(ctx context.Context) (sessionToken, agentID string, err error) {
	reqBody, _ := json.Marshal(dtos.RegisterRequest{
		ServerID:     c.cfg.ServerID,
		Token:        c.cfg.RegistrationToken,
		AgentVersion: "1.0.0",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/register", bytes.NewReader(reqBody))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		return "", "", fmt.Errorf("registration failed (HTTP %d)", resp.StatusCode)
	}

	var result dtos.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}

	if result.WsURL != "" {
		c.cfg.WsURL = result.WsURL
	}
	if result.PollInterval > 0 {
		c.cfg.PollInterval = result.PollInterval
	}

	log.Printf("registered: agent_id=%s", result.AgentID)
	return result.SessionToken, result.AgentID, nil
}

func (c *WSClient) SendHeartbeat(ctx context.Context, agentID, sessionToken string, metrics dtos.HeartbeatMetrics) error {
	reqBody, _ := json.Marshal(dtos.HeartbeatRequest{
		AgentID:  agentID,
		ServerID: c.cfg.ServerID,
		TS:       time.Now().UTC().Format(time.RFC3339),
		Metrics:  metrics,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/heartbeat", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("heartbeat failed (HTTP %d)", resp.StatusCode)
	}

	var result dtos.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
		if result.PendingJobsCount > 0 {
			log.Printf("%d pending job(s) on server", result.PendingJobsCount)
		}
	}
	return nil
}

func (c *WSClient) PollJobs(ctx context.Context) ([]dtos.JobItem, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.APIURL+"/agents/jobs", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result dtos.PollJobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *WSClient) AcceptJob(ctx context.Context, jobID uint) error {
	url := fmt.Sprintf("%s/agents/jobs/%d/accept", c.cfg.APIURL, jobID)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("accept job failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (c *WSClient) StreamLogs(ctx context.Context, jobID uint, lines []dtos.LogLine) error {
	url := fmt.Sprintf("%s/agents/jobs/%d/logs", c.cfg.APIURL, jobID)
	body, _ := json.Marshal(dtos.StreamLogsRequest{Lines: lines})

	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("stream logs failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (c *WSClient) CompleteJob(ctx context.Context, jobID uint, status, errorMsg, step string, result interface{}) error {
	url := fmt.Sprintf("%s/agents/jobs/%d/complete", c.cfg.APIURL, jobID)
	body, _ := json.Marshal(dtos.CompleteJobRequest{
		Status: status,
		Error:  errorMsg,
		Step:   step,
		Result: result,
	})

	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("complete job failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func (c *WSClient) Connect(ctx context.Context) {
	log.Println("WebSocket connection loop starting (polling fallback active)")
	ticker := time.NewTicker(time.Duration(c.cfg.PollInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jobs, err := c.PollJobs(ctx)
			if err != nil {
				log.Printf("poll jobs failed: %v", err)
				continue
			}
			for _, job := range jobs {
				log.Printf("received job: id=%d action=%s", job.ID, job.Action)
			}
		}
	}
}
