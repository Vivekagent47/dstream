package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"
)

func cliCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cli",
		Short: "Local development CLI (tunnel, replay, listen)",
	}
	c.AddCommand(listenCmd(), fixturesCmd(), replayCmd(), importCmd(), scenarioCmd())
	return c
}

func listenCmd() *cobra.Command {
	var (
		sourceFlag  string
		forwardFlag string
		baseURLFlag string
	)
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Forward events from a source to a local URL via WebSocket tunnel",
		RunE: func(_ *cobra.Command, _ []string) error {
			apiKey := os.Getenv("DSTREAM_API_KEY")
			if apiKey == "" {
				return errors.New("DSTREAM_API_KEY env var required")
			}
			base := baseURLFlag
			if base == "" {
				if env := os.Getenv("DSTREAM_API_URL"); env != "" {
					base = env
				} else {
					base = "http://localhost:8080"
				}
			}
			base = strings.TrimRight(base, "/")

			sourceID, err := resolveSource(base, apiKey, sourceFlag)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "→ tunneling source %s to %s\n", sourceID, forwardFlag)

			wsURL, err := buildWSURL(base, sourceID)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sigCh
				fmt.Fprintln(os.Stderr, "\nshutting down")
				cancel()
			}()

			return runTunnel(ctx, wsURL, apiKey, forwardFlag)
		},
	}
	cmd.Flags().StringVar(&sourceFlag, "source", "", "Source ID or name to listen on (required)")
	cmd.Flags().StringVar(&forwardFlag, "forward", "http://localhost:3000", "Local URL to forward events to")
	cmd.Flags().StringVar(&baseURLFlag, "url", "", "dstream API base URL (default: $DSTREAM_API_URL or http://localhost:8080)")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}

