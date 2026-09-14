package acp

import (
	"strings"
	"testing"

	"github.com/yusheng-g/openagent-go/slash"
)

// newModeTestRegistry builds a command registry from a zero-value AgentServer.
// buildCommandRegistry does not read any AgentServer fields — every handler
// closes over the slash.Context passed to Handle — so the zero value is safe.
func newModeTestRegistry() *slash.Registry {
	return (&AgentServer{}).buildCommandRegistry()
}

// modeTestContext returns a slash.Context whose SetMode records the mode it
// was called with. The returned *string stays in sync: empty when SetMode
// was never invoked, else holds the last mode passed.
func modeTestContext(currentMode string) (slash.Context, *string) {
	var called string
	ctx := slash.Context{
		Mode: currentMode,
		SetMode: func(mode string) error {
			called = mode
			return nil
		},
	}
	return ctx, &called
}

func TestModeAllValidValues(t *testing.T) {
	for _, mode := range []string{"auto", "semi-auto", "manual", "plan"} {
		t.Run(mode, func(t *testing.T) {
			reg := newModeTestRegistry()
			ctx, called := modeTestContext("manual")

			resp, ok := reg.Handle(ctx, "/mode "+mode)
			if !ok {
				t.Fatalf("/mode %s: expected dispatch (ok=true)", mode)
			}
			if !strings.Contains(resp, "Switched to "+mode+" mode") {
				t.Errorf("/mode %s: resp = %q, want substring %q", mode, resp, "Switched to "+mode+" mode")
			}
			if *called != mode {
				t.Errorf("/mode %s: SetMode called with %q, want %q", mode, *called, mode)
			}
		})
	}
}

func TestModeSemiAutoFunctional(t *testing.T) {
	// Regression: "semi-auto" was missing from the case list, so /mode
	// semi-auto used to fall through to the usage branch without calling
	// SetMode. This test pins the fix.
	reg := newModeTestRegistry()
	ctx, called := modeTestContext("manual")

	resp, ok := reg.Handle(ctx, "/mode semi-auto")
	if !ok {
		t.Fatal("/mode semi-auto: expected dispatch (ok=true)")
	}
	if *called != "semi-auto" {
		t.Errorf("/mode semi-auto: SetMode called with %q, want %q (regression: case branch missing semi-auto)", *called, "semi-auto")
	}
	if !strings.Contains(resp, "Switched to semi-auto mode") {
		t.Errorf("/mode semi-auto: resp = %q, want confirmation", resp)
	}
}

func TestModeUsageIncludesSemiAuto(t *testing.T) {
	reg := newModeTestRegistry()

	for _, input := range []string{"/mode", "/mode foo"} {
		ctx, called := modeTestContext("manual")
		resp, ok := reg.Handle(ctx, input)
		if !ok {
			t.Fatalf("%s: expected dispatch (ok=true)", input)
		}
		if !strings.Contains(resp, "semi-auto") {
			t.Errorf("%s: usage text = %q, must list semi-auto", input, resp)
		}
		if *called != "" {
			t.Errorf("%s: SetMode should not be called for usage, got %q", input, *called)
		}
	}
}

func TestModeUsageShowsCurrentMode(t *testing.T) {
	reg := newModeTestRegistry()
	ctx, _ := modeTestContext("plan")

	resp, ok := reg.Handle(ctx, "/mode")
	if !ok {
		t.Fatal("/mode: expected dispatch")
	}
	if !strings.Contains(resp, "current: plan") {
		t.Errorf("/mode: resp = %q, want 'current: plan' in usage", resp)
	}
}
