package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static int call_host_api(cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"

	"github.com/nitansde/smart-load-balancer/balancer"
	"github.com/nitansde/smart-load-balancer/quota"
)

const (
	pluginID      = "smart-load-balancer"
	pluginVersion = "0.1.0"
	pluginAuthor  = "nitansde"
	pluginRepo    = "https://github.com/nitansde/smart-load-balancer"
	// quotaProviderIdentifier is the provider key this plugin serves
	// precise quota for through the QuotaProvider capability.
	quotaProviderIdentifier = "codex"
)

var (
	currentConfig atomic.Value // balancer.Config
	loadBalancer  = balancer.New()

	// hostAPI is the host callback table captured at plugin init. It lets
	// the quota refresher ask the host for auth credentials and perform
	// upstream HTTP requests without touching raw sockets.
	hostAPI atomic.Pointer[C.cliproxy_host_api]

	// quotaStore holds the latest precise upstream quota snapshots.
	// Snapshots come from background calibration or from manual refreshes
	// through the quota provider; the usage-feedback ledger is the primary
	// quota signal and needs no polling.
	quotaStore = quota.NewStore()

	// quotaLedger estimates quota consumption per profile from usage
	// feedback (usage.handle). It is the primary input for fill-first
	// ordering.
	quotaLedger = quota.NewLedger()

	quotaRefresherMu sync.Mutex
	quotaRefresher   *quota.Refresher
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler bool `json:"scheduler"`
	// SchedulerAcrossPriorities opts into receiving candidates from all host
	// priority tiers so the balancer can walk the host's default priority
	// order itself. Without it the host only sends the highest tier.
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	// UsagePlugin receives a usage record after every completed request,
	// feeding the quota estimation ledger without polling upstream.
	UsagePlugin bool `json:"usage_plugin"`
	// QuotaProvider serves precise quota for the management UI: when the
	// user manually refreshes a credential's quota display, the host calls
	// our quota.fetch, which updates the snapshot store as a side effect.
	// One code path means a manual refresh never duplicates background work.
	QuotaProvider bool `json:"quota_provider"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host != nil {
		hostAPI.Store(host)
	}
	currentConfig.Store(balancer.DefaultConfig())
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

// callHostCallback invokes a host.* callback through the captured host API.
// It mirrors the mechanism used by the reference codex-quota-scheduler.
func callHostCallback(method string, payload any) (json.RawMessage, error) {
	host := hostAPI.Load()
	if host == nil {
		return nil, fmt.Errorf("host callback %s unavailable: plugin not initialized with host api", method)
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(host, cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(host, response.ptr, response.len)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s failed with code %d", method, int(callCode))
	}
	// The host wraps every callback response in an RPC envelope:
	// {"ok":true,"result":{...}} on success, {"ok":false,"error":{...}}
	// on failure (still with return code 0). Unwrap it here so callers
	// decode the payload directly and callback errors are never masked
	// as empty successful responses.
	var env pluginabi.Envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback %s envelope: %w", method, err)
	}
	if !env.OK {
		msg := "unknown error"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return nil, fmt.Errorf("host callback %s failed: %s", method, msg)
	}
	return json.RawMessage(env.Result), nil
}

// cgoHostClient implements quota.HostClient through host callbacks.
type cgoHostClient struct{}

func (cgoHostClient) ListAuths() ([]quota.AuthEntry, error) {
	raw, err := callHostCallback(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
			Provider  string `json:"provider"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	entries := make([]quota.AuthEntry, 0, len(resp.Files))
	for _, f := range resp.Files {
		entries = append(entries, quota.AuthEntry{ID: f.ID, AuthIndex: f.AuthIndex, Provider: f.Provider})
	}
	return entries, nil
}

func (cgoHostClient) GetAuthJSON(authIndex string) (json.RawMessage, error) {
	raw, err := callHostCallback(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.get: %w", err)
	}
	return resp.JSON, nil
}

func (cgoHostClient) DoHTTP(req quota.HTTPRequest) (quota.HTTPResponse, error) {
	headers := make(http.Header, len(req.Headers))
	for k, v := range req.Headers {
		headers.Set(k, v)
	}
	raw, err := callHostCallback(pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: headers,
		Body:    req.Body,
	})
	if err != nil {
		return quota.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return quota.HTTPResponse{}, fmt.Errorf("decode host.http.do: %w", err)
	}
	return quota.HTTPResponse{StatusCode: resp.StatusCode, Body: resp.Body}, nil
}

// quotaRefresherConfig reads the live balancer config into a quota.Config so
// plugin.reconfigure takes effect without restarting the refresher.
// An interval of 0 disables background calibration (on-demand only).
func quotaRefresherConfig() quota.Config {
	cfg, _ := currentConfig.Load().(balancer.Config)
	return quota.Config{
		Enabled:    cfg.QuotaEnabled,
		Providers:  cfg.QuotaProviders,
		Interval:   time.Duration(cfg.QuotaRefreshSeconds) * time.Second,
		ProbeFresh: cfg.QuotaProbeFresh,
	}
}

