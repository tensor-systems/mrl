package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelrelay/modelrelay/platform/postgressource"
	"github.com/modelrelay/modelrelay/platform/rlm"
	"github.com/modelrelay/modelrelay/platform/rlmrun"
	"github.com/modelrelay/modelrelay/platform/rlmrunner"
	"github.com/modelrelay/modelrelay/platform/workflow"
	sdk "github.com/modelrelay/modelrelay/sdk/go"
	"github.com/modelrelay/modelrelay/sdk/go/llm"
	"github.com/spf13/cobra"
)

func newRLMCmd() *cobra.Command {
	var flags rlmFlags

	cmd := &cobra.Command{
		Use:   "rlm <query>",
		Short: "Run an RLM session (local Python by default; use --remote for hosted)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRLM(cmd, args, &flags)
		},
	}

	cmd.Flags().StringVar(&flags.model, "model", "", "Model ID (overrides profile default)")
	cmd.Flags().StringArrayVarP(&flags.attachments, "attachment", "a", nil, "Attach a local file (repeatable; use '-' for stdin)")
	cmd.Flags().StringVar(&flags.attachmentType, "attachment-type", "", "Override attachment MIME type (useful for stdin)")
	cmd.Flags().BoolVar(&flags.attachStdin, "attach-stdin", false, "Attach stdin as a file")
	cmd.Flags().IntVar(&flags.maxSubcalls, "max-subcalls", 50, "Max llm_query/llm_batch calls")
	cmd.Flags().IntVar(&flags.maxDepth, "max-depth", 1, "Max recursion depth")
	cmd.Flags().IntVar(&flags.execTimeoutMS, "exec-timeout-ms", 0, "Local Python execution timeout in ms (0 uses interpreter default)")
	cmd.Flags().Int64Var(&flags.seedValue, "seed", 0, fmt.Sprintf("Rollout identity/attribution value (0-%d); omit when unavailable", rlmrun.MaxSeed))
	cmd.Flags().StringVar(&flags.pythonPath, "python", "", "Python executable (default: python3)")
	cmd.Flags().Int64Var(&flags.maxInlineBytes, "max-inline-bytes", 0, "Max inline context bytes (0 uses interpreter default)")
	cmd.Flags().Int64Var(&flags.maxTotalBytes, "max-total-bytes", 0, "Max total context bytes (0 uses interpreter default)")
	cmd.Flags().Int64Var(&flags.inlineTextMaxBytes, "inline-text-max-bytes", 0, "Max inline text bytes per file (0 uses default)")
	cmd.Flags().StringVar(&flags.system, "system", "", "Custom instructions prepended to the default RLM system prompt")
	cmd.Flags().BoolVar(&flags.systemOverride, "system-override", false, "Replace the entire system prompt instead of prepending")
	cmd.Flags().StringVar(&flags.toolChoice, "tool-choice", "", "Tool choice mode (unsupported for rlm-core)")
	cmd.Flags().BoolVar(&flags.remote, "remote", false, "Run RLM on ModelRelay (/rlm/execute) instead of local Python")
	cmd.Flags().StringVar(&flags.db, "db", "", "SQLite database file to expose as a read-only SQL data source")
	cmd.Flags().StringVar(&flags.postgresDSNEnv, "postgres-dsn-env", "", "Environment variable containing a PostgreSQL DSN for a trusted read-only edge connector")
	cmd.Flags().StringVar(&flags.snowflakeBrokerURL, "snowflake-broker-url", "", "URL of a trusted ModelRelay Edge for Snowflake broker")
	cmd.Flags().StringVar(&flags.snowflakeBrokerTokenEnv, "snowflake-broker-token-env", "", "Environment variable containing the Snowflake broker capability token")
	cmd.Flags().StringVar(&flags.dbName, "db-name", "db", "Sandbox name for the SQL data source (e.g. db.query(...))")
	cmd.Flags().StringVar(&flags.sqlProfile, "sql-profile", "", "SQL profile ID for the read-only policy (default: permissive read-only policy)")
	cmd.Flags().StringArrayVar(&flags.mcpConfigs, "mcp-config", nil, "Trusted remote MCP source config file (local/VPC mode; repeatable)")
	cmd.Flags().StringVar(&flags.defaultSource, "default-source", "", "Default generated-code data source when more than one is mounted")
	cmd.Flags().Int64Var(&flags.subcallMaxOutputTokens, "subcall-max-output-tokens", 0, "Max output tokens per llm_query/llm_batch subcall (0 = server default, 2048)")
	cmd.Flags().StringVar(&flags.subcallModel, "subcall-model", "", "Model for llm_query/llm_batch subcalls, e.g. a cheaper non-reasoning model (default: the root model)")
	cmd.Flags().StringVar(&flags.subcallReasoningEffort, "subcall-reasoning-effort", "", "Reasoning effort for subcalls: none, minimal, low, medium, high, or xhigh (default: server default, none)")

	return cmd
}

type rlmFlags struct {
	model                   string
	system                  string
	systemOverride          bool
	attachments             []string
	attachmentType          string
	attachStdin             bool
	maxSubcalls             int
	maxDepth                int
	execTimeoutMS           int
	seedValue               int64
	seed                    *int64
	pythonPath              string
	maxInlineBytes          int64
	maxTotalBytes           int64
	inlineTextMaxBytes      int64
	toolChoice              string
	remote                  bool
	db                      string
	postgresDSNEnv          string
	snowflakeBrokerURL      string
	snowflakeBrokerTokenEnv string
	dbName                  string
	sqlProfile              string
	mcpConfigs              []string
	defaultSource           string
	// Subcall cost controls (rlm-core#25); zero values mean server defaults.
	subcallMaxOutputTokens int64
	subcallModel           string
	subcallReasoningEffort string
}

// Subcall cost defaults applied by the local subcall proxy when neither the
// runner payload nor the flags provide a value; mirrors the hosted
// /rlm/subcall defaults (rlm-core#25).
const (
	localDefaultSubcallMaxOutputTokens = int64(2048)
	localDefaultSubcallReasoningEffort = "none"
)

