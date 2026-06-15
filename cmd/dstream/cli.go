package main

import (
	"bytes"
	"context"
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
	c.AddCommand(listenCmd())
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
