package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	agentbase "github.com/plexusone/agent-team-stats/pkg/agent"
	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/models"
)

// SynthesisAgent extracts statistics from webpage content using LLM
type SynthesisAgent struct {
	*agentbase.BaseAgent
	adkAgent agent.Agent
}

// SynthesisInput defines input for synthesis tool
type SynthesisInput struct {
	Topic         string                `json:"topic"`
	SearchResults []models.SearchResult `json:"search_results"`
	MinStatistics int                   `json:"min_statistics"`
	MaxStatistics int                   `json:"max_statistics"`
}

// SynthesisToolOutput defines output from synthesis tool
type SynthesisToolOutput struct {
	Candidates []models.CandidateStatistic `json:"candidates"`
}

// NewSynthesisAgent creates a new ADK-based synthesis agent
func NewSynthesisAgent(cfg *config.Config, logger *slog.Logger) (*SynthesisAgent, error) {
	ctx := logging.WithLogger(context.Background(), logger)

	// Create base agent with LLM
	base, err := agentbase.NewBaseAgent(ctx, cfg, 45)
	if err != nil {
		return nil, fmt.Errorf("failed to create base agent: %w", err)
	}

	logger.Info("agent initialized", "provider", base.GetProviderInfo())

	sa := &SynthesisAgent{
		BaseAgent: base,
	}

	// Create synthesis tool
	synthesisTool, err := functiontool.New(functiontool.Config{
		Name:        "synthesize_statistics",
		Description: "Extracts numerical statistics from web page content",
	}, sa.synthesisToolHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to create synthesis tool: %w", err)
	}

	// Create ADK agent
	adkAgent, err := llmagent.New(llmagent.Config{
		Name:        "statistics_synthesis_agent",
		Model:       base.Model,
		Description: "Extracts statistics from web content",
		Instruction: `You are a statistics synthesis agent. Your job is to:
1. Fetch content from provided URLs
2. Analyze the content to find numerical statistics
3. Extract exact values, units, and context
4. Create verbatim excerpts containing the statistics
5. Identify the source credibility

When extracting statistics:
- Look for numerical values with context (percentages, measurements, counts)
- Extract the exact excerpt containing the statistic (word-for-word)
- Identify the unit of measurement
- Verify the source is reputable (academic, government, research)
- Only extract statistics that are clearly stated with numbers

Reputable sources include:
- Government agencies (.gov domains)
- Academic institutions (.edu domains)
- Research organizations (Pew, Gallup, etc.)
- International organizations (WHO, UN, World Bank, etc.)
- Peer-reviewed journals`,
		Tools: []tool.Tool{synthesisTool},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK agent: %w", err)
	}

	sa.adkAgent = adkAgent

	return sa, nil
}

// synthesisToolHandler implements the synthesis logic
func (sa *SynthesisAgent) synthesisToolHandler(ctx tool.Context, input SynthesisInput) (SynthesisToolOutput, error) {
	sa.Logger.Info("analyzing URLs", "count", len(input.SearchResults), "topic", input.Topic)

	candidates := make([]models.CandidateStatistic, 0)

	// Analyze each search result
	for i, result := range input.SearchResults {
		if len(candidates) >= input.MaxStatistics && input.MaxStatistics > 0 {
			break
		}

		sa.Logger.Debug("fetching content", "url", result.URL)

		// Fetch webpage content using base agent method
		content, err := sa.FetchURL(context.Background(), result.URL, 1)
		if err != nil {
			sa.Logger.Warn("failed to fetch URL", "url", result.URL, "error", err)
			continue
		}

		// Extract statistics from content using LLM
		stats, err := sa.extractStatisticsWithLLM(context.Background(), input.Topic, result, content)
		if err != nil {
			sa.Logger.Warn("failed to extract statistics", "url", result.URL, "error", err)
			continue
		}
		candidates = append(candidates, stats...)

		sa.Logger.Info("extracted statistics",
			"extracted", len(stats),
			"domain", result.Domain,
			"total", len(candidates),
			"target", input.MinStatistics)

		// Stop if we have enough
		if len(candidates) >= input.MinStatistics && i > 2 {
			break
		}
	}

	sa.Logger.Info("synthesis completed", "candidates", len(candidates))

	return SynthesisToolOutput{
		Candidates: candidates,
	}, nil
}

// extractStatisticsWithLLM uses LLM to intelligently extract statistics from content
func (sa *SynthesisAgent) extractStatisticsWithLLM(ctx context.Context, topic string, result models.SearchResult, content string) ([]models.CandidateStatistic, error) {
	// Truncate content if too long (LLMs have token limits)
	maxContentLen := 30000 // ~8000 tokens - increased from 15000 to capture more statistics
	if len(content) > maxContentLen {
		content = content[:maxContentLen]
	}

	// Create prompt for LLM to extract statistics
	prompt := fmt.Sprintf(`Analyze the following webpage content and extract ALL numerical statistics related to "%s".

IMPORTANT RULES:
1. Extract EVERY statistic you find, not just one or two. Be thorough and comprehensive.
2. The "value" field MUST be the exact number that appears in the excerpt - do not approximate or round
3. The "excerpt" MUST be a verbatim quote containing the exact number you put in "value"
4. If the excerpt says "1.5°C", the value must be 1.5, not 1
5. If you cannot find an exact number in the text, skip that statistic

For each statistic found, provide:
1. name: A brief descriptive name
2. value: The EXACT numerical value from the text (as a number, not string)
3. unit: The unit of measurement (percent, million, billion, degrees Celsius, people, countries, etc.)
4. excerpt: The verbatim excerpt from the text containing this EXACT statistic (50-200 characters)

Return valid JSON array with this structure:
[
  {
    "name": "Global temperature rise",
    "value": 1.5,
    "unit": "degrees Celsius",
    "excerpt": "limiting global warming to 1.5°C above pre-industrial levels"
  },
  {
    "name": "Survey respondents",
    "value": 75000,
    "unit": "people",
    "excerpt": "Over 75,000 people across 77 countries participated"
  }
]

CRITICAL: The value field must match the number in the excerpt exactly. Do not invent numbers.

Extract ALL statistics with clear numerical values. If the page contains 10 statistics, return 10 items in the array.
Return empty array [] ONLY if absolutely no statistics are found.

Webpage URL: %s
Domain: %s

Content:
%s

JSON output with ALL statistics:`, topic, result.URL, result.Domain, content)

	// Call LLM to extract statistics using ADK
	llmReq := &model.LLMRequest{
		Contents: genai.Text(prompt),
	}

	var response string
	for llmResp, err := range sa.Model.GenerateContent(ctx, llmReq, false) {
		if err != nil {
			return nil, fmt.Errorf("LLM generation failed: %w", err)
		}
		// Extract text from response
		if llmResp.Content != nil && llmResp.Content.Parts != nil {
			for _, part := range llmResp.Content.Parts {
				if part.Text != "" {
					response += part.Text
				}
			}
		}
	}

	// Parse JSON response
	type StatExtraction struct {
		Name    string  `json:"name"`
		Value   float32 `json:"value"`
		Unit    string  `json:"unit"`
		Excerpt string  `json:"excerpt"`
	}

	var extractions []StatExtraction
	if err := json.Unmarshal([]byte(response), &extractions); err != nil {
		// LLM might wrap JSON in markdown code blocks
		response = extractJSONFromMarkdown(response)
		if err := json.Unmarshal([]byte(response), &extractions); err != nil {
			return nil, fmt.Errorf("failed to parse LLM response as JSON: %w (response: %s)", err, response)
		}
	}

	// Convert to CandidateStatistic
	candidates := make([]models.CandidateStatistic, 0, len(extractions))
	for _, ext := range extractions {
		if ext.Value == 0 || ext.Excerpt == "" {
			continue // Skip invalid entries
		}

		candidates = append(candidates, models.CandidateStatistic{
			Name:      ext.Name,
			Value:     ext.Value,
			Unit:      ext.Unit,
			Source:    result.Domain,
			SourceURL: result.URL,
			Excerpt:   ext.Excerpt,
		})
	}

	return candidates, nil
}

// extractJSONFromMarkdown removes markdown code fences and extra text from LLM response
func extractJSONFromMarkdown(response string) string {
	response = strings.TrimSpace(response)

	// Try to find JSON array in the response
	startIdx := strings.Index(response, "[")
	if startIdx == -1 {
		return response // No array found, return as-is
	}

	// Find the matching closing bracket
	endIdx := strings.LastIndex(response, "]")
	if endIdx == -1 || endIdx < startIdx {
		return response // No valid closing bracket
	}

	// Extract just the JSON array
	jsonStr := response[startIdx : endIdx+1]
	return strings.TrimSpace(jsonStr)
}

// Synthesize processes a synthesis request directly
func (sa *SynthesisAgent) Synthesize(ctx context.Context, req *models.SynthesisRequest) (*models.SynthesisResponse, error) { // nolint:unparam // error return kept for future usage
	sa.Logger.Info("processing search results", "count", len(req.SearchResults), "topic", req.Topic)

	var candidates []models.CandidateStatistic
	pagesProcessed := 0
	minPagesToProcess := 15 // Process at least 15 pages for comprehensive coverage (increased from 5)

	// Analyze each search result
	for _, result := range req.SearchResults {
		// Stop only if we have enough candidates AND processed minimum pages
		if len(candidates) >= req.MaxStatistics && req.MaxStatistics > 0 && pagesProcessed >= minPagesToProcess {
			sa.Logger.Info("reached max statistics", "max", req.MaxStatistics, "pages", pagesProcessed)
			break
		}

		// Fetch webpage content using base agent
		content, err := sa.FetchURL(ctx, result.URL, 1)
		if err != nil {
			sa.Logger.Warn("failed to fetch URL", "url", result.URL, "error", err)
			continue
		}

		// Extract statistics using LLM
		stats, err := sa.extractStatisticsWithLLM(ctx, req.Topic, result, content)
		if err != nil {
			sa.Logger.Warn("failed to extract statistics", "url", result.URL, "error", err)
			continue
		}

		pagesProcessed++

		if len(stats) > 0 {
			candidates = append(candidates, stats...)
			sa.Logger.Info("extracted statistics",
				"extracted", len(stats),
				"domain", result.Domain,
				"total", len(candidates),
				"pages", pagesProcessed)
		} else {
			sa.Logger.Debug("no statistics found",
				"domain", result.Domain,
				"total", len(candidates),
				"pages", pagesProcessed)
		}

		// Only stop early if we have well exceeded the minimum requirement
		// Use 5x multiplier to account for verification failures (increased from 2x)
		if len(candidates) >= req.MinStatistics*5 && pagesProcessed >= minPagesToProcess {
			sa.Logger.Info("exceeded minimum threshold",
				"candidates", len(candidates),
				"pages", pagesProcessed)
			break
		}
	}

	response := &models.SynthesisResponse{
		Topic:           req.Topic,
		Candidates:      candidates,
		SourcesAnalyzed: min(len(req.SearchResults), len(candidates)/2+1),
		Timestamp:       time.Now(),
	}

	sa.Logger.Info("synthesis completed",
		"candidates", len(candidates),
		"sources", response.SourcesAnalyzed)

	return response, nil
}

// HandleSynthesisRequest is the HTTP handler
func (sa *SynthesisAgent) HandleSynthesisRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req models.SynthesisRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// Set defaults
	if req.MinStatistics == 0 {
		req.MinStatistics = 5
	}
	if req.MaxStatistics == 0 {
		req.MaxStatistics = 20
	}

	resp, err := sa.Synthesize(r.Context(), &req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Synthesis failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		sa.Logger.Error("failed to encode response", "error", err)
	}
}

