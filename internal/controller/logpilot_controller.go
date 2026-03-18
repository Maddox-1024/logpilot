/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	openai "github.com/openai/openai-go"
	openAIOption "github.com/openai/openai-go/option"
	googleOption "google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	logv1 "llm-log-operator/api/v1"
)

const (
	maxLogEntries     = 2000
	maxLogLineBytes   = 2048
	maxClusterSamples = 3
)

type retryCategory string

const (
	retryCategoryTransient retryCategory = "transient"
	retryCategorySecret    retryCategory = "secret"
	retryCategoryPermanent retryCategory = "permanent"
)

type httpStatusError struct {
	Service string
	Status  int
	Body    string
}

func (e *httpStatusError) Error() string {
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("%s returned status %d", e.Service, e.Status)
	}
	return fmt.Sprintf("%s returned status %d: %s", e.Service, e.Status, e.Body)
}

// LogPilotReconciler reconciles a LogPilot object
type LogPilotReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups=log.aiops.com,resources=logpilots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=log.aiops.com,resources=logpilots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=log.aiops.com,resources=logpilots/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the LogPilot object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *LogPilotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	var logPilot logv1.LogPilot
	if err := r.Get(ctx, req.NamespacedName, &logPilot); err != nil {
		logger.Error(err, "unable to fetch LogPilot", "name", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval, err := time.ParseDuration(logPilot.Spec.Interval)
	if err != nil {
		logger.Error(err, "unable to parse interval", "interval", logPilot.Spec.Interval)
		// Use default interval of 1 minute if parsing fails.
		interval = time.Minute
	}
	if interval <= 0 {
		interval = time.Minute
	}

	lastAttemptTime := logPilot.Status.LastAttemptTime.Time
	if !lastAttemptTime.IsZero() {
		elapsed := time.Since(lastAttemptTime)
		if elapsed < interval {
			return ctrl.Result{RequeueAfter: interval - elapsed}, nil
		}
	}

	apiKey, err := r.getSecretValue(ctx, req.Namespace, logPilot.Spec.LLMAPIKeySecret, logPilot.Spec.LLMAPIKeySecretKey)
	if err != nil {
		logger.Error(err, "unable to get LLM API key from secret")
		if statusErr := r.updateStatusError(ctx, &logPilot, fmt.Sprintf("Secret error: %v", err)); statusErr != nil {
			logger.Error(statusErr, "failed to update status for secret retrieval error")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterForCategory(interval, retryCategorySecret)}, nil
	}

	webhook, err := r.getSecretValue(ctx, req.Namespace, logPilot.Spec.WebhookSecret, logPilot.Spec.WebhookSecretKey)
	if err != nil {
		logger.Error(err, "unable to get webhook URL from secret")
		if statusErr := r.updateStatusError(ctx, &logPilot, fmt.Sprintf("Secret error: %v", err)); statusErr != nil {
			logger.Error(statusErr, "failed to update status for secret retrieval error")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterForCategory(interval, retryCategorySecret)}, nil
	}
	// Loki query window:
	// - normal case: query from last successful check time to now
	// - first run fallback: query the last "interval"
	lokiQuery := logPilot.Spec.LogQL
	end := time.Now()
	start := logPilot.Status.LastSuccessTime.Time
	if start.IsZero() {
		start = end.Add(-interval)
	}
	// Guard against clock skew or invalid status time.
	if start.After(end) {
		start = end.Add(-interval)
	}

	endTime := end.UnixNano()
	startTime := start.UnixNano()
	logger.Info("query window", "startTime", startTime, "endTime", endTime)

	queryParams := url.Values{}
	queryParams.Set("query", lokiQuery)
	queryParams.Set("start", strconv.FormatInt(startTime, 10))
	queryParams.Set("end", strconv.FormatInt(endTime, 10))
	queryParams.Set("limit", strconv.Itoa(maxLogEntries))
	lokiURL := fmt.Sprintf("%s/loki/api/v1/query_range?%s", logPilot.Spec.LokiURL, queryParams.Encode())
	lokiCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Mark attempt time before making external calls to avoid duplicate inflight work.
	if statusErr := r.updateStatusAttempt(ctx, &logPilot); statusErr != nil {
		logger.Error(statusErr, "failed to update status for attempt time")
		return ctrl.Result{}, statusErr
	}

	lokiEntries, err := r.queryLoki(lokiCtx, lokiURL)
	if err != nil {
		logger.Error(err, "unable to query Loki")
		if statusErr := r.updateStatusError(ctx, &logPilot, fmt.Sprintf("Loki error: %v", err)); statusErr != nil {
			logger.Error(statusErr, "failed to update status for loki query error")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterForCategory(interval, classifyLokiRetry(err))}, nil
	}

	// If there are log results, call LLM for analysis
	if len(lokiEntries) > 0 {
		logger.Info("send logs to llm")
		trimmed := trimEntries(lokiEntries, maxLogEntries, maxLogLineBytes)
		enriched := enrichEntries(trimmed)
		errEntries := filterSeverities(enriched, "error", "critical")
		patternsAll := detectPatterns(enriched)
		if len(errEntries) == 0 {
			if statusErr := r.updateStatusSuccess(ctx, &logPilot, "No error/critical logs found", end, ""); statusErr != nil {
				logger.Error(statusErr, "failed to update status for no error/critical logs")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: interval}, nil
		}
		clusters := clusterEntries(errEntries)
		patternsErr := detectPatterns(errEntries)
		summary := buildSummary(errEntries, clusters, patternsErr)
		analysisText := "Deterministic summary:\n" + summary

		llmCtx, llmCancel := context.WithTimeout(ctx, 30*time.Second)
		defer llmCancel()

		analysisResult, err := r.analyzeLogsWithLLM(llmCtx, logPilot.Spec, apiKey, summary)
		if err != nil {
			logger.Error(err, "unable to analyze logs with LLM")
			if statusErr := r.updateStatusError(ctx, &logPilot, fmt.Sprintf("LLM analysis error: %v", err)); statusErr != nil {
				logger.Error(statusErr, "failed to update status for llm analysis error")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: requeueAfterForCategory(interval, classifyLLMRetry(err))}, nil
		}
		if analysisResult != nil && strings.TrimSpace(analysisResult.Analysis) != "" {
			analysisText = analysisText + "\n\nLLM reasoning:\n" + analysisResult.Analysis
		}

		hasError := shouldAlert(errEntries, patternsAll)
		var deliveryErr string
		// If the deterministic result indicates there is a problem with the logs, send Feishu alert
		if hasError {
			err := r.sendLarkAlert(ctx, webhook, analysisText)
			if err != nil {
				logger.Error(err, "Failed to send lark alert")
				deliveryErr = fmt.Sprintf("Webhook delivery error: %v", err)
			}
		} else {
			logger.Info("No issues found in logs according to deterministic analysis")
		}
		// Update status with analysis result
		if statusErr := r.updateStatusSuccess(ctx, &logPilot, analysisText, end, deliveryErr); statusErr != nil {
			logger.Error(statusErr, "failed to update status for successful analysis")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	} else {
		if statusErr := r.updateStatusSuccess(ctx, &logPilot, "No logs found", end, ""); statusErr != nil {
			logger.Error(statusErr, "failed to update status for empty log result")
			return ctrl.Result{}, statusErr
		}
	}

	// Reconcile again after 10 seconds
	return ctrl.Result{RequeueAfter: interval}, nil
}

type LokiResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// Loki may return errorType for non-success responses.
	ErrorType string `json:"errorType,omitempty"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

type LogEntry struct {
	Timestamp time.Time
	Line      string
	Labels    map[string]string
	Severity  string
}

// queryLoki retrieves logs from Loki
func (r *LogPilotReconciler) queryLoki(ctx context.Context, lokiURL string) ([]LogEntry, error) {
	client := r.httpClient()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lokiURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, &httpStatusError{Service: "loki", Status: resp.StatusCode, Body: string(body)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var lokiResp LokiResponse
	if err := json.Unmarshal(body, &lokiResp); err != nil {
		return nil, err
	}
	if lokiResp.Status != "success" {
		return nil, fmt.Errorf("loki returned status=%s errorType=%s error=%s", lokiResp.Status, lokiResp.ErrorType, lokiResp.Error)
	}

	if len(lokiResp.Data.Result) == 0 {
		return nil, nil // No logs found
	}

	var entries []LogEntry

	for _, stream := range lokiResp.Data.Result {
		for _, entry := range stream.Values {
			if len(entry) == 2 {
				ts, err := parseLokiTime(entry[0])
				if err != nil {
					ts = time.Now()
				}
				entries = append(entries, LogEntry{
					Timestamp: ts,
					Line:      entry[1],
					Labels:    stream.Stream,
				})
			}
		}
	}

	return entries, nil
}

// LLMAnalysisResult is used to store the results of LLM analysis
type LLMAnalysisResult struct {
	HasError bool   // Whether there are error logs
	Analysis string // Analysis results returned by LLM
}

func (r *LogPilotReconciler) getSecretValue(ctx context.Context, namespace, name, key string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		return "", err
	}

	value, exists := secret.Data[key]
	if !exists {
		return "", fmt.Errorf("key %s not found in secret %s", key, name)
	}
	return string(value), nil
}

const systemPrompt = `你是一名经验丰富的日志分析专家和系统优化顾问。以下输入是经过自动严重级别判断、聚类去重与根因模式检测后的JSON摘要。
你的任务是：给出环境（根据namespace标签确定是哪个环境）,基于摘要进行原因推断，给出关键结论和简短建议。不要重新判断严重级别。`

// analyzeLogsWithLLM calls the LLM interface to analyze logs
func (r *LogPilotReconciler) analyzeLogsWithLLM(ctx context.Context, spec logv1.LogPilotSpec, token, summary string) (*LLMAnalysisResult, error) {
	switch spec.LLMProvider {
	case "OpenAI":
		return callOpenAI(ctx, spec, token, summary)
	case "Gemini":
		return callGemini(ctx, spec, token, summary)
	default:
		return nil, fmt.Errorf("unsupported LLM provider: %s", spec.LLMProvider)
	}
}

func callOpenAI(ctx context.Context, spec logv1.LogPilotSpec, token, summary string) (*LLMAnalysisResult, error) {

	opts := []openAIOption.RequestOption{
		openAIOption.WithAPIKey(token),
	}

	cfg := spec.OpenAI
	if cfg != nil {
		if cfg.BaseURL != "" {
			opts = append(opts, openAIOption.WithBaseURL(cfg.BaseURL))
		}
	}

	client := openai.NewClient(opts...)

	completion, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: spec.LLMModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(summary),
		},
	})

	if err != nil {
		return nil, fmt.Errorf("openai api error: %w", err)
	}

	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned")
	}

	analysis := completion.Choices[0].Message.Content

	return &LLMAnalysisResult{Analysis: analysis}, nil
}

func callGemini(ctx context.Context, spec logv1.LogPilotSpec, token, summary string) (*LLMAnalysisResult, error) {
	client, err := genai.NewClient(ctx, googleOption.WithAPIKey(token))
	if err != nil {
		return nil, err
	}
	defer client.Close()

	modelClient := client.GenerativeModel(spec.LLMModel)
	prompt := genai.Text(fmt.Sprintf("%s\n%s", systemPrompt, summary))

	resp, err := modelClient.GenerateContent(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var analysis string
	if resp != nil {
		for _, cand := range resp.Candidates {
			if cand.Content != nil {
				for _, part := range cand.Content.Parts {
					if txt, ok := part.(genai.Text); ok {
						analysis += string(txt)
					}
				}
			}
		}
	}

	return &LLMAnalysisResult{Analysis: analysis}, nil
}

// sendFeishuAlert sends Feishu alert
func (r *LogPilotReconciler) sendLarkAlert(ctx context.Context, webhook, analysis string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type LarkMessage struct {
		MsgType string `json:"msg_type"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	message := LarkMessage{
		MsgType: "text",
	}
	message.Content.Text = analysis

	messageBody, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal lark message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewBuffer(messageBody))
	if err != nil {
		return fmt.Errorf("create lark request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	// Send request
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("send lark request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lark webhook failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (r *LogPilotReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (r *LogPilotReconciler) updateStatusAttempt(ctx context.Context, logPilot *logv1.LogPilot) error {
	key := client.ObjectKeyFromObject(logPilot)
	return r.updateStatusWithRetry(ctx, key, func(current *logv1.LogPilot) {
		current.Status.LastAttemptTime = metav1.Now()
	})
}

func (r *LogPilotReconciler) updateStatusSuccess(ctx context.Context, logPilot *logv1.LogPilot, analysis string, queryEnd time.Time, statusErr string) error {
	key := client.ObjectKeyFromObject(logPilot)
	return r.updateStatusWithRetry(ctx, key, func(current *logv1.LogPilot) {
		current.Status.LastAttemptTime = metav1.Now()
		current.Status.LastSuccessTime = metav1.NewTime(queryEnd)
		current.Status.LastAnalysis = analysis
		current.Status.LastError = statusErr
	})
}

func (r *LogPilotReconciler) updateStatusError(ctx context.Context, logPilot *logv1.LogPilot, errMsg string) error {
	key := client.ObjectKeyFromObject(logPilot)
	return r.updateStatusWithRetry(ctx, key, func(current *logv1.LogPilot) {
		current.Status.LastAttemptTime = metav1.Now()
		current.Status.LastError = errMsg
	})
}

func (r *LogPilotReconciler) updateStatusWithRetry(ctx context.Context, key client.ObjectKey, mutate func(*logv1.LogPilot)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var current logv1.LogPilot
		if err := r.Get(ctx, key, &current); err != nil {
			return err
		}
		mutate(&current)
		return r.Status().Update(ctx, &current)
	})
}

func classifyLokiRetry(err error) retryCategory {
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.Status == http.StatusTooManyRequests:
			return retryCategoryTransient
		case statusErr.Status >= 400 && statusErr.Status < 500:
			return retryCategoryPermanent
		default:
			return retryCategoryTransient
		}
	}
	return retryCategoryTransient
}

