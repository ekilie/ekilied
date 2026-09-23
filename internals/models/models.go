package models

import "gorm.io/gorm"

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

// AllModels returns the models AutoMigrate manages. Only models with readers
// or writers belong here: the agent keeps in-flight jobs in memory
// (JobEngine.active and .dispatched), so job, site, and setting tables would
// be migrated on every boot for nothing.
//
// The four tables removed in this change (pending_jobs, completed_jobs,
// site_caches, settings) are left untouched in existing local databases.
// They are empty, unused, and harmless. A downgrade to an older binary would
// simply recreate them through AutoMigrate.
func AllModels() []any {
	return []any{
		&Identity{},
		&Capability{},
	}
}
