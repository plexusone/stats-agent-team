// Package config provides configuration for stats-agent-team.
// It wraps agentkit/config to provide unified configuration loading
// from config.json, environment variables, and OmniVault secrets.
package config

import (
	"context"
	"os"
	"strconv"

	akconfig "github.com/plexusone/agentkit/config"
)

// Config holds the application configuration.
// It embeds agentkit's Config for core settings and adds
// stats-agent-team specific fields.
type Config struct {
	*akconfig.Config

	// Agent URLs (stats-agent-team specific)
	ResearchAgentURL     string
	SynthesisAgentURL    string
	VerificationAgentURL string
	OrchestratorURL      string
	OrchestratorEinoURL  string

	// Observability workspace (stats-agent-team specific)
	ObservabilityWorkspace string

	// HTTP Server Configuration
	HTTPTimeoutSeconds int
}

// Load loads configuration from config.json, environment variables, and OmniVault.
// This is the recommended way to load configuration as it:
//   - Reads settings from config.json (LLM_PROVIDER, SEARCH_PROVIDER, etc.)
//   - Allows environment variable overrides
//   - Loads secrets from OmniVault (API keys from env or AWS Secrets Manager)
func Load(ctx context.Context) (*Config, error) {
	// Load agentkit config (handles config.json + env + secrets)
	akCfg, err := akconfig.Load(ctx, akconfig.LoadOptions{})
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Config: akCfg,

		// Agent URLs from environment (or config.json agents section)
		ResearchAgentURL:     getEnv("RESEARCH_AGENT_URL", getAgentURL(akCfg, "research", "http://localhost:8001")),
		SynthesisAgentURL:    getEnv("SYNTHESIS_AGENT_URL", getAgentURL(akCfg, "synthesis", "http://localhost:8004")),
		VerificationAgentURL: getEnv("VERIFICATION_AGENT_URL", getAgentURL(akCfg, "verification", "http://localhost:8002")),
		OrchestratorURL:      getEnv("ORCHESTRATOR_URL", getAgentURL(akCfg, "orchestrator", "http://localhost:8000")),
		OrchestratorEinoURL:  getEnv("ORCHESTRATOR_EINO_URL", getAgentURL(akCfg, "orchestrator-eino", "http://localhost:8000")),

		// Observability workspace
		ObservabilityWorkspace: getEnv("OBSERVABILITY_WORKSPACE", getEnv("OPIK_WORKSPACE", getEnv("PHOENIX_SPACE_ID", ""))),

		// HTTP Server
		HTTPTimeoutSeconds: getEnvInt("HTTP_TIMEOUT_SECONDS", 300),
	}

	// Provider-specific observability settings
	if cfg.ObservabilityEnabled {
		switch cfg.ObservabilityProvider {
		case "phoenix":
			if phoenixKey := getEnv("PHOENIX_API_KEY", ""); phoenixKey != "" {
				cfg.ObservabilityAPIKey = phoenixKey
			}
			if spaceID := getEnv("PHOENIX_SPACE_ID", ""); spaceID != "" {
				cfg.ObservabilityWorkspace = spaceID
			}
		case "opik":
			if cfg.ObservabilityAPIKey == "" {
				cfg.ObservabilityAPIKey = getEnv("OPIK_API_KEY", "")
			}
			if cfg.ObservabilityWorkspace == "" {
				cfg.ObservabilityWorkspace = getEnv("OPIK_WORKSPACE", "")
			}
		}
	}

	return cfg, nil
}

// LoadConfig loads configuration from environment variables only.
// Deprecated: Use Load(ctx) instead for config.json and OmniVault support.
func LoadConfig() *Config {
	ctx := context.Background()
	cfg, err := Load(ctx)
	if err != nil {
		// Fall back to env-only loading for backward compatibility
		return loadFromEnvOnly()
	}
	return cfg
}

