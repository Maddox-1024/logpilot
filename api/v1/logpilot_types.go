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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
type ModelProvider string

const (
	ModelProviderOpenAI ModelProvider = "OpenAI"
	ModelProviderGemini ModelProvider = "Gemini"
)

type OpenAIConfig struct {
	// Base URL for the OpenAI API
	// +optional
	BaseURL string `json:"baseURL,omitempty"`
}

type GeminiConfig struct{}

// LogPilotSpec defines the desired state of LogPilot.
type LogPilotSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// Loki URL
	LokiURL string `json:"lokiURL,omitempty"`
	// Loki LogQL
	LogQL string `json:"logQL,omitempty"`
	// Interval
	Interval string `json:"interval,omitempty"`
	// Model Provider
	LLMProvider ModelProvider `json:"llmProvider,omitempty"`
	// LLM Model
	LLMModel string `json:"llmModel,omitempty"`
	// LLM API Key Secret
	LLMAPIKeySecret string `json:"llmAPIKeySecret,omitempty"`
	// LLM API Key Secret Key
	LLMAPIKeySecretKey string `json:"llmAPIKeySecretKey,omitempty"`
	// OpenAI Config
	OpenAI *OpenAIConfig `json:"openAI,omitempty"`
	// Gemini Config
	Gemini *GeminiConfig `json:"gemini,omitempty"`
	// Feishu Webhook
	LarkWebhook string `json:"larkWebhook,omitempty"`
}

// LogPilotStatus defines the observed state of LogPilot.
type LogPilotStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	LastCheckTime metav1.Time `json:"lastCheckTime,omitempty"`
	LastAnalysis  string      `json:"lastAnalysis,omitempty"`
	LastError     string      `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// LogPilot is the Schema for the logpilots API.
type LogPilot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogPilotSpec   `json:"spec,omitempty"`
	Status LogPilotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LogPilotList contains a list of LogPilot.
type LogPilotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogPilot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LogPilot{}, &LogPilotList{})
}
