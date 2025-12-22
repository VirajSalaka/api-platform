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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/generated"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/policyxds"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
	policyenginev1 "github.com/wso2/api-platform/sdk/gateway/policyengine/v1"
	"go.uber.org/zap"
)

// EventProcessor handles applying sync events to local state
type EventProcessor struct {
	configStore     *storage.ConfigStore
	storage         storage.Storage
	snapshotManager *xds.SnapshotManager
	policyManager   *policyxds.PolicyManager
	routerConfig    *config.RouterConfig
	logger          *zap.Logger
}

// NewEventProcessor creates a new EventProcessor
func NewEventProcessor(
	configStore *storage.ConfigStore,
	storage storage.Storage,
	snapshotManager *xds.SnapshotManager,
	policyManager *policyxds.PolicyManager,
	routerConfig *config.RouterConfig,
	logger *zap.Logger,
) *EventProcessor {
	return &EventProcessor{
		configStore:     configStore,
		storage:         storage,
		snapshotManager: snapshotManager,
		policyManager:   policyManager,
		routerConfig:    routerConfig,
		logger:          logger,
	}
}

// ProcessAPIEvents handles API entity sync events
func (ep *EventProcessor) ProcessAPIEvents(entityType EntityType, events []Event) error {
	if entityType != EntityTypeAPI {
		return fmt.Errorf("unexpected entity type: %s", entityType)
	}

	// Track processed events for policy updates
	type processedEvent struct {
		action   Action
		entityID string
		config   *models.StoredConfig
	}
	processedEvents := make([]processedEvent, 0, len(events))

	for _, event := range events {
		cfg, err := ep.processAPIEvent(event)
		if err != nil {
			ep.logger.Error("Failed to process API event",
				zap.String("entity_id", event.EntityID),
				zap.String("action", string(event.Action)),
				zap.Error(err))
			// Continue processing other events
			continue
		}
		processedEvents = append(processedEvents, processedEvent{
			action:   event.Action,
			entityID: event.EntityID,
			config:   cfg,
		})
	}

	// Trigger xDS update after processing all events
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ep.snapshotManager.UpdateSnapshot(ctx, "sync-event"); err != nil {
		ep.logger.Error("Failed to update xDS snapshot after sync", zap.Error(err))
		return err
	}

	// Update policy manager if configured
	if ep.policyManager != nil {
		for _, pe := range processedEvents {
			ep.updatePolicyForEvent(pe.action, pe.entityID, pe.config)
		}
	}

	return nil
}

