package llm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/grokify/mogo/log/slogutil"
	"github.com/plexusone/omnillm"
	omnillmhook "github.com/plexusone/omniobserve/integrations/omnillm"
	"github.com/plexusone/omniobserve/llmops"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/genai"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/llm/adapters"

	// Import observability providers (driver registration via init())
	// TODO: move to build tags for smaller binaries
	_ "github.com/plexusone/omniobserve/llmops/langfuse"
	_ "github.com/plexusone/opik-go/llmops"
	_ "github.com/plexusone/phoenix-go/llmops"
)

// ModelFactory creates LLM models based on configuration
type ModelFactory struct {
	cfg      *config.Config
	logger   *slog.Logger
	obsHook  omnillm.ObservabilityHook
	obsClose func() error
}

// NewModelFactory creates a new model factory.
// The logger is retrieved from ctx using slogutil.LoggerFromContext.
func NewModelFactory(ctx context.Context, cfg *config.Config) *ModelFactory {
	logger := slogutil.LoggerFromContext(ctx, slog.Default())
	mf := &ModelFactory{
		cfg:    cfg,
		logger: logger.With("component", "model-factory"),
	}

	// Initialize observability if enabled
	if cfg.ObservabilityEnabled && cfg.ObservabilityProvider != "" {
		hook, closeFn := mf.initObservability()
		mf.obsHook = hook
		mf.obsClose = closeFn
	}

	return mf
}

// initObservability initializes the observability provider and returns a hook
func (mf *ModelFactory) initObservability() (omnillm.ObservabilityHook, func() error) {
	opts := []llmops.ClientOption{
		llmops.WithProjectName(mf.cfg.ObservabilityProject),
	}

	if mf.cfg.ObservabilityAPIKey != "" {
		opts = append(opts, llmops.WithAPIKey(mf.cfg.ObservabilityAPIKey))
	}

	if mf.cfg.ObservabilityEndpoint != "" {
		opts = append(opts, llmops.WithEndpoint(mf.cfg.ObservabilityEndpoint))
	}

	if mf.cfg.ObservabilityWorkspace != "" {
		opts = append(opts, llmops.WithWorkspace(mf.cfg.ObservabilityWorkspace))
	}

	provider, err := llmops.Open(mf.cfg.ObservabilityProvider, opts...)
	if err != nil {
		// Log warning but don't fail - observability is optional
		mf.logger.Warn("failed to initialize observability provider",
			"provider", mf.cfg.ObservabilityProvider,
			"error", err)
		return nil, nil
	}

	// Ensure project exists (some providers require this)
	ctx := context.Background()
	if _, err = provider.CreateProject(ctx, mf.cfg.ObservabilityProject); err != nil {
		// Ignore error - project may already exist
		mf.logger.Debug("CreateProject returned error (may already exist)", "error", err)
	}

	// Set the project as active
	if err := provider.SetProject(ctx, mf.cfg.ObservabilityProject); err != nil {
		mf.logger.Warn("failed to set observability project", "project", mf.cfg.ObservabilityProject, "error", err)
	}

	hook := omnillmhook.NewHook(provider)

	closeFn := func() error {
		return provider.Close()
	}

	return hook, closeFn
}

// Close cleans up resources (call when factory is no longer needed)
func (mf *ModelFactory) Close() error {
	if mf.obsClose != nil {
		return mf.obsClose()
	}
	return nil
}

// CreateModel creates an LLM model based on the configured provider
func (mf *ModelFactory) CreateModel(ctx context.Context) (model.LLM, error) {
	switch mf.cfg.LLMProvider {
	case "gemini", "":
		return mf.createGeminiModel(ctx)
	case "claude":
		return mf.createClaudeModel()
	case "openai":
		return mf.createOpenAIModel()
	case "xai":
		return mf.createXAIModel()
	case "ollama":
		return mf.createOllamaModel()
	default:
		return nil, fmt.Errorf("unsupported LLM provider: %s (supported: gemini, claude, openai, xai, ollama)", mf.cfg.LLMProvider)
	}
}

