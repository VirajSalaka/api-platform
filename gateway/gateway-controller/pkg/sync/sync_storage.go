/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"go.uber.org/zap"
)

// Storage defines the interface for persistent storage
type Storage interface {
	SaveConfig(cfg *models.StoredConfig) error
	UpdateConfig(cfg *models.StoredConfig) error
	DeleteConfig(id string) error
	GetConfig(id string) (*models.StoredConfig, error)
	GetConfigByNameVersion(name, version string) (*models.StoredConfig, error)
	GetConfigByHandle(handle string) (*models.StoredConfig, error)
	GetAllConfigs() ([]*models.StoredConfig, error)
	GetAllConfigsByKind(kind string) ([]*models.StoredConfig, error)
	SaveLLMProviderTemplate(template *models.StoredLLMProviderTemplate) error
	UpdateLLMProviderTemplate(template *models.StoredLLMProviderTemplate) error
	DeleteLLMProviderTemplate(id string) error
	GetLLMProviderTemplate(id string) (*models.StoredLLMProviderTemplate, error)
	GetLLMProviderTemplateByName(name string) (*models.StoredLLMProviderTemplate, error)
	GetAllLLMProviderTemplates() ([]*models.StoredLLMProviderTemplate, error)
	SaveCertificate(cert *models.StoredCertificate) error
	GetCertificate(id string) (*models.StoredCertificate, error)
	GetCertificateByName(name string) (*models.StoredCertificate, error)
	ListCertificates() ([]*models.StoredCertificate, error)
	DeleteCertificate(id string) error
	Close() error
}

// SyncAwareStorage wraps a Storage implementation and adds synchronization event recording
type SyncAwareStorage struct {
	storage      Storage
	db           *sql.DB
	stateManager StateManager
	eventStore   EventStore
	logger       *zap.Logger
}

// NewSyncAwareStorage creates a new SyncAwareStorage instance
func NewSyncAwareStorage(
	storage Storage,
	db *sql.DB,
	stateManager StateManager,
	eventStore EventStore,
	logger *zap.Logger,
) *SyncAwareStorage {
	return &SyncAwareStorage{
		storage:      storage,
		db:           db,
		stateManager: stateManager,
		eventStore:   eventStore,
		logger:       logger,
	}
}

// SaveConfig saves a config and records a CREATE event
func (s *SyncAwareStorage) SaveConfig(cfg *models.StoredConfig) error {
	ctx := context.Background()

	// Begin transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Save config using underlying storage
	if err := s.storage.SaveConfig(cfg); err != nil {
		return err
	}

	// Serialize config to JSON for event data
	eventData, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config for event: %w", err)
	}

	// Record CREATE event
	if err := s.eventStore.RecordEventWithTx(ctx, tx, EntityTypeAPI, ActionCreate, cfg.ID, eventData, time.Now()); err != nil {
		return fmt.Errorf("failed to record CREATE event: %w", err)
	}

	// Update entity state version
	if _, err := s.stateManager.UpdateStateWithTx(ctx, tx, EntityTypeAPI); err != nil {
		return fmt.Errorf("failed to update entity state: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	s.logger.Debug("Recorded CREATE event for config",
		zap.String("config_id", cfg.ID),
		zap.String("name", cfg.GetName()),
		zap.String("version", cfg.GetVersion()))

	return nil
}

// UpdateConfig updates a config and records an UPDATE event
func (s *SyncAwareStorage) UpdateConfig(cfg *models.StoredConfig) error {
	ctx := context.Background()

	// Begin transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Update config using underlying storage
	if err := s.storage.UpdateConfig(cfg); err != nil {
		return err
	}

	// Serialize config to JSON for event data
	eventData, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config for event: %w", err)
	}

	// Record UPDATE event
	if err := s.eventStore.RecordEventWithTx(ctx, tx, EntityTypeAPI, ActionUpdate, cfg.ID, eventData, time.Now()); err != nil {
		return fmt.Errorf("failed to record UPDATE event: %w", err)
	}

	// Update entity state version
	if _, err := s.stateManager.UpdateStateWithTx(ctx, tx, EntityTypeAPI); err != nil {
		return fmt.Errorf("failed to update entity state: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	s.logger.Debug("Recorded UPDATE event for config",
		zap.String("config_id", cfg.ID),
		zap.String("name", cfg.GetName()),
		zap.String("version", cfg.GetVersion()))

	return nil
}

