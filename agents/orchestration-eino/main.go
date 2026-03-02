package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/plexusone/agent-team-stats/pkg/config"
	"github.com/plexusone/agent-team-stats/pkg/logging"
	"github.com/plexusone/agent-team-stats/pkg/orchestration"
)

func main() {
	cfg := config.LoadConfig()
	logger := logging.NewAgentLogger("eino-orchestrator")
	einoAgent := orchestration.NewEinoOrchestrationAgent(cfg, logger)

	// Start A2A server if enabled (standard protocol for agent interoperability)
	// Note: Eino uses graph-based orchestration, wrapped in ADK for A2A compatibility
	if cfg.A2AEnabled {
		a2aServer, err := NewA2AServer(einoAgent, "9000", logger)
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
	timeout := time.Duration(cfg.HTTPTimeoutSeconds) * time.Second
	server := &http.Server{
		Addr:         ":8000",
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		IdleTimeout:  timeout * 2,
	}

	http.HandleFunc("/orchestrate", einoAgent.HandleOrchestrationRequest)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("OK")); err != nil {
			logger.Error("failed to write health response", "error", err)
		}
	})

	logger.Info("HTTP server starting",
		"port", 8000,
		"mode", "Eino graph-based deterministic")
	if err := server.ListenAndServe(); err != nil {
		logger.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
}
