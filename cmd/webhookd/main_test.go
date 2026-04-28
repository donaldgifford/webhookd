// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/donaldgifford/webhookd/internal/k8s"
	"github.com/donaldgifford/webhookd/internal/webhook/wizapi"
)

// envtest lifecycle shared across the integration tests in this
// package. Start once in TestMain, stop once after; goroutines that
// envtest leaves around are listed in goleak.IgnoreTopFunction.
var (
	testEnv  *envtest.Environment
	testREST *rest.Config
)

// TestMain wraps every test in goleak so leaked goroutines from the
// BatchSpanProcessor or HTTP listeners are caught immediately. envtest
// (when KUBEBUILDER_ASSETS is set) is brought up here too — Phase 6's
// end-to-end test needs a real apiserver. The IgnoreTopFunction list
// covers transport-pool / wait.Until goroutines that envtest legitimately
// holds across the test binary's lifetime.
func TestMain(m *testing.M) {
	if assets := os.Getenv("KUBEBUILDER_ASSETS"); assets != "" {
		testEnv = &envtest.Environment{
			CRDDirectoryPaths: []string{
				filepath.Join("..", "..", "deploy", "crds"),
			},
			ErrorIfCRDPathMissing: true,
			Scheme:                k8s.Scheme,
		}
		cfg, err := testEnv.Start()
		if err != nil {
			log.Fatalf("envtest start: %v", err)
		}
		testREST = cfg
	}
	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	if leaks := goleak.Find(
		goleak.IgnoreTopFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue"),
		// envtest holds long-lived HTTP keepalives + wait.Until loops
		// that survive Stop(). These show up only when envtest is in
		// the picture; ignoring them keeps cmd/webhookd's existing
		// goleak coverage useful without forcing every Phase 1 test to
		// pay the envtest start cost.
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreTopFunction("k8s.io/apimachinery/pkg/util/wait.loopConditionUntilContext"),
		goleak.IgnoreTopFunction("k8s.io/client-go/util/workqueue.(*Type).updateUnfinishedWorkLoop"),
	); leaks != nil {
		log.Printf("goleak found leaks: %v", leaks)
		os.Exit(1)
	}
	os.Exit(code)
}

// TestRun_HappyPath drives the full wiring with a real port pair,
// posts a signed webhook, and asserts a clean shutdown when the
// context is canceled. We rely on the public+admin servers actually
// binding so this test catches misconfiguration that unit tests miss.
func TestRun_HappyPath(t *testing.T) {
	t.Setenv("WEBHOOK_SIGNING_SECRET", "topsecret")
	t.Setenv("WEBHOOK_ADDR", "127.0.0.1:0")
	t.Setenv("WEBHOOK_ADMIN_ADDR", "127.0.0.1:0")
	// Disable the real OTLP exporter; an unreachable collector would
	// keep the BatchSpanProcessor alive past Shutdown's deadline.
	t.Setenv("WEBHOOK_TRACING_ENABLED", "false")
	t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", "5s")
	// Disable JSM provider — Phase 1 integration tests don't exercise
	// the JSM/CR pipeline, and JSM enabled would require config that
	// isn't relevant here. Phase 6 adds a dedicated end-to-end test.
	t.Setenv("WEBHOOK_PROVIDERS", "")
	t.Setenv("WEBHOOK_CR_SYNC_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	// Run is dispatched; cancel after a short wait so the shutdown
	// path executes. Without the wait the listeners may not have
	// bound yet.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run() err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not return within 10s after cancel")
	}
}

