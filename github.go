package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type githubApp struct {
	apiURL string
	appID  string // app id or client id
	key    *rsa.PrivateKey
	client *http.Client
	now    func() time.Time
}

type apiError struct {
	method, path string
	status       int
	body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.method, e.path, e.status, e.body)
}

func isStatus(err error, status int) bool {
	var e *apiError
	return errors.As(err, &e) && e.status == status
}

func (g *githubApp) appJWT() (string, error) {
	now := g.now()
	return signRS256(g.key,
		map[string]string{"alg": "RS256", "typ": "JWT"},
		map[string]any{
			"iat": now.Add(-60 * time.Second).Unix(),
			"exp": now.Add(9 * time.Minute).Unix(),
			"iss": g.appID,
		})
}

func (g *githubApp) installationID(ctx context.Context, owner, repo string) (int64, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	err = g.call(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/installation", jwt, "", nil, &out)
	if isStatus(err, http.StatusNotFound) {
		return 0, reject("app not installed on %s/%s", owner, repo)
	}
	return out.ID, err
}

// installationToken mints a token for one repo with only the given permissions.
func (g *githubApp) installationToken(ctx context.Context, installation int64, repo string, perms map[string]string) (string, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	in := map[string]any{"repositories": []string{repo}, "permissions": perms}
	var out struct {
		Token string `json:"token"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installation)
	return out.Token, g.call(ctx, http.MethodPost, path, jwt, "", in, &out)
}

func (g *githubApp) readFile(ctx context.Context, token, owner, repo, path string) ([]byte, error) {
	var out []byte
	err := g.call(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/contents/"+path, token, "application/vnd.github.raw+json", nil, &out)
	if isStatus(err, http.StatusNotFound) {
		return nil, reject("no %s in %s/%s", path, owner, repo)
	}
	return out, err
}

// dispatch returns the html URL of the created run.
func (g *githubApp) dispatch(ctx context.Context, token, owner, repo, workflow, ref string, inputs map[string]string) (string, error) {
	in := map[string]any{"ref": ref, "inputs": inputs, "return_run_details": true}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := "/repos/" + owner + "/" + repo + "/actions/workflows/" + workflow + "/dispatches"
	err := g.call(ctx, http.MethodPost, path, token, "", in, &out)
	if isStatus(err, http.StatusNotFound) {
		return "", reject("workflow %s not found in %s/%s", workflow, owner, repo)
	}
	return out.HTMLURL, err
}

func (g *githubApp) call(ctx context.Context, method, path, token, accept string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiURL+path, body)
	if err != nil {
		return err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &apiError{method, path, resp.StatusCode, string(b[:min(len(b), 300)])}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o = b
		return nil
	default:
		return json.Unmarshal(b, out)
	}
}

func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}
