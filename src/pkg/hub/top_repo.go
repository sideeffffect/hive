package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

var errTopRepoNoData = errors.New("top repo unavailable")

const (
	topRepoRefreshInterval = 24 * time.Hour
	topRepoHTTPTimeout     = 10 * time.Second
	topRepoEventsPages     = 3
	topRepoEventsPerPage   = 100

	topRepoSourceGitHub = "github"
	topRepoSourceGHE    = "ghe"
)

type topRepoAssociation struct {
	Label  string
	URL    string
	Source string
}

type topRepoResolver struct {
	client *http.Client
	now    func() time.Time
}

func userTopRepoAssociation(u *SaaSUser, _ []RegistryEntry) topRepoAssociation {
	if u == nil || strings.TrimSpace(u.TopRepo) == "" {
		return topRepoAssociation{}
	}
	return topRepoAssociation{Label: strings.TrimSpace(u.TopRepo), URL: strings.TrimSpace(u.TopRepoURL), Source: strings.TrimSpace(u.TopRepoSource)}
}

func (s *HubServer) queueTopRepoRefresh(u *SaaSUser, now time.Time) {
	if s == nil || u == nil || !s.topRepoRefreshEnabled || s.authProviders == nil || s.authProviders.Count() == 0 || !topRepoRefreshDue(u, now) {
		return
	}
	if profile := s.topRepoProfile(u); profile.Login == "" || profile.APIBase == "" {
		return
	}
	key := userCanonicalID(u)
	if key == "" {
		key = u.GitHubUsername
	}
	if key == "" {
		return
	}
	if _, loaded := s.topRepoRefreshInFlight.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		defer s.topRepoRefreshInFlight.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), topRepoHTTPTimeout*time.Duration(topRepoEventsPages+1))
		defer cancel()
		fresh := loadSaaSUser(key)
		if fresh == nil && key != u.GitHubUsername {
			fresh = loadSaaSUser(u.GitHubUsername)
		}
		if fresh == nil {
			return
		}
		if err := s.refreshUserTopRepo(ctx, fresh, time.Now().UTC()); err != nil && s.logger != nil {
			s.logger.Debug("top repo refresh skipped", "user", key, "error", err)
		}
	}()
}

func topRepoRefreshDue(u *SaaSUser, now time.Time) bool {
	if u == nil {
		return false
	}
	stamp := strings.TrimSpace(u.TopRepoUpdatedAt)
	if stamp == "" {
		return true
	}
	updated, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return true
	}
	return !now.Before(updated.Add(topRepoRefreshInterval))
}

func (s *HubServer) refreshUserTopRepo(ctx context.Context, u *SaaSUser, now time.Time) error {
	resolver := topRepoResolver{client: &http.Client{Timeout: topRepoHTTPTimeout}, now: func() time.Time { return now }}
	assoc, err := resolver.resolve(ctx, s.topRepoProfile(u))
	if err != nil {
		if errors.Is(err, errTopRepoNoData) {
			_ = saveUserTopRepoCache(u, topRepoAssociation{}, now)
		}
		return err
	}
	return saveUserTopRepoCache(u, assoc, now)
}

func saveUserTopRepoCache(u *SaaSUser, assoc topRepoAssociation, now time.Time) error {
	if u == nil {
		return fmt.Errorf("nil user")
	}
	latest := loadSaaSUser(userCanonicalID(u))
	if latest == nil && userCanonicalID(u) != u.GitHubUsername {
		latest = loadSaaSUser(u.GitHubUsername)
	}
	if latest == nil {
		latest = u
	}
	latest.TopRepo = assoc.Label
	latest.TopRepoURL = assoc.URL
	latest.TopRepoSource = assoc.Source
	latest.TopRepoUpdatedAt = now.UTC().Format(time.RFC3339)
	return saveSaaSUser(latest)
}

type topRepoProfile struct {
	Login       string
	APIBase     string
	WebBase     string
	GraphQLURL  string
	Token       string
	Source      string
	AllowEvents bool
}

