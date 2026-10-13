package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const userAgent = "dispatch-relay (+https://github.com/entur/dispatch-relay)"

var (
	targetRE   = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	workflowRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)
)

type rejection struct{ reason string }

func (r *rejection) Error() string { return r.reason }

func reject(format string, args ...any) error {
	return &rejection{fmt.Sprintf(format, args...)}
}

type server struct {
	verifier *verifier
	gh       *githubApp
	log      *slog.Logger
}

type audit struct {
	target, workflow string
	c                *claims
	runURL           string
}

func (a *audit) attrs() []any {
	out := []any{"target", a.target, "workflow", a.workflow}
	if a.c != nil {
		out = append(out, "source_repo", a.c.Repository, "source_repo_id", a.c.RepositoryID,
			"ref", a.c.Ref, "sha", a.c.SHA, "event", a.c.EventName, "run_id", a.c.RunID)
	}
	return append(out, "run_url", a.runURL)
}

func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var a audit
	err := s.dispatch(r, &a)
	var rej *rejection
	switch {
	case err == nil:
		s.log.Info("dispatch", append(a.attrs(), "decision", "allow")...)
		writeJSON(w, http.StatusOK, map[string]string{"run_url": a.runURL})
	case errors.As(err, &rej):
		s.log.Warn("dispatch", append(a.attrs(), "decision", "deny", "reason", err.Error())...)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	default:
		s.log.Error("dispatch", append(a.attrs(), "decision", "error", "reason", err.Error())...)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "unavailable"})
	}
}

func (s *server) dispatch(r *http.Request, a *audit) error {
	ctx := r.Context()
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return reject("no bearer token")
	}
	var req struct {
		Target   string `json:"target"`
		Workflow string `json:"workflow"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return reject("bad body: %v", err)
	}
	a.target, a.workflow = req.Target, req.Workflow
	if !targetRE.MatchString(req.Target) || !workflowRE.MatchString(req.Workflow) {
		return reject("bad target or workflow")
	}

	c, err := s.verifier.verify(ctx, token)
	if err != nil {
		return err
	}
	a.c = c

	owner, repo, _ := strings.Cut(req.Target, "/")
	inst, err := s.gh.installationID(ctx, owner, repo)
	if err != nil {
		return err
	}
	tok, err := s.gh.installationToken(ctx, inst, repo, map[string]string{"single_file": "read"})
	if err != nil {
		return err
	}
	raw, err := s.gh.readFile(ctx, tok, owner, repo, manifestPath)
	if err != nil {
		return err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return err
	}
	ref, err := m.match(req.Workflow, c)
	if err != nil {
		return err
	}

	tok, err = s.gh.installationToken(ctx, inst, repo, map[string]string{"actions": "write"})
	if err != nil {
		return err
	}
	inputs := map[string]string{
		"sha":            c.SHA,
		"source_run_url": fmt.Sprintf("https://github.com/%s/actions/runs/%s/attempts/%s", c.Repository, c.RunID, c.RunAttempt),
	}
	a.runURL, err = s.gh.dispatch(ctx, tok, owner, repo, req.Workflow, ref, inputs)
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/dispatch", s.handleDispatch)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// Cloud Logging field names.
func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.MessageKey:
				a.Key = "message"
			case slog.LevelKey:
				a.Key = "severity"
				if a.Value.String() == "WARN" {
					a.Value = slog.StringValue("WARNING")
				}
			}
			return a
		},
	}))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if def == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", key)
		os.Exit(2)
	}
	return def
}

func main() {
	log := newLogger()
	key, err := parsePrivateKey([]byte(env("GITHUB_APP_PRIVATE_KEY", "")))
	if err != nil {
		log.Error("GITHUB_APP_PRIVATE_KEY", "err", err)
		os.Exit(2)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	s := &server{
		verifier: &verifier{
			issuer:   env("OIDC_ISSUER", "https://token.actions.githubusercontent.com"),
			audience: env("OIDC_AUDIENCE", "entur-dispatch-relay"),
			ownerID:  env("ALLOWED_OWNER_ID", ""),
			client:   client,
			now:      time.Now,
		},
		gh: &githubApp{
			apiURL: env("GITHUB_API_URL", "https://api.github.com"),
			appID:  env("GITHUB_APP_ID", ""),
			key:    key,
			client: client,
			now:    time.Now,
		},
		log: log,
	}

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           newMux(s),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      40 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
