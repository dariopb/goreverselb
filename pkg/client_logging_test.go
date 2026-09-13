package tunnel

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
)

type tokenLogHook struct {
	token         string
	mu            sync.Mutex
	found         bool
	debugMessages map[string]int
	debugEntries  map[string][]log.Fields
}

func (h *tokenLogHook) Levels() []log.Level { return log.AllLevels }

func (h *tokenLogHook) Fire(entry *log.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.found = h.found || strings.Contains(entry.Message, h.token) || strings.Contains(fmt.Sprint(entry.Data), h.token)
	if entry.Level == log.DebugLevel {
		if h.debugMessages == nil {
			h.debugMessages = make(map[string]int)
		}
		h.debugMessages[entry.Message]++
		if h.debugEntries == nil {
			h.debugEntries = make(map[string][]log.Fields)
		}
		fields := make(log.Fields, len(entry.Data))
		for key, value := range entry.Data {
			fields[key] = value
		}
		h.debugEntries[entry.Message] = append(h.debugEntries[entry.Message], fields)
	}
	return nil
}

func captureClientLogs(t *testing.T, token string, level log.Level) *tokenLogHook {
	t.Helper()
	hook := &tokenLogHook{token: token}
	logger := log.StandardLogger()
	hooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	logger.AddHook(hook)
	logger.SetLevel(level)
	t.Cleanup(func() { logger.ReplaceHooks(hooks); logger.SetLevel(previousLevel) })
	return hook
}

func assertClientDebugLogs(t *testing.T, hook *tokenLogHook, messages ...string) {
	t.Helper()
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.found {
		t.Error("client exposed its token in logs")
	}
	for _, message := range messages {
		if hook.debugMessages[message] == 0 {
			t.Errorf("missing debug diagnostic %q", message)
		}
	}
}

func TestClientDebugLogging(t *testing.T) {
	for _, level := range []log.Level{log.InfoLevel, log.DebugLevel} {
		t.Run(level.String(), func(t *testing.T) {
			const token = "unique-lifecycle-token-not-for-logs"
			hook := captureClientLogs(t, token, level)
			cert, _ := clientTestCertificate(t)
			endpoint, ready := mockClientControl(t, cert, func(_ *yamux.Session, control net.Conn, td TunnelData) error {
				return json.NewEncoder(control).Encode(TunnelDataResponse{
					ServiceName: td.ServiceName, FrontendPort: 12345, PublicationMode: "dynamic",
				})
			})
			tc, err := NewMuxTunnelClient(endpoint, TunnelData{ServiceName: "service", Token: token})
			if err != nil {
				t.Fatal(err)
			}
			defer tc.Close()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("registration timed out")
			}
			deadline := time.Now().Add(3 * time.Second)
			for tc.FrontendPort() != 12345 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if tc.FrontendPort() != 12345 {
				t.Fatal("client did not receive registration response")
			}
			tc.Close()
			if level == log.DebugLevel {
				assertClientDebugLogs(t, hook,
					"Starting tunnel client", "Connecting to tunnel control plane",
					"Control TLS connection established", "Yamux control stream opened",
					"Registering tunnel", "Tunnel registration accepted", "Stopping tunnel client")
			} else {
				hook.mu.Lock()
				defer hook.mu.Unlock()
				if len(hook.debugMessages) != 0 {
					t.Error("debug diagnostics were emitted at info level")
				}
				if hook.found {
					t.Error("client exposed its token in logs")
				}
			}
		})
	}
}

func TestClientLogsDoNotExposeToken(t *testing.T) {
	hook := &tokenLogHook{token: "unique-test-secret-not-for-logs"}
	logger := log.StandardLogger()
	hooks := logger.ReplaceHooks(make(log.LevelHooks))
	level := logger.GetLevel()
	logger.AddHook(hook)
	logger.SetLevel(log.DebugLevel)
	defer func() { logger.ReplaceHooks(hooks); logger.SetLevel(level) }()
	cert, _ := clientTestCertificate(t)
	endpoint, ready := mockClientControl(t, cert, nil)
	tc, err := NewMuxTunnelClient(endpoint, TunnelData{ServiceName: "service", Token: hook.token})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("registration timed out")
	}
	tc.Close()
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.found {
		t.Fatal("client exposed its token in logs")
	}
}