// validSubcallReasoningEffort mirrors the server-side allowed values.
func validSubcallReasoningEffort(effort string) bool {
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

const (
	defaultRLMInlineBytes    = int64(128 * 1024)
	defaultRLMMaxOutputChars = 1_048_576
)

type rlmUsage struct {
	mu    sync.Mutex
	usage workflow.TokenUsage
}

func (u *rlmUsage) add(usage sdk.Usage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.usage.InputTokens += usage.InputTokens
	u.usage.OutputTokens += usage.OutputTokens
	u.usage.TotalTokens += usage.TotalTokens
}

func (u *rlmUsage) snapshot() workflow.TokenUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.usage
}

func runRLM(cmd *cobra.Command, args []string, flags *rlmFlags) error {
	cfg, err := runtimeConfigFrom(cmd)
	if err != nil {
		return err
	}

	model := resolveModel(flags.model, cfg)
	if model == "" {
		return errors.New("model is required (set via --model, MODELRELAY_MODEL, or mrl config set --model)")
	}

	stdinIsTTY, err := isTerminal(os.Stdin)
	if err != nil {
		return err
	}

	autoAttachStdin := !stdinIsTTY &&
		len(flags.attachments) == 0 &&
		strings.TrimSpace(flags.attachmentType) == "" &&
		!flags.attachStdin
	attachStdin := flags.attachStdin || autoAttachStdin
	attachmentPaths, err := resolveAttachmentInputs(flags.attachments, flags.attachmentType, attachStdin, stdinIsTTY)
	if err != nil {
		return err
	}

	textInlineLimit := resolveRLMInlineTextLimit(flags.inlineTextMaxBytes, flags.maxInlineBytes)
	files, cleanup, err := buildRLMFileAttachments(attachmentPaths, flags.attachmentType, os.Stdin, textInlineLimit)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	if flags.remote {
		if err := validateRLMRemoteAttachments(files); err != nil {
			return err
		}
		files = stripRemoteAttachmentPaths(files)
	}

	contextPayload, err := mergeRLMContextFiles([]byte("null"), files)
	if err != nil {
		return err
	}

	var (
		client *sdk.Client
		apiKey sdk.APIKeyAuth
	)
	if flags.remote {
		if strings.TrimSpace(cfg.APIKey) == "" {
			return errors.New("api key required")
		}
		apiKey, err = sdk.ParseAPIKeyAuth(cfg.APIKey)
		if err != nil {
			return err
		}
	} else {
		client, err = newPromptClient(cfg)
		if err != nil {
			return err
		}
	}

	ctx, cancel := contextWithTimeout(cfg.Timeout)
	defer cancel()

	if strings.TrimSpace(flags.toolChoice) != "" {
		return errors.New("tool-choice is not supported for rlm-core")
	}
	if flags.subcallMaxOutputTokens < 0 {
		return errors.New("subcall-max-output-tokens must be >= 0")
	}
	if cmd.Flags().Changed("seed") {
		seed, seedErr := rlmrun.SeedFactFromPointer(&flags.seedValue)
		if seedErr != nil {
			return seedErr
		}
		flags.seed = seed.Pointer()
	} else {
		flags.seed = nil
	}
	flags.subcallReasoningEffort = strings.TrimSpace(flags.subcallReasoningEffort)
	if !validSubcallReasoningEffort(flags.subcallReasoningEffort) {
		return errors.New("invalid subcall-reasoning-effort (want none, minimal, low, medium, high, or xhigh)")
	}
	flags.subcallModel = strings.TrimSpace(flags.subcallModel)

	maxInlineBytes := flags.maxInlineBytes
	if maxInlineBytes <= 0 {
		maxInlineBytes = defaultRLMInlineBytes
	}
	policy := rlm.ContextPolicy{
		MaxInlineBytes: maxInlineBytes,
		MaxTotalBytes:  flags.maxTotalBytes,
		PreferInline:   true,
	}

	contextDir, err := os.MkdirTemp("", "modelrelay-rlm-context-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(contextDir); err != nil {
			log.Printf("warning: failed to remove temp dir %s: %v", contextDir, err)
		}
	}()
	contextPath := filepath.Join(contextDir, "context.json")
	plan, err := rlm.PlanContext(contextPayload, policy, contextPath)
	if err != nil {
		return err
	}
	if plan.Mode == rlm.ContextLoadFile && !flags.remote {
		if err := os.WriteFile(plan.ContextPath, contextPayload, 0o644); err != nil {
			return err
		}
	}

	if flags.remote {
		if strings.TrimSpace(flags.db) != "" || strings.TrimSpace(flags.postgresDSNEnv) != "" || strings.TrimSpace(flags.snowflakeBrokerURL) != "" || len(flags.mcpConfigs) > 0 {
			return errors.New("--db, --postgres-dsn-env, --snowflake-broker-url, and --mcp-config are local/VPC-mode only: trusted data-provider transports execute at the customer-controlled edge")
		}
		return runRLMRemote(ctx, cfg, apiKey, model, strings.Join(args, " "), contextPayload, plan, flags, len(files) > 0)
	}

	usage := &rlmUsage{}
	mcpMounts, err := loadLocalMCPMounts(flags.mcpConfigs)
	if err != nil {
		return err
	}
	postgresConnector, err := openLocalPostgresConnector(ctx, flags, cfg)
	if err != nil {
		return err
	}
	if postgresConnector != nil {
		defer func() {
			if err := postgresConnector.Close(); err != nil {
				log.Printf("warning: close PostgreSQL connector: %v", err)
			}
		}()
	}
	server, err := startLocalRLMServer(ctx, client, cfg, model, flags.maxDepth, flags.maxSubcalls, usage, localSubcallDefaults{
		MaxOutputTokens: flags.subcallMaxOutputTokens,
		Model:           flags.subcallModel,
		ReasoningEffort: flags.subcallReasoningEffort,
	}, newLocalPostgresBrokerConfig(flags, postgresConnector), mcpMounts.Secrets)
	if err != nil {
		return err
	}
	defer server.Close()

	dataSources, defaultSource, err := buildLocalSQLDataSource(flags, cfg, server)
	if err != nil {
		return err
	}
	dataSources = append(dataSources, mcpMounts.Sources...)
	defaultSource, err = resolveLocalDefaultSource(dataSources, defaultSource, flags.defaultSource)
	if err != nil {
		return err
	}

	query := strings.Join(args, " ")
	sessionID, err := randomToken()
	if err != nil {
		return err
	}
	if strings.TrimSpace(flags.snowflakeBrokerURL) != "" {
		for i := range dataSources {
			if dataSources[i].Type == "sql" && dataSources[i].BrokerURL != "" {
				dataSources[i].BrokerURL, err = brokerURLWithRunID(dataSources[i].BrokerURL, sessionID)
				if err != nil {
					return err
				}
			}
		}
	}
	systemPrompt := ""
	systemAdditions := ""
	if flags.systemOverride && strings.TrimSpace(flags.system) != "" {
		systemPrompt = strings.TrimSpace(flags.system)
	} else {
		systemAdditions = rlm.BuildRunnerSystemAdditions(flags.system, flags.maxDepth, flags.maxSubcalls)
	}

	contextInline := json.RawMessage(nil)
	contextFile := ""
	switch plan.Mode {
	case rlm.ContextLoadInline:
		contextInline = plan.InlineJSON
	case rlm.ContextLoadFile:
		contextFile = plan.ContextPath
	}
	subcallOutputTokens := flags.subcallMaxOutputTokens
	if subcallOutputTokens <= 0 {
		subcallOutputTokens = rlmrunner.DefaultSubcallOutputTokens
	}
	budget, err := rlmrunner.NewBudget(
		rlmrunner.DefaultTotalTokenBudget, flags.maxSubcalls, flags.maxDepth, rlmrunner.DefaultRunnerTimeoutMS,
		rlmrunner.DefaultRootOutputTokens, subcallOutputTokens,
	)
	if err != nil {
		return err
	}
	sandbox, err := rlmrunner.NewRunnerSandbox(defaultRLMMaxOutputChars, flags.execTimeoutMS, defaultRLMMaxOutputChars)
	if err != nil {
		return err
	}

	runnerReq := rlmrunner.RunnerRequest{
		Model:                  model,
		Budget:                 budget,
		Sandbox:                sandbox,
		Question:               query,
		SystemPrompt:           systemPrompt,
		SystemPromptAdditions:  systemAdditions,
		Context:                contextInline,
		ContextPath:            contextFile,
		DataSources:            dataSources,
		DefaultSource:          defaultSource,
		MaxDepth:               flags.maxDepth,
		MaxSubcalls:            flags.maxSubcalls,
		SubcallMaxOutputTokens: flags.subcallMaxOutputTokens,
		SubcallModel:           flags.subcallModel,
		SubcallReasoningEffort: flags.subcallReasoningEffort,
		SubcallConcurrency:     rlmrunner.DefaultSubcallConcurrency,
		ExecTimeoutMS:          flags.execTimeoutMS,
		MaxOutputChars:         defaultRLMMaxOutputChars,
		Token:                  server.Token,
		RootEndpoint:           server.RootEndpoint,
		SubcallEndpoint:        server.SubcallEndpoint,
		Session:                sessionID,
		SessionIndex:           1,
		Seed:                   flags.seed,
	}
	if len(mcpMounts.Sources) > 0 {
		runnerReq.MCPSecretEndpoint = server.MCPSecretURL
		runnerReq.MCPSecretToken = server.Token
		runnerReq.MCPAllowedNetworks = mcpMounts.AllowedNetworks
	}

	var runnerEnv []string
	if postgresEnv := strings.TrimSpace(flags.postgresDSNEnv); postgresEnv != "" {
		runnerEnv = environmentWithoutVariable(os.Environ(), postgresEnv)
	}
	if tokenEnv := strings.TrimSpace(flags.snowflakeBrokerTokenEnv); tokenEnv != "" {
		if runnerEnv == nil {
			runnerEnv = os.Environ()
		}
		runnerEnv = environmentWithoutVariable(runnerEnv, tokenEnv)
	}
	for _, secretEnv := range mcpMounts.SecretEnvNames {
		if runnerEnv == nil {
			runnerEnv = os.Environ()
		}
		runnerEnv = environmentWithoutVariable(runnerEnv, secretEnv)
	}
	interpreter := rlm.NewLocalInterpreter(rlm.LocalInterpreterConfig{
		PythonPath: flags.pythonPath,
		Env:        runnerEnv,
		Limits: rlm.InterpreterLimits{
			MaxTimeoutMS:   rlmrunner.DefaultRunnerTimeoutMS,
			MaxOutputBytes: defaultRLMMaxOutputChars,
		},
		Caps: rlm.InterpreterCapabilities{
			MaxInlineBytes: maxInlineBytes,
			MaxTotalBytes:  flags.maxTotalBytes,
		},
	})
	session, err := interpreter.Start(ctx, "rlm-local", nil)
	if err != nil {
		return err
	}
	defer session.Close()

	runtimeDir, err := rlmrunner.RuntimeDir()
	if err != nil {
		return err
	}

	runOpts := rlmrunner.RunOptions{
		RequestID: sessionID,
		TimeoutMS: interpreter.Limits().MaxTimeoutMS,
	}
	runOpts.OnProgress = func(evt rlmrunner.ProgressEvent) {
		fmt.Fprintf(os.Stderr, "rlm: %s\n", evt.Status)
	}
	runnerResult, err := rlmrunner.RunWithSession(ctx, session, runtimeDir, runnerReq, runOpts)
	if err != nil {
		// Prefer the runner's structured error message when present; still emit
		// whatever partial response and diagnostics we parsed (issue #1597).
		runErr := err
		if runnerResult.Response.Error != nil && strings.TrimSpace(runnerResult.Response.Error.Message) != "" {
			runErr = errors.New(runnerResult.Response.Error.Message)
		}
		return writeRLMLocalOutcome(cfg, usage, runnerResult.Response, runErr)
	}
	runnerResp := runnerResult.Response
	if !runnerResp.Ready {
		// A post-exhaustion extracted answer is a usable best-effort result;
		// it is flagged so callers never mistake it for a confirmed one.
		if !runnerResp.Extracted || strings.TrimSpace(runnerResp.Answer) == "" {
			return writeRLMLocalOutcome(cfg, usage, runnerResp, errors.New("RLM execution budget exhausted without a confirmed answer"))
		}
		fmt.Fprintln(os.Stderr, "rlm: execution budget exhausted; answer extracted from retained loop state (not confirmed)")
	}

	return writeRLMLocalOutcome(cfg, usage, runnerResp, nil)
}

