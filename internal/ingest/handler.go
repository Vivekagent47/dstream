package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/filter"
	"github.com/Vivekagent47/dstream/internal/metrics"
	"github.com/Vivekagent47/dstream/internal/store"
)

// ingestTracer names spans for the ingest hot path. Bound to the global
// provider's delegate at init, so it forwards to whatever tracing.Init sets.
var ingestTracer = otel.Tracer("dstream/ingest")

const (
	MaxBodyBytes   = 5 << 20 // 5 MiB
	DedupWindow    = 60 * time.Second
	SourceCacheTTL = 60 * time.Second

	// NegativeSourceCacheTTL caches an unknown ingest token so a flood of the
	// same bad token (unauthenticated POST /e/{random}) collapses to one DB
	// round-trip per TTL instead of one per request — otherwise every miss hits
	// Postgres (GetSourceByIngestToken) and burns a pool slot, and the per-source
	// rate limiter can't fire for a source that never resolves (audit #12). Kept
	// much shorter than SourceCacheTTL so a source created just after a bad-token
	// probe becomes reachable within a few seconds.
	NegativeSourceCacheTTL = 3 * time.Second
)

type Handler struct {
	Log       *slog.Logger
	Queries   *store.Queries
	Redis     *redis.Client
	Queue     *dqueue.Client
	BodyStore BodyStore

	// Per-source ingest rate limit (token bucket in Redis). Limiter is nil or
	// RateLimitRPS<=0 → disabled. Guards the DB/queue from a single source
	// (or a leaked ingest token) flooding the endpoint.
	Limiter        *redis_rate.Limiter
	RateLimitRPS   int
	RateLimitBurst int

	// MaxWebhookHops rejects an ingest request whose Dstream-Webhook-Hops
	// header has already reached this ceiling (loop guard). 0 disables.
	MaxWebhookHops int

	// In-process cache for source lookups keyed by ingest_token. The
	// ingest hot path was hitting Postgres on every webhook (~0.5–1ms per
	// request) even though source rows change rarely; this collapses
	// repeat lookups within the TTL into a single map probe. Cache
	// invalidation on source deletion is implicit via the TTL — a
	// just-deleted source remains addressable for up to SourceCacheTTL,
	// which is acceptable for v1 (the worst case is one extra request
	// queued for an org that just rotated tokens).
	sourceCache sync.Map // map[string]sourceCacheEntry, keyed by ingest_token
}

type sourceCacheEntry struct {
	src      store.Source
	rules    []compiledRule
	expires  time.Time
	notFound bool // negative entry: token resolved to ErrSourceNotFound
}

// compiledRule is a source's enabled capture rule with its filter pre-compiled
// at cache-load time, so the ingest hot path never compiles CEL per request.
// prg == nil means the rule has no filter (match every request).
type compiledRule struct {
	id   uuid.UUID
	cap  int32
	name string
	prg  *filter.Program
}

func (h *Handler) Mount(r chi.Router) {
	// GET is intentionally not registered — dstream never ingests over GET.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		r.Method(m, "/e/{token}", http.HandlerFunc(h.handleIngest))
	}
}

type ingestResponse struct {
	RequestID string   `json:"request_id"`
	EventIDs  []string `json:"event_ids"`
	Deduped   bool     `json:"deduped,omitempty"`
}

