package search

import (
	"context"
	"fmt"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/omniserp"
	"github.com/plexusone/omniserp/client"
)

// Service provides web search capabilities using metaserp
type Service struct {
	client *client.Client
}

// SearchResult represents a single search result
type SearchResult struct {
	Title       string
	URL         string
	Snippet     string
	DisplayLink string
}

// SearchResponse contains search results
type SearchResponse struct {
	Results []SearchResult
	Total   int
}

// NewService creates a new search service
func NewService(cfg *config.Config) (*Service, error) {
	var engineName string

	// Determine which search provider to use and validate API key
	switch cfg.SearchProvider {
	case "serper":
		if cfg.SerperAPIKey == "" {
			return nil, fmt.Errorf("SERPER_API_KEY is required when using serper provider")
		}
		engineName = "serper"

	case "serpapi":
		if cfg.SerpAPIKey == "" {
			return nil, fmt.Errorf("SERPAPI_API_KEY is required when using serpapi provider")
		}
		engineName = "serpapi"

	default:
		return nil, fmt.Errorf("unsupported search provider: %s (use 'serper' or 'serpapi')", cfg.SearchProvider)
	}

	// Create metaserp client with specific engine
	c, err := client.NewWithEngine(engineName)
	if err != nil {
		return nil, fmt.Errorf("failed to create search client: %w", err)
	}

	return &Service{
		client: c,
	}, nil
}

// Search performs a web search for the given query
func (s *Service) Search(ctx context.Context, query string, numResults int) (*SearchResponse, error) {
	if numResults <= 0 {
		numResults = 10
	}

	// Perform normalized search using omniserp
	result, err := s.client.SearchNormalized(ctx, omniserp.SearchParams{
		Query:      query,
		NumResults: numResults,
		Language:   "en",
		Country:    "us",
	})
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	// Convert to our response format
	searchResults := make([]SearchResult, 0, len(result.OrganicResults))

	// Extract organic results
	for _, org := range result.OrganicResults {
		searchResults = append(searchResults, SearchResult{
			Title:       org.Title,
			URL:         org.Link,
			Snippet:     org.Snippet,
			DisplayLink: org.Domain,
		})
	}

	return &SearchResponse{
		Results: searchResults,
		Total:   len(searchResults),
	}, nil
}

// SearchForStatistics performs a search optimized for finding statistics
func (s *Service) SearchForStatistics(ctx context.Context, topic string, numResults int) (*SearchResponse, error) {
	// Enhance query to find statistics from reputable sources
	enhancedQuery := fmt.Sprintf("%s statistics data research study", topic)

	return s.Search(ctx, enhancedQuery, numResults)
}
