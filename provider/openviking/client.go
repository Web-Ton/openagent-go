// Package openviking implements the OpenViking Context Database adapters:
// memory, skill, and resource providers backed by an OpenViking server.
//
// OpenViking is a Provider — the runtime never depends on it directly.
// The client talks to the OpenViking HTTP API directly (no SDK): the
// server's REST surface is the stable contract, and the provider needs
// only a handful of endpoints.
//
//	Search     — POST /api/v1/search/search  (semantic retrieval)
//	ListSkills — GET  /api/v1/skills         (list skill catalog)
//	Remember   — POST /api/v1/sessions → /messages/batch → /commit
//	AddMessage — POST /api/v1/sessions/{id}/messages/batch  (session reuse)
//	MaybeCommit— POST /api/v1/sessions/{id}/commit          (threshold-based)
//	Read       — GET  /api/v1/content/read  (viking:// URI → content)
package openviking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Item is a stored knowledge entry in OpenViking (one search hit).
type Item struct {
	ID      string         `json:"id,omitempty"`   // viking:// URI
	Kind    string         `json:"kind,omitempty"` // memory | resource | skill
	Content string         `json:"content"`        // abstract
	Meta    map[string]any `json:"meta,omitempty"`
	Score   float64        `json:"score,omitempty"`
}

// Client talks to an OpenViking server over its HTTP API. An optional API
// key is sent as a Bearer token; when empty, the server's own identity
// (account / user) scopes the knowledge.
//
// When SessionConfig.SessionIDSeed is non-empty, the client maintains one
// OV session per conversation (derived from seed + conversation ID),
// lazily created and crash-recovered. Commits are threshold-based instead
// of per-call. When the seed is empty (legacy mode via NewClient), each
// Remember call creates a fresh session and commits immediately — the
// original behavior.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	sessMu   sync.Mutex
	sessions map[string]*sessionState // ovSessionID → state
	sessCfg  SessionConfig
}

// sessionState tracks the commit-threshold state for one OV session.
type sessionState struct {
	id             string
	pendingTokens  int       // last reported by server via add_message response
	msgSinceCommit int       // turn-count fallback counter
	lastCommitAt   time.Time // for MinCommitInterval enforcement
}

// SessionConfig controls the OpenViking per-conversation session reuse
// and commit threshold policy. When SessionIDSeed is empty, the client
// falls back to legacy mode (create session + immediate commit per
// Remember call).
//
// Zero threshold/interval values fall back to built-in defaults via
// applySessionDefaults: CommitTokenThreshold=30000,
// CommitMessageThreshold=20, MinCommitInterval=5m. These align with the
// OpenViking team's recommendation for knowledge-fragment stores.
type SessionConfig struct {
	// SessionIDSeed is combined with the conversation ID to derive a
	// deterministic OV session ID per conversation:
	// "openagent-{sha256(seed + "/" + conversationID)[:12]}".
	// Same seed + same conversation → same OV session across restarts.
	// Empty = legacy mode (no reuse).
	SessionIDSeed string

	// CommitTokenThreshold is the pending-token count that triggers a
	// commit. The server returns pending_tokens after each add_message.
	// Default 30000.
	CommitTokenThreshold int

	// CommitMessageThreshold is the turn-count fallback: commit when
	// msgSinceCommit reaches this value, even if pending_tokens is below
	// the token threshold. Default 20.
	CommitMessageThreshold int

	// MinCommitInterval prevents burst commits. Even if a threshold is
	// crossed, the client waits at least this long since the last commit.
	// Default 5m.
	MinCommitInterval time.Duration

	// WM v2 retention parameters — passed to the commit endpoint. These
	// control how many recent messages are retained in the live session
	// after commit. Defaults: turn_count=3, budget=6000, steps=1.
	KeepRecentTurnCount        int
	RetainedMessageTokenBudget int
	MinRawTailSteps            int
}

