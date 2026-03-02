package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cloudwego/eino/compose"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/httpclient"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/models"
)

// EinoOrchestrationAgent uses Eino framework for deterministic orchestration
type EinoOrchestrationAgent struct {
	cfg    *config.Config
	client *http.Client
	graph  *compose.Graph[*models.OrchestrationRequest, *models.OrchestrationResponse]
	logger *slog.Logger
}

// NewEinoOrchestrationAgent creates a new Eino-based orchestration agent
func NewEinoOrchestrationAgent(cfg *config.Config, logger *slog.Logger) *EinoOrchestrationAgent {
	if logger == nil {
		logger = logging.NewAgentLogger("eino-orchestrator")
	}

	oa := &EinoOrchestrationAgent{
		cfg:    cfg,
		client: &http.Client{Timeout: time.Duration(cfg.HTTPTimeoutSeconds) * time.Second},
		logger: logger,
	}

	// Build the deterministic workflow graph
	oa.graph = oa.buildWorkflowGraph()

	return oa
}

// buildWorkflowGraph creates a deterministic Eino graph for the workflow
func (oa *EinoOrchestrationAgent) buildWorkflowGraph() *compose.Graph[*models.OrchestrationRequest, *models.OrchestrationResponse] {
	// Create a new graph with typed input/output
	g := compose.NewGraph[*models.OrchestrationRequest, *models.OrchestrationResponse]()

	// Node names
	const (
		nodeValidateInput  = "validate_input"
		nodeResearch       = "research"
		nodeSynthesis      = "synthesis"
		nodeVerification   = "verification"
		nodeCheckQuality   = "check_quality"
		nodeRetryResearch  = "retry_research"
		nodeFormatResponse = "format_response"
	)

	// Add Lambda nodes for each step in the workflow

	// 1. Validate Input Node
	validateInputLambda := compose.InvokableLambda(func(ctx context.Context, req *models.OrchestrationRequest) (*models.OrchestrationRequest, error) {
		logger := logging.FromContext(ctx)
		logger.Info("validating input", "topic", req.Topic)

		// Set defaults
		if req.MinVerifiedStats == 0 {
			req.MinVerifiedStats = 10
		}
		if req.MaxCandidates == 0 {
			req.MaxCandidates = 30
		}

		return req, nil
	})
	if err := g.AddLambdaNode(nodeValidateInput, validateInputLambda); err != nil {
		oa.logger.Warn("failed to add validate input node", "error", err)
	}

	// 2. Research Node - calls research agent to find sources (URLs)
	researchLambda := compose.InvokableLambda(func(ctx context.Context, req *models.OrchestrationRequest) (*ResearchState, error) {
		logger := logging.FromContext(ctx)
		logger.Info("executing research", "topic", req.Topic)

		researchReq := &models.ResearchRequest{
			Topic:         req.Topic,
			MinStatistics: req.MinVerifiedStats,
			MaxStatistics: req.MaxCandidates,
			ReputableOnly: req.ReputableOnly,
		}

		resp, err := oa.callResearchAgent(ctx, researchReq)
		if err != nil {
			return nil, fmt.Errorf("research failed: %w", err)
		}

		// Convert candidates to search results
		searchResults := make([]models.SearchResult, 0, len(resp.Candidates))
		for _, cand := range resp.Candidates {
			searchResults = append(searchResults, models.SearchResult{
				URL:     cand.SourceURL,
				Title:   cand.Name,
				Snippet: cand.Excerpt,
				Domain:  cand.Source,
			})
		}

		logger.Info("research completed", "sources", len(searchResults))

		return &ResearchState{
			Request:       req,
			SearchResults: searchResults,
		}, nil
	})
	if err := g.AddLambdaNode(nodeResearch, researchLambda); err != nil {
		oa.logger.Warn("failed to add research node", "error", err)
	}

	// 3. Synthesis Node - calls synthesis agent to extract statistics
	synthesisLambda := compose.InvokableLambda(func(ctx context.Context, state *ResearchState) (*SynthesisState, error) {
		logger := logging.FromContext(ctx)
		logger.Info("synthesizing statistics", "sources", len(state.SearchResults))

		synthesisReq := &models.SynthesisRequest{
			Topic:         state.Request.Topic,
			SearchResults: state.SearchResults,
			MinStatistics: state.Request.MinVerifiedStats,
			MaxStatistics: state.Request.MaxCandidates,
		}

		resp, err := oa.callSynthesisAgent(ctx, synthesisReq)
		if err != nil {
			return nil, fmt.Errorf("synthesis failed: %w", err)
		}

		logger.Info("synthesis completed", "candidates", len(resp.Candidates))

		return &SynthesisState{
			Request:       state.Request,
			SearchResults: state.SearchResults,
			Candidates:    resp.Candidates,
		}, nil
	})
	if err := g.AddLambdaNode(nodeSynthesis, synthesisLambda); err != nil {
		oa.logger.Warn("failed to add synthesis node", "error", err)
	}

	// 4. Verification Node - calls verification agent
	verificationLambda := compose.InvokableLambda(func(ctx context.Context, state *SynthesisState) (*VerificationState, error) {
		logger := logging.FromContext(ctx)
		logger.Info("verifying candidates", "count", len(state.Candidates))

		verifyReq := &models.VerificationRequest{
			Candidates: state.Candidates,
		}

		resp, err := oa.callVerificationAgent(ctx, verifyReq)
		if err != nil {
			return nil, fmt.Errorf("verification failed: %w", err)
		}

		// Extract verified statistics
		var verifiedStats []models.Statistic
		for _, result := range resp.Results {
			if result.Verified {
				verifiedStats = append(verifiedStats, *result.Statistic)
			}
		}

		return &VerificationState{
			Request:       state.Request,
			AllCandidates: state.Candidates,
			Verified:      verifiedStats,
			Failed:        resp.Failed,
		}, nil
	})
	if err := g.AddLambdaNode(nodeVerification, verificationLambda); err != nil {
		oa.logger.Warn("failed to add verification node", "error", err)
	}

	// 5. Quality Check Node - deterministic decision
	qualityCheckLambda := compose.InvokableLambda(func(ctx context.Context, state *VerificationState) (*QualityDecision, error) {
		logger := logging.FromContext(ctx)
		verified := len(state.Verified)
		target := state.Request.MinVerifiedStats

		logger.Info("quality check", "verified", verified, "target", target)

		decision := &QualityDecision{
			State:     state,
			NeedMore:  verified < target,
			Shortfall: target - verified,
		}

		if decision.NeedMore {
			logger.Info("need more verified statistics", "shortfall", decision.Shortfall)
		} else {
			logger.Info("quality target met")
		}

		return decision, nil
	})
	if err := g.AddLambdaNode(nodeCheckQuality, qualityCheckLambda); err != nil {
		oa.logger.Warn("failed to add quality check node", "error", err)
	}

	// 6. Retry Research Node (if needed) - NOT IMPLEMENTED YET in 4-agent architecture
	// TODO: Implement retry logic for 4-agent workflow
	retryResearchLambda := compose.InvokableLambda(func(ctx context.Context, decision *QualityDecision) (*VerificationState, error) {
		if !decision.NeedMore {
			// No retry needed, return existing state
			return decision.State, nil
		}

		logger := logging.FromContext(ctx)
		logger.Warn("retry logic not yet implemented for 4-agent architecture", "shortfall", decision.Shortfall)

		// For now, just return the existing state
		// TODO: Implement: Research → Synthesis → Verification loop
		return decision.State, nil
	})
	if err := g.AddLambdaNode(nodeRetryResearch, retryResearchLambda); err != nil {
		oa.logger.Warn("failed to add retry research node", "error", err)
	}

	// 7. Format Response Node
	formatResponseLambda := compose.InvokableLambda(func(ctx context.Context, state *VerificationState) (*models.OrchestrationResponse, error) {
		logger := logging.FromContext(ctx)
		verifiedCount := len(state.Verified)
		targetCount := state.Request.MinVerifiedStats
		isPartial := verifiedCount < targetCount

		if isPartial {
			logger.Info("formatting partial response", "verified", verifiedCount, "target", targetCount)
		} else {
			logger.Info("formatting complete response", "verified", verifiedCount)
		}

		return &models.OrchestrationResponse{
			Topic:           state.Request.Topic,
			Statistics:      state.Verified,
			TotalCandidates: len(state.AllCandidates),
			VerifiedCount:   verifiedCount,
			FailedCount:     state.Failed,
			Timestamp:       time.Now(),
			Partial:         isPartial,
			TargetCount:     targetCount,
		}, nil
	})
	if err := g.AddLambdaNode(nodeFormatResponse, formatResponseLambda); err != nil {
		oa.logger.Warn("failed to add format response node", "error", err)
	}

	// Add edges to define the workflow
	_ = g.AddEdge(compose.START, nodeValidateInput)
	_ = g.AddEdge(nodeValidateInput, nodeResearch)
	_ = g.AddEdge(nodeResearch, nodeSynthesis)     // NEW: Research → Synthesis
	_ = g.AddEdge(nodeSynthesis, nodeVerification) // NEW: Synthesis → Verification
	_ = g.AddEdge(nodeVerification, nodeCheckQuality)

	// Conditional branching based on quality check
	_ = g.AddEdge(nodeCheckQuality, nodeRetryResearch)
	_ = g.AddEdge(nodeRetryResearch, nodeFormatResponse)
	_ = g.AddEdge(nodeFormatResponse, compose.END)

	oa.logger.Info("workflow graph built", "flow", "ValidateInput → Research → Synthesis → Verification → QualityCheck → Format")

	return g
}

