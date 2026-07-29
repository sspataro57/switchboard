package slackweb_test

// SWT-12 criterion 6: "Both slackweb.CommandBridge and slackweb.HTTPBridge
// implement it" — the tools.SlackSender seam
// (`Send(ctx, targetURL, text string) error` + `SetSlackSender`), mirroring
// GmailSender/JiraSender.
//
// This lives in the EXTERNAL test package on purpose: internal/tools imports
// internal/connector/slackweb, so an in-package assertion would be an import
// cycle. Go permits the external test package to import a package that imports
// the package under test.
//
// GREENFIELD NOTE: neither tools.SlackSender nor the Send methods exist yet, so
// this file compile-FAILs until they do — the expected red state. A compile
// failure here is the whole point: it is what makes criterion 15's wiring
// (cmd/opsctl and cmd/dashboard passing ONE bridge to both SetSlackDrafter and
// SetSlackSender) type-check rather than be hoped for.

import (
	"strings"
	"testing"

	"github.com/sspataro57/switchboard/internal/connector/slackweb"
	"github.com/sspataro57/switchboard/internal/tools"
)

// Compile-time proof that both transports satisfy both delivery seams. A
// URL-only cluster/workstation deployment must be able to draft AND send with
// one bridge object (criterion 15), which is only true if HTTPBridge satisfies
// both interfaces.
var (
	_ tools.SlackSender  = (*slackweb.CommandBridge)(nil)
	_ tools.SlackSender  = (*slackweb.HTTPBridge)(nil)
	_ tools.SlackDrafter = (*slackweb.CommandBridge)(nil)
	_ tools.SlackDrafter = (*slackweb.HTTPBridge)(nil)
)

// The runtime half of the same claim: ONE constructed HTTP bridge goes into both
// seams, which is exactly what criterion 15's wiring does. No network — nothing
// is called on it.
func TestOneHTTPBridgeSatisfiesBothDeliverySeams(t *testing.T) {
	bridge, err := slackweb.NewHTTPBridge("http://127.0.0.1:8787", strings.Repeat("k", 40), nil)
	if err != nil {
		t.Fatalf("NewHTTPBridge: %v", err)
	}
	tools.SetSlackDrafter(bridge)
	tools.SetSlackSender(bridge)
	t.Cleanup(func() {
		tools.SetSlackDrafter(nil)
		tools.SetSlackSender(nil)
	})
}