// assistantMemoryPolicy is the memory_policy sent at session creation.
// self=false: the agent is an assistant — don't extract into the session
// owner's scope. peer=true: extract into the user's (peer) scope so
// memories are attributed to the user, not the agent.
var assistantMemoryPolicy = map[string]any{
	"self": map[string]any{"enabled": false},
	"peer": map[string]any{"enabled": true},
}

// NewClient creates a client in legacy mode (no session reuse). Each
// Remember call creates a fresh session and commits immediately.
func NewClient(endpoint, apiKey string) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("openviking: empty endpoint")
	}
	return &Client{
		baseURL: endpoint,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// NewClientWithSession creates a client with per-conversation session
// reuse and threshold-based commit. The seed is combined with each
// conversation ID to derive a deterministic OV session ID so the same
// deployment reuses the same OV session for the same conversation across
// restarts.
func NewClientWithSession(endpoint, apiKey string, cfg SessionConfig) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("openviking: empty endpoint")
	}
	applySessionDefaults(&cfg)
	return &Client{
		baseURL:  endpoint,
		apiKey:   apiKey,
		http:     &http.Client{Timeout: 120 * time.Second},
		sessions: make(map[string]*sessionState),
		sessCfg:  cfg,
	}, nil
}

// applySessionDefaults fills zero-value threshold/retention fields with
// the built-in defaults. Called by NewClientWithSession.
func applySessionDefaults(cfg *SessionConfig) {
	if cfg.CommitTokenThreshold == 0 {
		cfg.CommitTokenThreshold = 30000
	}
	if cfg.CommitMessageThreshold == 0 {
		cfg.CommitMessageThreshold = 20
	}
	if cfg.MinCommitInterval == 0 {
		cfg.MinCommitInterval = 5 * time.Minute
	}
	if cfg.KeepRecentTurnCount == 0 {
		cfg.KeepRecentTurnCount = 3
	}
	if cfg.RetainedMessageTokenBudget == 0 {
		cfg.RetainedMessageTokenBudget = 6000
	}
	if cfg.MinRawTailSteps == 0 {
		cfg.MinRawTailSteps = 1
	}
}

// responseEnvelope is the OpenViking API envelope: {status, result, error}.
type responseEnvelope struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiError is a non-2xx / envelope-error response.
type apiError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("openviking api: %s (%s, HTTP %d)", e.Message, e.Code, e.StatusCode)
	}
	return fmt.Sprintf("openviking api: HTTP %d: %s", e.StatusCode, e.Message)
}