// processAPIEvent processes a single API event and returns the processed config
func (ep *EventProcessor) processAPIEvent(event Event) (*models.StoredConfig, error) {
	switch event.Action {
	case ActionCreate, ActionUpdate:
		var cfg models.StoredConfig
		if err := json.Unmarshal(event.EventData, &cfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal config: %w", err)
		}

		// Check if config exists in DATABASE (source of truth)
		existing, err := ep.storage.GetConfig(cfg.ID)
		if err == nil && existing != nil {
			// Update existing in in-memory store
			if err := ep.configStore.Update(existing); err != nil {
				return nil, err
			}
			return existing, nil
		}
		// Add new to in-memory store
		if err := ep.configStore.Add(existing); err != nil {
			return nil, err
		}
		return existing, nil

	case ActionDelete:
		if err := ep.configStore.Delete(event.EntityID); err != nil {
			return nil, err
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown action: %s", event.Action)
	}
}

// updatePolicyForEvent updates the policy manager based on the event action
func (ep *EventProcessor) updatePolicyForEvent(action Action, entityID string, cfg *models.StoredConfig) {
	switch action {
	case ActionCreate, ActionUpdate:
		if cfg == nil {
			return
		}
		storedPolicy := ep.derivePolicyFromConfig(cfg)
		if storedPolicy != nil {
			if err := ep.policyManager.AddPolicy(storedPolicy); err != nil {
				ep.logger.Error("Failed to add/update derived policy configuration",
					zap.String("policy_id", storedPolicy.ID),
					zap.Error(err))
			} else {
				ep.logger.Info("Derived policy configuration added/updated",
					zap.String("policy_id", storedPolicy.ID),
					zap.Int("route_count", len(storedPolicy.Configuration.Routes)))
			}
		} else if action == ActionUpdate {
			// Config was updated and no longer has policies, remove the existing policy
			policyID := cfg.ID + "-policies"
			if err := ep.policyManager.RemovePolicy(policyID); err != nil {
				ep.logger.Debug("No policy configuration to remove", zap.String("policy_id", policyID))
			} else {
				ep.logger.Info("Derived policy configuration removed (config no longer has policies)",
					zap.String("policy_id", policyID))
			}
		}

	case ActionDelete:
		policyID := entityID + "-policies"
		if err := ep.policyManager.RemovePolicy(policyID); err != nil {
			ep.logger.Debug("No policy configuration to remove", zap.String("policy_id", policyID))
		} else {
			ep.logger.Info("Derived policy configuration removed", zap.String("policy_id", policyID))
		}
	}
}

// derivePolicyFromConfig derives a policy configuration from a stored config
func (ep *EventProcessor) derivePolicyFromConfig(cfg *models.StoredConfig) *models.StoredPolicyConfig {
	if ep.routerConfig == nil {
		return nil
	}

	apiCfg := &cfg.Configuration

	// Collect API-level policies
	apiPolicies := make(map[string]policyenginev1.PolicyInstance)
	if cfg.GetPolicies() != nil {
		for _, p := range *cfg.GetPolicies() {
			apiPolicies[p.Name] = convertAPIPolicy(p)
		}
	}

	routes := make([]policyenginev1.PolicyChain, 0)

	switch apiCfg.Kind {
	case api.RestApi:
		apiData, err := apiCfg.Spec.AsAPIConfigData()
		if err != nil {
			return nil
		}

		for _, op := range apiData.Operations {
			var finalPolicies []policyenginev1.PolicyInstance

			if op.Policies != nil && len(*op.Policies) > 0 {
				finalPolicies = make([]policyenginev1.PolicyInstance, 0, len(*op.Policies))
				addedNames := make(map[string]struct{})

				for _, opPolicy := range *op.Policies {
					finalPolicies = append(finalPolicies, convertAPIPolicy(opPolicy))
					addedNames[opPolicy.Name] = struct{}{}
				}

				if apiData.Policies != nil {
					for _, apiPolicy := range *apiData.Policies {
						if _, exists := addedNames[apiPolicy.Name]; !exists {
							finalPolicies = append(finalPolicies, apiPolicies[apiPolicy.Name])
						}
					}
				}
			} else {
				if apiData.Policies != nil {
					finalPolicies = make([]policyenginev1.PolicyInstance, 0, len(*apiData.Policies))
					for _, p := range *apiData.Policies {
						finalPolicies = append(finalPolicies, apiPolicies[p.Name])
					}
				}
			}

			// Determine effective vhosts
			effectiveMainVHost := ep.routerConfig.VHosts.Main.Default
			effectiveSandboxVHost := ep.routerConfig.VHosts.Sandbox.Default
			if apiData.Vhosts != nil {
				if strings.TrimSpace(apiData.Vhosts.Main) != "" {
					effectiveMainVHost = apiData.Vhosts.Main
				}
				if apiData.Vhosts.Sandbox != nil && strings.TrimSpace(*apiData.Vhosts.Sandbox) != "" {
					effectiveSandboxVHost = *apiData.Vhosts.Sandbox
				}
			}

			vhosts := []string{effectiveMainVHost}
			if apiData.Upstream.Sandbox != nil && apiData.Upstream.Sandbox.Url != nil &&
				strings.TrimSpace(*apiData.Upstream.Sandbox.Url) != "" {
				vhosts = append(vhosts, effectiveSandboxVHost)
			}

			for _, vhost := range vhosts {
				routes = append(routes, policyenginev1.PolicyChain{
					RouteKey: xds.GenerateRouteName(string(op.Method), apiData.Context, apiData.Version, op.Path, vhost),
					Policies: finalPolicies,
				})
			}
		}
	}

	// If there are no policies at all, return nil
	policyCount := 0
	for _, r := range routes {
		policyCount += len(r.Policies)
	}
	if policyCount == 0 {
		return nil
	}

	now := time.Now().Unix()
	return &models.StoredPolicyConfig{
		ID: cfg.ID + "-policies",
		Configuration: policyenginev1.Configuration{
			Routes: routes,
			Metadata: policyenginev1.Metadata{
				CreatedAt:       now,
				UpdatedAt:       now,
				ResourceVersion: 0,
				APIName:         cfg.GetDisplayName(),
				Version:         cfg.GetVersion(),
				Context:         cfg.GetContext(),
			},
		},
		Version: 0,
	}
}

// convertAPIPolicy converts generated api.Policy to policyenginev1.PolicyInstance
func convertAPIPolicy(p api.Policy) policyenginev1.PolicyInstance {
	paramsMap := make(map[string]interface{})
	if p.Params != nil {
		for k, v := range *p.Params {
			paramsMap[k] = v
		}
	}
	return policyenginev1.PolicyInstance{
		Name:               p.Name,
		Version:            p.Version,
		Enabled:            true,
		ExecutionCondition: p.ExecutionCondition,
		Parameters:         paramsMap,
	}
}

// ProcessCertificateEvents handles Certificate entity sync events
func (ep *EventProcessor) ProcessCertificateEvents(entityType EntityType, events []Event) error {
	if entityType != EntityTypeCertificate {
		return fmt.Errorf("unexpected entity type: %s", entityType)
	}

	ep.logger.Info("Processing certificate sync events",
		zap.Int("event_count", len(events)))

	// Certificate sync would update certificate store and trigger SDS update
	// For now, we just log it as certificates are less frequently updated
	// and may need additional integration with SDS

	// TODO: Implement certificate event processing when needed
	ep.logger.Warn("Certificate sync event processing not yet implemented")

	return nil
}

// ProcessLLMTemplateEvents handles LLM Template entity sync events
func (ep *EventProcessor) ProcessLLMTemplateEvents(entityType EntityType, events []Event) error {
	if entityType != EntityTypeLLMTemplate {
		return fmt.Errorf("unexpected entity type: %s", entityType)
	}

	ep.logger.Info("Processing LLM template sync events",
		zap.Int("event_count", len(events)))

	// LLM template sync would update template store
	// For now, we just log it as templates are less frequently updated

	// TODO: Implement LLM template event processing when needed
	ep.logger.Warn("LLM template sync event processing not yet implemented")

	return nil
}
