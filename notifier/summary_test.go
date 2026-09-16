package notifier

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0x2142/frigate-notify/config"
)

// TestRequestFrigateSummary exercises the summarize call against a mock Frigate,
// including the non-2xx responses Frigate 0.18 returns with a JSON body
// (no descriptions-role provider, or a user without access to all cameras).
func TestRequestFrigateSummary(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantSummary string
		wantErr     string
	}{
		{"success", 200, `{"success":true,"summary":"# Security Summary\nRoutine activity."}`, "# Security Summary\nRoutine activity.", ""},
		{"genai not configured (400)", 400, `{"success":false,"message":"GenAI must be configured to use this feature."}`, "", "GenAI must be configured"},
		{"missing full camera access (403)", 403, `{"detail":"Access to all cameras is required for this endpoint"}`, "", "Access to all cameras is required"},
		{"generation failed (500)", 500, `{"success":false,"message":"Failed to create summary."}`, "", "Failed to create summary"},
		{"non-json error body", 502, `Bad Gateway`, "", "status code 502"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			config.ConfigData.Frigate.Server = srv.URL

			start := time.Unix(1_700_000_000, 0)
			end := time.Unix(1_700_003_600, 0)
			summary, err := requestFrigateSummary(start, end)

			if gotPath != "/api/review/summarize/start/1700000000/end/1700003600" {
				t.Errorf("unexpected request path %q", gotPath)
			}
			if summary != tc.wantSummary {
				t.Errorf("summary = %q, want %q", summary, tc.wantSummary)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
