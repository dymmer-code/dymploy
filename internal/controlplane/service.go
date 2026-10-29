package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"slices"
)

type Command struct {
	Before    string `json:"before"`
	Revision  string `json:"revision"`
	ID        string `json:"id"`
	Action    string `json:"action"`
	App       string `json:"app"`
	Job       string `json:"job"`
	Ref       string `json:"ref"`
	RequestID string `json:"request_id"`
	After     int64  `json:"after"`
	Limit     int    `json:"limit"`
}
type Result struct {
	ID    string `json:"id"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}
type Service struct {
	Catalog Catalog
	Jobs    control.Jobs
	Check   func(Application) error
	Enqueue func(context.Context, Application, job.Origin) (job.Job, error)
}

func safeJob(j job.Job) job.Job {
	j.Params = nil
	j.Input = nil
	var outcome deploy.Outcome
	if len(j.Details) > 0 && json.Unmarshal(j.Details, &outcome) == nil && len(outcome.Targets) > 0 && len(outcome.Targets) <= 100 {
		j.Details, _ = json.Marshal(outcome)
	} else {
		j.Details = nil
	}
	return j
}
func (s *Service) Handle(ctx context.Context, c Command) Result {
	data, err := s.handle(ctx, c)
	out := Result{ID: c.ID, Data: data}
	if err != nil {
		out.Data = nil
		out.Error = "unavailable"
		if errors.Is(err, ErrPending) {
			out.Error = "pending_acceptance"
		}
		if errors.Is(err, job.ErrNotFound) {
			out.Error = "not_found"
		}
	}
	return out
}
func (s *Service) handle(ctx context.Context, c Command) (any, error) {
	a, err := s.Catalog.AcceptedAt(c.App, c.Revision)
	if errors.Is(err, ErrStaleRevision) {
		return nil, err
	}
	if c.Action == "status" {
		if err == nil && s.Check != nil {
			err = s.Check(a)
		}
		return map[string]any{"accepted": err == nil}, nil
	}
	if err != nil {
		return nil, err
	}
	if s.Check != nil {
		if err = s.Check(a); err != nil {
			return nil, err
		}
	}
	switch c.Action {
	case "enqueue":
		if len(c.RequestID) < 8 || len(c.RequestID) > 128 || !a.AllowsRef(c.Ref) {
			return nil, fmt.Errorf("invalid deployment request")
		}
		j, err := s.Enqueue(ctx, a, job.Origin{Owner: a.OwnerID, Project: a.ProjectID, Source: s.Catalog.OriginSource(), App: a.ID, Request: c.RequestID, Ref: c.Ref, Definition: a.Digest()})
		return safeJob(j), err
	case "history":
		limit := c.Limit
		if limit <= 0 || limit > 100 {
			limit = 100
		}
		jobs, err := s.Jobs.List(ctx, job.Filter{Source: s.Catalog.OriginSource(), App: a.ID, Owner: a.OwnerID, Project: a.ProjectID, Before: job.ID(c.Before), Limit: limit})
		out := make([]job.Job, 0, len(jobs))
		size := 0
		for _, j := range jobs {
			safe := safeJob(j)
			raw, _ := json.Marshal(safe)
			if len(out) > 0 && size+len(raw) > 1<<20 {
				break
			}
			size += len(raw)
			out = append(out, safe)
		}
		next := ""
		if len(out) > 0 && (len(out) == limit || len(out) < len(jobs)) {
			next = string(out[len(out)-1].ID)
		}
		return map[string]any{"jobs": out, "next_cursor": next}, err
	case "job", "logs", "abort":
		j, err := s.Jobs.Get(ctx, job.ID(c.Job))
		if err != nil {
			return nil, err
		}
		if j.Origin == nil || j.Origin.Source != s.Catalog.OriginSource() || j.Origin.App != a.ID || j.Origin.Owner != a.OwnerID || j.Origin.Project != a.ProjectID {
			return nil, job.ErrNotFound
		}
		if c.Action == "job" {
			return safeJob(j), nil
		}
		if c.Action == "abort" {
			return nil, s.Jobs.Abort(ctx, j.ID, "requested from Dymmer")
		}
		if c.After < 0 {
			return nil, fmt.Errorf("invalid cursor")
		}
		limit := c.Limit
		if limit <= 0 || limit > 200 {
			limit = 200
		}
		entries, err := s.Jobs.Logs(j.ID, c.After, limit)
		if entries == nil {
			entries = []job.Entry{}
		}
		size, end := 0, 0
		for i, entry := range entries {
			raw, _ := json.Marshal(entry)
			if size+len(raw) > 1<<20 {
				break
			}
			size += len(raw)
			end = i + 1
		}
		if len(entries) > 0 && end == 0 {
			return nil, errors.New("log entry exceeds transport limit")
		}
		more := end < len(entries) || len(entries) == limit
		return map[string]any{"entries": entries[:end], "state": j.State, "finished": j.State.Final(), "complete": j.State.Final() && !more}, err
	}
	return nil, fmt.Errorf("unsupported command")
}

// Associate identifies only unambiguous, locally accepted webhook builds.
// A control-plane outage does not disable ordinary local webhook processing.
func (s *Service) Associate(ev provider.Event) (*job.Origin, []string) {
	apps, err := s.Catalog.Applications()
	if err != nil {
		return nil, nil
	}
	var matched *Application
	for _, a := range apps {
		if a.RepositoryName() != ev.Provider+"/"+ev.FullName || !a.AllowsRef(ev.Ref) {
			continue
		}
		if len(a.Repository.InstallationIDs) > 0 && !slices.Contains(a.Repository.InstallationIDs, ev.InstallationID) {
			continue
		}
		if _, err := s.Catalog.Accepted(a.ID); err != nil {
			continue
		}
		if s.Check != nil && s.Check(a) != nil {
			continue
		}
		if matched != nil {
			return nil, nil
		}
		copy := a
		matched = &copy
	}
	if matched == nil {
		return nil, nil
	}
	request := ""
	if ev.DeliveryID != "" {
		request = "hook:" + ev.DeliveryID
	}
	return &job.Origin{Trigger: "webhook", Source: s.Catalog.OriginSource(), App: matched.ID, Owner: matched.OwnerID, Project: matched.ProjectID, Definition: matched.Digest(), Request: request, Ref: ev.Ref}, matched.TargetNames()
}