// doJSON performs one API call and decodes the envelope's result into out.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openviking request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var env responseEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return &apiError{StatusCode: resp.StatusCode, Message: truncate(string(data), 200)}
	}
	if env.Error != nil {
		return &apiError{StatusCode: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if env.Status != "" && env.Status != "ok" {
		return &apiError{StatusCode: resp.StatusCode, Code: env.Status, Message: truncate(string(data), 200)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{StatusCode: resp.StatusCode, Message: truncate(string(data), 200)}
	}
	if out == nil || len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// ── Search ──

// findResult mirrors the /api/v1/search/search response.
type findResult struct {
	Memories  []matchedContext `json:"memories,omitempty"`
	Resources []matchedContext `json:"resources,omitempty"`
	Skills    []matchedContext `json:"skills,omitempty"`
}

// matchedContext is one retrieval hit.
type matchedContext struct {
	URI      string  `json:"uri,omitempty"`
	Abstract string  `json:"abstract,omitempty"`
	Score    float64 `json:"score,omitempty"`
}

// Search runs the server's semantic retrieval. contextType narrows to one
// index ("memory" | "resource" | "skill"); empty returns everything.
func (c *Client) Search(ctx context.Context, query string, limit int, contextType ...string) ([]Item, error) {
	payload := map[string]any{
		"query": query,
		"limit": limit,
	}
	kind := "memory"
	if len(contextType) > 0 && contextType[0] != "" {
		kind = contextType[0]
		payload["context_type"] = kind
	}

	var res findResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/search/search", nil, payload, &res); err != nil {
		return nil, fmt.Errorf("openviking search: %w", err)
	}

	var hits []matchedContext
	switch kind {
	case "resource":
		hits = res.Resources
	case "skill":
		hits = res.Skills
	default:
		hits = res.Memories
	}
	items := make([]Item, 0, len(hits))
	for _, m := range hits {
		items = append(items, Item{
			Kind:    kind,
			ID:      m.URI,
			Content: m.Abstract,
			Score:   m.Score,
		})
	}
	return items, nil
}

// ── Recall ──

// recallRequest mirrors the POST /api/v1/search/recall request body.
type recallRequest struct {
	Query    string         `json:"query"`
	Quotas   map[string]int `json:"quotas,omitempty"`
	MaxChars int            `json:"max_chars,omitempty"`
	MinScore float64        `json:"min_score,omitempty"`
	Render   bool           `json:"render"`
}

// recallEntry is one hit in the recall response.
type recallEntry struct {
	URI      string  `json:"uri,omitempty"`
	Score    float64 `json:"score,omitempty"`
	Type     string  `json:"type,omitempty"`   // events | entities | preferences | experiences
	Mode     string  `json:"mode,omitempty"`   // full | summary | uri
	Origin   string  `json:"origin,omitempty"` // actor_peer | self | other_peer
	Content  string  `json:"content,omitempty"`
	Summary  string  `json:"summary,omitempty"`
	Abstract string  `json:"abstract,omitempty"`
	Rank     int     `json:"rank,omitempty"`
}

// recallResult mirrors the /api/v1/search/recall response result.
type recallResult struct {
	Entries  []recallEntry  `json:"entries,omitempty"`
	Rendered string         `json:"rendered,omitempty"`
	Stats    map[string]any `json:"stats,omitempty"`
}

// Recall runs the server's type-quota memory recall endpoint
// (POST /api/v1/search/recall). Unlike Search, which does a flat semantic
// lookup, Recall searches memory subtrees independently by type
// (events/entities/preferences/experiences), applies per-type quotas, and
// returns a bounded context block. The caller is responsible for
// supplying non-zero cfg values (the provider's RecallConfig defaults
// handle this).
func (c *Client) Recall(ctx context.Context, query string, cfg RecallConfig) (recallResult, error) {
	req := recallRequest{
		Query:    query,
		Quotas:   cfg.Quotas,
		MaxChars: cfg.MaxChars,
		MinScore: cfg.MinScore,
		Render:   false,
	}
	var res recallResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/search/recall", nil, req, &res); err != nil {
		return recallResult{}, fmt.Errorf("openviking recall: %w", err)
	}
	return res, nil
}

// ── ListSkills ──

// SkillEntry is one skill in the GET /api/v1/skills catalog listing.
type SkillEntry struct {
	Name        string   `json:"name"`
	URI         string   `json:"uri"`
	RootURI     string   `json:"root_uri,omitempty"`
	SkillMDURI  string   `json:"skill_md_uri,omitempty"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Level       int      `json:"level,omitempty"`
}

// listSkillsResult mirrors the GET /api/v1/skills response result.
type listSkillsResult struct {
	Skills []SkillEntry `json:"skills,omitempty"`
	Total  int          `json:"total,omitempty"`
}

// ListSkills lists the full skill catalog from OpenViking. nodeLimit caps
// the number of nodes returned per skill (0 = server default). Unlike
// Search, this endpoint requires no query and returns every skill.
func (c *Client) ListSkills(ctx context.Context, nodeLimit int) ([]SkillEntry, error) {
	query := url.Values{}
	if nodeLimit > 0 {
		query.Set("node_limit", fmt.Sprintf("%d", nodeLimit))
	}
	var res listSkillsResult
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/skills", query, nil, &res); err != nil {
		return nil, fmt.Errorf("openviking list skills: %w", err)
	}
	return res.Skills, nil
}

// ── Remember (legacy + session-reuse) ──

// Remember stores a message into OpenViking long-term memory. It is the
// single-call API: add a message and commit in one shot.
//
// In legacy mode (SessionIDSeed empty, via NewClient), each call creates a
// fresh session, adds one message, and commits immediately — the
// original behavior that matches the OpenViking REST contract but
// produces per-call VLM extraction overhead.
//
// In session-reuse mode (SessionIDSeed set, via NewClientWithSession),
// it delegates to AddMessage + MaybeCommit with a global session (empty
// conversation ID): the message is appended to the global OV session and
// committed only when a threshold is crossed.
//
// Deprecated: prefer AddMessage + MaybeCommit for explicit threshold
// control and per-conversation session routing. Remember is retained for
// backward compatibility.
func (c *Client) Remember(ctx context.Context, content string) (string, error) {
	if c.sessCfg.SessionIDSeed == "" {
		return c.legacyRemember(ctx, content)
	}
	ovSID := c.SessionIDFor("")
	if _, err := c.AddMessage(ctx, "assistant", content, "", ovSID); err != nil {
		return "", err
	}
	if err := c.MaybeCommit(ctx, ovSID); err != nil {
		return "", err
	}
	return "stored", nil
}

// legacyRemember is the original create+add+commit-per-call implementation.
// Uses role="user" (the original behavior); the session-reuse path
// (Store → AddMessage) uses role="assistant" instead, since knowledge
// items are agent-extracted conclusions, not user input. The role
// difference is intentional — legacy callers (e.g. team_memory.go)
// expect the original contract.
func (c *Client) legacyRemember(ctx context.Context, content string) (string, error) {
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/sessions", nil, map[string]any{}, &created); err != nil {
		return "", fmt.Errorf("openviking create session: %w", err)
	}
	if created.SessionID == "" {
		return "", fmt.Errorf("openviking create session: empty session id")
	}

	msg := map[string]any{"role": "user", "content": content}
	if err := c.doJSON(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(created.SessionID)+"/messages/batch",
		nil, map[string]any{"messages": []any{msg}}, nil); err != nil {
		return "", fmt.Errorf("openviking add messages: %w", err)
	}

	if err := c.doJSON(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(created.SessionID)+"/commit",
		nil, map[string]any{"keep_recent_count": 0}, nil); err != nil {
		return "", fmt.Errorf("openviking commit session: %w", err)
	}
	return "stored", nil
}

// ── Per-conversation session reuse ──

// SessionIDFor derives the OV session ID for a conversation. The same
// seed + conversation ID always produces the same OV session ID, so a
// restarted process resumes the same session. An empty conversationID
// yields a global session (backward-compatible with Remember).
func (c *Client) SessionIDFor(conversationID string) string {
	return deriveSessionID(c.sessCfg.SessionIDSeed, conversationID)
}

// AddMessage appends a message to the OV session identified by ovSessionID
// and returns the server-reported pending_tokens. The session is lazily
// created on first call for that ID and reused across subsequent calls.
// If the session has expired (404), it is recreated and the call is
// retried once.
//
// peerID is attached to the message for memory attribution (per-message,
// not per-session). Pass scope.UserID so OV's VLM extracts memories into
// the user's peer scope. Empty peerID is valid (no attribution).
func (c *Client) AddMessage(ctx context.Context, role, content, peerID, ovSessionID string) (int, error) {
	if err := c.ensureSession(ctx, ovSessionID); err != nil {
		return 0, err
	}

	pending, err := c.doAddMessage(ctx, role, content, peerID, ovSessionID)
	if err != nil && isNotFound(err) {
		c.sessMu.Lock()
		delete(c.sessions, ovSessionID)
		c.sessMu.Unlock()
		slog.Warn("openviking session expired, recreating", "ov_session", ovSessionID)
		if err2 := c.ensureSession(ctx, ovSessionID); err2 != nil {
			return 0, err2
		}
		return c.doAddMessage(ctx, role, content, peerID, ovSessionID)
	}
	if err != nil {
		return 0, err
	}
	return pending, nil
}

// doAddMessage sends a messages/batch request and updates the per-session
// pending state. The caller must have called ensureSession first.
func (c *Client) doAddMessage(ctx context.Context, role, content, peerID, ovSessionID string) (int, error) {
	c.sessMu.Lock()
	ss := c.sessions[ovSessionID]
	c.sessMu.Unlock()
	if ss == nil {
		return 0, fmt.Errorf("openviking: session %s not initialized", ovSessionID)
	}

	msg := map[string]any{"role": role, "content": content}
	if peerID != "" {
		msg["peer_id"] = peerID
	}
	var resp struct {
		PendingTokens int `json:"pending_tokens"`
	}
	if err := c.doJSON(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(ss.id)+"/messages/batch",
		nil, map[string]any{"messages": []any{msg}}, &resp); err != nil {
		return 0, fmt.Errorf("openviking add messages: %w", err)
	}

	c.sessMu.Lock()
	ss.pendingTokens = resp.PendingTokens
	ss.msgSinceCommit++
	c.sessMu.Unlock()
	return resp.PendingTokens, nil
}

// MaybeCommit commits the OV session identified by ovSessionID if a
// threshold is crossed (token count or message count), subject to
// MinCommitInterval. Below threshold or within the interval, it is a
// no-op — the messages stay in the live session and accumulate until
// the next check.
func (c *Client) MaybeCommit(ctx context.Context, ovSessionID string) error {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()

	if c.sessCfg.SessionIDSeed == "" {
		return nil
	}
	ss := c.sessions[ovSessionID]
	if ss == nil {
		return nil
	}

	tokenTriggered := ss.pendingTokens >= c.sessCfg.CommitTokenThreshold
	msgTriggered := ss.msgSinceCommit >= c.sessCfg.CommitMessageThreshold
	if !tokenTriggered && !msgTriggered {
		return nil
	}
	if !ss.lastCommitAt.IsZero() && time.Since(ss.lastCommitAt) < c.sessCfg.MinCommitInterval {
		return nil
	}
	return c.commitLocked(ctx, ss)
}

// Flush forces a commit on all sessions with pending messages, regardless
// of thresholds. Call on shutdown to ensure pending messages are archived
// and extracted before the process exits. No-op when nothing is pending.
func (c *Client) Flush(ctx context.Context) error {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()

	if c.sessCfg.SessionIDSeed == "" {
		return nil
	}
	for ovSID, ss := range c.sessions {
		if ss.pendingTokens == 0 && ss.msgSinceCommit == 0 {
			continue
		}
		slog.Debug("openviking flush", "ov_session", ovSID,
			"pending_tokens", ss.pendingTokens, "msg_since_commit", ss.msgSinceCommit)
		if err := c.commitLocked(ctx, ss); err != nil {
			slog.Warn("openviking flush failed for session", "ov_session", ovSID, "error", err)
		}
	}
	return nil
}

// ensureSession lazily creates or recovers the OV session for the given
// ovSessionID. On first call it tries GET /sessions/{id}; if the session
// exists (previous process crashed or restarted) and has pending_tokens >
// 0, it commits the leftovers (crash recovery). If 404, it creates a new
// session with the assistant memory_policy. Subsequent calls are no-ops.
func (c *Client) ensureSession(ctx context.Context, ovSessionID string) error {
	c.sessMu.Lock()
	if ss := c.sessions[ovSessionID]; ss != nil {
		c.sessMu.Unlock()
		return nil
	}
	seed := c.sessCfg.SessionIDSeed
	c.sessMu.Unlock()

	if seed == "" {
		return nil
	}

	var sess struct {
		PendingTokens int `json:"pending_tokens"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(ovSessionID), nil, nil, &sess)
	if err == nil {
		// Crash recovery: session exists from a previous process.
		// If it has pending (uncommitted) messages, flush them before
		// proceeding. The entire recovery path is under a single lock
		// hold so no other goroutine can interleave.
		c.sessMu.Lock()
		defer c.sessMu.Unlock()
		ss := &sessionState{id: ovSessionID}
		c.sessions[ovSessionID] = ss
		if sess.PendingTokens > 0 {
			slog.Info("openviking crash recovery: committing leftover session",
				"session", ovSessionID, "pending_tokens", sess.PendingTokens)
			ss.pendingTokens = sess.PendingTokens
			_ = c.commitLocked(ctx, ss)
		} else {
			slog.Debug("openviking session reused", "session", ovSessionID)
		}
		return nil
	}
	if isNotFound(err) {
		return c.createSession(ctx, ovSessionID)
	}
	return fmt.Errorf("openviking ensure session: %w", err)
}