func (s *HubServer) topRepoProfile(u *SaaSUser) topRepoProfile {
	login := topRepoProfileLogin(u)
	if login == "" {
		return topRepoProfile{}
	}
	provider := userProvider(u)
	if provider == legacyProvider {
		token := decryptStoredGitHubToken(u)
		if token != "" && !looksLikeGitHubBearer(token) {
			return topRepoProfile{}
		}
		if token == "" {
			token = hubGitHubToken()
		}
		return topRepoProfile{Login: login, APIBase: githubActivityDefaultAPIURL, WebBase: "https://github.com", GraphQLURL: "https://api.github.com/graphql", Token: token, Source: topRepoSourceGitHub, AllowEvents: true}
	}
	if provider != "ibmid" {
		return topRepoProfile{}
	}
	apiBase, webBase, token := s.gheTopRepoEndpoint()
	if apiBase == "" || token == "" {
		return topRepoProfile{}
	}
	return topRepoProfile{Login: login, APIBase: apiBase, WebBase: webBase, GraphQLURL: gheGraphQLURL(apiBase), Token: token, Source: topRepoSourceGHE, AllowEvents: true}
}

func topRepoProfileLogin(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if login := strings.TrimSpace(u.LinkedGitHubLogin); login != "" {
		return login
	}
	provider, subject, ok := parseCanonical(userCanonicalID(u))
	if ok && provider == legacyProvider {
		return strings.TrimSpace(subject)
	}
	return ""
}

