package citest

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func EventFromEnvironment(env []string) (Event, error) {
	e := Event{Name: envValue(env, "GITHUB_EVENT_NAME"), Base: envValue(env, "CI_BASE_SHA"), Head: envValue(env, "GITHUB_SHA"), Ref: envValue(env, "GITHUB_REF"), Full: strings.EqualFold(envValue(env, "CI_FULL_TESTS"), "true")}
	if e.Name != "pull_request" {
		return e, nil
	}
	data, err := os.ReadFile(envValue(env, "GITHUB_EVENT_PATH"))
	if err != nil {
		return e, err
	}
	var payload struct {
		Number      int `json:"number"`
		PullRequest *struct {
			Number int `json:"number"`
			User   *struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			Head *struct {
				SHA string `json:"sha"`
			} `json:"head"`
			Base *struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return e, err
	}
	pr := payload.PullRequest
	if pr == nil {
		return e, fmt.Errorf("missing pull_request event object")
	}
	// Preserve known full-suite overrides even if other event fields are absent.
	if pr.User != nil {
		e.Author = pr.User.Login
	}
	for _, label := range pr.Labels {
		e.Labels = append(e.Labels, label.Name)
	}
	if payload.Number <= 0 || pr.Number != payload.Number || pr.User == nil || e.Author == "" || pr.Labels == nil || pr.Head == nil || pr.Base == nil {
		return e, fmt.Errorf("incomplete or inconsistent pull_request event object")
	}
	for _, label := range e.Labels {
		if label == "" {
			return e, fmt.Errorf("missing pull-request label name")
		}
	}
	e.PullRequest = &PullRequest{Number: pr.Number, Head: pr.Head.SHA, Base: pr.Base.SHA, BaseRef: pr.Base.Ref}
	return e, nil
}
func appendFile(path, text string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, text)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
func RunSelect(ctx context.Context, root string, args, env []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ci-select", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "selection manifest destination")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" || flags.NArg() != 0 {
		return fmt.Errorf("set --output manifest.json")
	}
	event, err := EventFromEnvironment(env)
	if err != nil {
		// Unknown metadata is a full plan, not an empty selection or a guessed
		// comparison range. Explicit full-suite overrides remain authoritative.
		event.MetadataError = err.Error()
	}
	p := NewPlanner(root)
	p.Env = env
	m, err := p.Plan(ctx, event)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*output, append(data, '\n'), 0600); err != nil {
		return err
	}
	report := m.Summary()
	if _, err := io.WriteString(stdout, report); err != nil {
		return err
	}
	if err := appendFile(envValue(env, "GITHUB_STEP_SUMMARY"), report); err != nil {
		return err
	}
	matrix, err := json.Marshal(m.Matrix)
	if err != nil {
		return err
	}
	return appendFile(envValue(env, "GITHUB_OUTPUT"), fmt.Sprintf("mode=%s\nhas_tests=%t\nempty_ok=%t\nmatrix=%s\n", m.Mode, len(m.Matrix.Include) > 0, m.EmptyOK, matrix))
}