func classifyLLMRetry(err error) retryCategory {
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		switch {
		case statusErr.Status == http.StatusTooManyRequests:
			return retryCategoryTransient
		case statusErr.Status >= 400 && statusErr.Status < 500:
			return retryCategoryPermanent
		default:
			return retryCategoryTransient
		}
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unsupported llm provider"):
		return retryCategoryPermanent
	case strings.Contains(msg, "invalid api key"),
		strings.Contains(msg, "incorrect api key"),
		strings.Contains(msg, "authentication"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "model_not_found"),
		strings.Contains(msg, "invalid_request_error"),
		strings.Contains(msg, "status 400"),
		strings.Contains(msg, "status 401"),
		strings.Contains(msg, "status 403"),
		strings.Contains(msg, "status 404"):
		return retryCategoryPermanent
	default:
		return retryCategoryTransient
	}
}

func requeueAfterForCategory(interval time.Duration, category retryCategory) time.Duration {
	if interval <= 0 {
		interval = time.Minute
	}

	switch category {
	case retryCategorySecret:
		return maxDuration(interval*5, 5*time.Minute)
	case retryCategoryPermanent:
		return maxDuration(interval*10, 15*time.Minute)
	default:
		return minDuration(maxDuration(interval*2, 30*time.Second), 15*time.Minute)
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func parseLokiTime(ns string) (time.Time, error) {
	// Loki timestamps are nanoseconds since epoch.
	nano, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	// Convert nanoseconds to time.Time.
	return time.Unix(0, nano), nil
}

func trimEntries(entries []LogEntry, maxEntries, maxLineBytes int) []LogEntry {
	// Fast path for empty input.
	if len(entries) == 0 {
		return entries
	}
	// Ensure stable chronological order for trimming and summaries.
	if !isSortedByTime(entries) {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Timestamp.Before(entries[j].Timestamp)
		})
	}
	// Keep only the most recent N entries.
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	// Hard cap each log line length to avoid payload bloat.
	for i := range entries {
		if len(entries[i].Line) > maxLineBytes {
			entries[i].Line = entries[i].Line[:maxLineBytes]
		}
	}
	return entries
}