// Orchestrate executes the deterministic Eino workflow
func (oa *EinoOrchestrationAgent) Orchestrate(ctx context.Context, req *models.OrchestrationRequest) (*models.OrchestrationResponse, error) {
	// Inject logger into context for lambda nodes
	ctx = logging.WithLogger(ctx, oa.logger)

	oa.logger.Info("starting deterministic workflow", "topic", req.Topic)

	// Compile the graph
	compiledGraph, err := oa.graph.Compile(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to compile graph: %w", err)
	}

	// Execute the graph
	result, err := compiledGraph.Invoke(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("workflow execution failed: %w", err)
	}

	oa.logger.Info("workflow completed successfully")
	return result, nil
}

// Helper methods to call research and verification agents

func (oa *EinoOrchestrationAgent) callResearchAgent(ctx context.Context, req *models.ResearchRequest) (*models.ResearchResponse, error) {
	var resp models.ResearchResponse
	url := fmt.Sprintf("%s/research", oa.cfg.ResearchAgentURL)
	if err := httpclient.PostJSON(ctx, oa.client, url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (oa *EinoOrchestrationAgent) callSynthesisAgent(ctx context.Context, req *models.SynthesisRequest) (*models.SynthesisResponse, error) {
	var resp models.SynthesisResponse
	url := fmt.Sprintf("%s/synthesize", oa.cfg.SynthesisAgentURL)
	if err := httpclient.PostJSON(ctx, oa.client, url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (oa *EinoOrchestrationAgent) callVerificationAgent(ctx context.Context, req *models.VerificationRequest) (*models.VerificationResponse, error) {
	var resp models.VerificationResponse
	url := fmt.Sprintf("%s/verify", oa.cfg.VerificationAgentURL)
	if err := httpclient.PostJSON(ctx, oa.client, url, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// HTTP Handler
func (oa *EinoOrchestrationAgent) HandleOrchestrationRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.OrchestrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
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

// State types for the workflow
type ResearchState struct {
	Request       *models.OrchestrationRequest
	SearchResults []models.SearchResult
}

type SynthesisState struct {
	Request       *models.OrchestrationRequest
	SearchResults []models.SearchResult
	Candidates    []models.CandidateStatistic
}

type VerificationState struct {
	Request       *models.OrchestrationRequest
	AllCandidates []models.CandidateStatistic
	Verified      []models.Statistic
	Failed        int
}

type QualityDecision struct {
	State     *VerificationState
	NeedMore  bool
	Shortfall int
}