func (h *Handler) handleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()
	token := chi.URLParam(r, "token")

	src, rules, err := func() (store.Source, []compiledRule, error) {
		ctx, span := ingestTracer.Start(ctx, "ingest.resolve_source")
		defer span.End()
		src, rules, err := h.resolveSource(ctx, token)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return src, rules, err
	}()
	if err != nil {
		if errors.Is(err, ErrSourceNotFound) {
			http.Error(w, "unknown source", http.StatusNotFound)
			return
		}
		h.Log.ErrorContext(ctx, "ingest: resolve source", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sourceID := store.GoUUID(src.ID)
	defer func() { metrics.IngestDuration(sourceID, time.Since(start)) }()

	if !methodAllowed(src.AllowedMethods, r.Method) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Loop guard: refuse a request that has already bounced through dstream too
	// many times (deliver→ingest→deliver…). No enqueue, before reading the body.
	if h.MaxWebhookHops > 0 && hopCount(r) >= h.MaxWebhookHops {
		h.Log.WarnContext(ctx, "ingest: webhook loop guard tripped", "source_id", src.ID, "hops", hopCount(r))
		http.Error(w, "loop detected: Dstream-Webhook-Hops limit reached", http.StatusForbidden)
		return
	}

	// Per-source rate limit BEFORE reading the (up to 5 MiB) body, so a flood
	// can't force large reads. Fail-open on limiter error — availability beats
	// strict limiting for inbound webhooks.
	if h.Limiter != nil && h.RateLimitRPS > 0 {
		burst := h.RateLimitBurst
		if burst <= 0 {
			burst = h.RateLimitRPS
		}
		res, rlErr := h.Limiter.Allow(ctx, "ingest:src:"+sourceID.String(), redis_rate.Limit{
			Rate:   h.RateLimitRPS,
			Burst:  burst,
			Period: time.Second,
		})
		if rlErr != nil {
			// Fail-open by design (availability beats strict limiting), but a
			// swallowed error means a Redis limiter outage silently disables
			// ingest rate limiting with zero signal (audit N6) — log so it's
			// observable. Still proceed below.
			h.Log.WarnContext(ctx, "ingest: rate limiter error (fail-open)", "err", rlErr, "source_id", sourceID.String())
		}
		if rlErr == nil && res.Allowed == 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
	}

	body, err := func() ([]byte, error) {
		_, span := ingestTracer.Start(ctx, "ingest.read_body")
		defer span.End()
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return b, err
	}()
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])

	dup, err := func() (bool, error) {
		ctx, span := ingestTracer.Start(ctx, "ingest.dedup")
		defer span.End()
		return h.checkDedup(ctx, sourceID, bodyHash)
	}()
	if err != nil {
		h.Log.WarnContext(ctx, "ingest: dedup check failed (ignored)", "err", err)
	}
	metrics.IngestRequest(sourceID, dup)

	// If we just claimed the dedup key (not a duplicate, and the SetNX
	// succeeded), roll it back on any failure return before the events are
	// durably created. Otherwise the sender's retry within the dedup window is
	// deduped and the webhook is silently lost — a request row exists but no
	// events were ever created, and the reaper can't recover what was never
	// inserted (2026-07-21 audit #1). Background ctx: request-ctx cancellation
	// mid-insert is one of the triggers, so r.Context() may already be done.
	dedupClaimed := !dup && err == nil
	committed := false
	if dedupClaimed {
		defer func() {
			if !committed {
				_ = h.Redis.Del(context.Background(), dedupKey(sourceID, bodyHash)).Err()
			}
		}()
	}

	// v7 so the request id sorts by creation time and clusters in the PK
	// B-tree (matches the uuidv7() column defaults). NewV7 only errs if the
	// system RNG fails, which is catastrophic — Must is appropriate.
	reqID := uuid.Must(uuid.NewV7())
	bodyRef := "pg:" + reqID.String()

	req, err := func() (store.Request, error) {
		ctx, span := ingestTracer.Start(ctx, "ingest.persist")
		defer span.End()
		req, err := h.Queries.CreateRequest(ctx, store.CreateRequestParams{
			ID:          store.UUID(reqID),
			SourceID:    src.ID,
			HTTPMethod:  r.Method,
			HTTPPath:    r.URL.Path,
			Headers:     captureHeaders(r.Header),
			BodyHash:    bodyHash,
			BodyRef:     bodyRef,
			BodySize:    int32(len(body)),
			ContentType: optStr(r.Header.Get("Content-Type")),
			// Signature verification removed — auth is post-release scope (see
			// PLAN.md ingest path). Column kept so no migration when it lands.
			SigVerified: false,
			IngestIP:    parseRemoteAddr(r),
		})
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			h.Log.ErrorContext(ctx, "ingest: create request", "err", err)
			return req, err
		}
		if _, err := h.BodyStore.Put(ctx, store.GoUUID(req.ID), body); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			h.Log.ErrorContext(ctx, "ingest: store body", "err", err)
			return req, err
		}
		return req, nil
	}()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := ingestResponse{RequestID: reqID.String()}

	// Flatten headers once, only if this source has rules — capture is the
	// only consumer, and a no-rule source (the common case) must do zero
	// extra work per request.
	var captureHdr map[string]string
	if len(rules) > 0 {
		captureHdr = make(map[string]string, len(r.Header))
		for k := range r.Header {
			captureHdr[k] = r.Header.Get(k)
		}
	}

	if dup {
		resp.Deduped = true
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	conns, err := h.Queries.ListEnabledConnectionsBySource(ctx, src.ID)
	if err != nil {
		h.Log.ErrorContext(ctx, "ingest: list connections", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(conns) == 0 {
		// Source has no enabled connections — nothing to fan out to. Still a
		// successful ingest (the request + body are persisted for replay), and
		// a retry would produce the same zero-event result, so keep the dedup key.
		committed = true
		if len(rules) > 0 {
			h.capture(ctx, rules, src.OrgID, reqID, body, captureHdr)
		}
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	// Fan out all events in ONE insert instead of a per-connection roundtrip.
	// RETURNING order is unspecified, so we key the per-connection retry
	// policy by connection_id rather than positional index.
	connIDs := make([]pgtype.UUID, len(conns))
	connByID := make(map[uuid.UUID]store.Connection, len(conns))
	for i, c := range conns {
		connIDs[i] = c.ID
		connByID[store.GoUUID(c.ID)] = c
	}

	// Fan-out span covers the batch insert + the per-event enqueue loop. The
	// enqueue MUST run while this span's ctx is current: dqueue.Enqueue injects
	// the current trace context into each event's carrier, which is how the
	// consumer's deliver span links back to ingest.fanout.
	err = func() error {
		ctx, span := ingestTracer.Start(ctx, "ingest.fanout")
		defer span.End()
		events, err := h.Queries.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
			RequestID:     req.ID,
			OrgID:         src.OrgID,
			ConnectionIds: connIDs,
			IsTest:        false,
		})
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			h.Log.ErrorContext(ctx, "ingest: create events batch", "err", err)
			return err
		}

		// One enqueue per event — local-Redis roundtrips, not the per-connection
		// Postgres roundtrips the batch insert above eliminated. A failed enqueue
		// leaves the event 'queued' in Postgres; the worker's reaper re-queues it.
		for _, ev := range events {
			c := connByID[store.GoUUID(ev.ConnectionID)]
			if err := h.Queue.Enqueue(ctx, dqueue.Payload{
				EventID:             store.GoUUID(ev.ID),
				OrgID:               store.GoUUID(ev.OrgID),
				Attempt:             0,
				EnqueuedAt:          time.Now().UnixMilli(),
				RetryStrategy:       c.RetryStrategy,
				RetryBaseMs:         c.RetryBaseMs,
				RetryCapMs:          c.RetryCapMs,
				RetryJitterPct:      c.RetryJitterPct,
				CustomRetrySchedule: c.CustomRetrySchedule,
			}); err != nil {
				h.Log.ErrorContext(ctx, "ingest: enqueue delivery", "err", err, "event_id", store.GoUUID(ev.ID))
				continue
			}
			resp.EventIDs = append(resp.EventIDs, store.GoUUID(ev.ID).String())
		}
		return nil
	}()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Events are durably in Postgres now (CreateEventsBatch committed); a failed
	// enqueue leaves them 'queued' for the reaper. Keep the dedup key.
	committed = true
	if len(rules) > 0 {
		h.capture(ctx, rules, src.OrgID, reqID, body, captureHdr)
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// capture runs a source's compiled capture rules against one committed
// request, best-effort: a match auto-bookmarks the request and evicts past
// the rule's cap. Called only when len(rules) > 0, so a no-rule source pays
// nothing here. Every failure (eval, create, evict) is logged and swallowed,
// and a panic is recovered — capture must never fail or change the ingest
// response, which has already been decided by the caller.
func (h *Handler) capture(ctx context.Context, rules []compiledRule, orgID pgtype.UUID, reqID uuid.UUID, body []byte, hdr map[string]string) {
	defer func() {
		if rec := recover(); rec != nil {
			h.Log.ErrorContext(ctx, "capture panic (ignored)", "panic", rec)
		}
	}()
	for _, rule := range rules {
		if rule.prg != nil {
			ok, err := rule.prg.Eval(body, hdr, filter.Meta{})
			if err != nil {
				// Fail-CLOSED: skip capture on eval error. Opposite of the
				// delivery filter's fail-open — a spurious capture is noise,
				// not a lost event.
				h.Log.WarnContext(ctx, "capture filter eval error (skipping)", "rule", rule.id, "err", err)
				continue
			}
			if !ok {
				continue
			}
		}
		// Full reqID, not a truncated prefix: a UUIDv7's first 8 hex chars are
		// only the high 32 bits of its 48-bit millisecond timestamp, so two
		// requests within the same ~65s window share it — a truncated suffix
		// collided against bookmarks' UNIQUE(org_id, name) under any request
		// burst on one rule, silently dropping the capture.
		autoName := rule.name + "-" + reqID.String()
		if _, err := h.Queries.CreateAutoBookmark(ctx, store.CreateAutoBookmarkParams{
			OrgID:         orgID,
			RequestID:     store.UUID(reqID),
			Name:          autoName,
			CaptureRuleID: store.UUID(rule.id),
		}); err != nil {
			h.Log.WarnContext(ctx, "capture: create bookmark (ignored)", "rule", rule.id, "err", err)
			continue
		}
		if err := h.Queries.EvictCaptureBookmarks(ctx, store.EvictCaptureBookmarksParams{
			CaptureRuleID: store.UUID(rule.id),
			Limit:         rule.cap,
		}); err != nil {
			h.Log.WarnContext(ctx, "capture: evict (ignored)", "rule", rule.id, "err", err)
		}
	}
}

// InvalidateSource drops a source from the in-process cache so enable/disable
// and allowed-methods edits take effect immediately instead of after
// SourceCacheTTL.
// ponytail: in-process only. If ingest is ever split into its own process,
// this must become a Redis pub/sub invalidation.
func (h *Handler) InvalidateSource(token string) {
	h.sourceCache.Delete(token)
}

func (h *Handler) resolveSource(ctx context.Context, token string) (store.Source, []compiledRule, error) {
	// Cache hit? (positive or negative)
	if v, ok := h.sourceCache.Load(token); ok {
		entry := v.(sourceCacheEntry)
		if time.Now().Before(entry.expires) {
			if entry.notFound {
				return store.Source{}, nil, ErrSourceNotFound
			}
			return entry.src, entry.rules, nil
		}
		// Expired — fall through to a fresh lookup. We delete eagerly to
		// keep the map size bounded even for tokens that stop being
		// presented.
		h.sourceCache.Delete(token)
	}
	src, err := h.Queries.GetSourceByIngestToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Negative-cache the miss so a bad-token flood can't hammer the DB
			// pool (audit #12). Short TTL: a source created moments later still
			// resolves once the entry lapses.
			// ponytail: if this proves insufficient under attack, add a global
			// per-IP rate limit before token resolution (heavier, out of scope here).
			h.sourceCache.Store(token, sourceCacheEntry{
				expires:  time.Now().Add(NegativeSourceCacheTTL),
				notFound: true,
			})
			return store.Source{}, nil, ErrSourceNotFound
		}
		return store.Source{}, nil, err
	}

	// Load + compile this source's enabled capture rules ONCE per cache entry
	// (not per request). A source with no rules gets a nil slice, so the
	// per-request capture check below is a single len()==0 branch — no extra
	// DB work on the no-rule path. Best-effort: a failure here never fails
	// source resolution, it just means capture is (temporarily) disabled for
	// this source until the entry expires and reloads.
	var rules []compiledRule
	rows, err := h.Queries.ListEnabledCaptureRulesBySource(ctx, src.ID)
	if err != nil {
		h.Log.WarnContext(ctx, "ingest: list capture rules (treating as none)", "source_id", store.GoUUID(src.ID), "err", err)
	}
	for _, r := range rows {
		var prg *filter.Program
		if r.FilterExpr != nil && *r.FilterExpr != "" {
			p, err := filter.Compile(*r.FilterExpr, false)
			if err != nil {
				h.Log.WarnContext(ctx, "capture rule: bad filter, skipping", "rule", store.GoUUID(r.ID), "err", err)
				continue
			}
			prg = p
		}
		rules = append(rules, compiledRule{id: store.GoUUID(r.ID), cap: r.Cap, name: r.Name, prg: prg})
	}

	h.sourceCache.Store(token, sourceCacheEntry{
		src:     src,
		rules:   rules,
		expires: time.Now().Add(SourceCacheTTL),
	})
	return src, rules, nil
}

