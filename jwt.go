package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const leeway = 60 // seconds

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// See https://docs.github.com/en/actions/reference/security/oidc
type claims struct {
	Iss               string          `json:"iss"`
	Aud               json.RawMessage `json:"aud"`
	Exp               int64           `json:"exp"`
	Nbf               int64           `json:"nbf"`
	Iat               int64           `json:"iat"`
	Repository        string          `json:"repository"`
	RepositoryID      string          `json:"repository_id"`
	RepositoryOwnerID string          `json:"repository_owner_id"`
	Ref               string          `json:"ref"`
	RefType           string          `json:"ref_type"`
	SHA               string          `json:"sha"`
	EventName         string          `json:"event_name"`
	WorkflowRef       string          `json:"workflow_ref"`
	RunnerEnvironment string          `json:"runner_environment"`
	RunID             string          `json:"run_id"`
	RunAttempt        string          `json:"run_attempt"`
}

type verifier struct {
	issuer   string
	audience string
	ownerID  string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func (v *verifier) verify(ctx context.Context, token string) (*claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, reject("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, reject("bad header: %v", err)
	}
	if hdr.Alg != "RS256" {
		return nil, reject("alg %q", hdr.Alg)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, reject("bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return nil, reject("bad signature")
	}
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return nil, reject("bad claims: %v", err)
	}
	return &c, v.check(&c)
}

func (v *verifier) check(c *claims) error {
	now := v.now().Unix()
	switch {
	case c.Iss != v.issuer:
		return reject("iss %q", c.Iss)
	case !audienceIs(c.Aud, v.audience):
		return reject("aud %s", c.Aud)
	case c.Exp == 0 || now > c.Exp+leeway:
		return reject("expired")
	case c.Nbf > now+leeway, c.Iat > now+leeway:
		return reject("not yet valid")
	case c.RepositoryOwnerID != v.ownerID:
		return reject("repository_owner_id %q", c.RepositoryOwnerID)
	case c.RefType != "branch":
		return reject("ref_type %q", c.RefType)
	case c.RunnerEnvironment != "github-hosted":
		return reject("runner_environment %q", c.RunnerEnvironment)
	case !shaRE.MatchString(c.SHA):
		return reject("sha %q", c.SHA)
	case c.RepositoryID == "" || c.Repository == "" || c.RunID == "":
		return reject("missing claims")
	}
	return nil
}

// aud may be a string or a single-element array.
func audienceIs(raw json.RawMessage, want string) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == want
	}
	var a []string
	return json.Unmarshal(raw, &a) == nil && len(a) == 1 && a[0] == want
}

// key refetches the JWKS on an unknown kid, at most once a minute.
func (v *verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if v.keys != nil && v.now().Sub(v.fetched) < time.Minute {
		return nil, reject("unknown kid %q", kid)
	}
	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	v.keys, v.fetched = keys, v.now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, reject("unknown kid %q", kid)
}

func (v *verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	var disc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := getJSON(ctx, v.client, v.issuer+"/.well-known/openid-configuration", &disc); err != nil {
		return nil, err
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := getJSON(ctx, v.client, disc.JWKSURI, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("no RSA keys")
	}
	return keys, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func signRS256(key *rsa.PrivateKey, header, payload any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	s := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(s))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return s + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
