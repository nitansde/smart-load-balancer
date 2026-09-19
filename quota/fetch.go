package quota

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// codexUsageEndpoint reports five-hour/weekly/monthly quota windows.
	codexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
	codexUserAgent     = "codex_cli_rs/0.76.0"
)

// usageEndpoints maps a provider to its quota endpoint. Providers without an
// entry are skipped: we only query endpoints we understand.
var usageEndpoints = map[string]string{
	"codex": codexUsageEndpoint,
}

// AuthEntry describes one host auth record.
type AuthEntry struct {
	ID        string
	AuthIndex string
	Provider  string
}

// HTTPRequest is a plain outbound HTTP request executed by the host.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    []byte
}

// HTTPResponse is the host-executed HTTP response.
type HTTPResponse struct {
	StatusCode int
	Body       []byte
}

// HostClient abstracts the host callbacks used for quota fetching so the
// fetch stays testable without cgo.
type HostClient interface {
	ListAuths() ([]AuthEntry, error)
	GetAuthJSON(authIndex string) (json.RawMessage, error)
	DoHTTP(req HTTPRequest) (HTTPResponse, error)
}

// CredentialsForAuth reads the credential JSON for an auth entry through
// the host and extracts the Codex access token and account ID.
func CredentialsForAuth(client HostClient, auth AuthEntry) (Credentials, error) {
	raw, err := client.GetAuthJSON(auth.AuthIndex)
	if err != nil {
		return Credentials{}, err
	}
	if len(raw) == 0 {
		return Credentials{}, fmt.Errorf("empty credential for auth index %q", auth.AuthIndex)
	}
	creds, err := ExtractCodexCredentials(raw)
	if err != nil {
		return Credentials{}, err
	}
	if creds.AccessToken == "" {
		return Credentials{}, fmt.Errorf("no access token for auth index %q", auth.AuthIndex)
	}
	return creds, nil
}

// FetchSnapshot fetches the precise upstream quota snapshot for one auth
// entry. It is shared by the background calibrator and the quota provider
// serving manual refreshes from the management UI, so a manual refresh
// never duplicates background work: it is the same code path.
func FetchSnapshot(client HostClient, auth AuthEntry) (Snapshot, error) {
	endpoint, ok := usageEndpoints[strings.ToLower(auth.Provider)]
	if !ok {
		return Snapshot{}, fmt.Errorf("no quota endpoint for provider %q", auth.Provider)
	}
	creds, err := CredentialsForAuth(client, auth)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := client.DoHTTP(HTTPRequest{Method: http.MethodGet, URL: endpoint, Headers: usageHeaders(creds)})
	if err != nil {
		return Snapshot{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Token expired between host read and use; CPA refreshes tokens in
		// the background, so the next attempt picks up a fresh one.
		return Snapshot{}, fmt.Errorf("unauthorized fetching quota for auth %q", auth.ID)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || len(resp.Body) == 0 {
		return Snapshot{}, fmt.Errorf("quota endpoint returned status %d for auth %q", resp.StatusCode, auth.ID)
	}
	parsed, err := ParseUsage(resp.Body, time.Now())
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		AuthID:    auth.ID,
		Provider:  strings.ToLower(auth.Provider),
		FiveHour:  parsed.FiveHour,
		Long:      parsed.Long,
		PlanType:  parsed.PlanType,
		FetchedAt: time.Now(),
	}, nil
}
func usageHeaders(creds Credentials) map[string]string {
	headers := map[string]string{
		"Authorization": "Bearer " + creds.AccessToken,
		"Content-Type":  "application/json",
		"User-Agent":    codexUserAgent,
	}
	if creds.ChatGPTAccountID != "" {
		headers["Chatgpt-Account-Id"] = creds.ChatGPTAccountID
	}
	return headers
}
