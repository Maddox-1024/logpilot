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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	openai "github.com/openai/openai-go"
	openAIOption "github.com/openai/openai-go/option"
	googleOption "google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	logv1 "llm-log-operator/api/v1"
)

// LogPilotReconciler reconciles a LogPilot object
type LogPilotReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
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
		// Use default interval of 10 seconds if parsing fails
		interval = time.Minute
	}

	lastTime := logPilot.Status.LastCheckTime.Time
	if !lastTime.IsZero() && time.Since(lastTime) < interval {
		return ctrl.Result{RequeueAfter: interval - time.Since(lastTime)}, nil
	}

	apiKey, err := r.getSecretValue(ctx, req.Namespace, logPilot.Spec.LLMAPIKeySecret, logPilot.Spec.LLMAPIKeySecretKey)
	if err != nil {
		logger.Error(err, "unable to get LLM API key from secret")
		if statusErr := r.updateStatus(ctx, &logPilot, "", fmt.Sprintf("Secret error: %v", err)); statusErr != nil {
			logger.Error(statusErr, "failed to update status for secret retrieval error")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	// Loki query window:
	// - normal case: query from last successful check time to now
	// - first run fallback: query the last "interval"
	lokiQuery := logPilot.Spec.LogQL
	end := time.Now()
	start := logPilot.Status.LastCheckTime.Time
	if start.IsZero() {
		start = end.Add(-interval)
	}
	// Guard against clock skew or invalid status time.
	if start.After(end) {
		start = end.Add(-interval)
	}

	endTime := end.UnixNano()
	startTime := start.UnixNano()
	fmt.Printf("startTime: %d, endTime: %d\n", startTime, endTime)

	lokiURL := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d", logPilot.Spec.LokiURL, url.QueryEscape(lokiQuery), startTime, endTime)
	// fmt.Printf("lokiURL: %s\n", lokiURL)
	lokiLogs, err := r.queryLoki(lokiURL)
	// fmt.Println(lokiLogs)
	if err != nil {
		logger.Error(err, "unable to query Loki")
		if statusErr := r.updateStatus(ctx, &logPilot, "", fmt.Sprintf("Loki error: %v", err)); statusErr != nil {
			logger.Error(statusErr, "failed to update status for loki query error")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	}

	// If there are log results, call LLM for analysis
	if lokiLogs != "" {
		fmt.Println("send log to llm")
		analysisResult, err := r.analyzeLogsWithLLM(logPilot.Spec, apiKey, lokiLogs)
		if err != nil {
			logger.Error(err, "unable to analyze logs with LLM")
			if statusErr := r.updateStatus(ctx, &logPilot, "", fmt.Sprintf("LLM analysis error: %v", err)); statusErr != nil {
				logger.Error(statusErr, "failed to update status for llm analysis error")
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: interval}, nil
		}
		// If the LLM result indicates there is a problem with the logs, send Feishu alert
		if analysisResult.HasError {
			err := r.sendLarkAlert(ctx, logPilot.Spec.LarkWebhook, analysisResult.Analysis)
			if err != nil {
				logger.Error(err, "Failed to send lark alert")
			}
		} else {
			logger.Info("No issues found in logs according to LLM analysis")
		}
		// Update status with analysis result
		if statusErr := r.updateStatus(ctx, &logPilot, analysisResult.Analysis, ""); statusErr != nil {
			logger.Error(statusErr, "failed to update status for successful analysis")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: interval}, nil
	} else {
		if statusErr := r.updateStatus(ctx, &logPilot, "No logs found", ""); statusErr != nil {
			logger.Error(statusErr, "failed to update status for empty log result")
			return ctrl.Result{}, statusErr
		}
	}

	// Reconcile again after 10 seconds
	return ctrl.Result{RequeueAfter: interval}, nil
}

type LokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// queryLoki retrieves logs from Loki
func (r *LogPilotReconciler) queryLoki(lokiURL string) (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(lokiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("loki returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var lokiResp LokiResponse
	if err := json.Unmarshal(body, &lokiResp); err != nil {
		return "", err
	}

	if len(lokiResp.Data.Result) == 0 {
		return "", nil // No logs found
	}

	var logs []string

	for _, stream := range lokiResp.Data.Result {
		for _, entry := range stream.Values {
			if len(entry) == 2 {
				logs = append(logs, entry[1])
			}
		}
	}

	return strings.Join(logs, "\n"), nil
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

const systemPrompt = `你是一名经验丰富的日志分析专家和系统优化顾问。以下日志是从日志系统里获取到的日志。你的任务是：给出环境（根据namespace标签确定是哪个环境）,给出服务（根据app标签确定是哪个服务），分析日志内容，总结关键信息，提出可能的原因，给出简短的建议。如果遇到严重问题，例如外部系统请求失败、系统故障、致命错误、数据库连接异常等严重问题时，在内容里返回[lark]标识`

// analyzeLogsWithLLM calls the LLM interface to analyze logs
func (r *LogPilotReconciler) analyzeLogsWithLLM(spec logv1.LogPilotSpec, token, logs string) (*LLMAnalysisResult, error) {
	switch spec.LLMProvider {
	case "OpenAI":
		return callOpenAI(spec, token, logs)
	case "Gemini":
		return callGemini(spec, token, logs)
	default:
		return nil, fmt.Errorf("unsupported LLM provider: %s", spec.LLMProvider)
	}
}

func callOpenAI(spec logv1.LogPilotSpec, token, logs string) (*LLMAnalysisResult, error) {
	// 1. 配置 Client 选项
	opts := []openAIOption.RequestOption{
		openAIOption.WithAPIKey(token),
	}

	cfg := spec.OpenAI
	if cfg != nil {
		if cfg.BaseURL != "" {
			opts = append(opts, openAIOption.WithBaseURL(cfg.BaseURL))
		}
	}

	// 2. 初始化 Client
	client := openai.NewClient(opts...)

	// 4. 发起调用
	completion, err := client.Chat.Completions.New(context.TODO(), openai.ChatCompletionNewParams{
		Model: spec.LLMModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(logs),
		},
	})

	if err != nil {
		return nil, fmt.Errorf("openai api error: %w", err)
	}

	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned")
	}

	// 5. 解析结果
	analysis := completion.Choices[0].Message.Content

	return parseLLMResponse(analysis), nil
}

func callGemini(spec logv1.LogPilotSpec, token, logs string) (*LLMAnalysisResult, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, googleOption.WithAPIKey(token))
	if err != nil {
		return nil, err
	}
	defer client.Close()

	modelClient := client.GenerativeModel(spec.LLMModel)
	prompt := genai.Text(fmt.Sprintf("%s\n%s", systemPrompt, logs))

	resp, err := modelClient.GenerateContent(ctx, prompt)
	if err != nil {
		fmt.Printf("ChatCompletion error: %v\n", err)
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

	return parseLLMResponse(analysis), nil
}

// parseLLMResponse parses the response from the LLM API
func parseLLMResponse(resp string) *LLMAnalysisResult {

	result := &LLMAnalysisResult{
		Analysis: resp, // Get analysis results from text returned by LLM
	}

	// Simple judgment whether the analysis result contains error identifiers
	if strings.Contains(strings.ToLower(result.Analysis), "lark") {
		result.HasError = true
	} else {
		result.HasError = false
	}

	return result
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
	resp, err := r.HTTPClient.Do(req)
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

func (r *LogPilotReconciler) updateStatus(ctx context.Context, logPilot *logv1.LogPilot, analysis, errMsg string) error {
	logPilot.Status.LastCheckTime = metav1.Now()
	logPilot.Status.LastAnalysis = analysis
	logPilot.Status.LastError = errMsg
	return r.Status().Update(ctx, logPilot)
}

// SetupWithManager sets up the controller with the Manager.
func (r *LogPilotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&logv1.LogPilot{}).
		Named("logpilot").
		Complete(r)
}
