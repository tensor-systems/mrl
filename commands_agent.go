package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/modelrelay/modelrelay/sdk/go"
	"github.com/modelrelay/modelrelay/sdk/go/llm"
	"github.com/spf13/cobra"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Agent tools",
	}
	cmd.AddCommand(newAgentLoopCmd())
	return cmd
}

func newAgentClient(cfg runtimeConfig) (*sdk.Client, error) {
	opts := []sdk.Option{sdk.WithClientHeader(clientHeader())}
	if strings.TrimSpace(cfg.BaseURL) != "" {
		opts = append(opts, sdk.WithBaseURL(cfg.BaseURL))
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

func resolveInput(inputText, inputFile string, tail []string) ([]llm.InputItem, error) {
	if inputFile != "" {
		return readInputFile(inputFile)
	}
	text := strings.TrimSpace(inputText)
	if text == "" && len(tail) > 0 {
		text = strings.TrimSpace(strings.Join(tail, " "))
	}
	if text == "" {
		return nil, errors.New("input is required (use --input, --input-file, or trailing args)")
	}
	return []llm.InputItem{llm.NewUserText(text)}, nil
}

func readInputFile(path string) ([]llm.InputItem, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // input path is explicitly selected by the CLI user
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("input file is empty")
	}

	var items []llm.InputItem
	if err := json.Unmarshal(raw, &items); err == nil && len(items) > 0 {
		return items, nil
	}

	var wrapper struct {
		Input []llm.InputItem `json:"input"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Input) > 0 {
		return wrapper.Input, nil
	}

	return nil, errors.New("input file must be a JSON array of input items or an object with {input: [...]}")
}
