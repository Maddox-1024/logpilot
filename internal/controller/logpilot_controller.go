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
	"strconv"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	logv1 "llm-log-operator/api/v1"
)

// LogPilotReconciler reconciles a LogPilot object
type LogPilotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

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

	// Calculate the time range for the query
	currentTime := time.Now().Unix()
	preTimeStamp := logPilot.Status.PreTimeStamp
	// print preTimeStamp for debugging
	fmt.Printf("preTimeStamp: %s\n", preTimeStamp)

	var preTime int64
	if preTimeStamp == "" {
		preTime = currentTime - 5
	} else {
		preTime, _ = strconv.ParseInt(preTimeStamp, 10, 64)
	}
	// Loki query
	lokiQuery := logPilot.Spec.LogQL
	endTime := currentTime * 1000000000     // Current time in nanoseconds
	startTime := (preTime - 5) * 1000000000 // Previous timestamp
	fmt.Printf("startTime: %d, endTime: %d\n", startTime, endTime)

	if startTime >= endTime {
		logger.Info("startTime >= endTime")
		// print startTime and endTime for debugging
		return ctrl.Result{RequeueAfter: time.Second * 10}, nil
	}

	startTimeForUpdate := currentTime
	lokiURL := fmt.Sprintf("%s/loki/api/v1/query_range?query=%s&start=%d&end=%d", logPilot.Spec.LokiURL, url.QueryEscape(lokiQuery), startTime, endTime)
	fmt.Printf("lokiURL: %s\n", lokiURL)
	lokiLogs, err := r.queryLoki(lokiURL)
	fmt.Println(lokiLogs)
	if err != nil {
		// logger.Error(err, "unable to query Loki")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// If there are log results, call LLM for analysis
	if lokiLogs != "" {
		fmt.Println("send log to llm")
		analysisResult, err := r.analyzeLogsWithLLM(logPilot.Spec.LLMToken, logPilot.Spec.LLMModel, lokiLogs)
		if err != nil {
			logger.Error(err, "unable to analyze logs with LLM")
			return ctrl.Result{}, err
		}
		// If the LLM result indicates there is a problem with the logs, send Feishu alert
		if analysisResult.HasError {
			err := r.sendLarkAlert(logPilot.Spec.LarkWebhook, analysisResult.Analysis)
			if err != nil {
				logger.Error(err, "unable to send lark alert")
				return ctrl.Result{}, err
			}
		}
	}
	// Update PreTimeStamp in status
	logPilot.Status.PreTimeStamp = fmt.Sprintf("%d", startTimeForUpdate)
	if err := r.Status().Update(ctx, &logPilot); err != nil {
		logger.Error(err, "unable to update logPilot status")
		return ctrl.Result{}, err
	}

	// Reconcile again after 10 seconds
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// queryLoki retrieves logs from Loki
func (r *LogPilotReconciler) queryLoki(lokiURL string) (string, error) {
	resp, err := http.Get(lokiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var lokiResponse map[string]interface{}
	err = json.Unmarshal(body, &lokiResponse)
	if err != nil {
		return "", err
	}
	data, ok := lokiResponse["data"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("data not found")
	}

	result, ok := data["result"].([]interface{})
	if !ok || len(result) == 0 {
		return "", fmt.Errorf("result not found")
	}

	return string(body), nil
}

// LLMAnalysisResult is used to store the results of LLM analysis
type LLMAnalysisResult struct {
	HasError bool   // Whether there are error logs
	Analysis string // Analysis results returned by LLM
}

// analyzeLogsWithLLM calls the LLM interface to analyze logs
func (r *LogPilotReconciler) analyzeLogsWithLLM(token, model, logs string) (*LLMAnalysisResult, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(token))
	if err != nil {
		return nil, err
	}
	defer client.Close()

	modelClient := client.GenerativeModel(model)
	prompt := genai.Text(fmt.Sprintf("你是一名经验丰富的日志分析专家和系统优化顾问。以下日志是从日志系统里获取到的日志。你的任务是：给出环境（根据namespace标签确定是哪个环境）,给出服务（根据app标签确定是哪个服务），分析日志内容，总结关键信息，提出可能的原因，给出简短的建议。如果遇到严重问题，例如外部系统请求失败、系统故障、致命错误、数据库连接异常等严重问题时，在内容里返回[lark]标识\n%s", logs))

	resp, err := modelClient.GenerateContent(ctx, prompt)
	if err != nil {
		fmt.Printf("ChatCompletion error: %v\n", err)
		return nil, err
	}

	return r.parseLLMResponse(resp), nil
}

// parseLLMResponse parses the response from the LLM API
func (r *LogPilotReconciler) parseLLMResponse(resp *genai.GenerateContentResponse) *LLMAnalysisResult {
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

	result := &LLMAnalysisResult{
		Analysis: analysis, // Get analysis results from text returned by LLM
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
func (r *LogPilotReconciler) sendLarkAlert(webhook, analysis string) error {
	// Feishu message content
	message := map[string]interface{}{
		"msg_type": "text",
		"content": map[string]string{
			"text": analysis,
		},
	}
	// Serialize message content to JSON
	messageBody, _ := json.Marshal(message)
	// Create HTTP POST request
	req, err := http.NewRequest("POST", webhook, bytes.NewBuffer(messageBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Send request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// Check response status
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code: %d", resp.StatusCode)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *LogPilotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&logv1.LogPilot{}).
		Named("logpilot").
		Complete(r)
}