// ensureQuotaRefresher creates and starts the background quota calibrator once.
func ensureQuotaRefresher() {
	quotaRefresherMu.Lock()
	defer quotaRefresherMu.Unlock()
	if quotaRefresher == nil {
		quotaRefresher = quota.NewRefresher(cgoHostClient{}, quotaStore, quotaRefresherConfig)
	}
	quotaRefresher.Start()
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	quotaRefresherMu.Lock()
	r := quotaRefresher
	quotaRefresherMu.Unlock()
	if r != nil {
		r.Stop()
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return pickAuth(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": quotaProviderIdentifier})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{
			SupportedProviders: []string{quotaProviderIdentifier},
			DisplayName:        "Smart Load Balancer",
			SupportsReset:      false,
		})
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)
	case pluginabi.MethodQuotaReset:
		return errorEnvelope("unsupported", "quota reset is not supported"), nil
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	cfg := balancer.DefaultConfig()
	if len(raw) > 0 {
		var req lifecycleRequest
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
		if len(req.ConfigYAML) > 0 {
			if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
				return errDecode
			}
		}
	}
	cfg = cfg.WithDefaults()
	if errValidate := cfg.Validate(); errValidate != nil {
		return errValidate
	}
	currentConfig.Store(cfg)
	ensureQuotaRefresher()
	return nil
}

func loadedConfig() balancer.Config {
	if raw := currentConfig.Load(); raw != nil {
		if cfg, ok := raw.(balancer.Config); ok {
			return cfg
		}
	}
	return balancer.DefaultConfig()
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "providers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Only balance across these provider keys (for example codex). Empty means all providers.",
				},
				{
					Name:        "strategy",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{balancer.StrategyLeastConnections, balancer.StrategyRoundRobin},
					Description: "Balancing policy: least-connections spreads load; round-robin cycles through profiles.",
				},
				{
					Name:        "sticky",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Pin each client API key to one profile while it stays healthy, improving prompt-cache reuse.",
				},
				{
					Name:        "sticky_ttl_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "How long an idle sticky assignment is kept, in seconds.",
				},
				{
					Name:        "window_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Sliding window in seconds used to estimate recent load per profile.",
				},
				{
					Name:        "max_inflight_per_profile",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Recent-pick threshold above which a sticky assignment spills over to the least-loaded profile.",
				},
				{
					Name:        "quota_enabled",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Enable quota-aware ordering: usage-feedback ledger plus precise quota via the quota provider. Defaults to true.",
				},
				{
					Name:        "quota_providers",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Only fetch precise quota for these provider keys. Empty means every provider with a known quota endpoint.",
				},
				{
					Name:        "quota_refresh_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Background precise-quota calibration interval in seconds. 0 disables it (on-demand only via manual refresh). Defaults to 72h.",
				},
				{
					Name:        "quota_probe_fresh",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Send one minimal ping the first time a never-used profile is selected, starting its weekly window countdown. Defaults to true.",
				},
				{
					Name:        "quota_priorities",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Ordered auth profile IDs, most preferred first. Applies inside quota-aware ordering; unlisted profiles rank last.",
				},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			UsagePlugin:               true,
			QuotaProvider:             true,
		},
	}
}

func pickAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()

	candidates := make([]balancer.Candidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		candidates = append(candidates, balancer.Candidate{
			ID:       c.ID,
			Provider: c.Provider,
			Priority: c.Priority,
			Status:   c.Status,
		})
	}
	keyHash := balancer.ClientKeyHash(http.Header(req.Options.Headers))
	authID, handled := loadBalancer.PickWithQuota(keyHash, candidates, cfg, &quotaResolver{now: time.Now})
	if handled && authID != "" {
		// Tell the quota calibrator this profile's numbers may have moved.
		quotaStore.MarkUsed(authID)
		// First pick of a never-used profile: optionally send one minimal
		// ping to start its weekly window countdown.
		if cfg.QuotaProbeFresh {
			maybeProbeFresh(authID)
		}
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  authID,
		Handled: handled,
	})
}

// handleUsage receives one usage record per completed request and feeds
// the quota estimation ledger. This is the primary quota signal: the
// scheduler owns all routing, so token consumption plus upstream quota
// failure signals are enough to estimate remaining quota without polling.
func handleUsage(raw []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &rec); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	observedAt := rec.RequestedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	quotaLedger.Observe(quota.UsageObservation{
		AuthID:          rec.AuthID,
		Provider:        rec.Provider,
		TotalTokens:     rec.Detail.TotalTokens,
		Failed:          rec.Failed,
		StatusCode:      rec.Failure.StatusCode,
		FailureBody:     rec.Failure.Body,
		ResponseHeaders: http.Header(rec.ResponseHeaders),
		ObservedAt:      observedAt,
	})
	return okEnvelope(struct{}{})
}

