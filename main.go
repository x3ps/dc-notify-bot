// Delta Chat Notify Bot — a webhook-to-DeltaChat forwarder.
//
// Accepts HTTP POST requests on /webhook and delivers the payload as
// Delta Chat messages to a pre-configured list of recipients.
// Recipients can be plain email addresses (unencrypted) or
// OPENPGP4FPR: SecureJoin invite links (end-to-end encrypted).
//
// GET /recipients returns a JSON array of configured recipients with
// their chat IDs and current status ("ready" or "pending").
//
// Requires deltachat-rpc-server to be available in PATH — the bot
// framework (deltabot-cli-go) spawns it as a subprocess for all
// Delta Chat RPC operations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
	"github.com/deltachat-bot/deltabot-cli-go/v2/botcli"
	"github.com/spf13/cobra"
)

// Recipient holds a resolved recipient's address, chat ID, and current
// status ("ready" or "pending" SecureJoin handshake).
type Recipient struct {
	Address string `json:"address"`
	ChatID  uint32 `json:"chat_id"`
	Status  string `json:"status"`
}

// contactToChatId maps inviter contactId → chatId for pending SecureJoin
// handshakes. At most one pending SecureJoin per contactId is allowed;
// duplicates cause startup failure. This map is populated during
// OnBotStart (single-threaded, before the HTTP server starts) and is
// read-only after that, so no mutex is needed.
var contactToChatId = make(map[uint32]uint32)

// pendingChats tracks chatIds whose SecureJoin handshake has not yet
// completed. It is a sync.Map because it is written by the
// SecurejoinJoinerProgress event handler and read by the /webhook HTTP
// handler — both run concurrently on separate goroutines.
var pendingChats sync.Map

// getMaxPayloadBytes reads the optional NOTIFY_BOT_MAX_PAYLOAD_BYTES
// env var and returns the limit used by http.MaxBytesReader to cap
// incoming webhook payloads. Falls back to 1 MiB if unset or invalid.
func getMaxPayloadBytes(logger interface{ Warnf(string, ...any) }) int64 {
	const defaultLimit int64 = 1 << 20 // 1 MiB
	raw := os.Getenv("NOTIFY_BOT_MAX_PAYLOAD_BYTES")
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		logger.Warnf("invalid NOTIFY_BOT_MAX_PAYLOAD_BYTES %q (must be a positive integer); using default %d", raw, defaultLimit)
		return defaultLimit
	}
	return n
}

