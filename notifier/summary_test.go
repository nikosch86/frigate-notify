package notifier

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

// TestActivityHasConcerns checks the pre-summarize predicate that mirrors
// Frigate's: only segments whose GenAI metadata flags a threat or other
// concerns reach the LLM, otherwise summarize returns a canned placeholder.
func TestActivityHasConcerns(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    bool
		wantErr bool
	}{
		{"no reviews", 200, `[]`, false, false},
		{"no genai metadata", 200, `[{"data":{"metadata":null}}]`, false, false},
		{"only normal activity", 200, `[{"data":{"metadata":{"potential_threat_level":0,"other_concerns":[]}}}]`, false, false},
		{"suspicious", 200, `[{"data":{"metadata":{"potential_threat_level":0}}},{"data":{"metadata":{"potential_threat_level":1}}}]`, true, false},
		{"other concerns", 200, `[{"data":{"metadata":{"potential_threat_level":0,"other_concerns":["gate left open"]}}}]`, true, false},
		{"null other concerns", 200, `[{"data":{"metadata":{"potential_threat_level":0,"other_concerns":null}}}]`, false, false},
		// Frigate keeps GenAI output that fails validation, so types can be off
		{"blank concern counts like in python", 200, `[{"data":{"metadata":{"other_concerns":[""]}}}]`, true, false},
		{"concern as string", 200, `[{"data":{"metadata":{"other_concerns":"gate left open"}}}]`, true, false},
		{"float threat level", 200, `[{"data":{"metadata":{"potential_threat_level":1.0}}}]`, true, false},
		{"malformed threat level does not hide others", 200, `[{"data":{"metadata":{"potential_threat_level":"high"}}},{"data":{"metadata":{"potential_threat_level":2}}}]`, true, false},
		{"api error", 500, `{"success":false}`, false, true},
		{"non-json response", 200, `Bad Gateway`, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotQuery url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.Query()
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			config.ConfigData.Frigate.Server = srv.URL

			got, err := activityHasConcerns(time.Unix(1_700_000_000, 0), time.Unix(1_700_003_600, 0))

			if gotPath != "/api/review" {
				t.Errorf("unexpected request path %q", gotPath)
			}
			// No limit (Frigate has no cap) and no reviewed filter (summarize
			// ignores review status), so every segment in the window counts
			wantQuery := url.Values{
				"after":  {"1700000000"},
				"before": {"1700003600"},
			}
			if !reflect.DeepEqual(gotQuery, wantQuery) {
				t.Errorf("query = %v, want %v", gotQuery, wantQuery)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("activityHasConcerns = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOnSummaryIdleRequiresConcerns ensures a flurry of benign activity neither
// calls summarize nor sends Frigate's placeholder text, while a failed check
// still falls back to asking Frigate.
func TestOnSummaryIdleRequiresConcerns(t *testing.T) {
	tests := []struct {
		name          string
		reviewsStatus int
		reviewsBody   string
		wantSummarize bool
	}{
		{"benign activity", 200, `[{"data":{"metadata":{"potential_threat_level":0,"other_concerns":[]}}}]`, false},
		{"suspicious activity", 200, `[{"data":{"metadata":{"potential_threat_level":1,"other_concerns":[]}}}]`, true},
		{"review check failed", 500, `{"success":false}`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summarizeCalled := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/review/summarize") {
					summarizeCalled = true
					w.Write([]byte(`{"success":true,"summary":"No concerns were found during this time period."}`))
					return
				}
				w.WriteHeader(tc.reviewsStatus)
				w.Write([]byte(tc.reviewsBody))
			}))
			defer srv.Close()
			config.ConfigData.Frigate.Server = srv.URL

			summaryMu.Lock()
			flurryStartTime = time.Now().Add(-5 * time.Minute)
			summaryMu.Unlock()

			onSummaryIdle()

			if summarizeCalled != tc.wantSummarize {
				t.Errorf("summarize called = %v, want %v", summarizeCalled, tc.wantSummarize)
			}
		})
	}
}
