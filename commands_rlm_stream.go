package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelrelay/modelrelay/platform/rlm"
	"github.com/modelrelay/modelrelay/platform/rlmrunner"
	sdk "github.com/modelrelay/modelrelay/sdk/go"
)

const (
	rlmStreamMediaType     = rlm.RunnerStreamContentType
	rlmStreamMaxErrorBytes = 1024 * 1024
)

func validateRLMStreamMode(cfg runtimeConfig, flags *rlmFlags) error {
	if !flags.stream {
		return nil
	}
	if flags.relaySession {
		return errors.New("--stream cannot be used with --relay-session")
	}
	if !flags.remote {
		return errors.New("--stream requires --remote")
	}
	if cfg.Output == outputFormatJSON {
		return errors.New("--stream cannot be used with --json or a JSON output profile")
	}
	return nil
}

func executeRLMRemoteStream(
	ctx context.Context,
	httpClient *http.Client,
	baseURL string,
	apiKey sdk.APIKeyAuth,
	payload rlmExecuteRemoteRequest,
	stdout io.Writer,
) error {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return errors.New("base URL is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode rlm execute request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/rlm/execute", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build rlm execute stream request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", rlmStreamMediaType)
	if apiKey != nil && strings.TrimSpace(apiKey.String()) != "" {
		request.Header.Set("X-ModelRelay-Api-Key", apiKey.String())
	}
	if header := strings.TrimSpace(clientHeader()); header != "" {
		request.Header.Set("X-ModelRelay-Client", header)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, readErr := readRLMStreamError(response.Body)
		if readErr != nil {
			return fmt.Errorf("read rlm execute error response: %w", readErr)
		}
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("rlm execute failed (%d): %s", response.StatusCode, message)
	}
	if err := validateRLMStreamContentType(response.Header.Get("Content-Type")); err != nil {
		return err
	}
	if stdout == nil {
		return errors.New("rlm stream stdout is required")
	}
	return copyAndValidateRLMStream(stdout, response.Body)
}

func validateRLMStreamContentType(value string) error {
	if !rlm.IsRunnerStreamContentType(value) {
		return fmt.Errorf("rlm execute stream returned unsupported Content-Type %q", value)
	}
	return nil
}

func readRLMStreamError(reader io.Reader) (string, error) {
	limited := &io.LimitedReader{R: reader, N: rlmStreamMaxErrorBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if len(body) > rlmStreamMaxErrorBytes {
		return "", errors.New("rlm execute error response exceeded limit")
	}
	return strings.TrimSpace(string(body)), nil
}

func copyAndValidateRLMStream(dst io.Writer, src io.Reader) error {
	decoder := rlm.NewRunnerEventDecoder()
	terminal := rlmrunner.StreamTerminalState{}
	reader := bufio.NewReaderSize(src, rlm.DefaultRunnerEventFrameBytes+1)
	for {
		line, readErr := reader.ReadSlice('\n')
		if len(line) > 0 {
			written, writeErr := dst.Write(line)
			if writeErr != nil {
				return fmt.Errorf("write rlm stream: %w", writeErr)
			}
			if written != len(line) {
				return fmt.Errorf("write rlm stream: %w", io.ErrShortWrite)
			}
		}

		switch {
		case readErr == nil:
			frame, err := decoder.Accept(line[:len(line)-1])
			if err != nil {
				return err
			}
			next, err := terminal.Accept(frame)
			if err != nil {
				return err
			}
			terminal = next
		case errors.Is(readErr, bufio.ErrBufferFull):
			return &rlm.EventTransportError{Kind: rlm.EventTransportOversized, Frame: terminal.Frames() + 1}
		case errors.Is(readErr, io.EOF) && len(line) == 0:
			summary, err := decoder.Summary()
			if err != nil {
				return err
			}
			return terminal.ValidateComplete(summary)
		case errors.Is(readErr, io.EOF):
			return &rlm.EventTransportError{Kind: rlm.EventTransportTruncated, Frame: terminal.Frames() + 1}
		default:
			return fmt.Errorf("read rlm stream: %w", readErr)
		}
	}
}