func main() {
	// botcli.New creates a CLI app with subcommands: "init" (configure
	// a Delta Chat account) and "serve" (start the bot event loop).
	// It manages the deltachat-rpc-server subprocess lifecycle.
	cli := botcli.New("dc-notify-bot")

	// OnBotInit is called after the RPC connection is established but
	// before the event loop starts. An empty callback is required —
	// without it botcli skips bot initialization entirely.
	cli.OnBotInit(func(cli *botcli.BotCli, bot *deltachat.Bot, cmd *cobra.Command, args []string) {})

	// OnBotStart runs inside the "serve" subcommand after the bot's
	// event loop is already running. This is where we resolve
	// recipients, set up SecureJoin tracking, and start the HTTP
	// server.
	cli.OnBotStart(func(cli *botcli.BotCli, bot *deltachat.Bot, cmd *cobra.Command, args []string) {
		// --- Read configuration from environment variables ---
		recipientsEnv := os.Getenv("NOTIFY_BOT_RECIPIENTS")
		if recipientsEnv == "" {
			cli.Logger.Error("NOTIFY_BOT_RECIPIENTS environment variable is not set")
			os.Exit(1)
		}

		listenAddr := os.Getenv("NOTIFY_BOT_LISTEN")
		if listenAddr == "" {
			listenAddr = "127.0.0.1:8080"
		}

		maxPayloadBytes := getMaxPayloadBytes(cli.Logger)

		// --- Find the first configured Delta Chat account ---
		// botcli can manage multiple accounts; we pick the first one
		// that has already been initialized via "dc-notify-bot init".
		accounts, err := bot.Rpc.GetAllAccountIds()
		if err != nil {
			cli.Logger.Errorf("failed to get accounts: %v", err)
			os.Exit(1)
		}
		var accId uint32
		for _, id := range accounts {
			if ok, _ := bot.Rpc.IsConfigured(id); ok {
				accId = id
				break
			}
		}
		if accId == 0 {
			cli.Logger.Error("no configured accounts, run: dc-notify-bot init <email> <password>")
			os.Exit(1)
		}

		// --- Resolve each recipient to a chatId ---
		// For plain emails: CreateContact + CreateChatByContactId (no
		// handshake, contactId=0). For SecureJoin URIs: SecureJoin()
		// starts an async handshake, and we track the contactId so we
		// can detect completion via the event handler.
		var recipients []Recipient
		for _, recipient := range strings.Split(recipientsEnv, ",") {
			recipient = strings.TrimSpace(recipient)
			if recipient == "" {
				continue
			}
			chatId, contactId, err := resolveRecipient(bot.Rpc, accId, recipient)
			if err != nil {
				cli.Logger.Errorf("failed to resolve recipient %q: %v", recipient, err)
				recipients = append(recipients, Recipient{Address: recipient, Status: "error"})
				continue
			}
			recipients = append(recipients, Recipient{Address: recipient, ChatID: chatId})
			if contactId != 0 {
				if existingChat, dup := contactToChatId[contactId]; dup {
					cli.Logger.Errorf(
						"duplicate SecureJoin inviter: contactId %d already has pending chatId %d; "+
							"cannot add recipient %q (chatId %d) — remove the duplicate from recipients",
						contactId, existingChat, recipient, chatId)
					os.Exit(1)
				}
				contactToChatId[contactId] = chatId
				pendingChats.Store(chatId, struct{}{})
				cli.Logger.Infof("SecureJoin pending: recipient=%q chatId=%d contactId=%d", recipient, chatId, contactId)
			}
		}

		// Build chatIds from successfully resolved recipients only.
		var chatIds []uint32
		for _, r := range recipients {
			if r.Status != "error" {
				chatIds = append(chatIds, r.ChatID)
			}
		}

		if len(chatIds) == 0 {
			cli.Logger.Error("no recipients could be resolved")
			os.Exit(1)
		}

		// Build email→chatId lookup for optional per-recipient routing.
		recipientMap := make(map[string]uint32)
		for _, r := range recipients {
			if r.Status != "error" {
				recipientMap[r.Address] = r.ChatID
			}
		}

		// --- SecureJoin completion handler ---
		// Delta Chat emits SecurejoinJoinerProgress events during the
		// handshake. Progress=1000 means the handshake is complete and
		// the chat is ready to receive messages. We remove the chatId
		// from pendingChats so the webhook handler will deliver to it.
		bot.On(&deltachat.EventTypeSecurejoinJoinerProgress{}, func(bot *deltachat.Bot, accId uint32, event deltachat.EventType) {
			ev := event.(*deltachat.EventTypeSecurejoinJoinerProgress)
			chatId, tracked := contactToChatId[ev.ContactId]
			if !tracked {
				return
			}
			cli.Logger.Infof("SecureJoin progress: contactId=%d progress=%d chatId=%d", ev.ContactId, ev.Progress, chatId)
			if ev.Progress == 1000 {
				pendingChats.Delete(chatId)
				cli.Logger.Infof("SecureJoin complete (pending→ready): chatId=%d contactId=%d", chatId, ev.ContactId)
			}
		})

		// --- HTTP server ---
		mux := http.NewServeMux()

		// POST /webhook — accepts application/json or multipart/form-data,
		// sends to all ready chats (or a specific recipient). Chats still
		// in SecureJoin handshake are skipped. Returns 503 with
		// Retry-After if all chats are pending; 500 if all sends fail.
		mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, maxPayloadBytes)
			// ParseMediaType errors are intentionally ignored: if Content-Type
			// is missing or malformed, mediaType is "" and the switch falls
			// through to the default case returning 415.
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))

			var text string
			var recipients []string
			var filePath string
			var cleanup func()

			switch mediaType {
			case "application/json":
				body, err := io.ReadAll(r.Body)
				if err != nil {
					var maxBytesErr *http.MaxBytesError
					if errors.As(err, &maxBytesErr) {
						http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
						return
					}
					http.Error(w, "failed to read body", http.StatusBadRequest)
					return
				}
				var parseErr error
				text, recipients, parseErr = parseJSONPayload(body)
				if parseErr != nil {
					http.Error(w, parseErr.Error(), http.StatusBadRequest)
					return
				}
			case "multipart/form-data":
				var err error
				text, recipients, filePath, cleanup, err = parseMultipartPayload(r, maxPayloadBytes)
				if err != nil {
					var maxBytesErr *http.MaxBytesError
					if errors.As(err, &maxBytesErr) {
						http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
						return
					}
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if cleanup != nil {
					defer cleanup()
				}
			default:
				http.Error(w, "unsupported Content-Type, use application/json or multipart/form-data", http.StatusUnsupportedMediaType)
				return
			}

			// Resolve target chat IDs
			var targetIds []uint32
			if len(recipients) > 0 {
				for _, r := range recipients {
					chatId, ok := recipientMap[r]
					if !ok {
						http.Error(w, fmt.Sprintf("unknown recipient: %q", r), http.StatusBadRequest)
						return
					}
					targetIds = append(targetIds, chatId)
				}
			} else {
				targetIds = chatIds
			}

			// Filter out pending SecureJoin chats
			var readyIds []uint32
			var pendingCount int
			for _, chatId := range targetIds {
				if _, isPending := pendingChats.Load(chatId); isPending {
					cli.Logger.Warnf("skipping chat %d: SecureJoin handshake not yet complete", chatId)
					pendingCount++
				} else {
					readyIds = append(readyIds, chatId)
				}
			}
			if len(readyIds) == 0 {
				w.Header().Set("Retry-After", "10")
				http.Error(w, "all recipients pending SecureJoin handshake", http.StatusServiceUnavailable)
				return
			}

			var errs []string
			for _, chatId := range readyIds {
				msg := deltachat.MessageData{Text: &text}
				if filePath != "" {
					msg.File = &filePath
				}
				_, err := bot.Rpc.SendMsg(accId, chatId, msg)
				if err != nil {
					cli.Logger.Errorf("failed to send to chat %d: %v", chatId, err)
					errs = append(errs, err.Error())
				}
			}

			// Return 500 only when no messages were delivered at all:
			// errs counts send failures, pendingCount counts skipped chats.
			// If their sum equals targetIds, nothing got through.
			if len(errs)+pendingCount == len(targetIds) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok\n")
		})

		// GET /health — simple liveness probe for load balancers / monitoring.
		mux.HandleFunc("/health", healthHandler())

		// GET /recipients — returns configured recipients as JSON with live status.
		mux.HandleFunc("/recipients", recipientsHandler(recipients, &pendingChats))

		// Build the HTTP server with explicit timeouts to prevent
		// slowloris-style attacks from holding connections indefinitely.
		server := &http.Server{
			Addr:         listenAddr,
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		}

		// Run the HTTP server in a background goroutine. If it fails
		// (e.g. port already in use), stop the bot so the process
		// exits instead of silently running without a webhook endpoint.
		cli.Logger.Infof("webhook server listening on http://%v/webhook", listenAddr)
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				cli.Logger.Errorf("HTTP server error: %v", err)
				bot.Stop()
			}
		}()

		// Graceful shutdown: drain in-flight requests on SIGTERM/SIGINT
		// before stopping the bot.
		go func() {
			quit := make(chan os.Signal, 1)
			signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
			<-quit
			cli.Logger.Infof("received shutdown signal, draining HTTP server...")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := server.Shutdown(ctx); err != nil {
				cli.Logger.Errorf("HTTP server shutdown error: %v", err)
			}
			bot.Stop()
		}()
	})

	if err := cli.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// resolveRecipient turns a recipient string into a chatId (and
