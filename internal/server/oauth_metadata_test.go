package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestOAuthHTTPRegistrationApplicationType(t *testing.T) {
	for _, tc := range []struct {
		name, extra, wantType string
		status               int
	}{
		{"omitted", "", "", http.StatusCreated},
		{"native", `,"application_type":"native"`, "native", http.StatusCreated},
		{"web", `,"application_type":"web"`, "web", http.StatusCreated},
		{"unsupported", `,"application_type":"service"`, "", http.StatusBadRequest},
		{"wrong-type", `,"application_type":42`, "", http.StatusBadRequest},
		{"duplicate", `,"application_type":"native","application_type":"web"`, "", http.StatusBadRequest},
		{"cannot-claim-identity", `,"application_type":"native","principal_id":"alice"`, "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, handler := oauthHTTPFixture(t)
			body := `{"redirect_uris":["http://127.0.0.1:8420/callback"],"client_name":"Hermes Agent","grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"` + tc.extra + `}`
			response := oauthHTTP(handler, http.MethodPost, "/register", body, "application/json", nil)
			if response.Code != tc.status {
				t.Fatalf("registration status = %d, want %d", response.Code, tc.status)
			}
			if tc.status != http.StatusCreated {
				return
			}
			var result struct {
				ClientID        string `json:"client_id"`
				ApplicationType string `json:"application_type"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ClientID == "" || result.ApplicationType != tc.wantType {
				t.Fatal("registered client metadata was not preserved")
			}
			if _, err := s.Authenticate(context.Background(), result.ClientID); err == nil {
				t.Fatal("registration granted access without approval")
			}
		})
	}
}
