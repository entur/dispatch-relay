package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testManifest = `version: 1
workflows:
  deploy.yml:
    ref: main
    sources:
      - repository: henrik242/dispatch-demo-app
        repository_id: 42
        workflow: .github/workflows/build.yml
        refs: [refs/heads/main]
`

var now = time.Unix(1_800_000_000, 0)

type fakeGitHub struct {
	mu        sync.Mutex
	calls     []string
	tokenReqs []map[string]any
	dispatch  map[string]any
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch r.Method + " " + r.URL.Path {
	case "GET /repos/henrik242/dispatch-demo-deploy/installation":
		writeJSON(w, 200, map[string]int{"id": 7})
	case "POST /app/installations/7/access_tokens":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.tokenReqs = append(f.tokenReqs, body)
		writeJSON(w, 201, map[string]string{"token": "ghs_test"})
	case "GET /repos/henrik242/dispatch-demo-deploy/contents/.github/dispatch-sources.yml":
		if r.Header.Get("Authorization") != "Bearer ghs_test" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, testManifest)
	case "POST /repos/henrik242/dispatch-demo-deploy/actions/workflows/deploy.yml/dispatches":
		if r.Header.Get("Authorization") != "Bearer ghs_test" {
			http.Error(w, "", http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&f.dispatch)
		writeJSON(w, 200, map[string]any{"workflow_run_id": 1, "html_url": "https://github.com/run/1"})
	default:
		http.NotFound(w, r)
	}
}

func validClaims() map[string]any {
	return map[string]any{
		"iss":                 "ISSUER",
		"aud":                 "entur-dispatch-relay",
		"iat":                 now.Unix(),
		"nbf":                 now.Unix(),
		"exp":                 now.Unix() + 300,
		"repository":          "henrik242/dispatch-demo-app",
		"repository_id":       "42",
		"repository_owner_id": "129931",
		"ref":                 "refs/heads/main",
		"ref_type":            "branch",
		"sha":                 strings.Repeat("a", 40),
		"event_name":          "push",
		"workflow_ref":        "henrik242/dispatch-demo-app/.github/workflows/build.yml@refs/heads/main",
		"runner_environment":  "github-hosted",
		"run_id":              "100",
		"run_attempt":         "1",
	}
}

func setup(t *testing.T) (*httptest.Server, *fakeGitHub, *rsa.PrivateKey, string) {
	t.Helper()
	oidcKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	appKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/jwks" {
			b64 := base64.RawURLEncoding.EncodeToString
			writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1",
				"n": b64(oidcKey.N.Bytes()),
				"e": b64(big.NewInt(int64(oidcKey.E)).Bytes()),
			}}})
		}
	}))
	t.Cleanup(issuer.Close)

	gh := &fakeGitHub{}
	api := httptest.NewServer(gh)
	t.Cleanup(api.Close)

	clock := func() time.Time { return now }
	s := &server{
		verifier: &verifier{issuer: issuer.URL, audience: "entur-dispatch-relay", ownerID: "129931", client: http.DefaultClient, now: clock},
		gh:       &githubApp{apiURL: api.URL, appID: "1", key: appKey, client: http.DefaultClient, now: clock},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	relay := httptest.NewServer(newMux(s))
	t.Cleanup(relay.Close)
	return relay, gh, oidcKey, issuer.URL
}

