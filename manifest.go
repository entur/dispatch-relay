package main

import (
	"bytes"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v3"
)

const manifestPath = ".github/dispatch-sources.yml"

type manifest struct {
	Version   int                      `yaml:"version"`
	Workflows map[string]workflowEntry `yaml:"workflows"`
}

type workflowEntry struct {
	Ref     string   `yaml:"ref"`
	Sources []source `yaml:"sources"`
}

type source struct {
	Repository   string   `yaml:"repository"` // informational
	RepositoryID int64    `yaml:"repository_id"`
	Workflow     string   `yaml:"workflow"` // empty: any workflow
	Refs         []string `yaml:"refs"`
	Events       []string `yaml:"events"`
}

func parseManifest(b []byte) (*manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var m manifest
	if err := dec.Decode(&m); err != nil {
		return nil, reject("invalid manifest: %v", err)
	}
	if m.Version != 1 {
		return nil, reject("manifest version %d", m.Version)
	}
	return &m, nil
}

// match returns the ref to dispatch on.
func (m *manifest) match(workflow string, c *claims) (string, error) {
	wf, ok := m.Workflows[workflow]
	if !ok || wf.Ref == "" {
		return "", reject("workflow %s not in manifest", workflow)
	}
	err := reject("repository_id %s not in manifest", c.RepositoryID)
	for _, s := range wf.Sources {
		if strconv.FormatInt(s.RepositoryID, 10) != c.RepositoryID {
			continue
		}
		events := s.Events
		if len(events) == 0 {
			events = []string{"push"}
		}
		switch {
		case s.Workflow != "" && c.WorkflowRef != c.Repository+"/"+s.Workflow+"@"+c.Ref:
			err = reject("workflow_ref %q not allowed", c.WorkflowRef)
		case !slices.Contains(s.Refs, c.Ref):
			err = reject("ref %q not allowed", c.Ref)
		case !slices.Contains(events, c.EventName):
			err = reject("event %q not allowed", c.EventName)
		default:
			return wf.Ref, nil
		}
	}
	return "", err
}
