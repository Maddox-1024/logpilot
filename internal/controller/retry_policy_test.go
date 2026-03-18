package controller

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRequeueAfterForCategory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		interval time.Duration
		category retryCategory
		want     time.Duration
	}{
		{name: "transient doubles short interval", interval: time.Minute, category: retryCategoryTransient, want: 2 * time.Minute},
		{name: "transient caps large interval", interval: 10 * time.Minute, category: retryCategoryTransient, want: 15 * time.Minute},
		{name: "secret backs off harder", interval: time.Minute, category: retryCategorySecret, want: 5 * time.Minute},
		{name: "permanent config backs off longest", interval: time.Minute, category: retryCategoryPermanent, want: 15 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := requeueAfterForCategory(tt.interval, tt.category); got != tt.want {
				t.Fatalf("requeueAfterForCategory(%s, %s) = %s, want %s", tt.interval, tt.category, got, tt.want)
			}
		})
	}
}

func TestClassifyLokiRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want retryCategory
	}{
		{
			name: "bad request is permanent",
			err:  &httpStatusError{Service: "loki", Status: http.StatusBadRequest},
			want: retryCategoryPermanent,
		},
		{
			name: "rate limit is transient",
			err:  &httpStatusError{Service: "loki", Status: http.StatusTooManyRequests},
			want: retryCategoryTransient,
		},
		{
			name: "server error is transient",
			err:  &httpStatusError{Service: "loki", Status: http.StatusInternalServerError},
			want: retryCategoryTransient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLokiRetry(tt.err); got != tt.want {
				t.Fatalf("classifyLokiRetry(%v) = %s, want %s", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyLLMRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want retryCategory
	}{
		{
			name: "unsupported provider is permanent",
			err:  errors.New("unsupported LLM provider: FooAI"),
			want: retryCategoryPermanent,
		},
		{
			name: "authentication failure is permanent",
			err:  errors.New("openai api error: authentication failed"),
			want: retryCategoryPermanent,
		},
		{
			name: "timeout stays transient",
			err:  errors.New("openai api error: context deadline exceeded"),
			want: retryCategoryTransient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLLMRetry(tt.err); got != tt.want {
				t.Fatalf("classifyLLMRetry(%v) = %s, want %s", tt.err, got, tt.want)
			}
		})
	}
}