// checkDedup returns true if the body is a duplicate of one seen within the
// dedup window for this source.
func (h *Handler) checkDedup(ctx context.Context, sourceID uuid.UUID, bodyHash string) (bool, error) {
	ok, err := h.Redis.SetNX(ctx, dedupKey(sourceID, bodyHash), 1, DedupWindow).Result()
	if err != nil {
		return false, err
	}
	// SetNX returns true if the key was newly set — i.e. NOT a duplicate.
	return !ok, nil
}

// dedupKey is the Redis key for a source's per-body dedup marker. Kept in one
// place so the claim (checkDedup) and the failure rollback (handleIngest) can
// never drift apart — a mismatched key would silently fail to roll back.
func dedupKey(sourceID uuid.UUID, bodyHash string) string {
	return "dedup:" + sourceID.String() + ":" + bodyHash
}

// sensitiveHeaders are credential-bearing request headers whose VALUES are
// redacted before persisting to requests.headers — the row is later viewable by
// org members, and a sender's Authorization/Cookie must not leak at rest (audit
// N5). Keys are kept (value replaced) so it stays visible one was sent.
// ponytail: inbound header auth is post-release scope (PLAN.md); revisit if a
// future signature-verification path needs the raw value.
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Cookie":              true,
	"Proxy-Authorization": true,
}

func captureHeaders(h http.Header) []byte {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		// http.Header keys are already canonicalized; canonicalize again to be
		// safe against a non-canonical key reaching this via a direct call.
		if sensitiveHeaders[http.CanonicalHeaderKey(k)] {
			out[k] = []string{"[redacted]"}
			continue
		}
		out[k] = v
	}
	b, _ := json.Marshal(out)
	return b
}

func parseRemoteAddr(r *http.Request) *netip.Addr {
	// Trust ONLY r.RemoteAddr: the TrustedRealIP middleware has already
	// normalized it to the real client IP, peeling X-Forwarded-For only through
	// configured trusted proxies. Parsing raw XFF here would bypass that gate and
	// let any client forge the stored ingest_ip (GHSA-3fxj-6jh8-hvhx, audit #10).
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}

// hopCount reads the Dstream-Webhook-Hops request header (0 if absent/invalid).
func hopCount(r *http.Request) int {
	if n, err := strconv.Atoi(r.Header.Get("Dstream-Webhook-Hops")); err == nil && n > 0 {
		return n
	}
	return 0
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
