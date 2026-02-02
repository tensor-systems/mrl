package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"

	"github.com/modelrelay/modelrelay/platform/rlm"
	"github.com/modelrelay/modelrelay/platform/workflow"
	sdk "github.com/modelrelay/modelrelay/sdk/go"
	"github.com/modelrelay/modelrelay/sdk/go/llm"
	sdkrlm "github.com/modelrelay/modelrelay/sdk/go/rlm"
	"github.com/spf13/cobra"
)

func newRLMCmd() *cobra.Command {
	var flags rlmFlags

	cmd := &cobra.Command{
		Use:   "rlm <query>",
		Short: "Run a local RLM session (Python runs locally, LLM calls via ModelRelay)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRLM(cmd, args, &flags)
		},
	}

	cmd.Flags().StringVar(&flags.model, "model", "", "Model ID (overrides profile default)")
	cmd.Flags().StringArrayVarP(&flags.attachments, "attachment", "a", nil, "Attach a local file (repeatable; use '-' for stdin)")
	cmd.Flags().StringVar(&flags.attachmentType, "attachment-type", "", "Override attachment MIME type (useful for stdin)")
	cmd.Flags().BoolVar(&flags.attachStdin, "attach-stdin", false, "Attach stdin as a file")
	cmd.Flags().IntVar(&flags.maxIterations, "max-iterations", 10, "Max code generation cycles")
	cmd.Flags().IntVar(&flags.maxSubcalls, "max-subcalls", 50, "Max llm_query/llm_batch calls")
	cmd.Flags().IntVar(&flags.maxDepth, "max-depth", 1, "Max recursion depth")
	cmd.Flags().IntVar(&flags.execTimeoutMS, "exec-timeout-ms", 0, "Python execution timeout in ms (0 uses interpreter default)")
	cmd.Flags().StringVar(&flags.pythonPath, "python", "", "Python executable (default: python3)")
	cmd.Flags().Int64Var(&flags.maxInlineBytes, "max-inline-bytes", 0, "Max inline context bytes (0 uses interpreter default)")
	cmd.Flags().Int64Var(&flags.maxTotalBytes, "max-total-bytes", 0, "Max total context bytes (0 uses interpreter default)")
	cmd.Flags().Int64Var(&flags.inlineTextMaxBytes, "inline-text-max-bytes", 0, "Max inline text bytes per file (0 uses default)")
	cmd.Flags().StringVar(&flags.system, "system", "", "Custom instructions prepended to the default RLM system prompt")
	cmd.Flags().BoolVar(&flags.systemOverride, "system-override", false, "Replace the entire system prompt instead of prepending")
	cmd.Flags().StringVar(&flags.toolChoice, "tool-choice", "", "Tool choice mode: auto, required, none")

	return cmd
}

type rlmFlags struct {
	model              string
	system             string
	systemOverride     bool
	attachments        []string
	attachmentType     string
	attachStdin        bool
	maxIterations      int
	maxSubcalls        int
	maxDepth           int
	execTimeoutMS      int
	pythonPath         string
	maxInlineBytes     int64
	maxTotalBytes      int64
	inlineTextMaxBytes int64
	toolChoice         string
}

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

	contextPayload, err := mergeRLMContextFiles([]byte("null"), files)
	if err != nil {
		return err
	}

	client, err := newPromptClient(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := contextWithTimeout(cfg.Timeout)
	defer cancel()

	interp := sdkrlm.NewLocalInterpreter(sdkrlm.LocalInterpreterConfig{
		PythonPath: flags.pythonPath,
		Caps: sdkrlm.InterpreterCapabilities{
			MaxInlineBytes: flags.maxInlineBytes,
			MaxTotalBytes:  flags.maxTotalBytes,
		},
	})

	plan, err := interp.PlanContext(contextPayload, "context.json")
	if err != nil {
		return err
	}

	usage := &rlmUsage{}
	endpoint, token, subcalls, closeServer, err := startLocalRLMSubcallServer(ctx, client, model, flags.maxDepth, flags.maxSubcalls, usage)
	if err != nil {
		return err
	}
	defer closeServer()

	wrapper := rlm.BuildRLMWrapperWithPlan(sdkContextPlanToPlatform(plan))
	query := strings.Join(args, " ")

	toolChoice, err := parseToolChoice(flags.toolChoice)
	if err != nil {
		return err
	}

	result, err := runLocalRLMLoop(ctx, localRLMParams{
		client:         client,
		interpreter:    interp,
		endpoint:       endpoint,
		token:          token,
		query:          query,
		model:          model,
		system:         flags.system,
		systemOverride: flags.systemOverride,
		toolChoice:     toolChoice,
		maxIterations:  flags.maxIterations,
		maxSubcalls:    flags.maxSubcalls,
		maxDepth:       flags.maxDepth,
		execTimeoutMS:  flags.execTimeoutMS,
		contextPayload: contextPayload,
		contextPlan:    plan,
		wrapper:        wrapper,
		subcallCounter: subcalls,
		usage:          usage,
	})
	if err != nil {
		return err
	}

	if cfg.Output == outputFormatJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Println(string(result.Answer))
	return nil
}

