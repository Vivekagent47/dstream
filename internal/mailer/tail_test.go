package mailer

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/dqueue"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// uniqueQueue is a real dqueue under a prefix private to this test, removed on
// cleanup.
func uniqueQueue(t *testing.T) (*dqueue.Client, *redis.Client, string) {
	t.Helper()
	_, rdb := testQueue(t) // dials DSTREAM_REDIS_ADDR; skips only when Redis is unreachable
	pfx := "mailtail-" + uuid.NewString()
	t.Cleanup(func() {
		if keys, _ := rdb.Keys(context.Background(), pfx+":*").Result(); len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	return dqueue.NewClient(rdb).WithPrefix(pfx), rdb, pfx
}

// rejectingSMTP is a real TCP listener that greets every connection with a
// permanent 554 and hangs up, so a real go-mail client gets a real SMTP refusal.
func rejectingSMTP(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("554 5.3.2 mailbox service unavailable\r\n"))
			_ = c.Close()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func TestSMTPSenderSurfacesServerRefusal(t *testing.T) {
	host, port := rejectingSMTP(t)
	s, err := NewSender(config.SMTPConfig{Host: host, Port: port, From: "dstream@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = s.Send(ctx, Message{To: "a@example.test", Subject: "s", HTML: "<p>h</p>", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "554") {
		t.Fatalf("Send err = %v, want the server's 554 refusal", err)
	}
}

func TestSMTPSenderRejectsBadAddressesBeforeDialing(t *testing.T) {
	// Port 1 is never dialed: both failures happen while building the message.
	cases := []struct {
		name, from, to, wantPrefix string
	}{
		{"bad from", "not an address", "a@example.test", "mailer: from:"},
		{"bad to", "dstream@example.test", "not an address", "mailer: to:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSender(config.SMTPConfig{Host: "127.0.0.1", Port: 1, From: tc.from})
			if err != nil {
				t.Fatal(err)
			}
			err = s.Send(context.Background(), Message{To: tc.to, Subject: "s", HTML: "h", Text: "t"})
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("Send err = %v, want prefix %q", err, tc.wantPrefix)
			}
		})
	}
}

// The handler against the REAL smtpSender: the server refuses, so the task is
// rescheduled with attempt+1 and the lease is released.
func TestEmailHandlerRealSenderRefusedReschedules(t *testing.T) {
	q, rdb, pfx := uniqueQueue(t)
	ctx := context.Background()
	host, port := rejectingSMTP(t)
	snd, err := NewSender(config.SMTPConfig{Host: host, Port: port, From: "dstream@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, q, "magic_link", "a@example.test", map[string]any{"Link": "https://x/y"}, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	raw, p := leaseOne(t, q)
	var logs syncBuf
	h := EmailHandler{Sender: snd, Log: slogTo(&logs)}
	if err := h.Process(ctx, p, raw, q); err != nil {
		t.Fatal(err)
	}
	members, _ := rdb.ZRange(ctx, pfx+":scheduled", 0, -1).Result()
	if len(members) != 1 {
		t.Fatalf("scheduled = %d, want 1", len(members))
	}
	if !strings.Contains(members[0], `"attempt":1`) {
		t.Fatalf("scheduled payload %s, want attempt 1", members[0])
	}
	if n, _ := rdb.ZCard(ctx, pfx+":processing").Result(); n != 0 {
		t.Fatalf("processing = %d, want 0 (leased member acked after scheduling)", n)
	}
	if !strings.Contains(logs.String(), "554") {
		t.Errorf("send failure not logged with its cause: %s", logs.String())
	}
}

// If rescheduling itself fails the handler returns that error and leaves the
// task leased, so the recoverer re-injects it rather than the email vanishing.
func TestEmailHandlerScheduleFailureLeavesTaskLeased(t *testing.T) {
	q, rdb, pfx := uniqueQueue(t)
	ctx := context.Background()
	if err := Enqueue(ctx, q, "magic_link", "a@example.test", map[string]any{"Link": "x"}, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	raw, p := leaseOne(t, q)
	host, port := rejectingSMTP(t)
	snd, err := NewSender(config.SMTPConfig{Host: host, Port: port, From: "dstream@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel() // send fails, and so does the reschedule: real redis refuses a cancelled context
	err = EmailHandler{Sender: snd, Log: discardLog()}.Process(cctx, p, raw, q)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Process err = %v, want context canceled from the failed reschedule", err)
	}
	if n, _ := rdb.ZCard(ctx, pfx+":processing").Result(); n != 1 {
		t.Fatalf("processing = %d, want 1 (still leased for the recoverer)", n)
	}
	if n, _ := rdb.ZCard(ctx, pfx+":scheduled").Result(); n != 0 {
		t.Fatalf("scheduled = %d, want 0", n)
	}
}

func TestEmailHandlerBadTaskDeadLetters(t *testing.T) {
	cases := []struct {
		name string
		data string
		log  string
	}{
		{"undecodable task", "{not json", "bad email task"},
		{"unknown template", `{"template":"nope","to":"a@example.test"}`, "render failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, rdb, pfx := uniqueQueue(t)
			ctx := context.Background()
			if err := q.Enqueue(ctx, dqueue.Payload{Kind: emailKind, OrgID: uuid.Nil, Data: []byte(tc.data)}); err != nil {
				t.Fatal(err)
			}
			raw, p := leaseOne(t, q)
			fs := &fakeSender{}
			var logs syncBuf
			if err := (EmailHandler{Sender: fs, Log: slogTo(&logs)}).Process(ctx, p, raw, q); err != nil {
				t.Fatal(err)
			}
			if fs.calls != 0 {
				t.Fatalf("sender called %d times for a task that cannot be rendered", fs.calls)
			}
			if n, _ := rdb.LLen(ctx, pfx+":dead").Result(); n != 1 {
				t.Fatalf("dead = %d, want 1", n)
			}
			if n, _ := rdb.ZCard(ctx, pfx+":processing").Result(); n != 0 {
				t.Fatalf("processing = %d, want 0", n)
			}
			if !strings.Contains(logs.String(), tc.log) {
				t.Errorf("log %q missing %q", logs.String(), tc.log)
			}
		})
	}
}

// With no SMTP configured the link is logged only in dev; in prod it must never
// reach the log (it is a live sign-in URL). Either way the task is acked.
func TestEmailHandlerNoSenderLogsLinkOnlyInDev(t *testing.T) {
	const link = "https://app.example.test/verify?token=SECRET123"
	for _, dev := range []bool{true, false} {
		t.Run("dev="+strconv.FormatBool(dev), func(t *testing.T) {
			q, rdb, pfx := uniqueQueue(t)
			ctx := context.Background()
			if err := Enqueue(ctx, q, "magic_link", "a@example.test", map[string]any{"Link": link}, uuid.Nil); err != nil {
				t.Fatal(err)
			}
			raw, p := leaseOne(t, q)
			var logs syncBuf
			if err := (EmailHandler{Log: slogTo(&logs), DevMode: dev}).Process(ctx, p, raw, q); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if got := strings.Contains(out, "SECRET123"); got != dev {
				t.Fatalf("link in log = %v, want %v; log: %s", got, dev, out)
			}
			if dev && !strings.Contains(out, "dev: SMTP unconfigured") {
				t.Errorf("dev fallback not announced: %s", out)
			}
			if !dev && !strings.Contains(out, "SMTP unconfigured") {
				t.Errorf("prod no-SMTP not reported: %s", out)
			}
			if n, _ := rdb.ZCard(ctx, pfx+":processing").Result(); n != 0 {
				t.Fatalf("processing = %d, want acked", n)
			}
		})
	}
}

// A template var that cannot be printed fails the render. html/template prints
// it without complaint, so it is the text rendering that fails, and says so.
func TestRenderFailureNamesTheFormat(t *testing.T) {
	_, err := Render("magic_link", map[string]any{"Link": func() {}})
	if err == nil || !strings.HasPrefix(err.Error(), `mailer: render text "magic_link"`) {
		t.Fatalf("Render err = %v, want a render error", err)
	}
}

// A task whose vars cannot be JSON-encoded is refused up front, before the
// queue is touched.
func TestEnqueueUnencodableVars(t *testing.T) {
	bad := map[string]any{"Link": func() {}}
	if _, err := buildEmailTask("magic_link", "a@example.test", bad, uuid.Nil); err == nil {
		t.Fatal("buildEmailTask accepted an unencodable var")
	}
	q, rdb, pfx := uniqueQueue(t)
	if err := Enqueue(context.Background(), q, "magic_link", "a@example.test", bad, uuid.Nil); err == nil {
		t.Fatal("Enqueue accepted an unencodable var")
	}
	if keys, _ := rdb.Keys(context.Background(), pfx+":*").Result(); len(keys) != 0 {
		t.Fatalf("queue touched despite the encode failure: %v", keys)
	}
}

func TestEnqueueNilQueueIsNoop(t *testing.T) {
	if err := Enqueue(context.Background(), nil, "magic_link", "a@example.test", nil, uuid.Nil); err != nil {
		t.Fatalf("Enqueue with no queue = %v, want nil (dev fail-open)", err)
	}
}

func TestNewSenderPortHandling(t *testing.T) {
	s, err := NewSender(config.SMTPConfig{Host: "smtp.example.test", From: "f@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*smtpSender).client.ServerAddr(); got != "smtp.example.test:587" {
		t.Errorf("default server addr = %q, want smtp.example.test:587", got)
	}
	s, err = NewSender(config.SMTPConfig{Host: "smtp.example.test", Port: 2525, From: "f@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.(*smtpSender).client.ServerAddr(); got != "smtp.example.test:2525" {
		t.Errorf("explicit server addr = %q, want smtp.example.test:2525", got)
	}
	for _, port := range []int{70000, -1} {
		if s, err := NewSender(config.SMTPConfig{Host: "smtp.example.test", Port: port}); err == nil || s != nil || !strings.HasPrefix(err.Error(), "mailer: new client:") {
			t.Errorf("port %d: NewSender = (%v, %v), want a 'mailer: new client' error", port, s, err)
		}
	}
}