// DeleteConfig deletes a config and records a DELETE event
func (s *SyncAwareStorage) DeleteConfig(id string) error {
	ctx := context.Background()

	// Begin transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Delete config using underlying storage
	if err := s.storage.DeleteConfig(id); err != nil {
		return err
	}

	// For DELETE events, just store the ID
	deleteData := map[string]string{"id": id}
	eventData, err := json.Marshal(deleteData)
	if err != nil {
		return fmt.Errorf("failed to marshal delete data for event: %w", err)
	}

	// Record DELETE event
	if err := s.eventStore.RecordEventWithTx(ctx, tx, EntityTypeAPI, ActionDelete, id, eventData, time.Now()); err != nil {
		return fmt.Errorf("failed to record DELETE event: %w", err)
	}

	// Update entity state version
	if _, err := s.stateManager.UpdateStateWithTx(ctx, tx, EntityTypeAPI); err != nil {
		return fmt.Errorf("failed to update entity state: %w", err)
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	s.logger.Debug("Recorded DELETE event for config",
		zap.String("config_id", id))

	return nil
}

// All other methods delegate to the underlying storage

func (s *SyncAwareStorage) GetConfig(id string) (*models.StoredConfig, error) {
	return s.storage.GetConfig(id)
}

func (s *SyncAwareStorage) GetConfigByNameVersion(name, version string) (*models.StoredConfig, error) {
	return s.storage.GetConfigByNameVersion(name, version)
}

func (s *SyncAwareStorage) GetConfigByHandle(handle string) (*models.StoredConfig, error) {
	return s.storage.GetConfigByHandle(handle)
}

func (s *SyncAwareStorage) GetAllConfigs() ([]*models.StoredConfig, error) {
	return s.storage.GetAllConfigs()
}

func (s *SyncAwareStorage) GetAllConfigsByKind(kind string) ([]*models.StoredConfig, error) {
	return s.storage.GetAllConfigsByKind(kind)
}

func (s *SyncAwareStorage) SaveLLMProviderTemplate(template *models.StoredLLMProviderTemplate) error {
	return s.storage.SaveLLMProviderTemplate(template)
}

func (s *SyncAwareStorage) UpdateLLMProviderTemplate(template *models.StoredLLMProviderTemplate) error {
	return s.storage.UpdateLLMProviderTemplate(template)
}

func (s *SyncAwareStorage) DeleteLLMProviderTemplate(id string) error {
	return s.storage.DeleteLLMProviderTemplate(id)
}

func (s *SyncAwareStorage) GetLLMProviderTemplate(id string) (*models.StoredLLMProviderTemplate, error) {
	return s.storage.GetLLMProviderTemplate(id)
}

func (s *SyncAwareStorage) GetLLMProviderTemplateByName(name string) (*models.StoredLLMProviderTemplate, error) {
	return s.storage.GetLLMProviderTemplateByName(name)
}

func (s *SyncAwareStorage) GetAllLLMProviderTemplates() ([]*models.StoredLLMProviderTemplate, error) {
	return s.storage.GetAllLLMProviderTemplates()
}

func (s *SyncAwareStorage) SaveCertificate(cert *models.StoredCertificate) error {
	return s.storage.SaveCertificate(cert)
}

func (s *SyncAwareStorage) GetCertificate(id string) (*models.StoredCertificate, error) {
	return s.storage.GetCertificate(id)
}

func (s *SyncAwareStorage) GetCertificateByName(name string) (*models.StoredCertificate, error) {
	return s.storage.GetCertificateByName(name)
}

func (s *SyncAwareStorage) ListCertificates() ([]*models.StoredCertificate, error) {
	return s.storage.ListCertificates()
}

func (s *SyncAwareStorage) DeleteCertificate(id string) error {
	return s.storage.DeleteCertificate(id)
}

func (s *SyncAwareStorage) Close() error {
	return s.storage.Close()
}
