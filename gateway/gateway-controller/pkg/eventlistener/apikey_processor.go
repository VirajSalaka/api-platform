/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
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

package eventlistener

import (
	"log/slog"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/eventhub"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
)

// processAPIKeyEvent dispatches API key events by action.
func (l *EventListener) processAPIKeyEvent(event eventhub.Event) {
	switch event.Action {
	case "CREATE":
		l.handleAPIKeyCreate(event)
	default:
		l.logger.Warn("Unknown API key event action",
			slog.String("action", event.Action),
			slog.String("entity_id", event.EntityID))
	}
}

// handleAPIKeyCreate handles API key create events from write-path async sync.
func (l *EventListener) handleAPIKeyCreate(event eventhub.Event) {
	keyID := event.EntityID

	l.logger.Info("Processing API key create event from another replica",
		slog.String("api_key_id", keyID),
		slog.String("correlation_id", event.CorrelationID))

	if l.db == nil {
		l.logger.Warn("Database not available, cannot process API key event",
			slog.String("api_key_id", keyID))
		return
	}
	if l.store == nil {
		l.logger.Warn("In-memory store not available, cannot process API key event",
			slog.String("api_key_id", keyID))
		return
	}

	apiKey, err := l.db.GetAPIKeyByID(keyID)
	if err != nil {
		if storage.IsNotFoundError(err) {
			l.logger.Warn("API key not found in database for create event",
				slog.String("api_key_id", keyID),
				slog.String("correlation_id", event.CorrelationID))
			return
		}

		l.logger.Error("Failed to fetch API key from database",
			slog.String("api_key_id", keyID),
			slog.Any("error", err))
		return
	}

	if err := l.store.StoreAPIKey(apiKey); err != nil {
		existing, getErr := l.store.GetAPIKeyByID(apiKey.APIId, apiKey.ID)
		if getErr == nil && existing != nil {
			l.logger.Debug("API key already exists in memory store, skipping duplicate create event",
				slog.String("api_key_id", keyID),
				slog.String("api_id", apiKey.APIId))
		} else {
			l.logger.Error("Failed to store API key in memory store",
				slog.String("api_key_id", keyID),
				slog.String("api_id", apiKey.APIId),
				slog.Any("error", err))
			return
		}
	}

	cfg, err := l.store.Get(apiKey.APIId)
	if err != nil {
		cfg, err = l.db.GetConfig(apiKey.APIId)
		if err != nil {
			l.logger.Error("Failed to resolve API for API key event",
				slog.String("api_key_id", keyID),
				slog.String("api_id", apiKey.APIId),
				slog.Any("error", err))
			return
		}

		if addErr := l.store.Add(cfg); addErr != nil {
			if updateErr := l.store.Update(cfg); updateErr != nil {
				l.logger.Warn("Failed to sync API config into memory store while processing API key event",
					slog.String("api_id", apiKey.APIId),
					slog.Any("add_error", addErr),
					slog.Any("update_error", updateErr))
			}
		}
	}

	apiConfig, err := cfg.Configuration.Spec.AsAPIConfigData()
	if err != nil {
		l.logger.Error("Failed to parse API configuration for API key xDS update",
			slog.String("api_id", cfg.ID),
			slog.String("api_key_id", keyID),
			slog.Any("error", err))
		return
	}

	if l.apiKeyXDSManager != nil {
		if err := l.apiKeyXDSManager.StoreAPIKey(cfg.ID, apiConfig.DisplayName, apiConfig.Version, apiKey, event.CorrelationID); err != nil {
			l.logger.Error("Failed to update API key in policy engine after replica sync",
				slog.String("api_id", cfg.ID),
				slog.String("api_key_id", keyID),
				slog.Any("error", err))
			return
		}
	}

	l.logger.Info("Successfully processed API key create event from replica",
		slog.String("api_id", cfg.ID),
		slog.String("api_key_id", keyID),
		slog.String("correlation_id", event.CorrelationID))
}
