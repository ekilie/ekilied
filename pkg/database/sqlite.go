package database

import (
	"fmt"
	"log"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Config struct {
	Path            string
	MaxIdleConns    int
	MaxOpenConns    int
	ConnMaxLifetime time.Duration
	LogLevel        logger.LogLevel
}

var DB *gorm.DB

func DefaultConfig(path string) Config {
	return Config{
		Path:            path,
		MaxIdleConns:    1,
		MaxOpenConns:    1,
		ConnMaxLifetime: time.Hour,
		LogLevel:        logger.Warn,
	}
}

func Connect(cfg Config) error {
	db, err := gorm.Open(sqlite.Open(cfg.Path), &gorm.Config{
		Logger: logger.Default.LogMode(cfg.LogLevel),
	})
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", cfg.Path, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get underlying DB: %w", err)
	}

	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	DB = db
	log.Printf("sqlite database connected: %s", cfg.Path)
	return nil
}

func GetDB() *gorm.DB {
	return DB
}

func AutoMigrate(models ...any) error {
	if DB == nil {
		return fmt.Errorf("database not initialized")
	}
	return DB.AutoMigrate(models...)
}

func Close() error {
	if DB == nil {
		return nil
	}
	sqlDB, err := DB.DB()
	if err != nil {
		return fmt.Errorf("get underlying DB: %w", err)
	}
	return sqlDB.Close()
}
