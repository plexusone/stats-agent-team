package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/models"
	"github.com/plexusone/agent-team-stats/pkg/orchestration"
)

const (
	serverName    = "stats-agent-team"
	serverVersion = "1.0.0"
)

type SearchStatisticsParams struct {
	Topic            string `json:"topic"`
	MinVerifiedStats int    `json:"min_verified_stats,omitempty"`
	MaxCandidates    int    `json:"max_candidates,omitempty"`
	ReputableOnly    bool   `json:"reputable_only,omitempty"`
}

var (
	einoAgent *orchestration.EinoOrchestrationAgent
	logger    *slog.Logger
)

func SearchStatistics(ctx context.Context, req *mcp.CallToolRequest, args SearchStatisticsParams) (*mcp.CallToolResult, any, error) {
	// Validate input
	if args.Topic == "" {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: "Error: topic is required"},
			},
		}, nil, nil
	}

	// Set defaults
	if args.MinVerifiedStats == 0 {
		args.MinVerifiedStats = 10
	}
	if args.MaxCandidates == 0 {
		args.MaxCandidates = 30
	}
	if !args.ReputableOnly {
		args.ReputableOnly = true // default to true
	}

	// Create orchestration request
	orchReq := &models.OrchestrationRequest{
		Topic:            args.Topic,
		MinVerifiedStats: args.MinVerifiedStats,
		MaxCandidates:    args.MaxCandidates,
		ReputableOnly:    args.ReputableOnly,
	}

	logger.Info("searching for statistics", "topic", args.Topic)

	// Execute orchestration
	result, err := einoAgent.Orchestrate(ctx, orchReq)
	if err != nil {
		errMsg := fmt.Sprintf("Error searching for statistics: %v", err)
		logger.Error("search failed", "error", err)
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{Text: errMsg},
			},
		}, nil, nil
	}

	// Format response
	response := formatResponse(result)
	logger.Info("search completed",
		"verified", result.VerifiedCount,
		"candidates", result.TotalCandidates)

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: response},
		},
	}, nil, nil
}

// IOTransport implements a stdio transport for MCP
type IOTransport struct {
	r *bufio.Reader
	w io.Writer
}

// NewIOTransport creates a new IOTransport with the given io.Reader and io.Writer
func NewIOTransport(r io.Reader, w io.Writer) *IOTransport {
	return &IOTransport{
		r: bufio.NewReader(r),
		w: w,
	}
}

// ioConn implements mcp.Connection
type ioConn struct {
	r *bufio.Reader
	w io.Writer
}

// Connect implements mcp.Transport.Connect
func (t *IOTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	return &ioConn{
		r: t.r,
		w: t.w,
	}, nil
}

// Read implements mcp.Connection.Read, assuming messages are newline-delimited JSON
func (c *ioConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	data, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return jsonrpc.DecodeMessage(data[:len(data)-1])
}

// Write implements mcp.Connection.Write, appending a newline delimiter after the message
func (c *ioConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	_, err1 := c.w.Write(data)
	_, err2 := c.w.Write([]byte{'\n'})
	if err1 != nil {
		return err1
	}
	return err2
}

// Close implements mcp.Connection.Close
func (c *ioConn) Close() error {
	return nil
}

// SessionID implements mcp.Connection.SessionID
func (c *ioConn) SessionID() string {
	return ""
}

func main() {
	// Initialize logger
	logger = logging.NewAgentLogger("mcp-server")

	// Load configuration
	cfg := config.LoadConfig()

	// Create Eino orchestration agent
	einoAgent = orchestration.NewEinoOrchestrationAgent(cfg, logger)

	logger.Info("starting MCP server",
		"name", serverName,
		"version", serverVersion,
		"llm_provider", cfg.LLMProvider,
		"llm_model", cfg.LLMModel,
		"research_agent", cfg.ResearchAgentURL,
		"verification_agent", cfg.VerificationAgentURL)

	// Create MCP server
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    serverName,
			Version: serverVersion,
		},
		nil,
	)

	// Add the search_statistics tool
	mcp.AddTool(
		server,
		&mcp.Tool{
			Name: "search_statistics",
			Description: "Search for verified statistics on a given topic using a multi-agent system. " +
				"The system uses research and verification agents to find and validate statistics from " +
				"reputable sources (government agencies, academic institutions, research organizations). " +
				"Returns verified statistics with their sources, URLs, and verbatim excerpts.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"topic": map[string]interface{}{
						"type":        "string",
						"description": "The topic to search statistics for (e.g., 'climate change', 'AI adoption rates', 'cybersecurity threats')",
					},
					"min_verified_stats": map[string]interface{}{
						"type":        "number",
						"description": "Minimum number of verified statistics to return (default: 10)",
					},
					"max_candidates": map[string]interface{}{
						"type":        "number",
						"description": "Maximum number of candidate statistics to gather (default: 30)",
					},
					"reputable_only": map[string]interface{}{
						"type":        "boolean",
						"description": "Only use reputable sources like government, academic, and research organizations (default: true)",
					},
				},
				"required": []string{"topic"},
			},
		},
		SearchStatistics,
	)

	// Create stdio transport
	transport := NewIOTransport(os.Stdin, os.Stdout)

	logger.Info("server running on stdio transport")

	// Run server
	if err := server.Run(context.Background(), transport); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

// formatResponse formats the orchestration response for display
func formatResponse(result *models.OrchestrationResponse) string {
	if result == nil {
		return "No results found."
	}

	output := fmt.Sprintf("# Statistics Search Results\n\n")
	output += fmt.Sprintf("**Topic:** %s\n", result.Topic)
	output += fmt.Sprintf("**Verified:** %d statistics\n", result.VerifiedCount)
	output += fmt.Sprintf("**Failed:** %d statistics\n", result.FailedCount)
	output += fmt.Sprintf("**Total Candidates:** %d\n", result.TotalCandidates)
	output += fmt.Sprintf("**Timestamp:** %s\n\n", result.Timestamp.Format("2006-01-02 15:04:05"))

	if len(result.Statistics) == 0 {
		output += "No verified statistics found.\n"
		return output
	}

	// Add JSON representation
	output += "## JSON Output\n\n```json\n"
	jsonData, err := json.MarshalIndent(result.Statistics, "", "  ")
	if err == nil {
		output += string(jsonData)
	} else {
		output += fmt.Sprintf("Error formatting JSON: %v", err)
	}
	output += "\n```\n\n"

	// Add human-readable format
	output += "## Verified Statistics\n\n"
	for i, stat := range result.Statistics {
		output += fmt.Sprintf("### %d. %s\n\n", i+1, stat.Name)
		output += fmt.Sprintf("- **Value:** %v %s\n", stat.Value, stat.Unit)
		output += fmt.Sprintf("- **Source:** %s\n", stat.Source)
		output += fmt.Sprintf("- **URL:** %s\n", stat.SourceURL)
		output += fmt.Sprintf("- **Excerpt:** \"%s\"\n", stat.Excerpt)
		output += fmt.Sprintf("- **Verified:** ✓\n")
		output += fmt.Sprintf("- **Date Found:** %s\n\n", stat.DateFound.Format("2006-01-02"))
	}

	return output
}