// optionally a contactId for pending SecureJoin tracking).
//
// Returns (chatId, contactId, error):
//   - Plain email: CreateContact → CreateChatByContactId.
//     contactId=0 (no handshake needed).
//   - SecureJoin URI (QrAskVerify*/QrAskJoinBroadcast): SecureJoin()
//     starts an async handshake. contactId!=0 so the caller can track
//     completion via SecurejoinJoinerProgress events.
//   - Already-verified URI (QrFprOk) or plain address URI (QrAddr):
//     CreateChatByContactId directly, contactId=0.
//   - Login/Account URIs: rejected with a descriptive error.
func resolveRecipient(rpc *deltachat.Rpc, accId uint32, recipient string) (uint32, uint32, error) {
	// Plain email address — no SecureJoin, no encryption handshake.
	if !looksLikeURI(recipient) {
		contactId, err := rpc.CreateContact(accId, recipient, nil)
		if err != nil {
			return 0, 0, err
		}
		chatId, err := rpc.CreateChatByContactId(accId, contactId)
		if err != nil {
			return 0, 0, err
		}
		return chatId, 0, nil
	}

	// URI path — ask Delta Chat to classify the QR/invite link.
	qr, err := rpc.CheckQr(accId, recipient)
	if err != nil {
		return 0, 0, fmt.Errorf("CheckQr failed: %w", err)
	}

	// SecureJoin QR types require an async handshake. SecureJoin()
	// returns a chatId immediately, but the chat is not usable until
	// the handshake finishes (Progress=1000).
	if isSecureJoinQr(qr) {
		chatId, err := rpc.SecureJoin(accId, recipient)
		if err != nil {
			return 0, 0, err
		}
		var contactId uint32
		switch q := qr.(type) {
		case *deltachat.QrAskVerifyContact:
			contactId = q.ContactId
		case *deltachat.QrAskVerifyGroup:
			contactId = q.ContactId
		case *deltachat.QrAskJoinBroadcast:
			contactId = q.ContactId
		}
		return chatId, contactId, nil
	}

	// Non-SecureJoin QR types — either already verified or plain address.
	switch q := qr.(type) {
	case *deltachat.QrFprOk:
		// Contact fingerprint already verified (e.g. SecureJoin completed in a previous run).
		// No handshake needed — just create/get the chat.
		chatId, err := rpc.CreateChatByContactId(accId, q.ContactId)
		if err != nil {
			return 0, 0, err
		}
		return chatId, 0, nil
	case *deltachat.QrAddr:
		chatId, err := rpc.CreateChatByContactId(accId, q.ContactId)
		if err != nil {
			return 0, 0, err
		}
		return chatId, 0, nil
	case *deltachat.QrLogin:
		return 0, 0, fmt.Errorf("dclogin: is an account-login link, not a contact invite; use OPENPGP4FPR: links for SecureJoin")
	case *deltachat.QrAccount:
		return 0, 0, fmt.Errorf("dcaccount: is an account-setup link, not a contact invite; use OPENPGP4FPR: links for SecureJoin")
	default:
		return 0, 0, fmt.Errorf("unsupported QR code type %T; only OPENPGP4FPR contact/group invite links are supported", qr)
	}
}

