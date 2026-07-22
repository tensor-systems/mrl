package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelrelay/modelrelay/sdk/go"
	"github.com/modelrelay/modelrelay/sdk/go/llm"
	"github.com/spf13/cobra"
)

type bashToolArgs struct {
	Command string `json:"command" description:"Shell command to execute"`
}

func newDoCmd() *cobra.Command {
	var model string
	var system string
	var allowAll bool
	var allow []string
	var maxTurns int
	var trace bool

	cmd := &cobra.Command{
		Use:   "do <task>",
		Short: "Execute a task using AI with bash tools",
		Long: `Execute a task using AI that can run bash commands.

Examples:
  mrl do "commit my changes"
  mrl do "list all TODO comments in this repo"
  mrl do "run tests and fix any failures" --allow-all
  mrl do "show git status" --allow "git "

By default, no commands are allowed. Use --allow to whitelist
command prefixes, or --allow-all to permit any command.

Permissions can also be set in config:
  mrl config set --allow-all
  mrl config set --allow "git " --allow "npm "`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDo(cmd, args, model, system, allow, allowAll, maxTurns, trace)
		},
	}

	cmd.Flags().StringVar(&model, "model", "", "Model ID (overrides profile default)")
	cmd.Flags().StringVar(&system, "system", "", "System prompt")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "Allow bash command prefix (repeatable)")
	cmd.Flags().BoolVar(&allowAll, "allow-all", false, "Allow all bash commands (use with care)")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 50, "Max tool loop turns")
	cmd.Flags().BoolVar(&trace, "trace", false, "Print tool calls as they execute")

	return cmd
}

func runDo(cmd *cobra.Command, args []string, modelFlag, system string, allowFlag []string, allowAllFlag bool, maxTurns int, traceFlag bool) error {
	cfg, err := runtimeConfigFrom(cmd)
	if err != nil {
		return err
	}

	model := resolveModel(modelFlag, cfg)
	if model == "" {
		return errors.New("model is required (set via --model, MODELRELAY_MODEL, or mrl config set --model)")
	}

	// Merge CLI flags with config (CLI takes precedence)
	allowAll := allowAllFlag || cfg.AllowAll
	allow := allowFlag
	if len(allow) == 0 {
		allow = cfg.Allow
	}
	trace := traceFlag || cfg.Trace

	if !allowAll && len(allow) == 0 {
		return errors.New("bash permissions required: use --allow <prefix>, --allow-all, or set allow_all in config")
	}

	client, err := newPromptClient(cfg)
	if err != nil {
		return err
	}

	prompt := strings.Join(args, " ")

	ctx, cancel := contextWithTimeout(cfg.Timeout)
	defer cancel()

	return runDoLoop(ctx, client, model, system, prompt, allow, allowAll, maxTurns, trace)
}

func runDoLoop(ctx context.Context, client *sdk.Client, model, system, prompt string, allow []string, allowAll bool, maxTurns int, trace bool) error {
	// Build bash tool options
	bashOpts := []sdk.LocalBashOption{
		sdk.WithLocalBashTimeout(30 * time.Second),
		sdk.WithLocalBashMaxOutputBytes(64_000),
		sdk.WithLocalBashInheritEnv(),
	}
	if allowAll {
		bashOpts = append(bashOpts, sdk.WithLocalBashAllowAllCommands())
	}
	if len(allow) > 0 {
		rules := make([]sdk.BashCommandRule, len(allow))
		for i, prefix := range allow {
			rules[i] = sdk.BashCommandPrefix(prefix)
		}
		bashOpts = append(bashOpts, sdk.WithLocalBashAllowRules(rules...))
	}

	// Create tool registry and definitions
	registry := sdk.NewToolRegistry()
	sdk.NewLocalBashToolPack(".", bashOpts...).RegisterInto(registry)

	bashTool := sdk.MustFunctionToolFromType[bashToolArgs](sdk.ToolNameBash, "Execute a shell command")
	tools := []llm.Tool{bashTool}

	// Build initial messages
	var messages []llm.InputItem
	sysPrompt := system
	if sysPrompt == "" {
		sysPrompt = `You are an agent that completes tasks by executing shell commands. Use the bash tool to run commands. Do not explain how to do things - actually do them. Be concise - when done, just say what you did in one short sentence.

When writing git commits:
- Write clear, descriptive commit messages that explain what changed and why
- Use conventional commit format when appropriate (feat:, fix:, docs:, refactor:, etc.)
- Look at the actual diff to understand what changed before writing the message`
	}
	messages = append(messages, llm.NewSystemText(sysPrompt), llm.NewUserText(prompt))

	modelID := sdk.NewModelID(model)
	var usage sdk.AgentUsage

	for range maxTurns {
		req, callOpts, err := client.Responses.New().
			Model(modelID).
			Input(messages).
			Tools(tools).
			Build()
		if err != nil {
			return err
		}

		resp, err := client.Responses.Create(ctx, req, callOpts...)
		if err != nil {
			return err
		}

		usage.LLMCalls++
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens
		usage.TotalTokens += resp.Usage.TotalTokens
		usage.ReasoningTokens += resp.Usage.ReasoningTokens
		usage.CacheReadInputTokens += resp.Usage.CacheReadInputTokens
		usage.CacheWriteInputTokens += resp.Usage.CacheWriteInputTokens

		toolCalls := resp.ToolCalls()
		if len(toolCalls) == 0 {
			// Done - print final response and exit
			if text := resp.AssistantText(); text != "" {
				fmt.Println(text)
			}
			return nil
		}

		usage.ToolCalls += len(toolCalls)

		// Add assistant message with tool calls
		messages = append(messages, sdk.AssistantMessageWithToolCalls(resp.AssistantText(), toolCalls))

		// Print tool calls before execution
		if trace {
			for _, tc := range toolCalls {
				if tc.Function != nil {
					var args bashToolArgs
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil && args.Command != "" {
						fmt.Printf("\033[1;36m→ %s\033[0m\n", args.Command)
					} else {
						fmt.Printf("\033[1;36m→ %s %s\033[0m\n", tc.Function.Name, tc.Function.Arguments)
					}
				}
			}
		}

		// Execute tools and add results
		results := registry.ExecuteAll(toolCalls)
		messages = append(messages, registry.ResultsToMessages(results)...)

		// Print tool execution output
		for _, result := range results {
			if result.Result != nil {
				switch r := result.Result.(type) {
				case sdk.BashResult:
					if r.Output != "" {
						fmt.Printf("\033[2m%s\033[0m\n", r.Output)
					}
					if r.Error != "" {
						fmt.Printf("\033[31merror: %s\033[0m\n", r.Error)
					}
				case string:
					if r != "" {
						fmt.Println(r)
					}
				}
			}
			if result.Error != nil {
				fmt.Printf("\033[31merror: %s\033[0m\n", result.Error)
			}
		}
	}

	return fmt.Errorf("max turns (%d) reached without completion", maxTurns)
}