// quotaFetchRequest mirrors the host's rpcQuotaFetchRequest wrapper so the
// embedded QuotaFetchRequest fields decode without importing host internals.
type quotaFetchRequest struct {
	pluginapi.QuotaFetchRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleQuotaFetch serves manual quota refreshes from the management UI.
// The host routes the user's "refresh quota" click here; the fetched
// snapshot updates the store as a side effect, so a manual refresh also
// recalibrates the scheduler's fill-first ordering. Same code path as the
// background calibrator, never duplicated work.
func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req quotaFetchRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !quota.HasEndpoint(req.Provider) {
		return errorEnvelope("unsupported_provider", "no quota endpoint for provider "+req.Provider), nil
	}
	client := cgoHostClient{}
	entry := quota.AuthEntry{Provider: req.Provider}
	if req.AuthIndex != "" {
		auths, err := client.ListAuths()
		if err != nil {
			return errorEnvelope("host_error", "failed to list auths: "+err.Error()), nil
		}
		found := false
		for _, a := range auths {
			if a.AuthIndex == req.AuthIndex || a.ID == req.AuthID {
				entry = a
				found = true
				break
			}
		}
		if !found {
			return errorEnvelope("not_found", "auth not found"), nil
		}
	} else {
		entry.ID = req.AuthID
		entry.AuthIndex = ""
	}
	snap, err := quota.FetchSnapshot(client, entry)
	if err != nil {
		return errorEnvelope("fetch_failed", err.Error()), nil
	}
	quotaStore.Set(snap)
	return okEnvelope(quotaFetchResponse(snap))
}

// quotaFetchResponse normalizes a snapshot into the management UI shape.
func quotaFetchResponse(snap quota.Snapshot) pluginapi.QuotaFetchResponse {
	metrics := make([]pluginapi.QuotaMetric, 0, 4)
	if snap.FiveHour != nil && snap.FiveHour.UsedPercent != nil {
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key: "five_hour_used_percent", Label: "5-hour window used",
			Value: *snap.FiveHour.UsedPercent, Unit: "%",
		})
	}
	if snap.Long != nil && snap.Long.UsedPercent != nil {
		label := "Weekly window used"
		if snap.Long.Kind == quota.WindowMonthly {
			label = "Monthly window used"
		}
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key: "long_window_used_percent", Label: label,
			Value: *snap.Long.UsedPercent, Unit: "%",
		})
	}
	if reset := snap.EarliestReset(); !reset.IsZero() {
		metrics = append(metrics, pluginapi.QuotaMetric{
			Key: "reset_in_seconds", Label: "Next window reset in",
			Value: time.Until(reset).Seconds(), Unit: "s",
		})
	}
	return pluginapi.QuotaFetchResponse{Summary: metrics}
}

// quotaResolver implements balancer.QuotaResolver from the precise snapshot
// store and the usage-feedback ledger.
type quotaResolver struct {
	now func() time.Time
}

func (r *quotaResolver) Lookup(authID, provider string) balancer.QuotaInfo {
	now := r.now()
	if now.IsZero() {
		now = time.Now()
	}
	info := balancer.QuotaInfo{}
	if entry, ok := quotaLedger.Get(authID); ok {
		info.Known = true
		info.ConsumedTokens = entry.ConsumedTokens
		info.Fresh = entry.Fresh()
		if entry.Blocked(now) {
			info.BlockedUntil = entry.BlockedUntil
		}
	}
	if snap, ok := quotaStore.Get(authID); ok {
		info.Known = true
		info.Fresh = false
		if snap.Long != nil && snap.Long.UsedPercent != nil {
			info.UsedPercent = snap.Long.UsedPercent
		}
		// Weekly reset drives reset-soonest ordering; reset moments
		// within an hour count as the same tier.
		if snap.Long != nil && !snap.Long.ResetAt.IsZero() {
			info.WeeklyResetAt = snap.Long.ResetAt
		}
		if snap.Exhausted() {
			// A precise snapshot reporting exhaustion blocks the profile
			// until its earliest window reset.
			if reset := snap.EarliestReset(); !reset.IsZero() && reset.After(now) {
				info.BlockedUntil = reset
			}
		}
	}
	if !info.Known && quota.HasEndpoint(provider) {
		// A never-touched profile of a quota-tracked provider is known to
		// be at 100% remaining: the scheduler owns all routing, so nothing
		// else could have consumed it (drift is corrected by calibration).
		info.Known = true
		info.Fresh = true
	}
	return info
}

// maybeProbeFresh sends one minimal ping when authID is a never-used
// profile of a quota-tracked provider, starting its weekly window countdown.
func maybeProbeFresh(authID string) {
	entry, ok := quotaLedger.Get(authID)
	if ok && (!entry.Fresh() || entry.Probed) {
		return
	}
	if _, ok := quotaStore.Get(authID); ok {
		return // precise snapshot exists: not fresh
	}
	auths, err := cgoHostClient{}.ListAuths()
	if err != nil {
		return
	}
	for _, auth := range auths {
		if auth.ID != authID || !quota.HasEndpoint(auth.Provider) {
			continue
		}
		creds, err := quota.CredentialsForAuth(cgoHostClient{}, auth)
		if err != nil {
			return
		}
		quota.ProbeFreshWindow(cgoHostClient{}.DoHTTP, creds)
		quotaLedger.MarkProbed(authID)
		return
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