func main() {
	logger := logging.NewAgentLogger("synthesis")
	cfg := config.LoadConfig()

	synthesisAgent, err := NewSynthesisAgent(cfg, logger)
	if err != nil {
		logger.Error("failed to create synthesis agent", "error", err)
		os.Exit(1)
	}

	// Start A2A server if enabled
	if cfg.A2AEnabled {
		a2aServer, err := NewA2AServer(synthesisAgent, "9004", logger)
		if err != nil {
			logger.Error("failed to create A2A server", "error", err)
		} else {
			go func() {
				if err := a2aServer.Start(context.Background()); err != nil {
					logger.Error("A2A server error", "error", err)
				}
			}()
			logger.Info("A2A server started", "port", 9004)
		}
	}

	// Start HTTP server with timeout (backward compatible)
	timeout := time.Duration(cfg.HTTPTimeoutSeconds) * time.Second
	server := &http.Server{
		Addr:         ":8004",
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		IdleTimeout:  timeout * 2,
	}

	http.HandleFunc("/synthesize", synthesisAgent.HandleSynthesisRequest)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			logger.Error("failed to write health response", "error", err)
		}
	})

	// Setup graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("HTTP server starting",
			"port", 8004,
			"mode", "ADK-based LLM extraction")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	<-stop
	logger.Info("shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("server shutdown error", "error", err)
	}

	// Close agent to flush observability data
	if err := synthesisAgent.Close(); err != nil {
		logger.Error("failed to close agent", "error", err)
	}
	logger.Info("shutdown complete")
}

// Helper functions
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

/*
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
*/