func isSortedByTime(entries []LogEntry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i].Timestamp.Before(entries[i-1].Timestamp) {
			return false
		}
	}
	return true
}

func enrichEntries(entries []LogEntry) []LogEntry {
	// Attach deterministic severity to each entry.
	out := make([]LogEntry, 0, len(entries))
	for _, e := range entries {
		e.Severity = classifySeverity(e)
		out = append(out, e)
	}
	return out
}

func filterSeverities(entries []LogEntry, severities ...string) []LogEntry {
	if len(entries) == 0 {
		return entries
	}
	if len(severities) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(severities))
	for _, s := range severities {
		allowed[s] = struct{}{}
	}
	out := make([]LogEntry, 0, len(entries))
	for _, e := range entries {
		if _, ok := allowed[e.Severity]; ok {
			out = append(out, e)
		}
	}
	return out
}

func classifySeverity(e LogEntry) string {
	// Normalize content to lower case for matching.
	lower := strings.ToLower(e.Line)
	// Prefer explicit labels if present.
	if lvl, ok := labelValue(e.Labels, "level", "severity", "log_level"); ok {
		switch strings.ToLower(lvl) {
		case "fatal", "panic", "critical", "crit":
			return "critical"
		case "error", "err":
			return "error"
		case "warn", "warning":
			return "warn"
		case "info", "notice":
			return "info"
		case "debug", "trace":
			return "debug"
		}
	}

	// Keyword-based detection for critical conditions.
	if hasAny(lower, "panic", "fatal", "segmentation fault", "oom", "out of memory") {
		return "critical"
	}
	// Keyword-based detection for errors.
	if hasAny(lower, "error", "failed", "exception", "stacktrace", "connection refused", "context deadline exceeded", "timeout") {
		return "error"
	}
	// Keyword-based detection for warnings.
	if hasAny(lower, "warn", "retry", "rate limit", "slow", "throttle") {
		return "warn"
	}
	// HTTP 5xx is treated as error.
	if httpStatusClass(lower) == 5 {
		return "error"
	}
	// HTTP 4xx is treated as warning.
	if httpStatusClass(lower) == 4 {
		return "warn"
	}
	// Default to info when no signal is found.
	return "info"
}