// rlmJSONError is the machine-readable failure block for `mrl rlm --json`.
type rlmJSONError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message"`
}

// rlmJSONResult is the stdout envelope for `mrl rlm --json` (success and failure).
// On failure Error is set and the process still exits non-zero.
type rlmJSONResult struct {
	Answer             json.RawMessage         `json:"answer,omitempty"`
	Iterations         int                     `json:"iterations"`
	Subcalls           int                     `json:"subcalls"`
	DataSourceRequests *int                    `json:"data_source_requests,omitempty"`
	TotalUsage         workflow.TokenUsage     `json:"total_usage,omitempty"`
	Trajectory         workflow.RLMContentFact `json:"trajectory"`
	Ready              bool                    `json:"ready"`
	Extracted          bool                    `json:"extracted,omitempty"`
	Error              *rlmJSONError           `json:"error,omitempty"`
}

func buildRLMJSONResult(usage *rlmUsage, resp rlmrunner.RunnerResponse, runErr error) (rlmJSONResult, error) {
	// Always marshal answer (including "") so clients can distinguish empty
	// answer from missing field — matches pre-#1597 success encoding.
	answerPayload, err := json.Marshal(resp.Answer)
	if err != nil {
		return rlmJSONResult{}, err
	}
	totalUsage := workflow.TokenUsage{}
	if usage != nil {
		totalUsage = usage.snapshot()
	}
	result := rlmJSONResult{
		Answer:             answerPayload,
		Iterations:         resp.Iterations,
		Subcalls:           resp.Subcalls,
		DataSourceRequests: resp.DataSourceRequests,
		TotalUsage:         totalUsage,
		Trajectory:         workflow.UnavailableRLMContent("default_no_content_retention"),
		Ready:              resp.Ready,
		Extracted:          resp.Extracted,
	}
	if runErr != nil {
		errType := "RLMError"
		msg := runErr.Error()
		if resp.Error != nil {
			if strings.TrimSpace(resp.Error.Type) != "" {
				errType = resp.Error.Type
			}
			if strings.TrimSpace(resp.Error.Message) != "" {
				msg = resp.Error.Message
			}
		}
		result.Error = &rlmJSONError{Type: errType, Message: msg}
	}
	return result, nil
}