// looksLikeURI reports whether s looks like a URI rather than a bare
// email address. The heuristic: if a colon appears before the first @
// (or there is no @ at all), it is treated as a scheme prefix
// (e.g. "OPENPGP4FPR:…", "dclogin:…").
func looksLikeURI(s string) bool {
	at := strings.Index(s, "@")
	col := strings.Index(s, ":")
	return col >= 0 && (at < 0 || col < at)
}

// isSecureJoinQr reports whether qr is a QR type that requires a
// SecureJoin handshake (async key-verification protocol). These are:
// QrAskVerifyContact (1:1 verified contact), QrAskVerifyGroup
// (verified group invite), and QrAskJoinBroadcast (broadcast invite).
func isSecureJoinQr(qr deltachat.Qr) bool {
	switch qr.(type) {
	case *deltachat.QrAskVerifyContact,
		*deltachat.QrAskVerifyGroup,
		*deltachat.QrAskJoinBroadcast:
		return true
	}
	return false
}

// healthHandler returns an http.HandlerFunc that responds to GET /health
// with a plain-text "ok". Any other method returns 405.
func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(w, "ok\n")
	}
}

// recipientsHandler returns an http.HandlerFunc that serves the
// configured recipients as a JSON array. Each entry includes the
// live status derived from pendingChats.
func recipientsHandler(recipients []Recipient, pendingChats *sync.Map) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result := make([]Recipient, len(recipients))
		for i, rec := range recipients {
			result[i] = rec
			if rec.Status == "error" {
				continue
			}
			if _, isPending := pendingChats.Load(rec.ChatID); isPending {
				result[i].Status = "pending"
			} else {
				result[i].Status = "ready"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// webhookPayload is the expected JSON body for POST /webhook.
type webhookPayload struct {
	Text       string   `json:"text"`
	Recipient  string   `json:"recipient,omitempty"`
	Recipients []string `json:"recipients,omitempty"`
}

// parseJSONPayload extracts the text and optional recipients from a
// Slack-compatible JSON payload. Both "recipient" (singular string)
// and "recipients" (array) are accepted and merged. Returns an error
// if the JSON is invalid or the "text" field is missing/empty.
func parseJSONPayload(body []byte) (text string, recipients []string, err error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", nil, fmt.Errorf("invalid JSON: %w", err)
	}
	text = strings.TrimSpace(p.Text)
	if text == "" {
		return "", nil, fmt.Errorf("missing or empty \"text\" field")
	}
	recipients = mergeRecipients(p.Recipient, p.Recipients)
	return text, recipients, nil
}

// mergeRecipients combines a singular recipient and a recipients list,
// trims whitespace, and deduplicates.
func mergeRecipients(singular string, plural []string) []string {
	seen := make(map[string]struct{})
	var result []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		result = append(result, s)
	}
	add(singular)
	for _, r := range plural {
		add(r)
	}
	return result
}

