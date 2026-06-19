package models

import (
	"time"

	"gorm.io/gorm"
)

type Identity struct {
	gorm.Model
	AgentID           string `gorm:"uniqueIndex;not null"`
	ServerID          uint   `gorm:"not null"`
	SessionToken      string `gorm:"not null"`
	RegistrationToken string
	APIURL            string `gorm:"not null"`
	WsURL             string `gorm:"not null"`
	PollInterval      int    `gorm:"default:5"`
	LastHeartbeat     int64
	Connected         bool   `gorm:"default:false"`
	Version           string `gorm:"not null"`
}

type Capability struct {
	gorm.Model
	AgentID     string `gorm:"index"`
	Name        string `gorm:"not null"`
	Version     string
	Available   bool `gorm:"not null"`
	LastChecked int64
}

type PendingJob struct {
	gorm.Model
	JobID        uint   `gorm:"uniqueIndex;not null"`
	Action       string `gorm:"not null"`
	Status       string `gorm:"not null"`
	Params       string `gorm:"type:text"`
	Retries      int    `gorm:"default:0"`
	MaxRetries   int    `gorm:"default:3"`
	LastError    string `gorm:"type:text"`
	StartedAt    int64
	CompletedAt  int64
	DeployLockID string `gorm:"index"`
}

type CompletedJob struct {
	gorm.Model
	JobID       uint   `gorm:"uniqueIndex;not null"`
	Action      string `gorm:"not null"`
	Status      string `gorm:"not null"`
	Params      string `gorm:"type:text"`
	Summary     string `gorm:"type:text"`
	Retries     int
	StartedAt   int64
	CompletedAt int64
}

type SiteCache struct {
	gorm.Model
	SiteName      string `gorm:"uniqueIndex;not null"`
	SiteType      string `gorm:"not null"`
	Domains       string `gorm:"type:text"`
	WebDirectory  string `gorm:"default:/"`
	NginxConfig   string `gorm:"type:text"`
	DeployScript  string `gorm:"type:text"`
	ActiveRelease string
	LastDeployAt  int64
	EnvHash       string
}

type Setting struct {
	gorm.Model
	Key   string `gorm:"uniqueIndex;not null"`
	Value string `gorm:"type:text;not null"`
}

// AllModels returns all models for AutoMigrate.
func AllModels() []any {
	return []any{
		&Identity{},
		&Capability{},
		&PendingJob{},
		&CompletedJob{},
		&SiteCache{},
		&Setting{},
	}
}

// Ensure time import is used (referenced by gorm.Model)
var _ = time.Time{}
