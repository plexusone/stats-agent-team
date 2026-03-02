package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/httpclient"
	"github.com/plexusone/agent-team-stats/pkg/llm"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/models"
)

// OrchestrationAgent uses ADK to coordinate research and verification agents
type OrchestrationAgent struct {
	cfg      *config.Config
	client   *http.Client
	adkAgent agent.Agent
	logger   *slog.Logger
}

// OrchestrationInput defines input for orchestration tool
type OrchestrationInput struct {
	Topic            string `json:"topic"`
	MinVerifiedStats int    `json:"min_verified_stats"`
	MaxCandidates    int    `json:"max_candidates"`
	ReputableOnly    bool   `json:"reputable_only"`
}

// OrchestrationToolOutput defines output from orchestration tool
type OrchestrationToolOutput struct {
	Response *models.OrchestrationResponse `json:"response"`
}

// NewOrchestrationAgent creates a new ADK-based orchestration agent
func NewOrchestrationAgent(cfg *config.Config, logger *slog.Logger) (*OrchestrationAgent, error) {
	ctx := logging.WithLogger(context.Background(), logger)

	// Create model using factory
	modelFactory := llm.NewModelFactory(ctx, cfg)
	model, err := modelFactory.CreateModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create model: %w", err)
	}

	logger.Info("agent initialized", "provider", modelFactory.GetProviderInfo())

	oa := &OrchestrationAgent{
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second},
		logger: logger,
	}

	// Create orchestration tool
	orchestrationTool, err := functiontool.New(functiontool.Config{
		Name:        "orchestrate_statistics_workflow",
		Description: "Coordinates research and verification agents to find verified statistics on a topic",
	}, oa.orchestrationToolHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to create orchestration tool: %w", err)
	}

	// Create ADK agent
	adkAgent, err := llmagent.New(llmagent.Config{
		Name:        "statistics_orchestration_agent",
		Model:       model,
		Description: "Orchestrates multi-agent workflow to find and verify statistics",
		Instruction: `You are a statistics orchestration agent. Your job is to:
1. Coordinate the research agent to find candidate statistics
2. Send candidates to the verification agent for validation
3. Retry if needed to meet the target number of verified statistics
4. Return a final set of verified statistics with sources

Workflow:
- Request statistics from research agent based on topic
- Send candidates to verification agent
- Collect verified statistics
- If target not met and retries available, request more candidates
- Build final response with all verified statistics`,
		Tools: []tool.Tool{orchestrationTool},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK agent: %w", err)
	}

	oa.adkAgent = adkAgent

	return oa, nil
}

// orchestrationToolHandler implements the orchestration logic
func (oa *OrchestrationAgent) orchestrationToolHandler(ctx tool.Context, input OrchestrationInput) (OrchestrationToolOutput, error) {
	oa.logger.Info("starting orchestration",
		"topic", input.Topic,
		"target", input.MinVerifiedStats,
		"max_candidates", input.MaxCandidates)

	req := &models.OrchestrationRequest{
		Topic:            input.Topic,
		MinVerifiedStats: input.MinVerifiedStats,
		MaxCandidates:    input.MaxCandidates,
		ReputableOnly:    input.ReputableOnly,
	}

	// Use background context since tool.Context is different
	bgCtx := context.Background()
	response, err := oa.orchestrate(bgCtx, req)
	if err != nil {
		return OrchestrationToolOutput{}, fmt.Errorf("orchestration failed: %w", err)
	}

	return OrchestrationToolOutput{
		Response: response,
	}, nil
}

