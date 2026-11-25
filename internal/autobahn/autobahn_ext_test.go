// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package autobahn_test runs the Autobahn WebSocket test suite against
// internal/websocket. It lives in its own module to keep Docker out of the
// library's tests. Set AUTOBAHN=1 to run it. AUTOBAHN_REPORTS sets where
// reports are written.
package autobahn_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect/v2/internal/assert"
	"connectrpc.com/connect/v2/internal/websocket"
)

const (
	image = "crossbario/autobahn-testsuite"
	agent = "connect"
	// readLimit fits section 9's largest message of 16 MiB.
	readLimit = 32 << 20
	// closeGrace bounds the drain after a failure, as connecthttp's grace
	// timer does. A peer stuck mid-frame never sends its Close.
	closeGrace = time.Second
)

// TestClient runs our client against the suite's fuzzing server.
func TestClient(t *testing.T) {
	t.Parallel()
	skipUnlessEnabled(t)
	reports := reportsDir(t)
	// The suite rejects a Host header whose port differs from the one it
	// listens on, which rules out remapping the port.
	port := freePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	spec := writeSpec(t, map[string]any{
		"url":           fmt.Sprintf("ws://127.0.0.1:%d", port),
		"outdir":        "/reports/client",
		"cases":         []string{"*"},
		// We never offer client_max_window_bits, which 13.3 to 13.6 need.
		"exclude-cases": []string{"13.3.*", "13.4.*", "13.5.*", "13.6.*"},
	})
	runDocker(t, reports, "client", false,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-v", spec+":/spec.json:ro",
		image, "wstest", "--mode", "fuzzingserver", "--spec", "/spec.json", "--webport=0",
	)
	count := caseCount(t, baseURL)
	for index := 1; index <= count; index++ {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		conn, err := dial(ctx, fmt.Sprintf("%s/runCase?case=%d&agent=%s", baseURL, index, agent))
		if assert.Nil(t, err, assert.Sprintf("case %d", index)) {
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			_ = echo(conn)
			stop()
		}
		cancel()
	}
	conn, err := dial(t.Context(), baseURL+"/updateReports?agent="+agent)
	assert.Nil(t, err)
	stop := context.AfterFunc(t.Context(), func() { _ = conn.Close() })
	// The server closes once the reports are written.
	_, err = conn.ReadMessage(nil, -1)
	assert.NotNil(t, err)
	stop()
	_ = conn.Close()
	checkReport(t, filepath.Join(reports, "client", "index.json"))
}

// TestServer runs the suite's fuzzing client against our echo server.
func TestServer(t *testing.T) {
	t.Parallel()
	skipUnlessEnabled(t)
	reports := reportsDir(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(t, err)
	server := &http.Server{
		Handler: http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
			conn, err := websocket.Accept(responseWriter, request, websocket.Options{Compression: true})
			if err != nil {
				return
			}
			stop := context.AfterFunc(request.Context(), func() { _ = conn.Close() })
			defer stop()
			_ = echo(conn)
		}),
		ReadHeaderTimeout: time.Minute,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	assert.True(t, ok)
	// Docker Desktop forwards host.docker.internal to the host's loopback.
	// Native Docker on Linux maps it to the bridge, which a loopback listener
	// doesn't accept, and the host network reaches loopback directly.
	host, network := "host.docker.internal", "--add-host=host.docker.internal:host-gateway"
	if runtime.GOOS == "linux" {
		host, network = "127.0.0.1", "--network=host"
	}
	spec := writeSpec(t, map[string]any{
		"outdir": "/reports/server",
		"servers": []map[string]string{{
			"agent": agent,
			"url":   fmt.Sprintf("ws://%s:%d", host, tcpAddr.Port),
		}},
		"cases":         []string{"*"},
		// 13.3 and 13.5 offer server_max_window_bits below 15, which we
		// decline.
		"exclude-cases": []string{"13.3.*", "13.5.*"},
	})
	runDocker(t, reports, "server", true,
		network,
		"-v", spec+":/spec.json:ro",
		image, "wstest", "--mode", "fuzzingclient", "--spec", "/spec.json",
	)
	checkReport(t, filepath.Join(reports, "server", "index.json"))
}