// writeRLMLocalOutcome prints the RLM result. In --json mode it always writes the
// full envelope (including on failure) so scripts never see empty stdout (#1597).
// Non-JSON failure mode returns the error without writing an answer line.
func writeRLMLocalOutcome(cfg runtimeConfig, usage *rlmUsage, resp rlmrunner.RunnerResponse, runErr error) error {
	return writeRLMLocalOutcomeTo(os.Stdout, cfg, usage, resp, runErr)
}

func writeRLMLocalOutcomeTo(w io.Writer, cfg runtimeConfig, usage *rlmUsage, resp rlmrunner.RunnerResponse, runErr error) error {
	if cfg.Output == outputFormatJSON {
		result, err := buildRLMJSONResult(usage, resp, runErr)
		if err != nil {
			if runErr != nil {
				return errors.Join(runErr, err)
			}
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			if runErr != nil {
				return errors.Join(runErr, err)
			}
			return err
		}
		return runErr
	}
	if runErr != nil {
		return runErr
	}
	_, err := fmt.Fprintln(w, resp.Answer)
	return err
}

func callLLM(ctx context.Context, client *sdk.Client, model string, input []llm.InputItem, maxOutputTokens int64, reasoningEffort string) (*sdk.Response, error) {
	builder := client.Responses.New().Model(sdk.NewModelID(model)).Input(input)
	if maxOutputTokens > 0 {
		builder = builder.MaxOutputTokens(maxOutputTokens)
	}
	if reasoningEffort != "" {
		builder = builder.ReasoningEffort(reasoningEffort)
	}
	req, opts, err := builder.Build()
	if err != nil {
		return nil, err
	}
	resp, err := client.Responses.Create(ctx, req, opts...)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// defaultLocalSQLPolicy is the inline read-only policy used when --db is given
// without --sql-profile: SELECT-only with sane limits, but otherwise permissive
// (aggregates, joins, subqueries) — it's the user's own local file, and the
// restrictive knobs exist for locked-down customer profiles, not for this. The
// cloud validator enforces it; the SQLite file is also opened mode=ro at the
// edge as defense in depth.
var defaultLocalSQLPolicy = json.RawMessage(`{
	"dialect": "sqlite",
	"read_only": true,
	"limits": {"default_limit": 1000, "max_limit": 10000, "timeout_ms": 5000},
	"aggregations": {"allowed": true, "functions": ["count", "sum", "avg", "min", "max", "total", "group_concat"]},
	"subqueries": {"allowed": true}
}`)

var defaultLocalPostgresPolicy = json.RawMessage(`{
	"dialect": "postgres",
	"read_only": true,
	"limits": {"default_limit": 1000, "max_limit": 1000, "timeout_ms": 5000},
	"aggregations": {"allowed": true, "functions": ["count", "sum", "avg", "min", "max", "string_agg", "array_agg", "json_agg"]},
	"subqueries": {"allowed": true}
}`)

// buildLocalSQLDataSource turns --db/--db-name/--sql-profile into the runner's
// declarative data_sources entry (docs/design/sql-data-source.md §4): the spec
// carries the SQLite path (edge, local machine) and the loopback validate URL —
// never database credentials, and rows never leave the process.
func buildLocalSQLDataSource(flags *rlmFlags, cfg runtimeConfig, server localRLMServer) ([]rlmrunner.DataSourceSpec, string, error) {
	dbPath := strings.TrimSpace(flags.db)
	postgresEnv := strings.TrimSpace(flags.postgresDSNEnv)
	snowflakeURL := strings.TrimSpace(flags.snowflakeBrokerURL)
	configured := 0
	for _, present := range []bool{dbPath != "", postgresEnv != "", snowflakeURL != ""} {
		if present {
			configured++
		}
	}
	if configured > 1 {
		return nil, "", errors.New("--db, --postgres-dsn-env, and --snowflake-broker-url are mutually exclusive")
	}
	if configured == 0 {
		if strings.TrimSpace(flags.snowflakeBrokerTokenEnv) != "" {
			return nil, "", errors.New("--snowflake-broker-token-env requires --snowflake-broker-url")
		}
		return nil, "", nil
	}
	if strings.TrimSpace(cfg.APIKey) == "" && snowflakeURL == "" {
		return nil, "", errors.New("SQL data sources require an API key (the policy check runs via ModelRelay /sql/validate)")
	}
	name := strings.TrimSpace(flags.dbName)
	if name == "" {
		name = "db"
	}
	if postgresEnv != "" {
		if server.BrokerURL == "" {
			return nil, "", errors.New("PostgreSQL edge broker is unavailable")
		}
		return []rlmrunner.DataSourceSpec{{
			Type:        "sql",
			Name:        name,
			BrokerURL:   server.BrokerURL,
			BrokerToken: server.Token,
		}}, name, nil
	}
	if snowflakeURL != "" {
		if strings.TrimSpace(flags.sqlProfile) != "" {
			return nil, "", errors.New("--sql-profile cannot be used with --snowflake-broker-url; the trusted broker owns the policy")
		}
		parsed, err := url.Parse(snowflakeURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Path != "/v1/sql" {
			return nil, "", errors.New("--snowflake-broker-url must be an HTTP(S) /v1/sql URL without userinfo, query, or fragment")
		}
		tokenEnv := strings.TrimSpace(flags.snowflakeBrokerTokenEnv)
		if tokenEnv == "" {
			return nil, "", errors.New("--snowflake-broker-url requires --snowflake-broker-token-env")
		}
		token, err := requiredSecretEnvironment(tokenEnv)
		if err != nil {
			return nil, "", err
		}
		return []rlmrunner.DataSourceSpec{{
			Type: "sql", Name: name, BrokerURL: snowflakeURL, BrokerToken: token,
		}}, name, nil
	}
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, "", err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, "", fmt.Errorf("--db: %w", err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("--db: %s is a directory, expected a SQLite file", absPath)
	}
	spec := rlmrunner.DataSourceSpec{
		Type:          "sql",
		Name:          name,
		SQLitePath:    absPath,
		ValidateURL:   server.ValidateURL,
		ValidateToken: server.Token,
	}
	if profile := strings.TrimSpace(flags.sqlProfile); profile != "" {
		spec.ProfileID = profile
	} else {
		spec.Policy = defaultLocalSQLPolicy
	}
	return []rlmrunner.DataSourceSpec{spec}, name, nil
}

func brokerURLWithRunID(rawURL, runID string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse Snowflake broker URL: %w", err)
	}
	values := parsed.Query()
	values.Set("run_id", runID)
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func openLocalPostgresConnector(ctx context.Context, flags *rlmFlags, cfg runtimeConfig) (*postgressource.Connector, error) {
	envName := strings.TrimSpace(flags.postgresDSNEnv)
	if envName == "" {
		return nil, nil
	}
	if strings.TrimSpace(flags.db) != "" {
		return nil, errors.New("--db and --postgres-dsn-env are mutually exclusive")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("--postgres-dsn-env requires an API key for SQL policy validation")
	}
	dsn, ok := os.LookupEnv(envName)
	if !ok || strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("--postgres-dsn-env: environment variable %q is unset or empty", envName)
	}
	connector, err := postgressource.Open(ctx, dsn, postgressource.Limits{})
	if err != nil {
		return nil, fmt.Errorf("--postgres-dsn-env %q: %w", envName, err)
	}
	return connector, nil
}

func environmentWithoutVariable(environment []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type postgresSQLSource interface {
	Query(context.Context, string, int) ([]map[string]any, error)
	Schema(context.Context) (string, error)
}

type localPostgresBrokerConfig struct {
	source    postgresSQLSource
	profileID string
	policy    json.RawMessage
}

func newLocalPostgresBrokerConfig(flags *rlmFlags, source *postgressource.Connector) *localPostgresBrokerConfig {
	if source == nil {
		return nil
	}
	config := &localPostgresBrokerConfig{source: source}
	if profile := strings.TrimSpace(flags.sqlProfile); profile != "" {
		config.profileID = profile
	} else {
		config.policy = defaultLocalPostgresPolicy
	}
	return config
}

// localRLMServer is the loopback server the local runner talks to: LLM
// root/subcall proxies, plus a /sql/validate forwarder when a SQL data source
// is attached (the runner sends only the SQL string; mrl adds the API key).
type localRLMServer struct {
	SubcallEndpoint string
	RootEndpoint    string
	ValidateURL     string
	BrokerURL       string
	MCPSecretURL    string
	Token           string
	Close           func()
}

// localSubcallDefaults carries the --subcall-* flag values into the local
// subcall proxy; per-payload values from the runner take precedence
// (rlm-core#25).
type localSubcallDefaults struct {
	MaxOutputTokens int64
	Model           string
	ReasoningEffort string
}

func startLocalRLMServer(ctx context.Context, client *sdk.Client, cfg runtimeConfig, defaultModel string, maxDepth, maxSubcalls int, usage *rlmUsage, subcallDefaults localSubcallDefaults, postgresBroker *localPostgresBrokerConfig, mcpSecrets map[localMCPSecretKey]string) (localRLMServer, error) {
	if maxSubcalls < 0 {
		return localRLMServer{}, errors.New("max_subcalls must be >= 0")
	}
	token, err := randomToken()
	if err != nil {
		return localRLMServer{}, err
	}
	counter := 0

	subcallHandler := &localSubcallHandler{
		ctx:             ctx,
		client:          client,
		defaultModel:    defaultModel,
		subcallDefaults: subcallDefaults,
		maxDepth:        maxDepth,
		maxSubcalls:     maxSubcalls,
		token:           token,
		counter:         &counter,
		usage:           usage,
	}
	rootHandler := &localRootHandler{
		ctx:          ctx,
		client:       client,
		defaultModel: defaultModel,
		token:        token,
		usage:        usage,
	}

	mux := http.NewServeMux()
	mux.Handle("/rlm/subcall", subcallHandler)
	mux.Handle("/rlm/root", rootHandler)
	mux.Handle("/sql/validate", &localSQLValidateHandler{
		ctx:     ctx,
		baseURL: cfg.BaseURL,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		token:   token,
	})
	if postgresBroker != nil {
		mux.Handle("/sql/source", &localPostgresBrokerHandler{
			baseURL:   cfg.BaseURL,
			apiKey:    strings.TrimSpace(cfg.APIKey),
			token:     token,
			source:    postgresBroker.source,
			profileID: postgresBroker.profileID,
			policy:    postgresBroker.policy,
		})
	}
	// A non-nil empty map represents an unauthenticated MCP mount. Keep the
	// callback route present whenever MCP is enabled so the runner request and
	// host capability surface derive from the same source of truth.
	if mcpSecrets != nil {
		mux.Handle("/mcp/secret", &localMCPSecretBrokerHandler{token: token, secrets: mcpSecrets})
	}
	server := httptest.NewServer(mux)
	return localRLMServer{
		SubcallEndpoint: server.URL + "/rlm/subcall",
		RootEndpoint:    server.URL + "/rlm/root",
		ValidateURL:     server.URL + "/sql/validate",
		BrokerURL:       server.URL + "/sql/source",
		MCPSecretURL:    server.URL + "/mcp/secret",
		Token:           token,
		Close:           server.Close,
	}, nil
}

type localPostgresBrokerHandler struct {
	baseURL   string
	apiKey    string
	token     string
	source    postgresSQLSource
	profileID string
	policy    json.RawMessage
}

func (h *localPostgresBrokerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !validBearerToken(r, h.token) {
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
	var result any
	switch request.Method {
	case "schema":
		var err error
		result, err = h.source.Schema(r.Context())
		if err != nil {
			writeSQLBrokerError(w, http.StatusBadGateway, err.Error())
			return
		}
	case "query":
		if strings.TrimSpace(request.SQL) == "" {
			writeSQLBrokerError(w, http.StatusBadRequest, "query requires non-empty SQL")
			return
		}
		normalized, timeoutMS, err := h.validate(r.Context(), request.SQL)
		if err != nil {
			writeSQLBrokerError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err = h.source.Query(r.Context(), normalized, timeoutMS)
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
		log.Printf("warning: encode PostgreSQL broker response: %v", err)
	}
}

func (h *localPostgresBrokerHandler) validate(ctx context.Context, sqlText string) (string, int, error) {
	payload := map[string]any{"sql": sqlText}
	if h.profileID != "" {
		payload["profile_id"] = h.profileID
	} else {
		payload["policy"] = h.policy
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, errors.New("encode SQL validation request")
	}
	endpoint := strings.TrimSuffix(h.baseURL, "/") + "/sql/validate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, errors.New("build SQL validation request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ModelRelay-Api-Key", h.apiKey)
	if header := strings.TrimSpace(clientHeader()); header != "" {
		req.Header.Set("X-ModelRelay-Client", header)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, errors.New("SQL policy validation failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, errors.New("read SQL validation response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("SQL policy rejected the query: %s", extractAPIErrorMessage(raw, resp.Status))
	}
	var validated struct {
		Valid         bool   `json:"valid"`
		NormalizedSQL string `json:"normalized_sql"`
		TimeoutMS     int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(raw, &validated); err != nil || !validated.Valid || strings.TrimSpace(validated.NormalizedSQL) == "" {
		return "", 0, errors.New("SQL policy validator returned an invalid response")
	}
	return validated.NormalizedSQL, validated.TimeoutMS, nil
}

func validBearerToken(r *http.Request, expected string) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return false
	}
	actual := strings.TrimSpace(auth[len("bearer "):])
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func writeSQLBrokerError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}

func extractAPIErrorMessage(raw []byte, fallback string) string {
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &response) == nil {
		if message := strings.TrimSpace(response.Error.Message); message != "" {
			return message
		}
		if message := strings.TrimSpace(response.Message); message != "" {
			return message
		}
	}
	return fallback
}

// localSQLValidateHandler forwards the runner's policy-validation calls to the
// cloud /sql/validate endpoint with mrl's credentials. Only the SQL string and
// policy/profile reference pass through — never rows, never the database.
type localSQLValidateHandler struct {
	ctx     context.Context
	baseURL string
	apiKey  string
	token   string
}

func (h *localSQLValidateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.TrimSpace(auth[len("bearer "):]) != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	endpoint := strings.TrimSuffix(h.baseURL, "/") + "/sql/validate"
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if h.apiKey != "" {
		req.Header.Set("X-ModelRelay-Api-Key", h.apiKey)
	}
	if header := strings.TrimSpace(clientHeader()); header != "" {
		req.Header.Set("X-ModelRelay-Client", header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("validate call failed: %v", err), http.StatusBadGateway)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, 1<<20)); err != nil {
		log.Printf("warning: copy validate response: %v", err)
	}
}

type localSubcallHandler struct {
	ctx             context.Context
	client          *sdk.Client
	defaultModel    string
	subcallDefaults localSubcallDefaults
	maxDepth        int
	maxSubcalls     int
	token           string
	counter         *int
	usage           *rlmUsage
	mu              sync.Mutex
}

func (h *localSubcallHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.TrimSpace(auth[len("bearer "):]) != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Prompt          string  `json:"prompt"`
		Model           *string `json:"model,omitempty"`
		MaxOutputTokens *int64  `json:"max_output_tokens,omitempty"`
		ReasoningEffort *string `json:"reasoning_effort,omitempty"`
		Depth           int     `json:"depth,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}
	if req.Depth < 0 {
		http.Error(w, "invalid depth", http.StatusBadRequest)
		return
	}
	if h.maxDepth >= 0 && req.Depth > h.maxDepth {
		http.Error(w, "max depth exceeded", http.StatusConflict)
		return
	}

	h.mu.Lock()
	*h.counter = *h.counter + 1
	current := *h.counter
	h.mu.Unlock()
	if h.maxSubcalls >= 0 && current > h.maxSubcalls {
		http.Error(w, "max subcalls exceeded", http.StatusConflict)
		return
	}

	// Model precedence: per-payload override > --subcall-model > root model.
	model := h.defaultModel
	if h.subcallDefaults.Model != "" {
		model = h.subcallDefaults.Model
	}
	if req.Model != nil && strings.TrimSpace(*req.Model) != "" {
		model = strings.TrimSpace(*req.Model)
	}
	if model == "" {
		http.Error(w, "model required", http.StatusBadRequest)
		return
	}

	// Subcall cost controls (rlm-core#25): default to bounded output and no
	// thinking, mirroring the hosted /rlm/subcall defaults. Precedence:
	// per-payload value > --subcall-* flag > default.
	maxOutputTokens := h.subcallDefaults.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = localDefaultSubcallMaxOutputTokens
	}
	if req.MaxOutputTokens != nil {
		if *req.MaxOutputTokens <= 0 {
			http.Error(w, "max_output_tokens must be > 0", http.StatusBadRequest)
			return
		}
		maxOutputTokens = *req.MaxOutputTokens
	}
	reasoningEffort := h.subcallDefaults.ReasoningEffort
	if reasoningEffort == "" {
		reasoningEffort = localDefaultSubcallReasoningEffort
	}
	if req.ReasoningEffort != nil {
		effort := strings.TrimSpace(*req.ReasoningEffort)
		if !validSubcallReasoningEffort(effort) {
			http.Error(w, "invalid reasoning_effort (want none, minimal, low, medium, high, or xhigh)", http.StatusBadRequest)
			return
		}
		reasoningEffort = effort
	}

	resp, err := callLLM(h.ctx, h.client, model, []llm.InputItem{llm.NewUserText(req.Prompt)}, maxOutputTokens, reasoningEffort)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if h.usage != nil {
		h.usage.add(resp.Usage)
	}

	text := strings.TrimSpace(resp.AssistantText())
	if text == "" {
		http.Error(w, "missing response", http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"result": text})
}

type localRootMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type localRootRequest struct {
	Messages        []localRootMessage `json:"messages"`
	Model           *string            `json:"model,omitempty"`
	Provider        string             `json:"provider,omitempty"`
	MaxOutputTokens int64              `json:"max_output_tokens,omitempty"`
	Temperature     *float64           `json:"temperature,omitempty"`
	Stop            []string           `json:"stop,omitempty"`
	Session         string             `json:"session,omitempty"`
	SessionIndex    int64              `json:"session_index,omitempty"`
}

type localRootUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type localRootResponse struct {
	Result     string         `json:"result"`
	Usage      localRootUsage `json:"usage"`
	Provider   string         `json:"provider,omitempty"`
	ResponseID string         `json:"response_id,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
	Model      string         `json:"model,omitempty"`
}

type localRootHandler struct {
	ctx          context.Context
	client       *sdk.Client
	defaultModel string
	token        string
	usage        *rlmUsage
}

func (h *localRootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.TrimSpace(auth[len("bearer "):]) != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req localRootRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}

	model := h.defaultModel
	if req.Model != nil && strings.TrimSpace(*req.Model) != "" {
		model = strings.TrimSpace(*req.Model)
	}
	if model == "" {
		http.Error(w, "model required", http.StatusBadRequest)
		return
	}

	input, err := buildLocalRLMRootInput(req.Messages)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	builder := h.client.Responses.New().Model(sdk.NewModelID(model)).Input(input)
	if provider := strings.TrimSpace(req.Provider); provider != "" {
		builder = builder.Provider(sdk.NewProviderID(provider))
	}
	if req.MaxOutputTokens > 0 {
		builder = builder.MaxOutputTokens(req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		builder = builder.Temperature(*req.Temperature)
	}
	if len(req.Stop) > 0 {
		builder = builder.Stop(req.Stop...)
	}

	request, opts, err := builder.Build()
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	resp, err := h.client.Responses.Create(h.ctx, request, opts...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if h.usage != nil {
		h.usage.add(resp.Usage)
	}

	text := strings.TrimSpace(resp.AssistantText())
	if text == "" {
		http.Error(w, "missing response", http.StatusBadGateway)
		return
	}

	payload := localRootResponse{
		Result:     text,
		Usage:      localRootUsage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, TotalTokens: resp.Usage.TotalTokens},
		Provider:   resp.Provider,
		ResponseID: resp.ID,
		StopReason: string(resp.StopReason),
		Model:      resp.Model.String(),
	}
	writeJSON(w, http.StatusOK, payload)
}

func buildLocalRLMRootInput(messages []localRootMessage) ([]llm.InputItem, error) {
	if len(messages) == 0 {
		return nil, errors.New("messages required")
	}
	items := make([]llm.InputItem, 0, len(messages))
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case string(llm.RoleSystem):
			items = append(items, llm.NewSystemText(msg.Content))
		case string(llm.RoleUser):
			items = append(items, llm.NewUserText(msg.Content))
		case string(llm.RoleAssistant):
			items = append(items, llm.NewAssistantText(msg.Content))
		default:
			return nil, fmt.Errorf("unsupported message role")
		}
	}
	return items, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func resolveRLMInlineTextLimit(override int64, maxInlineBytes int64) int64 {
	if override > 0 {
		return override
	}
	if maxInlineBytes > 0 {
		return maxInlineBytes
	}
	return 1024 * 1024
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Headers already written; log the error since we can't change the response
		log.Printf("writeJSON encode error: %v", err)
	}
}

