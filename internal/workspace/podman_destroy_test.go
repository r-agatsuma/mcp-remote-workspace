//go:build linux

package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type destroyRunnerFunc func(context.Context, ...string) ([]byte, error)

func (f destroyRunnerFunc) run(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}

func TestPodmanDestroyPreSubmissionFailuresAreTerminal(t *testing.T) {
	for _, mode := range []string{"invalid-exact", "list-failure", "classification-failure", "duplicate-target", "canceled-list"} {
		t.Run(mode, func(t *testing.T) {
			e := Exact{identity(t, "s", "c").Workspace(), Generation(strings.Repeat("b", 64))}
			o := fakeObject(e)
			steps := []commandStep{infoStep()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "invalid-exact":
				e = Exact{}
			case "list-failure", "canceled-list":
				failure := listStep("")
				failure.err = injected
				if mode == "canceled-list" {
					cancel()
					failure.err = ctx.Err()
				}
				steps = append(steps, failure)
			case "classification-failure":
				invalid := fakeObject(e)
				delete(invalid.Labels, ownerLabel)
				steps = append(steps, listStep(listJSON(t, o)), inspectStep(t, invalid))
			case "duplicate-target":
				steps = append(steps, listStep(listJSON(t, o, o)), inspectStep(t, o), inspectStep(t, o))
			}
			// No rm, retry or final absence probe is allowed by this script.
			p, _ := podmanStub(t, steps...)
			p.hostRunner = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("pre-submission failure invoked scope recovery")
				return nil, nil
			}
			if err := p.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := p.Destroy(ctx, e)
			if err == nil || completionOf(err) != DefinitelyCompleted || errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("pre-submission failure retained backend uncertainty", err)
			}
		})
	}
}

func TestServicePodmanDestroyPreflightReleasesPermitButRetainsGeneration(t *testing.T) {
	s, _, _ := openFake(t, newFake())
	i := identity(t, "s", "c")
	e, err := s.Create(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	o := fakeObject(e)
	failure := listStep("")
	failure.err = injected
	p, stub := podmanStub(t,
		infoStep(),
		listStep(listJSON(t, o)), inspectStep(t, o), // Service discovery before permit acquisition
		failure,                                     // Podman.Destroy's read-only preflight, after durable permit acquisition
		listStep(listJSON(t, o)), inspectStep(t, o), // later Service retry
		listStep(listJSON(t, o)), inspectStep(t, o), // later Podman preflight
		commandStep{args: []string{"container", "rm", "--force", "--", o.ID}},
		listStep("[]"), // independent Podman absence check
		listStep("[]"), // independent Service absence check
	)
	if err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.hostRunner = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("preflight failure or ordinary retry invoked scope recovery")
		return nil, nil
	}
	var failedOperation BackendOperation
	p.runner = destroyRunnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		if stub.calls == 3 || stub.calls == 8 {
			entries, _, _, backend, err := readAdmissionJournal(s.path, s.image)
			if err != nil || entries[e.Workspace] != e || len(backend.Operations) != 1 {
				t.Fatal("exact destroy permit not durable before runtime work", backend, err)
			}
			op := backend.Operations[0]
			if op.Exact != e || op.Kind != "destroy" || op.ID == 0 || !validBootID(op.BootID) {
				t.Fatal("wrong durable operation", op)
			}
			if stub.calls == 3 {
				failedOperation = op
			} else if failedOperation.ID == 0 || op.ID <= failedOperation.ID {
				t.Fatal("retry did not acquire a fresh destroy permit", op)
			}
		}
		return stub.run(ctx, args...)
	})
	s.runtime = p
	err = s.Destroy(context.Background(), i)
	if !errors.Is(err, injected) || completionOf(err) != DefinitelyCompleted || errors.Is(err, ErrOutcomeUnknown) {
		t.Fatal("read-only failure did not attest backend completion", err)
	}
	if failedOperation.ID == 0 || stub.calls != 4 {
		t.Fatal("failure submitted removal, retry or an absence probe", stub.calls)
	}
	if len(s.PendingBackend()) != 0 || s.entries[e.Workspace] != e {
		t.Fatal("preflight failure did not release only the backend permit")
	}
	assertState(t, s, i, Present)
	entries, lifetimes, _, backend, err := readAdmissionJournal(s.path, s.image)
	if err != nil || entries[e.Workspace] != e || lifetimes[e].Exact != e || len(backend.Operations) != 0 || backend.Next != failedOperation.ID {
		t.Fatal("permit release or retained generation not durable", backend, err)
	}
	if err := s.Destroy(context.Background(), i); err != nil {
		t.Fatal("released preflight permit blocked a later destroy", err)
	}
	assertState(t, s, i, Absent)
	entries, _, _, backend, err = readAdmissionJournal(s.path, s.image)
	if err != nil || len(entries) != 0 || len(backend.Operations) != 0 {
		t.Fatal("successful exact retry did not durably finish", backend, err)
	}
}