// parseMultipartPayload extracts text, recipients, and an optional
// file upload from a multipart/form-data request. At least one of
// "text" or "file" must be present. Multiple "recipient" form fields
// are supported. The caller must call cleanup() (if non-nil) to
// remove any temporary file.
func parseMultipartPayload(r *http.Request, maxBytes int64) (text string, recipients []string, filePath string, cleanup func(), err error) {
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		return "", nil, "", nil, fmt.Errorf("invalid multipart data: %w", err)
	}

	text = strings.TrimSpace(r.FormValue("text"))
	recipients = mergeRecipients("", r.MultipartForm.Value["recipient"])

	file, header, fileErr := r.FormFile("file")
	if fileErr != nil && !errors.Is(fileErr, http.ErrMissingFile) {
		return "", nil, "", nil, fmt.Errorf("invalid file field: %w", fileErr)
	}

	if text == "" && file == nil {
		return "", nil, "", nil, fmt.Errorf("missing \"text\" and \"file\" fields")
	}

	if file != nil {
		defer file.Close()
		ext := filepath.Ext(header.Filename)
		tmp, tmpErr := os.CreateTemp("", "dc-notify-*"+ext)
		if tmpErr != nil {
			return "", nil, "", nil, fmt.Errorf("failed to create temp file: %w", tmpErr)
		}
		if _, cpErr := io.Copy(tmp, file); cpErr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", nil, "", nil, fmt.Errorf("failed to write temp file: %w", cpErr)
		}
		tmp.Close()
		filePath = tmp.Name()
		cleanup = func() { os.Remove(filePath) }
	}

	if text == "" {
		// Delta Chat requires non-empty message text. For file-only
		// uploads (no "text" field), use a placeholder — the attachment
		// is the real content.
		text = "(empty notification)"
	}

	return text, recipients, filePath, cleanup, nil
}