// echo writes each message back until the connection ends. The package
// echoes the peer's Close and sends 1002, 1007 and 1009 itself. The buffer
// starts with header space, which makes the echo zero-copy. The drain after
// a failure gets closeGrace.
func echo(conn *websocket.Conn) error {
	defer func() { _ = conn.Close() }()
	var headerSpace [websocket.MaxHeaderSize]byte
	var frame bytes.Buffer
	for {
		frame.Reset()
		frame.Write(headerSpace[:])
		messageType, err := conn.ReadMessage(&frame, readLimit)
		if err != nil {
			// Drain after a failure that sent a Close frame.
			timer := time.AfterFunc(closeGrace, func() { _ = conn.Close() })
			_, _ = conn.ReadMessage(nil, -1)
			timer.Stop()
			return err
		}
		if err := conn.WriteMessage(messageType, frame.Bytes()); err != nil {
			return err
		}
	}
}

// dial opens a client connection to url, an http URL.
func dial(ctx context.Context, url string) (*websocket.Conn, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	conn, response, err := websocket.Dial(http.DefaultClient, request, websocket.Options{Compression: true})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return nil, err
	}
	return conn, nil
}

// skipUnlessEnabled skips the test unless AUTOBAHN is set.
func skipUnlessEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("AUTOBAHN") == "" {
		t.Skip("set AUTOBAHN=1 to run the Autobahn suite")
	}
}

func reportsDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("AUTOBAHN_REPORTS")
	if dir == "" {
		dir = "reports"
	}
	dir, err := filepath.Abs(dir)
	assert.Nil(t, err)
	assert.Nil(t, os.MkdirAll(dir, 0o750))
	return dir
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	assert.Nil(t, err)
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	assert.True(t, ok)
	assert.Nil(t, listener.Close())
	return tcpAddr.Port
}

func writeSpec(t *testing.T, spec map[string]any) string {
	t.Helper()
	data, err := json.Marshal(spec)
	assert.Nil(t, err)
	path := filepath.Join(t.TempDir(), "spec.json")
	assert.Nil(t, os.WriteFile(path, data, 0o600))
	return path
}

// runDocker runs the suite's image, logging to the reports directory. With
// wait it returns once the container exits, otherwise it stops the
// container when the test ends.
func runDocker(t *testing.T, reports, name string, wait bool, args ...string) {
	t.Helper()
	logFile, err := os.Create(filepath.Join(reports, name+".log"))
	assert.Nil(t, err)
	t.Cleanup(func() { _ = logFile.Close() })
	// Killing the docker CLI leaves the container running, which a name
	// lets cleanup remove.
	container := fmt.Sprintf("autobahn-%s-%d", name, os.Getpid())
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "rm", "-f", container).Run()
	})
	// The image is amd64 only.
	args = append([]string{
		"run", "--rm", "--name", container, "-e", "PYTHONUNBUFFERED=1", "--platform", "linux/amd64", "-v", reports + ":/reports",
	}, args...)
	cmd := exec.CommandContext(t.Context(), "docker", args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if wait {
		assert.Nil(t, cmd.Run(), assert.Sprintf("see %s", logFile.Name()))
		return
	}
	assert.Nil(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// caseCount asks the fuzzing server for its case count, retrying while the
// container starts.
func caseCount(t *testing.T, baseURL string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		conn, err := dial(ctx, baseURL+"/getCaseCount")
		if err == nil {
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			var buffer bytes.Buffer
			_, err := conn.ReadMessage(&buffer, -1)
			stop()
			_ = conn.Close()
			assert.Nil(t, err)
			count, err := strconv.Atoi(buffer.String())
			assert.Nil(t, err)
			return count
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fuzzing server didn't start: %v", errors.Join(err, ctx.Err()))
		case <-ticker.C:
		}
	}
}

// checkReport fails each case whose behavior isn't a pass.
func checkReport(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if !assert.Nil(t, err) {
		return
	}
	var index map[string]map[string]struct {
		Behavior      string `json:"behavior"`
		BehaviorClose string `json:"behaviorClose"`
	}
	assert.Nil(t, json.Unmarshal(data, &index))
	assert.NotZero(t, len(index[agent]))
	for name, result := range index[agent] {
		if !passed(result.Behavior) || !passed(result.BehaviorClose) {
			t.Errorf("case %s: behavior %s, close behavior %s", name, result.Behavior, result.BehaviorClose)
		}
	}
	if t.Failed() {
		t.Logf("see %s", filepath.Join(filepath.Dir(path), "index.html"))
	}
}

func passed(behavior string) bool {
	switch behavior {
	case "OK", "NON-STRICT", "INFORMATIONAL":
		return true
	default:
		return false
	}
}
