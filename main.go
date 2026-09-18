package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
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
*/
import "C"

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"

	"github.com/nitansde/smart-load-balancer/balancer"
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
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
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

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

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
			Scheduler: true,
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
