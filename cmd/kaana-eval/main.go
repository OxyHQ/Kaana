// Command kaana-eval measures, from the caller's side, how each model line
// answers through Kaana's public API: time to first output, total time, output
// tokens and throughput, over repeated identical requests.
//
// It is an operator's tool and is not part of the served image. It calls
// POST /v1/chat/completions with the credential in KAANA_API_KEY, so every run
// is billed to that credential's account like any other request: the operator
// chooses the models, the number of runs and the output cap, and nothing runs
// until they start it. It writes figures only (never a prompt or a completion)
// and changes no routing: the signed authorizedRoutes lane is Oxy's to order.
//
// A latency reading is a sample of one provider on one day. Two runs of the same
// request can differ for reasons the caller cannot see (docs/determinism.md), so
// compare percentiles over many runs, never single results.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

const defaultPrompt = "Count from 1 to 40, one number per line, and write nothing else."

// sample is one request's measurement.
type sample struct {
	TTFT         time.Duration
	Total        time.Duration
	OutputTokens int
	Err          string
}

// summary is one model's report.
type summary struct {
	Model        string  `json:"model"`
	Runs         int     `json:"runs"`
	Failures     int     `json:"failures"`
	FirstError   string  `json:"firstError,omitempty"`
	TTFTp50Ms    float64 `json:"ttftP50Ms"`
	TTFTp95Ms    float64 `json:"ttftP95Ms"`
	TotalP50Ms   float64 `json:"totalP50Ms"`
	TotalP95Ms   float64 `json:"totalP95Ms"`
	OutputTokens int     `json:"outputTokensMedian"`
	TokensPerSec float64 `json:"tokensPerSecondMedian"`
}

type config struct {
	baseURL   string
	key       string
	prompt    string
	maxTokens int
	runs      int
	client    *http.Client
}

// measure sends one streamed chat completion and times it.
func measure(ctx context.Context, cfg config, model string) sample {
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": cfg.maxTokens,
		"messages": []any{map[string]any{"role": "user", "content": cfg.prompt}},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return sample{Err: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+cfg.key)
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := cfg.client.Do(request)
	if err != nil {
		return sample{Err: "request failed: " + redact(err.Error(), cfg.key)}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return sample{Err: fmt.Sprintf("HTTP %d %s", response.StatusCode, redact(string(message), cfg.key))}
	}
	var out sample
	reader := bufio.NewReader(response.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:"); ok {
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				out.Total = time.Since(started)
				return out
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content *string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				if chunk.Error != nil {
					return sample{Err: "stream error: " + redact(chunk.Error.Message, cfg.key)}
				}
				for _, choice := range chunk.Choices {
					if out.TTFT == 0 && choice.Delta.Content != nil && *choice.Delta.Content != "" {
						out.TTFT = time.Since(started)
					}
				}
				if chunk.Usage != nil {
					out.OutputTokens = chunk.Usage.CompletionTokens
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return sample{Err: "stream ended without [DONE]"}
			}
			return sample{Err: "read failed: " + redact(readErr.Error(), cfg.key)}
		}
	}
}

func redact(text, key string) string {
	if key == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1)*p + 0.5)
	return sorted[index]
}

// summarize reduces a model's samples to the figures reported. A failed run
// counts as a failure and contributes no latency: it is never averaged in as a
// fast answer.
func summarize(model string, samples []sample) summary {
	report := summary{Model: model, Runs: len(samples)}
	var ttft, total, tokens, rate []float64
	for _, s := range samples {
		if s.Err != "" {
			report.Failures++
			if report.FirstError == "" {
				report.FirstError = s.Err
			}
			continue
		}
		ttft = append(ttft, float64(s.TTFT.Microseconds())/1000)
		total = append(total, float64(s.Total.Microseconds())/1000)
		tokens = append(tokens, float64(s.OutputTokens))
		if generating := s.Total - s.TTFT; generating > 0 && s.OutputTokens > 0 {
			rate = append(rate, float64(s.OutputTokens)/generating.Seconds())
		}
	}
	for _, values := range [][]float64{ttft, total, tokens, rate} {
		slices.Sort(values)
	}
	report.TTFTp50Ms, report.TTFTp95Ms = percentile(ttft, 0.5), percentile(ttft, 0.95)
	report.TotalP50Ms, report.TotalP95Ms = percentile(total, 0.5), percentile(total, 0.95)
	report.OutputTokens = int(percentile(tokens, 0.5))
	report.TokensPerSec = percentile(rate, 0.5)
	return report
}

func run(ctx context.Context, cfg config, models []string, out io.Writer) error {
	encoder := json.NewEncoder(out)
	for _, model := range models {
		samples := make([]sample, 0, cfg.runs)
		for range cfg.runs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			samples = append(samples, measure(ctx, cfg, model))
		}
		if err := encoder.Encode(summarize(model, samples)); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	baseURL := flag.String("base-url", "https://kaana.ai/v1", "Kaana's public API base URL")
	runs := flag.Int("runs", 5, "requests per model")
	maxTokens := flag.Int("max-tokens", 128, "output cap per request (bounds the spend)")
	prompt := flag.String("prompt", defaultPrompt, "the request every run sends")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "usage: KAANA_API_KEY=oxy_sk_... kaana-eval [flags] publisher/model [publisher/model ...]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	key := os.Getenv("KAANA_API_KEY")
	if key == "" || flag.NArg() == 0 || *runs < 1 || *maxTokens < 1 {
		flag.Usage()
		os.Exit(2)
	}
	cfg := config{baseURL: *baseURL, key: key, prompt: *prompt, maxTokens: *maxTokens, runs: *runs, client: &http.Client{Timeout: 3 * time.Minute}}
	if err := run(context.Background(), cfg, flag.Args(), os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
