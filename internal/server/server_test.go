package server

import (
	"net/http"
	"testing"
)

// Run the complete protocol suite through the actual Kratos server, not a shim.
func TestKratosProtocolIntegration(t *testing.T) {
	old := protocolTestFactory
	protocolTestFactory = func(b Backend, o Options) (http.Handler, error) { return New(b, o) }
	defer func() { protocolTestFactory = old }()
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"rest_all", TestRESTAllCommunicationOperations},
		{"rest_boundary", TestRESTBoundaryRejectsBeforeDispatch},
		{"rest_private", TestRESTPrivateDenialAndRevocation},
		{"sdk", TestMCPRealSDKInitializeListCallAndRevocation},
		{"mcp_auth", TestMCPInitializeAndListNeedAuthentication},
		{"oauth", TestOAuthMountedAndMCPUsesOAuthGrant},
		{"limits", TestRequestRateConcurrencyAndTimeout},
		{"mcp_boundary", TestMCPHostOriginChunkedCapAndRevocation},
		{"health_surface", TestTrustedOriginAndHealthSurface},
		{"deadline", TestMCPDeadlineDoesNotCancelBeforeResponse},
	} {
		t.Run(tc.name, tc.run)
	}
}
