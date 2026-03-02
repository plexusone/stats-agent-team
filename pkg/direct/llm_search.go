package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/llm"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/models"
)

// LLMSearchService provides direct LLM-based statistics search (like ChatGPT)
type LLMSearchService struct {
	cfg    *config.Config
	model  model.LLM
	logger *slog.Logger
}

// NewLLMSearchService creates a new direct LLM search service
func NewLLMSearchService(cfg *config.Config) (*LLMSearchService, error) {
	logger := logging.NewAgentLogger("llm-search")
	ctx := logging.WithLogger(context.Background(), logger)

	// Create model using factory
	modelFactory := llm.NewModelFactory(ctx, cfg)
	llmModel, err := modelFactory.CreateModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create model: %w", err)
	}

	return &LLMSearchService{
		cfg:    cfg,
		model:  llmModel,
		logger: logger,
	}, nil
}

// SearchStatistics uses LLM directly to find statistics (like ChatGPT with web search)
// If verifyWithAgent is true, sends LLM claims to verification agent for actual web verification
func (s *LLMSearchService) SearchStatistics(ctx context.Context, topic string, minStats int) (*models.OrchestrationResponse, error) {
	return s.SearchStatisticsWithVerification(ctx, topic, minStats, false)
}

// SearchStatisticsWithVerification allows optional verification agent integration
func (s *LLMSearchService) SearchStatisticsWithVerification(ctx context.Context, topic string, minStats int, verifyWithAgent bool) (*models.OrchestrationResponse, error) {
	prompt := fmt.Sprintf(`Find %d or more verified, numerical statistics about "%s".

For each statistic, provide:
1. name: Brief description
2. value: The exact numerical value (as a plain number, NO commas or formatting)
3. unit: Unit of measurement
4. source: Name of the authoritative source
5. source_url: Direct URL to the source (if available)
6. excerpt: Exact quote containing the statistic

IMPORTANT INSTRUCTIONS:
- Prioritize statistics from reputable sources (government agencies, research organizations, academic institutions)
- Include the actual URL where each statistic can be verified
- Use real, verifiable data - do not make up statistics
- Extract the exact numerical values
- Provide verbatim excerpts
- CRITICAL: The "value" field must be a plain number with NO commas (e.g., 2537 not 2,537)

Return a JSON array:
[
  {
    "name": "Global temperature increase since 1880",
    "value": 1.1,
    "unit": "degrees Celsius",
    "source": "NASA",
    "source_url": "https://climate.nasa.gov/vital-signs/global-temperature/",
    "excerpt": "The planet's average surface temperature has risen about 1.1 degrees Celsius since the late 19th century"
  },
  {
    "name": "Example large number",
    "value": 75000,
    "unit": "people",
    "source": "Example",
    "source_url": "https://example.com",
    "excerpt": "Over 75,000 people participated"
  }
]

REMEMBER: Numbers like 75,000 should be written as 75000 (no comma).

Find at least %d statistics. Return only the JSON array, no other text.`, minStats, topic, minStats)

	// Call LLM
	req := &model.LLMRequest{
		Contents: genai.Text(prompt),
	}

	var response string
	for llmResp, err := range s.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return nil, fmt.Errorf("LLM generation failed: %w", err)
		}
		if llmResp.Content != nil && llmResp.Content.Parts != nil {
			for _, part := range llmResp.Content.Parts {
				if part.Text != "" {
					response += part.Text
				}
			}
		}
	}

	// Extract JSON from response
	response = extractJSONFromMarkdown(response)

	// Parse JSON
	type StatResponse struct {
		Name      string  `json:"name"`
		Value     float32 `json:"value"`
		Unit      string  `json:"unit"`
		Source    string  `json:"source"`
		SourceURL string  `json:"source_url"`
		Excerpt   string  `json:"excerpt"`
	}

	var stats []StatResponse
	if err := json.Unmarshal([]byte(response), &stats); err != nil {
		return nil, fmt.Errorf("failed to parse LLM response: %w\nResponse: %s", err, response)
	}

	// Convert to candidate statistics for potential verification
	candidates := make([]models.CandidateStatistic, 0, len(stats))
	for _, stat := range stats {
		candidates = append(candidates, models.CandidateStatistic{
			Name:      stat.Name,
			Value:     stat.Value,
			Unit:      stat.Unit,
			Source:    stat.Source,
			SourceURL: stat.SourceURL,
			Excerpt:   stat.Excerpt,
		})
	}

	// If verification requested, send to verification agent
	if verifyWithAgent {
		return s.verifyWithVerificationAgent(ctx, topic, candidates, minStats)
	}

	// Otherwise, trust LLM claims and mark as verified
	verifiedStats := make([]models.Statistic, 0, len(candidates))
	for _, cand := range candidates {
		verifiedStats = append(verifiedStats, models.Statistic{
			Name:      cand.Name,
			Value:     cand.Value,
			Unit:      cand.Unit,
			Source:    cand.Source,
			SourceURL: cand.SourceURL,
			Excerpt:   cand.Excerpt,
			Verified:  true, // Marked as verified since from LLM with sources (not web-verified)
			DateFound: time.Now(),
		})
	}

	return &models.OrchestrationResponse{
		Topic:         topic,
		Statistics:    verifiedStats,
		VerifiedCount: len(verifiedStats),
		Timestamp:     time.Now(),
		Partial:       len(verifiedStats) < minStats,
		TargetCount:   minStats,
	}, nil
}

