package quota

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Credentials carries the minimum fields needed to query a provider's usage
// endpoint. Values are only kept in memory for the duration of a refresh.
type Credentials struct {
	AccessToken      string
	ChatGPTAccountID string
}

// ExtractCodexCredentials pulls the OAuth fields out of a host-returned
// credential JSON document. The document shape varies, so fields are looked
// up deeply by name.
func ExtractCodexCredentials(raw json.RawMessage) (Credentials, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Credentials{}, err
	}
	accessToken, ok := lookupStringDeep(doc, "access_token")
	if !ok || accessToken == "" {
		return Credentials{}, errors.New("quota: missing access_token")
	}
	accountID, _ := lookupStringDeep(doc, "account_id")
	if accountID == "" {
		accountID, _ = lookupStringDeep(doc, "chatgpt_account_id")
	}
	if accountID == "" {
		if idToken, ok := lookupStringDeep(doc, "id_token"); ok && idToken != "" {
			accountID, _ = chatGPTAccountIDFromJWT(idToken)
		}
	}
	return Credentials{AccessToken: accessToken, ChatGPTAccountID: accountID}, nil
}

func chatGPTAccountIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", errors.New("quota: malformed jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if v, ok := stringFromMap(claims, "https://api.openai.com/auth.chatgpt_account_id"); ok && v != "" {
		return v, nil
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := stringFromMap(auth, "chatgpt_account_id"); ok && v != "" {
			return v, nil
		}
	}
	if v, ok := lookupStringDeep(claims, "chatgpt_account_id"); ok && v != "" {
		return v, nil
	}
	return "", errors.New("quota: missing chatgpt_account_id")
}

func lookupStringDeep(v any, key string) (string, bool) {
	switch typed := v.(type) {
	case map[string]any:
		for k, val := range typed {
			if strings.EqualFold(k, key) {
				if s, ok := val.(string); ok && s != "" {
					return s, true
				}
			}
		}
		for _, val := range typed {
			if s, ok := lookupStringDeep(val, key); ok {
				return s, true
			}
		}
	case []any:
		for _, item := range typed {
			if s, ok := lookupStringDeep(item, key); ok {
				return s, true
			}
		}
	}
	return "", false
}

func lookupTimeDeep(v any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if s, ok := lookupStringDeep(v, key); ok {
			for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z07:00"} {
				if t, err := time.Parse(layout, s); err == nil {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}

func stringFromMap(m map[string]any, key string) (string, bool) {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s, true
		}
	}
	return "", false
}
