package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelrelay/modelrelay/platform/snowflakesource"
	"github.com/modelrelay/modelrelay/platform/sqlprofiles"
	"github.com/modelrelay/modelrelay/platform/sqlvalidate"
	"github.com/spf13/cobra"
)

type snowflakeEdgeConfig struct {
	Snowflake snowflakesource.Config `json:"snowflake"`
	Policy    sqlprofiles.Policy     `json:"policy"`
	TenantID  string                 `json:"tenant_id"`
	SourceID  string                 `json:"source_id,omitempty"`
}

func (c snowflakeEdgeConfig) validate() error {
	if err := c.Snowflake.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.TenantID) == "" {
		return errors.New("tenant_id is required")
	}
	if len(c.TenantID) > 128 || len(c.SourceID) > 128 {
		return errors.New("tenant_id and source_id must be at most 128 bytes")
	}
	if c.Policy.Dialect != sqlprofiles.Dialect("snowflake") {
		return errors.New("policy.dialect must be snowflake")
	}
	if err := c.Policy.Validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if c.Policy.ReadOnly == nil || !*c.Policy.ReadOnly {
		return errors.New("policy.read_only must be true")
	}
	if c.Policy.Limits.DefaultLimit <= 0 || c.Policy.Limits.MaxLimit <= 0 || c.Policy.Limits.TimeoutMS <= 0 {
		return errors.New("policy.limits.default_limit, max_limit, and timeout_ms must be positive")
	}
	return nil
}

func loadSnowflakeEdgeConfig(path string) (snowflakeEdgeConfig, error) {
	file, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return snowflakeEdgeConfig{}, fmt.Errorf("open Snowflake Edge config: %w", err)
	}
	defer file.Close()
	var config snowflakeEdgeConfig
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return snowflakeEdgeConfig{}, fmt.Errorf("decode Snowflake Edge config: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return snowflakeEdgeConfig{}, errors.New("Snowflake Edge config must contain one JSON object")
	}
	if err := config.validate(); err != nil {
		return snowflakeEdgeConfig{}, err
	}
	return config, nil
}

func newSnowflakeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "snowflake", Short: "Operate the trusted ModelRelay Edge broker for Snowflake"}
	cmd.AddCommand(newSnowflakeServeCmd(), newSnowflakeTestConnectionCmd(), newSnowflakeSchemaCmd())
	return cmd
}

func newSnowflakeServeCmd() *cobra.Command {
	var configPath, listenAddress, tokenEnv string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the credential-isolated Snowflake query broker",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadSnowflakeEdgeConfig(configPath)
			if err != nil {
				return err
			}
			token, err := requiredSecretEnvironment(tokenEnv)
			if err != nil {
				return err
			}
			connector, err := snowflakesource.Open(cmd.Context(), config.Snowflake)
			if err != nil {
				return err
			}
			defer connector.Close()
			listener, err := net.Listen("tcp", listenAddress)
			if err != nil {
				return fmt.Errorf("listen for Snowflake Edge broker: %w", err)
			}
			defer listener.Close()
			server := newSnowflakeHTTPServer(listener.Addr().String(), token, config, connector)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			log.Printf("Snowflake Edge broker listening on %s", listener.Addr())
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return server.Shutdown(shutdownCtx)
			}
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to the non-secret Snowflake Edge JSON config")
	cmd.Flags().StringVar(&listenAddress, "listen", "127.0.0.1:8787", "Broker listen address")
	cmd.Flags().StringVar(&tokenEnv, "broker-token-env", "", "Environment variable containing the broker bearer token")
	_ = cmd.MarkFlagRequired("config")
	_ = cmd.MarkFlagRequired("broker-token-env")
	return cmd
}

func newSnowflakeTestConnectionCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "test-connection",
		Short: "Verify Snowflake identity and configured session defaults",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadSnowflakeEdgeConfig(configPath)
			if err != nil {
				return err
			}
			connector, err := snowflakesource.Open(cmd.Context(), config.Snowflake)
			if err != nil {
				return err
			}
			defer connector.Close()
			state, err := connector.TestConnection(cmd.Context())
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to the non-secret Snowflake Edge JSON config")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func newSnowflakeSchemaCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print role-visible Snowflake tables, views, secure views, and columns",
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadSnowflakeEdgeConfig(configPath)
			if err != nil {
				return err
			}
			connector, err := snowflakesource.Open(cmd.Context(), config.Snowflake)
			if err != nil {
				return err
			}
			defer connector.Close()
			schema, err := connector.Schema(cmd.Context(), snowflakesource.QueryMetadata{TenantID: config.TenantID, Source: config.SourceID})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), schema)
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to the non-secret Snowflake Edge JSON config")
	_ = cmd.MarkFlagRequired("config")
	return cmd
}

func requiredSecretEnvironment(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("secret environment variable name is required")
	}
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("required secret environment variable %q is unset or empty", name)
	}
	if len(value) < 32 {
		return "", fmt.Errorf("secret environment variable %q must contain at least 32 bytes", name)
	}
	return value, nil
}

type snowflakeBrokerSource interface {
	Ping(context.Context) error
	TestConnection(context.Context) (snowflakesource.ConnectionState, error)
	Schema(context.Context, snowflakesource.QueryMetadata) (string, error)
	Query(context.Context, string, int, snowflakesource.QueryMetadata) ([]map[string]any, error)
}

func newSnowflakeHTTPServer(address, token string, config snowflakeEdgeConfig, source snowflakeBrokerSource) *http.Server {
	handler := &snowflakeBrokerHandler{token: token, config: config, source: source}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"live"}`)
	})
	mux.Handle("/readyz", handler.readyHandler())
	mux.Handle("/v1/sql", handler)
	writeTimeout := config.Snowflake.Limits.RequestBudget() + 5*time.Second
	return &http.Server{
		Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: writeTimeout, IdleTimeout: 60 * time.Second,
	}
}

type snowflakeBrokerHandler struct {
	token  string
	config snowflakeEdgeConfig
	source snowflakeBrokerSource
}

func (h *snowflakeBrokerHandler) authorized(request *http.Request) bool {
	return validBearerToken(request, h.token)
}

func (h *snowflakeBrokerHandler) readyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !h.authorized(r) {
			writeSQLBrokerError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if _, err := h.source.TestConnection(r.Context()); err != nil {
			writeSQLBrokerError(w, http.StatusServiceUnavailable, "Snowflake is not ready")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})
}

func (h *snowflakeBrokerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(r) {
		writeSQLBrokerError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var request struct {
		Method string `json:"method"`
		SQL    string `json:"sql,omitempty"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeSQLBrokerError(w, http.StatusBadRequest, "invalid broker request")
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		writeSQLBrokerError(w, http.StatusBadRequest, "invalid broker request")
		return
	}
	runID := r.URL.Query().Get("run_id")
	if len(runID) > 128 {
		writeSQLBrokerError(w, http.StatusBadRequest, "run_id must be at most 128 bytes")
		return
	}
	var result any
	switch request.Method {
	case "schema":
		var err error
		result, err = h.source.Schema(r.Context(), snowflakesource.QueryMetadata{
			RunID: runID, TenantID: h.config.TenantID, Source: h.config.SourceID,
		})
		if err != nil {
			writeSQLBrokerError(w, http.StatusBadGateway, err.Error())
			return
		}
	case "query":
		validated, err := sqlvalidate.Validate(request.SQL, h.config.Policy, nil)
		if err != nil {
			writeSQLBrokerError(w, http.StatusBadRequest, err.Error())
			return
		}
		metadata := snowflakesource.QueryMetadata{
			RunID: runID, TenantID: h.config.TenantID, Source: h.config.SourceID,
		}
		result, err = h.source.Query(r.Context(), validated.NormalizedSQL, validated.TimeoutMS, metadata)
		if err != nil {
			writeSQLBrokerError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	default:
		writeSQLBrokerError(w, http.StatusBadRequest, "method must be query or schema")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"result": result}); err != nil {
		log.Printf("warning: encode Snowflake broker response: %v", err)
	}
}