type localRLMParams struct {
	client         *sdk.Client
	interpreter    *sdkrlm.LocalInterpreter
	endpoint       string
	token          string
	query          string
	model          string
	system         string
	systemOverride bool
	toolChoice     *llm.ToolChoice
	maxIterations  int
	maxSubcalls    int
	maxDepth       int
	execTimeoutMS  int
	contextPayload []byte
	contextPlan    sdkrlm.ContextPlan
	wrapper        string
	subcallCounter *int
	usage          *rlmUsage
}

// sdkContextPlanToPlatform converts SDK ContextPlan to platform ContextPlan.
func sdkContextPlanToPlatform(p sdkrlm.ContextPlan) rlm.ContextPlan {
	return rlm.ContextPlan{
		Mode:        rlm.ContextLoadMode(p.Mode),
		ContextPath: p.ContextPath,
		InlineJSON:  p.InlineJSON,
	}
}

// sdkExecResultToPlatform converts SDK ExecutionResult to platform ExecutionResult.
func sdkExecResultToPlatform(r *sdkrlm.ExecutionResult) *rlm.ExecutionResult {
	if r == nil {
		return nil
	}
	return &rlm.ExecutionResult{
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		ExitCode:   r.ExitCode,
		DurationMS: r.DurationMS,
		TimedOut:   r.TimedOut,
	}
}

func runLocalRLMLoop(ctx context.Context, params localRLMParams) (workflow.RLMResultV1, error) {
	if strings.TrimSpace(params.query) == "" {
		return workflow.RLMResultV1{}, errors.New("query is required")
	}

	var (
		trajectory []workflow.RLMIterationV1
		answer     json.RawMessage
	)

	var (
		session      sdkrlm.CodeSession
		sessionID    string
		sessionIndex int64
	)
	startSession := func() error {
		nextIndex := sessionIndex + 1
		nextSession, nextID, err := startLocalRLMSession(ctx, params.interpreter, params.contextPlan, params.contextPayload)
		if err != nil {
			return err
		}
		if session != nil {
			session.Close()
		}
		session = nextSession
		sessionID = nextID
		sessionIndex = nextIndex
		return nil
	}
	if err := startSession(); err != nil {
		return workflow.RLMResultV1{}, err
	}
	defer func() {
		if session != nil {
			session.Close()
		}
	}()

	buildEnv := func() []string {
		return rlm.BuildEnv(params.token, params.endpoint, rlm.SessionState{
			ID:    sessionID,
			Index: sessionIndex,
		})
	}

	var systemPrompt string
	if params.systemOverride && strings.TrimSpace(params.system) != "" {
		// Full override: use provided system prompt as-is
		systemPrompt = params.system
	} else {
		// Default: prepend custom instructions to the default RLM system prompt
		systemPrompt = rlm.BuildSystemPrompt(rlm.SystemPromptOptions{
			MaxDepth:     params.maxDepth,
			MaxSubcalls:  params.maxSubcalls,
			CustomPrefix: params.system,
		})
	}
	conversation := []llm.InputItem{
		llm.NewSystemText(systemPrompt),
		llm.NewUserText(params.query),
	}

	for i := 0; i < params.maxIterations; i++ {
		resp, err := callLLM(ctx, params.client, params.model, conversation, params.toolChoice)
		if err != nil {
			return workflow.RLMResultV1{}, err
		}
		if params.usage != nil {
			params.usage.add(resp.Usage)
		}

		code := rlm.NormalizeRLMCode(resp.AssistantText())
		if code == "" {
			return workflow.RLMResultV1{}, errors.New("missing response text")
		}

		execResult, execErr := session.RunPython(ctx, params.wrapper+code, buildEnv(), params.execTimeoutMS)
		platformExecResult := sdkExecResultToPlatform(execResult)

		// Use shared iteration processing
		iterOutput := rlm.ProcessIteration(rlm.IterationInput{
			Iteration:  i + 1,
			Code:       code,
			ExecResult: platformExecResult,
			ExecErr:    execErr,
		})
		if iterOutput.ParseErr != nil {
			return workflow.RLMResultV1{}, iterOutput.ParseErr
		}

		recovered := false
		if iterOutput.NeedsRestart {
			if startErr := startSession(); startErr != nil {
				return workflow.RLMResultV1{}, startErr
			}
			recovered = true
		}

		// Build iteration record
		record := rlm.BuildIterationRecord(i+1, code, platformExecResult)
		trajectory = append(trajectory, workflow.RLMIterationV1{
			Iteration: record.Iteration,
			Code:      record.Code,
			Stdout:    record.Stdout,
			Stderr:    record.Stderr,
			ExitCode:  record.ExitCode,
			TimedOut:  record.TimedOut,
		})

		if iterOutput.Done {
			answer = iterOutput.Answer
			break
		}

		conversation = append(conversation, llm.NewAssistantText(code))
		feedback := rlm.BuildFeedbackMessage(platformExecResult, execErr, recovered)
		conversation = append(conversation, llm.NewUserText(feedback))
	}

	if len(answer) == 0 {
		return workflow.RLMResultV1{}, errors.New("max iterations exceeded")
	}

	usage := workflow.TokenUsage{}
	if params.usage != nil {
		usage = params.usage.snapshot()
	}

	return workflow.RLMResultV1{
		Answer:     answer,
		Iterations: len(trajectory),
		Subcalls:   *params.subcallCounter,
		TotalUsage: usage,
		Trajectory: trajectory,
	}, nil
}

