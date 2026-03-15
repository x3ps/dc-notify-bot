// Tests for pure helper functions (no network, no RPC, no Delta Chat
// account needed). Each test is self-contained and deterministic.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

type testLogger struct {
	warnings []string
}

func (l *testLogger) Warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func TestIsSecureJoinQr(t *testing.T) {
	tests := []struct {
		name string
		qr   deltachat.Qr
		want bool
	}{
		{"AskVerifyContact", &deltachat.QrAskVerifyContact{}, true},
		{"AskVerifyGroup", &deltachat.QrAskVerifyGroup{}, true},
		{"AskJoinBroadcast", &deltachat.QrAskJoinBroadcast{}, true},
		{"Login", &deltachat.QrLogin{}, false},
		{"Account", &deltachat.QrAccount{}, false},
		{"FprOk", &deltachat.QrFprOk{}, false},
		{"Addr", &deltachat.QrAddr{}, false},
		{"Url", &deltachat.QrUrl{}, false},
		{"Text", &deltachat.QrText{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSecureJoinQr(tt.qr); got != tt.want {
				t.Errorf("isSecureJoinQr(%T) = %v, want %v", tt.qr, got, tt.want)
			}
		})
	}
}

func TestLooksLikeURI(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"openpgp4fpr:AABB1234#a=alice@example.com", true},
		{"OPENPGP4FPR:AABB1234#a=alice@example.com", true},
		{"dclogin:foo@example.com", true},
		{"dcaccount:foo", true},
		{"bob@example.com", false},
		{"alice@host:8080", false}, // colon is after @
		{"plaintext", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := looksLikeURI(tt.input); got != tt.want {
				t.Errorf("looksLikeURI(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestContactToChatIdDuplicateDetection(t *testing.T) {
	// Simulate the duplicate-detection logic from OnBotStart.
	m := make(map[uint32]uint32)
	contactId := uint32(42)
	firstChatId := uint32(100)

	// First registration should succeed.
	if _, dup := m[contactId]; dup {
		t.Fatal("unexpected duplicate on first insert")
	}
	m[contactId] = firstChatId

	// Second registration with the same contactId must be detected as duplicate.
	secondChatId := uint32(200)
	existingChat, dup := m[contactId]
	if !dup {
		t.Fatal("expected duplicate detection on second insert")
	}
	if existingChat != firstChatId {
		t.Errorf("existing chatId = %d, want %d", existingChat, firstChatId)
	}
	// Verify the map was not overwritten.
	if m[contactId] != firstChatId {
		t.Errorf("map value = %d after dup detection, want %d", m[contactId], firstChatId)
	}
	_ = secondChatId
}

func TestParseJSONPayload(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		wantText       string
		wantRecipients []string
		wantErr        bool
	}{
		{"valid text", `{"text":"Hello"}`, "Hello", nil, false},
		{"with recipient", `{"text":"Hi","recipient":"a@b.com"}`, "Hi", []string{"a@b.com"}, false},
		{"recipients array", `{"text":"hi","recipients":["a@b.com","c@d.com"]}`, "hi", []string{"a@b.com", "c@d.com"}, false},
		{"both fields merged", `{"text":"hi","recipient":"a@b.com","recipients":["c@d.com"]}`, "hi", []string{"a@b.com", "c@d.com"}, false},
		{"duplicate dedup", `{"text":"hi","recipient":"a@b.com","recipients":["a@b.com"]}`, "hi", []string{"a@b.com"}, false},
		{"whitespace trimmed", `{"text":"  hello  "}`, "hello", nil, false},
		{"markdown preserved", `{"text":"**bold** _italic_"}`, "**bold** _italic_", nil, false},
		{"extra fields ignored", `{"text":"hi","channel":"#foo"}`, "hi", nil, false},
		{"newlines preserved", `{"text":"line1\nline2"}`, "line1\nline2", nil, false},
		{"unicode preserved", `{"text":"Hello 🌍"}`, "Hello 🌍", nil, false},
		{"empty text", `{"text":""}`, "", nil, true},
		{"whitespace-only text", `{"text":"   "}`, "", nil, true},
		{"missing text", `{"recipient":"a@b.com"}`, "", nil, true},
		{"empty object", `{}`, "", nil, true},
		{"invalid JSON", `{broken`, "", nil, true},
		{"JSON array", `[1,2,3]`, "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, recipients, err := parseJSONPayload([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseJSONPayload(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if len(recipients) != len(tt.wantRecipients) {
				t.Fatalf("recipients = %v, want %v", recipients, tt.wantRecipients)
			}
			for i := range recipients {
				if recipients[i] != tt.wantRecipients[i] {
					t.Errorf("recipients[%d] = %q, want %q", i, recipients[i], tt.wantRecipients[i])
				}
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{"/etc/shadow", "shadow"},
		{"C:\\temp\\a.txt", "a.txt"},
		{"", "attachment"},
		{".", "attachment"},
		{"..", "attachment"},
		{"  spaces.txt  ", "spaces.txt"},
		{"../foo.png", "foo.png"},
	}
	for _, tc := range cases {
		got := sanitizeFilename(tc.input)
		if got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseMultipartPayload(t *testing.T) {
	// Helper to build a multipart request. fields is a list of
	// key-value pairs to support repeated keys (e.g. multiple recipients).
	makeRequest := func(fields [][2]string, fileName string, fileContent []byte) *http.Request {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for _, kv := range fields {
			w.WriteField(kv[0], kv[1])
		}
		if fileName != "" {
			part, _ := w.CreateFormFile("file", fileName)
			part.Write(fileContent)
		}
		w.Close()
		req := httptest.NewRequest(http.MethodPost, "/webhook", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		return req
	}

	t.Run("text only", func(t *testing.T) {
		req := makeRequest([][2]string{{"text", "hello"}}, "", nil)
		text, recipients, filePath, cleanup, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "hello" {
			t.Errorf("text = %q, want %q", text, "hello")
		}
		if len(recipients) != 0 {
			t.Errorf("recipients = %v, want empty", recipients)
		}
		if filePath != "" {
			t.Errorf("filePath = %q, want empty", filePath)
		}
		if cleanup != nil {
			t.Error("cleanup should be nil when no file")
		}
	})

	t.Run("file only", func(t *testing.T) {
		req := makeRequest(nil, "test.png", []byte("fake-png-data"))
		text, _, filePath, cleanup, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "(empty notification)" {
			t.Errorf("text = %q, want %q", text, "(empty notification)")
		}
		if filePath == "" {
			t.Fatal("filePath should not be empty")
		}
		if filepath.Base(filePath) != "test.png" {
			t.Errorf("filePath base = %q, want %q", filepath.Base(filePath), "test.png")
		}
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Fatalf("failed to read temp file: %v", readErr)
		}
		if string(data) != "fake-png-data" {
			t.Errorf("file content = %q, want %q", data, "fake-png-data")
		}
		tmpDir := filepath.Dir(filePath)
		if cleanup != nil {
			cleanup()
		}
		if _, err := os.Stat(filePath); !os.IsNotExist(err) {
			t.Error("temp file should have been removed by cleanup")
		}
		if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
			t.Error("temp dir should have been removed by cleanup")
		}
	})

	t.Run("text and file", func(t *testing.T) {
		req := makeRequest([][2]string{{"text", "hi"}}, "test.png", []byte("data"))
		text, _, filePath, cleanup, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "hi" {
			t.Errorf("text = %q, want %q", text, "hi")
		}
		if filePath == "" {
			t.Error("filePath should not be empty")
		}
		if cleanup != nil {
			cleanup()
		}
	})

	t.Run("file preserves original name", func(t *testing.T) {
		req := makeRequest([][2]string{{"text", "hi"}}, "report.pdf", []byte("pdf-data"))
		_, _, filePath, cleanup, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if filepath.Base(filePath) != "report.pdf" {
			t.Errorf("filePath base = %q, want %q", filepath.Base(filePath), "report.pdf")
		}
		if cleanup != nil {
			cleanup()
		}
	})

	t.Run("text and recipient", func(t *testing.T) {
		req := makeRequest([][2]string{{"text", "hi"}, {"recipient", "a@b.com"}}, "", nil)
		text, recipients, _, _, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "hi" {
			t.Errorf("text = %q, want %q", text, "hi")
		}
		if len(recipients) != 1 || recipients[0] != "a@b.com" {
			t.Errorf("recipients = %v, want [a@b.com]", recipients)
		}
	})

	t.Run("multiple recipients", func(t *testing.T) {
		req := makeRequest([][2]string{
			{"text", "hi"},
			{"recipient", "a@b.com"},
			{"recipient", "c@d.com"},
		}, "", nil)
		text, recipients, _, _, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if text != "hi" {
			t.Errorf("text = %q, want %q", text, "hi")
		}
		if len(recipients) != 2 || recipients[0] != "a@b.com" || recipients[1] != "c@d.com" {
			t.Errorf("recipients = %v, want [a@b.com c@d.com]", recipients)
		}
	})

	t.Run("neither text nor file", func(t *testing.T) {
		req := makeRequest(nil, "", nil)
		_, _, _, _, err := parseMultipartPayload(req, 1<<20)
		if err == nil {
			t.Fatal("expected error for empty form")
		}
	})

	t.Run("empty text no file", func(t *testing.T) {
		req := makeRequest([][2]string{{"text", ""}}, "", nil)
		_, _, _, _, err := parseMultipartPayload(req, 1<<20)
		if err == nil {
			t.Fatal("expected error for empty text without file")
		}
	})

	t.Run("duplicate recipients deduplicated", func(t *testing.T) {
		req := makeRequest([][2]string{
			{"text", "hi"},
			{"recipient", "a@b.com"},
			{"recipient", "a@b.com"},
		}, "", nil)
		_, recipients, _, _, err := parseMultipartPayload(req, 1<<20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(recipients) != 1 || recipients[0] != "a@b.com" {
			t.Errorf("recipients = %v, want [a@b.com] (deduplicated)", recipients)
		}
	})

	t.Run("payload too large returns MaxBytesError", func(t *testing.T) {
		req := makeRequest([][2]string{{"text", "this message is definitely longer than ten bytes"}}, "", nil)
		rec := httptest.NewRecorder()
		req.Body = http.MaxBytesReader(rec, req.Body, 10)
		_, _, _, _, err := parseMultipartPayload(req, 10)
		if err == nil {
			t.Fatal("expected error for oversized payload")
		}
		var maxBytesErr *http.MaxBytesError
		if !errors.As(err, &maxBytesErr) {
			t.Errorf("expected MaxBytesError to be propagated, got %T: %v", err, err)
		}
	})
}

func TestHealthHandler(t *testing.T) {
	handler := healthHandler()

	t.Run("GET returns 200 ok", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if body := rec.Body.String(); body != "ok\n" {
			t.Errorf("body = %q, want %q", body, "ok\n")
		}
	})

	t.Run("POST returns 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/health", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("DELETE returns 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/health", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})
}

func TestRecipientsHandler(t *testing.T) {
	recipients := []Recipient{
		{Address: "alice@example.com", ChatID: 1},
		{Address: "bob@example.com", ChatID: 2},
		{Address: "carol@example.com", ChatID: 3},
	}
	var pending sync.Map
	pending.Store(uint32(2), struct{}{}) // bob is pending

	handler := recipientsHandler(recipients, &pending)

	t.Run("GET returns JSON with status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/recipients", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var got []Recipient
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("failed to decode JSON: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}

		want := []struct {
			addr   string
			chatID uint32
			status string
		}{
			{"alice@example.com", 1, "ready"},
			{"bob@example.com", 2, "pending"},
			{"carol@example.com", 3, "ready"},
		}
		for i, w := range want {
			if got[i].Address != w.addr || got[i].ChatID != w.chatID || got[i].Status != w.status {
				t.Errorf("got[%d] = %+v, want {%s %d %s}", i, got[i], w.addr, w.chatID, w.status)
			}
		}
	})

	t.Run("POST returns 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/recipients", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})
}

func TestRecipientsHandlerEmpty(t *testing.T) {
	var pending sync.Map
	handler := recipientsHandler(nil, &pending)

	req := httptest.NewRequest(http.MethodGet, "/recipients", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var got []Recipient
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestGetMaxPayloadBytes(t *testing.T) {
	const defaultLimit int64 = 1 << 20

	tests := []struct {
		name     string
		envValue string
		want     int64
		warnLogs int
	}{
		{"unset returns default", "", defaultLimit, 0},
		{"valid value", "2048", 2048, 0},
		{"zero is invalid", "0", defaultLimit, 1},
		{"negative is invalid", "-1", defaultLimit, 1},
		{"non-numeric is invalid", "abc", defaultLimit, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				os.Setenv("NOTIFY_BOT_MAX_PAYLOAD_BYTES", tt.envValue)
				t.Cleanup(func() { os.Unsetenv("NOTIFY_BOT_MAX_PAYLOAD_BYTES") })
			} else {
				os.Unsetenv("NOTIFY_BOT_MAX_PAYLOAD_BYTES")
			}
			logger := &testLogger{}
			got := getMaxPayloadBytes(logger)
			if got != tt.want {
				t.Errorf("getMaxPayloadBytes() = %d, want %d", got, tt.want)
			}
			if len(logger.warnings) != tt.warnLogs {
				t.Errorf("got %d warnings, want %d", len(logger.warnings), tt.warnLogs)
			}
		})
	}
}