// orchestrate coordinates the workflow to find verified statistics
func (oa *OrchestrationAgent) orchestrate(ctx context.Context, req *models.OrchestrationRequest) (*models.OrchestrationResponse, error) {
	var allCandidates []models.CandidateStatistic
	var verifiedStatistics []models.Statistic
	totalVerified := 0
	totalFailed := 0
	maxRetries := 3
	retry := 0

	for retry < maxRetries && totalVerified < req.MinVerifiedStats {
		// Calculate how many more candidates we need
		candidatesNeeded := req.MinVerifiedStats - totalVerified
		if candidatesNeeded < 5 {
			candidatesNeeded = 5 // Always request at least 5 for buffer
		}

		// Don't exceed max candidates
		candidatesLeft := req.MaxCandidates - len(allCandidates)
		if candidatesLeft <= 0 {
			oa.logger.Info("reached maximum candidates limit", "max", req.MaxCandidates)
			break
		}
		if candidatesNeeded > candidatesLeft {
			candidatesNeeded = candidatesLeft
		}

		// Step 1: Request sources from research agent
		researchReq := &models.ResearchRequest{
			Topic:         req.Topic,
			MinStatistics: candidatesNeeded,
			MaxStatistics: candidatesNeeded + 5,
			ReputableOnly: req.ReputableOnly,
		}

		oa.logger.Info("requesting sources from research agent",
			"needed", candidatesNeeded,
			"attempt", retry+1,
			"max_retries", maxRetries)

		researchResp, err := oa.callResearchAgent(ctx, researchReq)
		if err != nil {
			oa.logger.Warn("research agent failed", "error", err)
			retry++
			continue
		}

		// Convert candidates to search results (research agent returns placeholder candidates now)
		searchResults := make([]models.SearchResult, 0, len(researchResp.Candidates))
		for _, cand := range researchResp.Candidates {
			searchResults = append(searchResults, models.SearchResult{
				URL:     cand.SourceURL,
				Title:   cand.Name,
				Snippet: cand.Excerpt,
				Domain:  cand.Source,
			})
		}

		oa.logger.Info("received sources from research agent", "count", len(searchResults))

		// Step 2: Send sources to synthesis agent to extract statistics
		synthesisReq := &models.SynthesisRequest{
			Topic:         req.Topic,
			SearchResults: searchResults,
			MinStatistics: candidatesNeeded,
			MaxStatistics: candidatesNeeded + 5,
		}

		oa.logger.Info("sending sources to synthesis agent", "count", len(searchResults))

		synthesisResp, err := oa.callSynthesisAgent(ctx, synthesisReq)
		if err != nil {
			oa.logger.Warn("synthesis agent failed", "error", err)
			retry++
			continue
		}

		oa.logger.Info("synthesis extracted candidates", "count", len(synthesisResp.Candidates))
		allCandidates = append(allCandidates, synthesisResp.Candidates...)

		// Step 3: Send candidates to verification agent
		verifyReq := &models.VerificationRequest{
			Candidates: synthesisResp.Candidates,
		}

		oa.logger.Info("sending candidates to verification agent", "count", len(verifyReq.Candidates))

		verifyResp, err := oa.callVerificationAgent(ctx, verifyReq)
		if err != nil {
			oa.logger.Warn("verification agent failed", "error", err)
			retry++
			continue
		}

		oa.logger.Info("verification complete",
			"verified", verifyResp.Verified,
			"failed", verifyResp.Failed)

		// Step 3: Collect verified statistics
		for _, result := range verifyResp.Results {
			if result.Verified {
				verifiedStatistics = append(verifiedStatistics, *result.Statistic)
				totalVerified++
			} else {
				totalFailed++
				oa.logger.Debug("statistic failed verification",
					"name", result.Statistic.Name,
					"reason", result.Reason)
			}
		}

		oa.logger.Info("progress update",
			"verified", totalVerified,
			"target", req.MinVerifiedStats)

		// Check if we have enough verified statistics to stop gathering more
		if totalVerified >= req.MinVerifiedStats {
			oa.logger.Info("minimum target reached",
				"verified", totalVerified)
			break
		}

		retry++
	}

	// Build final response with ALL verified statistics (not limited to MinVerifiedStats)
	response := &models.OrchestrationResponse{
		Topic:           req.Topic,
		Statistics:      verifiedStatistics, // Returns ALL verified statistics found
		TotalCandidates: len(allCandidates),
		VerifiedCount:   totalVerified,
		FailedCount:     totalFailed,
		Timestamp:       time.Now(),
	}

	if totalVerified < req.MinVerifiedStats {
		oa.logger.Warn("below target",
			"verified", totalVerified,
			"target", req.MinVerifiedStats)
	} else {
		oa.logger.Info("orchestration completed",
			"verified", totalVerified,
			"target", req.MinVerifiedStats)
	}

	return response, nil
}