type cliSource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func resolveSource(base, apiKey, ref string) (string, error) {
	if isUUID(ref) {
		return ref, nil
	}
	req, _ := http.NewRequest("GET", base+"/api/cli/sources", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("list sources: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("list sources: %d %s", resp.StatusCode, b)
	}
	var sources []cliSource
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		return "", err
	}
	for _, s := range sources {
		if s.Name == ref {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no source named %q in your project", ref)
}

func isUUID(s string) bool {
	return len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

func buildWSURL(base, sourceID string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/cli/connect"
	q := u.Query()
	q.Set("source_id", sourceID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type tunnelEvent struct {
	Type    string              `json:"type"`
	EventID string              `json:"event_id,omitempty"`
	Method  string              `json:"method,omitempty"`
	Path    string              `json:"path,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

type tunnelResponse struct {
	Type    string              `json:"type"`
	EventID string              `json:"event_id"`
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
	Error   string              `json:"error,omitempty"`
}

func runTunnel(ctx context.Context, wsURL, apiKey, forwardURL string) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+apiKey)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	fmt.Fprintln(os.Stderr, "✓ tunnel open")

	client := &http.Client{Timeout: 30 * time.Second}

	for {
		var ev tunnelEvent
		if err := wsjson.Read(ctx, conn, &ev); err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return fmt.Errorf("ws read: %w", err)
		}
		switch ev.Type {
		case "hello":
			fmt.Fprintf(os.Stderr, "  hello: %s\n", ev.EventID)
			continue
		case "ping":
			continue
		case "event":
			go forward(ctx, conn, client, forwardURL, ev)
		default:
			fmt.Fprintf(os.Stderr, "  unknown frame: %s\n", ev.Type)
		}
	}
}

func forward(ctx context.Context, conn *websocket.Conn, client *http.Client, forwardURL string, ev tunnelEvent) {
	resp := tunnelResponse{Type: "response", EventID: ev.EventID}

	req, err := http.NewRequestWithContext(ctx, ev.Method, forwardURL, bytes.NewReader(ev.Body))
	if err != nil {
		resp.Error = err.Error()
		_ = wsjson.Write(ctx, conn, resp)
		return
	}
	for k, vs := range ev.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	r, err := client.Do(req)
	if err != nil {
		resp.Error = err.Error()
		_ = wsjson.Write(ctx, conn, resp)
		return
	}
	defer r.Body.Close()
	resp.Status = r.StatusCode
	resp.Headers = r.Header
	resp.Body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = wsjson.Write(ctx, conn, resp)
}

// cliAuth resolves the API key + base URL the same way listen does.
func cliAuth(baseFlag string) (apiKey, base string, err error) {
	apiKey = os.Getenv("DSTREAM_API_KEY")
	if apiKey == "" {
		return "", "", errors.New("DSTREAM_API_KEY env var required")
	}
	base = baseFlag
	if base == "" {
		if env := os.Getenv("DSTREAM_API_URL"); env != "" {
			base = env
		} else {
			base = "http://localhost:8080"
		}
	}
	return apiKey, strings.TrimRight(base, "/"), nil
}

// cliGetJSON does an authed GET and decodes the JSON body into out.
func cliGetJSON(url, apiKey string, out any) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %d %s", url, resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// cliPostJSON does an authed POST of `in` (JSON) and decodes the JSON response
// into out (out may be nil to ignore the body). Non-2xx is an error with the
// server's status+body.
func cliPostJSON(url, apiKey string, in any, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("POST", url, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %d %s", url, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type cliFixture struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Tags       []string `json:"tags"`
	SourceID   string   `json:"source_id"`
	HTTPMethod string   `json:"http_method"`
}

type cliExport struct {
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Headers     map[string][]string `json:"headers"`
	ContentType string              `json:"content_type"`
	Body        string              `json:"body_base64"`
}

// resolveFixtureID returns ref if it's already a UUID, else looks it up by name.
func resolveFixtureID(base, apiKey, ref string) (string, error) {
	if isUUID(ref) {
		return ref, nil
	}
	var fixtures []cliFixture
	if err := cliGetJSON(base+"/api/bookmarks", apiKey, &fixtures); err != nil {
		return "", err
	}
	for _, f := range fixtures {
		if f.Name == ref {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("no fixture named %q", ref)
}

// cliSkipHeader drops headers that must not be forwarded on replay (mirrors the
// server-side bookmark.skipHeader): reserved/hop-by-hop/forwarding headers and
// any value stored redacted at rest.
func cliSkipHeader(key string, vals []string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Host", "Content-Length", "Content-Type", "Dstream-Webhook-Hops",
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip":
		return true
	}
	for _, v := range vals {
		if v == "[redacted]" {
			return true
		}
	}
	return false
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func fixturesCmd() *cobra.Command {
	var baseURLFlag string
	cmd := &cobra.Command{
		Use:   "fixtures",
		Short: "List saved webhook fixtures (bookmarks)",
		RunE: func(_ *cobra.Command, _ []string) error {
			apiKey, base, err := cliAuth(baseURLFlag)
			if err != nil {
				return err
			}
			var fixtures []cliFixture
			if err := cliGetJSON(base+"/api/bookmarks", apiKey, &fixtures); err != nil {
				return err
			}
			if len(fixtures) == 0 {
				fmt.Println("no fixtures")
				return nil
			}
			fmt.Printf("%-28s %-8s %-38s %s\n", "NAME", "METHOD", "SOURCE", "TAGS")
			for _, f := range fixtures {
				fmt.Printf("%-28s %-8s %-38s %s\n", truncateStr(f.Name, 28), f.HTTPMethod, f.SourceID, strings.Join(f.Tags, ","))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURLFlag, "url", "", "dstream API base URL (default: $DSTREAM_API_URL or http://localhost:8080)")
	return cmd
}

// forwardExport decodes exp's body and POSTs (or exp.Method's verb) it to
// target, honoring cliSkipHeader + Content-Type like replay/scenario both need.
func forwardExport(exp cliExport, target string) (status int, dur time.Duration, body string, err error) {
	b, err := base64.StdEncoding.DecodeString(exp.Body)
	if err != nil {
		return 0, 0, "", fmt.Errorf("decode fixture body: %w", err)
	}
	method := exp.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(b))
	if err != nil {
		return 0, 0, "", err
	}
	for k, vals := range exp.Headers {
		if cliSkipHeader(k, vals) {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	if exp.ContentType != "" {
		req.Header.Set("Content-Type", exp.ContentType)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, time.Since(start), "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, time.Since(start), string(rb), nil
}

func replayCmd() *cobra.Command {
	var forwardFlag, baseURLFlag string
	var count int
	cmd := &cobra.Command{
		Use:   "replay <fixture-name-or-id>",
		Short: "Replay a saved fixture to a local URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			apiKey, base, err := cliAuth(baseURLFlag)
			if err != nil {
				return err
			}
			id, err := resolveFixtureID(base, apiKey, args[0])
			if err != nil {
				return err
			}
			var exp cliExport
			if err := cliGetJSON(base+"/api/bookmarks/"+id+"/export", apiKey, &exp); err != nil {
				return err
			}
			for i := 0; i < count; i++ {
				status, dur, body, err := forwardExport(exp, forwardFlag)
				if err != nil {
					fmt.Fprintf(os.Stderr, "replay %d/%d: %v\n", i+1, count, err)
					continue
				}
				fmt.Printf("%d/%d  %d  %dms  %s\n", i+1, count, status, dur.Milliseconds(), truncateStr(body, 200))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&forwardFlag, "forward", "", "Local URL to send the fixture to (required)")
	cmd.Flags().IntVar(&count, "count", 1, "Number of times to replay")
	cmd.Flags().StringVar(&baseURLFlag, "url", "", "dstream API base URL (default: $DSTREAM_API_URL or http://localhost:8080)")
	_ = cmd.MarkFlagRequired("forward")
	return cmd
}

func importCmd() *cobra.Command {
	var sourceFlag, nameFlag, descFlag, tagsFlag, baseURLFlag string
	cmd := &cobra.Command{
		Use:   "import <file.json>",
		Short: "Import an exported fixture JSON as a bookmark",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			apiKey, base, err := cliAuth(baseURLFlag)
			if err != nil {
				return err
			}
			if nameFlag == "" {
				return errors.New("--name required")
			}
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read %s: %w", args[0], err)
			}
			// The exported fixture shape (from GET /api/bookmarks/{id}/export).
			var exp struct {
				Method      string              `json:"method"`
				Path        string              `json:"path"`
				Headers     map[string][]string `json:"headers"`
				ContentType string              `json:"content_type"`
				Body        string              `json:"body_base64"`
			}
			if err := json.Unmarshal(raw, &exp); err != nil {
				return fmt.Errorf("parse fixture json: %w", err)
			}
			srcID, err := resolveSource(base, apiKey, sourceFlag)
			if err != nil {
				return err
			}
			var tags []string
			if tagsFlag != "" {
				tags = strings.Split(tagsFlag, ",")
			}
			reqBody := map[string]any{
				"source_id":    srcID,
				"name":         nameFlag,
				"description":  descFlag,
				"tags":         tags,
				"method":       exp.Method,
				"path":         exp.Path,
				"headers":      exp.Headers,
				"content_type": exp.ContentType,
				"body_base64":  exp.Body,
			}
			var created struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := cliPostJSON(base+"/api/bookmarks/import", apiKey, reqBody, &created); err != nil {
				return err
			}
			fmt.Printf("imported fixture %q (id %s)\n", created.Name, created.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceFlag, "source", "", "Source ID or name to attach the imported request to (required)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "Fixture name (required)")
	cmd.Flags().StringVar(&descFlag, "description", "", "Optional description")
	cmd.Flags().StringVar(&tagsFlag, "tags", "", "Comma-separated tags")
	cmd.Flags().StringVar(&baseURLFlag, "url", "", "dstream API base URL (default: $DSTREAM_API_URL or http://localhost:8080)")
	_ = cmd.MarkFlagRequired("source")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

type cliScenario struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type cliScenarioStep struct {
	Position     int    `json:"position"`
	BookmarkID   string `json:"bookmark_id"`
	BookmarkName string `json:"bookmark_name"`
	DelayMs      int    `json:"delay_ms"`
}

type cliScenarioDetail struct {
	cliScenario
	Steps []cliScenarioStep `json:"steps"`
}

// resolveScenarioID returns ref if it's already a UUID, else looks it up by name.
func resolveScenarioID(base, apiKey, ref string) (string, error) {
	if isUUID(ref) {
		return ref, nil
	}
	var scenarios []cliScenario
	if err := cliGetJSON(base+"/api/scenarios", apiKey, &scenarios); err != nil {
		return "", err
	}
	for _, s := range scenarios {
		if s.Name == ref {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no scenario named %q", ref)
}

func scenarioCmd() *cobra.Command {
	c := &cobra.Command{Use: "scenario", Short: "Run saved scenarios (ordered fixture sequences)"}
	c.AddCommand(scenarioRunCmd())
	return c
}

func scenarioRunCmd() *cobra.Command {
	var forwardFlag, baseURLFlag string
	cmd := &cobra.Command{
		Use:   "run <scenario-name-or-id>",
		Short: "Run a saved scenario, forwarding each step's fixture in order",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			apiKey, base, err := cliAuth(baseURLFlag)
			if err != nil {
				return err
			}
			id, err := resolveScenarioID(base, apiKey, args[0])
			if err != nil {
				return err
			}
			var sc cliScenarioDetail
			if err := cliGetJSON(base+"/api/scenarios/"+id, apiKey, &sc); err != nil {
				return err
			}
			for _, step := range sc.Steps {
				if step.DelayMs > 0 {
					time.Sleep(time.Duration(step.DelayMs) * time.Millisecond)
				}
				var exp cliExport
				if err := cliGetJSON(base+"/api/bookmarks/"+step.BookmarkID+"/export", apiKey, &exp); err != nil {
					fmt.Fprintf(os.Stderr, "step %d %s: %v\n", step.Position, step.BookmarkName, err)
					return err
				}
				status, dur, _, err := forwardExport(exp, forwardFlag)
				if err != nil {
					fmt.Fprintf(os.Stderr, "step %d %s: %v\n", step.Position, step.BookmarkName, err)
					return err
				}
				fmt.Printf("step %d %s: %d %dms\n", step.Position, step.BookmarkName, status, dur.Milliseconds())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&forwardFlag, "forward", "", "Local URL to forward each step's fixture to (required)")
	cmd.Flags().StringVar(&baseURLFlag, "url", "", "dstream API base URL (default: $DSTREAM_API_URL or http://localhost:8080)")
	_ = cmd.MarkFlagRequired("forward")
	return cmd
}
