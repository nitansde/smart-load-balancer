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
)

var (
	currentConfig atomic.Value // balancer.Config
	loadBalancer  = balancer.New()

	// hostAPI is the host callback table captured at plugin init. It lets
	// the quota refresher ask the host for auth credentials and perform
	// upstream HTTP requests without touching raw sockets.
	hostAPI atomic.Pointer[C.cliproxy_host_api]

	// quotaStore holds the latest upstream quota snapshot per auth ID.
	quotaStore = quota.NewStore()

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
	return json.RawMessage(rawResponse), nil
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
func quotaRefresherConfig() quota.Config {
	cfg, _ := currentConfig.Load().(balancer.Config)
	interval := time.Duration(cfg.QuotaRefreshSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return quota.Config{
		Enabled:    cfg.QuotaEnabled,
		Providers:  cfg.QuotaProviders,
		Interval:   interval,
		ProbeFresh: cfg.QuotaProbeFresh,
	}
}

// ensureQuotaRefresher creates and starts the background quota refresher once.
func ensureQuotaRefresher() {
	quotaRefresherMu.Lock()
	defer quotaRefresherMu.Unlock()
	if quotaRefresher == nil {
		quotaRefresher = quota.NewRefresher(cgoHostClient{}, quotaStore, quotaRefresherConfig)
		loadBalancer.SetQuotaStore(quotaStore)
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
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:               true,
			SchedulerAcrossPriorities: true,
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
		candidates = append(candidates, balancer.Candidate{ID: c.ID, Provider: c.Provider})
	}
	keyHash := balancer.ClientKeyHash(http.Header(req.Options.Headers))
	authID, handled := loadBalancer.Pick(keyHash, candidates, cfg)
	return okEnvelope(pluginapi.SchedulerPickResponse{
		AuthID:  authID,
		Handled: handled,
	})
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