func post(t *testing.T, url, token, body string) (int, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/dispatch", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const reqBody = `{"target":"henrik242/dispatch-demo-deploy","workflow":"deploy.yml"}`

func TestDispatchAllowed(t *testing.T) {
	relay, gh, key, iss := setup(t)
	c := validClaims()
	c["iss"] = iss
	tok, _ := signRS256(key, map[string]string{"alg": "RS256", "kid": "k1"}, c)

	status, out := post(t, relay.URL, tok, reqBody)
	if status != 200 || out["run_url"] != "https://github.com/run/1" {
		t.Fatalf("got %d %v, calls %v", status, out, gh.calls)
	}
	if gh.dispatch["ref"] != "main" || gh.dispatch["return_run_details"] != true {
		t.Errorf("dispatch body %v", gh.dispatch)
	}
	inputs := gh.dispatch["inputs"].(map[string]any)
	if inputs["sha"] != strings.Repeat("a", 40) ||
		inputs["source_run_url"] != "https://github.com/henrik242/dispatch-demo-app/actions/runs/100/attempts/1" {
		t.Errorf("inputs %v", inputs)
	}
	if len(gh.tokenReqs) != 1 {
		t.Fatalf("token requests %v", gh.tokenReqs)
	}
	tr := gh.tokenReqs[0]
	if got := toJSON(tr["permissions"]); got != `{"actions":"write","single_file":"read"}` {
		t.Errorf("token permissions %s", got)
	}
	if got := toJSON(tr["repositories"]); got != `["dispatch-demo-deploy"]` {
		t.Errorf("token repositories %s", got)
	}
}

func TestDispatchRejected(t *testing.T) {
	cases := map[string]struct {
		mutate     func(c map[string]any)
		header     map[string]string
		forged     bool
		body       string
		reachesAPI bool
	}{
		"forged signature": {forged: true},
		"wrong issuer":     {mutate: func(c map[string]any) { c["iss"] = "https://example.com" }},
		"issued in future": {mutate: func(c map[string]any) { c["iat"] = now.Unix() + 300 }},
		"bad sha":          {mutate: func(c map[string]any) { c["sha"] = "abc" }},
		"wrong audience":   {mutate: func(c map[string]any) { c["aud"] = "https://github.com/entur" }},
		"expired":          {mutate: func(c map[string]any) { c["exp"] = now.Unix() - 120 }},
		"wrong owner":      {mutate: func(c map[string]any) { c["repository_owner_id"] = "1" }},
		"self-hosted":      {mutate: func(c map[string]any) { c["runner_environment"] = "self-hosted" }},
		"tag":              {mutate: func(c map[string]any) { c["ref_type"] = "tag" }},
		"alg HS256":        {header: map[string]string{"alg": "HS256", "kid": "k1"}},
		"unknown kid":      {header: map[string]string{"alg": "RS256", "kid": "nope"}},
		"extra body field": {body: `{"target":"henrik242/dispatch-demo-deploy","workflow":"deploy.yml","inputs":{}}`},
		"not in manifest":  {mutate: func(c map[string]any) { c["repository_id"] = "43" }, reachesAPI: true},
		"pull_request":     {mutate: func(c map[string]any) { c["event_name"] = "pull_request" }, reachesAPI: true},
		"other workflow": {mutate: func(c map[string]any) {
			c["workflow_ref"] = "henrik242/dispatch-demo-app/.github/workflows/evil.yml@refs/heads/main"
		}, reachesAPI: true},
		"other branch": {mutate: func(c map[string]any) {
			c["ref"] = "refs/heads/x"
			c["workflow_ref"] = "henrik242/dispatch-demo-app/.github/workflows/build.yml@refs/heads/x"
		}, reachesAPI: true},
		"not installed": {body: `{"target":"henrik242/other","workflow":"deploy.yml"}`, reachesAPI: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			relay, gh, key, iss := setup(t)
			c := validClaims()
			c["iss"] = iss
			if tc.mutate != nil {
				tc.mutate(c)
			}
			hdr := tc.header
			if hdr == nil {
				hdr = map[string]string{"alg": "RS256", "kid": "k1"}
			}
			body := tc.body
			if body == "" {
				body = reqBody
			}
			if tc.forged {
				key, _ = rsa.GenerateKey(rand.Reader, 2048)
			}
			tok, _ := signRS256(key, hdr, c)

			status, out := post(t, relay.URL, tok, body)
			if status != 403 || out["error"] != "forbidden" {
				t.Fatalf("got %d %v", status, out)
			}
			if gh.dispatch != nil {
				t.Error("dispatched")
			}
			if !tc.reachesAPI && len(gh.calls) > 0 {
				t.Errorf("GitHub called before verification: %v", gh.calls)
			}
		})
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