// callResearchAgent calls the research agent via HTTP
func (oa *OrchestrationAgent) callResearchAgent(ctx context.Context, req *models.ResearchRequest) (*models.ResearchResponse, error) {
	var resp models.ResearchResponse
	url := fmt.Sprintf("%s/research", oa.cfg.ResearchAgentURL)
	if err := httpclient.PostJSON(ctx, oa.client, url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// callSynthesisAgent calls the synthesis agent via HTTP
func (oa *OrchestrationAgent) callSynthesisAgent(ctx context.Context, req *models.SynthesisRequest) (*models.SynthesisResponse, error) {
	var resp models.SynthesisResponse
	url := fmt.Sprintf("%s/synthesize", oa.cfg.SynthesisAgentURL)
	if err := httpclient.PostJSON(ctx, oa.client, url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// callVerificationAgent calls the verification agent via HTTP
func (oa *OrchestrationAgent) callVerificationAgent(ctx context.Context, req *models.VerificationRequest) (*models.VerificationResponse, error) {
	var resp models.VerificationResponse
	url := fmt.Sprintf("%s/verify", oa.cfg.VerificationAgentURL)
	if err := httpclient.PostJSON(ctx, oa.client, url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Orchestrate is the public method for orchestrating the workflow
func (oa *OrchestrationAgent) Orchestrate(ctx context.Context, req *models.OrchestrationRequest) (*models.OrchestrationResponse, error) {
	return oa.orchestrate(ctx, req)
}

// HandleOrchestrationRequest is the HTTP handler for orchestration requests
func (oa *OrchestrationAgent) HandleOrchestrationRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.OrchestrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Set defaults
	if req.MinVerifiedStats == 0 {
		req.MinVerifiedStats = 10
	}
	if req.MaxCandidates == 0 {
		req.MaxCandidates = 30
	}

	resp, err := oa.Orchestrate(r.Context(), &req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Orchestration failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		oa.logger.Error("failed to encode response", "error", err)
	}
}

func main() {
	logger := logging.NewAgentLogger("orchestration")
	cfg := config.LoadConfig()

	orchestrationAgent, err := NewOrchestrationAgent(cfg, logger)
	if err != nil {
		logger.Error("failed to create orchestration agent", "error", err)
		os.Exit(1)
	}

	// Start A2A server if enabled (standard protocol for agent interoperability)
	if cfg.A2AEnabled {
		a2aServer, err := NewA2AServer(orchestrationAgent, "9000", logger)
		if err != nil {
			logger.Error("failed to create A2A server", "error", err)
		} else {
			go func() {
				if err := a2aServer.Start(context.Background()); err != nil {
					logger.Error("A2A server error", "error", err)
				}
			}()
			logger.Info("A2A server started", "port", 9000)
		}
	}

	// Start HTTP server with timeout (for custom security: SPIFFE, KYA, XAA, and observability)
	server := &http.Server{
		Addr:         ":8000",
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	http.HandleFunc("/orchestrate", orchestrationAgent.HandleOrchestrationRequest)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			logger.Error("failed to write health response", "error", err)
		}
	})

	logger.Info("HTTP server starting",
		"port", 8000,
		"mode", "dual (HTTP + A2A)")
	if err := server.ListenAndServe(); err != nil {
		logger.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}