// createSession creates a new OV session with the assistant memory_policy.
// AlreadyExists is tolerated (another process or goroutine created it
// concurrently).
func (c *Client) createSession(ctx context.Context, ovSessionID string) error {
	body := map[string]any{
		"session_id":    ovSessionID,
		"memory_policy": assistantMemoryPolicy,
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/sessions", nil, body, nil)
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("openviking create session: %w", err)
	}
	if isAlreadyExists(err) {
		slog.Debug("openviking session already exists", "session", ovSessionID)
	}
	c.sessMu.Lock()
	c.sessions[ovSessionID] = &sessionState{id: ovSessionID}
	c.sessMu.Unlock()
	slog.Info("openviking session created", "session", ovSessionID)
	return nil
}

// commitLocked sends a WM v2 commit request and resets the session's
// pending state. The caller must hold sessMu.
func (c *Client) commitLocked(ctx context.Context, ss *sessionState) error {
	body := map[string]any{
		"retention_mode":                "turn_budget",
		"keep_recent_turn_count":        c.sessCfg.KeepRecentTurnCount,
		"retained_message_token_budget": c.sessCfg.RetainedMessageTokenBudget,
		"min_raw_tail_steps":            c.sessCfg.MinRawTailSteps,
	}
	if err := c.doJSON(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(ss.id)+"/commit",
		nil, body, nil); err != nil {
		return fmt.Errorf("openviking commit session: %w", err)
	}
	archivedTokens := ss.pendingTokens
	archivedMsgs := ss.msgSinceCommit
	ss.pendingTokens = 0
	ss.msgSinceCommit = 0
	ss.lastCommitAt = time.Now()
	slog.Info("openviking session committed",
		"session", ss.id,
		"archived_tokens", archivedTokens,
		"messages", archivedMsgs)
	return nil
}

// deriveSessionID produces a deterministic OV session ID from a seed and
// a conversation ID: "openagent-{sha256(seed + "/" + conversationID)[:12]}".
// Same seed + same conversation → same ID across restarts. Different
// deployments (different seeds) naturally isolate. An empty conversationID
// yields a global session.
func deriveSessionID(seed, conversationID string) string {
	h := sha256.Sum256([]byte(seed + "/" + conversationID))
	return "openagent-" + hex.EncodeToString(h[:6])
}

// isNotFound reports whether err is an OpenViking 404 / NOT_FOUND.
func isNotFound(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == 404 || ae.Code == "NOT_FOUND"
	}
	return false
}

// isAlreadyExists reports whether err is an OpenViking 409 / ALREADY_EXISTS.
func isAlreadyExists(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode == 409 || ae.Code == "ALREADY_EXISTS"
	}
	return false
}

// ── Read ──

// Read expands a viking:// URI to its full content.
func (c *Client) Read(ctx context.Context, uri string) (string, error) {
	query := url.Values{"uri": []string{uri}}
	var content string
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/content/read", query, nil, &content); err != nil {
		return "", fmt.Errorf("openviking read %s: %w", uri, err)
	}
	return content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