// createGeminiModel creates a Gemini model
func (mf *ModelFactory) createGeminiModel(ctx context.Context) (model.LLM, error) {
	apiKey := mf.cfg.GeminiAPIKey
	if apiKey == "" {
		apiKey = mf.cfg.LLMAPIKey
	}

	if apiKey == "" {
		return nil, fmt.Errorf("gemini API key not set - please set GOOGLE_API_KEY or GEMINI_API_KEY")
	}

	modelName := mf.cfg.LLMModel
	if modelName == "" {
		modelName = "gemini-2.0-flash-exp"
	}

	return gemini.NewModel(ctx, modelName, &genai.ClientConfig{
		APIKey: apiKey,
	})
}

// createClaudeModel creates a Claude model using OmniLLM
func (mf *ModelFactory) createClaudeModel() (model.LLM, error) {
	apiKey := mf.cfg.ClaudeAPIKey
	if apiKey == "" {
		apiKey = mf.cfg.LLMAPIKey
	}

	if apiKey == "" {
		return nil, fmt.Errorf("claude API key not set - please set CLAUDE_API_KEY or ANTHROPIC_API_KEY")
	}

	modelName := mf.cfg.LLMModel
	if modelName == "" {
		modelName = "claude-3-5-sonnet-20241022"
	}

	return adapters.NewOmniLLMAdapterWithConfig(adapters.OmniLLMAdapterConfig{
		ProviderName:      "anthropic",
		APIKey:            apiKey,
		ModelName:         modelName,
		Timeout:           mf.getTimeout(),
		ObservabilityHook: mf.obsHook,
	})
}

// createOpenAIModel creates an OpenAI model using OmniLLM
func (mf *ModelFactory) createOpenAIModel() (model.LLM, error) {
	apiKey := mf.cfg.OpenAIAPIKey
	if apiKey == "" {
		apiKey = mf.cfg.LLMAPIKey
	}

	if apiKey == "" {
		return nil, fmt.Errorf("openai API key not set - please set OPENAI_API_KEY")
	}

	modelName := mf.cfg.LLMModel
	if modelName == "" {
		modelName = "gpt-4o-mini" // Use mini for cost efficiency
	}

	return adapters.NewOmniLLMAdapterWithConfig(adapters.OmniLLMAdapterConfig{
		ProviderName:      "openai",
		APIKey:            apiKey,
		ModelName:         modelName,
		Timeout:           mf.getTimeout(),
		ObservabilityHook: mf.obsHook,
	})
}

// createXAIModel creates an xAI Grok model using OmniLLM
func (mf *ModelFactory) createXAIModel() (model.LLM, error) {
	apiKey := mf.cfg.XAIAPIKey
	if apiKey == "" {
		apiKey = mf.cfg.LLMAPIKey
	}

	if apiKey == "" {
		return nil, fmt.Errorf("xAI API key not set - please set XAI_API_KEY")
	}

	modelName := mf.cfg.LLMModel
	if modelName == "" {
		modelName = "grok-3"
	}

	return adapters.NewOmniLLMAdapterWithConfig(adapters.OmniLLMAdapterConfig{
		ProviderName:      "xai",
		APIKey:            apiKey,
		ModelName:         modelName,
		Timeout:           mf.getTimeout(),
		ObservabilityHook: mf.obsHook,
	})
}

// createOllamaModel creates an Ollama model using OmniLLM
func (mf *ModelFactory) createOllamaModel() (model.LLM, error) {
	modelName := mf.cfg.LLMModel
	if modelName == "" {
		modelName = "llama3.2"
	}

	// Ollama doesn't need an API key for local instances
	// OmniLLM will use the base URL from environment or default to localhost
	return adapters.NewOmniLLMAdapterWithConfig(adapters.OmniLLMAdapterConfig{
		ProviderName:      "ollama",
		APIKey:            "",
		ModelName:         modelName,
		Timeout:           mf.getTimeout(),
		ObservabilityHook: mf.obsHook,
	})
}

// getTimeout returns the configured HTTP timeout for LLM API calls
func (mf *ModelFactory) getTimeout() time.Duration {
	if mf.cfg.HTTPTimeoutSeconds > 0 {
		return time.Duration(mf.cfg.HTTPTimeoutSeconds) * time.Second
	}
	return 0 // Let provider use its default
}

// GetProviderInfo returns information about the current provider
func (mf *ModelFactory) GetProviderInfo() string {
	return fmt.Sprintf("Provider: %s, Model: %s", mf.cfg.LLMProvider, mf.cfg.LLMModel)
}
