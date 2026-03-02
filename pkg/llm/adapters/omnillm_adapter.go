package adapters

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/plexusone/omnillm"
	"github.com/plexusone/omnillm/provider"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// OmniLLMAdapterConfig holds configuration for creating a OmniLLM adapter
type OmniLLMAdapterConfig struct {
	ProviderName      string
	APIKey            string `json:"-"` //nolint:gosec // G117: field name, not a hardcoded credential
	ModelName         string
	Timeout           time.Duration // HTTP timeout for API calls (0 = provider default)
	ObservabilityHook omnillm.ObservabilityHook
}

// OmniLLMAdapter adapts OmniLLM ChatClient to ADK's LLM interface
type OmniLLMAdapter struct {
	client *omnillm.ChatClient
	model  string
}

// NewOmniLLMAdapter creates a new OmniLLM adapter
func NewOmniLLMAdapter(providerName, apiKey, modelName string) (*OmniLLMAdapter, error) {
	return NewOmniLLMAdapterWithConfig(OmniLLMAdapterConfig{
		ProviderName: providerName,
		APIKey:       apiKey,
		ModelName:    modelName,
	})
}

// NewOmniLLMAdapterWithConfig creates a new OmniLLM adapter with full configuration
func NewOmniLLMAdapterWithConfig(cfg OmniLLMAdapterConfig) (*OmniLLMAdapter, error) {
	// For ollama, API key is optional
	if cfg.ProviderName != "ollama" && cfg.APIKey == "" {
		return nil, fmt.Errorf("%s API key is required", cfg.ProviderName)
	}

	config := omnillm.ClientConfig{
		Providers: []omnillm.ProviderConfig{
			{
				Provider: omnillm.ProviderName(cfg.ProviderName),
				APIKey:   cfg.APIKey,
				Timeout:  cfg.Timeout,
			},
		},
		ObservabilityHook: cfg.ObservabilityHook,
	}

	client, err := omnillm.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create OmniLLM client: %w", err)
	}

	return &OmniLLMAdapter{
		client: client,
		model:  cfg.ModelName,
	}, nil
}

// Name returns the model name
func (m *OmniLLMAdapter) Name() string {
	return m.model
}

// GenerateContent implements the LLM interface
func (m *OmniLLMAdapter) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		// Convert ADK request to OmniLLM request
		messages := make([]provider.Message, 0)

		for _, content := range req.Contents {
			var text string
			for _, part := range content.Parts {
				text += part.Text
			}

			role := provider.RoleUser
			if content.Role == "model" || content.Role == "assistant" {
				role = provider.RoleAssistant
			} else if content.Role == "system" {
				role = provider.RoleSystem
			}

			messages = append(messages, provider.Message{
				Role:    role,
				Content: text,
			})
		}

		// Create OmniLLM request
		omniReq := &provider.ChatCompletionRequest{
			Model:    m.model,
			Messages: messages,
		}

		// Call OmniLLM API
		// Note: The observability hook is called automatically by the ChatClient
		// (passed via ClientConfig.ObservabilityHook)
		resp, err := m.client.CreateChatCompletion(ctx, omniReq)

		if err != nil {
			yield(nil, fmt.Errorf("OmniLLM API error: %w", err))
			return
		}

		// Convert OmniLLM response to ADK response
		if len(resp.Choices) > 0 {
			adkResp := &model.LLMResponse{
				Content: &genai.Content{
					Parts: []*genai.Part{
						{Text: resp.Choices[0].Message.Content},
					},
				},
			}
			yield(adkResp, nil)
		}
	}
}