const rlmRemoteAttachmentNote = "Files in context include inline text only; do not attempt to open local file paths."

type rlmExecuteRemoteRequest struct {
	Model                  string          `json:"model"`
	Query                  string          `json:"query"`
	Context                json.RawMessage `json:"context,omitempty"`
	ContextRef             string          `json:"context_ref,omitempty"`
	SystemPrompt           string          `json:"system_prompt,omitempty"`
	MaxDepth               *int            `json:"max_depth,omitempty"`
	MaxSubcalls            *int            `json:"max_subcalls,omitempty"`
	TimeoutMS              *int            `json:"timeout_ms,omitempty"`
	Seed                   *int64          `json:"seed"`
	SubcallMaxOutputTokens int64           `json:"subcall_max_output_tokens,omitempty"`
	SubcallModel           string          `json:"subcall_model,omitempty"`
	SubcallReasoningEffort string          `json:"subcall_reasoning_effort,omitempty"`
}

type rlmExecuteRemoteResult struct {
	Raw      json.RawMessage
	Answer   json.RawMessage
	Progress []rlmrunner.ProgressEvent
}

type rlmContextCreateRequest struct {
	Context json.RawMessage `json:"context"`
}

type rlmContextCreateResponse struct {
	ID string `json:"id"`
}

func runRLMRemote(ctx context.Context, cfg runtimeConfig, apiKey sdk.APIKeyAuth, model string, query string, contextPayload json.RawMessage, plan rlm.ContextPlan, flags *rlmFlags, hasAttachments bool) error {
	if flags.systemOverride {
		return errors.New("system-override is not supported with --remote")
	}
	if flags.execTimeoutMS != 0 {
		return errors.New("--exec-timeout-ms is local-mode only; hosted run wall time is owned by the resolved execution profile")
	}

	systemPrompt := strings.TrimSpace(flags.system)
	if hasAttachments {
		systemPrompt = appendSystemPrompt(systemPrompt, rlmRemoteAttachmentNote)
	}

	contextInline := json.RawMessage(nil)
	contextRef := ""
	switch plan.Mode {
	case rlm.ContextLoadInline:
		if !isJSONNull(contextPayload) {
			contextInline = contextPayload
		}
	case rlm.ContextLoadFile:
		ref, err := createRLMContextRemote(ctx, nil, cfg.BaseURL, apiKey, contextPayload)
		if err != nil {
			return err
		}
		contextRef = ref
	}

	maxDepth := flags.maxDepth
	maxSubcalls := flags.maxSubcalls

	req := rlmExecuteRemoteRequest{
		Model:                  model,
		Query:                  query,
		Context:                contextInline,
		ContextRef:             contextRef,
		SystemPrompt:           systemPrompt,
		MaxDepth:               &maxDepth,
		MaxSubcalls:            &maxSubcalls,
		Seed:                   flags.seed,
		SubcallMaxOutputTokens: flags.subcallMaxOutputTokens,
		SubcallModel:           flags.subcallModel,
		SubcallReasoningEffort: flags.subcallReasoningEffort,
	}
	result, err := executeRLMRemote(ctx, nil, cfg.BaseURL, apiKey, req)
	if err != nil {
		return err
	}

	if cfg.Output == outputFormatJSON {
		return writeRawJSON(os.Stdout, result.Raw)
	}

	writeRLMProgress(os.Stderr, result.Progress)
	return writeRLMAnswer(os.Stdout, result.Answer)
}

