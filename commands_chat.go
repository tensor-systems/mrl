package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	sdk "github.com/modelrelay/modelrelay/sdk/go"
	"github.com/modelrelay/modelrelay/sdk/go/llm"
	"github.com/spf13/cobra"
)

// runPrompt is the default action when mrl is invoked with a prompt.
func runPrompt(cmd *cobra.Command, args []string, modelFlag, system string, attachments []string, attachmentType string, attachStdin bool, stream, showUsage bool) error {
	cfg, err := runtimeConfigFrom(cmd)
	if err != nil {
		return err
	}

	model := resolveModel(modelFlag, cfg)
	if model == "" {
		return errors.New("model is required (set via --model, MODELRELAY_MODEL, or mrl config set --model)")
	}

	client, err := newPromptClient(cfg)
	if err != nil {
		return err
	}

	prompt := strings.Join(args, " ")
	stdinIsTTY, err := isTerminal(os.Stdin)
	if err != nil {
		return err
	}

	// Auto-read stdin as text when piped and no explicit attachment flags are set.
	// This enables: cat file.md | mrl "question about the content"
	var stdinText string
	useStdinAsText := !stdinIsTTY && len(attachments) == 0 && attachmentType == "" && !attachStdin
	if useStdinAsText {
		data, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return fmt.Errorf("failed to read stdin: %w", readErr)
		}
		stdinText = string(data)
	}

	var attachmentParts []llm.ContentPart
	if !useStdinAsText {
		resolvedAttachments, resolveErr := resolveAttachmentInputs(attachments, attachmentType, attachStdin, stdinIsTTY)
		if resolveErr != nil {
			return resolveErr
		}
		attachmentParts, err = buildAttachmentParts(resolvedAttachments, attachmentType, os.Stdin)
		if err != nil {
			return err
		}
	}

	// Build the final prompt: stdin text (if any) + args prompt
	var finalPrompt string
	switch {
	case stdinText != "" && strings.TrimSpace(prompt) != "":
		finalPrompt = stdinText + "\n\n" + prompt
	case stdinText != "":
		finalPrompt = stdinText
	default:
		finalPrompt = prompt
	}

	userParts := make([]llm.ContentPart, 0, 1+len(attachmentParts))
	if strings.TrimSpace(finalPrompt) != "" {
		userParts = append(userParts, llm.TextPart(finalPrompt))
	}
	userParts = append(userParts, attachmentParts...)
	if len(userParts) == 0 {
		return errors.New("prompt or attachment required")
	}

	ctx, cancel := contextWithTimeout(cfg.Timeout)
	defer cancel()

	if stream {
		return runStreamWithUsage(ctx, client, model, system, userParts, showUsage)
	}

	builder := client.Responses.New().Model(sdk.NewModelID(model))
	if system != "" {
		builder = builder.System(system)
	}
	builder = builder.Item(llm.InputItem{
		Type:    llm.InputItemTypeMessage,
		Role:    llm.RoleUser,
		Content: userParts,
	})

	start := time.Now()
	req, callOpts, err := builder.Build()
	if err != nil {
		return err
	}
	resp, err := client.Responses.Create(ctx, req, callOpts...)
	if err != nil {
		return err
	}
	latency := time.Since(start)

	fmt.Println(resp.AssistantText())
	if showUsage {
		fmt.Printf("\nModel: %s | Tokens: %d in / %d out | Latency: %s\n",
			resp.Model,
			resp.Usage.InputTokens,
			resp.Usage.OutputTokens,
			latency.Round(time.Millisecond),
		)
	}
	return nil
}

func isTerminal(file *os.File) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	return (info.Mode() & os.ModeCharDevice) != 0, nil
}

func runStreamWithUsage(ctx context.Context, client *sdk.Client, model, system string, userParts []llm.ContentPart, showUsage bool) error {
	builder := client.Responses.New().Model(sdk.NewModelID(model))
	if system != "" {
		builder = builder.System(system)
	}
	builder = builder.Item(llm.InputItem{
		Type:    llm.InputItemTypeMessage,
		Role:    llm.RoleUser,
		Content: userParts,
	})

	req, opts, err := builder.Build()
	if err != nil {
		return err
	}

	start := time.Now()
	stream, err := client.Responses.Stream(ctx, req, opts...)
	if err != nil {
		return err
	}
	defer stream.Close()

	var finalModel sdk.ModelID
	var finalUsage *sdk.Usage
	var ttft time.Duration
	var sawFirstToken bool

	for {
		ev, ok, err := stream.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if ev.TextDelta != "" {
			if !sawFirstToken {
				ttft = time.Since(start)
				sawFirstToken = true
			}
			fmt.Print(ev.TextDelta)
		}
		if !ev.Model.IsEmpty() {
			finalModel = ev.Model
		}
		if ev.Usage != nil {
			finalUsage = ev.Usage
		}
	}
	totalDuration := time.Since(start)
	fmt.Println()

	if showUsage && finalUsage != nil {
		fmt.Printf("\nModel: %s | Tokens: %d in / %d out | TTFT: %s | Total: %s\n",
			finalModel,
			finalUsage.InputTokens,
			finalUsage.OutputTokens,
			ttft.Round(time.Millisecond),
			totalDuration.Round(time.Millisecond),
		)
	}
	return nil
}

func newPromptClient(cfg runtimeConfig) (*sdk.Client, error) {
	opts := []sdk.Option{sdk.WithClientHeader(clientHeader())}
	if strings.TrimSpace(cfg.BaseURL) != "" {
		opts = append(opts, sdk.WithBaseURL(cfg.BaseURL))
	}
	// Honor --timeout for individual requests too: the SDK's 60s per-request
	// default otherwise aborts long completions (e.g. large-context prompts
	// with thinking) even when the session timeout is much higher.
	if cfg.Timeout > 0 {
		opts = append(opts, sdk.WithRequestTimeout(cfg.Timeout))
	}

	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("api key required")
	}
	key, err := sdk.ParseAPIKeyAuth(cfg.APIKey)
	if err != nil {
		return nil, err
	}
	return sdk.NewClientWithKey(key, opts...)
}

func resolveModel(flagValue string, cfg runtimeConfig) string {
	if strings.TrimSpace(flagValue) != "" {
		return strings.TrimSpace(flagValue)
	}
	return cfg.Model
}