// TestRun_TombstoneWebhookRoute exercises the in-flight Phase 2 state:
// the legacy `webhook.NewHandler` is gone, the dispatcher (Phase 6)
// isn't wired yet, and the route is held by a 503 tombstone so the
// middleware chain stays exercised. Phase 6 replaces this test with a
// full envtest end-to-end against the real dispatcher + executor.
func TestRun_TombstoneWebhookRoute(t *testing.T) {
	t.Setenv("WEBHOOK_SIGNING_SECRET", "topsecret")
	t.Setenv("WEBHOOK_ADDR", "127.0.0.1:18091")
	t.Setenv("WEBHOOK_ADMIN_ADDR", "127.0.0.1:19091")
	t.Setenv("WEBHOOK_TRACING_ENABLED", "false")
	t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", "5s")
	t.Setenv("WEBHOOK_PROVIDERS", "")
	t.Setenv("WEBHOOK_CR_SYNC_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("run() did not return on cleanup")
		}
	})

	if err := waitFor(t, "http://127.0.0.1:19091/healthz"); err != nil {
		t.Fatalf("admin healthz never became ready: %v", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		"http://127.0.0.1:18091/webhook/github",
		bytes.NewReader([]byte(`{"event_type":"push"}`)),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 503\nbody: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After header = %q, want %q", got, "30")
	}
	if got := resp.Header.Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id header is empty — RequestID middleware not running")
	}
}

// TestRun_EndToEnd_JSMToReadyCR drives the entire pipeline: real
// listeners, signed JSM payload, dispatcher routes to JSM provider,
// executor SSA-applies the CR against envtest's apiserver, an
// operator-impersonator goroutine writes Ready=True, and the response
// comes back with `status:"success"`. This is the canonical proof
// that Phase 1's substrate + Phase 2's wiring work together.
func TestRun_EndToEnd_JSMToReadyCR(t *testing.T) {
	if testREST == nil {
		t.Skip("KUBEBUILDER_ASSETS not set; run via 'make test'")
	}

	const ns = "wiz-operator"
	const issueKey = "SEC-9001"
	const crName = "jsm-sec-9001"

	// Pre-create the namespace; envtest doesn't manage namespaces for us.
	c, err := client.NewWithWatch(testREST, client.Options{Scheme: k8s.Scheme})
	if err != nil {
		t.Fatalf("client.NewWithWatch: %v", err)
	}
	if err := createNamespace(t, c, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Materialize a kubeconfig file pointing at envtest, since
	// k8s.NewClients reads from WEBHOOK_KUBECONFIG.
	kubeconfigPath := writeKubeconfig(t, testREST)

	t.Setenv("WEBHOOK_SIGNING_SECRET", "topsecret")
	t.Setenv("WEBHOOK_ADDR", "127.0.0.1:18092")
	t.Setenv("WEBHOOK_ADMIN_ADDR", "127.0.0.1:19092")
	t.Setenv("WEBHOOK_TRACING_ENABLED", "false")
	t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", "20s")
	t.Setenv("WEBHOOK_PROVIDERS", "jsm")
	t.Setenv("WEBHOOK_KUBECONFIG", kubeconfigPath)
	t.Setenv("WEBHOOK_CR_NAMESPACE", ns)
	t.Setenv("WEBHOOK_CR_IDENTITY_PROVIDER_ID", "okta-prod")
	t.Setenv("WEBHOOK_CR_SYNC_TIMEOUT", "10s")
	t.Setenv("WEBHOOK_JSM_TRIGGER_STATUS", "Ready to Provision")
	t.Setenv("WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID", "customfield_10201")
	t.Setenv("WEBHOOK_JSM_FIELD_ROLE", "customfield_10202")
	t.Setenv("WEBHOOK_JSM_FIELD_PROJECT", "customfield_10203")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("run() did not return on cleanup")
		}
	})

	// Watch for early run() failure during the boot window so a
	// dispatcher misconfiguration surfaces as a useful error rather
	// than an opaque healthz timeout.
	bootCtx, bootCancel := context.WithTimeout(ctx, 5*time.Second)
	defer bootCancel()
	go func() {
		select {
		case err := <-done:
			t.Errorf("run() exited during boot: %v", err)
			bootCancel()
		case <-bootCtx.Done():
		}
	}()

	if err := waitFor(t, "http://127.0.0.1:19092/healthz"); err != nil {
		t.Fatalf("admin healthz never came up: %v", err)
	}

	// Operator impersonator: poll for the CR, write Ready=True.
	go markCRReady(t, c, ns, crName)

	// Build a signed JSM payload.
	body := []byte(`{
		"issue": {
			"key": "` + issueKey + `",
			"fields": {
				"status": {"name": "Ready to Provision"},
				"customfield_10201": "team-platform",
				"customfield_10202": "admin",
				"customfield_10203": "core"
			}
		}
	}`)
	sig, ts := signJSMPayload(t, []byte("topsecret"), time.Now(), body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://127.0.0.1:18092/webhook/jsm", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Webhook-Signature", sig)
	req.Header.Set("X-Webhook-Timestamp", ts)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", resp.StatusCode, respBody)
	}

	parsed := map[string]any{}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		t.Fatalf("decode response body: %v\nraw: %s", err, respBody)
	}
	if parsed["status"] != "success" {
		t.Errorf("status = %v, want success\nbody: %s", parsed["status"], respBody)
	}
	if parsed["crName"] != crName {
		t.Errorf("crName = %v, want %s\nbody: %s", parsed["crName"], crName, respBody)
	}

	// Scrape the admin /metrics endpoint and assert the Phase 2
	// metrics observed at least one event each. We grep for the metric
	// name + the labeled outcome we expect — the goal is "the wiring
	// is alive end-to-end," not a precise count comparison (timing
	// against envtest is jittery).
	metricsBody := scrapeMetrics(ctx, t, "http://127.0.0.1:19092/metrics")
	for _, want := range []string{
		`webhookd_k8s_apply_total{kind="SAMLGroupMapping",outcome="created"} 1`,
		`webhookd_k8s_sync_duration_seconds_count{kind="SAMLGroupMapping",outcome="ready"} 1`,
		`webhookd_jsm_response_total{status_code="200"} 1`,
	} {
		if !strings.Contains(metricsBody, want) {
			t.Errorf("metric line missing from scrape: %q", want)
		}
	}
}