func labelValue(labels map[string]string, keys ...string) (string, bool) {
	// Return the first non-empty label among the provided keys.
	for _, k := range keys {
		if v, ok := labels[k]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func hasAny(s string, terms ...string) bool {
	// Simple substring matching for any provided term.
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func httpStatusClass(line string) int {
	// Extract the first 3-digit HTTP status code if present.
	m := reHTTPStatus.FindStringSubmatch(line)
	if len(m) == 2 {
		switch m[1] {
		case "5":
			return 5
		case "4":
			return 4
		case "3":
			return 3
		case "2":
			return 2
		case "1":
			return 1
		}
	}
	return 0
}

type Cluster struct {
	Key      string            `json:"key"`
	Count    int               `json:"count"`
	Severity string            `json:"severity"`
	Samples  []string          `json:"samples"`
	Labels   map[string]string `json:"labels,omitempty"`
}

func clusterEntries(entries []LogEntry) []Cluster {
	// Group entries by normalized line template.
	buckets := make(map[string]*Cluster, len(entries))
	for _, e := range entries {
		key := normalizeLine(e.Line)
		b, ok := buckets[key]
		if !ok {
			// Create new cluster bucket on first sight.
			b = &Cluster{
				Key:      key,
				Severity: e.Severity,
				Labels:   e.Labels,
			}
			buckets[key] = b
		}
		// Increase cluster occurrence count.
		b.Count++
		// Store a few raw samples for context.
		if len(b.Samples) < maxClusterSamples {
			b.Samples = append(b.Samples, e.Line)
		}
		// Keep the most severe level seen in this cluster.
		if severityRank(e.Severity) > severityRank(b.Severity) {
			b.Severity = e.Severity
		}
	}

	out := make([]Cluster, 0, len(buckets))
	for _, v := range buckets {
		out = append(out, *v)
	}
	// Sort by frequency, then by severity.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return severityRank(out[i].Severity) > severityRank(out[j].Severity)
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func severityRank(s string) int {
	// Numeric ordering for severity comparison.
	switch s {
	case "critical":
		return 4
	case "error":
		return 3
	case "warn":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

var (
	reUUID       = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reHex        = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	reIP         = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	reNum        = regexp.MustCompile(`\b\d+\b`)
	reSpace      = regexp.MustCompile(`\s+`)
	reHTTPStatus = regexp.MustCompile(`\b([1-5])[0-9]{2}\b`)
)

func normalizeLine(line string) string {
	// Normalize casing and trim whitespace first.
	s := strings.ToLower(strings.TrimSpace(line))
	// Replace high-cardinality tokens with placeholders.
	s = reUUID.ReplaceAllString(s, "{uuid}")
	s = reHex.ReplaceAllString(s, "{hex}")
	s = reIP.ReplaceAllString(s, "{ip}")
	s = reNum.ReplaceAllString(s, "{n}")
	// Collapse multiple spaces for stable matching.
	s = reSpace.ReplaceAllString(s, " ")
	return s
}

type PatternHit struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
	Example  string `json:"example,omitempty"`
}

func detectPatterns(entries []LogEntry) []PatternHit {
	// Rules are deterministic pattern matchers with severity.
	type rule struct {
		name     string
		severity string
		match    func(string) bool
	}

	rules := []rule{
		{name: "database connection error", severity: "error", match: func(s string) bool { return hasAny(s, "connection refused", "dial tcp", "db connection", "sql:") }},
		{name: "timeout", severity: "error", match: func(s string) bool { return hasAny(s, "timeout", "context deadline exceeded") }},
		{name: "out of memory", severity: "critical", match: func(s string) bool { return hasAny(s, "oom", "out of memory", "killed process") }},
		{name: "disk full", severity: "error", match: func(s string) bool { return hasAny(s, "no space left on device", "disk full") }},
		{name: "permission denied", severity: "error", match: func(s string) bool { return hasAny(s, "permission denied", "access denied") }},
		{name: "tls error", severity: "error", match: func(s string) bool { return hasAny(s, "tls", "x509", "certificate") }},
		{name: "upstream unavailable", severity: "error", match: func(s string) bool { return hasAny(s, "503", "502", "bad gateway", "service unavailable") }},
	}

	hits := make(map[string]*PatternHit, len(rules))
	for _, e := range entries {
		lower := strings.ToLower(e.Line)
		for _, r := range rules {
			if r.match(lower) {
				// Record the first example and count occurrences.
				h, ok := hits[r.name]
				if !ok {
					h = &PatternHit{Name: r.name, Severity: r.severity, Example: e.Line}
					hits[r.name] = h
				}
				h.Count++
			}
		}
	}
	out := make([]PatternHit, 0, len(hits))
	for _, v := range hits {
		out = append(out, *v)
	}
	// Sort by severity then count to surface most important issues first.
	sort.Slice(out, func(i, j int) bool {
		if severityRank(out[i].Severity) == severityRank(out[j].Severity) {
			return out[i].Count > out[j].Count
		}
		return severityRank(out[i].Severity) > severityRank(out[j].Severity)
	})
	return out
}

type SummaryPayload struct {
	Window struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"window"`
	SeverityCount map[string]int `json:"severityCount"`
	TopLabels     map[string]int `json:"topLabels"`
	Clusters      []Cluster      `json:"clusters"`
	Patterns      []PatternHit   `json:"patterns"`
}

func buildSummary(entries []LogEntry, clusters []Cluster, patterns []PatternHit) string {
	// Assemble a compact JSON payload for LLM reasoning.
	payload := SummaryPayload{
		SeverityCount: make(map[string]int),
		TopLabels:     make(map[string]int),
		Clusters:      limitClusters(clusters, 20),
		Patterns:      limitPatterns(patterns, 20),
	}
	if len(entries) > 0 {
		// Window boundaries reflect the trimmed, sorted entries.
		payload.Window.Start = entries[0].Timestamp.Format(time.RFC3339)
		payload.Window.End = entries[len(entries)-1].Timestamp.Format(time.RFC3339)
	}
	for _, e := range entries {
		// Count severities.
		payload.SeverityCount[e.Severity]++
		// Track namespace distribution.
		if ns, ok := labelValue(e.Labels, "namespace", "k8s_namespace", "kubernetes_namespace"); ok {
			payload.TopLabels["namespace:"+ns]++
		}
		// Track app distribution.
		if app, ok := labelValue(e.Labels, "app", "app_kubernetes_io_name", "app_kubernetes_io_instance", "k8s_app"); ok {
			payload.TopLabels["app:"+app]++
		}
	}
	// Serialize for LLM consumption.
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func limitClusters(in []Cluster, max int) []Cluster {
	// Defensive bounds for summary size.
	if len(in) <= max {
		return in
	}
	return in[:max]
}

func limitPatterns(in []PatternHit, max int) []PatternHit {
	// Defensive bounds for summary size.
	if len(in) <= max {
		return in
	}
	return in[:max]
}

func shouldAlert(entries []LogEntry, patterns []PatternHit) bool {
	// Alert if any log entry is error/critical.
	for _, e := range entries {
		if e.Severity == "critical" || e.Severity == "error" {
			return true
		}
	}
	// Alert if any detected pattern is error/critical.
	for _, p := range patterns {
		if p.Severity == "critical" || p.Severity == "error" {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *LogPilotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&logv1.LogPilot{}).
		Named("logpilot").
		Complete(r)
}