func decryptStoredGitHubToken(u *SaaSUser) string {
	if u == nil || strings.TrimSpace(u.EncryptedToken) == "" {
		return ""
	}
	token, err := decryptToken(u.EncryptedToken)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

func looksLikeGitHubBearer(token string) bool {
	token = strings.TrimSpace(token)
	return strings.HasPrefix(token, "gh") || strings.HasPrefix(token, "github_pat_")
}

func (s *HubServer) gheTopRepoEndpoint() (apiBase, webBase, token string) {
	if s == nil {
		return "", "", ""
	}
	// GHE profile APIs generally require authentication. The hub currently only
	// has a reusable hub token (HIVE_HUB_GITHUB_TOKEN / dashboard config token) in
	// this process; GitHub App keys are installation-scoped and are not a user
	// profile credential, so we deliberately do not mint App tokens here.
	token = strings.TrimSpace(s.envGitHubToken)
	if token == "" {
		token = hubGitHubToken()
	}
	if token == "" {
		return "", "", ""
	}
	for _, c := range s.clusters {
		if host := forgeHostKey(c.GitHubBaseURL); host != publicForgeHost && host != "" {
			api := strings.TrimSpace(c.GitHubAPIURL)
			if api == "" {
				api = "https://" + host + "/api/v3"
			}
			base := strings.TrimSpace(c.GitHubBaseURL)
			if base == "" {
				base = "https://" + host
			}
			return strings.TrimRight(api, "/"), strings.TrimRight(base, "/"), token
		}
		for host, id := range forgesForCluster(&c) {
			host = forgeHostKey(host)
			if host == publicForgeHost || host == "" {
				continue
			}
			api := strings.TrimSpace(id.APIURL)
			if api == "" {
				api = "https://" + host + "/api/v3"
			}
			base := strings.TrimSpace(id.BaseURL)
			if base == "" {
				base = "https://" + host
			}
			return strings.TrimRight(api, "/"), strings.TrimRight(base, "/"), token
		}
	}
	return "", "", ""
}

func gheGraphQLURL(apiBase string) string {
	u, err := url.Parse(strings.TrimRight(apiBase, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/api/graphql"
}

func (r topRepoResolver) resolve(ctx context.Context, p topRepoProfile) (topRepoAssociation, error) {
	if strings.TrimSpace(p.Login) == "" || strings.TrimSpace(p.APIBase) == "" {
		return topRepoAssociation{}, errTopRepoNoData
	}
	if p.Source == "" {
		p.Source = topRepoSourceGitHub
	}
	if p.Token != "" && p.GraphQLURL != "" {
		if assoc, err := r.resolveGraphQL(ctx, p); err == nil && assoc.Label != "" {
			return assoc, nil
		} else if err != nil && topRepoRateLimited(err) {
			return topRepoAssociation{}, err
		}
	}
	if !p.AllowEvents {
		return topRepoAssociation{}, errTopRepoNoData
	}
	return r.resolveEvents(ctx, p)
}

type topRepoHTTPError struct{ status int }

func (e topRepoHTTPError) Error() string { return fmt.Sprintf("github api status %d", e.status) }

func topRepoRateLimited(err error) bool {
	if e, ok := err.(topRepoHTTPError); ok {
		return e.status == http.StatusForbidden || e.status == http.StatusTooManyRequests
	}
	return false
}

func (r topRepoResolver) resolveGraphQL(ctx context.Context, p topRepoProfile) (topRepoAssociation, error) {
	body := map[string]any{
		"query":     `query($login:String!){ user(login:$login){ contributionsCollection { commitContributionsByRepository(maxRepositories:1){ repository { nameWithOwner url } contributions(first:1, orderBy:{field:OCCURRED_AT,direction:DESC}) { nodes { occurredAt } } } } } }`,
		"variables": map[string]string{"login": p.Login},
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.GraphQLURL, bytes.NewReader(payload))
	if err != nil {
		return topRepoAssociation{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return topRepoAssociation{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return topRepoAssociation{}, topRepoHTTPError{status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return topRepoAssociation{}, fmt.Errorf("graphql status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			User *struct {
				ContributionsCollection struct {
					CommitContributionsByRepository []struct {
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
							URL           string `json:"url"`
						} `json:"repository"`
					} `json:"commitContributionsByRepository"`
				} `json:"contributionsCollection"`
			} `json:"user"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return topRepoAssociation{}, err
	}
	if len(out.Errors) > 0 || out.Data.User == nil || len(out.Data.User.ContributionsCollection.CommitContributionsByRepository) == 0 {
		return topRepoAssociation{}, errTopRepoNoData
	}
	repo := out.Data.User.ContributionsCollection.CommitContributionsByRepository[0].Repository
	label := strings.TrimSpace(repo.NameWithOwner)
	if label == "" {
		return topRepoAssociation{}, errTopRepoNoData
	}
	return topRepoAssociation{Label: label, URL: firstNonBlank(repo.URL, repoWebURL(p.WebBase, label)), Source: p.Source}, nil
}

func (r topRepoResolver) resolveEvents(ctx context.Context, p topRepoProfile) (topRepoAssociation, error) {
	// Events are the fallback because they work anonymously for github.com public
	// activity. We count contribution-shaped events by repo (PushEvent commits are
	// weighted by commit count; PR/issue/comment events count once) and break ties
	// by most recent event, then by repo name for deterministic output.
	scores := map[string]int{}
	recent := map[string]time.Time{}
	for page := 1; page <= topRepoEventsPages; page++ {
		events, err := r.fetchEventsPage(ctx, p, page)
		if err != nil {
			return topRepoAssociation{}, err
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			label := strings.TrimSpace(ev.Repo.Name)
			if label == "" {
				continue
			}
			weight := topRepoEventWeight(ev)
			if weight == 0 {
				continue
			}
			scores[label] += weight
			if ev.CreatedAt.After(recent[label]) {
				recent[label] = ev.CreatedAt
			}
		}
	}
	if len(scores) == 0 {
		return topRepoAssociation{}, errTopRepoNoData
	}
	labels := make([]string, 0, len(scores))
	for label := range scores {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		a, b := labels[i], labels[j]
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		if !recent[a].Equal(recent[b]) {
			return recent[a].After(recent[b])
		}
		return strings.ToLower(a) < strings.ToLower(b)
	})
	label := labels[0]
	return topRepoAssociation{Label: label, URL: repoWebURL(p.WebBase, label), Source: p.Source}, nil
}

type topRepoEvent struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Repo      struct {
		Name string `json:"name"`
	} `json:"repo"`
	Payload struct {
		Commits []struct{} `json:"commits"`
	} `json:"payload"`
}

func (r topRepoResolver) fetchEventsPage(ctx context.Context, p topRepoProfile, page int) ([]topRepoEvent, error) {
	base := strings.TrimRight(p.APIBase, "/")
	u := fmt.Sprintf("%s/users/%s/events/public?per_page=%d&page=%d", base, url.PathEscape(p.Login), topRepoEventsPerPage, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	} else if p.Source == topRepoSourceGitHub {
		authGitHubRequest(req)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, topRepoHTTPError{status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errTopRepoNoData
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("events status %d", resp.StatusCode)
	}
	var events []topRepoEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	return events, nil
}

func topRepoEventWeight(ev topRepoEvent) int {
	switch ev.Type {
	case "PushEvent":
		if len(ev.Payload.Commits) > 0 {
			return len(ev.Payload.Commits)
		}
		return 1
	case "PullRequestEvent", "IssuesEvent", "IssueCommentEvent":
		return 1
	default:
		return 0
	}
}

func (r topRepoResolver) httpClient() *http.Client {
	if r.client != nil {
		return r.client
	}
	return &http.Client{Timeout: topRepoHTTPTimeout}
}

func repoWebURL(webBase, label string) string {
	label = strings.Trim(label, "/ ")
	if label == "" || !strings.Contains(label, "/") {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(webBase), "/")
	if base == "" {
		base = "https://github.com"
	}
	return base + "/" + label
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