func executeRLMRemote(ctx context.Context, httpClient *http.Client, baseURL string, apiKey sdk.APIKeyAuth, req rlmExecuteRemoteRequest) (rlmExecuteRemoteResult, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/rlm/execute"
	if endpoint == "" {
		return rlmExecuteRemoteResult{}, errors.New("base URL is required")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return rlmExecuteRemoteResult{}, fmt.Errorf("encode rlm execute request: %w", err)
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return rlmExecuteRemoteResult{}, fmt.Errorf("build rlm execute request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != nil && strings.TrimSpace(apiKey.String()) != "" {
		httpReq.Header.Set("X-ModelRelay-Api-Key", apiKey.String())
	}
	if header := strings.TrimSpace(clientHeader()); header != "" {
		httpReq.Header.Set("X-ModelRelay-Client", header)
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return rlmExecuteRemoteResult{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return rlmExecuteRemoteResult{}, fmt.Errorf("read rlm execute response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return rlmExecuteRemoteResult{}, fmt.Errorf("rlm execute failed (%d): %s", resp.StatusCode, msg)
	}

	var partial struct {
		Answer   json.RawMessage           `json:"answer"`
		Progress []rlmrunner.ProgressEvent `json:"progress,omitempty"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return rlmExecuteRemoteResult{}, fmt.Errorf("decode rlm execute response: %w", err)
	}

	return rlmExecuteRemoteResult{
		Raw:      body,
		Answer:   partial.Answer,
		Progress: partial.Progress,
	}, nil
}

func createRLMContextRemote(ctx context.Context, httpClient *http.Client, baseURL string, apiKey sdk.APIKeyAuth, contextPayload json.RawMessage) (string, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/rlm/context"
	if endpoint == "" {
		return "", errors.New("base URL is required")
	}
	payload, err := json.Marshal(rlmContextCreateRequest{Context: contextPayload})
	if err != nil {
		return "", fmt.Errorf("encode rlm context request: %w", err)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build rlm context request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != nil && strings.TrimSpace(apiKey.String()) != "" {
		httpReq.Header.Set("X-ModelRelay-Api-Key", apiKey.String())
	}
	if header := strings.TrimSpace(clientHeader()); header != "" {
		httpReq.Header.Set("X-ModelRelay-Client", header)
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read rlm context response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return "", fmt.Errorf("rlm context upload failed (%d): %s", resp.StatusCode, msg)
	}

	var parsed rlmContextCreateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode rlm context response: %w", err)
	}
	if strings.TrimSpace(parsed.ID) == "" {
		return "", errors.New("rlm context response missing id")
	}
	return parsed.ID, nil
}

func validateRLMRemoteAttachments(files []rlmFileAttachment) error {
	for _, file := range files {
		if strings.TrimSpace(file.Text) == "" {
			label := file.Name
			if strings.TrimSpace(label) == "" {
				label = file.Path
			}
			if strings.TrimSpace(label) == "" {
				label = "attachment"
			}
			return fmt.Errorf("remote RLM requires inline text for %q (use --inline-text-max-bytes or drop --remote)", label)
		}
	}
	return nil
}

func stripRemoteAttachmentPaths(files []rlmFileAttachment) []rlmFileAttachment {
	if len(files) == 0 {
		return files
	}
	out := make([]rlmFileAttachment, 0, len(files))
	for _, file := range files {
		file.Path = ""
		out = append(out, file)
	}
	return out
}

func appendSystemPrompt(base, addition string) string {
	if strings.TrimSpace(addition) == "" {
		return strings.TrimSpace(base)
	}
	if strings.TrimSpace(base) == "" {
		return strings.TrimSpace(addition)
	}
	return strings.TrimSpace(base) + "\n\n" + strings.TrimSpace(addition)
}

func writeRLMProgress(w io.Writer, events []rlmrunner.ProgressEvent) {
	for _, evt := range events {
		if strings.TrimSpace(evt.Status) == "" {
			continue
		}
		fmt.Fprintf(w, "rlm: %s\n", evt.Status)
	}
}

func writeRLMAnswer(w io.Writer, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		_, err = fmt.Fprintln(w, text)
		return err
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err == nil {
		formatted, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = w.Write(append(formatted, '\n'))
		return err
	}
	_, err := fmt.Fprintln(w, string(trimmed))
	return err
}

func writeRawJSON(w io.Writer, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = w.Write(append(raw, '\n'))
		return err
	}
	_, err := w.Write(append(buf.Bytes(), '\n'))
	return err
}

func isJSONNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
