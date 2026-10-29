package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests cover the stop path only: does a request already running get its remaining
// events, does a request arriving after the signal get refused, and what happens when the drain
// window is too short. Nothing here touches a provider, a database, Docker or the network beyond
// loopback.

type streamResult struct {
	events   int
	finished bool
	err      error
}

// countingStream writes `events` server-sent events, `gap` apart, flushing each one. It is the
// slow request the drain has to make room for - the product's own SSE shape, not a stub of one.
func countingStream(events int, gap time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for index := 1; index <= events; index++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = fmt.Fprintf(w, "data: event-%d\n\n", index)
			flusher.Flush()
			time.Sleep(gap)
		}
	})
}

// readStream consumes the response line by line so a test can count delivered events rather
// than bytes, and signals `started` as soon as the first one arrives.
func readStream(url string, started chan<- struct{}) streamResult {
	response, err := http.Get(url) //nolint:noctx // a fixed URL built by the test
	if err != nil {
		return streamResult{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	result := streamResult{}
	for {
		line, readErr := reader.ReadString('\n')
		if strings.HasPrefix(line, "data: event-") {
			result.events++
			if started != nil {
				select {
				case started <- struct{}{}:
				default:
				}
				started = nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				result.finished = true
			} else {
				result.err = readErr
			}
			return result
		}
	}
}

// listeningStream binds an ephemeral loopback port and hands back the closure serveUntil needs,
// so the test exercises the real Serve/Shutdown pairing on a port it does not have to guess.
func listeningStream(t *testing.T, handler http.Handler) (*http.Server, string, string, func() error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()
	server := newHTTPServer(address, handler)
	return server, address, "http://" + address + "/stream", func() error { return server.Serve(listener) }
}

// waitForClosedListener blocks until the address stops accepting connections, which is the
// point Shutdown has reached. Without it a test would race the goroutine that performs the
// drain and could read "refused" before the signal was even handled.
func waitForClosedListener(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = connection.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener kept accepting connections; Shutdown never closed it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeUntilDrainsAnInFlightStream(t *testing.T) {
	server, _, url, serve := listeningStream(t, countingStream(12, 20*time.Millisecond))
	started := make(chan struct{}, 1)
	client := make(chan streamResult, 1)
	go func() { client <- readStream(url, started) }()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveUntil(ctx, server, 2*time.Second, serve) }()
	<-started
	// The stop signal arrives while the stream still has events to write.
	cancel()

	if err := <-served; err != nil {
		t.Fatalf("serveUntil = %v, want a clean stop after the drain", err)
	}
	result := <-client
	if result.err != nil {
		t.Fatalf("the in-flight stream was cut short: %v", result.err)
	}
	if result.events != 12 || !result.finished {
		t.Fatalf("the client saw %d of 12 events (clean end=%v)", result.events, result.finished)
	}
}

func TestServeUntilRefusesNewRequestsOnceDraining(t *testing.T) {
	server, address, url, serve := listeningStream(t, countingStream(20, 40*time.Millisecond))
	started := make(chan struct{}, 1)
	client := make(chan streamResult, 1)
	go func() { client <- readStream(url, started) }()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveUntil(ctx, server, 5*time.Second, serve) }()
	<-started
	cancel()
	waitForClosedListener(t, address)

	// Every request that arrives from here on is refused: the listener is gone, so the stop
	// is not "queue it behind the drain".
	for attempt := 0; attempt < 3; attempt++ {
		response, err := http.Get(url) //nolint:noctx // loopback, bounded by the test
		if err != nil {
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatal("a request that arrived after the stop signal was served with 200")
		}
	}

	if err := <-served; err != nil {
		t.Fatalf("serveUntil = %v", err)
	}
	result := <-client
	if result.err != nil || result.events < 2 {
		t.Fatalf("the drained stream was disturbed: %+v", result)
	}
}

func TestServeUntilForcesClosedWhenTheWindowExpires(t *testing.T) {
	// Twenty events at 40ms needs ~800ms; a 120ms window cannot drain it.
	server, _, url, serve := listeningStream(t, countingStream(20, 40*time.Millisecond))
	started := make(chan struct{}, 1)
	client := make(chan streamResult, 1)
	go func() { client <- readStream(url, started) }()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveUntil(ctx, server, 120*time.Millisecond, serve) }()
	<-started
	cancel()

	if err := <-served; !errors.Is(err, errShutdownForced) {
		t.Fatalf("serveUntil = %v, want %v", err, errShutdownForced)
	}
	result := <-client
	if result.events >= 20 {
		t.Fatalf("every event still arrived (%d), so the drain window never expired", result.events)
	}
}

// TestSIGTERMReachesTheDrainPath lives in shutdown_posix_test.go and
// shutdown_windows_test.go: the signal has to be sent for real, and the way to do that is
// platform-specific.

// TestDrainWindowIsShorterThanTheContainerStopWindow keeps two numbers written in two files
// honest. If compose's stop_grace_period were at or below shutdownGrace, Docker's SIGKILL would
// land on top of the drain and a stream would be killed rather than finished.
func TestDrainWindowIsShorterThanTheContainerStopWindow(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	drain := regexp.MustCompile(`(?m)^const shutdownGrace = (\d+) \* time\.Second$`).FindStringSubmatch(string(source))
	if drain == nil {
		t.Fatal("main.go no longer declares shutdownGrace as a whole number of seconds")
	}
	drainSeconds, err := strconv.Atoi(drain[1])
	if err != nil {
		t.Fatal(err)
	}

	compose, err := os.ReadFile(filepath.Join("..", "..", "compose.app.yml"))
	if err != nil {
		t.Fatal(err)
	}
	backend := between(t, string(compose), "\n  backend:", "\n  frontend:")
	window := regexp.MustCompile(`stop_grace_period: (\d+)(s|m)`).FindStringSubmatch(backend)
	if window == nil {
		t.Fatal("the backend service sets no stop_grace_period")
	}
	value, err := strconv.Atoi(window[1])
	if err != nil {
		t.Fatal(err)
	}
	stopSeconds := value
	if window[2] == "m" {
		stopSeconds = value * 60
	}
	if stopSeconds <= drainSeconds {
		t.Fatalf("stop_grace_period=%ds would race the %ds drain window", stopSeconds, drainSeconds)
	}
}

func between(t *testing.T, text, start, end string) string {
	t.Helper()
	from := strings.Index(text, start)
	if from < 0 {
		t.Fatalf("%q is missing", start)
	}
	rest := text[from:]
	until := strings.Index(rest[len(start):], end)
	if until < 0 {
		return rest
	}
	return rest[:len(start)+until]
}
