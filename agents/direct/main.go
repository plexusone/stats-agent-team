package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/direct"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/models"
)

// DirectAgent provides HTTP API for direct LLM search
type DirectAgent struct {
	cfg       *config.Config
	directSvc *direct.LLMSearchService
	logger    *slog.Logger
}

// NewDirectAgent creates a new direct search agent
func NewDirectAgent(cfg *config.Config, logger *slog.Logger) (*DirectAgent, error) {
	directSvc, err := direct.NewLLMSearchService(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create direct search service: %w", err)
	}

	return &DirectAgent{
		cfg:       cfg,
		directSvc: directSvc,
		logger:    logger,
	}, nil
}

// DirectSearchInput represents the input for direct search
type DirectSearchInput struct {
	Body struct {
		Topic         string `json:"topic" minLength:"1" maxLength:"500" example:"climate change" doc:"Topic to search for statistics"`
		MinStats      int    `json:"min_stats,omitempty" minimum:"1" maximum:"100" default:"10" example:"10" doc:"Minimum number of statistics to find"`
		VerifyWithWeb bool   `json:"verify_with_web,omitempty" default:"false" example:"false" doc:"If true, verifies LLM claims with verification agent (requires verification agent running on port 8002)"`
	}
}

// DirectSearchOutput represents the output from direct search
type DirectSearchOutput struct {
	Body *models.OrchestrationResponse
}

// ErrorOutput represents an error response
type ErrorOutput struct {
	Body struct {
		Error   string `json:"error" example:"Invalid topic" doc:"Error message"`
		Message string `json:"message" example:"Topic must be at least 1 character" doc:"Detailed error message"`
	}
}

func main() {
	logger := logging.NewAgentLogger("direct")
	cfg := config.LoadConfig()

	directAgent, err := NewDirectAgent(cfg, logger)
	if err != nil {
		logger.Error("failed to create direct agent", "error", err)
		os.Exit(1)
	}

	// Create Chi router
	router := chi.NewMux()

	// Create Huma API
	api := humachi.New(router, huma.DefaultConfig("Statistics Direct Search API", "1.0.0"))

	// Configure API metadata
	api.OpenAPI().Info.Description = `Direct LLM-based statistics search service.

This service provides two modes:
1. **Direct Mode** (verify_with_web: false): Fast LLM search that returns statistics with source URLs
2. **Hybrid Mode** (verify_with_web: true): LLM search + web verification for accuracy

The service uses server-side LLM configuration, so clients don't need API keys.`

	api.OpenAPI().Info.Contact = &huma.Contact{
		Name: "Stats Agent Team",
		URL:  "https://github.com/plexusone/agent-team-stats",
	}

	// Add server information
	api.OpenAPI().Servers = []*huma.Server{
		{URL: "http://localhost:8005", Description: "Local development server"},
	}

	// Register the search operation
	huma.Register(api, huma.Operation{
		OperationID:   "search-statistics",
		Method:        http.MethodPost,
		Path:          "/search",
		Summary:       "Search for statistics on a topic",
		Description:   "Performs direct LLM search for statistics, optionally verifying claims with web scraping",
		Tags:          []string{"Statistics"},
		DefaultStatus: http.StatusOK,
	}, func(ctx context.Context, input *DirectSearchInput) (*DirectSearchOutput, error) {
		// Set defaults
		minStats := input.Body.MinStats
		if minStats == 0 {
			minStats = 10
		}

		directAgent.logger.Info("processing request",
			"topic", input.Body.Topic,
			"min_stats", minStats,
			"verify", input.Body.VerifyWithWeb)

		// Call direct search service
		resp, err := directAgent.directSvc.SearchStatisticsWithVerification(
			ctx,
			input.Body.Topic,
			minStats,
			input.Body.VerifyWithWeb,
		)
		if err != nil {
			directAgent.logger.Error("search failed", "error", err)
			return nil, huma.Error500InternalServerError(fmt.Sprintf("Search failed: %v", err))
		}

		directAgent.logger.Info("search completed",
			"verified", resp.VerifiedCount,
			"partial", resp.Partial)

		return &DirectSearchOutput{Body: resp}, nil
	})

	// Add health check endpoint
	huma.Register(api, huma.Operation{
		OperationID: "health-check",
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Health check endpoint",
		Description: "Returns OK if the service is healthy",
		Tags:        []string{"Health"},
	}, func(ctx context.Context, input *struct{}) (*struct {
		Body struct {
			Status string `json:"status" example:"OK" doc:"Service status"`
		}
	}, error) {
		return &struct {
			Body struct {
				Status string `json:"status" example:"OK" doc:"Service status"`
			}
		}{
			Body: struct {
				Status string `json:"status" example:"OK" doc:"Service status"`
			}{
				Status: "OK",
			},
		}, nil
	})

	logger.Info("HTTP server starting",
		"port", 8005,
		"llm_provider", cfg.LLMProvider,
		"llm_model", cfg.LLMModel,
		"docs_url", "http://localhost:8005/docs")

	// Create HTTP server with timeouts
	server := &http.Server{
		Addr:         ":8005",
		Handler:      router,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		logger.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}
