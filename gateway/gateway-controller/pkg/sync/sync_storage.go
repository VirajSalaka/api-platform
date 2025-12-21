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
	"time"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"go.uber.org/zap"
)

// SyncAwareStorage wraps Storage to add sync event recording
// It implements the storage.Storage interface and adds transactional event recording
type SyncAwareStorage struct {
	storage.Storage
	db             *sql.DB
	stateManager   StateManager
	eventStore     EventStore
	organizationID string
	logger         *zap.Logger
}

// NewSyncAwareStorage creates a new SyncAwareStorage wrapper
func NewSyncAwareStorage(
	underlying storage.Storage,
	db *sql.DB,
	stateManager StateManager,
	eventStore EventStore,
	organizationID string,
	logger *zap.Logger,
) *SyncAwareStorage {
	return &SyncAwareStorage{
		Storage:        underlying,
		db:             db,
		stateManager:   stateManager,
		eventStore:     eventStore,
		organizationID: organizationID,
		logger:         logger,
	}
}

// SaveConfig persists a new API configuration with sync event recording
func (s *SyncAwareStorage) SaveConfig(cfg *models.StoredConfig) error {
	ctx := context.Background()

	// Begin transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Save config using underlying storage
	if err := s.Storage.SaveConfig(cfg); err != nil {
		return err
	}

	// Record event
	eventData, err := json.Marshal(&ConfigEventData{
		ID:     cfg.ID,
		Handle: cfg.Configuration.Metadata.Name,
	})
	if err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionCreate,
		EntityID:            cfg.ID,
		EventData:           eventData,
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeAPI, event); err != nil {
		return err
	}

	// Update state version
	if _, err := s.stateManager.UpdateState(ctx, EntityTypeAPI, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// UpdateConfig updates an existing API configuration with sync event recording
func (s *SyncAwareStorage) UpdateConfig(cfg *models.StoredConfig) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.UpdateConfig(cfg); err != nil {
		return err
	}

	eventData, err := json.Marshal(&ConfigEventData{
		ID:     cfg.ID,
		Handle: cfg.Configuration.Metadata.Name,
	})
	if err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionUpdate,
		EntityID:            cfg.ID,
		EventData:           eventData,
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeAPI, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeAPI, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteConfig removes an API configuration with sync event recording
func (s *SyncAwareStorage) DeleteConfig(id string) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.DeleteConfig(id); err != nil {
		return err
	}

	eventData, err := json.Marshal(&ConfigEventData{
		ID:     id,
		Handle: "",
	})
	if err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionDelete,
		EntityID:            id,
		EventData:           eventData,
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeAPI, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeAPI, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// SaveCertificate persists a new certificate with sync event recording
func (s *SyncAwareStorage) SaveCertificate(cert *models.StoredCertificate) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.SaveCertificate(cert); err != nil {
		return err
	}

	eventData, err := json.Marshal(cert)
	if err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionCreate,
		EntityID:            cert.ID,
		EventData:           eventData,
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeCertificate, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeCertificate, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteCertificate removes a certificate with sync event recording
func (s *SyncAwareStorage) DeleteCertificate(id string) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.DeleteCertificate(id); err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionDelete,
		EntityID:            id,
		EventData:           []byte(`{}`),
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeCertificate, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeCertificate, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// SaveLLMProviderTemplate persists a new LLM provider template with sync event recording
func (s *SyncAwareStorage) SaveLLMProviderTemplate(template *models.StoredLLMProviderTemplate) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.SaveLLMProviderTemplate(template); err != nil {
		return err
	}

	eventData, err := json.Marshal(template)
	if err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionCreate,
		EntityID:            template.ID,
		EventData:           eventData,
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeLLMTemplate, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeLLMTemplate, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// UpdateLLMProviderTemplate updates an existing LLM provider template with sync event recording
func (s *SyncAwareStorage) UpdateLLMProviderTemplate(template *models.StoredLLMProviderTemplate) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.UpdateLLMProviderTemplate(template); err != nil {
		return err
	}

	eventData, err := json.Marshal(template)
	if err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionUpdate,
		EntityID:            template.ID,
		EventData:           eventData,
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeLLMTemplate, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeLLMTemplate, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}

// DeleteLLMProviderTemplate removes an LLM provider template with sync event recording
func (s *SyncAwareStorage) DeleteLLMProviderTemplate(id string) error {
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.Storage.DeleteLLMProviderTemplate(id); err != nil {
		return err
	}

	event := &Event{
		OriginatedTimestamp: time.Now(),
		OrganizationID:      s.organizationID,
		Action:              ActionDelete,
		EntityID:            id,
		EventData:           []byte(`{}`),
	}
	if err := s.eventStore.RecordEvent(ctx, EntityTypeLLMTemplate, event); err != nil {
		return err
	}

	if _, err := s.stateManager.UpdateState(ctx, EntityTypeLLMTemplate, s.organizationID); err != nil {
		return err
	}

	return tx.Commit()
}
