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
)

type WSClient struct {
	cfg    *config.Config
	client *http.Client
}

func NewWSClient(cfg *config.Config) *WSClient {
	return &WSClient{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *WSClient) Register(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"server_id":     c.cfg.ServerID,
		"token":         c.cfg.RegistrationToken,
		"agent_version": "1.0.0",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/register", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		return "", fmt.Errorf("registration failed: %s", resp.Status)
	}

	var result struct {
		SessionToken string `json:"session_token"`
		WsURL        string `json:"ws_url"`
		PollInterval int    `json:"poll_interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}

	if result.WsURL != "" {
		c.cfg.WsURL = result.WsURL
	}
	if result.PollInterval > 0 {
		c.cfg.PollInterval = result.PollInterval
	}

	log.Printf("registered as agent_id=%s", result.SessionToken[:12])
	return result.SessionToken, nil
}

func (c *WSClient) SendHeartbeat(ctx context.Context, agentID, sessionToken string, metrics interface{}) error {
	body, _ := json.Marshal(map[string]interface{}{
		"agent_id": agentID,
		"ts":       time.Now().UTC(),
		"metrics":  metrics,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.cfg.APIURL+"/agents/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("heartbeat: %s", resp.Status)
	}
	var result struct {
		PendingJobsCount int `json:"pending_jobs_count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.PendingJobsCount > 0 {
		log.Printf("%d pending jobs", result.PendingJobsCount)
	}
	return nil
}

func (c *WSClient) PollJobs(ctx context.Context) ([]interface{}, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.cfg.APIURL+"/agents/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.SessionToken)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data []interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *WSClient) Connect(ctx context.Context) {
	log.Println("WebSocket connection loop starting")
	<-ctx.Done()
}
