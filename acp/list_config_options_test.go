package acp

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	openacp "github.com/yusheng-g/openagent-go/acp/sdk"
	"github.com/yusheng-g/openagent-go/kernel"
)

// TestOnListConfigOptionsDefaults guards the session-less shape of the
// method: with no session state, the handler degrades to the defaults a
// fresh session would get (mode/thought_level selects, plus the model
// selector when the agent has models configured).
func TestOnListConfigOptionsDefaults(t *testing.T) {
	srv := newListMessagesServer(nil)

	resp, err := srv.OnListConfigOptions(context.Background(), openacp.ListConfigOptionsRequest{})
	if err != nil {
		t.Fatalf("OnListConfigOptions: %v", err)
	}

	byID := map[string]openacp.SessionConfigOption{}
	for _, o := range resp.ConfigOptions {
		byID[o.ID] = o
	}
	mode, ok := byID["mode"]
	if !ok {
		t.Fatal("config options must include the mode select")
	}
	if mode.CurrentValue != "auto" {
		t.Errorf("mode currentValue = %v, want auto (default)", mode.CurrentValue)
	}
	tl, ok := byID["thought_level"]
	if !ok {
		t.Fatal("config options must include the thought_level select")
	}
	if tl.CurrentValue != "medium" {
		t.Errorf("thought_level currentValue = %v, want medium (default)", tl.CurrentValue)
	}
	if model, ok := byID["model"]; ok {
		if len(model.Options) == 0 {
			t.Error("model select must carry its selectable values")
		}
		if model.CurrentValue == "" {
			t.Error("model currentValue must name the default model")
		}
	}
	// The request is session-less: the response must not be bound to any
	// session state (repeated calls stay identical).
	resp2, err := srv.OnListConfigOptions(context.Background(), openacp.ListConfigOptionsRequest{})
	if err != nil {
		t.Fatalf("second OnListConfigOptions: %v", err)
	}
	if len(resp2.ConfigOptions) != len(resp.ConfigOptions) {
		t.Errorf("repeated calls diverge: %d vs %d options",
			len(resp.ConfigOptions), len(resp2.ConfigOptions))
	}
}

// TestModelIDsSorted pins the /models ordering: the registry is a map and
// Go map iteration is randomized per call, which shuffled the panel between
// opens. ModelIDs must return keys in sorted order every time.
func TestModelIDsSorted(t *testing.T) {
	srv := NewAgentServer(nil, kernel.Deps{}, nil, nil)
	keys := []string{
		"openai/glm-5.3-flash",
		"mock/apple-v1-pro",
		"openai/deepseek-v4-flash-0731",
		"mock/apple-v1-flash",
		"mock/benchmark-v1-flash",
	}
	for _, k := range keys {
		provider, model, _ := strings.Cut(k, "/")
		srv.SetModel(provider, model, "sk-test", "", 0, 0)
	}
	got := srv.ModelIDs()
	want := append([]string(nil), keys...)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("ModelIDs = %v, want sorted %v", got, want)
	}
	// Repeated calls stay stable (the shuffle regression).
	for range 20 {
		if g := srv.ModelIDs(); !slices.Equal(g, want) {
			t.Fatalf("ModelIDs not stable: %v", g)
		}
	}
}