// loadFromEnvOnly loads configuration from environment variables only.
// This is used as a fallback when config.json or OmniVault is unavailable.
func loadFromEnvOnly() *Config {
	provider := getEnv("LLM_PROVIDER", "gemini")

	// Create a minimal agentkit config from env vars
	akCfg := &akconfig.Config{
		LLMProvider: provider,
		LLMAPIKey:   getEnv("LLM_API_KEY", ""),
		LLMModel:    getEnv("LLM_MODEL", akconfig.GetDefaultModel(provider)),
		LLMBaseURL:  getEnv("LLM_BASE_URL", ""),

		GeminiAPIKey: getEnv("GEMINI_API_KEY", getEnv("GOOGLE_API_KEY", "")),
		ClaudeAPIKey: getEnv("CLAUDE_API_KEY", getEnv("ANTHROPIC_API_KEY", "")),
		OpenAIAPIKey: getEnv("OPENAI_API_KEY", ""),
		XAIAPIKey:    getEnv("XAI_API_KEY", ""),
		OllamaURL:    getEnv("OLLAMA_URL", "http://localhost:11434"),

		SearchProvider: getEnv("SEARCH_PROVIDER", "serper"),
		SerperAPIKey:   getEnv("SERPER_API_KEY", ""),
		SerpAPIKey:     getEnv("SERPAPI_API_KEY", ""),

		A2AEnabled:   getEnv("A2A_ENABLED", "true") == "true",
		A2AAuthType:  getEnv("A2A_AUTH_TYPE", "apikey"),
		A2AAuthToken: getEnv("A2A_AUTH_TOKEN", ""),

		ObservabilityEnabled:  getEnv("OBSERVABILITY_ENABLED", "false") == "true",
		ObservabilityProvider: getEnv("OBSERVABILITY_PROVIDER", "opik"),
		ObservabilityAPIKey:   getEnv("OBSERVABILITY_API_KEY", getEnv("OPIK_API_KEY", getEnv("PHOENIX_API_KEY", ""))),
		ObservabilityEndpoint: getEnv("OBSERVABILITY_ENDPOINT", ""),
		ObservabilityProject:  getEnv("OBSERVABILITY_PROJECT", "stats-agent-team"),
	}

	// Set LLMAPIKey based on provider if not explicitly set
	if akCfg.LLMAPIKey == "" {
		switch provider {
		case "gemini":
			akCfg.LLMAPIKey = akCfg.GeminiAPIKey
		case "claude":
			akCfg.LLMAPIKey = akCfg.ClaudeAPIKey
		case "openai":
			akCfg.LLMAPIKey = akCfg.OpenAIAPIKey
		case "xai":
			akCfg.LLMAPIKey = akCfg.XAIAPIKey
		}
	}

	// Set LLMBaseURL for Ollama if not explicitly set
	if akCfg.LLMBaseURL == "" && provider == "ollama" {
		akCfg.LLMBaseURL = akCfg.OllamaURL
	}

	cfg := &Config{
		Config: akCfg,

		ResearchAgentURL:     getEnv("RESEARCH_AGENT_URL", "http://localhost:8001"),
		SynthesisAgentURL:    getEnv("SYNTHESIS_AGENT_URL", "http://localhost:8004"),
		VerificationAgentURL: getEnv("VERIFICATION_AGENT_URL", "http://localhost:8002"),
		OrchestratorURL:      getEnv("ORCHESTRATOR_URL", "http://localhost:8000"),
		OrchestratorEinoURL:  getEnv("ORCHESTRATOR_EINO_URL", "http://localhost:8000"),

		ObservabilityWorkspace: getEnv("OBSERVABILITY_WORKSPACE", getEnv("OPIK_WORKSPACE", getEnv("PHOENIX_SPACE_ID", ""))),

		HTTPTimeoutSeconds: getEnvInt("HTTP_TIMEOUT_SECONDS", 300),
	}

	// Provider-specific observability settings
	if cfg.ObservabilityEnabled {
		switch cfg.ObservabilityProvider {
		case "phoenix":
			if phoenixKey := getEnv("PHOENIX_API_KEY", ""); phoenixKey != "" {
				cfg.ObservabilityAPIKey = phoenixKey
			}
			if spaceID := getEnv("PHOENIX_SPACE_ID", ""); spaceID != "" {
				cfg.ObservabilityWorkspace = spaceID
			}
		case "opik":
			if cfg.ObservabilityAPIKey == "" {
				cfg.ObservabilityAPIKey = getEnv("OPIK_API_KEY", "")
			}
			if cfg.ObservabilityWorkspace == "" {
				cfg.ObservabilityWorkspace = getEnv("OPIK_WORKSPACE", "")
			}
		}
	}

	return cfg
}

// getAgentURL gets an agent URL from agentkit config or returns default.
func getAgentURL(cfg *akconfig.Config, name, defaultURL string) string {
	if url := cfg.GetAgentURL(name); url != "" {
		return url
	}
	return defaultURL
}

// getEnv gets an environment variable or returns a default value.
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt gets an environment variable as int or returns a default value.
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}