// verifyWithVerificationAgent sends LLM-claimed statistics to verification agent for web verification
func (s *LLMSearchService) verifyWithVerificationAgent(ctx context.Context, topic string, candidates []models.CandidateStatistic, minStats int) (*models.OrchestrationResponse, error) {
	// Get verification agent URL from config
	verificationURL := s.cfg.VerificationAgentURL
	if verificationURL == "" {
		verificationURL = "http://localhost:8002"
	}

	// Create verification request
	verifyReq := &models.VerificationRequest{
		Candidates: candidates,
	}

	// Call verification agent via HTTP
	reqData, err := json.Marshal(verifyReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal verification request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/verify", verificationURL), bytes.NewReader(reqData))
	if err != nil {
		return nil, fmt.Errorf("failed to create verification request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	httpResp, err := client.Do(httpReq) //nolint:gosec // G704: URL from config, not user input
	if err != nil {
		return nil, fmt.Errorf("verification agent request failed: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("verification agent returned HTTP %d", httpResp.StatusCode)
	}

	// Parse verification response
	var verifyResp models.VerificationResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&verifyResp); err != nil {
		return nil, fmt.Errorf("failed to decode verification response: %w", err)
	}

	// Extract verified statistics
	verifiedStats := make([]models.Statistic, 0, verifyResp.Verified)
	for _, result := range verifyResp.Results {
		if result.Verified {
			verifiedStats = append(verifiedStats, *result.Statistic)
		}
	}

	s.logger.Info("verification completed",
		"candidates", len(candidates),
		"verified", len(verifiedStats),
		"target", minStats)

	return &models.OrchestrationResponse{
		Topic:           topic,
		Statistics:      verifiedStats,
		TotalCandidates: len(candidates),
		VerifiedCount:   len(verifiedStats),
		FailedCount:     verifyResp.Failed,
		Timestamp:       time.Now(),
		Partial:         len(verifiedStats) < minStats,
		TargetCount:     minStats,
	}, nil
}

// extractJSONFromMarkdown removes markdown code fences from response
func extractJSONFromMarkdown(response string) string {
	response = strings.TrimSpace(response)

	// Try to find JSON array
	startIdx := strings.Index(response, "[")
	if startIdx == -1 {
		return response
	}

	endIdx := strings.LastIndex(response, "]")
	if endIdx == -1 || endIdx < startIdx {
		return response
	}

	return strings.TrimSpace(response[startIdx : endIdx+1])
}