func callLLM(ctx context.Context, client *sdk.Client, model string, input []llm.InputItem, toolChoice *llm.ToolChoice) (*sdk.Response, error) {
	builder := client.Responses.New().Model(sdk.NewModelID(model)).Input(input)
	if toolChoice != nil {
		builder = builder.ToolChoice(*toolChoice)
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

func startLocalRLMSession(ctx context.Context, interp *sdkrlm.LocalInterpreter, plan sdkrlm.ContextPlan, payload []byte) (sdkrlm.CodeSession, string, error) {
	session, err := interp.Start(ctx, "rlm-local", nil)
	if err != nil {
		return nil, "", err
	}
	if plan.Mode == sdkrlm.ContextLoadFile {
		if err := session.WriteFile(ctx, plan.ContextPath, payload, 0o644); err != nil {
			session.Close()
			return nil, "", err
		}
	}
	token, err := randomToken()
	if err != nil {
		session.Close()
		return nil, "", err
	}
	return session, token, nil
}

func startLocalRLMSubcallServer(ctx context.Context, client *sdk.Client, defaultModel string, maxDepth, maxSubcalls int, usage *rlmUsage) (endpoint string, token string, subcalls *int, closeFn func(), err error) {
	if maxSubcalls < 0 {
		return "", "", nil, nil, errors.New("max_subcalls must be >= 0")
	}
	token, err = randomToken()
	if err != nil {
		return "", "", nil, nil, err
	}
	counter := 0

	handler := &localSubcallHandler{
		ctx:          ctx,
		client:       client,
		defaultModel: defaultModel,
		maxDepth:     maxDepth,
		maxSubcalls:  maxSubcalls,
		token:        token,
		counter:      &counter,
		usage:        usage,
	}

	mux := http.NewServeMux()
	mux.Handle("/rlm/subcall", handler)
	server := httptest.NewServer(mux)
	return server.URL + "/rlm/subcall", token, &counter, server.Close, nil
}

type localSubcallHandler struct {
	ctx          context.Context
	client       *sdk.Client
	defaultModel string
	maxDepth     int
	maxSubcalls  int
	token        string
	counter      *int
	usage        *rlmUsage
	mu           sync.Mutex
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
		Prompt string  `json:"prompt"`
		Model  *string `json:"model,omitempty"`
		Depth  int     `json:"depth,omitempty"`
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

	model := h.defaultModel
	if req.Model != nil && strings.TrimSpace(*req.Model) != "" {
		model = strings.TrimSpace(*req.Model)
	}
	if model == "" {
		http.Error(w, "model required", http.StatusBadRequest)
		return
	}

	resp, err := callLLM(h.ctx, h.client, model, []llm.InputItem{llm.NewUserText(req.Prompt)}, nil)
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

func parseToolChoice(s string) (*llm.ToolChoice, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return nil, nil
	case "auto":
		return &llm.ToolChoice{Type: llm.ToolChoiceAuto}, nil
	case "required":
		return &llm.ToolChoice{Type: llm.ToolChoiceRequired}, nil
	case "none":
		return &llm.ToolChoice{Type: llm.ToolChoiceNone}, nil
	default:
		return nil, fmt.Errorf("invalid tool-choice: %q (must be auto, required, or none)", s)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Headers already written; log the error since we can't change the response
		log.Printf("writeJSON encode error: %v", err)
	}
}
