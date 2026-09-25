package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTopRepoEventsScoreAndTieBreakByRecent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/alice/events/public" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]any{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"type":"PushEvent","created_at":"2026-09-24T10:00:00Z","repo":{"name":"acme/alpha"},"payload":{"commits":[{},{}]}},
			{"type":"PullRequestEvent","created_at":"2026-09-24T11:00:00Z","repo":{"name":"acme/beta"},"payload":{}},
			{"type":"IssueCommentEvent","created_at":"2026-09-24T12:00:00Z","repo":{"name":"acme/beta"},"payload":{}},
			{"type":"WatchEvent","created_at":"2026-09-24T13:00:00Z","repo":{"name":"acme/noise"},"payload":{}}
		]`))
	}))
	defer srv.Close()

	got, err := (topRepoResolver{client: srv.Client()}).resolveEvents(context.Background(), topRepoProfile{
		Login:       "alice",
		APIBase:     srv.URL,
		WebBase:     "https://github.example",
		Source:      topRepoSourceGitHub,
		AllowEvents: true,
	})
	if err != nil {
		t.Fatalf("resolveEvents: %v", err)
	}
	if got.Label != "acme/beta" || got.URL != "https://github.example/acme/beta" {
		t.Fatalf("top repo = %+v, want acme/beta on github.example", got)
	}
}

func TestTopRepoGraphQLPreferredWhenTokenPresent(t *testing.T) {
	eventsCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql":
			if got := r.Header.Get("Authorization"); got != "Bearer token123" {
				t.Fatalf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"user":{"contributionsCollection":{"commitContributionsByRepository":[{"repository":{"nameWithOwner":"acme/graphql","url":"https://github.example/acme/graphql"},"contributions":{"nodes":[{"occurredAt":"2026-09-24T00:00:00Z"}]}}]}}}}`))
		case "/users/alice/events/public":
			eventsCalled = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	got, err := (topRepoResolver{client: srv.Client()}).resolve(context.Background(), topRepoProfile{
		Login:       "alice",
		APIBase:     srv.URL,
		WebBase:     "https://github.example",
		GraphQLURL:  srv.URL + "/graphql",
		Token:       "token123",
		Source:      topRepoSourceGitHub,
		AllowEvents: true,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if eventsCalled {
		t.Fatal("events fallback was called even though GraphQL returned a top repo")
	}
	if got.Label != "acme/graphql" || got.URL != "https://github.example/acme/graphql" {
		t.Fatalf("top repo = %+v, want GraphQL repo", got)
	}
}

func TestTopRepoTransientProviderErrorDoesNotClassifyAsNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := (topRepoResolver{client: srv.Client()}).resolveEvents(context.Background(), topRepoProfile{
		Login:       "alice",
		APIBase:     srv.URL,
		WebBase:     "https://github.example",
		Source:      topRepoSourceGitHub,
		AllowEvents: true,
	})
	if err == nil {
		t.Fatal("expected provider error")
	}
	if errors.Is(err, errTopRepoNoData) {
		t.Fatalf("transient provider error must preserve cached top repo, got no-data error: %v", err)
	}
}

func TestTopRepoProfileResolvesIBMidGHEEndpointOnlyWithToken(t *testing.T) {
	s := &HubServer{
		envGitHubToken: "ghe-token",
		clusters: map[string]ClusterConfig{
			"ibm": {GitHubBaseURL: "https://github.ibm.com", GitHubAPIURL: "https://github.ibm.com/api/v3"},
		},
	}
	profile := s.topRepoProfile(&SaaSUser{
		GitHubUsername:    "ibmid:650001ABCD",
		CanonicalID:       "ibmid:650001ABCD",
		Provider:          "ibmid",
		LinkedGitHubLogin: "jane",
	})
	if profile.Login != "jane" || profile.Source != topRepoSourceGHE || profile.APIBase != "https://github.ibm.com/api/v3" || profile.WebBase != "https://github.ibm.com" || profile.Token != "ghe-token" {
		t.Fatalf("GHE profile = %+v", profile)
	}

	withoutToken := (&HubServer{clusters: s.clusters}).topRepoProfile(&SaaSUser{
		GitHubUsername:    "ibmid:650001ABCD",
		CanonicalID:       "ibmid:650001ABCD",
		Provider:          "ibmid",
		LinkedGitHubLogin: "jane",
	})
	if withoutToken.Login != "" || withoutToken.APIBase != "" {
		t.Fatalf("IBMid without a usable GHE token should not call provider APIs: %+v", withoutToken)
	}
}

func TestTopRepoRefreshDueUsesCachedTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fresh := &SaaSUser{TopRepo: "acme/repo", TopRepoUpdatedAt: now.Add(-topRepoRefreshInterval + time.Minute).Format(time.RFC3339)}
	if topRepoRefreshDue(fresh, now) {
		t.Fatal("fresh cached top repo should not refresh")
	}
	stale := &SaaSUser{TopRepo: "acme/repo", TopRepoUpdatedAt: now.Add(-topRepoRefreshInterval - time.Minute).Format(time.RFC3339)}
	if !topRepoRefreshDue(stale, now) {
		t.Fatal("stale cached top repo should refresh")
	}
	if !topRepoRefreshDue(&SaaSUser{}, now) {
		t.Fatal("missing timestamp should refresh")
	}
}

func TestSaveUserTopRepoCacheMergesLatestRecord(t *testing.T) {
	useTempUserDir(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", FullName: "new contact value"}); err != nil {
		t.Fatal(err)
	}
	stale := &SaaSUser{GitHubUsername: "alice", FullName: "stale contact value"}
	if err := saveUserTopRepoCache(stale, topRepoAssociation{Label: "acme/repo", URL: "https://github.com/acme/repo", Source: topRepoSourceGitHub}, now); err != nil {
		t.Fatal(err)
	}
	got := loadSaaSUser("alice")
	if got == nil {
		t.Fatal("alice missing")
	}
	if got.FullName != "new contact value" {
		t.Fatalf("FullName was overwritten by stale refresh copy: %q", got.FullName)
	}
	if got.TopRepo != "acme/repo" || got.TopRepoURL != "https://github.com/acme/repo" || got.TopRepoUpdatedAt != now.Format(time.RFC3339) {
		t.Fatalf("top repo cache not persisted: %+v", got)
	}
}

func TestAdminUsersCarriesCachedTopRepo(t *testing.T) {
	s := sessionTestHub()
	useTempUserDir(t)
	updated := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", TopRepo: "acme/profile", TopRepoURL: "https://github.com/acme/profile", TopRepoSource: topRepoSourceGitHub, TopRepoUpdatedAt: updated}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.handleAdminUsers(rec, httptest.NewRequest("GET", "/api/saas/admin/users", nil))
	var resp struct {
		Users []struct {
			GitHubUsername string `json:"github_username"`
			TopRepo        string `json:"top_repo"`
			TopRepoURL     string `json:"top_repo_url"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad admin users payload: %v", err)
	}
	for _, u := range resp.Users {
		if u.GitHubUsername == "alice" {
			if u.TopRepo != "acme/profile" || u.TopRepoURL != "https://github.com/acme/profile" {
				t.Fatalf("cached top repo = %q %q", u.TopRepo, u.TopRepoURL)
			}
			return
		}
	}
	t.Fatal("alice missing from admin users payload")
}

func TestAdminUsersTableTopRepoColumn(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, `sortUsers(\'top_repo\')`) {
		t.Fatal("Top Repo header is not wired into the existing sortUsers mechanism")
	}
	if !strings.Contains(html, "'<td>' + topRepoCell(u) + '</td>'") {
		t.Fatal("admin users rows do not render topRepoCell")
	}
	if strings.Contains(html, `onclick="sortUsers(\'top_repo\')" style=`) {
		t.Fatal("Top Repo header added a new inline style attribute")
	}
}
