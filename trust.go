package leverprice

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	rootKeyPlaceholder = "REPLACE_WITH_ROOT_PUBLIC_KEY"
	maxVerifiedKeysets = 8
)

type signingKey struct {
	id        string
	publicKey string
}

// trustChain only moves keyset versions forward, so a key revoked by a newer keyset cannot come
// back with an older one. Callers serialize access.
type trustChain struct {
	rootKeys   map[string]string
	minVersion int64
	newest     *keyset
	verified   map[string]*keyset
}

func newTrustChain(rootKeys map[string]string, minVersion int64) *trustChain {
	keys := make(map[string]string, len(rootKeys))
	for id, k := range rootKeys {
		keys[id] = k
	}
	return &trustChain{rootKeys: keys, minVersion: minVersion, verified: map[string]*keyset{}}
}

func (t *trustChain) isTrusted(k signingKey) bool {
	if t.newest == nil {
		return true
	}
	for _, n := range t.newest.keys {
		if n == k {
			return true
		}
	}
	return false
}

func (t *trustChain) verify(envelope json.RawMessage, now int64) (*config, signingKey, []string, error) {
	notSigned := &configError{codeMalformed, errors.New("leverprice config: response is not a signed config")}
	obj, ok := jsonObject(envelope)
	if !ok {
		return nil, signingKey{}, nil, notSigned
	}
	payload, ok1 := jsonString(obj["payload"])
	signature, ok2 := jsonString(obj["signature"])
	keyID, ok3 := jsonString(obj["keyId"])
	if !ok1 || !ok2 || !ok3 {
		return nil, signingKey{}, nil, notSigned
	}
	ks, err := t.verifyKeyset(obj["keyset"], now)
	if err != nil {
		return nil, signingKey{}, nil, coded(codeUntrusted, err)
	}
	var key *signingKey
	for i := range ks.keys {
		if ks.keys[i].id == keyID {
			key = &ks.keys[i]
			break
		}
	}
	if key == nil {
		return nil, signingKey{}, nil, &configError{codeBadSignature,
			fmt.Errorf("leverprice config: signing key %q is not in keyset v%d", keyID, ks.version)}
	}
	if !ed25519Verify(key.publicKey, signature, payload) {
		return nil, signingKey{}, nil, &configError{codeBadSignature, fmt.Errorf("leverprice config: bad signature (key %q)", keyID)}
	}
	cfg, dropped, err := parseConfig(payload)
	if err != nil {
		return nil, signingKey{}, nil, coded(codeMalformed, err)
	}
	if float64(now) >= cfg.expiresAt {
		return nil, signingKey{}, nil, &configError{codeExpired,
			fmt.Errorf("leverprice config v%d: expired at %s", cfg.version, isoTime(cfg.expiresAt))}
	}
	return cfg, *key, dropped, nil
}

func (t *trustChain) verifyKeyset(raw json.RawMessage, now int64) (*keyset, error) {
	malformed := errors.New("leverprice keyset: missing or malformed")
	obj, ok := jsonObject(raw)
	if !ok {
		return nil, malformed
	}
	payload, ok1 := jsonString(obj["payload"])
	signature, ok2 := jsonString(obj["signature"])
	rootKeyID, ok3 := jsonString(obj["rootKeyId"])
	if !ok1 || !ok2 || !ok3 {
		return nil, malformed
	}
	ks := t.verified[payload]
	if ks == nil {
		root := t.rootKeys[rootKeyID]
		switch {
		case root == "":
			return nil, fmt.Errorf("leverprice keyset: root key %q is not trusted by this SDK", rootKeyID)
		case root == rootKeyPlaceholder:
			return nil, errors.New("leverprice: this SDK build has no root public key; pass Options.TrustedRootKeys")
		case !ed25519Verify(root, signature, payload):
			return nil, fmt.Errorf("leverprice keyset: signature does not match root key %q", rootKeyID)
		}
		parsed, err := parseKeyset(payload)
		if err != nil {
			return nil, err
		}
		if len(t.verified) >= maxVerifiedKeysets {
			clear(t.verified)
		}
		t.verified[payload] = parsed
		ks = parsed
	}
	if ks.expires && float64(now) >= ks.expiresAt {
		return nil, fmt.Errorf("leverprice keyset v%d: expired", ks.version)
	}
	if ks.version < t.minVersion {
		return nil, fmt.Errorf("leverprice keyset v%d: below the minimum version %d", ks.version, t.minVersion)
	}
	if t.newest != nil && ks.version < t.newest.version {
		return nil, fmt.Errorf("leverprice keyset v%d: older than v%d already seen (revoked keys cannot come back)",
			ks.version, t.newest.version)
	}
	if t.newest == nil || ks.version > t.newest.version {
		t.newest = ks
	}
	return ks, nil
}

func parseKeyset(payload string) (*keyset, error) {
	raw, err := decodeJSONPayload(payload)
	if err != nil {
		return nil, errors.New("leverprice keyset: payload is not base64url JSON")
	}
	unsupported := errors.New("leverprice keyset: unsupported schema")
	obj, ok := jsonObject(raw)
	if !ok {
		return nil, unsupported
	}
	if schema, ok := jsonNumber(obj["schema"]); !ok || schema != 1 {
		return nil, unsupported
	}
	version, ok := jsonSafeInt(obj["version"])
	if !ok {
		return nil, unsupported
	}
	list, ok := jsonArray(obj["keys"])
	if !ok {
		return nil, unsupported
	}
	ks := &keyset{version: version, keys: []signingKey{}}
	for _, item := range list {
		k, ok := jsonObject(item)
		if !ok {
			continue
		}
		id, ok1 := jsonString(k["id"])
		pub, ok2 := jsonString(k["publicKey"])
		if ok1 && ok2 {
			ks.keys = append(ks.keys, signingKey{id: id, publicKey: pub})
		}
	}
	// Only a numeric expiresAt expires; anything else means "no expiry", as on the server.
	ks.expiresAt, ks.expires = jsonNumber(obj["expiresAt"])
	return ks, nil
}

func isoTime(ms float64) string {
	if !isFinite(ms) || ms > 8.64e15 || ms < -8.64e15 {
		return "an invalid date"
	}
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z")
}
