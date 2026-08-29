package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/ollama-proxy-metrics/internal/budget"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
)

// listen opens a loopback listener on an arbitrary free port.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func TestServe_FinishesInFlightRequestOnShutdown(t *testing.T) {
	entered := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		// Long enough that an abrupt close would truncate the response.
		time.Sleep(150 * time.Millisecond)
		_, _ = fmt.Fprint(w, "finished")
	})}

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- result{body: string(body), err: err}
	}()

	<-entered
	cancel() // the SIGTERM equivalent, with a request mid-flight

	got := <-done
	if got.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", got.err)
	}
	if got.body != "finished" {
		t.Fatalf("response body = %q, want the complete %q", got.body, "finished")
	}
	if err := <-served; err != nil {
		t.Fatalf("serve returned %v, want nil after a clean shutdown", err)
	}
}

func TestServe_StopsAcceptingAfterShutdown(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})}

	ln := listen(t)
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 5*time.Second) }()

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("request before shutdown: %v", err)
	}
	_ = resp.Body.Close()

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("serve = %v, want nil", err)
	}

	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get("http://" + addr + "/"); err == nil {
		t.Fatal("server still accepted a request after shutdown")
	}
}

func TestServe_GraceExpiresOnAStuckHandler(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	entered := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	})}

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, 50*time.Millisecond) }()

	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + ln.Addr().String() + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-entered
	cancel()

	err := <-served
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("serve = %v, want the grace period to expire", err)
	}
}

func TestServe_ReturnsListenerError(t *testing.T) {
	ln := listen(t)
	_ = ln.Close() // Serve fails immediately on a closed listener

	srv := &http.Server{Handler: http.NotFoundHandler()}
	err := serve(context.Background(), srv, ln, time.Second)
	if err == nil {
		t.Fatal("serve = nil, want the listener error")
	}
}

func TestParseUpstreams(t *testing.T) {
	got, err := parseUpstreams(" http://a:11434 , http://b:11434 ")
	if err != nil {
		t.Fatalf("parseUpstreams: %v", err)
	}
	if len(got) != 2 || got[0].String() != "http://a:11434" || got[1].String() != "http://b:11434" {
		t.Fatalf("parseUpstreams = %v", got)
	}
}

func TestParseUpstreams_RejectsEmpty(t *testing.T) {
	if _, err := parseUpstreams("  , "); err == nil {
		t.Fatal("parseUpstreams(empty) = nil error, want an error")
	}
}

func TestSeedBudgets_ResumesTodaysUsage(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = store.Close() }()

	rec := db.RequestRecord{
		RequestID:   "r1",
		SessionID:   "sess",
		Timestamp:   time.Now().UTC(),
		Endpoint:    "/api/generate",
		Model:       "m",
		TotalTokens: 400,
		Cost:        1.25,
	}
	if err := store.InsertRequest(rec); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	tracker := budget.New(budget.Limits{TokensPerDay: 1000})
	if err := seedBudgets(store, tracker); err != nil {
		t.Fatalf("seedBudgets: %v", err)
	}

	_, usage := tracker.Snapshot()
	if usage["sess"].Tokens != 400 || usage["sess"].Cost != 1.25 {
		t.Fatalf("seeded usage = %+v, want today's row", usage["sess"])
	}
}

func TestSeedBudgets_SkippedWhenDisabled(t *testing.T) {
	tracker := budget.New(budget.Limits{})
	if err := seedBudgets(nil, tracker); err != nil {
		t.Fatalf("seedBudgets with no ceilings = %v, want nil", err)
	}
}

func TestBackfillModelNames(t *testing.T) {
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = store.Close() }()

	for i, model := range []string{"llama3", "llama3:latest", "mistral:7b"} {
		rec := db.RequestRecord{
			RequestID: fmt.Sprintf("r%d", i),
			Timestamp: time.Now().UTC(),
			Endpoint:  "/api/generate",
			Model:     model,
		}
		if err := store.InsertRequest(rec); err != nil {
			t.Fatalf("InsertRequest: %v", err)
		}
	}

	n, err := backfillModelNames(store, nil)
	if err != nil {
		t.Fatalf("backfillModelNames: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d rows, want 1", n)
	}

	models, err := store.Models()
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if strings.Join(models, ",") != "llama3:latest,mistral:7b" {
		t.Fatalf("models after backfill = %v", models)
	}
}

func TestGetEnvHelpers(t *testing.T) {
	t.Setenv("TEST_STR", "value")
	t.Setenv("TEST_INT", "42")
	t.Setenv("TEST_INT64", "9000000000")
	t.Setenv("TEST_FLOAT", "1.5")
	t.Setenv("TEST_BOOL", "true")
	t.Setenv("TEST_DUR", "45s")
	t.Setenv("TEST_BAD", "not-a-number")

	if got := getEnv("TEST_STR", "def"); got != "value" {
		t.Errorf("getEnv = %q", got)
	}
	if got := getEnvInt("TEST_INT", 0); got != 42 {
		t.Errorf("getEnvInt = %d", got)
	}
	if got := getEnvInt64("TEST_INT64", 0); got != 9_000_000_000 {
		t.Errorf("getEnvInt64 = %d", got)
	}
	if got := getEnvFloat("TEST_FLOAT", 0); got != 1.5 {
		t.Errorf("getEnvFloat = %v", got)
	}
	if got := getEnvBool("TEST_BOOL", false); !got {
		t.Error("getEnvBool = false")
	}
	if got := getEnvDuration("TEST_DUR", time.Second); got != 45*time.Second {
		t.Errorf("getEnvDuration = %v", got)
	}

	// An unparseable value falls back to the default rather than to a zero.
	if got := getEnvInt("TEST_BAD", 7); got != 7 {
		t.Errorf("getEnvInt(bad) = %d, want the default", got)
	}
	if got := getEnvDuration("TEST_BAD", time.Minute); got != time.Minute {
		t.Errorf("getEnvDuration(bad) = %v, want the default", got)
	}
	if got := getEnvBool("TEST_BAD", true); !got {
		t.Error("getEnvBool(bad) = false, want the default")
	}

	_ = os.Unsetenv("TEST_STR")
	if got := getEnv("TEST_STR", "def"); got != "def" {
		t.Errorf("getEnv(unset) = %q, want the default", got)
	}
}
