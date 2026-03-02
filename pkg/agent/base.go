package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/llm"
	"github.com/plexusone/agent-team-stats/pkg/logging"
)

// BaseAgent provides common functionality for all agents
type BaseAgent struct {
	Cfg          *config.Config
	Client       *http.Client
	Model        model.LLM
	ModelFactory *llm.ModelFactory
	Logger       *slog.Logger
}

// NewBaseAgent creates a new base agent with LLM initialization
func NewBaseAgent(ctx context.Context, cfg *config.Config, timeoutSec int) (*BaseAgent, error) {
	logger := logging.FromContext(ctx)

	// Create model using factory
	modelFactory := llm.NewModelFactory(ctx, cfg)
	llmModel, err := modelFactory.CreateModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create model: %w", err)
	}

	return &BaseAgent{
		Cfg:          cfg,
		Client:       &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
		Model:        llmModel,
		ModelFactory: modelFactory,
		Logger:       logger,
	}, nil
}

// NewBaseAgentWithLogger creates a new base agent with an explicit logger
func NewBaseAgentWithLogger(ctx context.Context, cfg *config.Config, timeoutSec int, logger *slog.Logger) (*BaseAgent, error) {
	// Ensure context has the logger for model factory
	ctx = logging.WithLogger(ctx, logger)

	// Create model using factory
	modelFactory := llm.NewModelFactory(ctx, cfg)
	llmModel, err := modelFactory.CreateModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create model: %w", err)
	}

	return &BaseAgent{
		Cfg:          cfg,
		Client:       &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
		Model:        llmModel,
		ModelFactory: modelFactory,
		Logger:       logger,
	}, nil
}

// GetProviderInfo returns information about the LLM provider
func (ba *BaseAgent) GetProviderInfo() string {
	return ba.ModelFactory.GetProviderInfo()
}

// Close cleans up resources including flushing observability data
func (ba *BaseAgent) Close() error {
	if ba.ModelFactory != nil {
		return ba.ModelFactory.Close()
	}
	return nil
}

// FetchURL fetches content from a URL with proper error handling
func (ba *BaseAgent) FetchURL(ctx context.Context, url string, maxSizeMB int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "StatsAgentTeam/1.0")

	resp, err := ba.Client.Do(req) //nolint:gosec // G704: URL provided by caller for web scraping
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Limit response size
	maxBytes := int64(maxSizeMB * 1024 * 1024)
	limitedReader := io.LimitReader(resp.Body, maxBytes)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	return string(body), nil
}

// Info logs an informational message
func (ba *BaseAgent) Info(msg string, args ...any) {
	ba.Logger.Info(msg, args...)
}

// Error logs an error message
func (ba *BaseAgent) Error(msg string, args ...any) {
	ba.Logger.Error(msg, args...)
}

// Debug logs a debug message
func (ba *BaseAgent) Debug(msg string, args ...any) {
	ba.Logger.Debug(msg, args...)
}

// Warn logs a warning message
func (ba *BaseAgent) Warn(msg string, args ...any) {
	ba.Logger.Warn(msg, args...)
}

// AgentWrapper wraps common agent initialization patterns
type AgentWrapper struct {
	*BaseAgent
	ADKAgent agent.Agent
}

// NewAgentWrapper creates a wrapper with both base functionality and ADK agent
func NewAgentWrapper(base *BaseAgent, adkAgent agent.Agent) *AgentWrapper {
	return &AgentWrapper{
		BaseAgent: base,
		ADKAgent:  adkAgent,
	}
}
