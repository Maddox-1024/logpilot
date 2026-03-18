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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apixclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	logv1 "llm-log-operator/api/v1"
)

var _ = Describe("LogPilot Controller", func() {
	const (
		llmSecretName     = "llm-api-key"
		llmSecretKey      = "token"
		webhookSecretName = "webhook-secret"
		webhookSecretKey  = "url"
	)

	var (
		ctx        context.Context
		namespace  string
		resource   *logv1.LogPilot
		reconciler *LogPilotReconciler
	)

	newLokiResponse := func(lines ...string) string {
		if len(lines) == 0 {
			return `{"status":"success","data":{"resultType":"streams","result":[]}}`
		}

		values := ""
		for i, line := range lines {
			if i > 0 {
				values += ","
			}
			ts := time.Now().Add(time.Duration(i) * time.Second).UnixNano()
			values += fmt.Sprintf(`["%d",%q]`, ts, line)
		}

		return fmt.Sprintf(`{
			"status":"success",
			"data":{
				"resultType":"streams",
				"result":[
					{
						"stream":{"namespace":"prod","app":"checkout"},
						"values":[%s]
					}
				]
			}
		}`, values)
	}

	createNamespace := func(ctx context.Context, name string) {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	}

	relaxCRDValidation := func(ctx context.Context) {
		clientset, err := apixclientset.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		crd, err := clientset.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "logpilots.log.aiops.com", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		for i := range crd.Spec.Versions {
			version := &crd.Spec.Versions[i]
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}

			specSchema := version.Schema.OpenAPIV3Schema.Properties["spec"]
			intervalSchema := specSchema.Properties["interval"]
			intervalSchema.Pattern = ""
			specSchema.Properties["interval"] = intervalSchema
			version.Schema.OpenAPIV3Schema.Properties["spec"] = specSchema
		}

		_, err = clientset.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, crd, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	createSecret := func(ctx context.Context, namespace, name, key, value string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				key: []byte(value),
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	fetchLogPilot := func() *logv1.LogPilot {
		current := &logv1.LogPilot{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: namespace}, current)).To(Succeed())
		return current
	}

	reconcileOnce := func() ctrl.Result {
		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: resource.Name, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("logpilot-test-%d", time.Now().UnixNano())
		relaxCRDValidation(ctx)
		createNamespace(ctx, namespace)

		reconciler = &LogPilotReconciler{
			Client:     k8sClient,
			Scheme:     k8sClient.Scheme(),
			HTTPClient: &http.Client{Timeout: 2 * time.Second},
		}
	})

	AfterEach(func() {
		if resource != nil {
			current := &logv1.LogPilot{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: namespace}, current)
			if err == nil {
				Expect(k8sClient.Delete(ctx, current)).To(Succeed())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}

		for _, secretRef := range []string{llmSecretName, webhookSecretName} {
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: secretRef, Namespace: namespace}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}

		ns := &corev1.Namespace{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns)
		if err == nil {
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	createResource := func(lokiURL string, interval string) {
		resource = &logv1.LogPilot{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sample",
				Namespace: namespace,
			},
			Spec: logv1.LogPilotSpec{
				LokiURL:            lokiURL,
				LogQL:              `{app="checkout"}`,
				Interval:           interval,
				LLMProvider:        logv1.ModelProviderOpenAI,
				LLMModel:           "gpt-4o-mini",
				LLMAPIKeySecret:    llmSecretName,
				LLMAPIKeySecretKey: llmSecretKey,
				WebhookSecret:      webhookSecretName,
				WebhookSecretKey:   webhookSecretKey,
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	}

	It("records a status error when the llm secret is missing", func() {
		createResource("http://127.0.0.1:1", "2m")

		result := reconcileOnce()
		current := fetchLogPilot()

		Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
		Expect(current.Status.LastError).To(ContainSubstring("Secret error"))
		Expect(current.Status.LastAttemptTime.IsZero()).To(BeFalse())
		Expect(current.Status.LastSuccessTime.IsZero()).To(BeTrue())
		Expect(current.Status.LastAnalysis).To(BeEmpty())
	})

	It("records a success status when loki returns no log lines", func() {
		createSecret(ctx, namespace, llmSecretName, llmSecretKey, "test-token")
		createSecret(ctx, namespace, webhookSecretName, webhookSecretKey, "https://example.invalid/lark")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/loki/api/v1/query_range"))
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(newLokiResponse()))
			Expect(err).NotTo(HaveOccurred())
		}))
		defer server.Close()

		createResource(server.URL, "90s")

		result := reconcileOnce()
		current := fetchLogPilot()

		Expect(result.RequeueAfter).To(Equal(90 * time.Second))
		Expect(current.Status.LastError).To(BeEmpty())
		Expect(current.Status.LastAnalysis).To(Equal("No logs found"))
		Expect(current.Status.LastAttemptTime.IsZero()).To(BeFalse())
		Expect(current.Status.LastSuccessTime.IsZero()).To(BeFalse())
	})

	It("records a success status when only non-error logs are returned", func() {
		createSecret(ctx, namespace, llmSecretName, llmSecretKey, "test-token")
		createSecret(ctx, namespace, webhookSecretName, webhookSecretKey, "https://example.invalid/lark")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(newLokiResponse(
				`INFO startup complete`,
				`WARN retrying background sync`,
			)))
			Expect(err).NotTo(HaveOccurred())
		}))
		defer server.Close()

		createResource(server.URL, "1m")

		result := reconcileOnce()
		current := fetchLogPilot()

		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(current.Status.LastError).To(BeEmpty())
		Expect(current.Status.LastAnalysis).To(Equal("No error/critical logs found"))
		Expect(current.Status.LastSuccessTime.IsZero()).To(BeFalse())
	})

	It("records a status error when the loki query fails", func() {
		createSecret(ctx, namespace, llmSecretName, llmSecretKey, "test-token")
		createSecret(ctx, namespace, webhookSecretName, webhookSecretKey, "https://example.invalid/lark")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream failed", http.StatusInternalServerError)
		}))
		defer server.Close()

		createResource(server.URL, "75s")

		result := reconcileOnce()
		current := fetchLogPilot()

		Expect(result.RequeueAfter).To(Equal(75 * time.Second))
		Expect(current.Status.LastError).To(ContainSubstring("Loki error"))
		Expect(current.Status.LastError).To(ContainSubstring("status 500"))
		Expect(current.Status.LastAttemptTime.IsZero()).To(BeFalse())
		Expect(current.Status.LastSuccessTime.IsZero()).To(BeTrue())
	})

	It("falls back to a one minute interval when spec.interval is empty", func() {
		createSecret(ctx, namespace, llmSecretName, llmSecretKey, "test-token")
		createSecret(ctx, namespace, webhookSecretName, webhookSecretKey, "https://example.invalid/lark")

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(newLokiResponse()))
			Expect(err).NotTo(HaveOccurred())
		}))
		defer server.Close()

		createResource(server.URL, "")

		result := reconcileOnce()
		current := fetchLogPilot()

		Expect(result.RequeueAfter).To(Equal(time.Minute))
		Expect(current.Status.LastError).To(BeEmpty())
		Expect(current.Status.LastAnalysis).To(Equal("No logs found"))
	})
})