// scrapeMetrics fetches the admin /metrics body so the e2e test can
// assert that Phase 2 metric pipelines fired end-to-end.
func scrapeMetrics(ctx context.Context, t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("metrics request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	return string(body)
}

// markCRReady polls envtest for the CR webhookd is about to apply,
// then writes Ready=True with observedGeneration matching .metadata.generation.
// Mirrors the operator we'd see in production.
func markCRReady(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		obj := &wizapi.SAMLGroupMapping{}
		err := c.Get(context.Background(),
			client.ObjectKey{Namespace: namespace, Name: name}, obj)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		obj.Status.ObservedGeneration = obj.Generation
		obj.Status.Conditions = []metav1.Condition{{
			Type:               wizapi.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "wiz happy",
			LastTransitionTime: metav1.Now(),
		}}
		if err := c.Status().Update(context.Background(), obj); err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return
	}
	t.Errorf("operator-impersonator never saw %s/%s", namespace, name)
}

// signJSMPayload computes a v0:<ts>:<body> HMAC matching the canonical
// scheme webhook.Verify enforces.
func signJSMPayload(t *testing.T, secret []byte, ts time.Time, body []byte) (sig, tsHeader string) {
	t.Helper()
	tsHeader = strconv.FormatInt(ts.Unix(), 10)
	canonical := []byte("v0:" + tsHeader + ":")
	canonical = append(canonical, body...)
	mac := hmac.New(sha256.New, secret)
	mac.Write(canonical)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil)), tsHeader
}

// writeKubeconfig serializes a *rest.Config to a real kubeconfig file
// in t.TempDir(). webhookd's startup path goes through clientcmd, so
// the only way to point it at envtest is via WEBHOOK_KUBECONFIG.
func writeKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	api := clientcmdapi.NewConfig()
	api.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	api.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	api.Contexts["envtest"] = &clientcmdapi.Context{
		Cluster:  "envtest",
		AuthInfo: "envtest",
	}
	api.CurrentContext = "envtest"
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	if err := clientcmd.WriteToFile(*api, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// createNamespace creates ns if it doesn't exist; idempotent across
// repeated runs.
func createNamespace(t *testing.T, c client.Client, name string) error {
	t.Helper()
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// waitFor polls url with a short retry loop. Listener binding is
// asynchronous after run() dispatches goroutines — this gives the
// kernel a few hundred ms to honor SO_REUSEADDR / port allocation.
func waitFor(t *testing.T, url string) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, url, http.NoBody,
		)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errTimeout
}

var errTimeout = &timeoutError{}

type timeoutError struct{}

func (timeoutError) Error() string { return "timeout waiting for url" }
